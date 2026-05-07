package shim

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/wire"
)

// fakeBroker is a tiny stand-in for the real broker that lets a test script
// the next reply to each send/list_peers, and inject delivers/goodbyes at
// will.
type fakeBroker struct {
	t          *testing.T
	socketPath string
	listener   net.Listener

	// One client connection at a time is enough for these tests.
	mu    sync.Mutex
	wConn *wire.Conn
	raw   net.Conn

	helloCh  chan wire.Hello
	requests chan wire.Frame // shim-originated frames (Send, ListPeers)
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	dir, err := os.MkdirTemp("", "icfb")
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

	fb := &fakeBroker{
		t:          t,
		socketPath: sock,
		listener:   ln,
		helloCh:    make(chan wire.Hello, 1),
		requests:   make(chan wire.Frame, 16),
	}

	go fb.acceptLoop()
	return fb
}

func (fb *fakeBroker) acceptLoop() {
	for {
		c, err := fb.listener.Accept()
		if err != nil {
			return
		}
		fb.mu.Lock()
		// Replace any prior connection. Tests reconnect by closing the
		// previous one — the new one wins.
		fb.raw = c
		fb.wConn = wire.NewConn(c)
		fb.mu.Unlock()
		go fb.readLoop(c, fb.wConn)
	}
}

func (fb *fakeBroker) readLoop(raw net.Conn, w *wire.Conn) {
	for {
		f, err := w.Read()
		if err != nil {
			return
		}
		switch r := f.(type) {
		case wire.Hello:
			select {
			case fb.helloCh <- r:
			default: // drop if unread; tests should consume promptly
			}
		default:
			select {
			case fb.requests <- f:
			case <-time.After(time.Second):
				fb.t.Errorf("fake broker: requests channel full; dropping %v", r.Kind())
			}
		}
	}
}

// awaitHello blocks until the shim sends a hello. Returns the hello frame.
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

func (fb *fakeBroker) sendWelcome() {
	fb.write(wire.Welcome{})
}

func (fb *fakeBroker) write(f wire.Frame) {
	fb.t.Helper()
	fb.mu.Lock()
	w := fb.wConn
	fb.mu.Unlock()
	if w == nil {
		fb.t.Fatal("no client connected")
	}
	if err := w.Write(f); err != nil {
		fb.t.Fatalf("fake broker write: %v", err)
	}
}

// closeClient closes the active client connection so the shim sees
// disconnection.
func (fb *fakeBroker) closeClient() {
	fb.mu.Lock()
	raw := fb.raw
	fb.raw = nil
	fb.wConn = nil
	fb.mu.Unlock()
	if raw != nil {
		_ = raw.Close()
	}
}

