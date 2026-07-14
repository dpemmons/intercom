package broker

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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

// oneConnListener hands serve exactly one connection, then blocks in Accept
// until Close. accepted lets tests cancel only after the connection has
// crossed the listener boundary.
type oneConnListener struct {
	pending   chan net.Conn
	accepted  chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	addr      net.Addr
}

// blockingWriteConn keeps one wire write parked until Close. Its channels are
// created inside synctest bubbles, so queue-deadline tests advance fake time
// deterministically without depending on socket buffer sizes.
type blockingWriteConn struct {
	writeStarted chan struct{}
	closed       chan struct{}
	writeOnce    sync.Once
	closeOnce    sync.Once
}

func newBlockingWriteConn() *blockingWriteConn {
	return &blockingWriteConn{
		writeStarted: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *blockingWriteConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, io.EOF
}

func (c *blockingWriteConn) Write([]byte) (int, error) {
	c.writeOnce.Do(func() { close(c.writeStarted) })
	<-c.closed
	return 0, net.ErrClosed
}

func (c *blockingWriteConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (*blockingWriteConn) LocalAddr() net.Addr              { return nil }
func (*blockingWriteConn) RemoteAddr() net.Addr             { return nil }
func (*blockingWriteConn) SetDeadline(time.Time) error      { return nil }
func (*blockingWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (*blockingWriteConn) SetWriteDeadline(time.Time) error { return nil }

func newOneConnListener(conn net.Conn) *oneConnListener {
	pending := make(chan net.Conn, 1)
	pending <- conn
	return &oneConnListener{
		pending:  pending,
		accepted: make(chan struct{}),
		closed:   make(chan struct{}),
		addr:     conn.LocalAddr(),
	}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.pending:
		close(l.accepted)
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConnListener) Addr() net.Addr { return l.addr }

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
	// The fixture owns every client connection for the duration of the test.
	// Merely returning c is insufficient: once a test stops using its
	// *wire.Conn, the net package's finalizer may close the underlying socket
	// and make a logically connected peer disappear during an unrelated GC.
	f.t.Cleanup(func() { _ = c.Close() })
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

func TestHelloMalformedAndOversizeFramesCloseConnection(t *testing.T) {
	for _, tt := range []struct {
		name string
		code wire.Code
		send func(*testing.T, net.Conn)
	}{
		{name: "malformed", code: wire.CodeBadFrame, send: func(t *testing.T, conn net.Conn) {
			writeRawFrame(t, conn, []byte(`not-json`))
		}},
		{name: "oversize", code: wire.CodeOversize, send: func(t *testing.T, conn net.Conn) {
			writeRawLength(t, conn, wire.MaxFrameSize+1)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			w, raw := f.dial()
			tt.send(t, raw)
			assertWireErrorThenClose(t, w, raw, tt.code)
		})
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
	// The fixture, not local variable liveness, owns connected peers.
	runtime.GC()

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

	// Bob should see a deliver carrying the exact originating request ID.
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
	if d.ID != id {
		t.Errorf("deliver id = %q, want %q", d.ID, id)
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

func TestOversizeDeliveryRejectsSendWithoutDroppingDestination(t *testing.T) {
	f := newFixture(t, Options{})
	wAlice, _ := f.dial()
	helloSync(t, wAlice, "alice")
	wBob, _ := f.dial()
	helloSync(t, wBob, "bob")

	emptySend, err := wire.EncodedFrameSize(wire.Send{ID: "1", To: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	// Quotes occupy two bytes once JSON-escaped. Fill the incoming Send as
	// close to its limit as possible; the broker-added delivery metadata then
	// pushes Deliver over the limit.
	message := strings.Repeat(`"`, (wire.MaxFrameSize-emptySend)/2)
	send := wire.Send{ID: "1", To: "bob", Message: message}
	sendSize, err := wire.EncodedFrameSize(send)
	if err != nil {
		t.Fatal(err)
	}
	deliverSize, err := wire.EncodedFrameSize(wire.Deliver{
		ID: "1", From: "alice", Message: message, Timestamp: "2026-07-13T12:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sendSize > wire.MaxFrameSize || deliverSize <= wire.MaxFrameSize {
		t.Fatalf("test boundary send=%d deliver=%d max=%d", sendSize, deliverSize, wire.MaxFrameSize)
	}

	if err := wAlice.Write(send); err != nil {
		t.Fatal(err)
	}
	frame, err := wAlice.Read()
	if err != nil {
		t.Fatal(err)
	}
	ack, ok := frame.(wire.SendAck)
	if !ok || ack.OK || ack.Code != wire.CodeOversize {
		t.Fatalf("oversize delivery ack = %#v, want oversize", frame)
	}

	if err := wAlice.Write(wire.ListPeers{ID: "after-oversize"}); err != nil {
		t.Fatal(err)
	}
	frame, err = wAlice.Read()
	if err != nil {
		t.Fatal(err)
	}
	peers, ok := frame.(wire.ListPeersReply)
	if !ok || len(peers.Peers) != 1 || peers.Peers[0] != "bob" {
		t.Fatalf("peers after oversize delivery = %#v, want bob retained", frame)
	}
}

func TestSendNotRoutedAfterShutdownBegins(t *testing.T) {
	b := newBroker(Options{})
	b.beginShutdown("shutdown")

	var stream bytes.Buffer
	from := &peer{name: "alice", wire: wire.NewConn(&stream)}
	b.handleSend(from, wire.Send{ID: "after-shutdown", To: "bob", Message: "hi"})
	if stream.Len() != 0 {
		t.Fatalf("send after shutdown wrote %d bytes, want no competing response", stream.Len())
	}
}

func TestDeliverDeadlineDropsUnresponsivePeer(t *testing.T) {
	f := newFixture(t, Options{DeliverDeadline: 10 * time.Millisecond})
	wAlice, _ := f.dial()
	helloSync(t, wAlice, "alice")
	wBob, rawBob := f.dial()
	defer rawBob.Close()
	helloSync(t, wBob, "bob")

	// Bob deliberately never reads. Fill the Unix socket receive buffer with
	// large, individually valid frames until the bounded broker write expires.
	message := strings.Repeat("x", wire.MaxFrameSize-1024)
	failed := false
	for i := 0; i < 100; i++ {
		id := "blocked-" + strconvI(i)
		if err := wAlice.Write(wire.Send{ID: id, To: "bob", Message: message}); err != nil {
			t.Fatal(err)
		}
		frame, err := wAlice.Read()
		if err != nil {
			t.Fatal(err)
		}
		ack, ok := frame.(wire.SendAck)
		if !ok || ack.ID != id {
			t.Fatalf("ack = %#v, want id %q", frame, id)
		}
		if !ack.OK {
			if ack.Code != wire.CodeDeliverFailed {
				t.Fatalf("failure ack = %#v, want deliver_failed", ack)
			}
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("destination writes never reached the delivery deadline")
	}

	if err := wAlice.Write(wire.ListPeers{ID: "after-timeout"}); err != nil {
		t.Fatal(err)
	}
	frame, err := wAlice.Read()
	if err != nil {
		t.Fatal(err)
	}
	peers, ok := frame.(wire.ListPeersReply)
	if !ok || len(peers.Peers) != 0 {
		t.Fatalf("peers after delivery timeout = %#v, want bob removed", frame)
	}
}

func TestDeliverDeadlineIncludesQueuedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const budget = 2 * time.Second
		b := newBroker(Options{DeliverDeadline: budget})
		var replies bytes.Buffer
		from := &peer{name: "alice", wire: wire.NewConn(&replies)}
		rawDest := newBlockingWriteConn()
		dest := &peer{name: "bob", wire: wire.NewConn(rawDest), raw: rawDest}
		if err := b.register(from); err != nil {
			t.Fatal(err)
		}
		if err := b.register(dest); err != nil {
			t.Fatal(err)
		}

		blockingWrite := make(chan error, 1)
		go func() {
			blockingWrite <- dest.wire.Write(wire.ListPeersReply{ID: "already-writing"})
		}()
		<-rawDest.writeStarted

		start := time.Now()
		b.handleSend(from, wire.Send{ID: "queued-delivery", To: "bob", Message: "hello"})
		if elapsed := time.Since(start); elapsed != budget {
			t.Fatalf("queued delivery elapsed = %v, want total budget %v", elapsed, budget)
		}
		select {
		case <-rawDest.closed:
		default:
			t.Fatal("timed-out destination was not closed")
		}
		if err := <-blockingWrite; err == nil {
			t.Fatal("older blocking write succeeded after destination close")
		}

		frame, err := wire.NewConn(&replies).Read()
		if err != nil {
			t.Fatalf("read delivery failure ack: %v", err)
		}
		ack, ok := frame.(wire.SendAck)
		if !ok || ack.OK || ack.ID != "queued-delivery" || ack.Code != wire.CodeDeliverFailed {
			t.Fatalf("delivery failure ack = %#v", frame)
		}
		b.peersMu.RLock()
		_, stillRegistered := b.peers[dest.name]
		b.peersMu.RUnlock()
		if stillRegistered {
			t.Fatal("timed-out destination remains registered")
		}
	})
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
	// Force finalizers here so removing fixture connection ownership makes
	// this regression fail instead of depending on incidental GC timing.
	runtime.GC()

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

func TestRegisteredPeerMalformedAndOversizeFramesCloseConnection(t *testing.T) {
	for _, tt := range []struct {
		name string
		code wire.Code
		send func(*testing.T, net.Conn)
	}{
		{name: "malformed", code: wire.CodeBadFrame, send: func(t *testing.T, conn net.Conn) {
			writeRawFrame(t, conn, []byte(`{"kind":`))
		}},
		{name: "oversize", code: wire.CodeOversize, send: func(t *testing.T, conn net.Conn) {
			writeRawLength(t, conn, wire.MaxFrameSize+1)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, Options{})
			w, raw := f.dial()
			helloSync(t, w, "alice")
			tt.send(t, raw)
			assertWireErrorThenClose(t, w, raw, tt.code)
		})
	}
}

// TestConcurrentSendsPreserveOrder verifies that the broker preserves
// per-sender → per-receiver ordering even with many sends in flight. This
// holds because each connection has its own write gate (via wire.Conn).
func TestConcurrentSendsPreserveOrder(t *testing.T) {
	f := newFixture(t, Options{})
	wA, _ := f.dial()
	helloSync(t, wA, "alice")
	wB, _ := f.dial()
	helloSync(t, wB, "bob")

	const N = 30

	// Bob reads delivers; each carries the message. We assert they arrive in
	// the order alice sent them (0..N-1).
	delivered := make(chan wire.Deliver, N)
	go func() {
		for i := 0; i < N; i++ {
			f, err := wB.Read()
			if err != nil {
				delivered <- wire.Deliver{}
				return
			}
			d, ok := f.(wire.Deliver)
			if !ok {
				delivered <- wire.Deliver{}
				return
			}
			delivered <- d
		}
	}()

	// Alice fires sends in order. They serialize through her write gate on
	// wire.Conn so the broker reads them in order, and the broker's per-conn
	// write gate on bob's conn means it writes to bob in order.
	for i := 0; i < N; i++ {
		if err := wA.Write(wire.Send{ID: "id-" + strconvI(i), To: "bob", Message: strconvI(i)}); err != nil {
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
			if got.Message != strconvI(i) {
				t.Fatalf("frame %d: message got %q want %q", i, got.Message, strconvI(i))
			}
			if got.ID != "id-"+strconvI(i) {
				t.Fatalf("frame %d: id got %q want %q", i, got.ID, "id-"+strconvI(i))
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

func writeRawFrame(t *testing.T, conn net.Conn, body []byte) {
	t.Helper()
	writeRawLength(t, conn, len(body))
	if _, err := conn.Write(body); err != nil {
		t.Fatalf("write raw body: %v", err)
	}
}

func writeRawLength(t *testing.T, conn net.Conn, length int) {
	t.Helper()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(length))
	if _, err := conn.Write(header[:]); err != nil {
		t.Fatalf("write raw length: %v", err)
	}
}

func assertWireErrorThenClose(t *testing.T, conn *wire.Conn, raw net.Conn, code wire.Code) {
	t.Helper()
	frame, err := conn.Read()
	if err != nil {
		t.Fatalf("read protocol error: %v", err)
	}
	protocolErr, ok := frame.(wire.Error)
	if !ok || protocolErr.Code != code {
		t.Fatalf("protocol response = %#v, want %s error", frame, code)
	}
	if err := raw.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("read after terminal protocol error = %v, want EOF", err)
	}
}

func TestShutdownClosesAcceptedConnectionBeforeHello(t *testing.T) {
	b := newBroker(Options{
		IdleAfter:     IdleExitDisabled,
		HelloDeadline: -1, // disabled: shutdown itself must interrupt Read
	})
	server, client := net.Pipe()
	listener := newOneConnListener(server)
	b.listener = listener

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = b.serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		_ = client.Close()
		_ = server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("serve did not exit during cleanup")
		}
	})

	select {
	case <-listener.accepted:
	case <-time.After(time.Second):
		t.Fatal("serve did not accept the test connection")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serve did not close a pre-Hello connection during shutdown")
	}
	if serveErr != nil {
		t.Fatalf("serve returned %v", serveErr)
	}

	var buf [1]byte
	if _, err := client.Read(buf[:]); !errors.Is(err, io.EOF) {
		t.Fatalf("client read after shutdown = %v, want EOF", err)
	}
}

func TestShutdownGoodbyeDeadlineIncludesQueuedWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBroker(Options{})
		raw := newBlockingWriteConn()
		p := &peer{name: "alice", wire: wire.NewConn(raw), raw: raw}
		if err := b.register(p); err != nil {
			t.Fatal(err)
		}

		blockingWrite := make(chan error, 1)
		go func() {
			blockingWrite <- p.wire.Write(wire.ListPeersReply{ID: "already-writing"})
		}()
		<-raw.writeStarted

		start := time.Now()
		b.beginShutdown("shutdown")
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Fatalf("shutdown elapsed = %v, want Goodbye budget %v", elapsed, time.Second)
		}
		select {
		case <-raw.closed:
		default:
			t.Fatal("shutdown did not close peer after queued Goodbye timed out")
		}
		if err := <-blockingWrite; err == nil {
			t.Fatal("older blocking response succeeded after shutdown close")
		}
	})
}

func TestRegisterRejectsAfterShutdownBegins(t *testing.T) {
	b := newBroker(Options{})
	b.beginShutdown("shutdown")

	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})
	p := &peer{name: "late", wire: wire.NewConn(server), raw: server}
	if err := b.register(p); !errors.Is(err, errShuttingDown) {
		t.Fatalf("register after shutdown = %v, want errShuttingDown", err)
	}
	b.peersMu.RLock()
	n := len(b.peers)
	b.peersMu.RUnlock()
	if n != 0 {
		t.Fatalf("registered peers after shutdown = %d, want 0", n)
	}
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

func TestIdleExitRequiresContinuousEmptyPeriod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const idleAfter = time.Hour
		b := newBroker(Options{IdleAfter: idleAfter})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			b.idleWatcher(ctx, cancel)
			close(done)
		}()
		synctest.Wait()

		// Connect halfway through the initial empty interval and keep the
		// peer across the original idle deadline.
		time.Sleep(idleAfter / 2)
		p := &peer{name: "alice"}
		if err := b.register(p); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(idleAfter)
		synctest.Wait()
		if err := ctx.Err(); err != nil {
			t.Fatalf("watcher exited while peer was connected: %v", err)
		}

		emptyAt := time.Now()
		b.deregister(p)
		synctest.Wait()
		time.Sleep(idleAfter - time.Nanosecond)
		synctest.Wait()
		if err := ctx.Err(); err != nil {
			t.Fatalf("watcher exited after %v empty, before IdleAfter: %v", time.Since(emptyAt), err)
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if err := ctx.Err(); !errors.Is(err, context.Canceled) {
			t.Fatalf("watcher state after IdleAfter empty = %v, want canceled", err)
		}
		select {
		case <-done:
		default:
			t.Fatal("idle watcher did not return after canceling the context")
		}
		if elapsed := time.Since(emptyAt); elapsed != idleAfter {
			t.Fatalf("idle exit after %v, want %v", elapsed, idleAfter)
		}
	})
}

func TestIdleWatcherReturnsOnCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := newBroker(Options{IdleAfter: time.Hour})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			b.idleWatcher(ctx, cancel)
			close(done)
		}()
		synctest.Wait()

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("idle watcher leaked after context cancellation")
		}
	})
}

func TestIdleExitCanBeDisabled(t *testing.T) {
	f := newFixture(t, Options{IdleAfter: IdleExitDisabled})
	select {
	case <-f.done:
		t.Fatalf("broker exited with idle exit disabled: %v", f.runErrLocked())
	case <-time.After(100 * time.Millisecond):
	}
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(2 * time.Second):
		t.Fatal("broker did not exit after explicit cancellation")
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
