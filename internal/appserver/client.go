package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

const (
	DefaultMaxMessageSize        int64 = 128 << 20
	DefaultMaxConcurrentHandlers       = 64
)

var (
	ErrClosed              = errors.New("appserver: client closed")
	ErrMessageTooLarge     = errors.New("appserver: websocket message too large")
	ErrBinaryMessage       = errors.New("appserver: binary websocket message")
	ErrUnknownResponseID   = errors.New("appserver: unknown response id")
	ErrDuplicateResponseID = errors.New("appserver: duplicate response id")
	ErrAlreadyResponded    = errors.New("appserver: reverse request already answered")
	ErrHandlerLimit        = errors.New("appserver: concurrent reverse request limit exceeded")
)

// Options configures protocol dispatch. Notification callbacks execute on the
// ordered reader goroutine and therefore must return promptly. Reverse request
// callbacks execute on independent goroutines so an approval or tool handler
// can wait without blocking lifecycle messages or normal responses.
type Options struct {
	// MaxMessageSize bounds each inbound and outbound JSON message. Values at
	// or below zero use DefaultMaxMessageSize.
	MaxMessageSize int64
	// MaxConcurrentHandlers bounds independently running reverse-request
	// callbacks. Exceeding the limit is a fatal protocol error rather than
	// blocking the ordered reader. Values at or below zero use the default.
	MaxConcurrentHandlers int
	OnNotification        func(Notification)
	// OnReverseRequestReceived runs synchronously on the ordered reader before
	// the request handler is scheduled. It must return promptly. Lifecycle
	// owners can use it to close startup races without performing any I/O.
	OnReverseRequestReceived func(method string)
	// OnReverseRequest runs on an independent goroutine and may perform I/O.
	OnReverseRequest func(*ReverseRequest)
}

type pendingResponse struct {
	result json.RawMessage
	err    error
}

type pendingCall struct {
	response chan pendingResponse
}

// Client owns one app-server websocket connection and its request IDs.
type Client struct {
	conn *websocket.Conn
	opts Options

	ctx    context.Context
	cancel context.CancelFunc

	nextID atomic.Int64

	writeGate   chan struct{}
	mu          sync.Mutex
	pending     map[RequestID]*pendingCall
	seen        map[RequestID]struct{}
	seenFIFO    []RequestID
	expired     map[RequestID]struct{}
	expiredFIFO []RequestID

	handlerMu     sync.Mutex
	handlerCount  int
	handlerSealed bool
	handlerDone   chan struct{}

	done       chan struct{}
	finish     sync.Once
	terminal   error
	closed     bool
	localClose bool
}

const responseIDHistory = 1024

func newClient(conn *websocket.Conn, opts Options) *Client {
	if opts.MaxMessageSize <= 0 {
		opts.MaxMessageSize = DefaultMaxMessageSize
	}
	if opts.MaxConcurrentHandlers <= 0 {
		opts.MaxConcurrentHandlers = DefaultMaxConcurrentHandlers
	}
	ctx, cancel := context.WithCancel(context.Background())
	writeGate := make(chan struct{}, 1)
	writeGate <- struct{}{}
	c := &Client{
		conn:      conn,
		opts:      opts,
		ctx:       ctx,
		cancel:    cancel,
		writeGate: writeGate,
		pending:   make(map[RequestID]*pendingCall),
		seen:      make(map[RequestID]struct{}),
		expired:   make(map[RequestID]struct{}),
		done:      make(chan struct{}),
	}
	conn.SetReadLimit(opts.MaxMessageSize)
	go c.readLoop()
	return c
}

// Done closes when the reader exits or Close is called.
func (c *Client) Done() <-chan struct{} { return c.done }

// Wait waits for connection termination. It returns nil for a local Close,
// io.EOF for a peer websocket close, or the fatal protocol/transport error.
func (c *Client) Wait() error {
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminal
}

// Close closes only this websocket client. It does not stop app-server.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.localClose = true
	c.mu.Unlock()

	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.terminate(nil)
	return err
}

// Call sends a request and waits for its matching result or error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	pending, err := c.StartCall(ctx, method, params)
	if err != nil {
		return err
	}
	return pending.Await(ctx, result)
}

// PendingCall is a request that has been written and is awaiting a response.
// It exposes the request ID for state machines that correlate early events.
type PendingCall struct {
	ID     RequestID
	client *Client
	call   *pendingCall
	once   sync.Once
	result pendingResponse
}

