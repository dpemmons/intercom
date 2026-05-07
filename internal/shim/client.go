// Package shim is the per-Claude-Code MCP server. It exposes the send_message
// and list_peers tools, declares the claude/channel capability, and forwards
// inbound deliver frames as notifications/claude/channel events.
//
// This file contains the broker client: a single-connection wrapper that
// auto-spawns the broker on first use, pumps inbound frames into a callback,
// and answers send/list_peers requests with id-correlated replies.
package shim

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

// Version is the shim version reported in the hello frame. Overridden at
// build time via -ldflags '-X github.com/dpemmons/intercom/internal/shim.Version=...'.
var Version = "0.0.0-dev"

// dialBackoff lists the sleep durations to wait between dial attempts after
// auto-spawning the broker. Five attempts total (one immediate, four after
// backoffs), ~4.4s total worst-case wall time.
var dialBackoff = []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, time.Second, 3 * time.Second}

// ClientOptions configures a Client.
type ClientOptions struct {
	Name       string // peer name (already validated by the caller)
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

	// connectMu serializes Connect calls so two concurrent callers don't
	// each try to dial + hello and have the broker reject the second as
	// name_taken. Held only during the connect handshake; not during
	// steady-state Send/ListPeers/read.
	connectMu sync.Mutex

	mu      sync.Mutex
	state   state
	conn    net.Conn
	wire    *wire.Conn
	pending map[string]chan wire.Frame
}

type state int

const (
	stateInit state = iota
	stateConnected
	stateDisconnected
	stateClosed
)

// NewClient constructs a client. Network I/O does not start until Connect
// (or one of the request methods, which connects on demand).
func NewClient(opts ClientOptions) *Client {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{opts: opts, pending: make(map[string]chan wire.Frame)}
}

// Connect establishes a connection (auto-spawning the broker if needed) and
// completes the hello/welcome handshake. Safe to call multiple times and
// concurrently: a successful Connect is idempotent; concurrent callers
// serialize on connectMu so the broker only ever sees one hello per peer.
func (c *Client) Connect(ctx context.Context) error {
	c.connectMu.Lock()
	defer c.connectMu.Unlock()

	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	switch state {
	case stateConnected:
		return nil
	case stateClosed:
		return errors.New("shim: client closed")
	}

	conn, err := c.dialOrSpawn(ctx)
	if err != nil {
		return err
	}
	w := wire.NewConn(conn)

	// Hello with a write deadline.
	if err := w.WriteWithTimeout(wire.Hello{Name: c.opts.Name, Version: Version}, 5*time.Second); err != nil {
		_ = conn.Close()
		return fmt.Errorf("shim: write hello: %w", err)
	}

	// Welcome / error with a read deadline.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	first, err := w.Read()
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("shim: await welcome: %w", err)
	}
	switch f := first.(type) {
	case wire.Welcome:
		// proceed
	case wire.Error:
		_ = conn.Close()
		return &HelloError{Code: f.Code, Message: f.Message}
	default:
		_ = conn.Close()
		return fmt.Errorf("shim: unexpected first frame: %v", f.Kind())
	}

	c.mu.Lock()
	c.conn = conn
	c.wire = w
	c.state = stateConnected
	c.mu.Unlock()

	go c.readLoop(conn, w)
	c.opts.Logger.Info("connected to broker", "socket", c.opts.SocketPath)
	return nil
}

// HelloError is returned when the broker rejects our hello. The shim's
// command handler turns this into an actionable user error and exits.
type HelloError struct {
	Code    wire.Code
	Message string
}

func (e *HelloError) Error() string {
	return fmt.Sprintf("broker rejected hello: %s (%s)", e.Message, e.Code)
}

