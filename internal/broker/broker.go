// Package broker implements the in-memory router that sits between intercom
// shims on a single host. One broker process serves all sessions; it is
// auto-spawned by the first shim and exits cleanly after a configurable idle
// period with no peers.
//
// Wire protocol: see [github.com/dpemmons/intercom/internal/wire]. The broker
// is the server side; shims are clients.
package broker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/dpemmons/intercom/internal/wire"
)

// Defaults document tunable behavior. Tests use Options to override.
const (
	DefaultIdleAfter       = 10 * time.Minute
	DefaultHelloDeadline   = 5 * time.Second
	DefaultDeliverDeadline = 5 * time.Second
)

// Options configures a Broker.
type Options struct {
	// SocketPath is the Unix socket path the broker listens on.
	SocketPath string
	// LockPath is the flock sentinel used to ensure only one broker runs at a
	// time. If empty, defaults to SocketPath + ".lock".
	LockPath string
	// IdleAfter is how long the broker waits with zero peers before exiting.
	// Zero disables idle exit.
	IdleAfter time.Duration
	// HelloDeadline bounds how long a connection has to send its first
	// frame. Zero disables (not recommended).
	HelloDeadline time.Duration
	// DeliverDeadline bounds how long a deliver write to a peer may block
	// before the peer is dropped as unresponsive. Zero disables.
	DeliverDeadline time.Duration
	// Logger receives structured log events. Required.
	Logger *slog.Logger
}

func (o Options) withDefaults() Options {
	if o.LockPath == "" {
		o.LockPath = o.SocketPath + ".lock"
	}
	if o.IdleAfter == 0 {
		o.IdleAfter = DefaultIdleAfter
	}
	if o.HelloDeadline == 0 {
		o.HelloDeadline = DefaultHelloDeadline
	}
	if o.DeliverDeadline == 0 {
		o.DeliverDeadline = DefaultDeliverDeadline
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return o
}

// Broker is the runtime state of a single broker process.
type Broker struct {
	opts     Options
	listener net.Listener
	lockFile *os.File

	peersMu sync.RWMutex
	peers   map[string]*peer

	shutdownMu sync.Mutex
	shutdown   bool
}

// peer is a registered shim connection. Closing raw also tears down wire.
type peer struct {
	name string
	wire *wire.Conn
	raw  net.Conn
}

// newBroker is the internal constructor (Run is the only public entry point).
func newBroker(opts Options) *Broker {
	opts = opts.withDefaults()
	return &Broker{
		opts:  opts,
		peers: make(map[string]*peer),
	}
}

// Run is the top-level entry point. It acquires the singleton lock, opens the
// listener, serves until ctx is cancelled or the idle timer fires, and cleans
// up on the way out. Returns nil for clean shutdown, [ErrLockHeld] if another
// broker is already running, or a non-nil error for any other startup
// failure.
func Run(ctx context.Context, opts Options) error {
	b := newBroker(opts)
	if err := b.acquireLock(); err != nil {
		return err
	}
	defer b.releaseLock()

	if err := b.listen(); err != nil {
		return err
	}
	defer b.removeSocket()

	return b.serve(ctx)
}

// ErrLockHeld is returned when another broker process holds the singleton
// lock. Callers (typically the shim's auto-spawn path) should treat this as a
// success: another broker is already serving, so we should connect to it
// rather than start our own.
var ErrLockHeld = errors.New("broker: another process holds the lock")

// acquireLock takes an exclusive non-blocking flock on the lock file.
// Returns ErrLockHeld if another process holds it.
func (b *Broker) acquireLock() error {
	f, err := os.OpenFile(b.opts.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("broker: open lock %s: %w", b.opts.LockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrLockHeld
		}
		return fmt.Errorf("broker: flock %s: %w", b.opts.LockPath, err)
	}
	b.lockFile = f
	return nil
}

func (b *Broker) releaseLock() {
	if b.lockFile == nil {
		return
	}
	_ = syscall.Flock(int(b.lockFile.Fd()), syscall.LOCK_UN)
	_ = b.lockFile.Close()
	b.lockFile = nil
}

// listen opens the Unix socket. Any stale socket file at the configured path
// is removed first; it's safe because we hold the singleton lock.
func (b *Broker) listen() error {
	if err := os.Remove(b.opts.SocketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("broker: remove stale socket: %w", err)
	}
	ln, err := net.Listen("unix", b.opts.SocketPath)
	if err != nil {
		return fmt.Errorf("broker: listen %s: %w", b.opts.SocketPath, err)
	}
	if err := os.Chmod(b.opts.SocketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("broker: chmod socket: %w", err)
	}
	b.listener = ln
	b.opts.Logger.Info("broker listening", "socket", b.opts.SocketPath)
	return nil
}

// removeSocket unlinks the socket file. The listener itself is closed inside
// serve when ctx cancels; this defer just cleans up the filesystem entry.
func (b *Broker) removeSocket() {
	if err := os.Remove(b.opts.SocketPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		b.opts.Logger.Warn("remove socket on shutdown", "err", err)
	}
}

// serve runs the accept loop and the idle-exit timer until ctx is cancelled
// or the timer fires.
func (b *Broker) serve(ctx context.Context) error {
	// Cancellable context for the broker's lifetime; the idle timer cancels
	// it from below, ctx cancels it from above.
	innerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Per-connection handler waitgroup, drained on shutdown.
	var wg sync.WaitGroup

	// Idle watcher exits on innerCtx.Done; no extra signal channel needed.
	go b.idleWatcher(innerCtx, cancel)

	// Closing the listener on cancellation makes Accept return; we use that
	// as the exit trigger.
	go func() {
		<-innerCtx.Done()
		_ = b.listener.Close()
	}()

	for {
		conn, err := b.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}
			b.opts.Logger.Warn("accept", "err", err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.handleConn(conn)
		}()
	}

	// Listener closed: send goodbye to remaining peers and drain handlers.
	b.beginShutdown("shutdown")
	wg.Wait()
	return nil
}

