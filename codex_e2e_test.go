package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/codex"
)

const codexE2ETimeout = 3 * time.Second

type codexAppMessage struct {
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
	JSONRPC json.RawMessage `json:"jsonrpc"`
	Raw     []byte          `json:"-"`
}

type fakeCodexAppServer struct {
	endpoint string

	ctx    context.Context
	cancel context.CancelFunc
	server *http.Server
	ln     net.Listener

	connected    chan struct{}
	disconnected chan struct{}
	incoming     chan codexAppMessage
	errs         chan error

	connMu         sync.Mutex
	conn           *websocket.Conn
	writeMu        sync.Mutex
	disconnectOnce sync.Once
}

func startFakeCodexAppServer(t *testing.T) *fakeCodexAppServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "codex-app-server.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := &fakeCodexAppServer{
		endpoint:     "unix://" + path,
		ctx:          ctx,
		cancel:       cancel,
		ln:           ln,
		connected:    make(chan struct{}),
		disconnected: make(chan struct{}),
		incoming:     make(chan codexAppMessage, 32),
		errs:         make(chan error, 4),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.server = &http.Server{Handler: mux}
	go func() {
		if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.report(fmt.Errorf("serve fake app-server: %w", err))
		}
	}()

	t.Cleanup(func() {
		s.cancel()
		s.connMu.Lock()
		conn := s.conn
		s.connMu.Unlock()
		if conn != nil {
			_ = conn.CloseNow()
		}
		_ = s.server.Close()
		_ = s.ln.Close()
	})
	return s
}

func (s *fakeCodexAppServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.report(fmt.Errorf("unexpected websocket path %q", r.URL.Path))
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.report(fmt.Errorf("accept websocket: %w", err))
		return
	}
	defer s.disconnectOnce.Do(func() { close(s.disconnected) })
	defer conn.CloseNow()

	s.connMu.Lock()
	if s.conn != nil {
		s.connMu.Unlock()
		s.report(errors.New("fake app-server received a second connection"))
		return
	}
	s.conn = conn
	close(s.connected)
	s.connMu.Unlock()

	for {
		typ, raw, err := conn.Read(s.ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			s.report(fmt.Errorf("app-server client wrote websocket type %v", typ))
			return
		}
		var message codexAppMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			s.report(fmt.Errorf("decode app-server client message: %w", err))
			return
		}
		message.Raw = append([]byte(nil), raw...)
		select {
		case s.incoming <- message:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *fakeCodexAppServer) report(err error) {
	select {
	case s.errs <- err:
	default:
	}
}

func (s *fakeCodexAppServer) next(t *testing.T) codexAppMessage {
	t.Helper()
	select {
	case err := <-s.errs:
		t.Fatalf("fake app-server: %v", err)
	case message := <-s.incoming:
		if len(message.JSONRPC) != 0 {
			t.Fatalf("app-server message unexpectedly contains jsonrpc: %s", message.Raw)
		}
		return message
	case <-s.disconnected:
		t.Fatal("app-server client disconnected unexpectedly")
	case <-time.After(codexE2ETimeout):
		t.Fatal("timeout waiting for app-server client message")
	}
	return codexAppMessage{}
}

func (s *fakeCodexAppServer) expectMethod(t *testing.T, method string) codexAppMessage {
	t.Helper()
	message := s.next(t)
	if message.Method != method {
		t.Fatalf("app-server method = %q, want %q; message: %s", message.Method, method, message.Raw)
	}
	if len(message.ID) == 0 && method != appserver.MethodInitialized {
		t.Fatalf("app-server request %q has no id: %s", method, message.Raw)
	}
	return message
}

func (s *fakeCodexAppServer) expectResponse(t *testing.T, id string) codexAppMessage {
	t.Helper()
	message := s.next(t)
	if message.Method != "" {
		t.Fatalf("got method %q while waiting for response %q: %s", message.Method, id, message.Raw)
	}
	var got string
	if err := json.Unmarshal(message.ID, &got); err != nil {
		t.Fatalf("decode response id: %v; message: %s", err, message.Raw)
	}
	if got != id {
		t.Fatalf("response id = %q, want %q", got, id)
	}
	if len(message.Error) != 0 {
		t.Fatalf("response %q contains error: %s", id, message.Error)
	}
	return message
}

