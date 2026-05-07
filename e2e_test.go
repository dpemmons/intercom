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
		ID int `json:"id"`
	}
	s.recv(&resp)
	if resp.ID != id {
		s.t.Errorf("initialize id mismatch: got %d", resp.ID)
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
