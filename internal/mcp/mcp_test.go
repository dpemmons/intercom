package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// driver wires a Server up to a pair of io.Pipes so a test can write JSON-RPC
// frames as the "client" and read what the server writes back. A background
// goroutine continuously drains the server's output into a channel so calls
// like Server.Notify don't deadlock against an unread io.Pipe.
type driver struct {
	srv      *Server
	cliWrite io.WriteCloser
	frames   chan []byte
	done     chan error
}

func newDriver(t *testing.T, opts Options, tools ...Tool) *driver {
	t.Helper()

	cliR, cliW := io.Pipe() // server reads cliR; client writes cliW
	srvR, srvW := io.Pipe() // server writes srvW; client reads srvR

	srv := NewServer(Implementation{Name: "test", Version: "0.0.0"}, opts)
	for _, tl := range tools {
		srv.RegisterTool(tl)
	}

	d := &driver{
		srv:      srv,
		cliWrite: cliW,
		frames:   make(chan []byte, 16),
		done:     make(chan error, 1),
	}
	go func() {
		err := srv.Run(t.Context(), cliR, srvW)
		d.done <- err
		_ = srvW.Close()
	}()
	go func() {
		defer close(d.frames)
		r := bufio.NewReader(srvR)
		for {
			line, err := r.ReadBytes('\n')
			if len(line) > 0 {
				d.frames <- line
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cliW.Close() // EOF the server's input
		select {
		case <-d.done:
		case <-time.After(2 * time.Second):
			t.Errorf("server did not exit within 2s")
		}
	})
	return d
}

// send writes one JSON-RPC frame as the client.
func (d *driver) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := io.WriteString(d.cliWrite, raw+"\n"); err != nil {
		t.Fatalf("client write: %v", err)
	}
}

// recv reads one JSON-RPC frame back from the server, decoded into m.
func (d *driver) recv(t *testing.T, m any) {
	t.Helper()
	select {
	case line, ok := <-d.frames:
		if !ok {
			t.Fatal("server output closed before frame arrived")
		}
		if err := json.Unmarshal(line, m); err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for server frame")
	}
}

func TestInitializeRespondsWithCapabilities(t *testing.T) {
	d := newDriver(t, Options{
		Instructions: "be helpful",
		Experimental: map[string]any{"claude/channel": map[string]any{}},
	}, Tool{
		Name:        "noop",
		Description: "does nothing",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{Text: "ok"}, nil
		},
	})

	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)

	var resp struct {
		ID     int `json:"id"`
		Result struct {
			ProtocolVersion string         `json:"protocolVersion"`
			Capabilities    map[string]any `json:"capabilities"`
			ServerInfo      Implementation `json:"serverInfo"`
			Instructions    string         `json:"instructions"`
		} `json:"result"`
	}
	d.recv(t, &resp)

	if resp.ID != 1 {
		t.Errorf("id = %d, want 1", resp.ID)
	}
	if resp.Result.ProtocolVersion != "2025-11-25" {
		t.Errorf("protocolVersion = %q", resp.Result.ProtocolVersion)
	}
	if resp.Result.Instructions != "be helpful" {
		t.Errorf("instructions = %q", resp.Result.Instructions)
	}
	if _, ok := resp.Result.Capabilities["tools"]; !ok {
		t.Errorf("capabilities missing tools: %v", resp.Result.Capabilities)
	}
	exp, ok := resp.Result.Capabilities["experimental"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.experimental missing or wrong type: %T", resp.Result.Capabilities["experimental"])
	}
	if _, ok := exp["claude/channel"]; !ok {
		t.Errorf("experimental missing claude/channel: %v", exp)
	}
}

func TestInitializedNotificationClosesChannel(t *testing.T) {
	d := newDriver(t, Options{})

	select {
	case <-d.srv.Initialized():
		t.Fatal("Initialized closed before notification")
	default:
	}

	d.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	select {
	case <-d.srv.Initialized():
		// ok
	case <-time.After(time.Second):
		t.Fatal("Initialized did not close after notifications/initialized")
	}
}

