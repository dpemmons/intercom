// Package appserverproxy exposes one initialized Codex app-server connection
// to a stock Codex TUI without making the TUI a second upstream subscriber.
package appserverproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/dpemmons/intercom/internal/appserver"
)

const (
	// Stock remote Codex clients accept WebSocket messages up to 128 MiB.
	defaultMaxMessageSize         int64 = 128 << 20
	defaultMaxConcurrentRequests        = 64
	defaultWriteQueueSize               = 256
	defaultHandshakeTimeout             = 30 * time.Second
	defaultWriteTimeout                 = 30 * time.Second
	defaultTurnStartTimeout             = 30 * time.Second
	defaultReverseResponseTimeout       = 30 * time.Second
	closeHandshakeGrace                 = 100 * time.Millisecond
	reverseResponseIDHistory            = 1024
	serverOverloadedCode          int64 = -32001
)

var (
	ErrClosed          = errors.New("appserver proxy: closed")
	ErrNoAttachedTUI   = errors.New("appserver proxy: no attached TUI")
	ErrTUIBusy         = errors.New("appserver proxy: a TUI is already connected")
	ErrUnknownResponse = errors.New("appserver proxy: unknown TUI response id")
)

// Upstream is the raw request surface needed to multiplex a downstream TUI
// over Intercom's one initialized app-server connection.
type Upstream interface {
	StartCall(context.Context, string, any) (*appserver.PendingCall, error)
	Notify(context.Context, string, any) error
}

// BeforeRequest validates or rewrites a TUI request before it is sent to the
// upstream app-server. State is passed to AfterRequest even when the upstream
// request fails.
type BeforeRequest func(method string, params json.RawMessage) (rewritten json.RawMessage, state any, rpcErr *appserver.RPCError)

// AfterRequest observes the terminal result of a forwarded TUI request.
// Implementations must return promptly.
type AfterRequest func(method string, state any, result json.RawMessage, err error)

// LocalRequest may terminate a rewritten TUI request without sending it to the
// upstream app-server. An error is meaningful only when handled is true.
// Implementations must return promptly.
type LocalRequest func(method string, params json.RawMessage, state any) (result json.RawMessage, handled bool, err error)

// Options configures one proxy listener.
type Options struct {
	Endpoint               string
	Upstream               Upstream
	InitializeResponse     appserver.InitializeResponse
	ExpectedClientVersion  string
	MaxMessageSize         int64
	MaxConcurrentCalls     int
	WriteQueueSize         int
	HandshakeTimeout       time.Duration
	WriteTimeout           time.Duration
	RequestTimeout         time.Duration
	TurnStartTimeout       time.Duration
	ReverseResponseTimeout time.Duration
	BeforeRequest          BeforeRequest
	LocalRequest           LocalRequest
	AfterRequest           AfterRequest
	OnAttach               func()
	OnDetach               func()
	Logger                 *slog.Logger
}

// Proxy owns a Unix HTTP/WebSocket listener and at most one downstream TUI.
type Proxy struct {
	opts Options
	path string

	ctx    context.Context
	cancel context.CancelFunc

	listener net.Listener
	server   *http.Server

	mu        sync.Mutex
	session   *session
	accepting bool
	closed    bool

	reverseID atomic.Uint64
	handlers  chan struct{}
	handlerWG sync.WaitGroup
	closeOnce sync.Once
	done      chan struct{}
}

