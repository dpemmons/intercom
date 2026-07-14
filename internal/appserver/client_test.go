package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const testTimeout = 5 * time.Second

type unixTestServer struct {
	endpoint string
	done     <-chan error
	server   *http.Server
	listener net.Listener
}

func startUnixTestServer(t *testing.T, handler func(context.Context, *websocket.Conn) error) *unixTestServer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-server.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	done := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			done <- fmt.Errorf("unexpected websocket path %q", r.URL.Path)
			http.Error(w, "bad path", http.StatusNotFound)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			done <- fmt.Errorf("accept websocket: %w", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		defer conn.CloseNow()
		done <- handler(ctx, conn)
	})
	server := &http.Server{Handler: mux}
	go func() {
		_ = server.Serve(listener)
	}()
	testServer := &unixTestServer{
		endpoint: "unix://" + path,
		done:     done,
		server:   server,
		listener: listener,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = listener.Close()
	})
	return testServer
}

func (s *unixTestServer) await(t *testing.T) {
	t.Helper()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("test websocket server: %v", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for test websocket server")
	}
}

func readObject(ctx context.Context, conn *websocket.Conn) (map[string]json.RawMessage, error) {
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		return nil, fmt.Errorf("got websocket message type %v, want text", typ)
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

func objectID(object map[string]json.RawMessage) (RequestID, error) {
	var id RequestID
	err := json.Unmarshal(object["id"], &id)
	return id, err
}

func objectMethod(object map[string]json.RawMessage) (string, error) {
	var method string
	err := json.Unmarshal(object["method"], &method)
	return method, err
}

func TestDialUnixRejectedUpgradeClosesPrivateTransport(t *testing.T) {
	const attempts = 12
	path := filepath.Join(t.TempDir(), "rejected.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan struct{}, attempts)
	closed := make(chan struct{}, attempts)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusForbidden)
		}),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				accepted <- struct{}{}
			case http.StateClosed:
				closed <- struct{}{}
			}
		},
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
	})

	endpoint := "unix://" + path
	for range attempts {
		client, err := DialUnix(context.Background(), endpoint, Options{})
		if client != nil || err == nil || !strings.Contains(err.Error(), "HTTP 403") {
			t.Fatalf("DialUnix rejected upgrade = (%v, %v), want HTTP 403 error", client, err)
		}
	}
	for range attempts {
		select {
		case <-accepted:
		case <-time.After(testTimeout):
			t.Fatal("rejected upgrade was not accepted by test server")
		}
	}
	// StateClosed also means net/http's per-connection serving goroutine has
	// exited. Without closing the one-shot transport's idle pool, every
	// rejected upgrade remains idle here indefinitely.
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for index := 0; index < attempts; index++ {
		select {
		case <-closed:
		case <-deadline.C:
			t.Fatalf("only %d/%d rejected-upgrade connections closed", index, attempts)
		}
	}
}

func TestDialUnixUpgradeAndRequestResponse(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		if _, present := request["jsonrpc"]; present {
			return errors.New("app-server messages must omit jsonrpc")
		}
		method, err := objectMethod(request)
		if err != nil {
			return err
		}
		if method != "test/echo" {
			return fmt.Errorf("method = %q", method)
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{
			"id": id, "result": map[string]any{"value": "ok"},
		})
	})

	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatalf("DialUnix: %v", err)
	}
	defer client.Close()
	var response struct {
		Value string `json:"value"`
	}
	if err := client.Call(context.Background(), "test/echo", map[string]string{"value": "hello"}, &response); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if response.Value != "ok" {
		t.Fatalf("response value = %q", response.Value)
	}
	server.await(t)
}

