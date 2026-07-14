package brokerclient

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
	"sync/atomic"
	"syscall"
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

type failingWriteConn struct {
	net.Conn
	err error
}

func (c *failingWriteConn) Write([]byte) (int, error) { return 0, c.err }

type gatedFailingWriteConn struct {
	net.Conn
	err     error
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *gatedFailingWriteConn) Write([]byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	<-c.release
	return 0, c.err
}

type observedHandshakeConn struct {
	net.Conn
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
	closeErr     error
}

func newObservedHandshakeConn(conn net.Conn) *observedHandshakeConn {
	return &observedHandshakeConn{
		Conn:         conn,
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *observedHandshakeConn) Write(p []byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writeStarted) })
	return c.Conn.Write(p)
}

func (c *observedHandshakeConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.Conn.Close()
		close(c.closed)
	})
	return c.closeErr
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
		helloCh:    make(chan wire.Hello, 32),
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
		w := wire.NewConn(c)
		fb.mu.Lock()
		// Replace any prior connection. Tests reconnect by closing the
		// previous one — the new one wins.
		fb.raw = c
		fb.wConn = w
		fb.mu.Unlock()
		go fb.readLoop(c, w)
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
		Version:    "test-version",
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

func TestClientOutboundOversizePreservesConnection(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	connected := mustConnectionEvent(t, c)
	if connected.State != ConnectionStateConnected || connected.Generation != 1 {
		t.Fatalf("connected event = %+v", connected)
	}

	_, err := c.Send(t.Context(), "bob", strings.Repeat("\x00", wire.MaxFrameSize))
	if !errors.Is(err, wire.ErrOversize) {
		t.Fatalf("oversize Send error = %v, want ErrOversize", err)
	}
	select {
	case event := <-c.ConnectionEvents():
		t.Fatalf("oversize preflight disconnected client: %+v", event)
	default:
	}
	select {
	case frame := <-fb.requests:
		t.Fatalf("oversize preflight reached broker: %T", frame)
	default:
	}

	result := make(chan error, 1)
	go func() {
		_, err := c.ListPeers(t.Context())
		result <- err
	}()
	frame := mustRecvFrame(t, fb.requests)
	request, ok := frame.(wire.ListPeers)
	if !ok {
		t.Fatalf("request after oversize = %T, want wire.ListPeers", frame)
	}
	fb.write(wire.ListPeersReply{ID: request.ID})
	if err := <-result; err != nil {
		t.Fatalf("ListPeers after oversize: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != ConnectionStateConnected || c.generation != 1 || len(c.pending) != 0 {
		t.Fatalf("post-oversize state=%v generation=%d pending=%d", c.state, c.generation, len(c.pending))
	}
}

func (fb *fakeBroker) sendWelcomeAfterHello() {
	hello := fb.awaitHello()
	if hello.Version != "test-version" {
		fb.t.Errorf("hello version = %q, want test-version", hello.Version)
	}
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

func TestConnectCancellationInterruptsHelloWrite(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	t.Cleanup(func() { _ = serverSide.Close() })
	observed := newObservedHandshakeConn(clientSide)
	c := NewClient(ClientOptions{Name: "alice", Version: "test-version", SocketPath: "/unused"})
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		return observed, nil
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- c.Connect(ctx) }()
	select {
	case <-observed.writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("hello write did not start")
	}

	// net.Pipe writes block until the peer reads. Cancellation must close the
	// handshake connection instead of waiting for the fixed five-second write
	// deadline.
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Connect error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect remained blocked in hello write after cancellation")
	}
	select {
	case <-observed.closed:
		// The cancellation callback is joined before Connect returns.
	default:
		t.Fatal("Connect returned before its cancellation callback closed the connection")
	}
}

