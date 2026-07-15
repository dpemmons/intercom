package appserverproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/dpemmons/intercom/internal/appserver"
)

const proxyTestTimeout = 5 * time.Second

// fakeUpstream is backed by a real appserver.Client connected to a test-owned
// WebSocket peer. That keeps PendingCall and ReverseRequest construction on the
// public appserver API while giving each test complete control over upstream
// request ordering and responses.
type fakeUpstream struct {
	client   *appserver.Client
	server   *http.Server
	listener net.Listener
	done     chan error
}

func startFakeUpstream(
	t *testing.T,
	opts appserver.Options,
	handler func(context.Context, *websocket.Conn) error,
) *fakeUpstream {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-upstream.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen fake upstream: %v", err)
	}
	done := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			done <- fmt.Errorf("accept fake upstream: %w", err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
		defer cancel()
		done <- handler(ctx, conn)
	})
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()

	client, err := appserver.DialUnix(context.Background(), "unix://"+path, opts)
	if err != nil {
		_ = server.Close()
		_ = listener.Close()
		t.Fatalf("dial fake upstream: %v", err)
	}
	fake := &fakeUpstream{client: client, server: server, listener: listener, done: done}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		_ = listener.Close()
	})
	return fake
}

func (f *fakeUpstream) StartCall(ctx context.Context, method string, params any) (*appserver.PendingCall, error) {
	return f.client.StartCall(ctx, method, params)
}

func (f *fakeUpstream) Notify(ctx context.Context, method string, params any) error {
	return f.client.Notify(ctx, method, params)
}

func (f *fakeUpstream) await(t *testing.T) {
	t.Helper()
	select {
	case err := <-f.done:
		if err != nil {
			t.Fatalf("fake upstream: %v", err)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("timed out waiting for fake upstream")
	}
}

type recordedCall struct {
	method string
	params json.RawMessage
}

type unusedUpstream struct {
	calls         chan recordedCall
	notifications chan recordedCall
	err           error
}

func newUnusedUpstream() *unusedUpstream {
	return &unusedUpstream{
		calls:         make(chan recordedCall, 16),
		notifications: make(chan recordedCall, 16),
		err:           errors.New("unexpected fake upstream call"),
	}
}

func (u *unusedUpstream) StartCall(_ context.Context, method string, params any) (*appserver.PendingCall, error) {
	u.calls <- recordedCall{method: method, params: marshalRaw(params)}
	return nil, u.err
}

func (u *unusedUpstream) Notify(_ context.Context, method string, params any) error {
	u.notifications <- recordedCall{method: method, params: marshalRaw(params)}
	return nil
}

func marshalRaw(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func proxyEndpoint(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tui-proxy.sock")
	return "unix://" + path, path
}

func listenTestProxy(t *testing.T, opts Options) *Proxy {
	t.Helper()
	if opts.Endpoint == "" {
		opts.Endpoint, _ = proxyEndpoint(t)
	}
	proxy, err := Listen(context.Background(), opts)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		_ = proxy.Close()
	})
	return proxy
}

func dialProxy(ctx context.Context, endpoint, requestPath string) (*websocket.Conn, *http.Response, error) {
	path, err := appserver.ParseUnixEndpoint(endpoint)
	if err != nil {
		return nil, nil, err
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
		ForceAttemptHTTP2: false,
	}
	defer transport.CloseIdleConnections()
	return websocket.Dial(ctx, "ws://localhost"+requestPath, &websocket.DialOptions{
		HTTPClient:      &http.Client{Transport: transport},
		CompressionMode: websocket.CompressionDisabled,
	})
}