// beginShutdown sends a goodbye frame to every connected peer and marks the
// broker as shutting down so subsequent send-routes refuse to schedule new
// deliveries.
func (b *Broker) beginShutdown(reason string) {
	b.shutdownMu.Lock()
	if b.shutdown {
		b.shutdownMu.Unlock()
		return
	}
	b.shutdown = true
	b.shutdownMu.Unlock()

	b.peersMu.RLock()
	conns := make([]*peer, 0, len(b.peers))
	for _, p := range b.peers {
		conns = append(conns, p)
	}
	b.peersMu.RUnlock()

	for _, p := range conns {
		// Best-effort goodbye; ignore errors. Then close so the peer's read
		// loop sees EOF after consuming any buffered data.
		_ = p.wire.WriteWithTimeout(wire.Goodbye{Reason: reason}, time.Second)
		_ = p.raw.Close()
	}
}

// idleWatcher cancels the broker context when IdleAfter elapses with zero
// peers. Exits when ctx is done.
func (b *Broker) idleWatcher(ctx context.Context, cancel context.CancelFunc) {
	if b.opts.IdleAfter <= 0 {
		<-ctx.Done()
		return
	}
	timer := time.NewTimer(b.opts.IdleAfter)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			b.peersMu.RLock()
			n := len(b.peers)
			b.peersMu.RUnlock()
			if n == 0 {
				b.opts.Logger.Info("idle exit", "after", b.opts.IdleAfter)
				cancel()
				return
			}
			// Peers are present: stale fire (peer connected after timer
			// was set). Reset and keep watching.
			timer.Reset(b.opts.IdleAfter)
		}
	}
}

// handleConn drives one client connection: hello → register → message loop →
// deregister.
func (b *Broker) handleConn(raw net.Conn) {
	wConn := wire.NewConn(raw)
	defer raw.Close()

	// Per-connection logger gets a stable id once the peer registers; until
	// then we don't have a name and Unix sockets have no useful remote
	// address, so log lines are unattributed by design.
	logger := b.opts.Logger

	// Hello deadline.
	if b.opts.HelloDeadline > 0 {
		_ = raw.SetReadDeadline(time.Now().Add(b.opts.HelloDeadline))
	}

	first, err := wConn.Read()
	if err != nil {
		if isTimeout(err) {
			logger.Info("hello timeout")
			_ = raw.SetWriteDeadline(time.Now().Add(time.Second))
			_ = wConn.Write(wire.Error{Code: wire.CodeHelloTimeout, Message: "hello not received within deadline"})
			return
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if errors.Is(err, wire.ErrOversize) {
			_ = wConn.Write(wire.Error{Code: wire.CodeOversize, Message: "frame exceeds max size"})
			return
		}
		_ = wConn.Write(wire.Error{Code: wire.CodeBadFrame, Message: err.Error()})
		return
	}

	hello, ok := first.(wire.Hello)
	if !ok {
		_ = wConn.Write(wire.Error{Code: wire.CodeBadHello, Message: "first frame must be hello"})
		return
	}
	if !wire.ValidName(hello.Name) {
		_ = wConn.Write(wire.Error{Code: wire.CodeBadName, Message: "invalid peer name"})
		return
	}

	p := &peer{name: hello.Name, wire: wConn, raw: raw}
	if err := b.register(p); err != nil {
		_ = wConn.Write(wire.Error{Code: wire.CodeNameTaken, Message: err.Error()})
		return
	}
	defer b.deregister(p)

	// Clear hello deadline; from here on, reads block indefinitely awaiting
	// the next request from this peer.
	_ = raw.SetReadDeadline(time.Time{})

	if err := wConn.Write(wire.Welcome{}); err != nil {
		logger.Warn("write welcome", "name", p.name, "err", err)
		return
	}
	logger = logger.With("peer", p.name)
	logger.Info("peer registered", "version", hello.Version)

	// Read loop. Relies on beginShutdown closing the raw conn (after writing
	// goodbye) to unblock Read at shutdown time. Don't add a non-blocking
	// ctx.Done check here — it would race with the first Read and could exit
	// the goroutine before beginShutdown gets a chance to send goodbye.
	for {
		f, err := wConn.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if errors.Is(err, wire.ErrOversize) {
				_ = wConn.Write(wire.Error{Code: wire.CodeOversize, Message: "frame exceeds max size"})
				return
			}
			// Bad frame, or connection closed during shutdown. If shutdown,
			// don't write to a half-closed connection — it'll just error.
			if !b.isShuttingDown() {
				_ = wConn.Write(wire.Error{Code: wire.CodeBadFrame, Message: err.Error()})
			}
			return
		}
		b.routeFrame(p, f)
	}
}