func TestConnectCancellationInterruptsWelcomeRead(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	observed := newObservedHandshakeConn(clientSide)
	c := NewClient(ClientOptions{Name: "alice", Version: "test-version", SocketPath: "/unused"})
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		return observed, nil
	}
	t.Cleanup(func() { _ = c.Close() })

	helloRead := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() {
		defer serverSide.Close()
		w := wire.NewConn(serverSide)
		frame, err := w.Read()
		if err == nil {
			if _, ok := frame.(wire.Hello); !ok {
				err = errors.New("first frame was not hello")
			}
		}
		helloRead <- err
		if err != nil {
			serverDone <- err
			return
		}
		_, err = w.Read()
		serverDone <- err
	}()

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- c.Connect(ctx) }()
	if err := <-helloRead; err != nil {
		t.Fatalf("server read hello: %v", err)
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Connect error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Connect remained blocked awaiting welcome after cancellation")
	}
	select {
	case <-observed.closed:
	default:
		t.Fatal("Connect returned before its cancellation callback completed")
	}
	select {
	case err := <-serverDone:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("server read after cancellation = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server handshake goroutine did not observe connection close")
	}
	select {
	case event := <-c.ConnectionEvents():
		t.Fatalf("canceled unestablished connection published event: %+v", event)
	default:
	}
}

func TestQueuedConnectHonorsCancellation(t *testing.T) {
	clientSide, serverSide := net.Pipe()
	observed := newObservedHandshakeConn(clientSide)
	c := NewClient(ClientOptions{Name: "alice", Version: "test-version", SocketPath: "/unused"})
	var dialCalls atomic.Int32
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		dialCalls.Add(1)
		return observed, nil
	}
	t.Cleanup(func() { _ = c.Close() })

	helloRead := make(chan error, 1)
	serverDone := make(chan error, 1)
	go func() {
		defer serverSide.Close()
		w := wire.NewConn(serverSide)
		_, err := w.Read()
		helloRead <- err
		if err != nil {
			serverDone <- err
			return
		}
		_, err = w.Read()
		serverDone <- err
	}()

	firstCtx, cancelFirst := context.WithCancel(t.Context())
	firstResult := make(chan error, 1)
	go func() { firstResult <- c.Connect(firstCtx) }()
	if err := <-helloRead; err != nil {
		t.Fatalf("server read first hello: %v", err)
	}

	secondCtx, cancelSecond := context.WithCancel(t.Context())
	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondResult <- c.Connect(secondCtx)
	}()
	<-secondStarted
	cancelSecond()
	select {
	case err := <-secondResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Connect error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued Connect did not honor cancellation")
	}
	if got := dialCalls.Load(); got != 1 {
		t.Fatalf("dial calls = %d, queued canceled caller should not dial", got)
	}

	cancelFirst()
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first Connect error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("active Connect did not honor cancellation")
	}
	select {
	case err := <-serverDone:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("server read after first cancellation = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server handshake goroutine did not exit")
	}
}

