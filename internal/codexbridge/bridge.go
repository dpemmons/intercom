// Package codexbridge carries Intercom tool calls from a Codex MCP helper to
// the controller process that owns the Intercom broker connection.
//
// The bridge is deliberately private and small: one authenticated JSON frame
// is exchanged per Unix-socket connection. It does not connect to the broker.
package codexbridge

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dpemmons/intercom/internal/intercomtools"
	"github.com/dpemmons/intercom/internal/wire"
)

const (
	protocolVersion = 1

	// MaxFrameSize bounds both requests and responses on the private bridge.
	// It is larger than the broker frame because a bridge request also carries
	// authentication and Codex routing metadata.
	MaxFrameSize = 1 << 20

	defaultTimeout       = 10 * time.Second
	defaultMaxConcurrent = 64
	minimumTokenBytes    = 32
	maximumTokenBytes    = 512

	methodPing        = "ping"
	methodSendMessage = intercomtools.SendMessageName
	methodListPeers   = intercomtools.ListPeersName
)

var (
	ErrFrameTooLarge = errors.New("codex bridge: frame too large")
)

// Handler is implemented by the controller that owns the broker connection.
// metadata is the raw tools/call _meta value supplied by Codex. It is nil when
// _meta was absent and contains "null" when Codex explicitly supplied null.
type Handler interface {
	SendMessage(ctx context.Context, metadata json.RawMessage, to, message string) (wire.SendAck, error)
	ListPeers(ctx context.Context, metadata json.RawMessage) ([]string, error)
}

// HandlerFuncs adapts functions to Handler.
type HandlerFuncs struct {
	SendMessageFunc func(context.Context, json.RawMessage, string, string) (wire.SendAck, error)
	ListPeersFunc   func(context.Context, json.RawMessage) ([]string, error)
}

func (h HandlerFuncs) SendMessage(ctx context.Context, metadata json.RawMessage, to, message string) (wire.SendAck, error) {
	if h.SendMessageFunc == nil {
		return wire.SendAck{}, errors.New("codex bridge: send_message handler is unavailable")
	}
	return h.SendMessageFunc(ctx, metadata, to, message)
}

func (h HandlerFuncs) ListPeers(ctx context.Context, metadata json.RawMessage) ([]string, error) {
	if h.ListPeersFunc == nil {
		return nil, errors.New("codex bridge: list_peers handler is unavailable")
	}
	return h.ListPeersFunc(ctx, metadata)
}