// Listen creates the Unix socket and starts accepting stock Codex TUI
// WebSocket connections. The socket is created with mode 0600.
func Listen(parent context.Context, opts Options) (*Proxy, error) {
	if opts.Upstream == nil {
		return nil, errors.New("appserver proxy: upstream is required")
	}
	path, err := appserver.ParseUnixEndpoint(opts.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("appserver proxy: endpoint: %w", err)
	}
	if opts.MaxMessageSize <= 0 {
		opts.MaxMessageSize = defaultMaxMessageSize
	}
	if opts.MaxConcurrentCalls <= 0 {
		opts.MaxConcurrentCalls = defaultMaxConcurrentRequests
	}
	if opts.WriteQueueSize <= 0 {
		opts.WriteQueueSize = defaultWriteQueueSize
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = defaultHandshakeTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = defaultWriteTimeout
	}
	if opts.TurnStartTimeout <= 0 {
		opts.TurnStartTimeout = defaultTurnStartTimeout
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultTurnStartTimeout
	}
	if opts.ReverseResponseTimeout <= 0 {
		opts.ReverseResponseTimeout = defaultReverseResponseTimeout
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("appserver proxy: socket path already exists: %s", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("appserver proxy: inspect socket path: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("appserver proxy: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("appserver proxy: chmod socket: %w", err)
	}

	ctx, cancel := context.WithCancel(parent)
	p := &Proxy{
		opts:     opts,
		path:     path,
		ctx:      ctx,
		cancel:   cancel,
		listener: listener,
		handlers: make(chan struct{}, opts.MaxConcurrentCalls),
		done:     make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.serveWebSocket)
	p.server = &http.Server{Handler: mux}
	go p.serve()
	go func() {
		<-ctx.Done()
		_ = p.Close()
	}()
	return p, nil
}

// Done closes after the listener and any attached TUI have stopped.
func (p *Proxy) Done() <-chan struct{} { return p.done }

// Endpoint returns the configured downstream Unix endpoint.
func (p *Proxy) Endpoint() string { return p.opts.Endpoint }

// Attached reports whether a TUI completed initialize and thread/resume.
func (p *Proxy) Attached() bool {
	p.mu.Lock()
	s := p.session
	p.mu.Unlock()
	return s != nil && s.isReady()
}

// Close stops the listener, disconnects the TUI, and removes the socket.
func (p *Proxy) Close() error {
	var result error
	p.closeOnce.Do(func() {
		p.cancel()
		p.mu.Lock()
		p.closed = true
		s := p.session
		p.mu.Unlock()
		if s != nil {
			s.abort()
		}
		if p.server != nil {
			if err := p.server.Close(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				result = err
			}
		}
		if err := p.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) && result == nil {
			result = err
		}
		if err := os.Remove(p.path); err != nil && !errors.Is(err, os.ErrNotExist) && result == nil {
			result = err
		}
	})
	<-p.done
	return result
}

// Notify forwards an upstream app-server notification without blocking the
// upstream ordered reader. A slow TUI is disconnected when its bounded write
// queue fills; the Intercom service remains alive.
func (p *Proxy) Notify(notification appserver.Notification) {
	p.mu.Lock()
	s := p.session
	p.mu.Unlock()
	if s == nil {
		return
	}
	data, err := json.Marshal(outboundNotification{Method: notification.Method, Params: notification.Params})
	if err != nil {
		p.opts.Logger.Warn("encode proxy notification", "method", notification.Method, "err", err)
		return
	}
	if s.offerNotification(notification.Method, data) == notificationOverflow {
		p.opts.Logger.Warn("disconnect slow Codex TUI", "method", notification.Method)
		s.abort()
	}
}

// ForwardReverse sends one app-server reverse request to the attached TUI and
// relays its result to upstream. It returns false without answering request
// when no ready TUI exists or the TUI disconnects before answering, allowing
// the caller to apply its headless fallback policy.
func (p *Proxy) ForwardReverse(ctx context.Context, request *appserver.ReverseRequest) (bool, error) {
	p.mu.Lock()
	s := p.session
	p.mu.Unlock()
	if s == nil || !s.isReady() {
		return false, nil
	}
	id := appserver.StringRequestID(fmt.Sprintf("intercom-%d", p.reverseID.Add(1)))
	response, err := s.reverse(ctx, id, request.Method, request.Params)
	if err != nil {
		return false, err
	}
	if response.rpcErr != nil {
		responseCtx, cancel := context.WithTimeout(p.ctx, p.opts.ReverseResponseTimeout)
		defer cancel()
		return true, request.RespondError(responseCtx, response.rpcErr)
	}
	responseCtx, cancel := context.WithTimeout(p.ctx, p.opts.ReverseResponseTimeout)
	defer cancel()
	return true, request.Respond(responseCtx, response.result)
}

func (p *Proxy) serve() {
	err := p.server.Serve(p.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
		p.opts.Logger.Error("Codex TUI proxy listener failed", "err", err)
		p.cancel()
	}
	p.mu.Lock()
	s := p.session
	p.closed = true
	p.mu.Unlock()
	if s != nil {
		s.abort()
		<-s.finished
	}
	p.handlerWG.Wait()
	_ = os.Remove(p.path)
	close(p.done)
}

func (p *Proxy) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/rpc" {
		http.NotFound(w, r)
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		http.Error(w, "Intercom Codex service is stopping", http.StatusServiceUnavailable)
		return
	}
	if p.accepting || p.session != nil {
		p.mu.Unlock()
		http.Error(w, ErrTUIBusy.Error(), http.StatusConflict)
		return
	}
	p.accepting = true
	p.mu.Unlock()

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		p.mu.Lock()
		p.accepting = false
		p.mu.Unlock()
		return
	}
	conn.SetReadLimit(p.opts.MaxMessageSize)
	s := newSession(p, conn)
	p.mu.Lock()
	p.accepting = false
	if p.closed || p.session != nil {
		p.mu.Unlock()
		s.abort()
		return
	}
	p.session = s
	p.mu.Unlock()

	s.run()
	p.mu.Lock()
	if p.session == s {
		p.session = nil
	}
	p.mu.Unlock()
	if s.wasReady() && p.opts.OnDetach != nil {
		p.opts.OnDetach()
	}
	close(s.finished)
}