func (s *fakeCodexAppServer) write(t *testing.T, value any) {
	t.Helper()
	select {
	case <-s.connected:
	case err := <-s.errs:
		t.Fatalf("fake app-server: %v", err)
	case <-time.After(codexE2ETimeout):
		t.Fatal("timeout waiting for app-server connection")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	s.connMu.Lock()
	conn := s.conn
	s.connMu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), codexE2ETimeout)
	defer cancel()
	s.writeMu.Lock()
	err = conn.Write(ctx, websocket.MessageText, raw)
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("write fake app-server message: %v", err)
	}
}

func (s *fakeCodexAppServer) respond(t *testing.T, request codexAppMessage, result any) {
	t.Helper()
	if len(request.ID) == 0 {
		t.Fatalf("cannot respond to message without id: %s", request.Raw)
	}
	s.write(t, map[string]any{"id": request.ID, "result": result})
}

func (s *fakeCodexAppServer) reverseTool(t *testing.T, id, turnID, tool string, arguments any) {
	t.Helper()
	s.write(t, map[string]any{
		"id":     id,
		"method": appserver.MethodDynamicToolCall,
		"params": map[string]any{
			"threadId":  "thread-e2e",
			"turnId":    turnID,
			"callId":    "call-" + id,
			"namespace": nil,
			"tool":      tool,
			"arguments": arguments,
		},
	})
}

type codexE2ERun struct {
	cancel   context.CancelFunc
	finished chan struct{}
	err      error
}

func startCodexE2EController(t *testing.T, server *fakeCodexAppServer, brokerSocket, cwd string) *codexE2ERun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	run := &codexE2ERun{cancel: cancel, finished: make(chan struct{})}
	stateDir := t.TempDir()
	go func() {
		run.err = codex.Run(ctx, codex.Config{
			Name:              "reviewer",
			Version:           "test-version",
			CWD:               cwd,
			AppServerEndpoint: server.endpoint,
			BrokerSocket:      brokerSocket,
			BrokerBin:         "/nonexistent",
			QueueSize:         8,
			StartupTimeout:    codexE2ETimeout,
			ControlTimeout:    codexE2ETimeout,
			ReverseTimeout:    codexE2ETimeout,
			ActivityTimeout:   10 * time.Second,
			StatePath:         filepath.Join(stateDir, "reviewer.json"),
			LockPath:          filepath.Join(stateDir, "reviewer.lock"),
			Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		close(run.finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-run.finished:
		case <-time.After(codexE2ETimeout + time.Second):
			t.Errorf("Codex controller did not stop")
		}
	})
	return run
}

func (r *codexE2ERun) stop(t *testing.T, server *fakeCodexAppServer) {
	t.Helper()
	r.cancel()
	timer := time.NewTimer(codexE2ETimeout + time.Second)
	defer timer.Stop()
	for {
		select {
		case <-r.finished:
			if !errors.Is(r.err, context.Canceled) {
				t.Fatalf("Codex controller exit = %v, want context canceled", r.err)
			}
			return
		case message := <-server.incoming:
			if message.Method != appserver.MethodTurnInterrupt {
				t.Fatalf("unexpected app-server message during shutdown: %s", message.Raw)
			}
			var params appserver.TurnInterruptParams
			if err := json.Unmarshal(message.Params, &params); err != nil {
				t.Fatal(err)
			}
			server.respond(t, message, appserver.TurnInterruptResponse{})
			server.write(t, map[string]any{
				"method": appserver.NotificationTurnCompleted,
				"params": appserver.TurnCompletedNotification{
					ThreadID: params.ThreadID,
					Turn:     appserver.Turn{ID: params.TurnID, Status: appserver.TurnStatusInterrupted},
				},
			})
		case err := <-server.errs:
			t.Fatalf("fake app-server during shutdown: %v", err)
		case <-timer.C:
			t.Fatal("timeout stopping Codex controller")
		}
	}
}