// GenerateToken returns a cryptographically random token suitable for Options
// and ClientOptions. The token must be passed to the helper through a private
// channel such as its environment, not written into the binding state file.
func GenerateToken() (string, error) {
	var raw [32]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return "", fmt.Errorf("codex bridge: generate token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// Options configures a controller-side bridge listener.
type Options struct {
	SocketPath     string
	Token          string
	Handler        Handler
	RequestTimeout time.Duration
	MaxConcurrent  int
}

// Controller owns the private Unix listener. Listen starts its accept loop;
// Close stops it, waits for active calls, and removes the socket.
type Controller struct {
	opts Options
	path string

	ctx    context.Context
	cancel context.CancelFunc
	ln     net.Listener
	info   os.FileInfo
	sem    chan struct{}

	mu     sync.Mutex
	active map[net.Conn]struct{}

	wg        sync.WaitGroup
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Listen validates the private parent directory, creates SocketPath with mode
// 0600, and begins serving. The immediate parent must be a real mode-0700
// directory owned by the current effective user.
func Listen(parent context.Context, opts Options) (*Controller, error) {
	if parent == nil {
		return nil, errors.New("codex bridge: parent context is nil")
	}
	if opts.Handler == nil {
		return nil, errors.New("codex bridge: handler is required")
	}
	if err := validateToken(opts.Token); err != nil {
		return nil, err
	}
	path, err := validateNewSocketPath(opts.SocketPath)
	if err != nil {
		return nil, err
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultTimeout
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = defaultMaxConcurrent
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("codex bridge: listen %s: %w", path, err)
	}
	// UnixListener otherwise unlinks its pathname on Close without checking
	// whether another process replaced it. Cleanup below verifies identity.
	if unixListener, ok := ln.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	cleanup := func() {
		_ = ln.Close()
		_ = os.Remove(path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("codex bridge: chmod socket: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("codex bridge: inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		cleanup()
		return nil, fmt.Errorf("codex bridge: socket %q has type/mode %v, want socket 0600", path, info.Mode())
	}

	ctx, cancel := context.WithCancel(parent)
	c := &Controller{
		opts:   opts,
		path:   path,
		ctx:    ctx,
		cancel: cancel,
		ln:     ln,
		info:   info,
		sem:    make(chan struct{}, opts.MaxConcurrent),
		active: make(map[net.Conn]struct{}),
		done:   make(chan struct{}),
	}
	go c.serve()
	go func() {
		<-ctx.Done()
		_ = c.Close()
	}()
	return c, nil
}

// SocketPath returns the canonical path owned by the controller.
func (c *Controller) SocketPath() string {
	if c == nil {
		return ""
	}
	return c.path
}

// Done closes after the accept loop and all active handlers have exited.
func (c *Controller) Done() <-chan struct{} {
	if c == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.done
}

// Close stops the controller. It is safe to call concurrently.
func (c *Controller) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.cancel()
		if err := c.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			c.closeErr = err
		}
		c.mu.Lock()
		for conn := range c.active {
			_ = conn.Close()
		}
		c.mu.Unlock()
		<-c.done
		if err := removeIfSame(c.path, c.info); err != nil && c.closeErr == nil {
			c.closeErr = err
		}
	})
	return c.closeErr
}

func (c *Controller) serve() {
	defer close(c.done)
	for {
		conn, err := c.ln.Accept()
		if err != nil {
			if c.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			continue
		}
		select {
		case c.sem <- struct{}{}:
		case <-c.ctx.Done():
			_ = conn.Close()
			break
		}
		if c.ctx.Err() != nil {
			_ = conn.Close()
			break
		}
		c.mu.Lock()
		c.active[conn] = struct{}{}
		c.mu.Unlock()
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer func() { <-c.sem }()
			defer func() {
				c.mu.Lock()
				delete(c.active, conn)
				c.mu.Unlock()
				_ = conn.Close()
			}()
			c.handle(conn)
		}()
	}
	c.wg.Wait()
}

type request struct {
	Version      int             `json:"version"`
	ID           uint64          `json:"id"`
	Token        string          `json:"token"`
	Method       string          `json:"method"`
	Params       json.RawMessage `json:"params,omitempty"`
	Meta         json.RawMessage `json:"_meta,omitempty"`
	DeadlineUnix int64           `json:"deadlineUnixNano,omitempty"`
}

