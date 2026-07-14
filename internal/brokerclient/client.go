// Package brokerclient provides a single-connection client for the intercom
// broker. It auto-spawns the broker on first use, pumps inbound frames into a
// callback, and answers send/list_peers requests with id-correlated replies.
package brokerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/dpemmons/intercom/internal/wire"
)

// dialBackoff lists the sleep durations to wait between dial attempts after
// auto-spawning the broker. Five attempts total (one immediate, four after
// backoffs), ~4.4s total worst-case wall time.
var dialBackoff = []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, time.Second, 3 * time.Second}

type dialBrokerFunc func(context.Context, string, time.Duration) (net.Conn, error)
type spawnBrokerFunc func() (<-chan error, error)

// ClientOptions configures a Client.
type ClientOptions struct {
	Name       string // peer name (already validated by the caller)
	Version    string // client version reported in the hello frame
	SocketPath string // broker socket
	BrokerBin  string // path to the broker binary; if empty, os.Executable() is used
	Logger     *slog.Logger
	OnDeliver  func(wire.Deliver)  // called from the read goroutine for each inbound deliver
	OnGoodbye  func(reason string) // called when the broker explicitly sends goodbye
}

// Client owns at most one connection to the broker at a time. It auto-spawns
// the broker on the first connect attempt if no broker is listening, and
// reconnects on demand if the connection drops.
type Client struct {
	opts ClientOptions

	// connectGate serializes Connect calls so two concurrent callers don't
	// each try to dial + hello and have the broker reject the second as
	// name_taken. Unlike a mutex, acquisition can honor a caller's context.
	// Held only during the connect handshake; not during steady-state
	// Send/ListPeers/read.
	connectGate chan struct{}

	mu      sync.Mutex
	state   ConnectionState
	conn    net.Conn
	wire    *wire.Conn
	pending map[string]chan wire.Frame

	// generation increments after every successful hello/welcome handshake.
	// Lifecycle transitions publish while mu is held so events cannot be
	// reordered across generations. eventMu makes replacing the latest unread
	// event atomic.
	generation uint64
	eventMu    sync.Mutex
	events     chan ConnectionEvent

	// These indirections keep dial/spawn retry tests deterministic without
	// requiring helper executables or wall-clock sleeps. They are initialized
	// once by NewClient and are immutable during normal use.
	dialBroker   dialBrokerFunc
	startBroker  spawnBrokerFunc
	retryBackoff []time.Duration
}

// ConnectionState describes the client's current broker connection state.
type ConnectionState uint8

const (
	ConnectionStateInit ConnectionState = iota
	ConnectionStateConnected
	ConnectionStateDisconnected
	ConnectionStateClosed
)

// ConnectionEventCause identifies why a connection state changed.
type ConnectionEventCause string

const (
	ConnectionEventCauseNone       ConnectionEventCause = ""
	ConnectionEventCauseEOF        ConnectionEventCause = "eof"
	ConnectionEventCauseGoodbye    ConnectionEventCause = "goodbye"
	ConnectionEventCauseReadError  ConnectionEventCause = "read_error"
	ConnectionEventCauseWriteError ConnectionEventCause = "write_error"
	ConnectionEventCauseClosed     ConnectionEventCause = "closed"
)

// ConnectionEvent is the latest observed connection state. Generation starts
// at one and advances after each successful handshake. A disconnected event
// retains the generation of the connection that ended; a subsequent connected
// event therefore has a larger generation.
type ConnectionEvent struct {
	State      ConnectionState
	Generation uint64
	Cause      ConnectionEventCause
	// Reason carries the broker-provided text for goodbye events.
	Reason string
	Err    error
}

// NewClient constructs a client. Network I/O does not start until Connect
// (or one of the request methods, which connects on demand).
func NewClient(opts ClientOptions) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Client{
		opts:         opts,
		connectGate:  make(chan struct{}, 1),
		pending:      make(map[string]chan wire.Frame),
		events:       make(chan ConnectionEvent, 1),
		dialBroker:   dialUnix,
		retryBackoff: append([]time.Duration(nil), dialBackoff...),
	}
	c.startBroker = c.spawnBroker
	return c
}

// ConnectionEvents returns a latest-state notification stream for connection
// lifecycle changes. The channel has capacity one: when a consumer falls
// behind, a newer state replaces the older state. Generation lets reconnect
// loops distinguish a new connection from the one that failed. The channel is
// never closed; ConnectionStateClosed is the terminal event.
func (c *Client) ConnectionEvents() <-chan ConnectionEvent { return c.events }

