package broker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/wire"
)

// testWriter routes io.Writer.Write into t.Log so test output stays attached
// to the failing test rather than spilling onto stdout.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", string(p))
	return len(p), nil
}

// fixture spins up a Broker in a tempdir and returns a dialer that produces
// raw client connections. Cleans up everything on test exit.
type fixture struct {
	t          *testing.T
	socketPath string
	logger     *slog.Logger
	cancel     context.CancelFunc

	// done is closed by the broker goroutine when Run returns. Multiple
	// observers (test body + Cleanup) can read freely from a closed channel.
	// The Run error is captured in runErr, guarded by errMu.
	done   chan struct{}
	errMu  sync.Mutex
	runErr error
}

// runErrLocked returns the broker's exit error after done has closed.
func (f *fixture) runErrLocked() error {
	f.errMu.Lock()
	defer f.errMu.Unlock()
	return f.runErr
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	if opts.SocketPath == "" {
		// macOS caps Unix socket paths at 104 bytes (sun_path), and t.TempDir()
		// produces something like /var/folders/.../TestNameVeryLong/001/ which
		// blows past that. Use os.MkdirTemp under /tmp with a tight prefix.
		dir, err := os.MkdirTemp("", "ic")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		opts.SocketPath = filepath.Join(dir, "s")
	}
	if opts.LockPath == "" {
		opts.LockPath = opts.SocketPath + ".lock"
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(testWriter{t}, nil))
	}
	if opts.HelloDeadline == 0 {
		opts.HelloDeadline = 500 * time.Millisecond
	}
	if opts.DeliverDeadline == 0 {
		opts.DeliverDeadline = 500 * time.Millisecond
	}

	ctx, cancel := context.WithCancel(t.Context())
	f := &fixture{
		t:          t,
		socketPath: opts.SocketPath,
		logger:     opts.Logger,
		cancel:     cancel,
		done:       make(chan struct{}),
	}

	go func() {
		defer close(f.done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("broker goroutine panic: %v", r)
			}
		}()
		err := Run(ctx, opts)
		f.errMu.Lock()
		f.runErr = err
		f.errMu.Unlock()
	}()
	// Wait for the socket file to appear before returning, so dial() doesn't race.
	deadline := time.Now().Add(2 * time.Second)
	ready := false
	for time.Now().Before(deadline) {
		select {
		case <-f.done:
			t.Fatalf("broker exited during startup: %v", f.runErrLocked())
		default:
		}
		c, err := net.DialTimeout("unix", opts.SocketPath, 50*time.Millisecond)
		if err == nil {
			_ = c.Close() // don't leak the probe connection
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("broker did not start listening within deadline")
	}

	t.Cleanup(func() {
		cancel()
		select {
		case <-f.done:
		case <-time.After(2 * time.Second):
			t.Errorf("broker did not exit within 2s")
		}
	})
	return f
}

// dial opens a fresh client connection to the broker.
func (f *fixture) dial() (*wire.Conn, net.Conn) {
	f.t.Helper()
	c, err := net.DialTimeout("unix", f.socketPath, time.Second)
	if err != nil {
		f.t.Fatalf("dial: %v", err)
	}
	return wire.NewConn(c), c
}