type response struct {
	Version int             `json:"version"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RemoteError    `json:"error,omitempty"`
}

// RemoteError is a controller-side failure returned over the private bridge.
type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RemoteError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("codex bridge remote error (%s): %s", e.Code, e.Message)
}

func (c *Controller) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(c.opts.RequestTimeout))
	raw, err := readFrame(conn)
	if err != nil {
		c.writeResponse(conn, response{Version: protocolVersion, Error: &RemoteError{Code: "invalid_request", Message: err.Error()}})
		return
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		c.writeResponse(conn, response{Version: protocolVersion, Error: &RemoteError{Code: "invalid_request", Message: "invalid JSON request"}})
		return
	}
	resp := response{Version: protocolVersion, ID: req.ID}
	if req.Version != protocolVersion {
		resp.Error = &RemoteError{Code: "unsupported_version", Message: fmt.Sprintf("unsupported protocol version %d", req.Version)}
		c.writeResponse(conn, resp)
		return
	}
	if !tokensEqual(c.opts.Token, req.Token) {
		resp.Error = &RemoteError{Code: "unauthorized", Message: "authentication failed"}
		c.writeResponse(conn, resp)
		return
	}

	handlerCtx, cancel := context.WithTimeout(c.ctx, c.opts.RequestTimeout)
	if req.DeadlineUnix > 0 {
		deadline := time.Unix(0, req.DeadlineUnix)
		if current, ok := handlerCtx.Deadline(); !ok || deadline.Before(current) {
			cancel()
			handlerCtx, cancel = context.WithDeadline(c.ctx, deadline)
		}
	}
	defer cancel()
	_ = conn.SetReadDeadline(time.Time{})
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		cancel()
	}()

	result, remoteErr := c.dispatch(handlerCtx, req)
	resp.Result = result
	resp.Error = remoteErr
	c.writeResponse(conn, resp)
}

func (c *Controller) dispatch(ctx context.Context, req request) (json.RawMessage, *RemoteError) {
	switch req.Method {
	case methodPing:
		return json.RawMessage(`{}`), nil
	case methodSendMessage:
		args, err := intercomtools.DecodeSendMessage(req.Params)
		if err != nil {
			return nil, &RemoteError{Code: "invalid_params", Message: err.Error()}
		}
		ack, err := c.opts.Handler.SendMessage(ctx, cloneRaw(req.Meta), args.To, args.Message)
		if err != nil {
			return nil, handlerRemoteError(ctx, err)
		}
		return marshalResult(ack)
	case methodListPeers:
		if err := intercomtools.DecodeListPeers(req.Params); err != nil {
			return nil, &RemoteError{Code: "invalid_params", Message: err.Error()}
		}
		peers, err := c.opts.Handler.ListPeers(ctx, cloneRaw(req.Meta))
		if err != nil {
			return nil, handlerRemoteError(ctx, err)
		}
		return marshalResult(struct {
			Peers []string `json:"peers"`
		}{Peers: peers})
	default:
		return nil, &RemoteError{Code: "method_not_found", Message: "unknown method: " + req.Method}
	}
}

func handlerRemoteError(ctx context.Context, err error) *RemoteError {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &RemoteError{Code: "deadline_exceeded", Message: ctxErr.Error()}
	}
	return &RemoteError{Code: "handler_error", Message: err.Error()}
}

func marshalResult(value any) (json.RawMessage, *RemoteError) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, &RemoteError{Code: "internal", Message: "encode handler result: " + err.Error()}
	}
	return raw, nil
}

func (c *Controller) writeResponse(conn net.Conn, resp response) {
	_ = conn.SetWriteDeadline(time.Now().Add(c.opts.RequestTimeout))
	_ = writeFrame(conn, resp)
}

// ClientOptions configures calls from the MCP helper to a Controller.
type ClientOptions struct {
	SocketPath string
	Token      string
	Timeout    time.Duration
}

// Client makes authenticated bridge calls. It is safe for concurrent use.
// Each call uses a separate Unix connection, avoiding shared-stream failure
// and head-of-line blocking between tool calls.
type Client struct {
	opts ClientOptions
	next atomic.Uint64
}

func NewClient(opts ClientOptions) (*Client, error) {
	if err := validateToken(opts.Token); err != nil {
		return nil, err
	}
	if opts.SocketPath == "" {
		return nil, errors.New("codex bridge: socket path is empty")
	}
	abs, err := filepath.Abs(opts.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("codex bridge: resolve socket path: %w", err)
	}
	opts.SocketPath = filepath.Clean(abs)
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	return &Client{opts: opts}, nil
}

func (c *Client) Ping(ctx context.Context) error {
	_, err := c.call(ctx, methodPing, json.RawMessage(`{}`), nil)
	return err
}

func (c *Client) SendMessage(ctx context.Context, metadata json.RawMessage, to, message string) (wire.SendAck, error) {
	params, err := json.Marshal(intercomtools.SendMessageArgs{To: to, Message: message})
	if err != nil {
		return wire.SendAck{}, fmt.Errorf("codex bridge: encode send_message: %w", err)
	}
	raw, err := c.call(ctx, methodSendMessage, params, metadata)
	if err != nil {
		return wire.SendAck{}, err
	}
	var ack wire.SendAck
	if err := json.Unmarshal(raw, &ack); err != nil {
		return wire.SendAck{}, fmt.Errorf("codex bridge: decode send_message result: %w", err)
	}
	return ack, nil
}

func (c *Client) ListPeers(ctx context.Context, metadata json.RawMessage) ([]string, error) {
	raw, err := c.call(ctx, methodListPeers, json.RawMessage(`{}`), metadata)
	if err != nil {
		return nil, err
	}
	var result struct {
		Peers []string `json:"peers"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("codex bridge: decode list_peers result: %w", err)
	}
	return result.Peers, nil
}