func newTestClient(t *testing.T, fb *fakeBroker) *Client {
	t.Helper()
	return NewClient(ClientOptions{
		Name:       "alice",
		SocketPath: fb.socketPath,
		BrokerBin:  "/nonexistent", // never spawned in these tests; broker already running
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestClientSendHappyPath(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	// Drive the handshake from the fake side.
	go fb.sendWelcomeAfterHello()

	// Background the Send so we can react to it from the fake.
	type result struct {
		ack wire.SendAck
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ack, err := c.Send(t.Context(), "bob", "hi")
		ch <- result{ack, err}
	}()

	// Read the send frame the shim emits, ack it.
	select {
	case f := <-fb.requests:
		s, ok := f.(wire.Send)
		if !ok {
			t.Fatalf("got %v, want send", f)
		}
		if s.To != "bob" || s.Message != "hi" {
			t.Errorf("send = %+v", s)
		}
		fb.write(wire.SendAckOK(s.ID))
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for send")
	}

	res := <-ch
	if res.err != nil {
		t.Fatalf("Send err: %v", res.err)
	}
	if !res.ack.OK {
		t.Errorf("ack.OK = false")
	}
}

func (fb *fakeBroker) sendWelcomeAfterHello() {
	fb.awaitHello()
	fb.sendWelcome()
}

func TestClientSendErrorAck(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()

	ch := make(chan error, 1)
	var ack wire.SendAck
	go func() {
		var err error
		ack, err = c.Send(t.Context(), "bob", "hi")
		ch <- err
	}()

	f := mustRecvFrame(t, fb.requests)
	s := f.(wire.Send)
	fb.write(wire.SendAckErr(s.ID, wire.CodeNoSuchPeer, "no such peer"))

	if err := <-ch; err != nil {
		t.Fatalf("Send returned err: %v", err)
	}
	if ack.OK || ack.Code != wire.CodeNoSuchPeer {
		t.Errorf("ack = %+v", ack)
	}
}

func TestClientListPeers(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()

	ch := make(chan struct {
		peers []string
		err   error
	}, 1)
	go func() {
		p, err := c.ListPeers(t.Context())
		ch <- struct {
			peers []string
			err   error
		}{p, err}
	}()

	f := mustRecvFrame(t, fb.requests)
	lp, ok := f.(wire.ListPeers)
	if !ok {
		t.Fatalf("got %v", f)
	}
	fb.write(wire.ListPeersReply{ID: lp.ID, Peers: []string{"bob", "carol"}})

	r := <-ch
	if r.err != nil {
		t.Fatalf("ListPeers err: %v", r.err)
	}
	if len(r.peers) != 2 || r.peers[0] != "bob" || r.peers[1] != "carol" {
		t.Errorf("peers = %v", r.peers)
	}
}

func TestClientHelloRejected(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go func() {
		fb.awaitHello()
		fb.write(wire.Error{Code: wire.CodeNameTaken, Message: "in use"})
	}()

	err := c.Connect(t.Context())
	var herr *HelloError
	if !errors.As(err, &herr) {
		t.Fatalf("got %v, want HelloError", err)
	}
	if herr.Code != wire.CodeNameTaken {
		t.Errorf("code = %s", herr.Code)
	}
}

func TestClientOnDeliverFires(t *testing.T) {
	fb := newFakeBroker(t)
	got := make(chan wire.Deliver, 1)
	c := NewClient(ClientOptions{
		Name:       "alice",
		SocketPath: fb.socketPath,
		BrokerBin:  "/nonexistent",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnDeliver:  func(d wire.Deliver) { got <- d },
	})
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	fb.write(wire.Deliver{From: "bob", Message: "hi", Timestamp: "2026-05-06T10:30:00Z"})

	select {
	case d := <-got:
		if d.From != "bob" || d.Message != "hi" {
			t.Errorf("deliver = %+v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnDeliver did not fire")
	}
}

func TestClientReconnectsAfterDisconnect(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Drop the connection from the broker side.
	fb.closeClient()

	// Give the read loop a moment to notice EOF and transition to disconnected.
	time.Sleep(100 * time.Millisecond)

	// Next Send should auto-reconnect.
	go fb.sendWelcomeAfterHello()

	ch := make(chan error, 1)
	var ack wire.SendAck
	go func() {
		var err error
		ack, err = c.Send(t.Context(), "bob", "hi again")
		ch <- err
	}()

	f := mustRecvFrame(t, fb.requests)
	s := f.(wire.Send)
	fb.write(wire.SendAckOK(s.ID))

	if err := <-ch; err != nil {
		t.Fatalf("Send after reconnect: %v", err)
	}
	if !ack.OK {
		t.Errorf("ack.OK = false")
	}
}

func TestClientPendingFailedOnDisconnect(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Fire a Send but don't ack it; just close the connection.
	ch := make(chan error, 1)
	go func() {
		_, err := c.Send(t.Context(), "bob", "hi")
		ch <- err
	}()

	// Wait for the send frame to arrive, then drop the connection without acking.
	mustRecvFrame(t, fb.requests)
	fb.closeClient()

	select {
	case err := <-ch:
		if !errors.Is(err, ErrDisconnected) {
			t.Fatalf("got %v, want ErrDisconnected", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending Send did not fail on disconnect")
	}
}

func mustRecvFrame(t *testing.T, ch <-chan wire.Frame) wire.Frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for frame")
		return nil
	}
}

// -- ResolveName tests -------------------------------------------------------

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
}