func mustDialProxy(t *testing.T, endpoint, requestPath string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	defer cancel()
	conn, response, err := dialProxy(ctx, endpoint, requestPath)
	closeHTTPResponse(response)
	if err != nil {
		t.Fatalf("dial proxy %s: %v", requestPath, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

func closeHTTPResponse(response *http.Response) {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
}

func writeJSON(t *testing.T, conn *websocket.Conn, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal websocket message: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
}

func readJSON(t *testing.T, conn *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read websocket message: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("websocket message type = %v, want text", typ)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatalf("decode websocket message %q: %v", data, err)
	}
	return object
}

func readObject(ctx context.Context, conn *websocket.Conn) (map[string]json.RawMessage, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("message type = %v, want text", typ)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	return object, nil
}

func writeObject(ctx context.Context, conn *websocket.Conn, object any) error {
	data, err := json.Marshal(object)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func wireMethod(object map[string]json.RawMessage) (string, error) {
	var method string
	err := json.Unmarshal(object["method"], &method)
	return method, err
}

func wireID(object map[string]json.RawMessage) (appserver.RequestID, error) {
	var id appserver.RequestID
	err := json.Unmarshal(object["id"], &id)
	return id, err
}

func initializeTUI(t *testing.T, conn *websocket.Conn, optOut ...string) appserver.InitializeResponse {
	t.Helper()
	writeJSON(t, conn, map[string]any{
		"id":     "test-initialize",
		"method": appserver.MethodInitialize,
		"params": appserver.InitializeParams{
			ClientInfo: appserver.ClientInfo{Name: "codex-tui", Version: "test"},
			Capabilities: &appserver.InitializeCapabilities{
				ExperimentalAPI:           true,
				OptOutNotificationMethods: optOut,
			},
		},
	})
	response := readJSON(t, conn)
	id, err := wireID(response)
	if err != nil {
		t.Fatalf("initialize response id: %v", err)
	}
	if text, ok := id.Text(); !ok || text != "test-initialize" {
		t.Fatalf("initialize response id = %v", id)
	}
	if rawErr := response["error"]; len(rawErr) != 0 {
		t.Fatalf("initialize error = %s", rawErr)
	}
	var result appserver.InitializeResponse
	if err := json.Unmarshal(response["result"], &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	writeJSON(t, conn, map[string]any{"method": appserver.MethodInitialized})
	return result
}

func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(proxyTestTimeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func requireRPCError(t *testing.T, object map[string]json.RawMessage, code int64) appserver.RPCError {
	t.Helper()
	var rpcErr appserver.RPCError
	if err := json.Unmarshal(object["error"], &rpcErr); err != nil {
		t.Fatalf("decode RPC error: %v (message: %#v)", err, object)
	}
	if rpcErr.Code != code {
		t.Fatalf("RPC error = %+v, want code %d", rpcErr, code)
	}
	return rpcErr
}

func TestListenCreatesPrivateSocketAndCleansItUp(t *testing.T) {
	endpoint, path := proxyEndpoint(t)
	proxy, err := Listen(context.Background(), Options{Endpoint: endpoint, Upstream: newUnusedUpstream()})
	if err != nil {
		t.Fatal(err)
	}
	if proxy.Endpoint() != endpoint {
		t.Fatalf("Endpoint() = %q, want %q", proxy.Endpoint(), endpoint)
	}
	if proxy.opts.MaxMessageSize != 128<<20 {
		t.Fatalf("default MaxMessageSize = %d, want %d", proxy.opts.MaxMessageSize, int64(128<<20))
	}
	if proxy.opts.ReverseResponseTimeout != 30*time.Second {
		t.Fatalf("default ReverseResponseTimeout = %s, want 30s", proxy.opts.ReverseResponseTimeout)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat proxy socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("proxy path mode = %v, want Unix socket", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("proxy socket permissions = %04o, want 0600", got)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-proxy.Done():
	default:
		t.Fatal("Done was not closed after Close returned")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket after Close: %v, want not exist", err)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestRPCUpgradeInitializeIsTerminatedAndResponseIsReusable(t *testing.T) {
	upstream := newUnusedUpstream()
	endpoint, _ := proxyEndpoint(t)
	want := appserver.InitializeResponse{
		UserAgent: "codex_cli_rs/test", CodexHome: "/tmp/codex-home",
		PlatformFamily: "unix", PlatformOS: "linux",
	}
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: upstream, InitializeResponse: want,
	})

	conn := mustDialProxy(t, endpoint, "/rpc")
	if got := initializeTUI(t, conn); got != want {
		t.Fatalf("initialize response = %+v, want %+v", got, want)
	}
	select {
	case call := <-upstream.calls:
		t.Fatalf("initialize leaked upstream as call %+v", call)
	case notification := <-upstream.notifications:
		t.Fatalf("initialized leaked upstream as notification %+v", notification)
	default:
	}

	writeJSON(t, conn, map[string]any{
		"id": 22, "method": appserver.MethodInitialize,
		"params": appserver.InitializeParams{ClientInfo: appserver.ClientInfo{Name: "again"}},
	})
	duplicate := readJSON(t, conn)
	requireRPCError(t, duplicate, appserver.ErrorCodeInvalidRequest)

	conn.CloseNow()
	waitFor(t, "first TUI to detach", func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		return proxy.session == nil
	})
	second := mustDialProxy(t, endpoint, "/")
	if got := initializeTUI(t, second); got != want {
		t.Fatalf("initialize response after reconnect = %+v, want %+v", got, want)
	}
}

func TestPipelinedInitializeRequestsHaveOneDeterministicWinner(t *testing.T) {
	tests := []struct {
		name       string
		first      appserver.InitializeParams
		firstError int64
	}{
		{
			name: "first valid initialize wins",
			first: appserver.InitializeParams{
				ClientInfo:   appserver.ClientInfo{Name: "codex-tui", Version: "test"},
				Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
			},
		},
		{
			name: "invalid first initialize permits ordered retry",
			first: appserver.InitializeParams{
				ClientInfo:   appserver.ClientInfo{},
				Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
			},
			firstError: appserver.ErrorCodeInvalidParams,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, _ := proxyEndpoint(t)
			listenTestProxy(t, Options{Endpoint: endpoint, Upstream: newUnusedUpstream()})
			conn := mustDialProxy(t, endpoint, "/rpc")

			writeJSON(t, conn, map[string]any{
				"id": "first", "method": appserver.MethodInitialize, "params": test.first,
			})
			writeJSON(t, conn, map[string]any{
				"id": "second", "method": appserver.MethodInitialize,
				"params": appserver.InitializeParams{
					ClientInfo:   appserver.ClientInfo{Name: "codex-tui", Version: "test"},
					Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
				},
			})

			first := readJSON(t, conn)
			firstID, err := wireID(first)
			if err != nil || firstID.String() != "first" {
				t.Fatalf("first response id = %v, err = %v", firstID, err)
			}
			second := readJSON(t, conn)
			secondID, err := wireID(second)
			if err != nil || secondID.String() != "second" {
				t.Fatalf("second response id = %v, err = %v", secondID, err)
			}

			if test.firstError == 0 {
				if _, exists := first["error"]; exists {
					t.Fatalf("first initialize error = %s", first["error"])
				}
				requireRPCError(t, second, appserver.ErrorCodeInvalidRequest)
				return
			}
			requireRPCError(t, first, test.firstError)
			if _, exists := second["error"]; exists {
				t.Fatalf("second initialize error = %s", second["error"])
			}
		})
	}
}

func TestThreadUnsubscribeIsVirtualized(t *testing.T) {
	upstream := newUnusedUpstream()
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)

	writeJSON(t, conn, map[string]any{
		"id": 41, "method": appserver.MethodThreadUnsubscribe,
		"params": map[string]any{"threadId": "thread-1"},
	})
	response := readJSON(t, conn)
	if _, exists := response["error"]; exists {
		t.Fatalf("thread/unsubscribe error = %s", response["error"])
	}
	var result struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(response["result"], &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "unsubscribed" {
		t.Fatalf("thread/unsubscribe status = %q", result.Status)
	}
	select {
	case call := <-upstream.calls:
		t.Fatalf("thread/unsubscribe leaked upstream: %+v", call)
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	_, _, err := conn.Read(ctx)
	cancel()
	if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure {
		t.Fatalf("close after thread/unsubscribe = %v (status %v), want normal closure", err, status)
	}
	waitFor(t, "unsubscribed session removal", func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		return proxy.session == nil
	})
	reconnected := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, reconnected)
}

func TestInitializeRejectsIncompatibleCodexClientVersion(t *testing.T) {
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: newUnusedUpstream(), ExpectedClientVersion: appserver.ProtocolVersion,
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	writeJSON(t, conn, map[string]any{
		"id": "initialize", "method": appserver.MethodInitialize,
		"params": appserver.InitializeParams{
			ClientInfo:   appserver.ClientInfo{Name: "codex-tui", Version: "99.0.0"},
			Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
		},
	})
	response := readJSON(t, conn)
	rpcErr := requireRPCError(t, response, appserver.ErrorCodeInvalidRequest)
	if !strings.Contains(rpcErr.Message, appserver.ProtocolVersion) {
		t.Fatalf("initialize error = %q", rpcErr.Message)
	}

	writeJSON(t, conn, map[string]any{
		"id": "initialize-retry", "method": appserver.MethodInitialize,
		"params": appserver.InitializeParams{
			ClientInfo:   appserver.ClientInfo{Name: "codex-tui", Version: appserver.ProtocolVersion},
			Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
		},
	})
	retry := readJSON(t, conn)
	if _, exists := retry["error"]; exists {
		t.Fatalf("compatible initialize retry error = %s", retry["error"])
	}
}

func TestOnlyRootAndRPCPathsUpgrade(t *testing.T) {
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{Endpoint: endpoint, Upstream: newUnusedUpstream()})
	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	defer cancel()
	conn, response, err := dialProxy(ctx, endpoint, "/not-rpc")
	if conn != nil {
		conn.CloseNow()
		t.Fatal("unexpected websocket connection on unknown path")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusNotFound {
		status := "<nil>"
		if response != nil {
			status = response.Status
			closeHTTPResponse(response)
		}
		t.Fatalf("unknown path upgrade = (%s, %v), want HTTP 404", status, err)
	}
	closeHTTPResponse(response)
}

func TestArbitraryTUIRequestIDsAreRemappedAndCorrelated(t *testing.T) {
	upstreamIDs := make(chan map[string]appserver.RequestID, 1)
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		ids := make(map[string]appserver.RequestID)
		for range 2 {
			request, err := readObject(ctx, conn)
			if err != nil {
				return err
			}
			method, err := wireMethod(request)
			if err != nil {
				return err
			}
			id, err := wireID(request)
			if err != nil {
				return err
			}
			ids[method] = id
		}
		upstreamIDs <- ids
		for _, method := range []string{"request/two", "request/one"} {
			if err := writeObject(ctx, conn, map[string]any{
				"id": ids[method], "result": map[string]string{"method": method},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	conn := mustDialProxy(t, endpoint, "/")
	initializeTUI(t, conn)

	writeJSON(t, conn, map[string]any{"id": "tui/string/id", "method": "request/one", "params": map[string]int{"n": 1}})
	writeJSON(t, conn, map[string]any{"id": -42, "method": "request/two", "params": map[string]int{"n": 2}})

	ids := <-upstreamIDs
	firstNumber, firstOK := ids["request/one"].Number()
	secondNumber, secondOK := ids["request/two"].Number()
	if !firstOK || !secondOK || firstNumber == secondNumber {
		t.Fatalf("upstream IDs = %#v, want distinct proxy-owned numbers", ids)
	}

	results := make(map[string]string)
	for range 2 {
		response := readJSON(t, conn)
		id, err := wireID(response)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal(response["result"], &result); err != nil {
			t.Fatal(err)
		}
		results[id.String()] = result.Method
	}
	if results["tui/string/id"] != "request/one" || results["-42"] != "request/two" {
		t.Fatalf("correlated downstream results = %#v", results)
	}
	upstream.await(t)
}

func TestDuplicateInFlightRequestIDClosesWithoutSecondTerminalResponse(t *testing.T) {
	firstArrived := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		close(firstArrived)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		id, err := wireID(request)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{"id": id, "result": map[string]bool{"ok": true}})
	})
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)

	request := map[string]any{"id": "same-id", "method": "slow/request", "params": map[string]any{}}
	writeJSON(t, conn, request)
	select {
	case <-firstArrived:
	case <-time.After(proxyTestTimeout):
		t.Fatal("first request did not reach upstream")
	}
	writeJSON(t, conn, request)

	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	typ, data, err := conn.Read(ctx)
	cancel()
	if err == nil {
		t.Fatalf("duplicate request produced a second terminal frame: type=%v data=%s", typ, data)
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
		t.Fatalf("duplicate request close = %v (status %v), want policy violation", err, status)
	}
	waitFor(t, "duplicate-id session removal", func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		return proxy.session == nil
	})

	releaseOnce.Do(func() { close(release) })
	upstream.await(t)
}

