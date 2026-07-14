// Package mcp implements the slice of the Model Context Protocol that the
// intercom shim needs:
//
//   - Stdio transport (newline-delimited JSON-RPC 2.0)
//   - The initialize / notifications/initialized handshake
//   - tools/list and tools/call dispatch
//   - Public Notify(method, params) for sending arbitrary outbound notifications
//
// The package keeps non-standard outbound notifications explicit because the
// shim emits notifications/claude/channel events.
//
// This is not a full MCP implementation. It serves one well-defined client
// (Claude Code) over one transport (stdio).
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// LatestProtocolVersion is the MCP protocol version this implementation
// targets. We echo the client's version when accepting initialize, falling
// back to this constant if the client is unspecified.
const LatestProtocolVersion = "2025-11-25"

// Implementation describes the server (or client) at a high level. Sent in
// the initialize result's serverInfo field.
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Options configures a Server. All fields are optional.
type Options struct {
	// Instructions is added to the initialize result. Claude Code includes
	// this in the model's system prompt.
	Instructions string

	// Experimental populates capabilities.experimental verbatim. The intercom
	// shim sets this to {"claude/channel": {}}.
	Experimental map[string]any
}

// Tool is a single registered MCP tool.
type Tool struct {
	Name        string
	Description string
	// InputSchema is a raw JSON object describing the tool's parameters as
	// JSON Schema. Stored raw so callers can author it inline without paying
	// a reflective inference dependency.
	InputSchema json.RawMessage
	Handler     ToolHandler
}

// ToolResult is what a tool handler returns: a text payload and whether to
// flag it as an error to the caller. Maps to MCP's CallToolResult.
type ToolResult struct {
	Text    string
	IsError bool
}

// ToolHandler implements a registered tool. It is invoked once per tools/call
// request and may run concurrently with other tool calls.
//
// Return a non-nil error only for protocol-level failures (e.g. malformed
// arguments that bypass the schema, internal panics). For user-facing errors
// — "no such peer", "broker disconnected" — return a ToolResult with
// IsError=true; that surfaces in Claude's context as part of the tool output
// rather than as an MCP error response.
type ToolHandler func(ctx context.Context, args json.RawMessage) (ToolResult, error)

// Server is a stdio-based MCP server. Construct with NewServer, register tools
// with RegisterTool, then run with Run.
type Server struct {
	impl Implementation
	opts Options

	toolsMu sync.RWMutex
	tools   map[string]Tool

	// writeM serializes JSON-RPC frames to the output (responses + Notify).
	writeM sync.Mutex
	out    io.Writer

	// initialized is closed once the client has sent notifications/initialized.
	// Notifications emitted before that are still flushed; the client may or
	// may not see them depending on its timing.
	initOnce sync.Once
	initCh   chan struct{}
}

// NewServer constructs a Server. The server does no I/O until Run is called.
func NewServer(impl Implementation, opts Options) *Server {
	return &Server{
		impl:   impl,
		opts:   opts,
		tools:  make(map[string]Tool),
		initCh: make(chan struct{}),
	}
}

// RegisterTool adds a tool. Must be called before Run; tool registration is
// not safe to mutate while the server is serving (and the spec requires sending
// listChanged, which we don't implement).
func (s *Server) RegisterTool(t Tool) {
	if t.Name == "" {
		panic("mcp: tool name required")
	}
	if t.Handler == nil {
		panic("mcp: tool handler required for " + t.Name)
	}
	s.toolsMu.Lock()
	s.tools[t.Name] = t
	s.toolsMu.Unlock()
}

// Initialized returns a channel closed after the client has completed the
// initialize/initialized handshake. Callers may want to wait on this before
// emitting their first Notify.
func (s *Server) Initialized() <-chan struct{} { return s.initCh }

// Notify sends a JSON-RPC notification with the given method and params.
// Goroutine-safe. Returns an error only if marshaling or stdout write fails.
func (s *Server) Notify(method string, params any) error {
	msg := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	return s.writeMessage(&msg)
}

// Run serves MCP over the given reader/writer until in EOFs or ctx is
// cancelled. It returns nil on a clean EOF, ctx.Err() on cancellation, or a
// non-nil error on a fatal I/O or protocol failure.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out

	// bufio.Scanner default buffer is 64 KiB; raise it so a max-frame request
	// fits.
	sc := bufio.NewScanner(in)
	const maxLine = 8 * 1024 * 1024
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)

	// Concurrent tool calls: each request handler runs in its own goroutine,
	// tracked so we can wait for them on shutdown.
	var wg sync.WaitGroup
	defer wg.Wait()

	// Pump scanner in a goroutine so ctx cancellation can interrupt the wait.
	type lineOrErr struct {
		line []byte
		err  error
	}
	lines := make(chan lineOrErr, 1)
	go func() {
		defer close(lines)
		for sc.Scan() {
			b := append([]byte(nil), sc.Bytes()...)
			select {
			case lines <- lineOrErr{line: b}:
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil {
			lines <- lineOrErr{err: err}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case lo, ok := <-lines:
			if !ok {
				return nil
			}
			if lo.err != nil {
				if errors.Is(lo.err, io.EOF) {
					return nil
				}
				return fmt.Errorf("mcp: read: %w", lo.err)
			}
			if len(lo.line) == 0 {
				continue
			}
			s.dispatch(ctx, &wg, lo.line)
		}
	}
}

// dispatch parses a single JSON-RPC frame and routes it. Synchronous handlers
// (initialize, tools/list, notifications) run inline; tool calls fork into
// the wg-tracked goroutine pool so a slow tool doesn't block the read loop.
func (s *Server) dispatch(ctx context.Context, wg *sync.WaitGroup, raw []byte) {
	var msg jsonrpcMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		// We can't reply because we don't know the id; per JSON-RPC the
		// server may send a parse-error notification with id=null, but most
		// clients ignore it. Drop silently — Claude Code won't send malformed
		// frames.
		return
	}
	notification, validID := classifyRequestID(msg.ID)
	if !validID {
		s.replyError(nil, codeInvalidRequest, "id must be a string, number, or null")
		return
	}
	if msg.JSONRPC != "2.0" {
		// Wrong version: protocol-level error.
		s.replyError(msg.ID, codeInvalidRequest, "expected jsonrpc 2.0")
		return
	}

	// Notification: no id, no response expected.
	if notification {
		switch msg.Method {
		case "notifications/initialized":
			s.initOnce.Do(func() { close(s.initCh) })
		default:
			// Other notifications (cancellation, progress, etc.) are not
			// relevant to this server. Drop silently.
		}
		return
	}

	// Request: must produce a response with the same id.
	switch msg.Method {
	case "initialize":
		s.handleInitialize(msg.ID, msg.Params)
	case "tools/list":
		s.handleListTools(msg.ID)
	case "tools/call":
		// Tool calls may block; run concurrently.
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleCallTool(ctx, msg.ID, msg.Params)
		}()
	case "ping":
		// Standard MCP ping: empty result.
		s.replyResult(msg.ID, struct{}{})
	default:
		s.replyError(msg.ID, codeMethodNotFound, "method not found: "+msg.Method)
	}
}