type session struct {
	proxy *Proxy
	conn  *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	writes   chan queuedWrite
	done     chan struct{}
	finished chan struct{}

	mu           sync.Mutex
	initialized  bool
	initializing bool
	barrier      bool
	barrierReady bool
	barrierDone  chan struct{}
	ready        bool
	everReady    bool
	optOut       map[string]struct{}
	barrierQueue [][]byte
	pending      map[appserver.RequestID]chan reverseResponse
	expired      map[appserver.RequestID]struct{}
	expiredFIFO  []appserver.RequestID
	requests     map[appserver.RequestID]struct{}
	turnTail     chan struct{}

	closeOne sync.Once
}

type reverseResponse struct {
	result json.RawMessage
	rpcErr *appserver.RPCError
	err    error
}

type queuedWrite struct {
	data            []byte
	sent            chan error
	requestID       appserver.RequestID
	releasesRequest bool
}

type notificationDisposition uint8

const (
	notificationDropped notificationDisposition = iota
	notificationAccepted
	notificationOverflow
)

func newSession(proxy *Proxy, conn *websocket.Conn) *session {
	ctx, cancel := context.WithCancel(proxy.ctx)
	turnTail := make(chan struct{})
	close(turnTail)
	return &session{
		proxy:    proxy,
		conn:     conn,
		ctx:      ctx,
		cancel:   cancel,
		writes:   make(chan queuedWrite, proxy.opts.WriteQueueSize),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
		optOut:   make(map[string]struct{}),
		pending:  make(map[appserver.RequestID]chan reverseResponse),
		expired:  make(map[appserver.RequestID]struct{}),
		requests: make(map[appserver.RequestID]struct{}),
		turnTail: turnTail,
	}
}

func (s *session) run() {
	handshakeTimer := time.AfterFunc(s.proxy.opts.HandshakeTimeout, func() {
		if !s.isReady() {
			s.close(websocket.StatusPolicyViolation, "initialize and thread/resume handshake timed out")
		}
	})
	defer handshakeTimer.Stop()
	go s.writeLoop()
	for {
		typ, data, err := s.conn.Read(s.ctx)
		if err != nil {
			s.close(websocket.StatusNormalClosure, "")
			return
		}
		if typ != websocket.MessageText {
			s.close(websocket.StatusUnsupportedData, "JSON text frames required")
			return
		}
		if int64(len(data)) > s.proxy.opts.MaxMessageSize {
			s.close(websocket.StatusMessageTooBig, "message exceeds limit")
			return
		}
		if err := s.dispatch(data); err != nil {
			s.proxy.opts.Logger.Warn("invalid Codex TUI message", "err", err)
			s.close(websocket.StatusPolicyViolation, "invalid app-server message")
			return
		}
	}
}