func TestRequestIDMayBeReusedAfterEachTerminalResponse(t *testing.T) {
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: newUnusedUpstream(),
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			switch method {
			case appserver.MethodThreadResume:
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			case "config/read":
				return json.RawMessage(`{"ok":true}`), true, nil
			default:
				return nil, false, nil
			}
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)

	const reusedID = "reused-id"
	writeJSON(t, conn, map[string]any{
		"id": reusedID, "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	if response := readJSON(t, conn); len(response["error"]) != 0 {
		t.Fatalf("resume error = %s", response["error"])
	}

	for i := 0; i < 128; i++ {
		writeJSON(t, conn, map[string]any{
			"id": reusedID, "method": "config/read", "params": map[string]int{"sequence": i},
		})
		response := readJSON(t, conn)
		id, err := wireID(response)
		if err != nil || id.String() != reusedID || len(response["error"]) != 0 {
			t.Fatalf("response %d = %#v, id %v, err %v", i, response, id, err)
		}
	}

	waitFor(t, "completed request-ID claims to be released", func() bool {
		proxy.mu.Lock()
		session := proxy.session
		proxy.mu.Unlock()
		if session == nil {
			return false
		}
		session.mu.Lock()
		defer session.mu.Unlock()
		return len(session.requests) == 0
	})
}

func TestBeforeAfterHooksRewriteAndObserveResult(t *testing.T) {
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		var params map[string]any
		if err := json.Unmarshal(request["params"], &params); err != nil {
			return err
		}
		if params["rewritten"] != true || params["original"] != nil {
			return fmt.Errorf("upstream params = %#v", params)
		}
		id, err := wireID(request)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{"id": id, "result": map[string]string{"value": "done"}})
	})
	type afterObservation struct {
		method string
		state  any
		result json.RawMessage
		err    error
	}
	after := make(chan afterObservation, 1)
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: upstream,
		BeforeRequest: func(method string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
			if method != "hook/test" || string(params) != `{"original":true}` {
				t.Errorf("BeforeRequest(%q, %s)", method, params)
			}
			return json.RawMessage(`{"rewritten":true}`), "hook-state", nil
		},
		AfterRequest: func(method string, state any, result json.RawMessage, err error) {
			after <- afterObservation{method: method, state: state, result: result, err: err}
		},
	})
	conn := mustDialProxy(t, endpoint, "/")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{"id": "hook-id", "method": "hook/test", "params": map[string]bool{"original": true}})
	response := readJSON(t, conn)
	var result struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(response["result"], &result); err != nil || result.Value != "done" {
		t.Fatalf("downstream result = %s, err = %v", response["result"], err)
	}
	select {
	case observation := <-after:
		if observation.method != "hook/test" || observation.state != "hook-state" || observation.err != nil {
			t.Fatalf("AfterRequest observation = %+v", observation)
		}
		if string(observation.result) != `{"value":"done"}` {
			t.Fatalf("AfterRequest result = %s", observation.result)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("AfterRequest was not called")
	}
	upstream.await(t)
}

func TestLocalThreadResumeMarksSessionReadyWithoutUpstreamCall(t *testing.T) {
	upstream := newUnusedUpstream()
	attached := make(chan struct{}, 1)
	after := make(chan struct{}, 1)
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: upstream,
		BeforeRequest: func(method string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
			if method != appserver.MethodThreadResume {
				t.Errorf("BeforeRequest method = %q", method)
			}
			return json.RawMessage(`{"threadId":"managed-thread","rewritten":true}`), "resume-state", nil
		},
		LocalRequest: func(method string, params json.RawMessage, state any) (json.RawMessage, bool, error) {
			if method != appserver.MethodThreadResume || string(params) != `{"threadId":"managed-thread","rewritten":true}` || state != "resume-state" {
				t.Errorf("LocalRequest(%q, %s, %#v)", method, params, state)
			}
			return json.RawMessage(`{"thread":{"id":"managed-thread"}}`), true, nil
		},
		AfterRequest: func(method string, state any, result json.RawMessage, err error) {
			if method != appserver.MethodThreadResume || state != "resume-state" || string(result) != `{"thread":{"id":"managed-thread"}}` || err != nil {
				t.Errorf("AfterRequest(%q, %#v, %s, %v)", method, state, result, err)
			}
			after <- struct{}{}
		},
		OnAttach: func() { attached <- struct{}{} },
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "requested-thread"},
	})
	response := readJSON(t, conn)
	if _, exists := response["error"]; exists {
		t.Fatalf("local resume error = %s", response["error"])
	}
	if string(response["result"]) != `{"thread":{"id":"managed-thread"}}` {
		t.Fatalf("local resume result = %s", response["result"])
	}
	waitFor(t, "local resume readiness", proxy.Attached)
	select {
	case <-attached:
	case <-time.After(proxyTestTimeout):
		t.Fatal("local resume did not invoke OnAttach")
	}
	select {
	case <-after:
	case <-time.After(proxyTestTimeout):
		t.Fatal("local resume did not invoke AfterRequest")
	}
	select {
	case call := <-upstream.calls:
		t.Fatalf("local resume leaked upstream: %+v", call)
	default:
	}
}

func TestAfterHookObservesUpstreamStartFailure(t *testing.T) {
	sentinel := errors.New("upstream unavailable")
	upstream := newUnusedUpstream()
	upstream.err = sentinel
	after := make(chan error, 1)
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: upstream,
		BeforeRequest: func(_ string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
			return params, "retained-state", nil
		},
		AfterRequest: func(_ string, state any, _ json.RawMessage, err error) {
			if state != "retained-state" {
				t.Errorf("AfterRequest state = %#v", state)
			}
			after <- err
		},
	})
	conn := mustDialProxy(t, endpoint, "/")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{"id": 7, "method": "failure/test", "params": map[string]any{}})
	response := readJSON(t, conn)
	rpcErr := requireRPCError(t, response, appserver.ErrorCodeInternal)
	if !strings.Contains(rpcErr.Message, sentinel.Error()) {
		t.Fatalf("RPC error message = %q", rpcErr.Message)
	}
	select {
	case err := <-after:
		if !errors.Is(err, sentinel) {
			t.Fatalf("AfterRequest error = %v", err)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("AfterRequest was not called")
	}
}

func TestNotificationsForwardAndHonorInitializeOptOut(t *testing.T) {
	resumeEntered := make(chan struct{})
	releaseResume := make(chan struct{})
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: newUnusedUpstream(),
		BeforeRequest: func(method string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
			if method == appserver.MethodThreadResume {
				close(resumeEntered)
				<-releaseResume
			}
			return params, nil, nil
		},
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			if method != appserver.MethodThreadResume {
				return nil, false, nil
			}
			return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
		},
	})

	// Notifications without a pending valid resume are dropped. Notifications
	// received after resume validation are buffered behind its response.
	proxy.Notify(appserver.Notification{Method: "before/initialize", Params: json.RawMessage(`{"ignored":true}`)})
	conn := mustDialProxy(t, endpoint, "/")
	initializeTUI(t, conn, "hidden/event")
	proxy.Notify(appserver.Notification{Method: "before/resume", Params: json.RawMessage(`{"ignored":true}`)})
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	select {
	case <-resumeEntered:
	case <-time.After(proxyTestTimeout):
		t.Fatal("local resume did not begin")
	}
	proxy.Notify(appserver.Notification{Method: "during/resume", Params: json.RawMessage(`{"buffered":true}`)})
	close(releaseResume)
	resumeResponse := readJSON(t, conn)
	resumeID, err := wireID(resumeResponse)
	if err != nil {
		t.Fatalf("resume response id: %v; message = %#v", err, resumeResponse)
	}
	if text, ok := resumeID.Text(); !ok || text != "resume" {
		t.Fatalf("first post-resume message id = %v, want resume response", resumeID)
	}
	buffered := readJSON(t, conn)
	method, err := wireMethod(buffered)
	if err != nil || method != "during/resume" {
		t.Fatalf("buffered post-resume notification = %#v, err = %v", buffered, err)
	}
	if string(buffered["params"]) != `{"buffered":true}` {
		t.Fatalf("buffered post-resume params = %s", buffered["params"])
	}
	waitFor(t, "notification session readiness", proxy.Attached)

	proxy.Notify(appserver.Notification{Method: "hidden/event", Params: json.RawMessage(`{"n":1}`)})
	proxy.Notify(appserver.Notification{Method: "visible/event", Params: json.RawMessage(`{"n":2}`)})
	forwarded := readJSON(t, conn)
	method, err = wireMethod(forwarded)
	if err != nil || method != "visible/event" {
		t.Fatalf("downstream notification = %#v, err = %v", forwarded, err)
	}
	if _, hasID := forwarded["id"]; hasID {
		t.Fatalf("downstream notification had id: %#v", forwarded)
	}
	if string(forwarded["params"]) != `{"n":2}` {
		t.Fatalf("downstream notification params = %s", forwarded["params"])
	}
}

