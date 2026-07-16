package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/intercomtools"
	"github.com/dpemmons/intercom/internal/wire"
)

// TestCompatibleCodexAppServerLocalProviderE2E drives the installed Codex
// binary through a real model-backed turn without credentials or external
// network access. A loopback Responses-compatible server deterministically
// asks for Intercom's list_peers dynamic tool, observes its output, and finishes
// the turn. The test then restarts app-server, resumes the materialized thread,
// and proves that the persisted dynamic tool remains usable. Finally it kills
// app-server while a reverse tool request is outstanding and verifies cold
// resume normalizes that turn to interrupted without replaying the request.
func TestCompatibleCodexAppServerLocalProviderE2E(t *testing.T) {
	if os.Getenv("INTERCOM_CODEX_SMOKE") != "1" {
		t.Skip("set INTERCOM_CODEX_SMOKE=1 to exercise the installed Codex app-server")
	}
	codexBin := os.Getenv("CODEX_BIN")
	if codexBin == "" {
		var err error
		codexBin, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	cwd := filepath.Join(root, "project")
	for _, dir := range []string{codexHome, cwd} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	const (
		firstCallID  = "intercom-before-restart"
		secondCallID = "intercom-after-restart"
		crashCallID  = "intercom-before-crash"
	)
	responses := newPinnedResponsesServer(t, []string{
		pinnedFunctionCallSSE("response-1", firstCallID, intercomtools.ListPeersName, `{}`),
		pinnedAssistantSSE("response-2", "message-1", "first turn complete"),
		pinnedFunctionCallSSE("response-3", secondCallID, intercomtools.ListPeersName, `{}`),
		pinnedAssistantSSE("response-4", "message-2", "resumed turn complete"),
		pinnedFunctionCallSSE("response-5", crashCallID, intercomtools.ListPeersName, `{}`),
	})
	writePinnedProviderConfig(t, codexHome, responses.URL())

	broker := &pinnedE2EBroker{peers: []string{"alice", "bob"}}
	probe := newPinnedReverseProbe(broker, crashCallID)
	instructions := developerInstructions("pinned-e2e")
	sandboxMode := appserver.SandboxWorkspaceWrite
	ephemeral := false

	firstNotifications := make(chan appserver.Notification, 256)
	first := startPinnedAppServer(t, codexBin, codexHome, root, "first", probe.options(firstNotifications))
	initializePinnedClient(t, first.client)
	started := callPinned(t, func(ctx context.Context) (appserver.ThreadStartResponse, error) {
		return first.client.ThreadStart(ctx, appserver.ThreadStartParams{
			CWD:                   &cwd,
			ApprovalPolicy:        string(appserver.ApprovalNever),
			Sandbox:               &sandboxMode,
			DeveloperInstructions: &instructions,
			Ephemeral:             &ephemeral,
			DynamicTools:          dynamicToolSpecs(),
		})
	})
	assertPinnedThread(t, started.Thread, started.CWD, started.ApprovalPolicy, started.Sandbox, cwd)
	threadID := started.Thread.ID
	firstTurn := startPinnedTurn(t, first.client, firstNotifications, threadID, cwd, started.Sandbox, "use list_peers once")
	assertPinnedDynamicCall(t, probe.nextCall(t), threadID, firstTurn.ID, firstCallID)
	materialized := readPinnedThread(t, first.client, threadID)
	assertPinnedTurnStatus(t, materialized, firstTurn.ID, appserver.TurnStatusCompleted)
	first.stop(t, syscall.SIGTERM, true)

	secondNotifications := make(chan appserver.Notification, 256)
	second := startPinnedAppServer(t, codexBin, codexHome, root, "second", probe.options(secondNotifications))
	initializePinnedClient(t, second.client)
	resumed := callPinned(t, func(ctx context.Context) (appserver.ThreadResumeResponse, error) {
		return second.client.ThreadResume(ctx, appserver.ThreadResumeParams{
			ThreadID:              threadID,
			CWD:                   &cwd,
			ApprovalPolicy:        string(appserver.ApprovalNever),
			Sandbox:               &sandboxMode,
			DeveloperInstructions: &instructions,
			ExcludeTurns:          true,
		})
	})
	assertPinnedThread(t, resumed.Thread, resumed.CWD, resumed.ApprovalPolicy, resumed.Sandbox, cwd)
	if resumed.Thread.ID != threadID {
		t.Fatalf("thread/resume returned thread %q, want %q", resumed.Thread.ID, threadID)
	}
	secondTurn := startPinnedTurn(t, second.client, secondNotifications, threadID, cwd, resumed.Sandbox, "use the restored list_peers tool once")
	assertPinnedDynamicCall(t, probe.nextCall(t), threadID, secondTurn.ID, secondCallID)
	afterResume := readPinnedThread(t, second.client, threadID)
	assertPinnedTurnStatus(t, afterResume, firstTurn.ID, appserver.TurnStatusCompleted)
	assertPinnedTurnStatus(t, afterResume, secondTurn.ID, appserver.TurnStatusCompleted)

	// Leave the third reverse request deliberately unanswered, then hard-kill
	// the real process. This is the crash boundary whose persisted history and
	// reverse-request behavior the next app-server instance must recover.
	crashTurn := beginPinnedTurn(t, second.client, threadID, cwd, resumed.Sandbox, "call list_peers and wait")
	assertPinnedDynamicCall(t, probe.nextCall(t), threadID, crashTurn.ID, crashCallID)
	second.stop(t, syscall.SIGKILL, false)

	thirdNotifications := make(chan appserver.Notification, 256)
	third := startPinnedAppServer(t, codexBin, codexHome, root, "third", probe.options(thirdNotifications))
	initializePinnedClient(t, third.client)
	crashResumed := callPinned(t, func(ctx context.Context) (appserver.ThreadResumeResponse, error) {
		return third.client.ThreadResume(ctx, appserver.ThreadResumeParams{
			ThreadID:              threadID,
			CWD:                   &cwd,
			ApprovalPolicy:        string(appserver.ApprovalNever),
			Sandbox:               &sandboxMode,
			DeveloperInstructions: &instructions,
			ExcludeTurns:          false,
		})
	})
	assertPinnedThread(t, crashResumed.Thread, crashResumed.CWD, crashResumed.ApprovalPolicy, crashResumed.Sandbox, cwd)
	if crashResumed.Thread.ID != threadID {
		t.Fatalf("cold thread/resume returned thread %q, want %q", crashResumed.Thread.ID, threadID)
	}
	assertPinnedTurnStatus(t, crashResumed.Thread, crashTurn.ID, appserver.TurnStatusInterrupted)
	probe.assertNoCall(t, 300*time.Millisecond)
	third.stop(t, syscall.SIGTERM, true)

	requests := responses.snapshotRequests(t)
	if len(requests) != 5 {
		t.Fatalf("Responses requests = %d, want 5", len(requests))
	}
	for _, index := range []int{0, 2, 4} {
		assertPinnedToolAdvertised(t, requests[index], intercomtools.ListPeersName)
	}
	assertPinnedToolOutput(t, requests[1], firstCallID, "alice")
	assertPinnedToolOutput(t, requests[3], secondCallID, "alice")
	if got := broker.listCount(); got != 2 {
		t.Fatalf("list_peers broker calls = %d, want 2 (the crash call must remain unanswered)", got)
	}
	probe.assertNoFatal(t)
}

// TestCompatibleCodexAppServerForkedSubagentDynamicToolE2E exercises the real
// app-server's full-history subagent path while the adapter authorizes an
// inherited dynamic tool by issuing thread/read on the same WebSocket that is
// dispatching the reverse request. The loopback provider routes concurrent
// parent and child requests by their call IDs, so no request ordering is
// assumed and no external model request is made.
func TestCompatibleCodexAppServerForkedSubagentDynamicToolE2E(t *testing.T) {
	if os.Getenv("INTERCOM_CODEX_SMOKE") != "1" {
		t.Skip("set INTERCOM_CODEX_SMOKE=1 to exercise the installed Codex app-server")
	}
	codexBin := os.Getenv("CODEX_BIN")
	if codexBin == "" {
		var err error
		codexBin, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	cwd := filepath.Join(root, "project")
	for _, dir := range []string{codexHome, cwd} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	const (
		seedPrompt    = "retain this full-history marker: cobalt-orchid"
		parentPrompt  = "spawn the requested full-history child"
		childPrompt   = "invoke the inherited list_peers tool exactly once"
		spawnCallID   = "spawn-full-history-child"
		childCallID   = "forked-child-list-peers"
		collabV1      = "multi_agent_v1"
		spawnToolName = "spawn_agent"
	)
	spawnArguments := fmt.Sprintf(`{"message":%q,"fork_context":true}`, childPrompt)
	responses := newPinnedResponsesRouter(t, func(request map[string]any) (string, error) {
		switch {
		case pinnedRequestContains(request, childCallID):
			return pinnedAssistantSSE("response-child-final", "message-child-final", "child complete"), nil
		case pinnedRequestContains(request, spawnCallID):
			return pinnedAssistantSSE("response-parent-final", "message-parent-final", "parent complete"), nil
		case pinnedRequestContains(request, childPrompt):
			return pinnedFunctionCallSSE("response-child-tool", childCallID, intercomtools.ListPeersName, `{}`), nil
		case pinnedRequestContains(request, parentPrompt):
			return pinnedNamespacedFunctionCallSSE("response-parent-spawn", spawnCallID, collabV1, spawnToolName, spawnArguments), nil
		case pinnedRequestContains(request, seedPrompt):
			return pinnedAssistantSSE("response-seed", "message-seed", "history seeded"), nil
		default:
			encoded, _ := json.Marshal(request)
			return "", fmt.Errorf("unmatched Responses request: %s", encoded)
		}
	})
	writePinnedProviderConfigWithCollab(t, codexHome, responses.URL())

	broker := &pinnedE2EBroker{peers: []string{"alice", "bob"}}
	fatal := make(chan error, 8)
	dynamicCalls := make(chan appserver.DynamicToolCallParams, 8)
	notifications := make(chan appserver.Notification, 512)
	c := &controller{
		ctx:      t.Context(),
		cfg:      Config{ActivityTimeout: 5 * time.Second},
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		phase:    phaseActive,
		activity: make(chan struct{}, 1),
	}
	c.reverse = reverseHandler{
		broker:     broker,
		authorize:  c.authorizeReverse,
		onActivity: c.touchActivity,
		onFatal: func(err error) {
			select {
			case fatal <- err:
			default:
			}
		},
		timeout: 5 * time.Second,
		logger:  c.logger,
	}
	process := startPinnedAppServer(t, codexBin, codexHome, root, "forked-subagent", appserver.Options{
		OnNotification: func(notification appserver.Notification) {
			notifications <- notification
		},
		OnReverseRequestReceived: c.observeReverseRequest,
		OnReverseRequest: func(request *appserver.ReverseRequest) {
			if request.Method == appserver.MethodDynamicToolCall {
				var params appserver.DynamicToolCallParams
				if err := request.DecodeParams(&params); err == nil {
					dynamicCalls <- params
				}
			}
			c.handleReverseRequest(request)
		},
	})
	readProbe := &pinnedThreadReadProbe{appServerClient: process.client}
	c.app = readProbe
	initializePinnedClient(t, process.client)

	instructions := developerInstructions("pinned-forked-subagent-e2e")
	sandboxMode := appserver.SandboxWorkspaceWrite
	ephemeral := false
	started := callPinned(t, func(ctx context.Context) (appserver.ThreadStartResponse, error) {
		return process.client.ThreadStart(ctx, appserver.ThreadStartParams{
			CWD:                   &cwd,
			ApprovalPolicy:        string(appserver.ApprovalNever),
			Sandbox:               &sandboxMode,
			DeveloperInstructions: &instructions,
			Ephemeral:             &ephemeral,
			DynamicTools:          dynamicToolSpecs(),
		})
	})
	rootThreadID := started.Thread.ID
	c.mu.Lock()
	c.threadID = rootThreadID
	c.ready = true
	c.mu.Unlock()

	seedTurn := startPinnedTurn(t, process.client, notifications, rootThreadID, cwd, started.Sandbox, seedPrompt)
	parentTurn := startPinnedTurn(t, process.client, notifications, rootThreadID, cwd, started.Sandbox, parentPrompt)
	if seedTurn.ID == parentTurn.ID {
		t.Fatalf("seed and parent turns share id %q", seedTurn.ID)
	}

	var childCall appserver.DynamicToolCallParams
	select {
	case childCall = <-dynamicCalls:
	case err := <-fatal:
		t.Fatalf("adapter reverse handler failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for forked child dynamic tool call")
	}
	if childCall.ThreadID == "" || childCall.TurnID == "" {
		t.Fatalf("forked child dynamic tool call is missing routing ids: %#v", childCall)
	}
	assertPinnedDynamicCall(t, childCall, childCall.ThreadID, childCall.TurnID, childCallID)
	if childCall.ThreadID == rootThreadID {
		t.Fatalf("dynamic tool call used root thread %q instead of a child", rootThreadID)
	}

	childThread := waitPinnedTurnStatus(t, process.client, childCall.ThreadID, childCall.TurnID, appserver.TurnStatusCompleted)
	if !pinnedThreadHasAncestor(childThread, rootThreadID) {
		t.Fatalf("forked child ancestry = parent:%v forkedFrom:%v, want root %q", childThread.ParentThreadID, childThread.ForkedFromID, rootThreadID)
	}
	reads := readProbe.snapshot()
	if len(reads) != 1 || reads[0].ThreadID != childCall.ThreadID {
		t.Fatalf("adapter ancestry thread/read calls = %#v, want one read of child %q", reads, childCall.ThreadID)
	}
	if got := broker.listCount(); got != 1 {
		t.Fatalf("list_peers broker calls = %d, want 1", got)
	}

	requests := responses.snapshotRequests(t)
	if len(requests) != 5 {
		t.Fatalf("Responses requests = %d, want 5", len(requests))
	}
	childInitial := pinnedFindRequest(t, requests, func(request map[string]any) bool {
		return pinnedRequestContains(request, childPrompt) &&
			!pinnedRequestContains(request, spawnCallID) &&
			!pinnedRequestContains(request, childCallID)
	}, "forked child initial request")
	if !pinnedRequestContains(childInitial, seedPrompt) {
		t.Fatal("forked child request did not inherit the seed turn history")
	}
	assertPinnedToolAdvertised(t, childInitial, intercomtools.ListPeersName)
	childFollowup := pinnedFindRequest(t, requests, func(request map[string]any) bool {
		return pinnedRequestContains(request, childCallID)
	}, "forked child tool follow-up")
	assertPinnedToolOutput(t, childFollowup, childCallID, "alice")

	select {
	case err := <-fatal:
		t.Fatalf("adapter reported a fatal error: %v", err)
	default:
	}
	select {
	case <-process.client.Done():
		t.Fatalf("app-server client terminated after re-entrant ancestry lookup: %v", process.client.Wait())
	default:
	}
	rootThread := readPinnedThread(t, process.client, rootThreadID)
	assertPinnedTurnStatus(t, rootThread, parentTurn.ID, appserver.TurnStatusCompleted)
	process.stop(t, syscall.SIGTERM, true)
}

// TestCompatibleCodexAppServerAdoptOrdinarySessionMCPBridgeE2E proves the
// ordinary-session adoption boundary against an installed Codex app-server.
// It materializes an ordinary remote-TUI thread without Intercom dynamic
// tools, verifies its interactive VS Code source classification, adopts it
// through the controller, and lets Codex invoke list_peers through the
// request-scoped MCP helper built from this checkout. It then restarts both
// controller and app-server and proves that a cold resume reinjects a fresh
// helper instance from persisted rollout and Intercom binding state.
// The loopback provider and in-process broker require no credentials or
// external network access.
func TestCompatibleCodexAppServerAdoptOrdinarySessionMCPBridgeE2E(t *testing.T) {
	if os.Getenv("INTERCOM_CODEX_SMOKE") != "1" {
		t.Skip("set INTERCOM_CODEX_SMOKE=1 to exercise the installed Codex app-server")
	}
	codexBin := os.Getenv("CODEX_BIN")
	if codexBin == "" {
		var err error
		codexBin, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	cwd := filepath.Join(root, "project")
	bridgeDir := filepath.Join(root, "bridge")
	for _, dir := range []string{codexHome, cwd, bridgeDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	intercomBin := filepath.Join(root, "intercom")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", intercomBin, "./cmd/intercom")
	build.Dir = repoRoot
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Intercom MCP helper: %v\n%s", err, output)
	}

	const (
		seedPrompt       = "materialize this ordinary interactive session"
		goalObjective    = "complete the active-goal adoption regression"
		firstPrompt      = "invoke the managed list_peers tool after adoption"
		secondPrompt     = "invoke the managed list_peers tool after cold resume"
		firstCallID      = "adopted-mcp-list-peers"
		secondCallID     = "cold-resume-mcp-list-peers"
		firstDeliveryID  = "adopted-delivery"
		secondDeliveryID = "cold-resume-delivery"
	)
	goalRequestStarted := make(chan struct{})
	allowGoalResponse := make(chan struct{})
	var goalRequestOnce sync.Once
	var allowGoalOnce sync.Once
	releaseGoalResponse := func() { allowGoalOnce.Do(func() { close(allowGoalResponse) }) }
	defer releaseGoalResponse()
	responses := newPinnedResponsesRouter(t, func(request map[string]any) (string, error) {
		switch {
		case pinnedRequestContains(request, secondCallID):
			return pinnedAssistantSSE("response-cold-final", "message-cold-final", "cold resume complete"), nil
		case pinnedRequestContains(request, secondPrompt):
			namespace, err := pinnedManagedListPeersNamespace(request)
			if err != nil {
				return "", err
			}
			return pinnedNamespacedFunctionCallSSE("response-cold-tool", secondCallID, namespace, intercomtools.ListPeersName, `{}`), nil
		case pinnedRequestContains(request, firstCallID):
			return pinnedAssistantSSE("response-adopt-final", "message-adopt-final", "adoption complete"), nil
		case pinnedRequestContains(request, firstPrompt):
			namespace, err := pinnedManagedListPeersNamespace(request)
			if err != nil {
				return "", err
			}
			return pinnedNamespacedFunctionCallSSE("response-adopt-tool", firstCallID, namespace, intercomtools.ListPeersName, `{}`), nil
		case pinnedRequestContains(request, goalObjective):
			goalRequestOnce.Do(func() { close(goalRequestStarted) })
			<-allowGoalResponse
			return pinnedAssistantSSE("response-goal-final", "message-goal-final", "goal adoption complete"), nil
		case pinnedRequestContains(request, seedPrompt):
			return pinnedAssistantSSE("response-seed", "message-seed", "ordinary session materialized"), nil
		default:
			encoded, _ := json.Marshal(request)
			return "", fmt.Errorf("unmatched Responses request: %s", encoded)
		}
	})
	writePinnedProviderConfig(t, codexHome, responses.URL())
	process := startPinnedAppServer(t, codexBin, codexHome, root, "adopt-mcp", appserver.Options{})
	initializePinnedClient(t, process.client)
	endpoint := "unix://" + filepath.Join(root, "app-server-adopt-mcp.sock")
	materializePinnedInteractiveSession(t, codexBin, codexHome, cwd, endpoint, seedPrompt, "ordinary session materialized")
	ordinaryThread := readPinnedInteractiveSession(t, process.client, cwd)
	if source := pinnedThreadSource(ordinaryThread.Source); source != string(appserver.ThreadSourceVSCode) {
		t.Fatalf("remote Codex TUI thread source = %q, want vscode", source)
	}
	baselineTurns := len(ordinaryThread.Turns)
	if baselineTurns == 0 {
		t.Fatal("ordinary thread did not inherit the materialized seed turn")
	}
	process.stop(t, syscall.SIGTERM, true)

	goalSetter := startPinnedAppServer(t, codexBin, codexHome, root, "g", appserver.Options{})
	initializePinnedClient(t, goalSetter.client)
	setPinnedThreadGoal(t, goalSetter.client, ordinaryThread.ID, goalObjective, "active")
	select {
	case <-goalRequestStarted:
		goalSetter.stop(t, syscall.SIGKILL, false)
		t.Fatal("setting a goal on an unloaded thread unexpectedly started a turn")
	case <-time.After(100 * time.Millisecond):
	}
	goalSetter.stop(t, syscall.SIGTERM, true)

	process = startPinnedAppServer(t, codexBin, codexHome, root, "a", appserver.Options{})
	initializePinnedClient(t, process.client)
	endpoint = "unix://" + filepath.Join(root, "app-server-a.sock")

	recorder := newPinnedAdoptionBrokerRecorder([]string{"alice", "bob"})
	statePath := filepath.Join(root, "state.json")
	lockPath := filepath.Join(root, "state.lock")
	bridgeSocket := filepath.Join(bridgeDir, "mcp.sock")
	baseConfig := Config{
		Name:              "adopted-e2e",
		Version:           "test",
		CWD:               cwd,
		AppServerEndpoint: endpoint,
		MCPBridgeSocket:   bridgeSocket,
		IntercomBin:       intercomBin,
		BrokerSocket:      filepath.Join(root, "unused-broker.sock"),
		ExecutionPolicy:   ExecutionWorkspaceWrite,
		StatePath:         statePath,
		LockPath:          lockPath,
		StartupTimeout:    5 * time.Second,
		ControlTimeout:    10 * time.Second,
		ReverseTimeout:    5 * time.Second,
		ActivityTimeout:   10 * time.Second,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	baseConfig.newBroker = recorder.newConnection
	baseConfig.threadLockPath = func(_, threadID string) (string, error) {
		return filepath.Join(root, "thread-locks", threadID+".lock"), nil
	}

	firstConfig := baseConfig
	firstConfig.AdoptThreadID = ordinaryThread.ID
	first := startPinnedAdoptionController(t, firstConfig)
	if first.info.ThreadID != ordinaryThread.ID {
		first.stop(t)
		t.Fatalf("adopted thread = %q, want %q", first.info.ThreadID, ordinaryThread.ID)
	}
	assertPinnedManagedMCPStatus(t, process.client, ordinaryThread.ID)
	select {
	case <-goalRequestStarted:
	case err := <-first.done:
		first.cancel()
		t.Fatalf("controller stopped before active goal continuation: %v", err)
	case <-time.After(10 * time.Second):
		first.stop(t)
		t.Fatal("active goal did not continue during adoption")
	}
	setPinnedThreadGoal(t, process.client, ordinaryThread.ID, "", "complete")
	releaseGoalResponse()
	waitPinnedCompletedTurnCount(t, process.client, ordinaryThread.ID, baselineTurns+1)
	first.broker.deliver(wire.Deliver{
		ID: firstDeliveryID, From: "alice", Message: firstPrompt, Timestamp: "2026-07-14T12:00:00Z",
	})
	recorder.waitForLists(t, 1, first.done)
	waitPinnedCompletedTurnCount(t, process.client, ordinaryThread.ID, baselineTurns+2)
	first.stop(t)
	process.stop(t, syscall.SIGTERM, true)

	state, err := readManagedState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.ThreadID != ordinaryThread.ID || state.ToolTransport != ToolTransportMCPBridge || !state.Materialized {
		t.Fatalf("adopted state = %#v", state)
	}

	resumedProcess := startPinnedAppServer(t, codexBin, codexHome, root, "r", appserver.Options{})
	initializePinnedClient(t, resumedProcess.client)
	resumedEndpoint := "unix://" + filepath.Join(root, "app-server-r.sock")
	secondConfig := baseConfig
	secondConfig.AppServerEndpoint = resumedEndpoint
	second := startPinnedAdoptionController(t, secondConfig)
	if second.info.ThreadID != ordinaryThread.ID {
		second.stop(t)
		t.Fatalf("cold-resumed thread = %q, want %q", second.info.ThreadID, ordinaryThread.ID)
	}
	assertPinnedManagedMCPStatus(t, resumedProcess.client, ordinaryThread.ID)
	second.broker.deliver(wire.Deliver{
		ID: secondDeliveryID, From: "bob", Message: secondPrompt, Timestamp: "2026-07-14T12:01:00Z",
	})
	recorder.waitForLists(t, 2, second.done)
	waitPinnedCompletedTurnCount(t, resumedProcess.client, ordinaryThread.ID, baselineTurns+3)
	second.stop(t)
	resumedProcess.stop(t, syscall.SIGTERM, true)

	requests := responses.snapshotRequests(t)
	if len(requests) != 6 {
		t.Fatalf("Responses requests = %d, want 6", len(requests))
	}
	if pinnedRequestAdvertisesTool(requests[0], intercomtools.ListPeersName) ||
		pinnedRequestAdvertisesTool(requests[0], intercomtools.SendMessageName) {
		t.Fatalf("ordinary seed request unexpectedly advertised Intercom tools: %#v", requests[0]["tools"])
	}
	firstInitial := pinnedFindRequest(t, requests, func(request map[string]any) bool {
		return pinnedRequestContains(request, firstPrompt) && !pinnedRequestContains(request, firstCallID)
	}, "adopted MCP initial request")
	if _, err := pinnedManagedListPeersNamespace(firstInitial); err != nil {
		t.Fatal(err)
	}
	firstFollowup := pinnedFindRequest(t, requests, func(request map[string]any) bool {
		return pinnedRequestContains(request, firstCallID) && !pinnedRequestContains(request, secondPrompt)
	}, "adopted MCP tool follow-up")
	assertPinnedToolOutput(t, firstFollowup, firstCallID, "alice")
	secondInitial := pinnedFindRequest(t, requests, func(request map[string]any) bool {
		return pinnedRequestContains(request, secondPrompt) && !pinnedRequestContains(request, secondCallID)
	}, "cold-resume MCP initial request")
	if _, err := pinnedManagedListPeersNamespace(secondInitial); err != nil {
		t.Fatal(err)
	}
	secondFollowup := pinnedFindRequest(t, requests, func(request map[string]any) bool {
		return pinnedRequestContains(request, secondCallID)
	}, "cold-resume MCP tool follow-up")
	assertPinnedToolOutput(t, secondFollowup, secondCallID, "alice")
	if got := recorder.listCount(); got != 2 {
		t.Fatalf("validated MCP list_peers broker calls = %d, want 2", got)
	}
}

func materializePinnedInteractiveSession(
	t *testing.T,
	codexBin, codexHome, cwd, endpoint, prompt, completionMarker string,
) {
	t.Helper()
	scriptBin, err := exec.LookPath("script")
	if err != nil {
		t.Fatalf("locate script(1) for the Codex TUI smoke test: %v", err)
	}
	wrapper := filepath.Join(filepath.Dir(codexHome), "run-codex-tui.sh")
	wrapperBody := `#!/bin/sh
exec "$CODEX_SMOKE_BIN" --remote "$CODEX_SMOKE_ENDPOINT" --no-alt-screen --cd "$CODEX_SMOKE_CWD" "$CODEX_SMOKE_PROMPT"
`
	if err := os.WriteFile(wrapper, []byte(wrapperBody), 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(scriptBin, "-qefc", wrapper, "/dev/null")
	cmd.Env = pinnedProcessEnv(os.Environ(), map[string]string{
		"CODEX_HOME":                           codexHome,
		"CODEX_APP_SERVER_MANAGED_CONFIG_PATH": filepath.Join(codexHome, "managed_config.toml"),
		"CODEX_SMOKE_BIN":                      codexBin,
		"CODEX_SMOKE_ENDPOINT":                 endpoint,
		"CODEX_SMOKE_CWD":                      cwd,
		"CODEX_SMOKE_PROMPT":                   prompt,
		"TERM":                                 "xterm-256color",
		"NO_PROXY":                             "127.0.0.1,localhost",
		"no_proxy":                             "127.0.0.1,localhost",
	})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start Codex TUI through script(1): %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	defer func() {
		_ = stdin.Close()
		if stopped || cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}()

	deadline := time.Now().Add(15 * time.Second)
	for !pinnedTreeContains(codexHome, completionMarker) {
		select {
		case processErr := <-done:
			stopped = true
			t.Fatalf("Codex TUI exited before materializing an interactive session: %v\n%s", processErr, output.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for Codex TUI to materialize an interactive session\n%s", output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The rollout is complete at this point. Terminate the scripted frontend so
	// adoption begins only after the ordinary TUI no longer owns the session.
	_ = stdin.Close()
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	select {
	case <-done:
		stopped = true
	case <-time.After(3 * time.Second):
		t.Fatalf("Codex TUI process group did not stop after SIGKILL\n%s", output.String())
	}
}

func pinnedTreeContains(root, marker string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found || entry.IsDir() {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr == nil && bytes.Contains(data, []byte(marker)) {
			found = true
		}
		return nil
	})
	return found
}

func readPinnedInteractiveSession(t *testing.T, client *appserver.Client, cwd string) appserver.Thread {
	t.Helper()
	limit := uint32(10)
	archived := false
	listed := callPinned(t, func(ctx context.Context) (appserver.ThreadListResponse, error) {
		return client.ThreadList(ctx, appserver.ThreadListParams{
			Limit:       &limit,
			CWD:         cwd,
			SourceKinds: []appserver.ThreadSourceKind{appserver.ThreadSourceCLI, appserver.ThreadSourceVSCode},
			Archived:    &archived,
		})
	})
	for _, candidate := range listed.Data {
		thread := readPinnedThread(t, client, candidate.ID)
		if pinnedThreadHasCompletedTurn(thread) {
			return thread
		}
	}
	t.Fatalf("Codex TUI did not create a materialized interactive session: %#v", listed.Data)
	return appserver.Thread{}
}

func setPinnedThreadGoal(t *testing.T, client *appserver.Client, threadID, objective, status string) {
	t.Helper()
	params := map[string]any{"threadId": threadID, "status": status}
	if objective != "" {
		params["objective"] = objective
	}
	var response struct {
		Goal struct {
			ThreadID string `json:"threadId"`
			Status   string `json:"status"`
		} `json:"goal"`
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Call(ctx, "thread/goal/set", params, &response); err != nil {
		t.Fatalf("set thread %s goal status %s: %v", threadID, status, err)
	}
	if response.Goal.ThreadID != threadID || response.Goal.Status != status {
		t.Fatalf("thread/goal/set response = %#v, want thread %s status %s", response, threadID, status)
	}
}

func pinnedThreadHasCompletedTurn(thread appserver.Thread) bool {
	for _, turn := range thread.Turns {
		if turn.Status == appserver.TurnStatusCompleted {
			return true
		}
	}
	return false
}

type pinnedAdoptionController struct {
	cancel context.CancelFunc
	done   <-chan error
	broker *pinnedAdoptionBroker
	info   ReadyInfo
}

func startPinnedAdoptionController(t *testing.T, cfg Config) pinnedAdoptionController {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan ReadyInfo, 1)
	brokers := make(chan *pinnedAdoptionBroker, 1)
	newBroker := cfg.newBroker
	cfg.newBroker = func(opts brokerclient.ClientOptions) brokerConnection {
		broker := newBroker(opts).(*pinnedAdoptionBroker)
		brokers <- broker
		return broker
	}
	cfg.OnReady = func(info ReadyInfo) error {
		ready <- info
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	var broker *pinnedAdoptionBroker
	select {
	case broker = <-brokers:
	case err := <-done:
		cancel()
		t.Fatalf("adoption controller exited before broker creation: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("timeout waiting for adoption controller broker")
	}
	select {
	case info := <-ready:
		return pinnedAdoptionController{cancel: cancel, done: done, broker: broker, info: info}
	case err := <-done:
		cancel()
		t.Fatalf("adoption controller exited before readiness: %v", err)
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("timeout waiting for adoption controller readiness")
	}
	return pinnedAdoptionController{}
}

func (c pinnedAdoptionController) stop(t *testing.T) {
	t.Helper()
	c.cancel()
	select {
	case err := <-c.done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("adoption controller shutdown = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout stopping adoption controller")
	}
}

type pinnedAdoptionBrokerRecorder struct {
	mu    sync.Mutex
	peers []string
	lists int
	calls chan struct{}
}

func newPinnedAdoptionBrokerRecorder(peers []string) *pinnedAdoptionBrokerRecorder {
	return &pinnedAdoptionBrokerRecorder{peers: append([]string(nil), peers...), calls: make(chan struct{}, 8)}
}

func (r *pinnedAdoptionBrokerRecorder) newConnection(opts brokerclient.ClientOptions) brokerConnection {
	return &pinnedAdoptionBroker{opts: opts, recorder: r, events: make(chan brokerclient.ConnectionEvent)}
}

func (r *pinnedAdoptionBrokerRecorder) recordList() []string {
	r.mu.Lock()
	r.lists++
	peers := append([]string(nil), r.peers...)
	r.mu.Unlock()
	r.calls <- struct{}{}
	return peers
}

func (r *pinnedAdoptionBrokerRecorder) waitForLists(t *testing.T, want int, controllerDone <-chan error) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for r.listCount() < want {
		select {
		case <-r.calls:
		case err := <-controllerDone:
			t.Fatalf("adoption controller exited while waiting for %d MCP list_peers calls: %v", want, err)
		case <-deadline.C:
			t.Fatalf("timeout waiting for %d MCP list_peers calls; got %d", want, r.listCount())
		}
	}
}

func (r *pinnedAdoptionBrokerRecorder) listCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lists
}

type pinnedAdoptionBroker struct {
	opts     brokerclient.ClientOptions
	recorder *pinnedAdoptionBrokerRecorder
	events   chan brokerclient.ConnectionEvent
}

func (b *pinnedAdoptionBroker) Connect(context.Context) error { return nil }
func (b *pinnedAdoptionBroker) Close() error                  { return nil }
func (b *pinnedAdoptionBroker) Send(context.Context, string, string) (wire.SendAck, error) {
	return wire.SendAck{}, errors.New("send_message is not used by the adoption E2E")
}
func (b *pinnedAdoptionBroker) ListPeers(context.Context) ([]string, error) {
	return b.recorder.recordList(), nil
}
func (b *pinnedAdoptionBroker) ConnectionEvents() <-chan brokerclient.ConnectionEvent {
	return b.events
}
func (b *pinnedAdoptionBroker) deliver(delivery wire.Deliver) { b.opts.OnDeliver(delivery) }

func pinnedManagedListPeersNamespace(request map[string]any) (string, error) {
	tools, ok := request["tools"].([]any)
	if !ok {
		return "", fmt.Errorf("Responses request tools = %#v", request["tools"])
	}
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		namespace, _ := tool["name"].(string)
		if tool["type"] != "namespace" || !strings.Contains(namespace, managedMCPServerName) {
			continue
		}
		members, _ := tool["tools"].([]any)
		for _, memberValue := range members {
			member, _ := memberValue.(map[string]any)
			if member["name"] == intercomtools.ListPeersName {
				return namespace, nil
			}
		}
	}
	return "", fmt.Errorf("Responses request did not advertise managed list_peers: %#v", tools)
}

func pinnedRequestAdvertisesTool(request map[string]any, name string) bool {
	tools, _ := request["tools"].([]any)
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		if tool["name"] == name {
			return true
		}
		members, _ := tool["tools"].([]any)
		for _, memberValue := range members {
			member, _ := memberValue.(map[string]any)
			if member["name"] == name {
				return true
			}
		}
	}
	return false
}

func pinnedThreadSource(raw json.RawMessage) string {
	var source string
	_ = json.Unmarshal(raw, &source)
	return source
}

func assertPinnedManagedMCPStatus(t *testing.T, client *appserver.Client, threadID string) {
	t.Helper()
	detail := appserver.MCPServerStatusToolsAndAuthOnly
	status := callPinned(t, func(ctx context.Context) (appserver.MCPServerStatusListResponse, error) {
		return client.MCPServerStatusList(ctx, appserver.MCPServerStatusListParams{
			ThreadID: &threadID,
			Detail:   &detail,
		})
	})
	for _, server := range status.Data {
		if server.Name != managedMCPServerName {
			continue
		}
		if _, ok := server.Tools[intercomtools.SendMessageName]; !ok {
			t.Fatalf("managed MCP status omits send_message: %#v", server)
		}
		if _, ok := server.Tools[intercomtools.ListPeersName]; !ok {
			t.Fatalf("managed MCP status omits list_peers: %#v", server)
		}
		return
	}
	t.Fatalf("managed MCP status is absent for thread %s: %#v", threadID, status.Data)
}

func waitPinnedCompletedTurnCount(t *testing.T, client *appserver.Client, threadID string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		thread := readPinnedThread(t, client, threadID)
		completed := 0
		for _, turn := range thread.Turns {
			if turn.Status == appserver.TurnStatusCompleted {
				completed++
			}
		}
		if completed >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("thread %s has %d completed turns, want at least %d: %#v", threadID, completed, want, thread.Turns)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

type pinnedResponsesServer struct {
	server  *httptest.Server
	scripts []string
	route   func(map[string]any) (string, error)

	mu       sync.Mutex
	requests []map[string]any
	errors   []string
}

func newPinnedResponsesServer(t *testing.T, scripts []string) *pinnedResponsesServer {
	t.Helper()
	s := &pinnedResponsesServer{scripts: scripts}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.server.Close)
	return s
}

func newPinnedResponsesRouter(t *testing.T, route func(map[string]any) (string, error)) *pinnedResponsesServer {
	t.Helper()
	s := &pinnedResponsesServer{route: route}
	s.server = httptest.NewServer(http.HandlerFunc(s.serveHTTP))
	t.Cleanup(s.server.Close)
	return s
}

func (s *pinnedResponsesServer) URL() string { return s.server.URL }

func (s *pinnedResponsesServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"models":[]}`)
		return
	}
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
		s.recordError(fmt.Sprintf("unexpected model request: %s %s", r.Method, r.URL.Path))
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		s.recordError("unexpected compressed Responses request: " + encoding)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		s.recordError("read Responses request: " + err.Error())
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		s.recordError("decode Responses request: " + err.Error())
		http.Error(w, "decode request", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	index := len(s.requests)
	s.requests = append(s.requests, request)
	route := s.route
	var script string
	if route == nil {
		if index >= len(s.scripts) {
			s.errors = append(s.errors, fmt.Sprintf("unexpected Responses request %d", index+1))
			s.mu.Unlock()
			http.Error(w, "unexpected request", http.StatusInternalServerError)
			return
		}
		script = s.scripts[index]
	}
	s.mu.Unlock()
	if route != nil {
		var routeErr error
		script, routeErr = route(request)
		if routeErr != nil {
			s.recordError(routeErr.Error())
			http.Error(w, "unmatched request", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = io.WriteString(w, script)
}

func (s *pinnedResponsesServer) recordError(message string) {
	s.mu.Lock()
	s.errors = append(s.errors, message)
	s.mu.Unlock()
}

func (s *pinnedResponsesServer) snapshotRequests(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errors) != 0 {
		t.Fatalf("loopback Responses server errors: %v", s.errors)
	}
	return append([]map[string]any(nil), s.requests...)
}

func pinnedRequestContains(request map[string]any, text string) bool {
	encoded, err := json.Marshal(request)
	if err != nil {
		return false
	}
	return bytes.Contains(encoded, []byte(text))
}

func pinnedFindRequest(
	t *testing.T,
	requests []map[string]any,
	match func(map[string]any) bool,
	description string,
) map[string]any {
	t.Helper()
	var found map[string]any
	for _, request := range requests {
		if !match(request) {
			continue
		}
		if found != nil {
			t.Fatalf("multiple Responses requests match %s", description)
		}
		found = request
	}
	if found == nil {
		t.Fatalf("no Responses request matches %s", description)
	}
	return found
}

func pinnedFunctionCallSSE(responseID, callID, name, arguments string) string {
	return pinnedSSE(
		map[string]any{"type": "response.created", "response": map[string]any{"id": responseID}},
		map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"type": "function_call", "call_id": callID, "name": name, "arguments": arguments,
		}},
		pinnedCompletedEvent(responseID),
	)
}

func pinnedNamespacedFunctionCallSSE(responseID, callID, namespace, name, arguments string) string {
	return pinnedSSE(
		map[string]any{"type": "response.created", "response": map[string]any{"id": responseID}},
		map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"type": "function_call", "call_id": callID, "namespace": namespace, "name": name, "arguments": arguments,
		}},
		pinnedCompletedEvent(responseID),
	)
}

func pinnedAssistantSSE(responseID, messageID, text string) string {
	return pinnedSSE(
		map[string]any{"type": "response.created", "response": map[string]any{"id": responseID}},
		map[string]any{"type": "response.output_item.done", "item": map[string]any{
			"type": "message", "role": "assistant", "id": messageID,
			"content": []any{map[string]any{"type": "output_text", "text": text}},
		}},
		pinnedCompletedEvent(responseID),
	)
}

func pinnedCompletedEvent(responseID string) map[string]any {
	return map[string]any{"type": "response.completed", "response": map[string]any{
		"id": responseID,
		"usage": map[string]any{
			"input_tokens": 0, "input_tokens_details": nil, "output_tokens": 0,
			"output_tokens_details": nil, "total_tokens": 0,
		},
	}}
}

func pinnedSSE(events ...map[string]any) string {
	var output strings.Builder
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			panic(err)
		}
		fmt.Fprintf(&output, "event: %s\ndata: %s\n\n", event["type"], data)
	}
	return output.String()
}

func writePinnedProviderConfig(t *testing.T, codexHome, serverURL string) {
	writePinnedProviderConfigFeatures(t, codexHome, serverURL, false)
}

func writePinnedProviderConfigWithCollab(t *testing.T, codexHome, serverURL string) {
	writePinnedProviderConfigFeatures(t, codexHome, serverURL, true)
}

func writePinnedProviderConfigFeatures(t *testing.T, codexHome, serverURL string, collab bool) {
	t.Helper()
	collabConfig := ""
	if collab {
		collabConfig = "collab = true\n"
	}
	config := fmt.Sprintf(`model = "mock-model"
model_provider = "intercom_test"
approval_policy = "never"
sandbox_mode = "workspace-write"
model_auto_compact_token_limit = 1000000

[features]
enable_request_compression = false
%s

[model_providers.intercom_test]
name = "Intercom loopback test provider"
base_url = %q
wire_api = "responses"
request_max_retries = 0
stream_max_retries = 0
supports_websockets = false
`, collabConfig, serverURL+"/v1")
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
}

type pinnedAppServerProcess struct {
	cmd    *exec.Cmd
	client *appserver.Client
	done   chan error
	stderr bytes.Buffer

	mu      sync.Mutex
	stopped bool
}

type pinnedThreadReadProbe struct {
	appServerClient

	mu    sync.Mutex
	reads []appserver.ThreadReadParams
}

func (p *pinnedThreadReadProbe) ThreadRead(ctx context.Context, params appserver.ThreadReadParams) (appserver.ThreadReadResponse, error) {
	p.mu.Lock()
	p.reads = append(p.reads, params)
	p.mu.Unlock()
	return p.appServerClient.ThreadRead(ctx, params)
}

func (p *pinnedThreadReadProbe) snapshot() []appserver.ThreadReadParams {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]appserver.ThreadReadParams(nil), p.reads...)
}

func startPinnedAppServer(
	t *testing.T,
	codexBin, codexHome, root, name string,
	opts appserver.Options,
) *pinnedAppServerProcess {
	t.Helper()
	socket := filepath.Join(root, "app-server-"+name+".sock")
	endpoint := "unix://" + socket
	cmd := exec.Command(codexBin, "app-server", "--listen", endpoint)
	cmd.Env = pinnedProcessEnv(os.Environ(), map[string]string{
		"CODEX_HOME":                           codexHome,
		"CODEX_APP_SERVER_MANAGED_CONFIG_PATH": filepath.Join(codexHome, "managed_config.toml"),
		"RUST_LOG":                             "warn",
		"NO_PROXY":                             "127.0.0.1,localhost",
		"no_proxy":                             "127.0.0.1,localhost",
	})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	process := &pinnedAppServerProcess{cmd: cmd, done: make(chan error, 1)}
	cmd.Stderr = &process.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		process.done <- cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() { process.stop(t, syscall.SIGKILL, false) })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		client, err := appserver.DialUnix(ctx, endpoint, opts)
		cancel()
		if err == nil {
			process.client = client
			return process
		}
		select {
		case processErr := <-process.done:
			t.Fatalf("codex app-server exited before readiness: %v\n%s", processErr, process.stderr.String())
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	process.stop(t, syscall.SIGKILL, false)
	t.Fatalf("codex app-server did not accept a WebSocket connection\n%s", process.stderr.String())
	return nil
}

func (p *pinnedAppServerProcess) stop(t *testing.T, signal syscall.Signal, closeClient bool) {
	t.Helper()
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	p.mu.Unlock()
	if closeClient && p.client != nil {
		_ = p.client.Close()
	}
	if p.cmd.Process != nil {
		_ = syscall.Kill(-p.cmd.Process.Pid, signal)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			t.Errorf("codex app-server did not stop after SIGKILL\n%s", p.stderr.String())
		}
	}
	if p.client != nil {
		_ = p.client.Close()
	}
}

func pinnedProcessEnv(base []string, overrides map[string]string) []string {
	result := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := overrides[name]; replaced {
			continue
		}
		switch name {
		case "OPENAI_API_KEY", "CODEX_API_KEY",
			"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
			"http_proxy", "https_proxy", "all_proxy":
			continue
		}
		result = append(result, entry)
	}
	for name, value := range overrides {
		result = append(result, name+"="+value)
	}
	return result
}

func initializePinnedClient(t *testing.T, client *appserver.Client) appserver.InitializeResponse {
	t.Helper()
	initialized := callPinned(t, func(ctx context.Context) (appserver.InitializeResponse, error) {
		return client.Initialize(ctx, appserver.InitializeParams{
			ClientInfo:   appserver.ClientInfo{Name: "intercom-pinned-e2e", Version: "test"},
			Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
		})
	})
	if _, err := validateServerVersion(initialized.UserAgent); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := client.Initialized(ctx); err != nil {
		t.Fatal(err)
	}
	return initialized
}

func callPinned[T any](t *testing.T, call func(context.Context) (T, error)) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result, err := call(ctx)
	if err != nil {
		var zero T
		t.Fatalf("Codex app-server call: %v", err)
		return zero
	}
	return result
}

func beginPinnedTurn(
	t *testing.T,
	client *appserver.Client,
	threadID, cwd string,
	sandbox appserver.SandboxPolicy,
	text string,
) appserver.Turn {
	t.Helper()
	response := callPinned(t, func(ctx context.Context) (appserver.TurnStartResponse, error) {
		return client.TurnStart(ctx, appserver.TurnStartParams{
			ThreadID:       threadID,
			Input:          []appserver.UserInput{appserver.TextInput(text)},
			CWD:            &cwd,
			ApprovalPolicy: string(appserver.ApprovalNever),
			SandboxPolicy:  &sandbox,
		})
	})
	if response.Turn.ID == "" || response.Turn.Status != appserver.TurnStatusInProgress {
		t.Fatalf("turn/start response = %#v", response)
	}
	return response.Turn
}

func startPinnedTurn(
	t *testing.T,
	client *appserver.Client,
	notifications <-chan appserver.Notification,
	threadID, cwd string,
	sandbox appserver.SandboxPolicy,
	text string,
) appserver.Turn {
	t.Helper()
	turn := beginPinnedTurn(t, client, threadID, cwd, sandbox, text)
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	started := false
	for {
		select {
		case notification := <-notifications:
			switch notification.Method {
			case appserver.NotificationTurnStarted:
				var params appserver.TurnStartedNotification
				if err := notification.DecodeParams(&params); err != nil {
					t.Fatal(err)
				}
				if params.ThreadID == threadID && params.Turn.ID == turn.ID {
					started = true
				}
			case appserver.NotificationTurnCompleted:
				var params appserver.TurnCompletedNotification
				if err := notification.DecodeParams(&params); err != nil {
					t.Fatal(err)
				}
				if params.ThreadID != threadID || params.Turn.ID != turn.ID {
					continue
				}
				if !started {
					t.Fatalf("turn %s completed without a matching turn/started notification", turn.ID)
				}
				if params.Turn.Status != appserver.TurnStatusCompleted {
					t.Fatalf("turn %s status = %q, want completed; error=%#v", turn.ID, params.Turn.Status, params.Turn.Error)
				}
				return params.Turn
			case appserver.NotificationError:
				var params appserver.ErrorNotification
				if err := notification.DecodeParams(&params); err != nil {
					t.Fatal(err)
				}
				if params.ThreadID == threadID && params.TurnID == turn.ID {
					t.Fatalf("app-server turn error: retry=%v error=%s", params.WillRetry, params.Error.Message)
				}
			}
		case <-deadline.C:
			t.Fatalf("timeout waiting for turn %s completion", turn.ID)
		}
	}
}

func readPinnedThread(t *testing.T, client *appserver.Client, threadID string) appserver.Thread {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		response, err := client.ThreadRead(ctx, appserver.ThreadReadParams{ThreadID: threadID, IncludeTurns: true})
		cancel()
		if err == nil {
			return response.Thread
		}
		if !isBeforeFirstUserMessage(err, threadID) || time.Now().After(deadline) {
			t.Fatalf("thread/read materialization: %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func waitPinnedTurnStatus(
	t *testing.T,
	client *appserver.Client,
	threadID, turnID, status string,
) appserver.Thread {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		response, err := client.ThreadRead(ctx, appserver.ThreadReadParams{ThreadID: threadID, IncludeTurns: true})
		cancel()
		if err == nil {
			for _, turn := range response.Thread.Turns {
				if turn.ID == turnID && turn.Status == status {
					return response.Thread
				}
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("wait for thread %s turn %s status %s: %v", threadID, turnID, status, err)
			}
			t.Fatalf("thread %s turn %s did not reach status %s: %#v", threadID, turnID, status, response.Thread.Turns)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func pinnedThreadHasAncestor(thread appserver.Thread, ancestor string) bool {
	return thread.ParentThreadID != nil && *thread.ParentThreadID == ancestor ||
		thread.ForkedFromID != nil && *thread.ForkedFromID == ancestor
}

func assertPinnedThread(
	t *testing.T,
	thread appserver.Thread,
	responseCWD string,
	approval any,
	sandbox appserver.SandboxPolicy,
	wantCWD string,
) {
	t.Helper()
	if thread.ID == "" || thread.CWD != wantCWD || responseCWD != wantCWD || thread.Ephemeral {
		t.Fatalf("thread response = thread:%#v cwd:%q", thread, responseCWD)
	}
	if thread.Status.Type != appserver.ThreadStatusIdle {
		t.Fatalf("thread status = %q, want idle", thread.Status.Type)
	}
	if approval != string(appserver.ApprovalNever) || sandbox.Type != "workspaceWrite" {
		t.Fatalf("thread policy = approval:%#v sandbox:%#v", approval, sandbox)
	}
}

func assertPinnedTurnStatus(t *testing.T, thread appserver.Thread, turnID, status string) {
	t.Helper()
	for _, turn := range thread.Turns {
		if turn.ID == turnID {
			if turn.Status != status {
				t.Fatalf("turn %s status = %q, want %q", turnID, turn.Status, status)
			}
			return
		}
	}
	t.Fatalf("thread %s does not contain turn %s: %#v", thread.ID, turnID, thread.Turns)
}

type pinnedE2EBroker struct {
	mu    sync.Mutex
	peers []string
	lists int
}

func (b *pinnedE2EBroker) Send(context.Context, string, string) (wire.SendAck, error) {
	return wire.SendAck{}, errors.New("send_message is not used by the pinned E2E")
}

func (b *pinnedE2EBroker) ListPeers(context.Context) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lists++
	return append([]string(nil), b.peers...), nil
}

func (b *pinnedE2EBroker) listCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lists
}

type pinnedReverseProbe struct {
	handler  reverseHandler
	holdCall string
	calls    chan appserver.DynamicToolCallParams
	fatal    chan error
}

func newPinnedReverseProbe(broker brokerTools, holdCall string) *pinnedReverseProbe {
	probe := &pinnedReverseProbe{
		holdCall: holdCall,
		calls:    make(chan appserver.DynamicToolCallParams, 16),
		fatal:    make(chan error, 16),
	}
	probe.handler = reverseHandler{
		broker:    broker,
		authorize: func(context.Context, string, string) error { return nil },
		onFatal: func(err error) {
			probe.fatal <- err
		},
		timeout: 5 * time.Second,
	}
	return probe
}

func (p *pinnedReverseProbe) options(notifications chan<- appserver.Notification) appserver.Options {
	return appserver.Options{
		OnNotification: func(notification appserver.Notification) {
			notifications <- notification
		},
		OnReverseRequest: func(request *appserver.ReverseRequest) {
			if request.Method == appserver.MethodDynamicToolCall {
				var params appserver.DynamicToolCallParams
				if err := request.DecodeParams(&params); err == nil {
					p.calls <- params
					if params.CallID == p.holdCall {
						return
					}
				}
			}
			p.handler.Handle(request)
		},
	}
}

func (p *pinnedReverseProbe) nextCall(t *testing.T) appserver.DynamicToolCallParams {
	t.Helper()
	select {
	case params := <-p.calls:
		return params
	case err := <-p.fatal:
		t.Fatalf("reverse handler failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for real dynamic tool call")
	}
	return appserver.DynamicToolCallParams{}
}

func (p *pinnedReverseProbe) assertNoCall(t *testing.T, duration time.Duration) {
	t.Helper()
	select {
	case params := <-p.calls:
		t.Fatalf("app-server replayed reverse request after cold resume: %#v", params)
	case err := <-p.fatal:
		t.Fatalf("reverse handler failed: %v", err)
	case <-time.After(duration):
	}
}

func (p *pinnedReverseProbe) assertNoFatal(t *testing.T) {
	t.Helper()
	select {
	case err := <-p.fatal:
		t.Fatalf("reverse handler failed: %v", err)
	default:
	}
}

func assertPinnedDynamicCall(
	t *testing.T,
	params appserver.DynamicToolCallParams,
	threadID, turnID, callID string,
) {
	t.Helper()
	if params.ThreadID != threadID || params.TurnID != turnID || params.CallID != callID ||
		params.Tool != intercomtools.ListPeersName || params.Namespace != nil {
		t.Fatalf("dynamic tool call = %#v", params)
	}
	if string(params.Arguments) != `{}` {
		t.Fatalf("dynamic tool arguments = %s, want {}", params.Arguments)
	}
}

func assertPinnedToolAdvertised(t *testing.T, request map[string]any, name string) {
	t.Helper()
	tools, ok := request["tools"].([]any)
	if !ok {
		t.Fatalf("Responses request tools = %#v", request["tools"])
	}
	for _, value := range tools {
		tool, _ := value.(map[string]any)
		if tool["name"] == name {
			return
		}
	}
	t.Fatalf("Responses request did not advertise %q: %#v", name, tools)
}

func assertPinnedToolOutput(t *testing.T, request map[string]any, callID, wantText string) {
	t.Helper()
	input, ok := request["input"].([]any)
	if !ok {
		t.Fatalf("Responses request input = %#v", request["input"])
	}
	for _, value := range input {
		item, _ := value.(map[string]any)
		if item["type"] != "function_call_output" || item["call_id"] != callID {
			continue
		}
		encoded, err := json.Marshal(item["output"])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(encoded, []byte(wantText)) {
			t.Fatalf("tool output for %s = %s, want text %q", callID, encoded, wantText)
		}
		return
	}
	t.Fatalf("Responses request did not contain function_call_output for %q: %#v", callID, input)
}
