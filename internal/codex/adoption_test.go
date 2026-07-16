package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/wire"
)

func prepareInteractiveSession(t *testing.T, h *controllerHarness, threadID string) {
	t.Helper()
	bridgeDir, err := os.MkdirTemp("", "intercom-bridge-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(bridgeDir) })
	if err := os.Chmod(bridgeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	thread := appserver.Thread{
		ID:        threadID,
		CWD:       h.cfg.CWD,
		Source:    json.RawMessage(`"cli"`),
		Status:    appserver.ThreadStatus{Type: appserver.ThreadStatusIdle},
		CreatedAt: 1,
		UpdatedAt: 2,
	}
	h.app.threadReadResponses = map[string]appserver.ThreadReadResponse{
		threadID: {Thread: thread},
	}
	h.app.threadReadErrors = make(map[string]error)
	h.app.list = appserver.ThreadListResponse{Data: []appserver.Thread{thread}}
	h.app.resume.Thread = thread
	h.app.resume.CWD = h.cfg.CWD
	h.app.resume.RuntimeWorkspaceRoots = []string{h.cfg.CWD}
	h.app.resume.ApprovalPolicy = string(appserver.ApprovalNever)
	h.app.resume.Sandbox = appserver.SandboxPolicy{Type: "workspaceWrite", NetworkAccess: false}
	h.app.mcpStatus = appserver.MCPServerStatusListResponse{Data: []appserver.MCPServerStatus{{
		Name: managedMCPServerName,
		Tools: map[string]appserver.MCPTool{
			"send_message": {Name: "send_message"},
			"list_peers":   {Name: "list_peers"},
		},
	}}}
	h.cfg.MCPBridgeSocket = filepath.Join(bridgeDir, "mcp-bridge.sock")
	h.cfg.IntercomBin = filepath.Join(h.cfg.CWD, "intercom-test")
}

func assertManagedMCPConfig(t *testing.T, config map[string]any, socket, executable string, timeout time.Duration) {
	t.Helper()
	raw, ok := config["mcp_servers."+managedMCPServerName]
	if !ok {
		t.Fatalf("managed MCP config is absent: %#v", config)
	}
	server, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("managed MCP config type = %T", raw)
	}
	if server["command"] != executable || server["required"] != true || server["supports_parallel_tool_calls"] != true {
		t.Fatalf("managed MCP server config = %#v", server)
	}
	args, ok := server["args"].([]string)
	if !ok || len(args) != 5 || args[0] != "codex-mcp-bridge" || args[1] != "--socket" || args[2] != socket ||
		args[3] != "--timeout" || args[4] != timeout.String() {
		t.Fatalf("managed MCP args = %#v", server["args"])
	}
	wantToolSeconds := int(timeout.Round(time.Second) / time.Second)
	if wantToolSeconds < 1 {
		wantToolSeconds = 1
	}
	if server["tool_timeout_sec"] != wantToolSeconds {
		t.Fatalf("managed MCP tool timeout = %#v, want %d", server["tool_timeout_sec"], wantToolSeconds)
	}
	env, ok := server["env"].(map[string]string)
	if !ok || len(env[bridgeTokenEnvironment]) < 64 {
		t.Fatalf("managed MCP token environment = %#v", server["env"])
	}
}

func TestControllerAdoptsInteractiveSessionTransactionally(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "ordinary-thread")
	h.cfg.AdoptThreadID = "ordinary-thread"

	cancel, done := runHarness(t, h)
	waitFor(t, "adopted binding commit", func() bool {
		_, err := os.Stat(h.cfg.StatePath)
		return err == nil
	})
	state, err := readManagedState(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.ThreadID != "ordinary-thread" || state.ToolTransport != ToolTransportMCPBridge || !state.Materialized {
		t.Fatalf("adopted state = %#v", state)
	}
	data, err := os.ReadFile(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), bridgeTokenEnvironment) || strings.Contains(string(data), h.cfg.MCPBridgeSocket) {
		t.Fatalf("ephemeral bridge credentials leaked into binding state: %s", data)
	}

	h.app.mu.Lock()
	resumes := append([]appserver.ThreadResumeParams(nil), h.app.threadResumes...)
	statuses := append([]appserver.MCPServerStatusListParams(nil), h.app.mcpStatusLists...)
	h.app.mu.Unlock()
	if len(resumes) != 1 || resumes[0].ThreadID != "ordinary-thread" || !resumes[0].ExcludeTurns ||
		resumes[0].ApprovalsReviewer == nil || *resumes[0].ApprovalsReviewer != appserver.ApprovalsReviewerUser {
		t.Fatalf("thread/resume calls = %#v", resumes)
	}
	assertManagedMCPConfig(t, resumes[0].Config, h.cfg.MCPBridgeSocket, h.cfg.IntercomBin, h.cfg.ReverseTimeout)
	if len(statuses) != 1 || statuses[0].ThreadID == nil || *statuses[0].ThreadID != "ordinary-thread" ||
		statuses[0].Detail == nil || *statuses[0].Detail != appserver.MCPServerStatusToolsAndAuthOnly {
		t.Fatalf("MCP status checks = %#v", statuses)
	}
	if info, err := os.Stat(h.cfg.MCPBridgeSocket); err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("live MCP bridge socket = %#v, %v", info, err)
	}

	stopHarness(t, cancel, done)
	if _, err := os.Lstat(h.cfg.MCPBridgeSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("MCP bridge socket remains after shutdown: %v", err)
	}
}