func TestUpstreamNotificationCannotOvertakeResumeResponse(t *testing.T) {
	notificationSeen := make(chan struct{})
	var proxy *Proxy
	upstream := startFakeUpstream(t, appserver.Options{
		OnNotification: func(notification appserver.Notification) {
			proxy.Notify(notification)
			close(notificationSeen)
		},
	}, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := wireID(request)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": id, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}},
		}); err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"method": "thread/tokenUsage/updated",
			"params": map[string]any{"threadId": "thread-1", "total": 42},
		}); err != nil {
			return err
		}
		select {
		case <-notificationSeen:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	endpoint, _ := proxyEndpoint(t)
	proxy = listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: upstream,
		AfterRequest: func(method string, _ any, _ json.RawMessage, err error) {
			if method == appserver.MethodThreadResume && err == nil {
				<-notificationSeen
			}
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})

	response := readJSON(t, conn)
	responseID, err := wireID(response)
	if err != nil {
		t.Fatalf("resume response id: %v; message = %#v", err, response)
	}
	if text, ok := responseID.Text(); !ok || text != "resume" {
		t.Fatalf("first message id = %v, want resume response", responseID)
	}
	notification := readJSON(t, conn)
	method, err := wireMethod(notification)
	if err != nil || method != "thread/tokenUsage/updated" {
		t.Fatalf("second message = %#v, err = %v", notification, err)
	}
	waitFor(t, "upstream resume notification readiness", proxy.Attached)
	upstream.await(t)
}

func TestUpstreamCompletionCannotOvertakeTurnStartResponse(t *testing.T) {
	notificationSeen := make(chan struct{})
	var proxy *Proxy
	upstream := startFakeUpstream(t, appserver.Options{
		OnNotification: func(notification appserver.Notification) {
			proxy.Notify(notification)
			close(notificationSeen)
		},
	}, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := wireID(request)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": id, "result": map[string]any{"turn": map[string]string{"id": "turn-1", "status": "inProgress"}},
		}); err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"method": appserver.NotificationTurnCompleted,
			"params": map[string]any{
				"threadId": "thread-1",
				"turn":     map[string]string{"id": "turn-1", "status": "completed"},
			},
		}); err != nil {
			return err
		}
		select {
		case <-notificationSeen:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	endpoint, _ := proxyEndpoint(t)
	proxy = listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: upstream,
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			if method == appserver.MethodThreadResume {
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			}
			return nil, false, nil
		},
		AfterRequest: func(method string, _ any, _ json.RawMessage, err error) {
			if method == appserver.MethodTurnStart && err == nil {
				<-notificationSeen
			}
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "start", "method": appserver.MethodTurnStart,
		"params": map[string]any{"threadId": "thread-1", "input": []any{}},
	})

	response := readJSON(t, conn)
	responseID, err := wireID(response)
	if err != nil {
		t.Fatalf("turn/start response id: %v; message = %#v", err, response)
	}
	if text, ok := responseID.Text(); !ok || text != "start" {
		t.Fatalf("first message id = %v, want turn/start response", responseID)
	}
	notification := readJSON(t, conn)
	method, err := wireMethod(notification)
	if err != nil || method != appserver.NotificationTurnCompleted {
		t.Fatalf("second message = %#v, err = %v", notification, err)
	}
	upstream.await(t)
}

func TestRejectedTurnStartDoesNotDropConcurrentReadyNotification(t *testing.T) {
	beforeEntered := make(chan struct{})
	releaseBefore := make(chan struct{})
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: newUnusedUpstream(),
		BeforeRequest: func(method string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
			if method == appserver.MethodTurnStart {
				close(beforeEntered)
				<-releaseBefore
				return nil, nil, &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "managed turn is active"}
			}
			return params, nil, nil
		},
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			if method == appserver.MethodThreadResume {
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			}
			if method == appserver.MethodTurnStart {
				return json.RawMessage(`{"turn":{"id":"turn-1","status":"inProgress"}}`), true, nil
			}
			return nil, false, nil
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "rejected", "method": appserver.MethodTurnStart,
		"params": map[string]any{"threadId": "thread-1", "input": []any{}},
	})
	select {
	case <-beforeEntered:
	case <-time.After(proxyTestTimeout):
		t.Fatal("turn/start validation did not begin")
	}
	proxy.Notify(appserver.Notification{Method: "turn/progress", Params: json.RawMessage(`{"n":1}`)})
	close(releaseBefore)

	response := readJSON(t, conn)
	responseID, err := wireID(response)
	if err != nil || responseID.String() != "rejected" {
		t.Fatalf("rejected response id = %v, err = %v", responseID, err)
	}
	requireRPCError(t, response, appserver.ErrorCodeInvalidRequest)
	notification := readJSON(t, conn)
	method, err := wireMethod(notification)
	if err != nil || method != "turn/progress" {
		t.Fatalf("concurrent notification = %#v, err = %v", notification, err)
	}
}

func TestTurnStartRequiresSuccessfulResume(t *testing.T) {
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{Endpoint: endpoint, Upstream: newUnusedUpstream()})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "start", "method": appserver.MethodTurnStart,
		"params": map[string]any{"threadId": "thread-1", "input": []any{}},
	})
	response := readJSON(t, conn)
	requireRPCError(t, response, appserver.ErrorCodeInvalidRequest)
	if !strings.Contains(string(response["error"]), "thread/resume") {
		t.Fatalf("turn/start pre-resume error = %s", response["error"])
	}
}

func TestPipelinedTurnStartCannotOvertakeResume(t *testing.T) {
	resumeEntered := make(chan struct{})
	releaseResume := make(chan struct{})
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: newUnusedUpstream(),
		BeforeRequest: func(method string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
			if method == appserver.MethodThreadResume {
				close(resumeEntered)
				<-releaseResume
			}
			return params, nil, nil
		},
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			if method == appserver.MethodThreadResume {
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			}
			if method == appserver.MethodTurnStart {
				return json.RawMessage(`{"turn":{"id":"turn-1","status":"inProgress"}}`), true, nil
			}
			return nil, false, nil
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	writeJSON(t, conn, map[string]any{
		"id": "start", "method": appserver.MethodTurnStart,
		"params": map[string]any{"threadId": "thread-1", "input": []any{}},
	})
	select {
	case <-resumeEntered:
	case <-time.After(proxyTestTimeout):
		t.Fatal("resume validation did not begin")
	}
	close(releaseResume)
	resumeResponse := readJSON(t, conn)
	resumeID, err := wireID(resumeResponse)
	if err != nil || resumeID.String() != "resume" {
		t.Fatalf("resume response id = %v, err = %v", resumeID, err)
	}
	waitFor(t, "pipelined resume readiness", proxy.Attached)
	startResponse := readJSON(t, conn)
	startID, err := wireID(startResponse)
	if err != nil || startID.String() != "start" {
		t.Fatalf("pipelined start response id = %v, err = %v", startID, err)
	}
	if _, hasError := startResponse["error"]; hasError {
		t.Fatalf("pipelined turn/start was rejected after resume: %s", startResponse["error"])
	}
}

