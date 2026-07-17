// e2e_test.go runs the full stack in-process: a broker, two shims, and a
// fake MCP "client" pumping JSON-RPC frames through each shim's stdio. The
// goal is to assert the contract that matters: a tools/call for send_message
// on one shim causes a notifications/claude/channel to appear on the other.
package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/broker"
	"github.com/dpemmons/intercom/internal/shim"
)

// shimSession wraps a shim's stdio for a test driver. Each session has a
// background goroutine that drains the shim's stdout into a channel, so
// outbound notifications and replies don't deadlock against unread pipes.
type shimSession struct {
	t      *testing.T
	stdinW *io.PipeWriter
	frames chan []byte
	done   chan struct{}
}

func startShim(t *testing.T, ctx context.Context, name, sock string) *shimSession {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	s := &shimSession{
		t:      t,
		stdinW: stdinW,
		frames: make(chan []byte, 64),
		done:   make(chan struct{}),
	}

	// Drain stdout into the frames channel.
	go func() {
		defer close(s.frames)
		r := bufio.NewReader(stdoutR)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				s.frames <- line
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		defer close(s.done)
		defer stdoutW.Close()
		_ = shim.Run(ctx, shim.Config{
			Name:       name,
			Version:    "test-version",
			SocketPath: sock,
			BrokerBin:  "/nonexistent", // broker is already running
			Stdin:      stdinR,
			Stdout:     stdoutW,
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	t.Cleanup(func() {
		_ = stdinW.Close()
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
			t.Errorf("shim %q did not exit within 2s", name)
		}
	})

	return s
}

func (s *shimSession) send(raw string) {
	s.t.Helper()
	if _, err := io.WriteString(s.stdinW, raw+"\n"); err != nil {
		s.t.Fatalf("stdin write: %v", err)
	}
}

func (s *shimSession) recv(into any) {
	s.t.Helper()
	select {
	case line, ok := <-s.frames:
		if !ok {
			s.t.Fatal("shim output closed unexpectedly")
		}
		if err := json.Unmarshal(line, into); err != nil {
			s.t.Fatalf("decode %s: %v", line, err)
		}
	case <-time.After(3 * time.Second):
		s.t.Fatal("timeout waiting for shim frame")
	}
}

// recvUntil reads frames until a matching predicate fires, or times out.
// Returns the matching raw frame.
func (s *shimSession) recvUntil(pred func(line []byte) bool) []byte {
	s.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-s.frames:
			if !ok {
				s.t.Fatal("shim output closed unexpectedly")
			}
			if pred(line) {
				return line
			}
		case <-deadline:
			s.t.Fatal("timeout waiting for matching frame")
			return nil
		}
	}
}

// initialize drives the standard MCP handshake.
func (s *shimSession) initialize(id int) {
	s.send(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ServerInfo struct {
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	s.recv(&resp)
	if resp.ID != id {
		s.t.Errorf("initialize id mismatch: got %d", resp.ID)
	}
	if resp.Result.ServerInfo.Version != "test-version" {
		s.t.Errorf("server version = %q, want test-version", resp.Result.ServerInfo.Version)
	}
	s.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	n := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

// startBroker spawns a broker in-process and waits for the socket to be live.
func startBroker(t *testing.T) (sock string, cancel context.CancelFunc) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ice2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	sock = filepath.Join(dir, "s")
	lock := sock + ".lock"

	ctx, c := context.WithCancel(t.Context())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = broker.Run(ctx, broker.Options{
			SocketPath: sock,
			LockPath:   lock,
			IdleAfter:  10 * time.Minute,
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	// Wait for socket to be ready.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			t.Cleanup(func() {
				c()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Errorf("broker did not exit within 2s")
				}
			})
			return sock, c
		}
		time.Sleep(10 * time.Millisecond)
	}
	c()
	t.Fatal("broker did not start within 2s")
	return "", nil
}