// helloSync performs a hello → welcome handshake and asserts success.
func helloSync(t *testing.T, w *wire.Conn, name string) {
	t.Helper()
	if err := w.Write(wire.Hello{Name: name, Version: "test"}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	f, err := w.Read()
	if err != nil {
		t.Fatalf("read welcome: %v", err)
	}
	if f.Kind() != wire.KindWelcome {
		t.Fatalf("got %v, want welcome", f)
	}
}

func TestHelloHappyPath(t *testing.T) {
	f := newFixture(t, Options{})
	w, _ := f.dial()
	helloSync(t, w, "alice")
}

func TestHelloBadFirstFrame(t *testing.T) {
	f := newFixture(t, Options{})
	w, _ := f.dial()
	if err := w.Write(wire.Send{ID: "x", To: "bob", Message: "hi"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := w.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	e, ok := got.(wire.Error)
	if !ok || e.Code != wire.CodeBadHello {
		t.Fatalf("got %v, want bad_hello error", got)
	}
}

func TestHelloBadName(t *testing.T) {
	f := newFixture(t, Options{})
	w, _ := f.dial()
	if err := w.Write(wire.Hello{Name: "alice bob", Version: "v"}); err != nil {
		t.Fatal(err)
	}
	got, err := w.Read()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := got.(wire.Error)
	if !ok || e.Code != wire.CodeBadName {
		t.Fatalf("got %v, want bad_name error", got)
	}
}

func TestHelloNameTaken(t *testing.T) {
	f := newFixture(t, Options{})
	w1, _ := f.dial()
	helloSync(t, w1, "alice")

	w2, _ := f.dial()
	if err := w2.Write(wire.Hello{Name: "alice", Version: "v"}); err != nil {
		t.Fatal(err)
	}
	got, err := w2.Read()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := got.(wire.Error)
	if !ok || e.Code != wire.CodeNameTaken {
		t.Fatalf("got %v, want name_taken", got)
	}
}

func TestHelloTimeout(t *testing.T) {
	f := newFixture(t, Options{HelloDeadline: 100 * time.Millisecond})
	w, _ := f.dial()
	// Don't send hello; server should time out and send hello_timeout error.
	got, err := w.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	e, ok := got.(wire.Error)
	if !ok || e.Code != wire.CodeHelloTimeout {
		t.Fatalf("got %v, want hello_timeout", got)
	}
}

func TestSendDeliversToPresentPeer(t *testing.T) {
	f := newFixture(t, Options{})
	wA, _ := f.dial()
	helloSync(t, wA, "alice")
	wB, _ := f.dial()
	helloSync(t, wB, "bob")

	id := wire.NewID()
	if err := wA.Write(wire.Send{ID: id, To: "bob", Message: "hi"}); err != nil {
		t.Fatal(err)
	}

	// Bob should see a deliver with from="alice".
	got, err := wB.Read()
	if err != nil {
		t.Fatal(err)
	}
	d, ok := got.(wire.Deliver)
	if !ok {
		t.Fatalf("got %v, want deliver", got)
	}
	if d.From != "alice" || d.Message != "hi" {
		t.Errorf("deliver = %+v", d)
	}
	if _, err := time.Parse(time.RFC3339, d.Timestamp); err != nil {
		t.Errorf("bad timestamp %q: %v", d.Timestamp, err)
	}

	// Alice should see ack ok.
	got, err = wA.Read()
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := got.(wire.SendAck)
	if !ok || !ack.OK || ack.ID != id {
		t.Errorf("ack = %+v", ack)
	}
}

func TestSendNoSuchPeer(t *testing.T) {
	f := newFixture(t, Options{})
	w, _ := f.dial()
	helloSync(t, w, "alice")

	if err := w.Write(wire.Send{ID: "1", To: "bob", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	got, err := w.Read()
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := got.(wire.SendAck)
	if !ok || ack.OK || ack.Code != wire.CodeNoSuchPeer {
		t.Fatalf("ack = %+v", ack)
	}
}

func TestSendSelfRejected(t *testing.T) {
	f := newFixture(t, Options{})
	w, _ := f.dial()
	helloSync(t, w, "alice")

	if err := w.Write(wire.Send{ID: "1", To: "alice", Message: "hi"}); err != nil {
		t.Fatal(err)
	}
	got, err := w.Read()
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := got.(wire.SendAck)
	if !ok || ack.OK || ack.Code != wire.CodeNoSelfSend {
		t.Fatalf("ack = %+v", ack)
	}
}

func TestListPeersExcludesSelfAndIsSorted(t *testing.T) {
	f := newFixture(t, Options{})

	// Connect three peers in non-alphabetical order.
	wB, _ := f.dial()
	helloSync(t, wB, "bob")
	wA, _ := f.dial()
	helloSync(t, wA, "alice")
	wC, _ := f.dial()
	helloSync(t, wC, "carol")

	// Alice asks; she should see bob, carol (not herself), sorted.
	id := "id1"
	if err := wA.Write(wire.ListPeers{ID: id}); err != nil {
		t.Fatal(err)
	}
	got, err := wA.Read()
	if err != nil {
		t.Fatal(err)
	}
	rep, ok := got.(wire.ListPeersReply)
	if !ok || rep.ID != id {
		t.Fatalf("got %v", got)
	}
	want := []string{"bob", "carol"}
	if !sort.StringsAreSorted(rep.Peers) {
		t.Errorf("peers not sorted: %v", rep.Peers)
	}
	if len(rep.Peers) != 2 || rep.Peers[0] != want[0] || rep.Peers[1] != want[1] {
		t.Errorf("peers = %v, want %v", rep.Peers, want)
	}
}

func TestPeerDisconnectCleanup(t *testing.T) {
	f := newFixture(t, Options{})
	wA, rawA := f.dial()
	helloSync(t, wA, "alice")
	wB, _ := f.dial()
	helloSync(t, wB, "bob")

	// Drop alice; bob should see her absent on list_peers.
	_ = rawA.Close()

	// Give the broker a moment to detect EOF.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if err := wB.Write(wire.ListPeers{ID: "x"}); err != nil {
			t.Fatal(err)
		}
		got, err := wB.Read()
		if err != nil {
			t.Fatal(err)
		}
		rep, ok := got.(wire.ListPeersReply)
		if !ok {
			t.Fatalf("got %v", got)
		}
		if len(rep.Peers) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("alice was not cleaned up after disconnect")
}

// TestConcurrentSendsPreserveOrder verifies that the broker preserves
// per-sender → per-receiver ordering even with many sends in flight. This
// holds because each connection has its own write mutex (via wire.Conn).
func TestConcurrentSendsPreserveOrder(t *testing.T) {
	f := newFixture(t, Options{})
	wA, _ := f.dial()
	helloSync(t, wA, "alice")
	wB, _ := f.dial()
	helloSync(t, wB, "bob")

	const N = 30

	// Bob reads delivers; each carries the message. We assert they arrive in
	// the order alice sent them (0..N-1).
	delivered := make(chan string, N)
	go func() {
		for i := 0; i < N; i++ {
			f, err := wB.Read()
			if err != nil {
				delivered <- ""
				return
			}
			d, ok := f.(wire.Deliver)
			if !ok {
				delivered <- ""
				return
			}
			delivered <- d.Message
		}
	}()

	// Alice fires sends in order. They serialize through her write mutex on
	// wire.Conn so the broker reads them in order, and the broker's per-conn
	// write mutex on bob's conn means it writes to bob in order.
	for i := 0; i < N; i++ {
		if err := wA.Write(wire.Send{ID: wire.NewID(), To: "bob", Message: strconvI(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Drain acks so the next test isn't surprised; we don't validate them.
	go func() {
		for i := 0; i < N; i++ {
			_, _ = wA.Read()
		}
	}()

	for i := 0; i < N; i++ {
		select {
		case got := <-delivered:
			if got != strconvI(i) {
				t.Fatalf("frame %d: got %q want %q", i, got, strconvI(i))
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for deliver %d", i)
		}
	}
}

// strconvI is a tiny helper to avoid pulling strconv into tests for one call.
func strconvI(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = digits[i%10]
		i /= 10
	}
	return string(buf[n:])
}

func TestIdleExitFiresWhenEmpty(t *testing.T) {
	f := newFixture(t, Options{IdleAfter: 200 * time.Millisecond})
	// No peers connect. Wait a bit longer than IdleAfter; the broker should exit.
	select {
	case <-f.done:
		if err := f.runErrLocked(); err != nil {
			t.Fatalf("broker exited with error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("broker did not idle-exit")
	}
}

func TestGoodbyeOnShutdown(t *testing.T) {
	f := newFixture(t, Options{})
	w, _ := f.dial()
	helloSync(t, w, "alice")

	// Trigger shutdown.
	f.cancel()

	// Alice should receive a goodbye, then the connection closes.
	got, err := w.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	g, ok := got.(wire.Goodbye)
	if !ok {
		t.Fatalf("got %v, want goodbye", got)
	}
	if g.Reason == "" {
		t.Errorf("goodbye missing reason")
	}
}

func TestErrLockHeldWhenSecondBrokerStarts(t *testing.T) {
	f := newFixture(t, Options{})
	// Try to start a second broker on the same socket/lock.
	err := Run(t.Context(), Options{
		SocketPath: f.socketPath,
		LockPath:   f.socketPath + ".lock",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("got %v, want ErrLockHeld", err)
	}
}

// TestNoCorruptionUnderHighConcurrency stresses the broker with many peers
// each sending to many other peers concurrently. Asserts no data races (run
// with -race) and that every message we send arrives somewhere.
func TestNoCorruptionUnderHighConcurrency(t *testing.T) {
	f := newFixture(t, Options{})
	const numPeers = 5
	const sendsPerPeer = 20

	// Connect peers.
	conns := make([]*wire.Conn, numPeers)
	names := make([]string, numPeers)
	for i := 0; i < numPeers; i++ {
		c, _ := f.dial()
		name := "peer" + strconvI(i)
		helloSync(t, c, name)
		conns[i] = c
		names[i] = name
	}

	// Each peer fires sendsPerPeer messages to peer 0 (concurrency hot spot).
	var wg sync.WaitGroup
	totalSent := 0
	for i := 1; i < numPeers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < sendsPerPeer; j++ {
				_ = conns[i].Write(wire.Send{ID: wire.NewID(), To: names[0], Message: "m"})
				_, _ = conns[i].Read() // consume ack
			}
		}()
		totalSent += sendsPerPeer
	}

	// Peer 0 reads expected delivers.
	got := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for got < totalSent {
			_, err := conns[0].Read()
			if err != nil {
				return
			}
			got++
		}
	}()

	wg.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("only got %d of %d delivers", got, totalSent)
	}
	if got != totalSent {
		t.Errorf("got %d delivers, want %d", got, totalSent)
	}
}