func (s *session) dispatch(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	method, err := optionalString(fields["method"])
	if err != nil {
		return err
	}
	idRaw, hasID := fields["id"]
	_, hasResult := fields["result"]
	_, hasError := fields["error"]
	if method != "" {
		params := cloneRaw(fields["params"])
		if !hasID {
			return s.dispatchNotification(method, params)
		}
		var id appserver.RequestID
		if err := json.Unmarshal(idRaw, &id); err != nil {
			return err
		}
		if !s.claimRequest(id) {
			// A duplicate ID makes it impossible to correlate a terminal
			// response with exactly one request. Close the session without
			// writing an error under the duplicated ID: doing so would itself
			// be a second terminal response for that ID.
			return fmt.Errorf("duplicate in-flight request id: %s", id)
		}
		// Initialize is terminated locally and cannot block on upstream I/O.
		// Handle it on the ordered reader so the first initialize frame is
		// deterministically the one that establishes the session.
		if method == appserver.MethodInitialize {
			s.handleInitialize(id, params)
			return nil
		}
		// Unsubscribe writes its terminal response and closes the downstream
		// lifecycle. Keep it on the ordered reader so no later TUI frame can
		// cross that boundary while the close handshake begins.
		if method == appserver.MethodThreadUnsubscribe {
			s.handleRequest(id, method, params)
			return nil
		}
		// Admit the initial resume on the ordered reader. A following pipelined
		// turn/start must not race a not-yet-scheduled resume handler and be
		// rejected even though resume appeared first on the wire.
		if method == appserver.MethodThreadResume && !s.isReady() {
			if !s.beginHandler() {
				return s.writeError(context.Background(), id, &appserver.RPCError{
					Code: serverOverloadedCode, Message: "Server overloaded; retry later.",
				})
			}
			s.handleRequest(id, method, params)
			s.endHandler()
			return nil
		}
		if method == appserver.MethodTurnStart {
			if !s.beginHandler() {
				return s.writeError(context.Background(), id, &appserver.RPCError{
					Code: serverOverloadedCode, Message: "Server overloaded; retry later.",
				})
			}
			s.queueTurnStart(id, params)
			return nil
		}
		if !s.beginHandler() {
			return s.writeError(context.Background(), id, &appserver.RPCError{
				Code: serverOverloadedCode, Message: "Server overloaded; retry later.",
			})
		}
		go func() {
			defer s.endHandler()
			s.handleRequest(id, method, params)
		}()
		return nil
	}
	if !hasID || hasResult == hasError {
		return errors.New("malformed response envelope")
	}
	var id appserver.RequestID
	if err := json.Unmarshal(idRaw, &id); err != nil {
		return err
	}
	response := reverseResponse{result: cloneRaw(fields["result"])}
	if hasError {
		var rpcErr appserver.RPCError
		if err := json.Unmarshal(fields["error"], &rpcErr); err != nil {
			return fmt.Errorf("decode error response: %w", err)
		}
		response.result = nil
		response.rpcErr = &rpcErr
	}
	s.mu.Lock()
	waiter := s.pending[id]
	if waiter != nil {
		delete(s.pending, id)
	} else if _, ok := s.expired[id]; ok {
		delete(s.expired, id)
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	if waiter == nil {
		return fmt.Errorf("%w: %s", ErrUnknownResponse, id)
	}
	waiter <- response
	return nil
}

func (s *session) queueTurnStart(id appserver.RequestID, params json.RawMessage) {
	s.mu.Lock()
	predecessor := s.turnTail
	completed := make(chan struct{})
	s.turnTail = completed
	s.mu.Unlock()

	go func() {
		defer s.endHandler()
		defer close(completed)
		select {
		case <-predecessor:
		case <-s.done:
			return
		}
		select {
		case <-s.done:
			return
		default:
		}
		s.handleRequest(id, appserver.MethodTurnStart, params)
	}()
}

func (s *session) handleRequest(id appserver.RequestID, method string, params json.RawMessage) {
	if !s.isInitialized() {
		_ = s.writeError(s.ctx, id, &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "Not initialized"})
		return
	}
	if method == appserver.MethodTurnStart && !s.waitForResumeBarrier() {
		_ = s.writeError(s.ctx, id, &appserver.RPCError{
			Code:    appserver.ErrorCodeInvalidRequest,
			Message: "thread/resume must complete before turn/start",
		})
		return
	}
	notificationBarrier := false
	barrierEstablishesReady := false
	barrierCompleted := false
	if method == appserver.MethodThreadResume || method == appserver.MethodTurnStart {
		var rpcErr *appserver.RPCError
		barrierEstablishesReady, rpcErr = s.beginNotificationBarrier(method)
		if rpcErr != nil {
			_ = s.writeError(s.ctx, id, rpcErr)
			return
		}
		notificationBarrier = true
	}
	defer func() {
		if notificationBarrier && !barrierCompleted {
			s.cancelNotificationBarrier()
		}
	}()
	rewritten := params
	var state any
	if s.proxy.opts.BeforeRequest != nil {
		var rpcErr *appserver.RPCError
		rewritten, state, rpcErr = s.proxy.opts.BeforeRequest(method, params)
		if rpcErr != nil {
			if failureErr := s.finishFailedRequest(id, rpcErr, notificationBarrier, !barrierEstablishesReady); failureErr != nil {
				s.close(websocket.StatusGoingAway, "TUI error response write failed")
				return
			}
			barrierCompleted = notificationBarrier && !barrierEstablishesReady
			return
		}
	}
	if s.proxy.opts.LocalRequest != nil {
		result, handled, err := s.proxy.opts.LocalRequest(method, rewritten, state)
		if handled {
			s.afterRequest(method, state, result, err)
			if err != nil {
				if failureErr := s.finishFailedRequest(id, err, notificationBarrier, !barrierEstablishesReady); failureErr != nil {
					s.close(websocket.StatusGoingAway, "TUI error response write failed")
					return
				}
				barrierCompleted = notificationBarrier && !barrierEstablishesReady
				return
			}
			if err := s.finishSuccessfulRequest(id, result, notificationBarrier); err != nil {
				s.close(websocket.StatusGoingAway, "TUI response write failed")
				return
			}
			barrierCompleted = true
			return
		}
	}
	if method == appserver.MethodThreadUnsubscribe {
		result := json.RawMessage(`{"status":"unsubscribed"}`)
		s.afterRequest(method, state, result, nil)
		if err := s.writeResult(s.ctx, id, result); err != nil {
			s.close(websocket.StatusGoingAway, "TUI response write failed")
			return
		}
		// thread/unsubscribe is a downstream lifecycle boundary. The proxy
		// remains the sole upstream subscriber, but this TUI session is no
		// longer entitled to receive notifications or reverse requests.
		// Closing it also settles any reverse requests that were already in
		// flight and permits a fresh TUI to reconnect cleanly.
		s.close(websocket.StatusNormalClosure, "thread unsubscribed")
		return
	}
	requestTimeout := s.proxy.opts.RequestTimeout
	if method == appserver.MethodTurnStart {
		requestTimeout = s.proxy.opts.TurnStartTimeout
	}
	requestCtx, cancel := context.WithTimeout(s.proxy.ctx, requestTimeout)
	defer cancel()
	pending, err := s.proxy.opts.Upstream.StartCall(requestCtx, method, rewritten)
	if err != nil {
		s.afterRequest(method, state, nil, err)
		if failureErr := s.finishFailedRequest(id, err, notificationBarrier, !barrierEstablishesReady); failureErr != nil {
			s.close(websocket.StatusGoingAway, "TUI error response write failed")
			return
		}
		barrierCompleted = notificationBarrier && !barrierEstablishesReady
		return
	}
	var result json.RawMessage
	err = pending.Await(requestCtx, &result)
	s.afterRequest(method, state, result, err)
	if err != nil {
		if failureErr := s.finishFailedRequest(id, err, notificationBarrier, !barrierEstablishesReady); failureErr != nil {
			s.close(websocket.StatusGoingAway, "TUI error response write failed")
			return
		}
		barrierCompleted = notificationBarrier && !barrierEstablishesReady
		return
	}
	if err := s.finishSuccessfulRequest(id, result, notificationBarrier); err != nil {
		s.close(websocket.StatusGoingAway, "TUI response write failed")
		return
	}
	barrierCompleted = true
}