func TestControllerAdoptsActiveGoalLifecycleDuringResume(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "ordinary-thread")
	h.cfg.AdoptThreadID = "ordinary-thread"
	h.app.turnStart = appserver.TurnStartResponse{
		Turn: appserver.Turn{ID: "delivery-turn", Status: appserver.TurnStatusInProgress},
	}
	h.app.goalGet.Goal = &appserver.ThreadGoal{ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusActive}
	h.app.resumeHook = func(appserver.ThreadResumeParams) {
		for _, notification := range []appserver.Notification{
			{
				Method: appserver.NotificationThreadGoalUpdated,
				Params: mustJSONBytes(appserver.ThreadGoalUpdatedNotification{
					ThreadID: "ordinary-thread",
					Goal: appserver.ThreadGoal{
						ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusActive,
					},
				}),
			},
			{
				Method: appserver.NotificationTurnStarted,
				Params: mustJSONBytes(appserver.TurnStartedNotification{
					ThreadID: "ordinary-thread",
					Turn:     appserver.Turn{ID: "goal-turn-1", Status: appserver.TurnStatusInProgress},
				}),
			},
			{
				Method: appserver.NotificationTurnCompleted,
				Params: mustJSONBytes(appserver.TurnCompletedNotification{
					ThreadID: "ordinary-thread",
					Turn:     appserver.Turn{ID: "goal-turn-1", Status: appserver.TurnStatusCompleted},
				}),
			},
			{
				Method: appserver.NotificationTurnStarted,
				Params: mustJSONBytes(appserver.TurnStartedNotification{
					ThreadID: "ordinary-thread",
					Turn:     appserver.Turn{ID: "goal-turn-2", Status: appserver.TurnStatusInProgress},
				}),
			},
		} {
			h.app.opts.OnNotification(notification)
		}
	}

	cancel, done := runHarness(t, h)
	h.broker.opts.OnDeliver(wire.Deliver{
		ID: "queued-delivery", From: "alice", Message: "wait for the goal",
		Timestamp: "2026-07-15T18:00:00Z",
	})
	time.Sleep(25 * time.Millisecond)
	if got := turnStartCount(h.app); got != 0 {
		t.Fatalf("broker delivery overtook Codex-owned goal turn: %d starts", got)
	}

	h.app.opts.OnNotification(appserver.Notification{
		Method: appserver.NotificationThreadGoalUpdated,
		Params: mustJSON(t, appserver.ThreadGoalUpdatedNotification{
			ThreadID: "ordinary-thread",
			Goal: appserver.ThreadGoal{
				ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusComplete,
			},
		}),
	})
	h.app.opts.OnNotification(appserver.Notification{
		Method: appserver.NotificationTurnCompleted,
		Params: mustJSON(t, appserver.TurnCompletedNotification{
			ThreadID: "ordinary-thread",
			Turn:     appserver.Turn{ID: "goal-turn-2", Status: appserver.TurnStatusCompleted},
		}),
	})
	waitFor(t, "queued delivery after automatic goal turns", func() bool {
		return turnStartCount(h.app) == 1
	})
	h.app.opts.OnNotification(appserver.Notification{
		Method: appserver.NotificationTurnCompleted,
		Params: mustJSON(t, appserver.TurnCompletedNotification{
			ThreadID: "ordinary-thread",
			Turn:     appserver.Turn{ID: "delivery-turn", Status: appserver.TurnStatusCompleted},
		}),
	})
	stopHarness(t, cancel, done)
}

