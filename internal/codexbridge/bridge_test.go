package codexbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/wire"
)

const testToken = "0123456789abcdef0123456789abcdef"

func listenTestController(t *testing.T, handler Handler, mutate func(*Options)) (*Controller, *Client) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		SocketPath:     filepath.Join(dir, "bridge.sock"),
		Token:          testToken,
		Handler:        handler,
		RequestTimeout: time.Second,
	}
	if mutate != nil {
		mutate(&opts)
	}
	controller, err := Listen(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := controller.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	client, err := NewClient(ClientOptions{SocketPath: opts.SocketPath, Token: opts.Token, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return controller, client
}

func TestGenerateToken(t *testing.T) {
	t.Parallel()
	a, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || len(b) != 64 || a == b {
		t.Fatalf("tokens have lengths %d/%d or matched", len(a), len(b))
	}
	if err := validateToken(a); err != nil {
		t.Fatal(err)
	}
}

func TestClientPingSendListAndMetadata(t *testing.T) {
	t.Parallel()
	type observed struct {
		method   string
		metadata json.RawMessage
		to       string
		message  string
	}
	seen := make(chan observed, 2)
	_, client := listenTestController(t, HandlerFuncs{
		SendMessageFunc: func(_ context.Context, metadata json.RawMessage, to, message string) (wire.SendAck, error) {
			seen <- observed{method: methodSendMessage, metadata: metadata, to: to, message: message}
			return wire.SendAckOK("controller-id"), nil
		},
		ListPeersFunc: func(_ context.Context, metadata json.RawMessage) ([]string, error) {
			seen <- observed{method: methodListPeers, metadata: metadata}
			return []string{"alice", "reviewer"}, nil
		},
	}, nil)
	if err := client.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	metadata := json.RawMessage(`{"threadId":"thread-1","x-codex-turn-metadata":{"turnId":"turn-9","parent":"turn-8"},"other":{"kept":true}}`)
	ack, err := client.SendMessage(t.Context(), metadata, "reviewer", "check this")
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.ID != "controller-id" {
		t.Fatalf("ack = %+v", ack)
	}
	peers, err := client.ListPeers(t.Context(), metadata)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(peers, ",") != "alice,reviewer" {
		t.Fatalf("peers = %v", peers)
	}
	for range 2 {
		got := <-seen
		if got.method == methodSendMessage && (got.to != "reviewer" || got.message != "check this") {
			t.Fatalf("send observation = %+v", got)
		}
		assertJSONEqual(t, got.metadata, metadata)
	}
}

func TestMetadataAbsentAndNullPreserved(t *testing.T) {
	t.Parallel()
	seen := make(chan json.RawMessage, 2)
	_, client := listenTestController(t, HandlerFuncs{
		ListPeersFunc: func(_ context.Context, metadata json.RawMessage) ([]string, error) {
			seen <- metadata
			return nil, nil
		},
	}, nil)
	if _, err := client.ListPeers(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListPeers(t.Context(), json.RawMessage(`null`)); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != nil {
		t.Fatalf("absent metadata = %q", got)
	}
	if got := <-seen; string(got) != "null" {
		t.Fatalf("null metadata = %q", got)
	}
}

func TestAuthenticationFailureDoesNotInvokeHandler(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	controller, _ := listenTestController(t, HandlerFuncs{
		ListPeersFunc: func(context.Context, json.RawMessage) ([]string, error) {
			calls.Add(1)
			return nil, nil
		},
	}, nil)
	client, err := NewClient(ClientOptions{SocketPath: controller.SocketPath(), Token: strings.Repeat("x", minimumTokenBytes)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.ListPeers(t.Context(), nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "unauthorized" {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("handler calls = %d", calls.Load())
	}
}

func TestControllerRevalidatesParamsAndMethod(t *testing.T) {
	t.Parallel()
	_, client := listenTestController(t, HandlerFuncs{}, nil)
	_, err := client.call(t.Context(), methodSendMessage, json.RawMessage(`{"to":"bad peer","message":"x"}`), nil)
	var remote *RemoteError
	if !errors.As(err, &remote) || remote.Code != "invalid_params" {
		t.Fatalf("invalid params error = %v", err)
	}
	_, err = client.call(t.Context(), "other", json.RawMessage(`{}`), nil)
	if !errors.As(err, &remote) || remote.Code != "method_not_found" {
		t.Fatalf("unknown method error = %v", err)
	}
}

func TestConcurrentCallsUseIndependentConnections(t *testing.T) {
	t.Parallel()
	const count = 24
	gate := make(chan struct{})
	entered := make(chan struct{}, count)
	var active atomic.Int32
	var maximum atomic.Int32
	_, client := listenTestController(t, HandlerFuncs{
		ListPeersFunc: func(ctx context.Context, _ json.RawMessage) ([]string, error) {
			current := active.Add(1)
			defer active.Add(-1)
			for {
				old := maximum.Load()
				if current <= old || maximum.CompareAndSwap(old, current) {
					break
				}
			}
			entered <- struct{}{}
			select {
			case <-gate:
				return []string{"ready"}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}, func(opts *Options) { opts.MaxConcurrent = count })

	var wg sync.WaitGroup
	errs := make(chan error, count)
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			peers, err := client.ListPeers(t.Context(), nil)
			if err == nil && (len(peers) != 1 || peers[0] != "ready") {
				err = errors.New("unexpected peer result")
			}
			errs <- err
		}()
	}
	for range count {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("calls did not enter concurrently")
		}
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if maximum.Load() < 2 {
		t.Fatalf("maximum concurrency = %d", maximum.Load())
	}
}

func TestCallDeadlineCancelsControllerHandler(t *testing.T) {
	t.Parallel()
	handlerDone := make(chan error, 1)
	_, client := listenTestController(t, HandlerFuncs{
		ListPeersFunc: func(ctx context.Context, _ json.RawMessage) ([]string, error) {
			<-ctx.Done()
			handlerDone <- ctx.Err()
			return nil, ctx.Err()
		},
	}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	_, err := client.ListPeers(ctx, nil)
	var remote *RemoteError
	if !errors.Is(err, context.DeadlineExceeded) && (!errors.As(err, &remote) || remote.Code != "deadline_exceeded") {
		t.Fatalf("error = %v", err)
	}
	select {
	case err := <-handlerDone:
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			t.Fatalf("handler error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("controller handler was not canceled")
	}
}

func TestClientDisconnectCancelsControllerHandler(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{})
	handlerDone := make(chan error, 1)
	controller, _ := listenTestController(t, HandlerFuncs{
		ListPeersFunc: func(ctx context.Context, _ json.RawMessage) ([]string, error) {
			close(entered)
			<-ctx.Done()
			handlerDone <- ctx.Err()
			return nil, ctx.Err()
		},
	}, func(opts *Options) { opts.RequestTimeout = 5 * time.Second })
	conn, err := net.Dial("unix", controller.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(conn, request{
		Version: protocolVersion,
		ID:      1,
		Token:   testToken,
		Method:  methodListPeers,
		Params:  json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-handlerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handler error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel handler")
	}
}

func TestSocketPermissionsAndCleanup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.sock")
	controller, err := Listen(t.Context(), Options{SocketPath: path, Token: testToken, Handler: HandlerFuncs{}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v", info.Mode())
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains after close: %v", err)
	}
}

func TestParentCancellationStopsController(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	path := filepath.Join(dir, "bridge.sock")
	controller, err := Listen(ctx, Options{SocketPath: path, Token: testToken, Handler: HandlerFuncs{}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-controller.Done():
	case <-time.After(time.Second):
		t.Fatal("controller did not stop")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket remains: %v", err)
	}
}

func TestCloseDoesNotRemoveReplacementPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.sock")
	controller, err := Listen(t.Context(), Options{SocketPath: path, Token: testToken, Handler: HandlerFuncs{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = controller.Close()
	if err == nil || !strings.Contains(err.Error(), "refusing to remove replacement") {
		t.Fatalf("Close error = %v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil || string(data) != "replacement" {
		t.Fatalf("replacement data=%q error=%v", data, readErr)
	}
}

func TestListenRejectsInsecureParentAndExistingPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.sock")
	_, err := Listen(t.Context(), Options{SocketPath: path, Token: testToken, Handler: HandlerFuncs{}})
	if err == nil || !strings.Contains(err.Error(), "want 0700") {
		t.Fatalf("insecure parent error = %v", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Listen(t.Context(), Options{SocketPath: path, Token: testToken, Handler: HandlerFuncs{}})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing path error = %v", err)
	}
}

func TestClientRejectsChangedSocketMode(t *testing.T) {
	t.Parallel()
	controller, client := listenTestController(t, HandlerFuncs{}, nil)
	if err := os.Chmod(controller.SocketPath(), 0o666); err != nil {
		t.Fatal(err)
	}
	err := client.Ping(t.Context())
	if err == nil || !strings.Contains(err.Error(), "want socket 0600") {
		t.Fatalf("error = %v", err)
	}
}

func TestBoundedFrames(t *testing.T) {
	t.Parallel()
	if err := writeFrame(io.Discard, map[string]string{"value": strings.Repeat("x", MaxFrameSize)}); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("writeFrame error = %v", err)
	}
	_, err := readFrame(bytes.NewBufferString(strings.Repeat("x", MaxFrameSize+1) + "\n"))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("readFrame error = %v", err)
	}
	_, err = readFrame(bytes.NewBufferString(`{"valid":true}`))
	if err == nil || !strings.Contains(err.Error(), "newline terminated") {
		t.Fatalf("unterminated readFrame error = %v", err)
	}
}

func TestRawUnauthorizedFrame(t *testing.T) {
	t.Parallel()
	controller, _ := listenTestController(t, HandlerFuncs{}, nil)
	conn, err := net.Dial("unix", controller.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := writeFrame(conn, request{Version: protocolVersion, ID: 77, Token: strings.Repeat("z", minimumTokenBytes), Method: methodPing}); err != nil {
		t.Fatal(err)
	}
	raw, err := readFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	var resp response
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != 77 || resp.Error == nil || resp.Error.Code != "unauthorized" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestRawProtocolErrors(t *testing.T) {
	t.Parallel()
	controller, _ := listenTestController(t, HandlerFuncs{}, nil)
	for _, test := range []struct {
		name string
		line string
		code string
	}{
		{name: "malformed JSON", line: `{`, code: "invalid_request"},
		{name: "unsupported version", line: `{"version":9,"id":2,"token":"` + testToken + `","method":"ping"}`, code: "unsupported_version"},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, err := net.Dial("unix", controller.SocketPath())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, test.line+"\n"); err != nil {
				t.Fatal(err)
			}
			raw, err := readFrame(conn)
			if err != nil {
				t.Fatal(err)
			}
			var resp response
			if err := json.Unmarshal(raw, &resp); err != nil {
				t.Fatal(err)
			}
			if resp.Error == nil || resp.Error.Code != test.code {
				t.Fatalf("response = %+v", resp)
			}
		})
	}
}

func TestConstructorValidation(t *testing.T) {
	t.Parallel()
	if _, err := Listen(nil, Options{}); err == nil || !strings.Contains(err.Error(), "parent context") {
		t.Fatalf("nil Listen context error = %v", err)
	}
	if _, err := NewClient(ClientOptions{Token: testToken}); err == nil || !strings.Contains(err.Error(), "socket path") {
		t.Fatalf("empty client socket error = %v", err)
	}
	client := &Client{opts: ClientOptions{Timeout: time.Second}}
	if _, err := client.call(nil, methodPing, nil, nil); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("nil call context error = %v", err)
	}
	var controller *Controller
	if controller.SocketPath() != "" {
		t.Fatal("nil controller has socket path")
	}
	select {
	case <-controller.Done():
	default:
		t.Fatal("nil controller Done is not closed")
	}
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateToken(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"", "short", strings.Repeat("x", maximumTokenBytes+1)} {
		if err := validateToken(token); err == nil {
			t.Fatalf("validateToken accepted length %d", len(token))
		}
	}
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON %q: %v", got, err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decode want JSON %q: %v", want, err)
	}
	gotCanonical, _ := json.Marshal(gotValue)
	wantCanonical, _ := json.Marshal(wantValue)
	if !bytes.Equal(gotCanonical, wantCanonical) {
		t.Fatalf("JSON = %s, want %s", gotCanonical, wantCanonical)
	}
}