// ----- Request handlers ---------------------------------------------------

type initializeParams struct {
	ProtocolVersion string `json:"protocolVersion"`
}

type initializeResult struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      Implementation `json:"serverInfo"`
	Instructions    string         `json:"instructions,omitempty"`
}

func (s *Server) handleInitialize(id json.RawMessage, paramsRaw json.RawMessage) {
	var params initializeParams
	if len(paramsRaw) > 0 {
		_ = json.Unmarshal(paramsRaw, &params) // tolerate missing/partial params
	}

	caps := map[string]any{}
	s.toolsMu.RLock()
	if len(s.tools) > 0 {
		caps["tools"] = struct{}{}
	}
	s.toolsMu.RUnlock()
	if len(s.opts.Experimental) > 0 {
		caps["experimental"] = s.opts.Experimental
	}

	version := params.ProtocolVersion
	if version == "" {
		version = LatestProtocolVersion
	}

	s.replyResult(id, initializeResult{
		ProtocolVersion: version,
		Capabilities:    caps,
		ServerInfo:      s.impl,
		Instructions:    s.opts.Instructions,
	})
}

type listToolsResult struct {
	Tools []toolDef `json:"tools"`
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (s *Server) handleListTools(id json.RawMessage) {
	s.toolsMu.RLock()
	defer s.toolsMu.RUnlock()
	out := listToolsResult{Tools: make([]toolDef, 0, len(s.tools))}
	for _, t := range s.tools {
		out.Tools = append(out.Tools, toolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	s.replyResult(id, out)
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type callToolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *Server) handleCallTool(ctx context.Context, id json.RawMessage, paramsRaw json.RawMessage) {
	var params callToolParams
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		s.replyError(id, codeInvalidParams, "invalid tools/call params: "+err.Error())
		return
	}

	s.toolsMu.RLock()
	tool, ok := s.tools[params.Name]
	s.toolsMu.RUnlock()
	if !ok {
		s.replyError(id, codeMethodNotFound, "unknown tool: "+params.Name)
		return
	}

	// Default args to {} so handlers can always Unmarshal without a nil check.
	args := params.Arguments
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}

	res, err := s.invokeHandler(ctx, tool, args)
	if err != nil {
		s.replyError(id, codeInternal, fmt.Sprintf("tool %q: %v", tool.Name, err))
		return
	}
	s.replyResult(id, callToolResult{
		Content: []textContent{{Type: "text", Text: res.Text}},
		IsError: res.IsError,
	})
}

// invokeHandler runs a tool handler with a panic guard so a buggy handler
// can't take down the whole shim.
func (s *Server) invokeHandler(ctx context.Context, tool Tool, args json.RawMessage) (res ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return tool.Handler(ctx, args)
}

// ----- JSON-RPC plumbing ---------------------------------------------------

// jsonrpcMessage models any JSON-RPC 2.0 frame. RawMessage preserves the exact
// string or number spelling used for request correlation.
type jsonrpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

// classifyRequestID returns notification=true for an absent or null ID. A
// request ID must be a JSON string or number; all other JSON types violate the
// JSON-RPC contract.
func classifyRequestID(id json.RawMessage) (notification, valid bool) {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return true, true
	}
	switch trimmed[0] {
	case '"', '-':
		return false, true
	default:
		return false, trimmed[0] >= '0' && trimmed[0] <= '9'
	}
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const (
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

func (s *Server) replyResult(id json.RawMessage, result any) {
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: result}
	// If the write fails the session is dead; the next read on Run will
	// return an error and unwind cleanly. Nothing useful to do here.
	_ = s.writeMessage(&msg)
}

func (s *Server) replyError(id json.RawMessage, code int, message string) {
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   *jsonrpcError   `json:"error"`
	}{JSONRPC: "2.0", ID: id, Error: &jsonrpcError{Code: code, Message: message}}
	_ = s.writeMessage(&msg)
}

// writeMessage marshals m and writes it as a single newline-terminated frame.
// Goroutine-safe.
func (s *Server) writeMessage(m any) error {
	buf, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("mcp: marshal: %w", err)
	}
	buf = append(buf, '\n')

	s.writeM.Lock()
	defer s.writeM.Unlock()
	if s.out == nil {
		return errors.New("mcp: server not running")
	}
	if _, err := s.out.Write(buf); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	return nil
}