func TestPipelinedTurnStartsAreAdmittedInWireOrder(t *testing.T) {
	firstEntered := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirst) }) })

	var activeMu sync.Mutex
	active := false
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{
		Endpoint: endpoint,
		Upstream: newUnusedUpstream(),
		BeforeRequest: func(method string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
			if method != appserver.MethodTurnStart {
				return params, nil, nil
			}
			var request struct {
				Label string `json:"label"`
			}
			if err := json.Unmarshal(params, &request); err != nil {
				return nil, nil, &appserver.RPCError{Code: appserver.ErrorCodeInvalidParams, Message: err.Error()}
			}
			if request.Label == "first" {
				close(firstEntered)
				<-releaseFirst
				activeMu.Lock()
				active = true
				activeMu.Unlock()
				return params, nil, nil
			}
			close(secondEntered)
			activeMu.Lock()
			defer activeMu.Unlock()
			if active {
				return nil, nil, &appserver.RPCError{
					Code: appserver.ErrorCodeInvalidRequest, Message: "managed turn is active",
				}
			}
			return params, nil, nil
		},
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			switch method {
			case appserver.MethodThreadResume:
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			case appserver.MethodTurnStart:
				return json.RawMessage(`{"turn":{"id":"turn-1","status":"inProgress"}}`), true, nil
			default:
				return nil, false, nil
			}
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, conn)

	writeJSON(t, conn, map[string]any{
		"id": "first", "method": appserver.MethodTurnStart,
		"params": map[string]any{"threadId": "thread-1", "label": "first", "input": []any{}},
	})
	select {
	case <-firstEntered:
	case <-time.After(proxyTestTimeout):
		t.Fatal("first turn/start validation did not begin")
	}
	writeJSON(t, conn, map[string]any{
		"id": "second", "method": appserver.MethodTurnStart,
		"params": map[string]any{"threadId": "thread-1", "label": "second", "input": []any{}},
	})

	select {
	case <-secondEntered:
		releaseOnce.Do(func() { close(releaseFirst) })
		t.Fatal("second turn/start overtook the first request")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseFirst) })

	firstResponse := readJSON(t, conn)
	firstID, err := wireID(firstResponse)
	if err != nil || firstID.String() != "first" {
		t.Fatalf("first turn/start response id = %v, err = %v", firstID, err)
	}
	if _, hasError := firstResponse["error"]; hasError {
		t.Fatalf("first turn/start was rejected: %s", firstResponse["error"])
	}
	secondResponse := readJSON(t, conn)
	secondID, err := wireID(secondResponse)
	if err != nil || secondID.String() != "second" {
		t.Fatalf("second turn/start response id = %v, err = %v", secondID, err)
	}
	requireRPCError(t, secondResponse, appserver.ErrorCodeInvalidRequest)
}

func TestReverseRequestsForwardResultsAndErrors(t *testing.T) {
	trigger := make(chan struct{})
	responses := make(chan map[string]json.RawMessage, 2)
	type forwardOutcome struct {
		handled bool
		err     error
	}
	forwarded := make(chan forwardOutcome, 2)
	var proxy *Proxy
	upstream := startFakeUpstream(t, appserver.Options{
		OnReverseRequest: func(request *appserver.ReverseRequest) {
			handled, err := proxy.ForwardReverse(context.Background(), request)
			forwarded <- forwardOutcome{handled: handled, err: err}
		},
	}, func(ctx context.Context, conn *websocket.Conn) error {
		resume, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		method, err := wireMethod(resume)
		if err != nil || method != appserver.MethodThreadResume {
			return fmt.Errorf("first upstream method = %q, err = %v", method, err)
		}
		resumeID, err := wireID(resume)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": resumeID, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}},
		}); err != nil {
			return err
		}

		requests := []map[string]any{
			{"id": "upstream-result", "method": "approval/result", "params": map[string]int{"sequence": 1}},
			{"id": -19, "method": "approval/error", "params": map[string]int{"sequence": 2}},
		}
		for _, request := range requests {
			select {
			case <-trigger:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := writeObject(ctx, conn, request); err != nil {
				return err
			}
			response, err := readObject(ctx, conn)
			if err != nil {
				return err
			}
			responses <- response
		}
		return nil
	})
	endpoint, _ := proxyEndpoint(t)
	proxy = listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	conn := mustDialProxy(t, endpoint, "/")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, conn)
	waitFor(t, "proxy readiness", proxy.Attached)

	trigger <- struct{}{}
	resultRequest := readJSON(t, conn)
	resultID, err := wireID(resultRequest)
	if err != nil {
		t.Fatal(err)
	}
	if text, ok := resultID.Text(); !ok || !strings.HasPrefix(text, "intercom-") {
		t.Fatalf("forwarded result request id = %v, want proxy-owned string", resultID)
	}
	method, err := wireMethod(resultRequest)
	if err != nil || method != "approval/result" {
		t.Fatalf("forwarded result method = %q, err = %v", method, err)
	}
	if string(resultRequest["params"]) != `{"sequence":1}` {
		t.Fatalf("forwarded result params = %s", resultRequest["params"])
	}
	writeJSON(t, conn, map[string]any{
		"id": resultID, "result": map[string]string{"decision": "accept"},
	})
	upstreamResult := <-responses
	upstreamResultID, err := wireID(upstreamResult)
	if err != nil || upstreamResultID.String() != "upstream-result" {
		t.Fatalf("upstream result response id = %v, err = %v", upstreamResultID, err)
	}
	if string(upstreamResult["result"]) != `{"decision":"accept"}` {
		t.Fatalf("upstream result = %s", upstreamResult["result"])
	}
	select {
	case outcome := <-forwarded:
		if !outcome.handled || outcome.err != nil {
			t.Fatalf("result ForwardReverse = %+v", outcome)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("result ForwardReverse did not finish")
	}

	trigger <- struct{}{}
	errorRequest := readJSON(t, conn)
	errorID, err := wireID(errorRequest)
	if err != nil {
		t.Fatal(err)
	}
	if errorID == resultID {
		t.Fatalf("reverse request IDs were reused: %v", errorID)
	}
	method, err = wireMethod(errorRequest)
	if err != nil || method != "approval/error" {
		t.Fatalf("forwarded error method = %q, err = %v", method, err)
	}
	writeJSON(t, conn, map[string]any{
		"id":    errorID,
		"error": map[string]any{"code": appserver.ErrorCodeInvalidParams, "message": "declined"},
	})
	upstreamError := <-responses
	upstreamErrorID, err := wireID(upstreamError)
	if err != nil || upstreamErrorID.String() != "-19" {
		t.Fatalf("upstream error response id = %v, err = %v", upstreamErrorID, err)
	}
	rpcErr := requireRPCError(t, upstreamError, appserver.ErrorCodeInvalidParams)
	if rpcErr.Message != "declined" {
		t.Fatalf("upstream RPC error = %+v", rpcErr)
	}
	select {
	case outcome := <-forwarded:
		if !outcome.handled || outcome.err != nil {
			t.Fatalf("error ForwardReverse = %+v", outcome)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("error ForwardReverse did not finish")
	}
	upstream.await(t)
}

func TestReverseResponseAcceptedAtDeadlineUsesFreshUpstreamWriteContext(t *testing.T) {
	trigger := make(chan struct{})
	reverseCtx, cancelReverse := context.WithCancel(context.Background())
	defer cancelReverse()
	type outcome struct {
		handled bool
		err     error
	}
	outcomes := make(chan outcome, 1)
	var proxy *Proxy
	upstream := startFakeUpstream(t, appserver.Options{
		OnReverseRequest: func(request *appserver.ReverseRequest) {
			handled, err := proxy.ForwardReverse(reverseCtx, request)
			outcomes <- outcome{handled: handled, err: err}
		},
	}, func(ctx context.Context, conn *websocket.Conn) error {
		resume, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		resumeID, err := wireID(resume)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": resumeID, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}},
		}); err != nil {
			return err
		}
		select {
		case <-trigger:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": "boundary", "method": "approval/boundary", "params": map[string]bool{"ask": true},
		}); err != nil {
			return err
		}
		response, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := wireID(response)
		if err != nil || id.String() != "boundary" {
			return fmt.Errorf("boundary response id = %v: %w", id, err)
		}
		if string(response["result"]) != `{"decision":"accept"}` {
			return fmt.Errorf("boundary response = %s", response["result"])
		}
		return nil
	})
	endpoint, _ := proxyEndpoint(t)
	proxy = listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, conn)
	waitFor(t, "proxy readiness", proxy.Attached)

	trigger <- struct{}{}
	reverse := readJSON(t, conn)
	reverseID, err := wireID(reverse)
	if err != nil {
		t.Fatal(err)
	}
	proxy.mu.Lock()
	session := proxy.session
	proxy.mu.Unlock()
	if session == nil {
		t.Fatal("ready proxy has no session")
	}
	session.mu.Lock()
	waiter := session.pending[reverseID]
	if waiter != nil {
		delete(session.pending, reverseID)
	}
	session.mu.Unlock()
	if waiter == nil {
		t.Fatalf("no pending waiter for reverse id %s", reverseID)
	}
	// Model the reader's ordering at the deadline boundary: it has accepted
	// and claimed the TUI response, but has not delivered it to the waiter.
	cancelReverse()
	time.Sleep(time.Millisecond)
	waiter <- reverseResponse{result: json.RawMessage(`{"decision":"accept"}`)}

	select {
	case got := <-outcomes:
		if !got.handled || got.err != nil {
			t.Fatalf("ForwardReverse() = %+v", got)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("boundary reverse request did not finish")
	}
	upstream.await(t)
}