func TestEndToEndAliceMessagesBob(t *testing.T) {
	sock, _ := startBroker(t)

	alice := startShim(t, t.Context(), "alice", sock)
	bob := startShim(t, t.Context(), "bob", sock)

	alice.initialize(1)
	bob.initialize(1)

	// The shim eager-connects to the broker in a goroutine after the
	// initialize/initialized handshake. To guarantee bob is registered before
	// alice sends, drive a tool call on bob first (any tool call triggers
	// Connect synchronously) and consume its response.
	bob.send(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`)
	bob.recvUntil(func(b []byte) bool { return strings.Contains(string(b), `"id":99`) })

	// Alice calls send_message(to="bob", message="hello").
	alice.send(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"bob","message":"hello"}}}`)

	// Two things should happen on Bob's side, in any order:
	//  (a) a notifications/claude/channel notification with the message
	//  (b) nothing else interesting
	// And on Alice's side:
	//  (c) a tools/call response with isError=false

	// Wait for the notification on Bob's side.
	rawNotif := bob.recvUntil(func(line []byte) bool {
		return strings.Contains(string(line), `"notifications/claude/channel"`)
	})
	var notif struct {
		Method string `json:"method"`
		Params struct {
			Content string `json:"content"`
			Meta    struct {
				From      string `json:"from"`
				Timestamp string `json:"timestamp"`
			} `json:"meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(rawNotif, &notif); err != nil {
		t.Fatalf("decode bob frame: %v", err)
	}
	if notif.Method != "notifications/claude/channel" {
		t.Errorf("method = %q", notif.Method)
	}
	if notif.Params.Content != "hello" {
		t.Errorf("content = %q", notif.Params.Content)
	}
	if notif.Params.Meta.From != "alice" {
		t.Errorf("from = %q", notif.Params.Meta.From)
	}
	if _, err := time.Parse(time.RFC3339, notif.Params.Meta.Timestamp); err != nil {
		t.Errorf("bad timestamp %q: %v", notif.Params.Meta.Timestamp, err)
	}

	// Wait for Alice's tool-call response.
	rawResp := alice.recvUntil(func(line []byte) bool {
		return strings.Contains(string(line), `"id":42`)
	})
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rawResp, &resp); err != nil {
		t.Fatalf("decode alice resp: %v", err)
	}
	if resp.Result.IsError {
		t.Errorf("alice got isError: %s", resp.Result.Content[0].Text)
	}
	if !strings.Contains(resp.Result.Content[0].Text, `bob`) {
		t.Errorf("alice response text = %q", resp.Result.Content[0].Text)
	}
}

func TestEndToEndListPeers(t *testing.T) {
	sock, _ := startBroker(t)

	alice := startShim(t, t.Context(), "alice", sock)
	bob := startShim(t, t.Context(), "bob", sock)
	carol := startShim(t, t.Context(), "carol", sock)

	alice.initialize(1)
	bob.initialize(1)
	carol.initialize(1)

	// Bob and Carol need to make at least one broker call (any tool call) so
	// they actually register with the broker; the shim connects lazily.
	// Use list_peers as a no-op trigger; we don't read the responses.
	bob.send(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`)
	carol.send(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`)
	// Drain those responses.
	bob.recvUntil(func(b []byte) bool { return strings.Contains(string(b), `"id":99`) })
	carol.recvUntil(func(b []byte) bool { return strings.Contains(string(b), `"id":99`) })

	// Now Alice asks list_peers and should see bob, carol (excluding herself).
	alice.send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`)
	raw := alice.recvUntil(func(b []byte) bool { return strings.Contains(string(b), `"id":7`) })

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.IsError {
		t.Errorf("isError: %s", resp.Result.Content[0].Text)
	}
	text := resp.Result.Content[0].Text
	if !strings.Contains(text, "bob") || !strings.Contains(text, "carol") {
		t.Errorf("text missing peer: %q", text)
	}
	if strings.Contains(text, "alice") {
		t.Errorf("text includes self: %q", text)
	}
}

type toolResp struct {
	Result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	} `json:"result"`
}