func (s *session) finishFailedRequest(id appserver.RequestID, requestErr error, notificationBarrier, preserveNotifications bool) error {
	if err := s.writeRPCFailure(id, requestErr); err != nil {
		return err
	}
	if !notificationBarrier || !preserveNotifications {
		return nil
	}
	_, err := s.completeNotificationBarrier()
	return err
}

func (s *session) finishSuccessfulRequest(id appserver.RequestID, result any, notificationBarrier bool) error {
	if err := s.writeResult(s.ctx, id, result); err != nil {
		return err
	}
	if !notificationBarrier {
		return nil
	}
	first, err := s.completeNotificationBarrier()
	if err != nil {
		return err
	}
	if first && s.proxy.opts.OnAttach != nil {
		s.proxy.opts.OnAttach()
	}
	return nil
}

func (s *session) handleInitialize(id appserver.RequestID, params json.RawMessage) {
	s.mu.Lock()
	already := s.initialized || s.initializing
	if !already {
		s.initializing = true
	}
	s.mu.Unlock()
	if already {
		_ = s.writeError(s.ctx, id, &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "Already initialized"})
		return
	}
	var init appserver.InitializeParams
	if err := json.Unmarshal(params, &init); err != nil || init.ClientInfo.Name == "" ||
		init.Capabilities == nil || !init.Capabilities.ExperimentalAPI {
		s.mu.Lock()
		s.initializing = false
		s.mu.Unlock()
		_ = s.writeError(s.ctx, id, &appserver.RPCError{Code: appserver.ErrorCodeInvalidParams, Message: "initialize requires client identity and experimentalApi capability"})
		return
	}
	if expected := s.proxy.opts.ExpectedClientVersion; expected != "" && init.ClientInfo.Version != expected {
		s.mu.Lock()
		s.initializing = false
		s.mu.Unlock()
		_ = s.writeError(s.ctx, id, &appserver.RPCError{
			Code:    appserver.ErrorCodeInvalidRequest,
			Message: fmt.Sprintf("Codex client version %q is incompatible; this service requires %s", init.ClientInfo.Version, expected),
		})
		return
	}
	if init.Capabilities.RequestAttestation || init.Capabilities.MCPServerOpenAIFormElicitation {
		s.mu.Lock()
		s.initializing = false
		s.mu.Unlock()
		_ = s.writeError(s.ctx, id, &appserver.RPCError{
			Code:    appserver.ErrorCodeInvalidRequest,
			Message: "requestAttestation and mcpServerOpenaiFormElicitation are unavailable through the Intercom proxy",
		})
		return
	}
	s.mu.Lock()
	s.initializing = false
	s.initialized = true
	if init.Capabilities != nil {
		for _, method := range init.Capabilities.OptOutNotificationMethods {
			s.optOut[method] = struct{}{}
		}
	}
	s.mu.Unlock()
	if err := s.writeResult(s.ctx, id, s.proxy.opts.InitializeResponse); err != nil {
		s.close(websocket.StatusGoingAway, "initialize response write failed")
	}
}