func TestEndToEndClaudeAndCodexAdapters(t *testing.T) {
	brokerSocket, _ := startBroker(t)
	alice := startShim(t, t.Context(), "alice", brokerSocket)
	alice.initialize(1)
	callShimTool(t, alice, 10, "list_peers", map[string]any{})

	server := startFakeCodexAppServer(t)
	project := t.TempDir()
	run := startCodexE2EController(t, server, brokerSocket, project)

	initialize := server.expectMethod(t, appserver.MethodInitialize)
	var initializeParams appserver.InitializeParams
	if err := json.Unmarshal(initialize.Params, &initializeParams); err != nil {
		t.Fatal(err)
	}
	if initializeParams.ClientInfo.Version != "test-version" || initializeParams.Capabilities == nil || !initializeParams.Capabilities.ExperimentalAPI {
		t.Fatalf("initialize params = %+v", initializeParams)
	}
	server.respond(t, initialize, appserver.InitializeResponse{
		UserAgent:      "codex_cli_rs/" + appserver.ProtocolVersion,
		CodexHome:      filepath.Join(project, "codex-home"),
		PlatformFamily: "unix",
		PlatformOS:     "linux",
	})
	server.expectMethod(t, appserver.MethodInitialized)

	threadStart := server.expectMethod(t, appserver.MethodThreadStart)
	var threadStartParams appserver.ThreadStartParams
	if err := json.Unmarshal(threadStart.Params, &threadStartParams); err != nil {
		t.Fatal(err)
	}
	if threadStartParams.CWD == nil || *threadStartParams.CWD != project || len(threadStartParams.DynamicTools) != 2 {
		t.Fatalf("thread/start params = %+v", threadStartParams)
	}
	thread := appserver.Thread{
		ID:        "thread-e2e",
		CWD:       project,
		Status:    appserver.ThreadStatus{Type: appserver.ThreadStatusIdle},
		Ephemeral: false,
		Turns:     make([]appserver.Turn, 0),
	}
	server.respond(t, threadStart, appserver.ThreadStartResponse{ThreadResponse: appserver.ThreadResponse{
		Thread:         thread,
		CWD:            project,
		ApprovalPolicy: string(appserver.ApprovalNever),
		Sandbox:        appserver.SandboxPolicy{Type: "workspaceWrite", NetworkAccess: false},
	}})
	waitForShimPeer(t, alice, "reviewer", run)

	callShimTool(t, alice, 20, "send_message", map[string]any{"to": "reviewer", "message": "first request"})
	firstStart := server.expectMethod(t, appserver.MethodTurnStart)
	firstParams := decodeTurnStart(t, firstStart)
	assertTurnPolicy(t, firstParams, project)
	if firstParams.ClientUserMessageID == nil || *firstParams.ClientUserMessageID == "" {
		t.Fatalf("first turn/start params = %+v", firstParams)
	}
	firstDeliveryID := *firstParams.ClientUserMessageID
	if input := firstParams.Input[0].Text; !strings.Contains(input, "From: alice") ||
		!strings.Contains(input, "Message-ID: "+firstDeliveryID) || !strings.Contains(input, "first request") {
		t.Fatalf("first turn input = %q", input)
	}
	server.respond(t, firstStart, appserver.TurnStartResponse{Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInProgress}})

	server.reverseTool(t, "tool-send", "turn-1", "send_message", map[string]any{"to": "alice", "message": "reply from codex"})
	reply := recvShimChannel(t, alice)
	if reply.Content != "reply from codex" || reply.From != "reviewer" {
		t.Fatalf("Claude notification = %+v", reply)
	}
	assertDynamicToolSuccess(t, server.expectResponse(t, "tool-send"))

	// The second delivery is acknowledged by the broker while turn-1 remains
	// active. A reverse-call barrier then proves the app-server stream contains
	// no second turn/start before turn-1 completes.
	callShimTool(t, alice, 21, "send_message", map[string]any{"to": "reviewer", "message": "second request"})
	server.reverseTool(t, "tool-barrier", "turn-1", "list_peers", map[string]any{})
	assertDynamicToolSuccess(t, server.expectResponse(t, "tool-barrier"))

	server.write(t, map[string]any{
		"method": appserver.NotificationTurnCompleted,
		"params": appserver.TurnCompletedNotification{
			ThreadID: "thread-e2e",
			Turn:     appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusCompleted},
		},
	})
	threadRead := server.expectMethod(t, appserver.MethodThreadRead)
	var readParams appserver.ThreadReadParams
	if err := json.Unmarshal(threadRead.Params, &readParams); err != nil {
		t.Fatal(err)
	}
	if readParams.ThreadID != "thread-e2e" || !readParams.IncludeTurns {
		t.Fatalf("thread/read params = %+v", readParams)
	}
	materialized := thread
	materialized.Turns = []appserver.Turn{{ID: "turn-1", Status: appserver.TurnStatusCompleted}}
	server.respond(t, threadRead, appserver.ThreadReadResponse{Thread: materialized})

	secondStart := server.expectMethod(t, appserver.MethodTurnStart)
	secondParams := decodeTurnStart(t, secondStart)
	assertTurnPolicy(t, secondParams, project)
	if secondParams.ClientUserMessageID == nil || *secondParams.ClientUserMessageID == "" || *secondParams.ClientUserMessageID == firstDeliveryID {
		t.Fatalf("second turn/start params = %+v", secondParams)
	}
	secondDeliveryID := *secondParams.ClientUserMessageID
	if input := secondParams.Input[0].Text; !strings.Contains(input, "From: alice") ||
		!strings.Contains(input, "Message-ID: "+secondDeliveryID) || !strings.Contains(input, "second request") {
		t.Fatalf("second turn input = %q", input)
	}
	server.respond(t, secondStart, appserver.TurnStartResponse{Turn: appserver.Turn{ID: "turn-2", Status: appserver.TurnStatusInProgress}})
	server.reverseTool(t, "tool-final-barrier", "turn-2", "list_peers", map[string]any{})
	assertDynamicToolSuccess(t, server.expectResponse(t, "tool-final-barrier"))

	run.stop(t, server)
}