// StartCall registers response correlation before writing the request, so a
// fast response cannot race past the pending map.
func (c *Client) StartCall(ctx context.Context, method string, params any) (*PendingCall, error) {
	if method == "" {
		return nil, errors.New("appserver: request method is empty")
	}
	id := NumberRequestID(c.nextID.Add(1) - 1)
	call := &pendingCall{response: make(chan pendingResponse, 1)}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = call
	c.mu.Unlock()

	message := outboundRequest{ID: id, Method: method, Params: params}
	if err := c.writeJSON(ctx, message); err != nil {
		c.removePending(id, call)
		if !errors.Is(err, ErrClosed) {
			c.terminate(err)
		}
		return nil, err
	}
	return &PendingCall{ID: id, client: c, call: call}, nil
}

// Await waits once for a PendingCall. A canceled deadline replaces live
// correlation with a bounded tombstone so one late response is ignored.
// Managed callers must still decide whether a mutating-request timeout makes
// their higher-level state ambiguous.
func (p *PendingCall) Await(ctx context.Context, result any) error {
	p.once.Do(func() {
		// Prefer an already-buffered response over a simultaneous connection
		// close. A server is allowed to write a response and immediately close.
		select {
		case p.result = <-p.call.response:
			return
		default:
		}
		select {
		case p.result = <-p.call.response:
		case <-ctx.Done():
			if p.client.expirePending(p.ID, p.call) {
				p.result.err = ctx.Err()
			} else {
				// The reader won the removal race and owns delivering the
				// response, even if the context expired at the same instant.
				p.result = <-p.call.response
			}
		case <-p.client.done:
			select {
			case p.result = <-p.call.response:
			default:
				p.result.err = p.client.Wait()
				if p.result.err == nil {
					p.result.err = ErrClosed
				}
			}
		}
	})
	if p.result.err != nil {
		return p.result.err
	}
	if result == nil || len(p.result.result) == 0 {
		return nil
	}
	if err := json.Unmarshal(p.result.result, result); err != nil {
		return fmt.Errorf("appserver: decode %s result: %w", p.ID, err)
	}
	return nil
}

// Notify sends a client notification such as initialized.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return errors.New("appserver: notification method is empty")
	}
	return c.writeJSON(ctx, outboundNotification{Method: method, Params: params})
}

func (c *Client) Initialize(ctx context.Context, params InitializeParams) (InitializeResponse, error) {
	var response InitializeResponse
	err := c.Call(ctx, MethodInitialize, params, &response)
	return response, err
}

func (c *Client) Initialized(ctx context.Context) error {
	return c.Notify(ctx, MethodInitialized, nil)
}

func (c *Client) ThreadStart(ctx context.Context, params ThreadStartParams) (ThreadStartResponse, error) {
	var response ThreadStartResponse
	err := c.Call(ctx, MethodThreadStart, params, &response)
	return response, err
}

func (c *Client) ThreadResume(ctx context.Context, params ThreadResumeParams) (ThreadResumeResponse, error) {
	var response ThreadResumeResponse
	err := c.Call(ctx, MethodThreadResume, params, &response)
	return response, err
}

func (c *Client) ThreadRead(ctx context.Context, params ThreadReadParams) (ThreadReadResponse, error) {
	var response ThreadReadResponse
	err := c.Call(ctx, MethodThreadRead, params, &response)
	return response, err
}

func (c *Client) TurnStart(ctx context.Context, params TurnStartParams) (TurnStartResponse, error) {
	await, err := c.StartTurn(ctx, params)
	if err != nil {
		return TurnStartResponse{}, err
	}
	return await(ctx)
}

// TurnStartAwait waits for a turn/start response whose request has already
// been written. Keeping correlation alive independently of the write context
// lets lifecycle owners drain an ambiguous start during graceful shutdown.
type TurnStartAwait func(context.Context) (TurnStartResponse, error)

func (c *Client) StartTurn(ctx context.Context, params TurnStartParams) (TurnStartAwait, error) {
	pending, err := c.StartCall(ctx, MethodTurnStart, params)
	if err != nil {
		return nil, err
	}
	return func(awaitCtx context.Context) (TurnStartResponse, error) {
		var response TurnStartResponse
		err := pending.Await(awaitCtx, &response)
		return response, err
	}, nil
}

func (c *Client) TurnInterrupt(ctx context.Context, params TurnInterruptParams) error {
	var response TurnInterruptResponse
	return c.Call(ctx, MethodTurnInterrupt, params, &response)
}