func (s *session) dispatchNotification(method string, params json.RawMessage) error {
	if method == appserver.MethodInitialized {
		if !s.isInitialized() {
			return errors.New("initialized received before initialize")
		}
		return nil
	}
	if !s.isInitialized() {
		return errors.New("notification received before initialize")
	}
	return fmt.Errorf("unsupported Codex TUI notification method %q", method)
}

func (s *session) reverse(ctx context.Context, id appserver.RequestID, method string, params json.RawMessage) (reverseResponse, error) {
	waiter := make(chan reverseResponse, 1)
	s.mu.Lock()
	if !s.ready {
		s.mu.Unlock()
		return reverseResponse{}, ErrNoAttachedTUI
	}
	s.pending[id] = waiter
	s.mu.Unlock()
	data, err := json.Marshal(outboundRequest{ID: id, Method: method, Params: params})
	if err != nil {
		s.removePending(id, waiter)
		return reverseResponse{}, err
	}
	if err := s.write(ctx, data); err != nil {
		s.removePending(id, waiter)
		return reverseResponse{}, err
	}
	select {
	case response := <-waiter:
		return response, response.err
	case <-ctx.Done():
		if s.expirePending(id, waiter) {
			return reverseResponse{}, ctx.Err()
		}
		// The reader removed this waiter before the deadline won the select.
		// It owns delivery now, so consume that terminal result instead of
		// discarding a response that was already accepted from the TUI.
		response := <-waiter
		return response, response.err
	case <-s.done:
		if s.removePending(id, waiter) {
			return reverseResponse{}, ErrNoAttachedTUI
		}
		response := <-waiter
		return response, response.err
	}
}

func (s *session) removePending(id appserver.RequestID, waiter chan reverseResponse) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != waiter {
		return false
	}
	delete(s.pending, id)
	return true
}

func (s *session) expirePending(id appserver.RequestID, waiter chan reverseResponse) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[id] != waiter {
		return false
	}
	delete(s.pending, id)
	s.expired[id] = struct{}{}
	s.expiredFIFO = append(s.expiredFIFO, id)
	if len(s.expiredFIFO) > reverseResponseIDHistory {
		oldest := s.expiredFIFO[0]
		s.expiredFIFO = s.expiredFIFO[1:]
		delete(s.expired, oldest)
	}
	return true
}

func (s *session) afterRequest(method string, state any, result json.RawMessage, err error) {
	if s.proxy.opts.AfterRequest != nil {
		s.proxy.opts.AfterRequest(method, state, result, err)
	}
}