func TestToolsListReturnsRegistered(t *testing.T) {
	d := newDriver(t, Options{}, Tool{
		Name:        "echo",
		Description: "echoes input",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{Text: "ok"}, nil
		},
	})

	d.send(t, `{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)

	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	d.recv(t, &resp)

	if resp.ID != 7 {
		t.Errorf("id = %d", resp.ID)
	}
	if len(resp.Result.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(resp.Result.Tools))
	}
	tool := resp.Result.Tools[0]
	if tool.Name != "echo" || tool.Description != "echoes input" {
		t.Errorf("tool = %+v", tool)
	}
	if !strings.Contains(string(tool.InputSchema), `"text"`) {
		t.Errorf("schema = %s", tool.InputSchema)
	}
}

func TestToolsCallSuccess(t *testing.T) {
	d := newDriver(t, Options{}, Tool{
		Name:        "shout",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Handler: func(_ context.Context, args json.RawMessage) (ToolResult, error) {
			var in struct{ Text string }
			_ = json.Unmarshal(args, &in)
			return ToolResult{Text: strings.ToUpper(in.Text)}, nil
		},
	})

	d.send(t, `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"shout","arguments":{"text":"hi"}}}`)

	var resp struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	d.recv(t, &resp)

	if resp.ID != 42 {
		t.Errorf("id = %d", resp.ID)
	}
	if resp.Result.IsError {
		t.Errorf("unexpected isError")
	}
	if len(resp.Result.Content) != 1 || resp.Result.Content[0].Type != "text" || resp.Result.Content[0].Text != "HI" {
		t.Errorf("content = %+v", resp.Result.Content)
	}
}

func TestToolsCallMetadataPreserved(t *testing.T) {
	wantMeta := json.RawMessage(`{"threadId":"thread-1","x-codex-turn-metadata":{"turnId":"turn-2","nested":{"sequence":3}},"extension":[true,null]}`)
	gotMeta := make(chan json.RawMessage, 1)
	d := newDriver(t, Options{}, Tool{
		Name:        "inspect",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		HandlerWithMeta: func(_ context.Context, _ json.RawMessage, meta json.RawMessage) (ToolResult, error) {
			gotMeta <- append(json.RawMessage(nil), meta...)
			return ToolResult{Text: "ok"}, nil
		},
	})

	d.send(t, `{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"inspect","arguments":{},"_meta":`+string(wantMeta)+`}}`)
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	d.recv(t, &resp)
	if len(resp.Result.Content) != 1 || resp.Result.Content[0].Text != "ok" {
		t.Fatalf("response = %+v", resp)
	}

	var got, want any
	if err := json.Unmarshal(<-gotMeta, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantMeta, &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("metadata = %s, want %s", gotJSON, wantJSON)
	}
}

func TestToolsCallMetadataAbsentAndNullRemainDistinct(t *testing.T) {
	got := make(chan json.RawMessage, 2)
	d := newDriver(t, Options{}, Tool{
		Name:        "inspect",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		HandlerWithMeta: func(_ context.Context, _ json.RawMessage, meta json.RawMessage) (ToolResult, error) {
			got <- append(json.RawMessage(nil), meta...)
			return ToolResult{Text: "ok"}, nil
		},
	})
	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"inspect","arguments":{}}}`)
	d.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"inspect","arguments":{},"_meta":null}}`)
	var response struct{}
	d.recv(t, &response)
	d.recv(t, &response)

	var absent, explicitNull bool
	for range 2 {
		meta := <-got
		switch string(meta) {
		case "":
			absent = true
		case "null":
			explicitNull = true
		default:
			t.Fatalf("unexpected metadata %q", meta)
		}
	}
	if !absent || !explicitNull {
		t.Fatalf("absent=%v explicitNull=%v", absent, explicitNull)
	}
}

func TestRegisterToolRejectsAmbiguousHandlers(t *testing.T) {
	t.Parallel()
	srv := NewServer(Implementation{}, Options{})
	defer func() {
		if recover() == nil {
			t.Fatal("RegisterTool did not panic")
		}
	}()
	srv.RegisterTool(Tool{
		Name: "ambiguous",
		Handler: func(context.Context, json.RawMessage) (ToolResult, error) {
			return ToolResult{}, nil
		},
		HandlerWithMeta: func(context.Context, json.RawMessage, json.RawMessage) (ToolResult, error) {
			return ToolResult{}, nil
		},
	})
}

func TestToolsCallIsErrorPropagates(t *testing.T) {
	d := newDriver(t, Options{}, Tool{
		Name:        "fail",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{Text: "no peer", IsError: true}, nil
		},
	})

	d.send(t, `{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"fail","arguments":{}}}`)

	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	d.recv(t, &resp)

	if !resp.Result.IsError {
		t.Errorf("expected isError true")
	}
	if resp.Result.Content[0].Text != "no peer" {
		t.Errorf("text = %q", resp.Result.Content[0].Text)
	}
}

func TestToolsCallProtocolErrorOnHandlerError(t *testing.T) {
	d := newDriver(t, Options{}, Tool{
		Name:        "boom",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{}, fmt.Errorf("kaboom")
		},
	})

	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom","arguments":{}}}`)

	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	d.recv(t, &resp)

	if resp.Error == nil {
		t.Fatal("expected protocol error")
	}
	if resp.Error.Code != codeInternal {
		t.Errorf("code = %d, want %d", resp.Error.Code, codeInternal)
	}
	if !strings.Contains(resp.Error.Message, "kaboom") {
		t.Errorf("message = %q", resp.Error.Message)
	}
}