func TestLateReverseResponseAfterTimeoutDoesNotDisconnectTUI(t *testing.T) {
	trigger := make(chan struct{})
	type outcome struct {
		handled  bool
		forward  error
		fallback error
	}
	outcomes := make(chan outcome, 1)
	var proxy *Proxy
	upstream := startFakeUpstream(t, appserver.Options{
		OnReverseRequest: func(request *appserver.ReverseRequest) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			handled, forwardErr := proxy.ForwardReverse(ctx, request)
			cancel()
			var fallbackErr error
			if !handled {
				fallbackErr = request.Respond(context.Background(), map[string]string{"decision": "fallback"})
			}
			outcomes <- outcome{handled: handled, forward: forwardErr, fallback: fallbackErr}
		},
	}, func(ctx context.Context, conn *websocket.Conn) error {
		resume, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		resumeID, err := wireID(resume)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": resumeID, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}},
		}); err != nil {
			return err
		}
		select {
		case <-trigger:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": "late", "method": "approval/late", "params": map[string]bool{"ask": true},
		}); err != nil {
			return err
		}
		fallback, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		fallbackID, err := wireID(fallback)
		if err != nil || fallbackID.String() != "late" || string(fallback["result"]) != `{"decision":"fallback"}` {
			return fmt.Errorf("fallback response = %#v, id %v, error %v", fallback, fallbackID, err)
		}
		probe, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		probeID, err := wireID(probe)
		if err != nil {
			return err
		}
		method, err := wireMethod(probe)
		if err != nil || method != "model/list" {
			return fmt.Errorf("probe method = %q: %w", method, err)
		}
		return writeObject(ctx, conn, map[string]any{
			"id": probeID, "result": map[string]bool{"ok": true},
		})
	})
	endpoint, _ := proxyEndpoint(t)
	proxy = listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, conn)
	waitFor(t, "proxy readiness", proxy.Attached)

	trigger <- struct{}{}
	reverse := readJSON(t, conn)
	reverseID, err := wireID(reverse)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-outcomes:
		if got.handled || !errors.Is(got.forward, context.DeadlineExceeded) || got.fallback != nil {
			t.Fatalf("timed-out ForwardReverse() = %+v", got)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("reverse request did not time out")
	}

	writeJSON(t, conn, map[string]any{
		"id": reverseID, "result": map[string]string{"decision": "late"},
	})
	writeJSON(t, conn, map[string]any{
		"id": "probe", "method": "model/list", "params": map[string]any{},
	})
	probe := readJSON(t, conn)
	probeID, err := wireID(probe)
	if err != nil || probeID.String() != "probe" || string(probe["result"]) != `{"ok":true}` {
		t.Fatalf("probe response after late reverse result = %#v, id %v, error %v", probe, probeID, err)
	}
	if !proxy.Attached() {
		t.Fatal("late reverse response disconnected the TUI")
	}
	upstream.await(t)
}

func TestThreadUnsubscribeSettlesPendingReverseAndPermitsReconnect(t *testing.T) {
	trigger := make(chan struct{})
	type forwardOutcome struct {
		handled  bool
		forward  error
		fallback error
	}
	outcomes := make(chan forwardOutcome, 1)
	upstreamResponse := make(chan map[string]json.RawMessage, 1)
	var proxy *Proxy
	upstream := startFakeUpstream(t, appserver.Options{
		OnReverseRequest: func(request *appserver.ReverseRequest) {
			handled, forwardErr := proxy.ForwardReverse(context.Background(), request)
			var fallbackErr error
			if !handled {
				fallbackErr = request.Respond(context.Background(), map[string]string{"decision": "fallback"})
			}
			outcomes <- forwardOutcome{handled: handled, forward: forwardErr, fallback: fallbackErr}
		},
	}, func(ctx context.Context, conn *websocket.Conn) error {
		resume, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		resumeID, err := wireID(resume)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": resumeID, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}},
		}); err != nil {
			return err
		}
		select {
		case <-trigger:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": "pending-approval", "method": "approval/pending", "params": map[string]bool{"ask": true},
		}); err != nil {
			return err
		}
		response, err := readObject(ctx, conn)
		if err == nil {
			upstreamResponse <- response
		}
		return err
	})
	endpoint, _ := proxyEndpoint(t)
	proxy = listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, conn)
	waitFor(t, "proxy readiness", proxy.Attached)

	trigger <- struct{}{}
	reverse := readJSON(t, conn)
	method, err := wireMethod(reverse)
	if err != nil || method != "approval/pending" {
		t.Fatalf("pending reverse request method = %q, err = %v", method, err)
	}
	writeJSON(t, conn, map[string]any{
		"id": "unsubscribe", "method": appserver.MethodThreadUnsubscribe,
		"params": map[string]string{"threadId": "thread-1"},
	})
	unsubscribed := readJSON(t, conn)
	unsubscribedID, err := wireID(unsubscribed)
	if err != nil || unsubscribedID.String() != "unsubscribe" {
		t.Fatalf("unsubscribe response id = %v, err = %v", unsubscribedID, err)
	}
	if string(unsubscribed["result"]) != `{"status":"unsubscribed"}` {
		t.Fatalf("unsubscribe result = %s", unsubscribed["result"])
	}
	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	_, _, err = conn.Read(ctx)
	cancel()
	if status := websocket.CloseStatus(err); status != websocket.StatusNormalClosure {
		t.Fatalf("unsubscribe close = %v (status %v), want normal closure", err, status)
	}
	waitFor(t, "proxy to become unready", func() bool { return !proxy.Attached() })

	select {
	case outcome := <-outcomes:
		if outcome.handled || !errors.Is(outcome.forward, ErrNoAttachedTUI) || outcome.fallback != nil {
			t.Fatalf("pending reverse settlement = %+v", outcome)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("pending reverse request was not settled")
	}
	select {
	case response := <-upstreamResponse:
		id, err := wireID(response)
		if err != nil || id.String() != "pending-approval" {
			t.Fatalf("fallback response id = %v, err = %v", id, err)
		}
		if string(response["result"]) != `{"decision":"fallback"}` {
			t.Fatalf("fallback result = %s", response["result"])
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("fallback response did not reach upstream")
	}
	upstream.await(t)

	waitFor(t, "unsubscribed session removal", func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		return proxy.session == nil
	})
	reconnected := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, reconnected)
}