func (s *session) writeRPCFailure(id appserver.RequestID, err error) error {
	var rpcErr *appserver.RPCError
	if errors.As(err, &rpcErr) {
		return s.writeError(s.ctx, id, rpcErr)
	}
	return s.writeError(s.ctx, id, &appserver.RPCError{Code: appserver.ErrorCodeInternal, Message: err.Error()})
}

func (s *session) writeResult(ctx context.Context, id appserver.RequestID, result any) error {
	data, err := json.Marshal(outboundResponse{ID: id, Result: result})
	if err != nil {
		return err
	}
	return s.writeTerminalAndWait(ctx, data, id)
}

func (s *session) writeError(ctx context.Context, id appserver.RequestID, rpcErr *appserver.RPCError) error {
	data, err := json.Marshal(outboundError{ID: id, Error: rpcErr})
	if err != nil {
		return err
	}
	return s.writeTerminalAndWait(ctx, data, id)
}

func (s *session) write(ctx context.Context, data []byte) error {
	return s.enqueueWrite(ctx, queuedWrite{data: data})
}

func (s *session) writeTerminalAndWait(ctx context.Context, data []byte, id appserver.RequestID) error {
	sent := make(chan error, 1)
	if err := s.enqueueWrite(ctx, queuedWrite{
		data: data, sent: sent, requestID: id, releasesRequest: true,
	}); err != nil {
		return err
	}
	select {
	case err := <-sent:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	}
}

func (s *session) enqueueWrite(ctx context.Context, write queuedWrite) error {
	data := write.data
	if int64(len(data)) > s.proxy.opts.MaxMessageSize {
		return fmt.Errorf("message is %d bytes; maximum is %d", len(data), s.proxy.opts.MaxMessageSize)
	}
	select {
	case s.writes <- write:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	}
}

func (s *session) tryWrite(data []byte) bool {
	if int64(len(data)) > s.proxy.opts.MaxMessageSize {
		return false
	}
	select {
	case s.writes <- queuedWrite{data: data}:
		return true
	case <-s.done:
		return false
	default:
		return false
	}
}

// offerNotification preserves the app-server's response-before-notification
// order across thread/resume and turn/start. Notifications that arrive after
// one of those requests begins are held until its response has been written;
// notifications before the initial resume have no downstream thread context
// and are dropped.
func (s *session) offerNotification(method string, data []byte) notificationDisposition {
	s.mu.Lock()
	if _, optedOut := s.optOut[method]; optedOut {
		s.mu.Unlock()
		return notificationDropped
	}
	if !s.ready && !s.barrier {
		s.mu.Unlock()
		return notificationDropped
	}
	if int64(len(data)) > s.proxy.opts.MaxMessageSize {
		s.mu.Unlock()
		return notificationOverflow
	}
	if s.barrier {
		if len(s.barrierQueue) >= s.proxy.opts.WriteQueueSize {
			s.mu.Unlock()
			return notificationOverflow
		}
		s.barrierQueue = append(s.barrierQueue, data)
		s.mu.Unlock()
		return notificationAccepted
	}
	s.mu.Unlock()
	if !s.tryWrite(data) {
		return notificationOverflow
	}
	return notificationAccepted
}