func TestConnectContextDoesNotOwnEstablishedConnection(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	go fb.sendWelcomeAfterHello()
	if err := c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if event := mustConnectionEvent(t, c); event.State != ConnectionStateConnected || event.Generation != 1 {
		t.Fatalf("connected event = %+v", event)
	}
	cancel()

	result := make(chan struct {
		peers []string
		err   error
	}, 1)
	go func() {
		peers, err := c.ListPeers(t.Context())
		result <- struct {
			peers []string
			err   error
		}{peers: peers, err: err}
	}()
	frame := mustRecvFrame(t, fb.requests)
	request, ok := frame.(wire.ListPeers)
	if !ok {
		t.Fatalf("post-cancel request = %T, want wire.ListPeers", frame)
	}
	fb.write(wire.ListPeersReply{ID: request.ID, Peers: []string{"bob"}})
	got := <-result
	if got.err != nil || len(got.peers) != 1 || got.peers[0] != "bob" {
		t.Fatalf("post-cancel ListPeers = %+v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != ConnectionStateConnected || c.generation != 1 {
		t.Fatalf("post-cancel connection state = %v generation=%d", c.state, c.generation)
	}
}

func TestClientOnDeliverFires(t *testing.T) {
	fb := newFakeBroker(t)
	got := make(chan wire.Deliver, 1)
	c := NewClient(ClientOptions{
		Name:       "alice",
		Version:    "test-version",
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
	connected := mustConnectionEvent(t, c)
	if connected.State != ConnectionStateConnected || connected.Generation != 1 {
		t.Fatalf("first connection event = %+v", connected)
	}

	// Drop the connection from the broker side.
	fb.closeClient()
	disconnected := mustConnectionEvent(t, c)
	if disconnected.State != ConnectionStateDisconnected || disconnected.Generation != 1 {
		t.Fatalf("disconnect event = %+v", disconnected)
	}

	// Next Send should auto-reconnect.
	go fb.sendWelcomeAfterHello()

	ch := make(chan error, 1)
	var ack wire.SendAck
	go func() {
		var err error
		ack, err = c.Send(t.Context(), "bob", "hi again")
		ch <- err
	}()

	reconnected := mustConnectionEvent(t, c)
	if reconnected.State != ConnectionStateConnected || reconnected.Generation != 2 {
		t.Fatalf("reconnect event = %+v", reconnected)
	}

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

func TestClientWriteFailureDisconnectsAndFailsPending(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if event := mustConnectionEvent(t, c); event.State != ConnectionStateConnected {
		t.Fatalf("connected event = %+v", event)
	}

	// Leave one ordinary request pending so the failing writer must wake it.
	pending := make(chan error, 1)
	go func() {
		_, err := c.Send(t.Context(), "bob", "pending")
		pending <- err
	}()
	mustRecvFrame(t, fb.requests)

	wantWriteErr := errors.New("forced write failure")
	c.mu.Lock()
	activeConn := c.conn
	c.wire = wire.NewConn(&failingWriteConn{Conn: activeConn, err: wantWriteErr})
	c.mu.Unlock()

	if _, err := c.ListPeers(t.Context()); !errors.Is(err, wantWriteErr) {
		t.Fatalf("ListPeers error = %v, want forced write failure", err)
	}
	select {
	case err := <-pending:
		if !errors.Is(err, ErrDisconnected) {
			t.Fatalf("pending Send error = %v, want ErrDisconnected", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending Send was not failed by write disconnect")
	}

	event := mustConnectionEvent(t, c)
	if event.State != ConnectionStateDisconnected || event.Generation != 1 || event.Cause != ConnectionEventCauseWriteError {
		t.Fatalf("write failure event = %+v", event)
	}
	if !errors.Is(event.Err, wantWriteErr) {
		t.Fatalf("write failure event error = %v", event.Err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != ConnectionStateDisconnected || c.conn != nil || c.wire != nil || len(c.pending) != 0 {
		t.Fatalf("post-write-failure state = %v, conn=%v, wire=%v, pending=%d", c.state, c.conn, c.wire, len(c.pending))
	}
}

func TestClientObsoleteWriteFailureDoesNotPoisonReplacement(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustConnectionEvent(t, c)

	wantWriteErr := errors.New("delayed write failure")
	blocked := &gatedFailingWriteConn{
		err:     wantWriteErr,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	c.mu.Lock()
	oldConn := c.conn
	blocked.Conn = oldConn
	c.wire = wire.NewConn(blocked)
	c.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-blocked.release:
		default:
			close(blocked.release)
		}
	})

	oldCall := make(chan error, 1)
	go func() {
		_, err := c.ListPeers(t.Context())
		oldCall <- err
	}()
	select {
	case <-blocked.started:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation write did not start")
	}

	// Let the read loop win the disconnect race while the writer is blocked.
	fb.closeClient()
	if event := mustConnectionEvent(t, c); event.State != ConnectionStateDisconnected || event.Generation != 1 {
		t.Fatalf("old-generation disconnect event = %+v", event)
	}

	// Establish generation 2 and leave a request pending on it.
	go fb.sendWelcomeAfterHello()
	newCall := make(chan struct {
		ack wire.SendAck
		err error
	}, 1)
	go func() {
		ack, err := c.Send(t.Context(), "bob", "new generation")
		newCall <- struct {
			ack wire.SendAck
			err error
		}{ack: ack, err: err}
	}()
	if event := mustConnectionEvent(t, c); event.State != ConnectionStateConnected || event.Generation != 2 {
		t.Fatalf("replacement connected event = %+v", event)
	}
	frame := mustRecvFrame(t, fb.requests)
	newSend, ok := frame.(wire.Send)
	if !ok {
		t.Fatalf("replacement request = %T, want wire.Send", frame)
	}

	// The old write now fails after generation 2 is active. Its cleanup must be
	// an identity-checked no-op for the replacement connection.
	close(blocked.release)
	if err := <-oldCall; !errors.Is(err, wantWriteErr) {
		t.Fatalf("old-generation call error = %v, want delayed write failure", err)
	}
	select {
	case event := <-c.ConnectionEvents():
		t.Fatalf("obsolete write published an event: %+v", event)
	default:
	}

	fb.write(wire.SendAckOK(newSend.ID))
	result := <-newCall
	if result.err != nil || !result.ack.OK {
		t.Fatalf("replacement request = %+v", result)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != ConnectionStateConnected || c.conn == oldConn || c.generation != 2 {
		t.Fatalf("replacement state = %v, conn reused=%v, generation=%d", c.state, c.conn == oldConn, c.generation)
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

func mustConnectionEvent(t *testing.T, c *Client) ConnectionEvent {
	t.Helper()
	select {
	case event := <-c.ConnectionEvents():
		return event
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for connection event")
		return ConnectionEvent{}
	}
}

func TestClientConnectionEventEOF(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if event := mustConnectionEvent(t, c); event.State != ConnectionStateConnected || event.Generation != 1 {
		t.Fatalf("connected event = %+v", event)
	}

	fb.closeClient()
	event := mustConnectionEvent(t, c)
	if event.State != ConnectionStateDisconnected || event.Generation != 1 || event.Cause != ConnectionEventCauseEOF {
		t.Fatalf("EOF event = %+v", event)
	}
	if !errors.Is(event.Err, io.EOF) {
		t.Fatalf("EOF event err = %v", event.Err)
	}
}

func TestClientConnectionEventGoodbye(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustConnectionEvent(t, c)

	fb.write(wire.Goodbye{Reason: "broker shutdown"})
	event := mustConnectionEvent(t, c)
	if event.State != ConnectionStateDisconnected || event.Generation != 1 || event.Cause != ConnectionEventCauseGoodbye {
		t.Fatalf("goodbye event = %+v", event)
	}
	if event.Reason != "broker shutdown" || event.Err != nil {
		t.Fatalf("goodbye details = %+v", event)
	}
}

func TestClientConnectionEventClose(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)

	go fb.sendWelcomeAfterHello()
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustConnectionEvent(t, c)

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	event := mustConnectionEvent(t, c)
	if event.State != ConnectionStateClosed || event.Generation != 1 || event.Cause != ConnectionEventCauseClosed {
		t.Fatalf("close event = %+v", event)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case extra := <-c.ConnectionEvents():
		t.Fatalf("unexpected event after idempotent close: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClientConcurrentConnectSingleGeneration(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)
	t.Cleanup(func() { _ = c.Close() })

	go fb.sendWelcomeAfterHello()
	const callers = 16
	errCh := make(chan error, callers)
	for range callers {
		go func() { errCh <- c.Connect(t.Context()) }()
	}
	for range callers {
		if err := <-errCh; err != nil {
			t.Fatalf("Connect: %v", err)
		}
	}
	event := mustConnectionEvent(t, c)
	if event.State != ConnectionStateConnected || event.Generation != 1 {
		t.Fatalf("connected event = %+v", event)
	}
	select {
	case hello := <-fb.helloCh:
		t.Fatalf("unexpected additional hello: %+v", hello)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClientCloseConcurrentWithConnect(t *testing.T) {
	fb := newFakeBroker(t)
	c := newTestClient(t, fb)

	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(t.Context()) }()
	hello := fb.awaitHello()
	if hello.Version != "test-version" {
		t.Fatalf("hello version = %q", hello.Version)
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	fb.sendWelcome()
	if err := <-connectDone; err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}

	for {
		event := mustConnectionEvent(t, c)
		if event.State == ConnectionStateClosed {
			if event.Generation != 1 || event.Cause != ConnectionEventCauseClosed {
				t.Fatalf("close event = %+v", event)
			}
			break
		}
	}
	if err := c.Connect(t.Context()); err == nil {
		t.Fatal("Connect after Close succeeded")
	}
}

func TestDialOrSpawnWaitsForReadiness(t *testing.T) {
	c := NewClient(ClientOptions{SocketPath: "/unused"})
	var dialCalls atomic.Int32
	var peer net.Conn
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		if dialCalls.Add(1) < 3 {
			return nil, os.ErrNotExist
		}
		client, server := net.Pipe()
		peer = server
		return client, nil
	}
	var spawnCalls atomic.Int32
	c.startBroker = func() (<-chan error, error) {
		spawnCalls.Add(1)
		return nil, nil
	}
	c.retryBackoff = []time.Duration{0, 0}

	conn, err := c.dialOrSpawn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	_ = peer.Close()
	if got := dialCalls.Load(); got != 3 {
		t.Fatalf("dial calls = %d, want 3", got)
	}
	if got := spawnCalls.Load(); got != 1 {
		t.Fatalf("spawn calls = %d, want 1", got)
	}
}

func TestDialOrSpawnReportsEarlyBrokerExit(t *testing.T) {
	c := NewClient(ClientOptions{SocketPath: "/unused"})
	var dialCalls atomic.Int32
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		dialCalls.Add(1)
		return nil, os.ErrNotExist
	}
	wantExitErr := errors.New("broker startup crashed")
	exitCh := make(chan error, 1)
	exitCh <- wantExitErr
	c.startBroker = func() (<-chan error, error) { return exitCh, nil }
	c.retryBackoff = []time.Duration{time.Hour}

	_, err := c.dialOrSpawn(t.Context())
	if !errors.Is(err, wantExitErr) {
		t.Fatalf("dialOrSpawn error = %v, want broker exit error", err)
	}
	if got := dialCalls.Load(); got != 2 {
		t.Fatalf("dial calls = %d, want initial plus post-exit dial", got)
	}
}

func TestDialOrSpawnCleanEarlyExitKeepsPolling(t *testing.T) {
	c := NewClient(ClientOptions{SocketPath: "/unused"})
	var dialCalls atomic.Int32
	var peer net.Conn
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		if dialCalls.Add(1) < 3 {
			return nil, os.ErrNotExist
		}
		client, server := net.Pipe()
		peer = server
		return client, nil
	}
	// A concurrently spawned broker can make our child lose the singleton
	// lock and exit successfully before the winner has opened its socket.
	exitCh := make(chan error, 1)
	exitCh <- nil
	c.startBroker = func() (<-chan error, error) { return exitCh, nil }
	c.retryBackoff = []time.Duration{time.Hour, 0}

	conn, err := c.dialOrSpawn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	_ = peer.Close()
	if got := dialCalls.Load(); got != 3 {
		t.Fatalf("dial calls = %d, want polling to continue after clean exit", got)
	}
}

func TestDialOrSpawnDoesNotSpawnForFutileDialError(t *testing.T) {
	c := NewClient(ClientOptions{SocketPath: "/unused"})
	wantDialErr := os.NewSyscallError("connect", syscall.EACCES)
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		return nil, wantDialErr
	}
	var spawnCalls atomic.Int32
	c.startBroker = func() (<-chan error, error) {
		spawnCalls.Add(1)
		return nil, nil
	}

	_, err := c.dialOrSpawn(t.Context())
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("dialOrSpawn error = %v, want EACCES", err)
	}
	if got := spawnCalls.Load(); got != 0 {
		t.Fatalf("spawn calls = %d, want 0", got)
	}
}

func TestDialOrSpawnCancellationStopsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := NewClient(ClientOptions{SocketPath: "/unused"})
	var dialCalls atomic.Int32
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		dialCalls.Add(1)
		return nil, os.ErrNotExist
	}
	c.startBroker = func() (<-chan error, error) {
		cancel()
		return nil, nil
	}
	c.retryBackoff = []time.Duration{time.Hour}

	_, err := c.dialOrSpawn(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("dialOrSpawn error = %v, want context.Canceled", err)
	}
	if got := dialCalls.Load(); got != 1 {
		t.Fatalf("dial calls = %d, want no retry after cancellation", got)
	}
}

func TestDialOrSpawnRetryBudgetExhausted(t *testing.T) {
	c := NewClient(ClientOptions{SocketPath: "/unused"})
	var dialCalls atomic.Int32
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		dialCalls.Add(1)
		return nil, os.ErrNotExist
	}
	c.startBroker = func() (<-chan error, error) { return nil, nil }
	c.retryBackoff = []time.Duration{0, 0}

	_, err := c.dialOrSpawn(t.Context())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dialOrSpawn error = %v, want last dial error", err)
	}
	if got := dialCalls.Load(); got != 3 {
		t.Fatalf("dial calls = %d, want initial plus two retries", got)
	}
}

func TestConcurrentConnectAutoSpawnsOnce(t *testing.T) {
	c := NewClient(ClientOptions{
		Name:       "alice",
		Version:    "test-version",
		SocketPath: "/unused",
	})
	var ready atomic.Bool
	var dialCalls atomic.Int32
	helloCh := make(chan wire.Hello, 1)
	serverDone := make(chan error, 1)
	c.dialBroker = func(context.Context, string, time.Duration) (net.Conn, error) {
		dialCalls.Add(1)
		if !ready.Load() {
			return nil, os.ErrNotExist
		}
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			w := wire.NewConn(server)
			frame, err := w.Read()
			if err != nil {
				serverDone <- err
				return
			}
			hello, ok := frame.(wire.Hello)
			if !ok {
				serverDone <- errors.New("first client frame was not hello")
				return
			}
			helloCh <- hello
			if err := w.Write(wire.Welcome{}); err != nil {
				serverDone <- err
				return
			}
			_, err = w.Read()
			serverDone <- err
		}()
		return client, nil
	}
	var spawnCalls atomic.Int32
	c.startBroker = func() (<-chan error, error) {
		spawnCalls.Add(1)
		ready.Store(true)
		return nil, nil
	}
	c.retryBackoff = []time.Duration{0}

	const callers = 32
	errCh := make(chan error, callers)
	for range callers {
		go func() { errCh <- c.Connect(t.Context()) }()
	}
	for range callers {
		if err := <-errCh; err != nil {
			t.Fatalf("Connect: %v", err)
		}
	}
	select {
	case hello := <-helloCh:
		if hello.Name != "alice" || hello.Version != "test-version" {
			t.Fatalf("hello = %+v", hello)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for hello")
	}
	if event := mustConnectionEvent(t, c); event.State != ConnectionStateConnected || event.Generation != 1 {
		t.Fatalf("connected event = %+v", event)
	}
	if got := spawnCalls.Load(); got != 1 {
		t.Fatalf("spawn calls = %d, want 1", got)
	}
	if got := dialCalls.Load(); got != 2 {
		t.Fatalf("dial calls = %d, want initial plus one retry", got)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serverDone:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("server read after client close = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe client close")
	}
}

func TestSpawnBrokerStartFailure(t *testing.T) {
	c := NewClient(ClientOptions{BrokerBin: filepath.Join(t.TempDir(), "missing-intercom")})
	exitCh, err := c.spawnBroker()
	if err == nil {
		t.Fatal("spawnBroker succeeded with a missing binary")
	}
	if exitCh != nil {
		t.Fatal("spawnBroker returned an exit channel after start failure")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spawnBroker error = %v, want os.ErrNotExist", err)
	}
}