func (s *shimSession) callTool(id int, name, args string) toolResp {
	s.t.Helper()
	s.send(`{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`)
	raw := s.recvUntil(func(b []byte) bool { return strings.Contains(string(b), `"id":`+itoa(id)) })
	var r toolResp
	if err := json.Unmarshal(raw, &r); err != nil {
		s.t.Fatalf("decode tool response: %v", err)
	}
	return r
}

// runBrokerOn starts an in-process broker on a specific socket and returns a
// cancel func plus a channel closed once it has fully exited (socket unlinked,
// lock released), so a test can restart a broker on the same socket.
func runBrokerOn(t *testing.T, sock, lock string) (context.CancelFunc, chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer close(done)
		if err := broker.Run(ctx, broker.Options{
			SocketPath: sock,
			LockPath:   lock,
			IdleAfter:  10 * time.Minute,
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		}); err != nil {
			errCh <- err
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			return cancel, done
		}
		select {
		case err := <-errCh:
			cancel()
			t.Fatalf("broker Run failed: %v", err)
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatal("broker did not start on socket")
	return nil, nil
}

// TestEndToEndDisabledSessionInert verifies the opt-in gate: a session started
// with no explicit name and no opt-in env is inert — its broker-backed tools
// refuse to touch the broker, so it never claims a peer name.
func TestEndToEndDisabledSessionInert(t *testing.T) {
	t.Setenv("INTERCOM_ENABLE", "")
	t.Setenv("INTERCOM_NAME", "")
	// A disabled session derives its (unused) name from the cwd basename; if the
	// checkout has a non-name-safe basename there is nothing to assert.
	if _, err := shim.ResolveName(); err != nil {
		t.Skipf("cwd basename is not a valid peer name here: %v", err)
	}
	sock, _ := startBroker(t)

	dark := startShim(t, t.Context(), "", sock) // no explicit name => disabled
	dark.initialize(1)

	if r := dark.callTool(10, "send_message", `{"to":"bob","message":"hi"}`); !r.Result.IsError ||
		!strings.Contains(r.Result.Content[0].Text, "not enabled") {
		t.Errorf("send_message should be inert; got isError=%v text=%q", r.Result.IsError, r.Result.Content[0].Text)
	}
	if r := dark.callTool(11, "list_peers", `{}`); !r.Result.IsError ||
		!strings.Contains(r.Result.Content[0].Text, "not enabled") {
		t.Errorf("list_peers should be inert; got isError=%v text=%q", r.Result.IsError, r.Result.Content[0].Text)
	}
	if r := dark.callTool(12, "channel_status", `{}`); !strings.Contains(r.Result.Content[0].Text, "DISABLED") {
		t.Errorf("channel_status text = %q", r.Result.Content[0].Text)
	}

	// The disabled session must not have registered: an enabled observer sees no peers.
	obs := startShim(t, t.Context(), "obs", sock)
	obs.initialize(1)
	if r := obs.callTool(13, "list_peers", `{}`); !strings.Contains(r.Result.Content[0].Text, "No other peers") {
		t.Errorf("dark session leaked into registry; obs sees: %q", r.Result.Content[0].Text)
	}
}

// TestEndToEndAutoSuffix verifies that a second session requesting a name
// already held registers under a numbered suffix instead of failing.
func TestEndToEndAutoSuffix(t *testing.T) {
	sock, _ := startBroker(t)

	dup1 := startShim(t, t.Context(), "dup", sock)
	dup1.initialize(1)
	dup1.callTool(1, "list_peers", `{}`) // force connect + register "dup"

	dup2 := startShim(t, t.Context(), "dup", sock)
	dup2.initialize(1)
	dup2.callTool(2, "list_peers", `{}`) // connect, find "dup" taken, register "dup-2"

	if r := dup2.callTool(3, "channel_status", `{}`); !strings.Contains(r.Result.Content[0].Text, "dup-2") {
		t.Errorf("dup2 should report effective name dup-2; got %q", r.Result.Content[0].Text)
	}

	obs := startShim(t, t.Context(), "obs", sock)
	obs.initialize(1)
	// Sorted peer list reads "…: dup, dup-2"; "dup," proves "dup" is its own entry.
	text := obs.callTool(4, "list_peers", `{}`).Result.Content[0].Text
	if !strings.Contains(text, "dup-2") || !strings.Contains(text, "dup,") {
		t.Errorf("expected both dup and dup-2 registered; got %q", text)
	}
}

// TestEndToEndBurstDelivery sends several messages back-to-back to one receiver
// and asserts all arrive, in order, through the buffered deliverLoop.
func TestEndToEndBurstDelivery(t *testing.T) {
	sock, _ := startBroker(t)
	alice := startShim(t, t.Context(), "alice", sock)
	bob := startShim(t, t.Context(), "bob", sock)
	alice.initialize(1)
	bob.initialize(1)
	bob.callTool(90, "list_peers", `{}`) // ensure bob is registered

	const n = 12
	for i := 0; i < n; i++ {
		if r := alice.callTool(100+i, "send_message", `{"to":"bob","message":"m`+itoa(i)+`"}`); r.Result.IsError {
			t.Fatalf("send %d error: %q", i, r.Result.Content[0].Text)
		}
	}
	for i := 0; i < n; i++ {
		want := `"m` + itoa(i) + `"` // trailing quote makes "m1" distinct from "m10"
		bob.recvUntil(func(b []byte) bool {
			return strings.Contains(string(b), `"notifications/claude/channel"`) && strings.Contains(string(b), want)
		})
	}
}

// TestEndToEndReceiverSurvivesBrokerRestart is the regression test for the core
// bug: a receive-only session must not go dark when the broker restarts. The
// supervisor reconnects it in the background, so a later send still lands.
func TestEndToEndReceiverSurvivesBrokerRestart(t *testing.T) {
	// Keep the socket path short: macOS caps sun_path at 104 bytes.
	dir, err := os.MkdirTemp("", "icrr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	lock := sock + ".lock"

	cancel1, done1 := runBrokerOn(t, sock, lock)

	bob := startShim(t, t.Context(), "bob", sock)
	bob.initialize(1)
	alice := startShim(t, t.Context(), "alice", sock)
	alice.initialize(1)

	bob.callTool(90, "list_peers", `{}`)
	alice.callTool(91, "list_peers", `{}`)

	// Baseline delivery works before the restart.
	alice.callTool(1, "send_message", `{"to":"bob","message":"one"}`)
	bob.recvUntil(func(b []byte) bool {
		return strings.Contains(string(b), `"notifications/claude/channel"`) && strings.Contains(string(b), `"one"`)
	})

	// Kill the broker and wait for it to fully release the socket + lock.
	cancel1()
	<-done1

	// Fresh broker on the same socket; bob's supervisor reconnects on its own.
	cancel2, done2 := runBrokerOn(t, sock, lock)
	t.Cleanup(func() { cancel2(); <-done2 })

	// Retry the send until bob's supervisor has re-registered him.
	deadline := time.Now().Add(10 * time.Second)
	for id := 100; ; id++ {
		if time.Now().After(deadline) {
			t.Fatal("receiver never came back after broker restart")
		}
		if !alice.callTool(id, "send_message", `{"to":"bob","message":"two"}`).Result.IsError {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	bob.recvUntil(func(b []byte) bool {
		return strings.Contains(string(b), `"notifications/claude/channel"`) && strings.Contains(string(b), `"two"`)
	})
}

func TestEndToEndSelfSendRejected(t *testing.T) {
	sock, _ := startBroker(t)
	alice := startShim(t, t.Context(), "alice", sock)
	alice.initialize(1)

	alice.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"alice","message":"hi"}}}`)
	raw := alice.recvUntil(func(b []byte) bool { return strings.Contains(string(b), `"id":5`) })

	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Result.IsError {
		t.Errorf("expected isError")
	}
	if !strings.Contains(resp.Result.Content[0].Text, "no_self_send") {
		t.Errorf("text = %q", resp.Result.Content[0].Text)
	}
}