func TestControllerQueuesDeliveryAcrossPersistentGoalContinuationGap(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "ordinary-thread")
	h.cfg.AdoptThreadID = "ordinary-thread"
	h.app.turnStart = appserver.TurnStartResponse{
		Turn: appserver.Turn{ID: "delivery-turn", Status: appserver.TurnStatusInProgress},
	}
	h.app.goalGet.Goal = &appserver.ThreadGoal{ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusActive}

	cancel, done := runHarness(t, h)
	h.broker.opts.OnDeliver(wire.Deliver{
		ID: "queued-delivery", From: "alice", Message: "wait across the scheduler gap",
		Timestamp: "2026-07-15T18:00:00Z",
	})
	time.Sleep(25 * time.Millisecond)
	if got := turnStartCount(h.app); got != 0 {
		t.Fatalf("broker delivery occupied persistent-goal continuation gap: %d starts", got)
	}

	for _, notification := range []appserver.Notification{
		{
			Method: appserver.NotificationThreadGoalUpdated,
			Params: mustJSONBytes(appserver.ThreadGoalUpdatedNotification{
				ThreadID: "ordinary-thread",
				Goal: appserver.ThreadGoal{
					ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusActive,
				},
			}),
		},
		{
			Method: appserver.NotificationTurnStarted,
			Params: mustJSONBytes(appserver.TurnStartedNotification{
				ThreadID: "ordinary-thread",
				Turn:     appserver.Turn{ID: "goal-turn", Status: appserver.TurnStatusInProgress},
			}),
		},
		{
			Method: appserver.NotificationThreadGoalUpdated,
			Params: mustJSONBytes(appserver.ThreadGoalUpdatedNotification{
				ThreadID: "ordinary-thread",
				Goal: appserver.ThreadGoal{
					ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusComplete,
				},
			}),
		},
		{
			Method: appserver.NotificationTurnCompleted,
			Params: mustJSONBytes(appserver.TurnCompletedNotification{
				ThreadID: "ordinary-thread",
				Turn:     appserver.Turn{ID: "goal-turn", Status: appserver.TurnStatusCompleted},
			}),
		},
	} {
		h.app.opts.OnNotification(notification)
	}
	waitFor(t, "delivery after persistent goal released scheduler ownership", func() bool {
		return turnStartCount(h.app) == 1
	})
	h.app.opts.OnNotification(appserver.Notification{
		Method: appserver.NotificationTurnCompleted,
		Params: mustJSONBytes(appserver.TurnCompletedNotification{
			ThreadID: "ordinary-thread",
			Turn:     appserver.Turn{ID: "delivery-turn", Status: appserver.TurnStatusCompleted},
		}),
	})
	stopHarness(t, cancel, done)
}

