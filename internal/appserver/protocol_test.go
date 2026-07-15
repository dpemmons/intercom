package appserver

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequestIDRoundTrip(t *testing.T) {
	for _, id := range []RequestID{NumberRequestID(-7), NumberRequestID(0), StringRequestID("request-1"), StringRequestID("")} {
		data, err := json.Marshal(id)
		if err != nil {
			t.Fatalf("marshal %v: %v", id, err)
		}
		var decoded RequestID
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("unmarshal %s: %v", data, err)
		}
		if decoded != id {
			t.Fatalf("round trip = %#v, want %#v", decoded, id)
		}
	}
}

func TestRequestIDRejectsNonInteger(t *testing.T) {
	for _, input := range []string{"null", "1.5", "{}", "true", "9223372036854775808"} {
		var id RequestID
		if err := json.Unmarshal([]byte(input), &id); err == nil {
			t.Errorf("Unmarshal(%s) succeeded", input)
		}
	}
}

func TestTextInputMatchesPinnedSchema(t *testing.T) {
	data, err := json.Marshal(TextInput("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"type":"text","text":"hello","text_elements":[]}`; got != want {
		t.Fatalf("text input = %s, want %s", got, want)
	}
}

func TestInitializeParamsExperimentalCapabilityWireName(t *testing.T) {
	params := InitializeParams{
		ClientInfo:   ClientInfo{Name: "intercom", Version: "test"},
		Capabilities: &InitializeCapabilities{ExperimentalAPI: true},
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	jsonText := string(data)
	for _, want := range []string{`"clientInfo"`, `"experimentalApi":true`, `"requestAttestation":false`} {
		if !strings.Contains(jsonText, want) {
			t.Errorf("initialize JSON %s does not contain %s", jsonText, want)
		}
	}
}

func TestThreadStartDynamicToolWireShape(t *testing.T) {
	cwd := "/tmp/project"
	sandbox := SandboxWorkspaceWrite
	ephemeral := false
	params := ThreadStartParams{
		CWD:            &cwd,
		ApprovalPolicy: ApprovalNever,
		Sandbox:        &sandbox,
		Ephemeral:      &ephemeral,
		DynamicTools: []DynamicToolSpec{{
			Type: "function", Name: "send_message", Description: "send",
			InputSchema: map[string]any{"type": "object"},
		}},
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if object["approvalPolicy"] != "never" || object["sandbox"] != "workspace-write" {
		t.Fatalf("policy wire shape = %s", data)
	}
	tools, ok := object["dynamicTools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("dynamic tools = %#v", object["dynamicTools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "send_message" {
		t.Fatalf("tool = %#v", tool)
	}
}

func TestThreadResponseDecodesPromotedFields(t *testing.T) {
	input := `{
		"thread":{"id":"thread-1","extra":null,"sessionId":"session-1","forkedFromId":null,"parentThreadId":null,"preview":"","ephemeral":false,"historyMode":"legacy","modelProvider":"openai","createdAt":1,"updatedAt":2,"recencyAt":2,"status":{"type":"idle"},"path":null,"cwd":"/tmp","cliVersion":"0.144.1","source":"appServer","threadSource":null,"agentNickname":null,"agentRole":null,"gitInfo":null,"name":null,"turns":[]},
		"model":"gpt-test","modelProvider":"openai","serviceTier":null,"cwd":"/tmp","runtimeWorkspaceRoots":[],"instructionSources":[],"approvalPolicy":"never","approvalsReviewer":"user","sandbox":{"type":"workspaceWrite","writableRoots":["/tmp"],"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false},"activePermissionProfile":null,"reasoningEffort":null,"multiAgentMode":"explicitRequestOnly"
	}`
	var response ThreadStartResponse
	if err := json.Unmarshal([]byte(input), &response); err != nil {
		t.Fatal(err)
	}
	if response.Thread.ID != "thread-1" || response.Thread.Status.Type != ThreadStatusIdle {
		t.Fatalf("thread = %+v", response.Thread)
	}
	if response.CWD != "/tmp" || response.Sandbox.Type != "workspaceWrite" {
		t.Fatalf("response = %+v", response)
	}
}

func TestDecodeTypedLifecycleNotification(t *testing.T) {
	notification := Notification{
		Method: NotificationTurnCompleted,
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1","items":[],"itemsView":"notLoaded","status":"interrupted","error":null,"startedAt":1,"completedAt":2,"durationMs":1000}}`),
	}
	var completed TurnCompletedNotification
	if err := notification.DecodeParams(&completed); err != nil {
		t.Fatal(err)
	}
	if completed.ThreadID != "thread-1" || completed.Turn.Status != TurnStatusInterrupted {
		t.Fatalf("completed = %+v", completed)
	}
}

func TestDefaultMessageLimitIs128MiB(t *testing.T) {
	if DefaultMaxMessageSize != 128*1024*1024 {
		t.Fatalf("DefaultMaxMessageSize = %d", DefaultMaxMessageSize)
	}
}

func TestParseUnixEndpoint(t *testing.T) {
	path, err := ParseUnixEndpoint("unix:///tmp/codex%20socket.sock")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/codex socket.sock" {
		t.Fatalf("path = %q", path)
	}
	for _, endpoint := range []string{"", "ws://localhost/x", "unix://relative/path", "unix:///tmp/x?query=1"} {
		if _, err := ParseUnixEndpoint(endpoint); err == nil {
			t.Errorf("ParseUnixEndpoint(%q) succeeded", endpoint)
		}
	}
}

func FuzzRequestIDJSON(f *testing.F) {
	for _, seed := range []string{`0`, `-7`, `"request-1"`, `""`, `null`, `1.5`, `{`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4<<10 {
			t.Skip()
		}
		var id RequestID
		if err := json.Unmarshal(data, &id); err != nil {
			return
		}
		encoded, err := json.Marshal(id)
		if err != nil {
			t.Fatalf("marshal accepted id: %v", err)
		}
		var again RequestID
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("unmarshal re-encoded id %s: %v", encoded, err)
		}
		if again != id {
			t.Fatalf("round trip = %#v, want %#v", again, id)
		}
	})
}

func FuzzParseUnixEndpoint(f *testing.F) {
	for _, seed := range []string{"unix:///tmp/codex.sock", "unix:///tmp/a%20b.sock", "", "ws://localhost/x", "unix://relative/path"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, endpoint string) {
		if len(endpoint) > 16<<10 {
			t.Skip()
		}
		path, err := ParseUnixEndpoint(endpoint)
		if err != nil {
			return
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
			t.Fatalf("accepted invalid canonical path %q from %q", path, endpoint)
		}
	})
}