func TestNoReadyTUIAllowsReverseFallback(t *testing.T) {
	trigger := make(chan struct{})
	upstreamResponse := make(chan map[string]json.RawMessage, 1)
	type outcome struct {
		handled  bool
		forward  error
		fallback error
	}
	outcomes := make(chan outcome, 1)
	var proxy *Proxy
	upstream := startFakeUpstream(t, appserver.Options{
		OnReverseRequest: func(request *appserver.ReverseRequest) {
			handled, forwardErr := proxy.ForwardReverse(context.Background(), request)
			var fallbackErr error
			if !handled {
				fallbackErr = request.Respond(context.Background(), map[string]string{"decision": "headless-fallback"})
			}
			outcomes <- outcome{handled: handled, forward: forwardErr, fallback: fallbackErr}
		},
	}, func(ctx context.Context, conn *websocket.Conn) error {
		select {
		case <-trigger:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": "headless-request", "method": "approval/headless",
			"params": map[string]bool{"ask": true},
		}); err != nil {
			return err
		}
		response, err := readObject(ctx, conn)
		if err == nil {
			upstreamResponse <- response
		}
		return err
	})
	endpoint, _ := proxyEndpoint(t)
	proxy = listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream})
	trigger <- struct{}{}

	select {
	case got := <-outcomes:
		if got.handled || got.forward != nil || got.fallback != nil {
			t.Fatalf("headless ForwardReverse/fallback = %+v", got)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("headless reverse handler did not finish")
	}
	select {
	case response := <-upstreamResponse:
		id, err := wireID(response)
		if err != nil || id.String() != "headless-request" {
			t.Fatalf("fallback response id = %v, err = %v", id, err)
		}
		if string(response["result"]) != `{"decision":"headless-fallback"}` {
			t.Fatalf("fallback result = %s", response["result"])
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("headless fallback response did not reach upstream")
	}
	upstream.await(t)
}

func TestSecondClientIsRejectedThenDisconnectCanReconnect(t *testing.T) {
	methods := make(chan string, 3)
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		for range 3 {
			request, err := readObject(ctx, conn)
			if err != nil {
				return err
			}
			method, err := wireMethod(request)
			if err != nil {
				return err
			}
			methods <- method
			id, err := wireID(request)
			if err != nil {
				return err
			}
			if err := writeObject(ctx, conn, map[string]any{
				"id": id, "result": map[string]any{"thread": map[string]string{"id": "thread-1"}},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	attached := make(chan struct{}, 2)
	detached := make(chan struct{}, 2)
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: upstream,
		OnAttach: func() { attached <- struct{}{} },
		OnDetach: func() { detached <- struct{}{} },
	})
	first := mustDialProxy(t, endpoint, "/")
	initializeTUI(t, first)
	if proxy.Attached() {
		t.Fatal("proxy was ready immediately after initialize")
	}

	writeJSON(t, first, map[string]any{
		"id": "start", "method": appserver.MethodThreadStart,
		"params": map[string]any{},
	})
	_ = readJSON(t, first)
	if proxy.Attached() {
		t.Fatal("thread/start incorrectly made proxy ready")
	}

	writeJSON(t, first, map[string]any{
		"id": "resume-one", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, first)
	waitFor(t, "first attach callback", func() bool {
		select {
		case <-attached:
			return true
		default:
			return false
		}
	})
	if !proxy.Attached() {
		t.Fatal("proxy was not ready after successful thread/resume")
	}

	ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
	second, response, err := dialProxy(ctx, endpoint, "/rpc")
	cancel()
	if second != nil {
		second.CloseNow()
		t.Fatal("second client unexpectedly connected")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		status := "<nil>"
		if response != nil {
			status = response.Status
		}
		t.Fatalf("second client upgrade = (%s, %v), want HTTP 409", status, err)
	}
	closeHTTPResponse(response)

	first.CloseNow()
	waitFor(t, "ready TUI detach", func() bool {
		select {
		case <-detached:
			return true
		default:
			return false
		}
	})
	if proxy.Attached() {
		t.Fatal("proxy remained ready after disconnect")
	}

	var reconnected *websocket.Conn
	waitFor(t, "proxy to accept reconnect", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		conn, response, err := dialProxy(ctx, endpoint, "/")
		if response != nil {
			closeHTTPResponse(response)
		}
		if err != nil {
			return false
		}
		reconnected = conn
		return true
	})
	t.Cleanup(func() { _ = reconnected.CloseNow() })
	initializeTUI(t, reconnected)
	writeJSON(t, reconnected, map[string]any{
		"id": "resume-two", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	_ = readJSON(t, reconnected)
	waitFor(t, "second attach callback", func() bool {
		select {
		case <-attached:
			return true
		default:
			return false
		}
	})
	if !proxy.Attached() {
		t.Fatal("proxy did not become ready after reconnect")
	}

	for index, want := range []string{appserver.MethodThreadStart, appserver.MethodThreadResume, appserver.MethodThreadResume} {
		select {
		case got := <-methods:
			if got != want {
				t.Fatalf("upstream method %d = %q, want %q", index, got, want)
			}
		case <-time.After(proxyTestTimeout):
			t.Fatalf("upstream method %d did not arrive", index)
		}
	}
	upstream.await(t)
}

func TestBinaryAndMalformedFramesCloseOnlyTheirSession(t *testing.T) {
	tests := []struct {
		name       string
		typ        websocket.MessageType
		payload    string
		initialize bool
		wantStatus websocket.StatusCode
	}{
		{name: "binary", typ: websocket.MessageBinary, payload: `{"method":"initialized"}`, wantStatus: websocket.StatusUnsupportedData},
		{name: "invalid JSON", typ: websocket.MessageText, payload: `{`, wantStatus: websocket.StatusPolicyViolation},
		{name: "malformed envelope", typ: websocket.MessageText, payload: `{"id":1,"result":{},"error":{"code":1,"message":"both"}}`, wantStatus: websocket.StatusPolicyViolation},
		{name: "unsupported client notification", typ: websocket.MessageText, payload: `{"method":"future/event","params":{}}`, initialize: true, wantStatus: websocket.StatusPolicyViolation},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, _ := proxyEndpoint(t)
			proxy := listenTestProxy(t, Options{Endpoint: endpoint, Upstream: newUnusedUpstream()})
			conn := mustDialProxy(t, endpoint, "/")
			if test.initialize {
				initializeTUI(t, conn)
			}
			ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
			if err := conn.Write(ctx, test.typ, []byte(test.payload)); err != nil {
				cancel()
				t.Fatalf("write invalid frame: %v", err)
			}
			_, _, err := conn.Read(ctx)
			cancel()
			if status := websocket.CloseStatus(err); status != test.wantStatus {
				t.Fatalf("close error = %v (status %v), want status %v", err, status, test.wantStatus)
			}
			waitFor(t, "invalid session removal", func() bool {
				proxy.mu.Lock()
				defer proxy.mu.Unlock()
				return proxy.session == nil
			})
			replacement := mustDialProxy(t, endpoint, "/rpc")
			initializeTUI(t, replacement)
		})
	}
}

func TestPreReadyHandshakeTimeoutReleasesSoleSessionSlot(t *testing.T) {
	for _, initialize := range []bool{false, true} {
		name := "before-initialize"
		if initialize {
			name = "before-resume"
		}
		t.Run(name, func(t *testing.T) {
			endpoint, _ := proxyEndpoint(t)
			proxy := listenTestProxy(t, Options{
				Endpoint: endpoint, Upstream: newUnusedUpstream(), HandshakeTimeout: 50 * time.Millisecond,
			})
			conn := mustDialProxy(t, endpoint, "/rpc")
			if initialize {
				initializeTUI(t, conn)
			}
			ctx, cancel := context.WithTimeout(context.Background(), proxyTestTimeout)
			_, _, err := conn.Read(ctx)
			cancel()
			if status := websocket.CloseStatus(err); status != websocket.StatusPolicyViolation {
				t.Fatalf("handshake timeout close = %v (status %v)", err, status)
			}
			waitFor(t, "timed-out pre-ready session removal", func() bool {
				proxy.mu.Lock()
				defer proxy.mu.Unlock()
				return proxy.session == nil
			})
			replacement := mustDialProxy(t, endpoint, "/rpc")
			initializeTUI(t, replacement)
		})
	}
}

func TestConcurrentRequestLimitReturnsOverloadedWithoutBlockingReader(t *testing.T) {
	firstArrived := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirst) }) })
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		close(firstArrived)
		select {
		case <-releaseFirst:
		case <-ctx.Done():
			return ctx.Err()
		}
		id, err := wireID(request)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{"id": id, "result": map[string]bool{"ok": true}})
	})
	endpoint, _ := proxyEndpoint(t)
	listenTestProxy(t, Options{Endpoint: endpoint, Upstream: upstream, MaxConcurrentCalls: 1})
	conn := mustDialProxy(t, endpoint, "/")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{"id": "blocked", "method": "slow/request", "params": map[string]any{}})
	select {
	case <-firstArrived:
	case <-time.After(proxyTestTimeout):
		t.Fatal("first request did not reach upstream")
	}
	writeJSON(t, conn, map[string]any{"id": "overflow", "method": "fast/request", "params": map[string]any{}})
	overflow := readJSON(t, conn)
	overflowID, err := wireID(overflow)
	if err != nil || overflowID.String() != "overflow" {
		t.Fatalf("overflow response id = %v, err = %v", overflowID, err)
	}
	rpcErr := requireRPCError(t, overflow, serverOverloadedCode)
	if !strings.Contains(strings.ToLower(rpcErr.Message), "overload") {
		t.Fatalf("overflow error message = %q", rpcErr.Message)
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	completed := readJSON(t, conn)
	completedID, err := wireID(completed)
	if err != nil || completedID.String() != "blocked" {
		t.Fatalf("completed response id = %v, err = %v", completedID, err)
	}
	upstream.await(t)
}