// WaitHandlers waits for reverse-request handlers that were dispatched by
// the reader. Lifecycle callers invoke this after observing a terminal turn
// event, which establishes that no later reverse requests belong to the turn.
func (c *Client) WaitHandlers(ctx context.Context) error {
	c.handlerMu.Lock()
	c.handlerSealed = true
	if c.handlerCount == 0 {
		c.handlerMu.Unlock()
		return nil
	}
	if c.handlerDone == nil {
		c.handlerDone = make(chan struct{})
	}
	done := c.handlerDone
	c.handlerMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) beginHandler() error {
	c.handlerMu.Lock()
	defer c.handlerMu.Unlock()
	if c.handlerSealed {
		return errors.New("appserver: reverse request received after handler drain began")
	}
	if c.handlerCount >= c.opts.MaxConcurrentHandlers {
		return ErrHandlerLimit
	}
	c.handlerCount++
	return nil
}

func (c *Client) finishHandler() {
	c.handlerMu.Lock()
	c.handlerCount--
	if c.handlerSealed && c.handlerCount == 0 && c.handlerDone != nil {
		close(c.handlerDone)
		c.handlerDone = nil
	}
	c.handlerMu.Unlock()
}

func (c *Client) writeJSON(ctx context.Context, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("appserver: encode message: %w", err)
	}
	if int64(len(data)) > c.opts.MaxMessageSize {
		return fmt.Errorf("%w: %d > %d bytes", ErrMessageTooLarge, len(data), c.opts.MaxMessageSize)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return ErrClosed
	case <-c.writeGate:
	}
	defer func() { c.writeGate <- struct{}{} }()
	// The slot and cancellation may become ready together. Recheck before any
	// bytes are written so an expired caller never spends more than its one
	// total queue-plus-write budget.
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return normalizeWebsocketError(err)
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			if c.isLocallyClosed() {
				c.terminate(nil)
			} else {
				c.terminate(normalizeWebsocketError(err))
			}
			return
		}
		if typ != websocket.MessageText {
			c.terminate(ErrBinaryMessage)
			_ = c.conn.Close(websocket.StatusUnsupportedData, "JSON text frames required")
			return
		}
		if int64(len(data)) > c.opts.MaxMessageSize {
			c.terminate(ErrMessageTooLarge)
			_ = c.conn.Close(websocket.StatusMessageTooBig, "message exceeds limit")
			return
		}
		if err := c.dispatch(data); err != nil {
			c.terminate(err)
			_ = c.conn.Close(websocket.StatusPolicyViolation, "invalid app-server message")
			return
		}
	}
}

func (c *Client) dispatch(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("appserver: decode message: %w", err)
	}
	method, err := decodeOptionalString(fields["method"])
	if err != nil {
		return err
	}
	idRaw, hasID := fields["id"]
	_, hasResult := fields["result"]
	_, hasError := fields["error"]

	if method != "" {
		params := cloneRaw(fields["params"])
		if !hasID {
			if c.opts.OnNotification != nil {
				c.opts.OnNotification(Notification{Method: method, Params: params})
			}
			return nil
		}
		var id RequestID
		if err := json.Unmarshal(idRaw, &id); err != nil {
			return err
		}
		request := &ReverseRequest{ID: id, Method: method, Params: params, client: c}
		if c.opts.OnReverseRequestReceived != nil {
			c.opts.OnReverseRequestReceived(method)
		}
		if err := c.beginHandler(); err != nil {
			return err
		}
		handler := c.opts.OnReverseRequest
		if handler == nil {
			go func() {
				defer c.finishHandler()
				_ = request.RespondError(c.ctx, &RPCError{
					Code: ErrorCodeMethodNotFound, Message: "unsupported server request: " + method,
				})
			}()
		} else {
			go func() {
				defer c.finishHandler()
				handler(request)
			}()
		}
		return nil
	}

	if !hasID || hasResult == hasError {
		return errors.New("appserver: malformed response envelope")
	}
	var id RequestID
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return err
	}
	if hasError {
		var rpcErr RPCError
		if err := json.Unmarshal(fields["error"], &rpcErr); err != nil {
			return fmt.Errorf("appserver: decode error response: %w", err)
		}
		return c.resolveResponse(id, pendingResponse{err: &rpcErr})
	}
	return c.resolveResponse(id, pendingResponse{result: cloneRaw(fields["result"])})
}

func (c *Client) resolveResponse(id RequestID, response pendingResponse) error {
	c.mu.Lock()
	call := c.pending[id]
	if call != nil {
		delete(c.pending, id)
		c.rememberResponseLocked(id)
		c.mu.Unlock()
		call.response <- response
		return nil
	}
	if _, expired := c.expired[id]; expired {
		delete(c.expired, id)
		c.rememberResponseLocked(id)
		c.mu.Unlock()
		return nil
	}
	_, duplicate := c.seen[id]
	c.mu.Unlock()
	if duplicate {
		return fmt.Errorf("%w: %s", ErrDuplicateResponseID, id)
	}
	return fmt.Errorf("%w: %s", ErrUnknownResponseID, id)
}