func TestInitializeAndInitialized(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		method, err := objectMethod(request)
		if err != nil {
			return err
		}
		if method != MethodInitialize {
			return fmt.Errorf("method = %q", method)
		}
		var params InitializeParams
		if err := json.Unmarshal(request["params"], &params); err != nil {
			return err
		}
		if params.Capabilities == nil || !params.Capabilities.ExperimentalAPI {
			return fmt.Errorf("experimental capability missing: %+v", params)
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": id, "result": map[string]any{
				"userAgent": "codex_cli_rs/0.144.1", "codexHome": "/tmp/codex-home",
				"platformFamily": "unix", "platformOs": "linux",
			},
		}); err != nil {
			return err
		}
		initialized, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		method, err = objectMethod(initialized)
		if err != nil {
			return err
		}
		if method != MethodInitialized {
			return fmt.Errorf("notification method = %q", method)
		}
		if _, hasID := initialized["id"]; hasID {
			return errors.New("initialized notification unexpectedly had id")
		}
		return nil
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Initialize(context.Background(), InitializeParams{
		ClientInfo:   ClientInfo{Name: "intercom", Version: "test"},
		Capabilities: &InitializeCapabilities{ExperimentalAPI: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.UserAgent != "codex_cli_rs/0.144.1" || response.CodexHome != "/tmp/codex-home" {
		t.Fatalf("initialize response = %+v", response)
	}
	if err := client.Initialized(context.Background()); err != nil {
		t.Fatal(err)
	}
	server.await(t)
}

func TestRPCErrorResponse(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{
			"id": id, "error": map[string]any{"code": ErrorCodeInvalidParams, "message": "bad params"},
		})
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Call(context.Background(), "test/error", struct{}{}, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != ErrorCodeInvalidParams || rpcErr.Message != "bad params" {
		t.Fatalf("Call error = %#v", err)
	}
	server.await(t)
}

func TestStartTurnKeepsCorrelationAfterWriteContextCancellation(t *testing.T) {
	release := make(chan struct{})
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		<-release
		return writeObject(ctx, conn, map[string]any{
			"id":     id,
			"result": map[string]any{"turn": map[string]any{"id": "turn-1", "status": TurnStatusInProgress}},
		})
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	writeCtx, cancelWrite := context.WithCancel(context.Background())
	await, err := client.StartTurn(writeCtx, TurnStartParams{ThreadID: "thread-1", Input: []UserInput{TextInput("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	cancelWrite()
	close(release)
	response, err := await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if response.Turn.ID != "turn-1" || response.Turn.Status != TurnStatusInProgress {
		t.Fatalf("turn/start response = %#v", response)
	}
	server.await(t)
}

func TestConcurrentCallsCorrelateOutOfOrderResponses(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		requests := make(map[string]RequestID)
		for range 2 {
			request, err := readObject(ctx, conn)
			if err != nil {
				return err
			}
			method, err := objectMethod(request)
			if err != nil {
				return err
			}
			id, err := objectID(request)
			if err != nil {
				return err
			}
			requests[method] = id
		}
		for _, method := range []string{"second", "first"} {
			if err := writeObject(ctx, conn, map[string]any{
				"id": requests[method], "result": map[string]string{"method": method},
			}); err != nil {
				return err
			}
		}
		return nil
	})

	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	type outcome struct {
		method string
		err    error
	}
	outcomes := make(chan outcome, 2)
	for _, method := range []string{"first", "second"} {
		go func(method string) {
			var response struct {
				Method string `json:"method"`
			}
			err := client.Call(context.Background(), method, struct{}{}, &response)
			if err == nil && response.Method != method {
				err = fmt.Errorf("response method = %q, want %q", response.Method, method)
			}
			outcomes <- outcome{method: method, err: err}
		}(method)
	}
	for range 2 {
		select {
		case outcome := <-outcomes:
			if outcome.err != nil {
				t.Fatalf("%s call: %v", outcome.method, outcome.err)
			}
		case <-time.After(testTimeout):
			t.Fatal("timed out waiting for concurrent call")
		}
	}
	server.await(t)
}

func TestNotificationMayArriveBeforeResponse(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"method": NotificationTurnStarted,
			"params": map[string]any{"threadId": "thread-1", "turn": map[string]any{"id": "turn-1"}},
		}); err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{"id": id, "result": map[string]any{"ok": true}})
	})

	notifications := make(chan Notification, 1)
	client, err := DialUnix(context.Background(), server.endpoint, Options{
		OnNotification: func(notification Notification) { notifications <- notification },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var response struct {
		OK bool `json:"ok"`
	}
	if err := client.Call(context.Background(), "test/race", struct{}{}, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatal("response did not decode")
	}
	select {
	case notification := <-notifications:
		if notification.Method != NotificationTurnStarted {
			t.Fatalf("notification = %q", notification.Method)
		}
	case <-time.After(testTimeout):
		t.Fatal("notification was not delivered")
	}
	server.await(t)
}

func TestBlockedReverseRequestDoesNotBlockReader(t *testing.T) {
	reverseResponse := make(chan map[string]json.RawMessage, 1)
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{
			"id": "tool-7", "method": MethodDynamicToolCall,
			"params": map[string]any{
				"threadId": "thread-1", "turnId": "turn-1", "callId": "call-1",
				"namespace": nil, "tool": "send_message", "arguments": map[string]any{"to": "bob"},
			},
		}); err != nil {
			return err
		}
		if err := writeObject(ctx, conn, map[string]any{"id": id, "result": map[string]any{"ok": true}}); err != nil {
			return err
		}
		response, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		reverseResponse <- response
		return nil
	})

	received := make(chan *ReverseRequest, 1)
	observed := make(chan string, 1)
	ordered := make(chan bool, 1)
	release := make(chan struct{})
	client, err := DialUnix(context.Background(), server.endpoint, Options{
		OnReverseRequestReceived: func(method string) { observed <- method },
		OnReverseRequest: func(request *ReverseRequest) {
			select {
			case method := <-observed:
				ordered <- method == MethodDynamicToolCall
			default:
				ordered <- false
			}
			received <- request
			<-release
			_ = request.Respond(context.Background(), DynamicToolCallResponse{
				ContentItems: []DynamicToolCallOutputContentItem{DynamicToolText("sent")}, Success: true,
			})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.Call(context.Background(), "test/while-tool-blocked", struct{}{}, &result); err != nil {
		t.Fatalf("normal response was blocked by reverse handler: %v", err)
	}
	if !result.OK {
		t.Fatal("normal response missing")
	}
	select {
	case ok := <-ordered:
		if !ok {
			t.Fatal("reverse handler ran before the ordered receive observer")
		}
	case <-time.After(testTimeout):
		t.Fatal("reverse handler did not report observer ordering")
	}
	var reverse *ReverseRequest
	select {
	case reverse = <-received:
	case <-time.After(testTimeout):
		t.Fatal("reverse request not delivered")
	}
	var params DynamicToolCallParams
	if err := reverse.DecodeParams(&params); err != nil {
		t.Fatal(err)
	}
	if params.Tool != "send_message" || params.TurnID != "turn-1" {
		t.Fatalf("decoded reverse request = %+v", params)
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 20*time.Millisecond)
	if err := client.WaitHandlers(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitHandlers() while blocked = %v, want deadline exceeded", err)
	}
	cancelWait()
	close(release)
	select {
	case response := <-reverseResponse:
		id, err := objectID(response)
		if err != nil {
			t.Fatal(err)
		}
		if text, ok := id.Text(); !ok || text != "tool-7" {
			t.Fatalf("reverse response id = %v", id)
		}
		if _, ok := response["result"]; !ok {
			t.Fatalf("reverse response has no result: %v", response)
		}
	case <-time.After(testTimeout):
		t.Fatal("reverse response not sent")
	}
	if err := client.WaitHandlers(context.Background()); err != nil {
		t.Fatalf("WaitHandlers() after response = %v", err)
	}
	server.await(t)
}

func TestDuplicateResponseIDTerminatesClient(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		response := map[string]any{"id": id, "result": map[string]any{"ok": true}}
		if err := writeObject(ctx, conn, response); err != nil {
			return err
		}
		return writeObject(ctx, conn, response)
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result map[string]any
	if err := client.Call(context.Background(), "test/duplicate", struct{}{}, &result); err != nil {
		t.Fatal(err)
	}
	if err := client.Wait(); !errors.Is(err, ErrDuplicateResponseID) {
		t.Fatalf("Wait error = %v, want duplicate response", err)
	}
	server.await(t)
}

func TestUnknownResponseIDTerminatesClient(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		return writeObject(ctx, conn, map[string]any{"id": 999, "result": map[string]any{}})
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Wait(); !errors.Is(err, ErrUnknownResponseID) {
		t.Fatalf("Wait error = %v, want unknown response", err)
	}
	server.await(t)
}

func TestBinaryMessageIsRejected(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		err := conn.Write(ctx, websocket.MessageBinary, []byte(`{"id":1,"result":{}}`))
		if websocket.CloseStatus(err) != -1 {
			return nil
		}
		return err
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Wait(); !errors.Is(err, ErrBinaryMessage) {
		t.Fatalf("Wait error = %v, want binary-message error", err)
	}
	server.await(t)
}

func TestUnhandledReverseRequestGetsMethodNotFound(t *testing.T) {
	response := make(chan map[string]json.RawMessage, 1)
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := writeObject(ctx, conn, map[string]any{
			"id": "future-1", "method": "future/request", "params": map[string]any{"x": 1},
		}); err != nil {
			return err
		}
		object, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		response <- object
		return nil
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case object := <-response:
		var rpcErr RPCError
		if err := json.Unmarshal(object["error"], &rpcErr); err != nil {
			t.Fatal(err)
		}
		if rpcErr.Code != ErrorCodeMethodNotFound {
			t.Fatalf("RPC error = %+v", rpcErr)
		}
	case <-time.After(testTimeout):
		t.Fatal("unhandled reverse request was not answered")
	}
	server.await(t)
}

func TestLargeWebsocketMessageOver64KiB(t *testing.T) {
	const size = 128 << 10
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		return writeObject(ctx, conn, map[string]any{
			"id": id, "result": map[string]string{"blob": strings.Repeat("x", size)},
		})
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result struct {
		Blob string `json:"blob"`
	}
	if err := client.Call(context.Background(), "test/large", struct{}{}, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Blob) != size {
		t.Fatalf("blob size = %d", len(result.Blob))
	}
	server.await(t)
}

func TestOversizeInboundMessage(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		request, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		id, err := objectID(request)
		if err != nil {
			return err
		}
		// The peer may report StatusMessageTooBig while this write completes;
		// either outcome means the test fixture did its job.
		err = writeObject(ctx, conn, map[string]any{
			"id": id, "result": map[string]string{"blob": strings.Repeat("x", 2048)},
		})
		if websocket.CloseStatus(err) == websocket.StatusMessageTooBig {
			return nil
		}
		return err
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{MaxMessageSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result map[string]any
	err = client.Call(context.Background(), "test/oversize", struct{}{}, &result)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Call error = %v, want message-too-large", err)
	}
	server.await(t)
}

func TestOversizeOutboundMessage(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		_, _, err := conn.Read(ctx)
		if websocket.CloseStatus(err) != -1 || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || strings.Contains(fmt.Sprint(err), "EOF") {
			return nil
		}
		return err
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{MaxMessageSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Call(context.Background(), "test/oversize", map[string]string{"blob": strings.Repeat("x", 2048)}, nil)
	if !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("Call error = %v, want message-too-large", err)
	}
	server.await(t)
}

func TestCallDeadline(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if _, err := readObject(ctx, conn); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
		return nil
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = client.Call(ctx, "test/timeout", struct{}{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v, want deadline", err)
	}
	server.await(t)
}

func TestServerCloseSurfacesEOF(t *testing.T) {
	server := startUnixTestServer(t, func(_ context.Context, conn *websocket.Conn) error {
		return conn.Close(websocket.StatusNormalClosure, "done")
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Wait(); !errors.Is(err, io.EOF) || !strings.Contains(err.Error(), "done") {
		t.Fatalf("Wait error = %v, want EOF with close reason", err)
	}
	server.await(t)
}

func TestAbruptServerEOF(t *testing.T) {
	server := startUnixTestServer(t, func(_ context.Context, conn *websocket.Conn) error {
		return conn.CloseNow()
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Wait(); !errors.Is(err, io.EOF) {
		t.Fatalf("Wait error = %v, want EOF-compatible transport error", err)
	}
	server.await(t)
}

func TestAbsentUnixSocket(t *testing.T) {
	endpoint := "unix://" + filepath.Join(t.TempDir(), "missing.sock")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := DialUnix(ctx, endpoint, Options{})
	if client != nil || err == nil {
		t.Fatalf("DialUnix = (%v, %v), want error", client, err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("DialUnix error = %v, want os.ErrNotExist", err)
	}
}

func TestReverseRequestCanOnlyBeAnsweredOnce(t *testing.T) {
	requestSeen := make(chan *ReverseRequest, 1)
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		if err := writeObject(ctx, conn, map[string]any{
			"id": 3, "method": MethodCurrentTimeRead, "params": map[string]string{"threadId": "t"},
		}); err != nil {
			return err
		}
		_, err := readObject(ctx, conn)
		return err
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{
		OnReverseRequest: func(request *ReverseRequest) { requestSeen <- request },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := <-requestSeen
	if err := request.Respond(context.Background(), CurrentTimeReadResponse{CurrentTimeAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := request.Respond(context.Background(), CurrentTimeReadResponse{CurrentTimeAt: 2}); !errors.Is(err, ErrAlreadyResponded) {
		t.Fatalf("second response error = %v", err)
	}
	server.await(t)
}

func TestQueuedWriteUsesCallerContextBudget(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		object, err := readObject(ctx, conn)
		if err != nil {
			return err
		}
		method, err := objectMethod(object)
		if err != nil {
			return err
		}
		if method != "test/after-queue" {
			return fmt.Errorf("method = %q, queued canceled write reached peer", method)
		}
		return nil
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Deterministically occupy the client's serialization slot. The canceled
	// call must return without waiting for the slot and must write no bytes.
	<-client.writeGate
	released := false
	defer func() {
		if !released {
			client.writeGate <- struct{}{}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- client.Notify(ctx, "test/canceled-queue", struct{}{}) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Notify error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queued Notify ignored its canceled context")
	}
	client.writeGate <- struct{}{}
	released = true

	if err := client.Notify(context.Background(), "test/after-queue", struct{}{}); err != nil {
		t.Fatalf("Notify after canceled queue wait: %v", err)
	}
	server.await(t)
}

func TestLocalCloseWinsRegisteredQueuedStartCall(t *testing.T) {
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		_, _, err := conn.Read(ctx)
		if websocket.CloseStatus(err) != -1 || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}

	<-client.writeGate
	released := false
	defer func() {
		if !released {
			client.writeGate <- struct{}{}
		}
	}()
	started := make(chan error, 1)
	go func() {
		_, err := client.StartCall(context.Background(), "test/close-race", struct{}{})
		started <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		client.mu.Lock()
		registered := len(client.pending) == 1
		client.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("StartCall did not register before Close")
		}
		time.Sleep(time.Millisecond)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-started:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("StartCall racing Close error = %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("StartCall remained queued after Close")
	}
	if err := client.Wait(); err != nil {
		t.Fatalf("Wait after local Close = %v, want nil", err)
	}
	client.writeGate <- struct{}{}
	released = true
	server.await(t)
}

func TestReverseRequestConcurrencyLimitTerminatesClient(t *testing.T) {
	const limit = 2
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		for id := 0; id <= limit; id++ {
			if err := writeObject(ctx, conn, map[string]any{
				"id": id, "method": MethodCurrentTimeRead,
				"params": map[string]string{"threadId": "thread-1"},
			}); err != nil {
				if websocket.CloseStatus(err) != -1 || errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
		_, _, err := conn.Read(ctx)
		if websocket.CloseStatus(err) != -1 || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	})
	started := make(chan struct{}, limit+1)
	release := make(chan struct{})
	client, err := DialUnix(context.Background(), server.endpoint, Options{
		MaxConcurrentHandlers: limit,
		OnReverseRequest: func(*ReverseRequest) {
			started <- struct{}{}
			<-release
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Wait(); !errors.Is(err, ErrHandlerLimit) {
		t.Fatalf("Wait error = %v, want handler limit", err)
	}
	for range limit {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("admitted reverse handler did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("reverse handler started beyond configured concurrency limit")
	default:
	}
	close(release)
	if err := client.WaitHandlers(context.Background()); err != nil {
		t.Fatalf("WaitHandlers after releasing bounded handlers: %v", err)
	}
	server.await(t)
}

func TestConcurrentWritesAreSerialized(t *testing.T) {
	const calls = 20
	server := startUnixTestServer(t, func(ctx context.Context, conn *websocket.Conn) error {
		for range calls {
			request, err := readObject(ctx, conn)
			if err != nil {
				return err
			}
			id, err := objectID(request)
			if err != nil {
				return err
			}
			if err := writeObject(ctx, conn, map[string]any{"id": id, "result": map[string]any{}}); err != nil {
				return err
			}
		}
		return nil
	})
	client, err := DialUnix(context.Background(), server.endpoint, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var wg sync.WaitGroup
	errs := make(chan error, calls)
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- client.Call(context.Background(), "test/concurrent", struct{}{}, nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
	}
	server.await(t)
}