func TestForwardedRequestTimeoutReturnsErrorAndReleasesHandler(t *testing.T) {
	requestArrived := make(chan struct{})
	timedOut := make(chan struct{})
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		first, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		firstID, err := wireID(first)
		if err != nil {
			return err
		}
		close(requestArrived)
		select {
		case <-timedOut:
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := writeObject(ctx, conn, map[string]any{"id": firstID, "result": map[string]bool{"late": true}}); err != nil {
			return err
		}
		second, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		secondID, err := wireID(second)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{"id": secondID, "result": map[string]bool{"ok": true}})
	})
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: upstream, RequestTimeout: 30 * time.Millisecond,
		AfterRequest: func(method string, _ any, _ json.RawMessage, err error) {
			if method == "config/read" && errors.Is(err, context.DeadlineExceeded) {
				close(timedOut)
			}
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{"id": "slow", "method": "config/read", "params": map[string]any{}})
	select {
	case <-requestArrived:
	case <-time.After(proxyTestTimeout):
		t.Fatal("forwarded request did not reach upstream")
	}
	response := readJSON(t, conn)
	if id, err := wireID(response); err != nil || id.String() != "slow" {
		t.Fatalf("timeout response id = %v, err = %v", id, err)
	}
	rpcErr := requireRPCError(t, response, appserver.ErrorCodeInternal)
	if !strings.Contains(rpcErr.Message, context.DeadlineExceeded.Error()) {
		t.Fatalf("timeout error = %+v", rpcErr)
	}
	waitFor(t, "timed-out request handler release", func() bool { return len(proxy.handlers) == 0 })
	writeJSON(t, conn, map[string]any{"id": "after-timeout", "method": "config/read", "params": map[string]any{}})
	second := readJSON(t, conn)
	if id, err := wireID(second); err != nil || id.String() != "after-timeout" || len(second["error"]) != 0 {
		t.Fatalf("request after late response = %#v, id %v, err %v", second, id, err)
	}
	upstream.await(t)
}

func TestRequestLimitIncludesDetachedSessionHandlers(t *testing.T) {
	requestArrived := make(chan struct{})
	release := make(chan struct{})
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		close(requestArrived)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		id, err := wireID(request)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{"id": id, "result": map[string]bool{"ok": true}})
	})
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: upstream, MaxConcurrentCalls: 1,
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			if method == appserver.MethodThreadResume {
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			}
			return nil, false, nil
		},
	})
	first := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, first)
	writeJSON(t, first, map[string]any{"id": "resume-1", "method": appserver.MethodThreadResume, "params": map[string]string{"threadId": "thread-1"}})
	_ = readJSON(t, first)
	writeJSON(t, first, map[string]any{"id": "blocked", "method": "config/read", "params": map[string]any{}})
	select {
	case <-requestArrived:
	case <-time.After(proxyTestTimeout):
		t.Fatal("detached request did not reach upstream")
	}
	first.CloseNow()
	waitFor(t, "first session removal", func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		return proxy.session == nil
	})

	second := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, second)
	writeJSON(t, second, map[string]any{"id": "resume-2", "method": appserver.MethodThreadResume, "params": map[string]string{"threadId": "thread-1"}})
	_ = readJSON(t, second)
	writeJSON(t, second, map[string]any{"id": "overflow", "method": "config/read", "params": map[string]any{}})
	response := readJSON(t, second)
	requireRPCError(t, response, serverOverloadedCode)

	close(release)
	upstream.await(t)
	waitFor(t, "detached request handler release", func() bool { return len(proxy.handlers) == 0 })
}

func TestCloseWaitsForSessionCleanupAndForwardedHandlers(t *testing.T) {
	requestArrived := make(chan struct{})
	afterEntered := make(chan struct{})
	upstream := startFakeUpstream(t, appserver.Options{}, func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := readObject(ctx, conn); err != nil {
			return err
		}
		close(requestArrived)
		select {
		case <-afterEntered:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	allowAfter := make(chan struct{})
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: upstream,
		AfterRequest: func(method string, _ any, _ json.RawMessage, _ error) {
			if method == "config/read" {
				close(afterEntered)
				<-allowAfter
			}
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{"id": "blocked", "method": "config/read", "params": map[string]any{}})
	select {
	case <-requestArrived:
	case <-time.After(proxyTestTimeout):
		t.Fatal("forwarded request did not reach upstream")
	}

	closed := make(chan error, 1)
	go func() { closed <- proxy.Close() }()
	select {
	case <-afterEntered:
	case <-time.After(proxyTestTimeout):
		t.Fatal("forwarded handler did not enter cleanup")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before handler cleanup: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowAfter)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(proxyTestTimeout):
		t.Fatal("Close did not join forwarded handler")
	}
	upstream.await(t)
}

func TestDownstreamWriteTimeoutReleasesSessionAndHandler(t *testing.T) {
	largeResult := json.RawMessage(`{"data":"` + strings.Repeat("x", 8<<20) + `"}`)
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: newUnusedUpstream(),
		MaxMessageSize: 16 << 20, WriteTimeout: 50 * time.Millisecond,
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			switch method {
			case appserver.MethodThreadResume:
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			case "config/read":
				return largeResult, true, nil
			default:
				return nil, false, nil
			}
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{"id": "resume", "method": appserver.MethodThreadResume, "params": map[string]string{"threadId": "thread-1"}})
	_ = readJSON(t, conn)
	writeJSON(t, conn, map[string]any{"id": "large", "method": "config/read", "params": map[string]any{}})

	waitFor(t, "write-timed-out session removal", func() bool {
		proxy.mu.Lock()
		defer proxy.mu.Unlock()
		return proxy.session == nil
	})
	waitFor(t, "write-timed-out handler release", func() bool { return len(proxy.handlers) == 0 })
}

func TestNotificationQueueOverflowDoesNotWaitForWebSocketClose(t *testing.T) {
	endpoint, _ := proxyEndpoint(t)
	proxy := listenTestProxy(t, Options{
		Endpoint: endpoint, Upstream: newUnusedUpstream(),
		MaxMessageSize: 2 << 20, WriteQueueSize: 1,
		LocalRequest: func(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
			if method == appserver.MethodThreadResume {
				return json.RawMessage(`{"thread":{"id":"thread-1"}}`), true, nil
			}
			return nil, false, nil
		},
	})
	conn := mustDialProxy(t, endpoint, "/rpc")
	initializeTUI(t, conn)
	writeJSON(t, conn, map[string]any{
		"id": "resume", "method": appserver.MethodThreadResume,
		"params": map[string]string{"threadId": "thread-1"},
	})
	resume := readJSON(t, conn)
	if _, exists := resume["error"]; exists {
		t.Fatalf("local resume error = %s", resume["error"])
	}
	waitFor(t, "notification overflow session readiness", proxy.Attached)

	proxy.mu.Lock()
	s := proxy.session
	proxy.mu.Unlock()
	if s == nil {
		t.Fatal("initialized session was not installed")
	}
	payload := json.RawMessage(`{"blob":"` + strings.Repeat("x", 1<<20) + `"}`)
	proxy.Notify(appserver.Notification{Method: "large/one", Params: payload})
	waitFor(t, "writer to consume first large notification", func() bool {
		return len(s.writes) == 0
	})
	proxy.Notify(appserver.Notification{Method: "large/two", Params: payload})
	waitFor(t, "bounded notification queue to fill", func() bool {
		return len(s.writes) == 1
	})

	started := time.Now()
	proxy.Notify(appserver.Notification{Method: "large/overflow", Params: payload})
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("notification overflow blocked for %v waiting for websocket close", elapsed)
	}
	select {
	case <-s.done:
	case <-time.After(proxyTestTimeout):
		t.Fatal("overflowed session did not close")
	}
}

func TestSessionWriteQueueAndMessageSizeAreBounded(t *testing.T) {
	s := &session{
		proxy:  &Proxy{opts: Options{MaxMessageSize: 4}},
		writes: make(chan queuedWrite, 1),
		done:   make(chan struct{}),
	}
	if !s.tryWrite([]byte("one")) {
		t.Fatal("first bounded write was rejected")
	}
	if s.tryWrite([]byte("two")) {
		t.Fatal("write was accepted after bounded queue filled")
	}
	<-s.writes
	if s.tryWrite([]byte("12345")) {
		t.Fatal("oversized write was accepted")
	}
	s.writes <- queuedWrite{data: []byte("one")}
	close(s.done)
	if s.tryWrite([]byte("one")) {
		t.Fatal("write was accepted after session closed")
	}
}