// Connect establishes a connection (auto-spawning the broker if needed) and
// completes the hello/welcome handshake. Safe to call multiple times and
// concurrently: a successful Connect is idempotent; concurrent callers
// serialize on connectGate so the broker only ever sees one hello per peer.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.acquireConnect(ctx); err != nil {
		return err
	}
	defer c.releaseConnect()

	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	switch state {
	case ConnectionStateConnected:
		return nil
	case ConnectionStateClosed:
		return errors.New("broker client: closed")
	}
	if c.opts.Version == "" {
		return errors.New("broker client: version is required")
	}

	conn, err := c.dialOrSpawn(ctx)
	if err != nil {
		return err
	}
	w := wire.NewConn(conn)

	// A handshake connection is not yet owned by the steady-state read loop.
	// Close it on context cancellation to interrupt a blocked hello write or
	// welcome read. stopHandshake joins an already-running callback, preventing
	// the callback from escaping Connect or racing the handoff below.
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(cancelDone)
	})
	cancelStopped := false
	stopHandshake := func() {
		if cancelStopped {
			return
		}
		cancelStopped = true
		if !stopCancel() {
			<-cancelDone
		}
	}
	defer stopHandshake()
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return err
	}

	// Hello with a write deadline.
	if err := w.WriteWithTimeout(wire.Hello{Name: c.opts.Name, Version: c.opts.Version}, 5*time.Second); err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("broker client: write hello: %w", err)
	}

	// Welcome / error with a read deadline.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := w.Read()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("broker client: await welcome: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return err
	}
	switch f := first.(type) {
	case wire.Welcome:
		// proceed
	case wire.Error:
		_ = conn.Close()
		return &HelloError{Code: f.Code, Message: f.Message}
	default:
		_ = conn.Close()
		return fmt.Errorf("broker client: unexpected first frame: %v", f.Kind())
	}

	// Disarm and, if necessary, join the cancellation callback before making
	// conn visible to other goroutines. Cancellation after this handoff does not
	// own the established connection.
	stopHandshake()
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return err
	}

	c.mu.Lock()
	c.conn = conn
	c.wire = w
	c.state = ConnectionStateConnected
	c.generation++
	generation := c.generation
	c.publishConnectionEvent(ConnectionEvent{
		State:      ConnectionStateConnected,
		Generation: generation,
	})
	c.mu.Unlock()

	go c.readLoop(conn, w)
	c.opts.Logger.Info("connected to broker", "socket", c.opts.SocketPath)
	return nil
}

// HelloError is returned when the broker rejects our hello.
type HelloError struct {
	Code    wire.Code
	Message string
}

func (e *HelloError) Error() string {
	return fmt.Sprintf("broker rejected hello: %s (%s)", e.Message, e.Code)
}

func (c *Client) acquireConnect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.connectGate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			c.releaseConnect()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) releaseConnect() { <-c.connectGate }

// Close terminates the connection (if any) and refuses further requests.
func (c *Client) Close() error {
	if err := c.acquireConnect(context.Background()); err != nil {
		return err
	}
	defer c.releaseConnect()

	c.mu.Lock()
	if c.state == ConnectionStateClosed {
		c.mu.Unlock()
		return nil
	}
	c.state = ConnectionStateClosed
	conn := c.conn
	c.conn = nil
	c.wire = nil
	generation := c.generation
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	if conn != nil {
		_ = conn.Close()
	}
	c.publishConnectionEvent(ConnectionEvent{
		State:      ConnectionStateClosed,
		Generation: generation,
		Cause:      ConnectionEventCauseClosed,
	})
	c.mu.Unlock()
	return nil
}

// Send issues a wire.Send and waits for the matching SendAck (or an error /
// disconnect). Reconnects automatically if previously disconnected. Honors
// ctx for cancellation.
func (c *Client) Send(ctx context.Context, to, message string) (wire.SendAck, error) {
	if err := c.Connect(ctx); err != nil {
		return wire.SendAck{}, err
	}
	id := wire.NewID()
	reply, err := c.dispatch(id, wire.Send{ID: id, To: to, Message: message})
	if err != nil {
		return wire.SendAck{}, err
	}
	f, err := c.awaitReply(ctx, id, reply)
	if err != nil {
		return wire.SendAck{}, err
	}
	switch r := f.(type) {
	case wire.SendAck:
		if r.ID != id {
			return wire.SendAck{}, fmt.Errorf("broker client: ack id mismatch: got %s want %s", r.ID, id)
		}
		return r, nil
	case wire.Error:
		return wire.SendAck{}, fmt.Errorf("broker error (%s): %s", r.Code, r.Message)
	default:
		return wire.SendAck{}, fmt.Errorf("broker client: unexpected reply %s", r.Kind())
	}
}

