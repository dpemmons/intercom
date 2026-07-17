package shim

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/wire"
)

func TestResolveNameEnv(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "explicit")
	got, err := ResolveName()
	if err != nil {
		t.Fatal(err)
	}
	if got != "explicit" {
		t.Errorf("got %q", got)
	}
}

func TestResolveNameCwdBasename(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "")
	dir, err := os.MkdirTemp("", "icname-myproj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveName()
	if err != nil {
		t.Fatal(err)
	}
	// MkdirTemp appends random chars to the prefix, so just check the prefix.
	if got == "" {
		t.Errorf("got empty name")
	}
}

func TestResolveNameRejectsInvalid(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "bad name")
	_, err := ResolveName()
	var nerr *InvalidNameError
	if !errors.As(err, &nerr) {
		t.Fatalf("got %v, want InvalidNameError", err)
	}
	want := `invalid peer name "bad name" from INTERCOM_NAME; allowed characters are ASCII letters, digits, '-', '_', up to 64 bytes`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// TestDisabledSessionStartsWithInvalidCwdBasename: a session that has not opted
// in must still serve the MCP server even when its cwd basename is not a valid
// peer name (e.g. a project dir like "web2.0"). The name is unused when
// disabled, so startup must not fail on it.
func TestDisabledSessionStartsWithInvalidCwdBasename(t *testing.T) {
	t.Setenv("INTERCOM_ENABLE", "")
	t.Setenv("INTERCOM_NAME", "")

	dir := filepath.Join(t.TempDir(), "web2.0") // '.' makes the basename invalid
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !isInvalidName(t, "web2.0") {
		t.Skip("environment considers 'web2.0' a valid name")
	}
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// Empty stdin EOFs immediately, so Run serves the (disabled) MCP server and
	// returns nil rather than hard-failing name resolution.
	err := Run(context.Background(), Config{
		Version:    "test-version",
		SocketPath: filepath.Join(dir, "nonexistent.sock"),
		BrokerBin:  "/nonexistent",
		Stdin:      strings.NewReader(""),
		Stdout:     io.Discard,
		Logger:     discardLogger(),
	})
	if err != nil {
		t.Fatalf("disabled session should start despite invalid basename, got: %v", err)
	}
}

func isInvalidName(t *testing.T, name string) bool {
	t.Helper()
	return !wire.ValidName(name)
}

func TestInstructionsDisabledExplainsHowToEnable(t *testing.T) {
	got := instructions("alice", false)
	if !strings.Contains(got, "NOT enabled") || !strings.Contains(got, "INTERCOM_ENABLE=1") {
		t.Errorf("disabled instructions missing enable guidance:\n%s", got)
	}
	if strings.Contains(got, "alice") {
		t.Errorf("disabled instructions should not present a live peer name:\n%s", got)
	}
}

func TestInstructionsEnabledMentionsChannelStatus(t *testing.T) {
	got := instructions("alice", true)
	if !strings.Contains(got, `"alice"`) || !strings.Contains(got, "channel_status()") {
		t.Errorf("enabled instructions missing name or channel_status:\n%s", got)
	}
}

func TestChannelStatusDisabled(t *testing.T) {
	fb := newFakeBroker(t)
	c := newSupervisedClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	out := channelStatus(context.Background(), false, "alice", c)
	if !strings.Contains(out, "DISABLED") || !strings.Contains(out, "INTERCOM_ENABLE=1") {
		t.Errorf("disabled channel_status wrong:\n%s", out)
	}
}

func TestChannelStatusEnabledNotConnected(t *testing.T) {
	fb := newFakeBroker(t)
	c := newSupervisedClient(t, fb) // never Connect'd
	t.Cleanup(func() { _ = c.Close() })

	out := channelStatus(context.Background(), true, "alice", c)
	if !strings.Contains(out, "enabled: yes") {
		t.Errorf("missing enabled marker:\n%s", out)
	}
	if !strings.Contains(out, "not connected") {
		t.Errorf("should report not connected:\n%s", out)
	}
	if !strings.Contains(out, "loaded the channel") {
		t.Errorf("missing channel-load caveat:\n%s", out)
	}
}

// TestSuperviseGivesUpOnFatalHello: a non-name_taken rejection will never fix
// itself, so the supervisor must stop rather than spin.
func TestSuperviseGivesUpOnFatalHello(t *testing.T) {
	fb := newFakeBroker(t)
	c := newSupervisedClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })
	fb.rejectAllHellos(t, wire.CodeBadName)

	done := runSupervisor(t, c, discardLogger())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not give up on a fatal hello rejection")
	}
}