// Close terminates the connection (if any) and refuses further requests.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == stateClosed {
		return nil
	}
	c.state = stateClosed
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
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
			return wire.SendAck{}, fmt.Errorf("shim: ack id mismatch: got %s want %s", r.ID, id)
		}
		return r, nil
	case wire.Error:
		return wire.SendAck{}, fmt.Errorf("broker error (%s): %s", r.Code, r.Message)
	default:
		return wire.SendAck{}, fmt.Errorf("shim: unexpected reply %s", r.Kind())
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
			return nil, fmt.Errorf("shim: reply id mismatch: got %s want %s", r.ID, id)
		}
		return r.Peers, nil
	case wire.Error:
		return nil, fmt.Errorf("broker error (%s): %s", r.Code, r.Message)
	default:
		return nil, fmt.Errorf("shim: unexpected reply %s", r.Kind())
	}
}

// dispatch registers a pending entry, sends the frame, and returns the
// reply channel. The reply channel is closed (with no value) on disconnect.
func (c *Client) dispatch(id string, f wire.Frame) (<-chan wire.Frame, error) {
	c.mu.Lock()
	if c.state != stateConnected {
		c.mu.Unlock()
		return nil, errors.New("shim: not connected to broker")
	}
	ch := make(chan wire.Frame, 1)
	c.pending[id] = ch
	w := c.wire
	c.mu.Unlock()

	if err := w.WriteWithTimeout(f, 5*time.Second); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("shim: write %s: %w", f.Kind(), err)
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
var ErrDisconnected = errors.New("shim: broker disconnected")

// readLoop runs as long as conn is open. It dispatches each inbound frame:
// either resolving a pending request by id, or pushing a deliver to the
// onDeliver callback. On EOF or error it transitions to disconnected and
// fails all pending requests.
func (c *Client) readLoop(conn net.Conn, w *wire.Conn) {
	defer c.afterDisconnect(conn)

	for {
		f, err := w.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) {
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

// afterDisconnect transitions to disconnected and fails all pending requests.
// Idempotent: safe to call once per readLoop exit.
func (c *Client) afterDisconnect(conn net.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == stateClosed {
		// Close already handled the cleanup.
		return
	}
	if c.conn != conn {
		// A newer connection has taken over; nothing to do.
		return
	}
	c.state = stateDisconnected
	c.conn = nil
	c.wire = nil
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
}

// dialOrSpawn dials the broker socket; on failure, spawns a broker and
// retries with backoff. Returns the connected net.Conn or the last error.
func (c *Client) dialOrSpawn(ctx context.Context) (net.Conn, error) {
	if conn, err := dialUnix(c.opts.SocketPath, time.Second); err == nil {
		return conn, nil
	} else if !isStartableErr(err) {
		// E.g. EACCES on the socket: don't bother spawning.
		return nil, fmt.Errorf("shim: dial %s: %w", c.opts.SocketPath, err)
	}

	if err := c.spawnBroker(); err != nil {
		return nil, err
	}

	// Retry with backoff.
	var lastErr error
	for _, delay := range dialBackoff {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		conn, err := dialUnix(c.opts.SocketPath, time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("shim: broker did not start within retry budget: %w", lastErr)
}

// spawnBroker launches the broker as a detached subprocess. We don't wait for
// it; the dial loop polls until it starts listening.
func (c *Client) spawnBroker() error {
	bin := c.opts.BrokerBin
	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("shim: locate self: %w", err)
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
		return fmt.Errorf("shim: spawn broker: %w", err)
	}
	// Reap the child after start so it doesn't become a zombie if it exits
	// before we connect (e.g. lock contention).
	go func() { _ = cmd.Wait() }()
	c.opts.Logger.Info("spawned broker", "bin", bin, "pid", cmd.Process.Pid)
	return nil
}

func dialUnix(path string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.Dial("unix", path)
}

// isStartableErr reports whether dial failed in a way that suggests the
// broker just isn't running (so spawning makes sense). For other errors
// (permission denied, address-too-long, etc.) spawning is futile.
func isStartableErr(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, os.ErrNotExist)
}
