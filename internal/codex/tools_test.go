package codex

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/intercomtools"
	"github.com/dpemmons/intercom/internal/wire"
)

type fakeResponder struct {
	result any
	rpcErr *appserver.RPCError
	ctxErr error
}

func (r *fakeResponder) Respond(ctx context.Context, result any) error {
	r.ctxErr = ctx.Err()
	r.result = result
	return nil
}

func (r *fakeResponder) RespondError(ctx context.Context, rpcErr *appserver.RPCError) error {
	r.ctxErr = ctx.Err()
	r.rpcErr = rpcErr
	return nil
}

type deadlineBrokerTools struct{}

func (deadlineBrokerTools) Send(ctx context.Context, _, _ string) (wire.SendAck, error) {
	<-ctx.Done()
	return wire.SendAck{}, ctx.Err()
}

func (deadlineBrokerTools) ListPeers(ctx context.Context) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

type fakeBrokerTools struct {
	ack   wire.SendAck
	peers []string
	err   error
	to    string
	msg   string
	sends int
	lists int
}

func (b *fakeBrokerTools) Send(_ context.Context, to, message string) (wire.SendAck, error) {
	b.sends++
	b.to, b.msg = to, message
	return b.ack, b.err
}

func (b *fakeBrokerTools) ListPeers(context.Context) ([]string, error) {
	b.lists++
	return b.peers, b.err
}

func dynamicParams(tool, args string) json.RawMessage {
	value, _ := json.Marshal(appserver.DynamicToolCallParams{
		ThreadID: "thread-1", TurnID: "turn-1", CallID: "call-1", Tool: tool, Arguments: json.RawMessage(args),
	})
	return value
}

func TestDynamicToolSpecsShareContracts(t *testing.T) {
	t.Parallel()
	specs := dynamicToolSpecs()
	if len(specs) != 2 || specs[0].Name != intercomtools.SendMessageName || specs[1].Name != intercomtools.ListPeersName {
		t.Fatalf("dynamicToolSpecs() = %#v", specs)
	}
	if !reflect.DeepEqual(specs[0].InputSchema, intercomtools.SendMessageInputSchema) {
		t.Fatal("send_message schema is not shared")
	}
}

func TestReverseHandlerSendMessage(t *testing.T) {
	t.Parallel()
	broker := &fakeBrokerTools{ack: wire.SendAck{OK: true}}
	h := reverseHandler{
		broker: broker,
		authorize: func(threadID, turnID string) error {
			if threadID != "thread-1" || turnID != "turn-1" {
				t.Fatalf("authorize(%q, %q)", threadID, turnID)
			}
			return nil
		},
	}
	response := &fakeResponder{}
	if err := h.handle(t.Context(), appserver.MethodDynamicToolCall, dynamicParams(intercomtools.SendMessageName, `{"to":"bob","message":"hi"}`), response); err != nil {
		t.Fatal(err)
	}
	if broker.to != "bob" || broker.msg != "hi" {
		t.Fatalf("Send(%q, %q)", broker.to, broker.msg)
	}
	result, ok := response.result.(appserver.DynamicToolCallResponse)
	if !ok || !result.Success {
		t.Fatalf("response = %#v", response.result)
	}
}

func TestReverseHandlerStartupGate(t *testing.T) {
	t.Parallel()
	h := reverseHandler{authorize: func(string, string) error { return errors.New("adapter not ready") }}
	response := &fakeResponder{}
	err := h.handle(t.Context(), appserver.MethodDynamicToolCall, dynamicParams(intercomtools.ListPeersName, `{}`), response)
	if err == nil {
		t.Fatal("expected ownership violation")
	}
	result, ok := response.result.(appserver.DynamicToolCallResponse)
	if !ok || result.Success {
		t.Fatalf("response = %#v", response.result)
	}
}

func TestReverseHandlerRejectsInvalidDynamicRouting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		params appserver.DynamicToolCallParams
	}{
		{name: "namespace", params: appserver.DynamicToolCallParams{ThreadID: "thread-1", TurnID: "turn-1", CallID: "call-1", Namespace: ptr("foreign"), Tool: intercomtools.SendMessageName, Arguments: json.RawMessage(`{"to":"bob","message":"hi"}`)}},
		{name: "missing thread", params: appserver.DynamicToolCallParams{TurnID: "turn-1", CallID: "call-1", Tool: intercomtools.SendMessageName, Arguments: json.RawMessage(`{"to":"bob","message":"hi"}`)}},
		{name: "missing turn", params: appserver.DynamicToolCallParams{ThreadID: "thread-1", CallID: "call-1", Tool: intercomtools.SendMessageName, Arguments: json.RawMessage(`{"to":"bob","message":"hi"}`)}},
		{name: "missing call", params: appserver.DynamicToolCallParams{ThreadID: "thread-1", TurnID: "turn-1", Tool: intercomtools.SendMessageName, Arguments: json.RawMessage(`{"to":"bob","message":"hi"}`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			broker := &fakeBrokerTools{ack: wire.SendAck{OK: true}}
			h := reverseHandler{broker: broker, authorize: func(string, string) error { return nil }}
			raw, err := json.Marshal(tt.params)
			if err != nil {
				t.Fatal(err)
			}
			response := &fakeResponder{}
			if err := h.handle(t.Context(), appserver.MethodDynamicToolCall, raw, response); err == nil {
				t.Fatal("invalid routing was not reported as an ownership violation")
			}
			if broker.sends != 0 || broker.lists != 0 {
				t.Fatalf("broker calls: sends=%d lists=%d", broker.sends, broker.lists)
			}
			result, ok := response.result.(appserver.DynamicToolCallResponse)
			if !ok || result.Success {
				t.Fatalf("response = %#v", response.result)
			}
		})
	}
}