// ListPeers issues a wire.ListPeers and returns the resulting peer list.
func (c *Client) ListPeers(ctx context.Context) ([]string, error) {
	if err := c.Connect(ctx); err != nil {
		return nil, err
	}
	id := wire.NewID()
	reply, err := c.dispatch(id, wire.ListPeers{ID: id})
	if err != nil {
		return nil, err
	}
	f, err := c.awaitReply(ctx, id, reply)
	if err != nil {
		return nil, err
	}
	switch r := f.(type) {
	case wire.ListPeersReply:
		if r.ID != id {
			return nil, fmt.Errorf("broker client: reply id mismatch: got %s want %s", r.ID, id)
		}
		return r.Peers, nil
	case wire.Error:
		return nil, fmt.Errorf("broker error (%s): %s", r.Code, r.Message)
	default:
		return nil, fmt.Errorf("broker client: unexpected reply %s", r.Kind())
	}
}

// dispatch registers a pending entry, sends the frame, and returns the
// reply channel. The reply channel is closed (with no value) on disconnect.
func (c *Client) dispatch(id string, f wire.Frame) (<-chan wire.Frame, error) {
	c.mu.Lock()
	if c.state != ConnectionStateConnected {
		c.mu.Unlock()
		return nil, errors.New("broker client: not connected to broker")
	}
	ch := make(chan wire.Frame, 1)
	c.pending[id] = ch
	conn := c.conn
	w := c.wire
	c.mu.Unlock()

	if err := w.WriteWithTimeout(f, 5*time.Second); err != nil {
		writeErr := fmt.Errorf("broker client: write %s: %w", f.Kind(), err)
		if errors.Is(err, wire.ErrOversize) {
			// wire rejects outbound oversize frames before writing either the
			// header or payload. Remove only this request; the stream remains
			// synchronized and later calls can reuse the connection.
			c.mu.Lock()
			if c.pending[id] == ch {
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return nil, writeErr
		}
		// Any write error makes framing state ambiguous, including an error
		// after a partial frame. Poison exactly the connection used for this
		// write. If its read loop already disconnected and a newer generation
		// has connected, disconnectConnection's identity check leaves the new
		// connection and its pending requests untouched.
		c.disconnectConnection(conn, ConnectionEventCauseWriteError, "", writeErr)
		return nil, writeErr
	}
	return ch, nil
}

// awaitReply blocks until the broker replies, the connection drops, or ctx
// expires. On ctx cancellation it removes the pending entry so a late reply
// from the broker doesn't leak it.
func (c *Client) awaitReply(ctx context.Context, id string, ch <-chan wire.Frame) (wire.Frame, error) {
	select {
	case f, ok := <-ch:
		if !ok {
			return nil, ErrDisconnected
		}
		return f, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// ErrDisconnected is returned from Send/ListPeers when the broker connection
// drops mid-call.
var ErrDisconnected = errors.New("broker client: broker disconnected")

// readLoop runs as long as conn is open. It dispatches each inbound frame:
// either resolving a pending request by id, or pushing a deliver to the
// onDeliver callback. On EOF or error it transitions to disconnected and
// fails all pending requests.
func (c *Client) readLoop(conn net.Conn, w *wire.Conn) {
	cause := ConnectionEventCauseEOF
	var reason string
	var readErr error
	defer func() { c.disconnectConnection(conn, cause, reason, readErr) }()

	for {
		f, err := w.Read()
		if err != nil {
			readErr = err
			if !errors.Is(err, io.EOF) {
				cause = ConnectionEventCauseReadError
				c.opts.Logger.Info("broker read", "err", err)
			}
			return
		}
		switch r := f.(type) {
		case wire.Deliver:
			if c.opts.OnDeliver != nil {
				c.opts.OnDeliver(r)
			}
		case wire.Goodbye:
			cause = ConnectionEventCauseGoodbye
			reason = r.Reason
			readErr = nil
			if c.opts.OnGoodbye != nil {
				c.opts.OnGoodbye(r.Reason)
			}
			return
		case wire.SendAck:
			c.deliverPending(r.ID, r)
		case wire.ListPeersReply:
			c.deliverPending(r.ID, r)
		case wire.Error:
			if r.ID != "" {
				c.deliverPending(r.ID, r)
			} else {
				c.opts.Logger.Warn("unsolicited broker error", "code", r.Code, "msg", r.Message)
			}
		default:
			c.opts.Logger.Warn("unexpected broker frame", "kind", r.Kind())
		}
	}
}

func (c *Client) deliverPending(id string, f wire.Frame) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ok {
		ch <- f
	} else {
		c.opts.Logger.Warn("reply for unknown id", "id", id, "kind", f.Kind())
	}
}

// disconnectConnection atomically poisons one connection, transitions its
// generation to disconnected, fails all requests pending on that generation,
// and publishes the lifecycle event. It is safe to race calls from the read
// loop and writers: only the first call for the active connection wins. A call
// for an obsolete connection cannot disturb a replacement connection.
func (c *Client) disconnectConnection(conn net.Conn, cause ConnectionEventCause, reason string, disconnectErr error) {
	c.mu.Lock()
	if c.state == ConnectionStateClosed {
		// Close already handled the cleanup.
		c.mu.Unlock()
		return
	}
	if c.conn != conn {
		// A newer connection has taken over; nothing to do.
		c.mu.Unlock()
		return
	}
	c.state = ConnectionStateDisconnected
	c.conn = nil
	c.wire = nil
	generation := c.generation
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	_ = conn.Close()
	c.publishConnectionEvent(ConnectionEvent{
		State:      ConnectionStateDisconnected,
		Generation: generation,
		Cause:      cause,
		Reason:     reason,
		Err:        disconnectErr,
	})
	c.mu.Unlock()
}

// publishConnectionEvent stores the newest lifecycle state without blocking a
// connection or wire-reading goroutine. Events are snapshots, so replacing a
// stale unread event is preferable to stalling the connection lifecycle.
func (c *Client) publishConnectionEvent(event ConnectionEvent) {
	c.eventMu.Lock()
	defer c.eventMu.Unlock()
	select {
	case c.events <- event:
		return
	default:
	}
	select {
	case <-c.events:
	default:
	}
	c.events <- event
}

// dialOrSpawn dials the broker socket; on failure, spawns a broker and
// retries with backoff. Returns the connected net.Conn or the last error.
func (c *Client) dialOrSpawn(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var lastErr error
	if conn, err := c.dialBroker(ctx, c.opts.SocketPath, time.Second); err == nil {
		return conn, nil
	} else if !isStartableErr(err) {
		// E.g. EACCES on the socket: don't bother spawning.
		return nil, fmt.Errorf("broker client: dial %s: %w", c.opts.SocketPath, err)
	} else {
		// Retain the initial error for the (possibly empty) retry budget.
		lastErr = err
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exitCh, err := c.startBroker()
	if err != nil {
		return nil, err
	}

	// Retry with backoff.
	for _, delay := range c.retryBackoff {
		timer := time.NewTimer(delay)
		var (
			brokerExited  bool
			brokerExitErr error
		)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case brokerExitErr = <-exitCh:
			brokerExited = true
			exitCh = nil
			timer.Stop()
		case <-timer.C:
		}
		conn, err := c.dialBroker(ctx, c.opts.SocketPath, time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if brokerExited && brokerExitErr != nil {
			return nil, fmt.Errorf("broker client: broker exited before becoming ready: %w (last dial: %v)", brokerExitErr, lastErr)
		}
	}
	return nil, fmt.Errorf("broker client: broker did not start within retry budget: %w", lastErr)
}

// spawnBroker launches the broker as a detached subprocess and returns a
// buffered completion channel. The caller polls for socket readiness without
// synchronously waiting for the long-lived broker process.
func (c *Client) spawnBroker() (<-chan error, error) {
	bin := c.opts.BrokerBin
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("broker client: locate self: %w", err)
		}
		bin = exe
	}
	cmd := exec.Command(bin, "broker")
	// Detach the broker from our process group so it survives our exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("broker client: spawn broker: %w", err)
	}
	// Reap the child after start so it doesn't become a zombie if it exits
	// before we connect (e.g. lock contention), and let dialOrSpawn distinguish
	// a real startup crash from a broker that is merely still warming up.
	exitCh := make(chan error, 1)
	go func() {
		exitCh <- cmd.Wait()
		close(exitCh)
	}()
	c.opts.Logger.Info("spawned broker", "bin", bin, "pid", cmd.Process.Pid)
	return exitCh, nil
}

func dialUnix(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "unix", path)
}

// isStartableErr reports whether dial failed in a way that suggests the
// broker just isn't running (so spawning makes sense). For other errors
// (permission denied, address-too-long, etc.) spawning is futile.
func isStartableErr(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, os.ErrNotExist)
}