func (s *session) writeLoop() {
	for {
		select {
		case write := <-s.writes:
			if write.releasesRequest {
				// Commit the terminal response before making its bytes visible.
				// A peer can legally reuse the ID as soon as it receives this
				// frame. Any response produced by that reused ID is serialized
				// behind this write. A write failure closes the whole session.
				s.releaseRequest(write.requestID)
			}
			writeCtx, cancel := context.WithTimeout(s.ctx, s.proxy.opts.WriteTimeout)
			err := s.conn.Write(writeCtx, websocket.MessageText, write.data)
			cancel()
			if write.sent != nil {
				write.sent <- err
			}
			if err != nil {
				s.close(websocket.StatusGoingAway, "write failed")
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *session) close(status websocket.StatusCode, reason string) {
	s.closeOne.Do(func() {
		s.settle()
		// Give a responsive peer time to receive and acknowledge the close
		// frame, then cancel active I/O so a nonresponsive peer cannot retain
		// the sole-session slot for the websocket library's full close timeout.
		closed := make(chan struct{})
		go func() {
			_ = s.conn.Close(status, reason)
			close(closed)
		}()
		go func() {
			timer := time.NewTimer(closeHandshakeGrace)
			defer timer.Stop()
			select {
			case <-closed:
			case <-timer.C:
			}
			s.cancel()
		}()
	})
}

func (s *session) abort() {
	s.closeOne.Do(func() {
		s.settle()
		s.cancel()
		go func() {
			_ = s.conn.CloseNow()
		}()
	})
}

func (s *session) settle() {
	s.mu.Lock()
	pending := s.pending
	s.pending = make(map[appserver.RequestID]chan reverseResponse)
	s.expired = make(map[appserver.RequestID]struct{})
	s.expiredFIFO = nil
	s.requests = make(map[appserver.RequestID]struct{})
	s.initialized = false
	s.initializing = false
	s.barrier = false
	s.barrierReady = false
	if s.barrierDone != nil {
		close(s.barrierDone)
		s.barrierDone = nil
	}
	s.ready = false
	s.barrierQueue = nil
	s.mu.Unlock()
	close(s.done)
	for _, waiter := range pending {
		waiter <- reverseResponse{err: ErrNoAttachedTUI}
	}
}

func (s *session) beginHandler() bool {
	select {
	case s.proxy.handlers <- struct{}{}:
		s.proxy.handlerWG.Add(1)
		return true
	default:
		return false
	}
}

func (s *session) endHandler() {
	<-s.proxy.handlers
	s.proxy.handlerWG.Done()
}

func (s *session) claimRequest(id appserver.RequestID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.requests[id]; exists {
		return false
	}
	s.requests[id] = struct{}{}
	return true
}

func (s *session) releaseRequest(id appserver.RequestID) {
	s.mu.Lock()
	delete(s.requests, id)
	s.mu.Unlock()
}

func (s *session) isInitialized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initialized
}

func (s *session) isReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ready
}

func (s *session) wasReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.everReady
}

func (s *session) beginNotificationBarrier(method string) (bool, *appserver.RPCError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.barrier {
		return false, &appserver.RPCError{
			Code:    appserver.ErrorCodeInvalidRequest,
			Message: "thread/resume or turn/start is already in progress",
		}
	}
	s.barrier = true
	s.barrierReady = method == appserver.MethodThreadResume && !s.ready
	s.barrierDone = make(chan struct{})
	establishesReady := s.barrierReady
	s.barrierQueue = nil
	return establishesReady, nil
}

func (s *session) cancelNotificationBarrier() {
	s.mu.Lock()
	if s.barrier {
		s.barrier = false
		s.barrierReady = false
		s.barrierQueue = nil
		if s.barrierDone != nil {
			close(s.barrierDone)
			s.barrierDone = nil
		}
	}
	s.mu.Unlock()
}

func (s *session) completeNotificationBarrier() (bool, error) {
	for {
		s.mu.Lock()
		if !s.barrier {
			s.mu.Unlock()
			return false, ErrClosed
		}
		if len(s.barrierQueue) == 0 {
			first := s.barrierReady && !s.everReady
			if s.barrierReady {
				s.ready = true
				s.everReady = true
			}
			s.barrier = false
			s.barrierReady = false
			if s.barrierDone != nil {
				close(s.barrierDone)
				s.barrierDone = nil
			}
			s.mu.Unlock()
			return first, nil
		}
		buffered := s.barrierQueue
		s.barrierQueue = nil
		s.mu.Unlock()

		for _, data := range buffered {
			if !s.tryWrite(data) {
				return false, errors.New("appserver proxy: TUI write queue is full while completing response notification barrier")
			}
		}
	}
}

func (s *session) waitForResumeBarrier() bool {
	s.mu.Lock()
	if s.ready {
		s.mu.Unlock()
		return true
	}
	if !s.barrier || !s.barrierReady || s.barrierDone == nil {
		s.mu.Unlock()
		return false
	}
	done := s.barrierDone
	s.mu.Unlock()

	select {
	case <-done:
		return s.isReady()
	case <-s.done:
		return false
	}
}

type outboundRequest struct {
	ID     appserver.RequestID `json:"id"`
	Method string              `json:"method"`
	Params json.RawMessage     `json:"params,omitempty"`
}

type outboundNotification struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type outboundResponse struct {
	ID     appserver.RequestID `json:"id"`
	Result any                 `json:"result"`
}

type outboundError struct {
	ID    appserver.RequestID `json:"id"`
	Error *appserver.RPCError `json:"error"`
}

func optionalString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("method must be a string")
	}
	return value, nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}