// isShuttingDown reports whether beginShutdown has already started.
func (b *Broker) isShuttingDown() bool {
	b.shutdownMu.Lock()
	defer b.shutdownMu.Unlock()
	return b.shutdown
}

func (b *Broker) register(p *peer) error {
	b.peersMu.Lock()
	defer b.peersMu.Unlock()
	if _, exists := b.peers[p.name]; exists {
		return fmt.Errorf("peer %q already connected", p.name)
	}
	b.peers[p.name] = p
	return nil
}

func (b *Broker) deregister(p *peer) {
	b.peersMu.Lock()
	defer b.peersMu.Unlock()
	// Only delete if this is still the registered conn — avoids races where
	// a name is reused after disconnect and the old goroutine's deferred
	// deregister fires after the new one registered.
	if cur, ok := b.peers[p.name]; ok && cur == p {
		delete(b.peers, p.name)
	}
}

// routeFrame dispatches a single post-hello frame from peer p.
func (b *Broker) routeFrame(p *peer, f wire.Frame) {
	switch f := f.(type) {
	case wire.Send:
		b.handleSend(p, f)
	case wire.ListPeers:
		b.handleListPeers(p, f)
	default:
		// Unexpected frames after hello: error and close.
		_ = p.wire.Write(wire.Error{Code: wire.CodeBadFrame, Message: fmt.Sprintf("unexpected frame %s after hello", f.Kind())})
		_ = p.raw.Close()
	}
}

func (b *Broker) handleSend(from *peer, s wire.Send) {
	// Self-send: reject.
	if s.To == from.name {
		_ = from.wire.Write(wire.SendAckErr(s.ID, wire.CodeNoSelfSend, "cannot send to self"))
		return
	}

	b.peersMu.RLock()
	dest, ok := b.peers[s.To]
	b.peersMu.RUnlock()
	if !ok {
		_ = from.wire.Write(wire.SendAckErr(s.ID, wire.CodeNoSuchPeer, "no such peer: "+s.To))
		return
	}

	// Bounded delivery: the per-frame write deadline is set inside
	// wire.Conn's write mutex, so concurrent senders to the same destination
	// can't clobber each other's deadlines.
	err := dest.wire.WriteWithTimeout(wire.Deliver{
		From:      from.name,
		Message:   s.Message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}, b.opts.DeliverDeadline)

	if err != nil {
		// Drop the destination from the registry — it's effectively gone.
		b.deregister(dest)
		_ = dest.raw.Close()
		_ = from.wire.Write(wire.SendAckErr(s.ID, wire.CodeDeliverFailed, "delivery failed: "+err.Error()))
		return
	}
	_ = from.wire.Write(wire.SendAckOK(s.ID))
}

func (b *Broker) handleListPeers(from *peer, lp wire.ListPeers) {
	b.peersMu.RLock()
	out := make([]string, 0, len(b.peers))
	for name := range b.peers {
		if name != from.name {
			out = append(out, name)
		}
	}
	b.peersMu.RUnlock()
	sort.Strings(out)
	_ = from.wire.Write(wire.ListPeersReply{ID: lp.ID, Peers: out})
}

// isTimeout matches the deadline-exceeded errors that can come back from a
// blocking Read after SetReadDeadline.
func isTimeout(err error) bool {
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return errors.Is(err, os.ErrDeadlineExceeded)
}