func TestControllerForksInteractiveSessionWithoutOwningSource(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "source-thread")
	h.cfg.ForkThreadID = "source-thread"
	forkedFrom := "source-thread"
	h.app.fork.ThreadResponse = h.app.resume.ThreadResponse
	h.app.fork.Thread.ID = "managed-fork"
	h.app.fork.Thread.ForkedFromID = &forkedFrom

	cancel, done := runHarness(t, h)
	waitFor(t, "forked binding commit", func() bool {
		_, err := os.Stat(h.cfg.StatePath)
		return err == nil
	})
	state, err := readManagedState(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.ThreadID != "managed-fork" || state.ToolTransport != ToolTransportMCPBridge || !state.Materialized {
		t.Fatalf("forked state = %#v", state)
	}
	h.app.mu.Lock()
	forks := append([]appserver.ThreadForkParams(nil), h.app.threadForks...)
	h.app.mu.Unlock()
	if len(forks) != 1 || forks[0].ThreadID != "source-thread" || forks[0].Ephemeral || !forks[0].ExcludeTurns ||
		forks[0].ApprovalsReviewer == nil || *forks[0].ApprovalsReviewer != appserver.ApprovalsReviewerUser {
		t.Fatalf("thread/fork calls = %#v", forks)
	}
	assertManagedMCPConfig(t, forks[0].Config, h.cfg.MCPBridgeSocket, h.cfg.IntercomBin, h.cfg.ReverseTimeout)

	sourcePath, err := h.cfg.threadLockPath(h.app.init.CodexHome, "source-thread")
	if err != nil {
		t.Fatal(err)
	}
	sourceLock, err := AcquireThreadLock(sourcePath)
	if err != nil {
		t.Fatalf("source thread remains locked after fork: %v", err)
	}
	_ = sourceLock.Close()
	managedPath, err := h.cfg.threadLockPath(h.app.init.CodexHome, "managed-fork")
	if err != nil {
		t.Fatal(err)
	}
	if lock, err := AcquireThreadLock(managedPath); err == nil {
		_ = lock.Close()
		t.Fatal("forked managed thread was not locked")
	}
	stopHarness(t, cancel, done)
}

func TestControllerFailedAdoptionPreservesExistingBinding(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "replacement-thread")
	old := validState()
	old.CWD = h.cfg.CWD
	old.CodexHome = h.app.init.CodexHome
	old.ServerUserAgent = h.app.init.UserAgent
	old.ThreadID = "old-thread"
	old.Materialized = true
	writeManagedState(t, h.cfg, old)
	h.cfg.AdoptThreadID = "replacement-thread"
	h.cfg.ReplaceBinding = true
	h.app.mcpStatus = appserver.MCPServerStatusListResponse{}

	err := Run(t.Context(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "managed MCP server") {
		t.Fatalf("Run() error = %v", err)
	}
	got, readErr := readManagedState(h.cfg.StatePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got != old {
		t.Fatalf("failed adoption changed binding: got %#v, want %#v", got, old)
	}
	lockPath, err := h.cfg.threadLockPath(h.app.init.CodexHome, "replacement-thread")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireThreadLock(lockPath)
	if err != nil {
		t.Fatalf("failed adoption leaked thread lock: %v", err)
	}
	_ = lock.Close()
}

func TestControllerReplacementRetainsPriorThreadLockThroughRollbackWindow(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "replacement-thread")
	old := validState()
	old.CWD = h.cfg.CWD
	old.CodexHome = h.app.init.CodexHome
	old.ServerUserAgent = h.app.init.UserAgent
	old.ThreadID = "old-thread"
	old.Materialized = true
	writeManagedState(t, h.cfg, old)
	h.cfg.AdoptThreadID = "replacement-thread"
	h.cfg.ReplaceBinding = true
	oldLockPath, err := h.cfg.threadLockPath(old.CodexHome, old.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	h.cfg.OnReady = func(ReadyInfo) error {
		checked = true
		lock, lockErr := AcquireThreadLock(oldLockPath)
		if lockErr == nil {
			_ = lock.Close()
			return errors.New("prior thread lock was released before replacement commit")
		}
		if !strings.Contains(lockErr.Error(), "already managed") {
			return fmt.Errorf("inspect prior thread lock: %w", lockErr)
		}
		return errors.New("readiness publication failed")
	}

	err = Run(t.Context(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "publish readiness: readiness publication failed") {
		t.Fatalf("Run() error = %v", err)
	}
	if !checked {
		t.Fatal("readiness hook did not inspect prior thread lock")
	}
	got, readErr := readManagedState(h.cfg.StatePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got != old {
		t.Fatalf("failed replacement changed binding: got %#v, want %#v", got, old)
	}
	lock, lockErr := AcquireThreadLock(oldLockPath)
	if lockErr != nil {
		t.Fatalf("prior thread lock remained held after rollback: %v", lockErr)
	}
	_ = lock.Close()
}

func TestControllerAdoptionCommitWaitsForBrokerAndProxyReadiness(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*controllerHarness)
		wantError string
	}{
		{
			name: "broker registration",
			configure: func(h *controllerHarness) {
				h.cfg.newBroker = func(opts brokerclient.ClientOptions) brokerConnection {
					broker := newFakeBroker(opts)
					broker.connectErr = errors.New("broker registration failed")
					return broker
				}
			},
			wantError: "register with broker",
		},
		{
			name: "TUI proxy",
			configure: func(h *controllerHarness) {
				h.cfg.ClientEndpoint = "unix://" + filepath.Join(h.cfg.CWD, "client.sock")
			},
			wantError: "does not expose raw proxy calls",
		},
		{
			name: "readiness publication",
			configure: func(h *controllerHarness) {
				h.cfg.OnReady = func(ReadyInfo) error { return errors.New("descriptor unavailable") }
			},
			wantError: "publish readiness",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newControllerHarness(t)
			prepareInteractiveSession(t, h, "replacement-thread")
			old := validState()
			old.CWD = h.cfg.CWD
			old.CodexHome = h.app.init.CodexHome
			old.ServerUserAgent = h.app.init.UserAgent
			old.ThreadID = "old-thread"
			old.Materialized = true
			writeManagedState(t, h.cfg, old)
			h.cfg.AdoptThreadID = "replacement-thread"
			h.cfg.ReplaceBinding = true
			test.configure(h)

			err := Run(t.Context(), h.cfg)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Run() error = %v, want fragment %q", err, test.wantError)
			}
			got, readErr := readManagedState(h.cfg.StatePath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if got != old {
				t.Fatalf("failed startup changed binding: got %#v, want %#v", got, old)
			}
		})
	}
}