func decodeTurnStart(t *testing.T, message codexAppMessage) appserver.TurnStartParams {
	t.Helper()
	var params appserver.TurnStartParams
	if err := json.Unmarshal(message.Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.ThreadID != "thread-e2e" || len(params.Input) != 1 {
		t.Fatalf("turn/start params = %+v", params)
	}
	return params
}

func assertTurnPolicy(t *testing.T, params appserver.TurnStartParams, cwd string) {
	t.Helper()
	if params.CWD == nil || *params.CWD != cwd || params.ApprovalPolicy != string(appserver.ApprovalNever) {
		t.Fatalf("turn/start policy = %+v", params)
	}
	if params.SandboxPolicy == nil || params.SandboxPolicy.Type != "workspaceWrite" || len(params.SandboxPolicy.WritableRoots) != 0 {
		t.Fatalf("turn/start sandbox = %#v", params.SandboxPolicy)
	}
}

func assertDynamicToolSuccess(t *testing.T, message codexAppMessage) {
	t.Helper()
	var response appserver.DynamicToolCallResponse
	if err := json.Unmarshal(message.Result, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success {
		t.Fatalf("dynamic tool response = %+v", response)
	}
}

func callShimTool(t *testing.T, session *shimSession, id int, name string, arguments any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": arguments},
	})
	if err != nil {
		t.Fatal(err)
	}
	session.send(string(raw))
	response := session.recvUntil(func(line []byte) bool {
		var envelope struct {
			ID int `json:"id"`
		}
		return json.Unmarshal(line, &envelope) == nil && envelope.ID == id
	})
	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(response, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Error) != 0 || result.Result.IsError {
		t.Fatalf("shim tool %s failed: %s", name, response)
	}
}

func waitForShimPeer(t *testing.T, session *shimSession, peer string, run *codexE2ERun) {
	t.Helper()
	deadline := time.Now().Add(codexE2ETimeout)
	for id := 100; time.Now().Before(deadline); id++ {
		select {
		case <-run.finished:
			t.Fatalf("Codex controller exited before broker registration: %v", run.err)
		default:
		}
		raw, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/call",
			"params":  map[string]any{"name": "list_peers", "arguments": map[string]any{}},
		})
		if err != nil {
			t.Fatal(err)
		}
		session.send(string(raw))
		response := session.recvUntil(func(line []byte) bool {
			var envelope struct {
				ID int `json:"id"`
			}
			return json.Unmarshal(line, &envelope) == nil && envelope.ID == id
		})
		if strings.Contains(string(response), peer) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("peer %q did not register", peer)
}

type shimChannelMessage struct {
	Content string
	From    string
}

func recvShimChannel(t *testing.T, session *shimSession) shimChannelMessage {
	t.Helper()
	raw := session.recvUntil(func(line []byte) bool {
		return strings.Contains(string(line), `"notifications/claude/channel"`)
	})
	var notification struct {
		Params struct {
			Content string `json:"content"`
			Meta    struct {
				From string `json:"from"`
			} `json:"meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &notification); err != nil {
		t.Fatal(err)
	}
	return shimChannelMessage{Content: notification.Params.Content, From: notification.Params.Meta.From}
}
