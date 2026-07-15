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

func TestThreadListParamsMatchGeneratedWireShape(t *testing.T) {
	cursor := "opaque"
	limit := uint32(25)
	sortKey := ThreadSortRecencyAt
	direction := SortDescending
	archived := false
	params := ThreadListParams{
		Cursor:        &cursor,
		Limit:         &limit,
		SortKey:       &sortKey,
		SortDirection: &direction,
		SourceKinds:   []ThreadSourceKind{ThreadSourceCLI, ThreadSourceVSCode},
		Archived:      &archived,
		CWD:           "/tmp/project",
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if object["cursor"] != "opaque" || object["limit"] != float64(25) || object["sortKey"] != "recency_at" || object["sortDirection"] != "desc" {
		t.Fatalf("pagination/sort wire shape = %s", data)
	}
	if object["archived"] != false || object["cwd"] != "/tmp/project" {
		t.Fatalf("filters wire shape = %s", data)
	}
	sources, ok := object["sourceKinds"].([]any)
	if !ok || len(sources) != 2 || sources[0] != "cli" || sources[1] != "vscode" {
		t.Fatalf("source kinds = %#v", object["sourceKinds"])
	}

	input := `{"data":[{"id":"thread-1","source":"cli","cwd":"/tmp/project"}],"nextCursor":"next","backwardsCursor":"back"}`
	var response ThreadListResponse
	if err := json.Unmarshal([]byte(input), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].ID != "thread-1" || response.NextCursor == nil || *response.NextCursor != "next" || response.BackwardsCursor == nil || *response.BackwardsCursor != "back" {
		t.Fatalf("thread/list response = %+v", response)
	}
}

func TestThreadForkParamsMatchGeneratedWireShape(t *testing.T) {
	cwd := "/tmp/project"
	sandbox := SandboxDangerFullAccess
	params := ThreadForkParams{
		ThreadID:              "thread-source",
		CWD:                   &cwd,
		RuntimeWorkspaceRoots: []string{"/tmp/project"},
		ApprovalPolicy:        ApprovalNever,
		Sandbox:               &sandbox,
		Config:                map[string]any{"mcp_servers.intercom.command": "intercom"},
		ExcludeTurns:          true,
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	if object["threadId"] != "thread-source" || object["cwd"] != cwd || object["approvalPolicy"] != "never" || object["sandbox"] != "danger-full-access" || object["excludeTurns"] != true {
		t.Fatalf("thread/fork wire shape = %s", data)
	}
	config, ok := object["config"].(map[string]any)
	if !ok || config["mcp_servers.intercom.command"] != "intercom" {
		t.Fatalf("fork config = %#v", object["config"])
	}
}

func TestMCPServerStatusListWireShape(t *testing.T) {
	threadID := "thread-1"
	detail := MCPServerStatusToolsAndAuthOnly
	limit := uint32(10)
	data, err := json.Marshal(MCPServerStatusListParams{ThreadID: &threadID, Detail: &detail, Limit: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"limit":10,"detail":"toolsAndAuthOnly","threadId":"thread-1"}`; got != want {
		t.Fatalf("status params = %s, want %s", got, want)
	}

	input := `{
		"data":[{
			"name":"intercom","serverInfo":{"name":"intercom","title":null,"version":"1.0","description":null,"icons":null,"websiteUrl":null},
			"tools":{"send_message":{"name":"send_message","description":"Send","inputSchema":{"type":"object"}}},
			"resources":[],"resourceTemplates":[],"authStatus":"unsupported"
		}],"nextCursor":null
	}`
	var response MCPServerStatusListResponse
	if err := json.Unmarshal([]byte(input), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data) != 1 || response.Data[0].Name != "intercom" || response.Data[0].ServerInfo == nil || response.Data[0].ServerInfo.Version != "1.0" || response.Data[0].Tools["send_message"].Name != "send_message" || response.Data[0].AuthStatus != MCPAuthUnsupported {
		t.Fatalf("mcpServerStatus/list response = %+v", response)
	}
}

func TestMCPServerStartupStatusNotification(t *testing.T) {
	threadID := "thread-1"
	notification := Notification{
		Method: NotificationMCPServerStartupStatusUpdated,
		Params: json.RawMessage(`{"threadId":"thread-1","name":"intercom","status":"ready","error":null,"failureReason":null}`),
	}
	var status MCPServerStatusUpdatedNotification
	if err := notification.DecodeParams(&status); err != nil {
		t.Fatal(err)
	}
	if status.ThreadID == nil || *status.ThreadID != threadID || status.Name != "intercom" || status.Status != MCPServerReady || status.Error != nil || status.FailureReason != nil {
		t.Fatalf("startup status = %+v", status)
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