func TestControllerStartupFailureInterruptsResumedGoalTurn(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "ordinary-thread")
	h.cfg.AdoptThreadID = "ordinary-thread"
	h.app.mcpStatusErr = errors.New("managed MCP status unavailable")
	h.app.goalGet.Goal = &appserver.ThreadGoal{ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusActive}
	h.app.resumeHook = func(appserver.ThreadResumeParams) {
		for _, notification := range []appserver.Notification{
			{
				Method: appserver.NotificationThreadGoalUpdated,
				Params: mustJSONBytes(appserver.ThreadGoalUpdatedNotification{
					ThreadID: "ordinary-thread",
					Goal: appserver.ThreadGoal{
						ThreadID: "ordinary-thread", Status: appserver.ThreadGoalStatusActive,
					},
				}),
			},
			{
				Method: appserver.NotificationTurnStarted,
				Params: mustJSONBytes(appserver.TurnStartedNotification{
					ThreadID: "ordinary-thread",
					Turn:     appserver.Turn{ID: "goal-turn", Status: appserver.TurnStatusInProgress},
				}),
			},
		} {
			h.app.opts.OnNotification(notification)
		}
	}

	started := time.Now()
	err := Run(t.Context(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "verify managed MCP server") {
		t.Fatalf("Run() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed >= h.cfg.ControlTimeout {
		t.Fatalf("startup cleanup consumed the full control timeout: %v", elapsed)
	}
	h.app.mu.Lock()
	interrupts := append([]appserver.TurnInterruptParams(nil), h.app.interrupts...)
	h.app.mu.Unlock()
	if len(interrupts) != 1 || interrupts[0].ThreadID != "ordinary-thread" || interrupts[0].TurnID != "goal-turn" {
		t.Fatalf("startup-failure turn interrupts = %#v", interrupts)
	}
	if _, statErr := os.Stat(h.cfg.StatePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed adoption persisted a binding: %v", statErr)
	}
	broker := <-h.brokerReady
	select {
	case <-broker.closed:
	default:
		t.Fatal("startup failure left broker client open")
	}
}

func TestControllerRefusesBindingReplacementWithoutExplicitFlag(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "replacement-thread")
	old := validState()
	old.CWD = h.cfg.CWD
	old.CodexHome = h.app.init.CodexHome
	old.ServerUserAgent = h.app.init.UserAgent
	old.ThreadID = "old-thread"
	old.Materialized = true
	writeManagedState(t, h.cfg, old)
	h.cfg.AdoptThreadID = "replacement-thread"

	err := Run(t.Context(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "--replace-binding") {
		t.Fatalf("Run() error = %v", err)
	}
	h.app.mu.Lock()
	initializations := len(h.app.initializeParams)
	h.app.mu.Unlock()
	if initializations != 0 {
		t.Fatalf("app-server was touched before replacement authorization: %d initializes", initializations)
	}
	got, readErr := readManagedState(h.cfg.StatePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got != old {
		t.Fatalf("refused replacement changed binding: got %#v, want %#v", got, old)
	}
}

func TestControllerReinjectsManagedMCPOnColdResume(t *testing.T) {
	h := newControllerHarness(t)
	prepareInteractiveSession(t, h, "adopted-thread")
	h.cfg.ReverseTimeout = 17*time.Second + 250*time.Millisecond
	state := validState()
	state.CWD = h.cfg.CWD
	state.CodexHome = h.app.init.CodexHome
	state.ServerUserAgent = h.app.init.UserAgent
	state.ThreadID = "adopted-thread"
	state.Materialized = true
	state.ToolTransport = ToolTransportMCPBridge
	writeManagedState(t, h.cfg, state)

	cancel, done := runHarness(t, h)
	h.app.mu.Lock()
	resumes := append([]appserver.ThreadResumeParams(nil), h.app.threadResumes...)
	statuses := len(h.app.mcpStatusLists)
	h.app.mu.Unlock()
	if len(resumes) != 1 || statuses != 1 {
		t.Fatalf("cold resume calls: resumes=%#v status-count=%d", resumes, statuses)
	}
	assertManagedMCPConfig(t, resumes[0].Config, h.cfg.MCPBridgeSocket, h.cfg.IntercomBin, h.cfg.ReverseTimeout)
	stopHarness(t, cancel, done)
}

func TestControllerYoloPinsDangerFullAccess(t *testing.T) {
	h := newControllerHarness(t)
	h.cfg.ExecutionPolicy = ExecutionDangerFullAccess
	h.app.start.Sandbox = appserver.SandboxPolicy{Type: "dangerFullAccess"}

	cancel, done := runHarness(t, h)
	h.app.mu.Lock()
	starts := append([]appserver.ThreadStartParams(nil), h.app.threadStarts...)
	h.app.mu.Unlock()
	if len(starts) != 1 || starts[0].Sandbox == nil || *starts[0].Sandbox != appserver.SandboxDangerFullAccess ||
		starts[0].ApprovalPolicy != string(appserver.ApprovalNever) {
		t.Fatalf("yolo thread/start = %#v", starts)
	}
	stopHarness(t, cancel, done)
}

func TestManagedMCPVerificationPaginatesAndRejectsCursorCycles(t *testing.T) {
	t.Parallel()
	tools := map[string]appserver.MCPTool{
		"send_message": {Name: "send_message"},
		"list_peers":   {Name: "list_peers"},
	}
	first, second, cycle := "first", "second", "first"
	app := newFakeApp(t.TempDir())
	app.mcpStatusResponses = []appserver.MCPServerStatusListResponse{
		{Data: []appserver.MCPServerStatus{{Name: "other"}}, NextCursor: &first},
		{Data: []appserver.MCPServerStatus{{Name: managedMCPServerName, Tools: tools}}},
	}
	c := &controller{cfg: Config{ControlTimeout: defaultControlTimeout}, app: app, threadID: "thread"}
	if err := c.verifyManagedMCP(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(app.mcpStatusLists) != 2 || app.mcpStatusLists[1].Cursor == nil || *app.mcpStatusLists[1].Cursor != first {
		t.Fatalf("MCP pagination calls = %#v", app.mcpStatusLists)
	}

	app.mcpStatusResponses = []appserver.MCPServerStatusListResponse{
		{NextCursor: &first},
		{NextCursor: &second},
		{NextCursor: &cycle},
	}
	if err := c.verifyManagedMCP(t.Context()); err == nil || !strings.Contains(err.Error(), "repeated a cursor") {
		t.Fatalf("cursor-cycle error = %v", err)
	}
}