func TestToolHandlerPanicBecomesProtocolError(t *testing.T) {
	d := newDriver(t, Options{}, Tool{
		Name:        "panicker",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			panic("oops")
		},
	})

	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"panicker","arguments":{}}}`)

	var resp struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	d.recv(t, &resp)

	if resp.Error == nil {
		t.Fatal("expected protocol error from panicking handler")
	}
	if resp.Error.Code != codeInternal {
		t.Errorf("code = %d, want %d", resp.Error.Code, codeInternal)
	}
	if !strings.Contains(resp.Error.Message, "panic") || !strings.Contains(resp.Error.Message, "oops") {
		t.Errorf("message = %q, want to mention panic/oops", resp.Error.Message)
	}
}

func TestUnknownToolReturnsMethodNotFound(t *testing.T) {
	d := newDriver(t, Options{})
	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`)

	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	d.recv(t, &resp)
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("error = %+v", resp.Error)
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	d := newDriver(t, Options{})
	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"nonsense"}`)

	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	d.recv(t, &resp)
	if resp.Error == nil || resp.Error.Code != codeMethodNotFound {
		t.Fatalf("error = %+v", resp.Error)
	}
}

// TestConcurrentToolCalls verifies a slow tool call does not block the read
// loop or other concurrent calls. We send two requests; the slow one parks on
// a channel until the fast one has completed.
func TestConcurrentToolCalls(t *testing.T) {
	gate := make(chan struct{})
	var releaseSlow sync.Once
	d := newDriver(t, Options{},
		Tool{
			Name:        "slow",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
				select {
				case <-gate:
				case <-ctx.Done():
					return ToolResult{}, ctx.Err()
				}
				return ToolResult{Text: "slow done"}, nil
			},
		},
		Tool{
			Name:        "fast",
			InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
				releaseSlow.Do(func() { close(gate) })
				return ToolResult{Text: "fast done"}, nil
			},
		},
	)

	// Fire slow first, then fast. fast must complete (releasing slow) before
	// slow's response arrives.
	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow","arguments":{}}}`)
	d.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fast","arguments":{}}}`)

	// Collect both responses; order isn't guaranteed.
	got := map[int]string{}
	for i := 0; i < 2; i++ {
		var resp struct {
			ID     int `json:"id"`
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		d.recv(t, &resp)
		got[resp.ID] = resp.Result.Content[0].Text
	}
	if got[1] != "slow done" {
		t.Errorf("slow text = %q", got[1])
	}
	if got[2] != "fast done" {
		t.Errorf("fast text = %q", got[2])
	}
}

func TestNotifyEmitsNotificationFrame(t *testing.T) {
	d := newDriver(t, Options{})

	// Hand-shake first so the client side is in a realistic state.
	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	var initResp struct{}
	d.recv(t, &initResp)
	d.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	<-d.srv.Initialized()

	if err := d.srv.Notify("notifications/claude/channel", map[string]any{
		"content": "hello",
		"meta":    map[string]string{"from": "alice"},
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	var notif struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			Content string            `json:"content"`
			Meta    map[string]string `json:"meta"`
		} `json:"params"`
		ID *int `json:"id"`
	}
	d.recv(t, &notif)

	if notif.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", notif.JSONRPC)
	}
	if notif.Method != "notifications/claude/channel" {
		t.Errorf("method = %q", notif.Method)
	}
	if notif.ID != nil {
		t.Errorf("notification has id %v", *notif.ID)
	}
	if notif.Params.Content != "hello" {
		t.Errorf("content = %q", notif.Params.Content)
	}
	if notif.Params.Meta["from"] != "alice" {
		t.Errorf("meta from = %q", notif.Params.Meta["from"])
	}
}

func TestPingReturnsEmpty(t *testing.T) {
	d := newDriver(t, Options{})
	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	var resp struct {
		ID     int            `json:"id"`
		Result map[string]any `json:"result"`
	}
	d.recv(t, &resp)
	if resp.ID != 1 || resp.Result == nil {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestRequestIDSpellingIsPreserved(t *testing.T) {
	for _, id := range []string{`"request-1"`, `9007199254740993`, `-1.25e+9`} {
		t.Run(id, func(t *testing.T) {
			d := newDriver(t, Options{})
			d.send(t, `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`)

			var resp struct {
				ID json.RawMessage `json:"id"`
			}
			d.recv(t, &resp)
			if string(resp.ID) != id {
				t.Fatalf("response id = %s, want exact %s", resp.ID, id)
			}
		})
	}
}

func TestInvalidRequestIDTypesReturnInvalidRequest(t *testing.T) {
	for _, id := range []string{`true`, `false`, `{}`, `[]`} {
		t.Run(id, func(t *testing.T) {
			d := newDriver(t, Options{})
			d.send(t, `{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`)

			var resp struct {
				ID    json.RawMessage `json:"id"`
				Error *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			d.recv(t, &resp)
			if string(resp.ID) != "null" {
				t.Fatalf("response id = %s, want null", resp.ID)
			}
			if resp.Error == nil || resp.Error.Code != codeInvalidRequest {
				t.Fatalf("error = %+v, want invalid request", resp.Error)
			}
		})
	}
}