func (c *Client) call(parent context.Context, method string, params, metadata json.RawMessage) (json.RawMessage, error) {
	if parent == nil {
		return nil, errors.New("codex bridge: call context is nil")
	}
	ctx, cancel := withTimeout(parent, c.opts.Timeout)
	defer cancel()
	if err := validateSocket(c.opts.SocketPath); err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", c.opts.SocketPath)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("codex bridge: dial: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	id := c.next.Add(1)
	req := request{
		Version: protocolVersion,
		ID:      id,
		Token:   c.opts.Token,
		Method:  method,
		Params:  params,
		Meta:    cloneRaw(metadata),
	}
	if deadline, ok := ctx.Deadline(); ok {
		req.DeadlineUnix = deadline.UnixNano()
	}
	if err := writeFrame(conn, req); err != nil {
		if ctxErr := contextualError(ctx, err); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	raw, err := readFrame(conn)
	if err != nil {
		if ctxErr := contextualError(ctx, err); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("codex bridge: decode response: %w", err)
	}
	if resp.Version != protocolVersion {
		return nil, fmt.Errorf("codex bridge: response protocol version %d", resp.Version)
	}
	if resp.ID != id {
		return nil, fmt.Errorf("codex bridge: response id mismatch: got %d want %d", resp.ID, id)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	if len(resp.Result) == 0 {
		return nil, errors.New("codex bridge: response has neither result nor error")
	}
	return resp.Result, nil
}

func contextualError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return context.DeadlineExceeded
	}
	return nil
}

func withTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if deadline, ok := parent.Deadline(); ok && time.Until(deadline) <= timeout {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func validateToken(token string) error {
	if len(token) < minimumTokenBytes {
		return fmt.Errorf("codex bridge: token must contain at least %d bytes", minimumTokenBytes)
	}
	if len(token) > maximumTokenBytes {
		return fmt.Errorf("codex bridge: token exceeds %d-byte limit", maximumTokenBytes)
	}
	return nil
}

func tokensEqual(want, got string) bool {
	if len(want) != len(got) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

func validateNewSocketPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("codex bridge: socket path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("codex bridge: resolve socket path: %w", err)
	}
	abs = filepath.Clean(abs)
	if err := validatePrivateParent(filepath.Dir(abs)); err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", fmt.Errorf("codex bridge: socket path already exists: %s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("codex bridge: inspect socket path: %w", err)
	}
	return abs, nil
}

func validatePrivateParent(parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("codex bridge: inspect socket parent: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("codex bridge: socket parent %q is not a real directory", parent)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("codex bridge: socket parent %q has mode %04o, want 0700", parent, info.Mode().Perm())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("codex bridge: socket parent %q is not owned by the current user", parent)
	}
	return nil
}

func validateSocket(path string) error {
	if err := validatePrivateParent(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("codex bridge: inspect socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("codex bridge: endpoint %q has type/mode %v, want socket 0600", path, info.Mode())
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("codex bridge: socket %q is not owned by the current user", path)
	}
	return nil
}

func removeIfSame(path string, original os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("codex bridge: inspect socket during cleanup: %w", err)
	}
	if current.Mode()&os.ModeSocket == 0 || original.Mode()&os.ModeSocket == 0 || !os.SameFile(original, current) {
		return errors.New("codex bridge: socket path was replaced; refusing to remove replacement")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("codex bridge: remove socket: %w", err)
	}
	return nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

func readFrame(r io.Reader) ([]byte, error) {
	// The extra two bytes distinguish a maximum-size payload plus newline from
	// an oversize payload. LimitedReader keeps ReadBytes bounded even when a
	// peer never terminates its frame.
	limited := &io.LimitedReader{R: r, N: MaxFrameSize + 2}
	reader := bufio.NewReaderSize(limited, 64*1024)
	line, err := reader.ReadBytes('\n')
	if len(line) > MaxFrameSize+1 {
		return nil, ErrFrameTooLarge
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			if len(line) == 0 {
				return nil, io.EOF
			}
			return nil, errors.New("codex bridge: frame is not newline terminated")
		}
		return nil, fmt.Errorf("codex bridge: read frame: %w", err)
	}
	raw := append([]byte(nil), line[:len(line)-1]...)
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("codex bridge: empty frame")
	}
	return raw, nil
}

func writeFrame(w io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("codex bridge: encode frame: %w", err)
	}
	if len(raw) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	raw = append(raw, '\n')
	if _, err := io.Copy(w, bytes.NewReader(raw)); err != nil {
		return fmt.Errorf("codex bridge: write frame: %w", err)
	}
	return nil
}