func (c *Client) rememberResponseLocked(id RequestID) {
	c.seen[id] = struct{}{}
	c.seenFIFO = append(c.seenFIFO, id)
	if len(c.seenFIFO) > responseIDHistory {
		old := c.seenFIFO[0]
		c.seenFIFO = c.seenFIFO[1:]
		delete(c.seen, old)
	}
}

func (c *Client) removePending(id RequestID, call *pendingCall) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[id] == call {
		delete(c.pending, id)
		return true
	}
	return false
}

func (c *Client) expirePending(id RequestID, call *pendingCall) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pending[id] != call {
		return false
	}
	delete(c.pending, id)
	c.expired[id] = struct{}{}
	c.expiredFIFO = append(c.expiredFIFO, id)
	if len(c.expiredFIFO) > responseIDHistory {
		oldest := c.expiredFIFO[0]
		c.expiredFIFO = c.expiredFIFO[1:]
		delete(c.expired, oldest)
	}
	return true
}

func (c *Client) terminate(err error) {
	c.finish.Do(func() {
		c.cancel()
		c.mu.Lock()
		if c.localClose {
			err = nil
		}
		c.terminal = err
		c.closed = true
		pending := c.pending
		c.pending = make(map[RequestID]*pendingCall)
		c.mu.Unlock()
		for _, call := range pending {
			call.response <- pendingResponse{err: terminalCallError(err)}
		}
		close(c.done)
		_ = c.conn.CloseNow()
	})
}

func terminalCallError(err error) error {
	if err == nil {
		return ErrClosed
	}
	return err
}

func (c *Client) isLocallyClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.localClose
}

func normalizeWebsocketError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, websocket.ErrMessageTooBig) || websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
		return fmt.Errorf("%w: %v", ErrMessageTooLarge, err)
	}
	if status := websocket.CloseStatus(err); status != -1 {
		var closeErr websocket.CloseError
		if errors.As(err, &closeErr) {
			return fmt.Errorf("appserver: websocket closed (status %d, reason %q): %w", status, closeErr.Reason, io.EOF)
		}
		return fmt.Errorf("appserver: websocket closed (status %d): %w", status, io.EOF)
	}
	if errors.Is(err, context.Canceled) {
		return ErrClosed
	}
	return fmt.Errorf("appserver: websocket: %w", err)
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func decodeOptionalString(value json.RawMessage) (string, error) {
	if len(value) == 0 {
		return "", nil
	}
	var result string
	if err := json.Unmarshal(value, &result); err != nil {
		return "", errors.New("appserver: message method must be a string")
	}
	return result, nil
}

type outboundRequest struct {
	ID     RequestID `json:"id"`
	Method string    `json:"method"`
	Params any       `json:"params,omitempty"`
}

type outboundNotification struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

type outboundResponse struct {
	ID     RequestID `json:"id"`
	Result any       `json:"result"`
}

type outboundError struct {
	ID    RequestID `json:"id"`
	Error *RPCError `json:"error"`
}

// ReverseRequest is an app-server-initiated request. A handler must answer it
// exactly once with Respond or RespondError.
type ReverseRequest struct {
	ID     RequestID
	Method string
	Params json.RawMessage

	client   *Client
	mu       sync.Mutex
	answered bool
}

func (r *ReverseRequest) DecodeParams(dst any) error {
	if len(r.Params) == 0 {
		return errors.New("appserver: reverse request has no params")
	}
	if err := json.Unmarshal(r.Params, dst); err != nil {
		return fmt.Errorf("appserver: decode %s request: %w", r.Method, err)
	}
	return nil
}

func (r *ReverseRequest) Respond(ctx context.Context, result any) error {
	if !r.claimResponse() {
		return ErrAlreadyResponded
	}
	return r.client.writeJSON(ctx, outboundResponse{ID: r.ID, Result: result})
}

func (r *ReverseRequest) RespondError(ctx context.Context, rpcErr *RPCError) error {
	if rpcErr == nil {
		return errors.New("appserver: nil reverse-request error")
	}
	if !r.claimResponse() {
		return ErrAlreadyResponded
	}
	return r.client.writeJSON(ctx, outboundError{ID: r.ID, Error: rpcErr})
}

func (r *ReverseRequest) claimResponse() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.answered {
		return false
	}
	r.answered = true
	return true
}