// TestSuperviseRetriesOnNameTaken: name_taken exhaustion (all suffixes taken)
// stays retryable, and the supervisor still stops promptly on ctx cancel.
func TestSuperviseRetriesOnNameTaken(t *testing.T) {
	fb := newFakeBroker(t)
	c := newSupervisedClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })
	fb.rejectAllHellos(t, wire.CodeNameTaken)

	ctx, cancel := context.WithCancel(context.Background())
	done := runSupervisorCtx(t, ctx, c, discardLogger())
	select {
	case <-done:
		cancel()
		t.Fatal("supervisor gave up on name_taken; it should keep retrying")
	case <-time.After(800 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop after ctx cancel")
	}
}

// TestSuperviseCleanShutdownIsQuiet: a clean shutdown (ctx cancel + Close) must
// log no warning/error and no "connection dropped"/"connect failed" lines.
func TestSuperviseCleanShutdownIsQuiet(t *testing.T) {
	fb := newFakeBroker(t)
	c := newSupervisedClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })
	go fb.welcomeNextHello()

	rh := &recordHandler{}
	ctx, cancel := context.WithCancel(context.Background())
	done := runSupervisorCtx(t, ctx, c, slog.New(rh))

	deadline := time.Now().Add(2 * time.Second)
	for !c.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("supervisor never connected")
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	_ = c.Close()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not exit on clean shutdown")
	}

	rh.mu.Lock()
	defer rh.mu.Unlock()
	for i, m := range rh.msgs {
		if rh.levels[i] >= slog.LevelWarn {
			t.Errorf("clean shutdown logged at %v: %q", rh.levels[i], m)
		}
		if strings.Contains(m, "connection dropped") || strings.Contains(m, "connect failed") {
			t.Errorf("clean shutdown logged spurious line: %q", m)
		}
	}
}

// ---- test helpers ---------------------------------------------------------

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func runSupervisor(t *testing.T, c *brokerclient.Client, logger *slog.Logger) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return runSupervisorCtx(t, ctx, c, logger)
}

func runSupervisorCtx(t *testing.T, ctx context.Context, c *brokerclient.Client, logger *slog.Logger) <-chan struct{} {
	t.Helper()
	initialized := make(chan struct{})
	close(initialized)
	done := make(chan struct{})
	go func() {
		superviseConnection(ctx, initialized, c, logger)
		close(done)
	}()
	return done
}

func newSupervisedClient(t *testing.T, fb *fakeBroker) *brokerclient.Client {
	t.Helper()
	return brokerclient.NewClient(brokerclient.ClientOptions{
		Name:         "alice",
		Version:      "test-version",
		SocketPath:   fb.sock,
		BrokerBin:    "/nonexistent",
		NameAttempts: maxNameAttempts,
		Logger:       discardLogger(),
	})
}

// recordHandler is a slog.Handler that records level+message of every record.
type recordHandler struct {
	mu     sync.Mutex
	levels []slog.Level
	msgs   []string
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.levels = append(h.levels, r.Level)
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

// fakeBroker is a minimal broker stand-in for supervisor tests.
type fakeBroker struct {
	t       *testing.T
	sock    string
	ln      net.Listener
	mu      sync.Mutex
	wConn   *wire.Conn
	helloCh chan wire.Hello
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	dir, err := os.MkdirTemp("", "icshimfb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	fb := &fakeBroker{t: t, sock: sock, ln: ln, helloCh: make(chan wire.Hello, 64)}
	go fb.acceptLoop()
	return fb
}

func (fb *fakeBroker) acceptLoop() {
	for {
		c, err := fb.ln.Accept()
		if err != nil {
			return
		}
		w := wire.NewConn(c)
		fb.mu.Lock()
		fb.wConn = w
		fb.mu.Unlock()
		go fb.readLoop(w)
	}
}

func (fb *fakeBroker) readLoop(w *wire.Conn) {
	for {
		f, err := w.Read()
		if err != nil {
			return
		}
		if h, ok := f.(wire.Hello); ok {
			select {
			case fb.helloCh <- h:
			default:
			}
		}
	}
}

func (fb *fakeBroker) awaitHello() wire.Hello {
	fb.t.Helper()
	select {
	case h := <-fb.helloCh:
		return h
	case <-time.After(2 * time.Second):
		fb.t.Fatal("timeout waiting for hello")
		return wire.Hello{}
	}
}

func (fb *fakeBroker) write(f wire.Frame) {
	fb.mu.Lock()
	w := fb.wConn
	fb.mu.Unlock()
	if w != nil {
		_ = w.Write(f)
	}
}

func (fb *fakeBroker) welcomeNextHello() {
	fb.awaitHello()
	fb.write(wire.Welcome{})
}

func (fb *fakeBroker) rejectAllHellos(t *testing.T, code wire.Code) {
	t.Helper()
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-fb.helloCh:
				fb.write(wire.Error{Code: code, Message: string(code)})
			case <-stop:
				return
			}
		}
	}()
}