func TestReverseHandlerListPeersRequiresEmptyObject(t *testing.T) {
	t.Parallel()
	for _, args := range []string{`null`, `[]`, `{"extra":true}`} {
		t.Run(args, func(t *testing.T) {
			broker := &fakeBrokerTools{peers: []string{"alice"}}
			h := reverseHandler{broker: broker, authorize: func(string, string) error { return nil }}
			response := &fakeResponder{}
			if err := h.handle(t.Context(), appserver.MethodDynamicToolCall, dynamicParams(intercomtools.ListPeersName, args), response); err != nil {
				t.Fatal(err)
			}
			if broker.lists != 0 {
				t.Fatal("invalid list_peers arguments reached the broker")
			}
			result, ok := response.result.(appserver.DynamicToolCallResponse)
			if !ok || result.Success {
				t.Fatalf("response = %#v", response.result)
			}
		})
	}
}

func TestReverseHandlerCanRespondAfterBrokerDeadline(t *testing.T) {
	t.Parallel()
	workCtx, cancel := context.WithCancel(t.Context())
	cancel()
	response := &fakeResponder{}
	responder := freshResponseResponder{next: response, timeout: time.Second}
	h := reverseHandler{broker: deadlineBrokerTools{}, authorize: func(string, string) error { return nil }}
	if err := h.handle(workCtx, appserver.MethodDynamicToolCall,
		dynamicParams(intercomtools.SendMessageName, `{"to":"bob","message":"hi"}`), responder); err != nil {
		t.Fatal(err)
	}
	if response.ctxErr != nil {
		t.Fatalf("response inherited expired broker context: %v", response.ctxErr)
	}
	result, ok := response.result.(appserver.DynamicToolCallResponse)
	if !ok || result.Success {
		t.Fatalf("response = %#v", response.result)
	}
}

func TestReverseHandlerDenials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method   string
		params   string
		wantJSON string
	}{
		{appserver.MethodCommandExecutionApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1}`, `{"decision":"decline"}`},
		{appserver.MethodFileChangeApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1}`, `{"decision":"decline"}`},
		{appserver.MethodPermissionsApproval, `{"threadId":"t","turnId":"u","itemId":"i","startedAtMs":1,"cwd":"/tmp","permissions":{}}`, `{"permissions":{},"scope":"turn"}`},
		{appserver.MethodMCPServerElicitation, `{"threadId":"t","serverName":"s","mode":"form","_meta":null,"message":"m"}`, `{"action":"decline","content":null,"_meta":null}`},
		{appserver.MethodLegacyApplyPatchApproval, `{"conversationId":"t","callId":"c","fileChanges":{}}`, `{"decision":"denied"}`},
		{appserver.MethodLegacyExecApproval, `{"conversationId":"t","callId":"c","command":[],"cwd":"/tmp","parsedCmd":[]}`, `{"decision":"denied"}`},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			t.Parallel()
			response := &fakeResponder{}
			if err := (&reverseHandler{}).handle(t.Context(), tt.method, json.RawMessage(tt.params), response); err != nil {
				t.Fatal(err)
			}
			if response.result == nil || response.rpcErr != nil {
				t.Fatalf("result = %#v, rpcErr = %#v", response.result, response.rpcErr)
			}
			assertJSONValueEqual(t, response.result, tt.wantJSON)
		})
	}
}

func TestReverseHandlerUnsupportedHeadlessRequests(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		method string
		params string
	}{
		{appserver.MethodToolRequestUserInput, `{"threadId":"t","turnId":"u","itemId":"i","questions":[]}`},
		{appserver.MethodChatGPTAuthTokensRefresh, `{"reason":"expired"}`},
		{appserver.MethodAttestationGenerate, `{}`},
		{appserver.MethodCurrentTimeRead, `{"threadId":"t"}`},
	} {
		t.Run(tt.method, func(t *testing.T) {
			response := &fakeResponder{}
			if err := (&reverseHandler{}).handle(t.Context(), tt.method, json.RawMessage(tt.params), response); err != nil {
				t.Fatal(err)
			}
			if response.rpcErr == nil || response.rpcErr.Code != appserver.ErrorCodeInternal || response.result != nil {
				t.Fatalf("result = %#v, rpcErr = %#v", response.result, response.rpcErr)
			}
		})
	}
}

func TestReverseHandlerUnknownMethod(t *testing.T) {
	t.Parallel()
	response := &fakeResponder{}
	if err := (&reverseHandler{}).handle(t.Context(), "future/request", json.RawMessage(`{}`), response); err != nil {
		t.Fatal(err)
	}
	if response.rpcErr == nil || response.rpcErr.Code != appserver.ErrorCodeMethodNotFound {
		t.Fatalf("rpc error = %#v", response.rpcErr)
	}
}

func ptr(value string) *string { return &value }

func assertJSONValueEqual(t *testing.T, got any, wantJSON string) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(gotJSON, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("response JSON = %s, want %s", gotJSON, wantJSON)
	}
}
