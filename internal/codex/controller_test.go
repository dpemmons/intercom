package codex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/wire"
)

type fakeAppServer struct {
	mu sync.Mutex

	opts               appserver.Options
	init               appserver.InitializeResponse
	start              appserver.ThreadStartResponse
	startErr           error
	resume             appserver.ThreadResumeResponse
	resumeErr          error
	readErr            error
	turnStart          appserver.TurnStartResponse
	turnStartResponses []appserver.TurnStartResponse
	turnStartHook      func(context.Context, appserver.TurnStartParams) error
	interruptHook      func(appserver.TurnInterruptParams) error
	waitHandlersHook   func(context.Context) error

	initializeParams []appserver.InitializeParams
	threadStarts     []appserver.ThreadStartParams
	threadResumes    []appserver.ThreadResumeParams
	threadReads      []appserver.ThreadReadParams
	turnStarts       []appserver.TurnStartParams
	interrupts       []appserver.TurnInterruptParams
	done             chan struct{}
	closeOnce        sync.Once
}

func newFakeApp(cwd string) *fakeAppServer {
	thread := appserver.Thread{ID: "thread-1", CWD: cwd, Status: appserver.ThreadStatus{Type: appserver.ThreadStatusIdle}}
	var start appserver.ThreadStartResponse
	start.Thread = thread
	start.CWD = cwd
	start.ApprovalPolicy = "never"
	start.Sandbox = appserver.SandboxPolicy{Type: "workspaceWrite", NetworkAccess: false}
	var resume appserver.ThreadResumeResponse
	resume.Thread = thread
	resume.CWD = cwd
	resume.ApprovalPolicy = "never"
	resume.Sandbox = appserver.SandboxPolicy{Type: "workspaceWrite", NetworkAccess: false}
	return &fakeAppServer{
		init: appserver.InitializeResponse{
			UserAgent: "codex_cli_rs/0.144.1", CodexHome: filepath.Join(cwd, "codex-home"), PlatformFamily: "unix", PlatformOS: "linux",
		},
		start:     start,
		resume:    resume,
		turnStart: appserver.TurnStartResponse{Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInProgress}},
		done:      make(chan struct{}),
	}
}

func (f *fakeAppServer) Initialize(_ context.Context, params appserver.InitializeParams) (appserver.InitializeResponse, error) {
	f.mu.Lock()
	f.initializeParams = append(f.initializeParams, params)
	f.mu.Unlock()
	return f.init, nil
}
func (f *fakeAppServer) Initialized(context.Context) error { return nil }
func (f *fakeAppServer) ThreadStart(_ context.Context, params appserver.ThreadStartParams) (appserver.ThreadStartResponse, error) {
	f.mu.Lock()
	f.threadStarts = append(f.threadStarts, params)
	f.mu.Unlock()
	return f.start, f.startErr
}
func (f *fakeAppServer) ThreadResume(_ context.Context, params appserver.ThreadResumeParams) (appserver.ThreadResumeResponse, error) {
	f.mu.Lock()
	f.threadResumes = append(f.threadResumes, params)
	f.mu.Unlock()
	return f.resume, f.resumeErr
}
func (f *fakeAppServer) ThreadRead(_ context.Context, params appserver.ThreadReadParams) (appserver.ThreadReadResponse, error) {
	f.mu.Lock()
	f.threadReads = append(f.threadReads, params)
	f.mu.Unlock()
	return appserver.ThreadReadResponse{Thread: f.start.Thread}, f.readErr
}
func (f *fakeAppServer) StartTurn(_ context.Context, params appserver.TurnStartParams) (appserver.TurnStartAwait, error) {
	f.mu.Lock()
	f.turnStarts = append(f.turnStarts, params)
	index := len(f.turnStarts) - 1
	hook := f.turnStartHook
	response := f.turnStart
	if index < len(f.turnStartResponses) {
		response = f.turnStartResponses[index]
	}
	f.mu.Unlock()
	return func(ctx context.Context) (appserver.TurnStartResponse, error) {
		if hook != nil {
			if err := hook(ctx, params); err != nil {
				return appserver.TurnStartResponse{}, err
			}
		}
		return response, nil
	}, nil
}

func (f *fakeAppServer) TurnStart(ctx context.Context, params appserver.TurnStartParams) (appserver.TurnStartResponse, error) {
	await, err := f.StartTurn(ctx, params)
	if err != nil {
		return appserver.TurnStartResponse{}, err
	}
	return await(ctx)
}
func (f *fakeAppServer) TurnInterrupt(_ context.Context, params appserver.TurnInterruptParams) error {
	f.mu.Lock()
	f.interrupts = append(f.interrupts, params)
	hook := f.interruptHook
	notify := f.opts.OnNotification
	turnID := params.TurnID
	if turnID == "" {
		turnID = f.turnStart.Turn.ID
	}
	f.mu.Unlock()
	if hook != nil {
		return hook(params)
	}
	if notify != nil && turnID != "" {
		notify(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSONBytes(appserver.TurnCompletedNotification{
			ThreadID: params.ThreadID, Turn: appserver.Turn{ID: turnID, Status: appserver.TurnStatusInterrupted},
		})})
	}
	return nil
}
func (f *fakeAppServer) Done() <-chan struct{} { return f.done }
func (f *fakeAppServer) Wait() error           { <-f.done; return nil }
func (f *fakeAppServer) WaitHandlers(ctx context.Context) error {
	f.mu.Lock()
	hook := f.waitHandlersHook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx)
	}
	return nil
}
func (f *fakeAppServer) Close() error {
	f.closeOnce.Do(func() { close(f.done) })
	return nil
}

type fakeBroker struct {
	mu                 sync.Mutex
	opts               brokerclient.ClientOptions
	events             chan brokerclient.ConnectionEvent
	connected          chan struct{}
	closed             chan struct{}
	connects           int
	connectHook        func(int)
	connectContextHook func(context.Context, int)
	closeHook          func()
	connOnce           sync.Once
	closeOnce          sync.Once
}

func newFakeBroker(opts brokerclient.ClientOptions) *fakeBroker {
	return &fakeBroker{
		opts: opts, events: make(chan brokerclient.ConnectionEvent, 8), connected: make(chan struct{}), closed: make(chan struct{}),
	}
}

func (b *fakeBroker) Connect(ctx context.Context) error {
	b.mu.Lock()
	b.connects++
	connects := b.connects
	hook := b.connectHook
	contextHook := b.connectContextHook
	b.mu.Unlock()
	if hook != nil {
		hook(connects)
	}
	if contextHook != nil {
		contextHook(ctx, connects)
	}
	b.connOnce.Do(func() { close(b.connected) })
	return nil
}
func (b *fakeBroker) Close() error {
	b.mu.Lock()
	hook := b.closeHook
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}
func (b *fakeBroker) Send(context.Context, string, string) (wire.SendAck, error) {
	return wire.SendAck{OK: true}, nil
}
func (b *fakeBroker) ListPeers(context.Context) ([]string, error) { return []string{"alice"}, nil }
func (b *fakeBroker) ConnectionEvents() <-chan brokerclient.ConnectionEvent {
	return b.events
}

type controllerHarness struct {
	cfg         Config
	app         *fakeAppServer
	broker      *fakeBroker
	brokerReady chan *fakeBroker
	turnActive  chan struct{}
	activeOnce  sync.Once
}

func newControllerHarness(t *testing.T) *controllerHarness {
	t.Helper()
	dir := t.TempDir()
	app := newFakeApp(dir)
	h := &controllerHarness{app: app, brokerReady: make(chan *fakeBroker, 1), turnActive: make(chan struct{})}
	h.cfg = Config{
		Name:              "reviewer",
		Version:           "test-version",
		CWD:               dir,
		AppServerEndpoint: "unix:///tmp/fake-intercom-app.sock",
		BrokerSocket:      filepath.Join(dir, "broker.sock"),
		StatePath:         filepath.Join(dir, "state.json"),
		LockPath:          filepath.Join(dir, "state.lock"),
		StartupTimeout:    time.Second,
		ControlTimeout:    time.Second,
		ReverseTimeout:    time.Second,
		ActivityTimeout:   time.Second,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	h.cfg.dialAppServer = func(_ context.Context, _ string, opts appserver.Options) (appServerClient, error) {
		app.opts = opts
		return app, nil
	}
	h.cfg.newBroker = func(opts brokerclient.ClientOptions) brokerConnection {
		broker := newFakeBroker(opts)
		h.brokerReady <- broker
		return broker
	}
	h.cfg.onTurnActive = func() { h.activeOnce.Do(func() { close(h.turnActive) }) }
	return h
}

func runHarness(t *testing.T, h *controllerHarness) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, h.cfg) }()
	select {
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("controller did not register with broker")
	case h.broker = <-h.brokerReady:
	}
	select {
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("controller did not register with broker")
	case <-h.broker.connected:
	}
	return cancel, done
}

func stopHarness(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not stop")
	}
}

func TestControllerNewThreadAndCompletionBeforeResponse(t *testing.T) {
	h := newControllerHarness(t)
	h.app.turnStartHook = func(_ context.Context, params appserver.TurnStartParams) error {
		h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnStarted, Params: mustJSON(t, appserver.TurnStartedNotification{
			ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInProgress},
		})})
		h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSON(t, appserver.TurnCompletedNotification{
			ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusCompleted},
		})})
		return nil
	}
	cancel, done := runHarness(t, h)
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "review this", Timestamp: "2026-07-13T18:42:00Z"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := readManagedState(h.cfg.StatePath)
		if err == nil && data.Materialized {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	state, err := readManagedState(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Materialized || state.ThreadID != "thread-1" {
		t.Fatalf("state = %#v", state)
	}
	h.app.mu.Lock()
	starts := append([]appserver.TurnStartParams(nil), h.app.turnStarts...)
	h.app.mu.Unlock()
	if len(starts) != 1 || starts[0].ClientUserMessageID == nil || *starts[0].ClientUserMessageID != "delivery-1" {
		t.Fatalf("turn starts = %#v", starts)
	}
	if got := starts[0].Input[0].Text; !containsAll(got, "From: alice", "Message-ID: delivery-1", "review this") {
		t.Fatalf("turn input = %q", got)
	}
	stopHarness(t, cancel, done)
}

func TestControllerStartsThreadWithPinnedPolicyAndTools(t *testing.T) {
	h := newControllerHarness(t)
	cancel, done := runHarness(t, h)

	h.app.mu.Lock()
	initializes := append([]appserver.InitializeParams(nil), h.app.initializeParams...)
	starts := append([]appserver.ThreadStartParams(nil), h.app.threadStarts...)
	h.app.mu.Unlock()
	if len(initializes) != 1 || initializes[0].Capabilities == nil || !initializes[0].Capabilities.ExperimentalAPI {
		t.Fatalf("initialize params = %#v", initializes)
	}
	if len(starts) != 1 {
		t.Fatalf("thread starts = %#v", starts)
	}
	start := starts[0]
	if start.CWD == nil || *start.CWD != h.cfg.CWD {
		t.Fatalf("thread/start cwd = %#v, want %q", start.CWD, h.cfg.CWD)
	}
	if start.ApprovalPolicy != string(appserver.ApprovalNever) {
		t.Fatalf("thread/start approval = %#v", start.ApprovalPolicy)
	}
	if start.Sandbox == nil || *start.Sandbox != appserver.SandboxWorkspaceWrite {
		t.Fatalf("thread/start sandbox = %#v", start.Sandbox)
	}
	if start.Ephemeral == nil || *start.Ephemeral {
		t.Fatalf("thread/start ephemeral = %#v", start.Ephemeral)
	}
	if start.DeveloperInstructions == nil || !containsAll(*start.DeveloperInstructions, "reviewer", "send_message", "list_peers") {
		t.Fatalf("thread/start developer instructions = %#v", start.DeveloperInstructions)
	}
	if len(start.DynamicTools) != 2 {
		t.Fatalf("thread/start dynamic tools = %#v", start.DynamicTools)
	}
	stopHarness(t, cancel, done)
}

func TestControllerUnavailableAppServerNeverRegistersBroker(t *testing.T) {
	h := newControllerHarness(t)
	h.cfg.StartupTimeout = 20 * time.Millisecond
	h.cfg.dialAppServer = func(context.Context, string, appserver.Options) (appServerClient, error) {
		return nil, errors.New("connection refused")
	}
	err := Run(t.Context(), h.cfg)
	if err == nil || !containsAll(err.Error(), "app-server unavailable", "connection refused") {
		t.Fatalf("Run() error = %v", err)
	}
	broker := <-h.brokerReady
	select {
	case <-broker.connected:
		t.Fatal("broker registered while app-server was unavailable")
	default:
	}
}

func TestControllerUnsupportedServerVersionNeverRegistersBroker(t *testing.T) {
	h := newControllerHarness(t)
	h.app.init.UserAgent = "codex_cli_rs/0.145.0"
	err := Run(t.Context(), h.cfg)
	if err == nil || !containsAll(err.Error(), "unsupported app-server version", appserver.ProtocolVersion) {
		t.Fatalf("Run() error = %v", err)
	}
	broker := <-h.brokerReady
	select {
	case <-broker.connected:
		t.Fatal("broker registered with an unsupported app-server")
	default:
	}
}

func TestControllerRejectsExpandedOrMalformedWorkspaceSandbox(t *testing.T) {
	dir := t.TempDir()
	thread := appserver.Thread{ID: "thread-1", CWD: dir, Status: appserver.ThreadStatus{Type: appserver.ThreadStatusIdle}}
	for _, tt := range []struct {
		name    string
		sandbox appserver.SandboxPolicy
	}{
		{name: "additional root", sandbox: appserver.SandboxPolicy{Type: "workspaceWrite", NetworkAccess: false, WritableRoots: []string{"/tmp"}}},
		{name: "missing network bool", sandbox: appserver.SandboxPolicy{Type: "workspaceWrite", WritableRoots: []string{}}},
		{name: "wrong network type", sandbox: appserver.SandboxPolicy{Type: "workspaceWrite", NetworkAccess: "restricted", WritableRoots: []string{}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := &controller{cfg: Config{CWD: dir}}
			if err := c.acceptThread(thread, dir, string(appserver.ApprovalNever), tt.sandbox); err == nil {
				t.Fatalf("acceptThread() accepted %#v", tt.sandbox)
			}
		})
	}
}

func TestControllerResumeReassertsPinnedPolicy(t *testing.T) {
	h := newControllerHarness(t)
	state := validState()
	state.CWD = h.cfg.CWD
	state.CodexHome = h.app.init.CodexHome
	state.ServerUserAgent = h.app.init.UserAgent
	state.ThreadID = "thread-1"
	state.Materialized = true
	writeManagedState(t, h.cfg, state)

	cancel, done := runHarness(t, h)
	h.app.mu.Lock()
	resumes := append([]appserver.ThreadResumeParams(nil), h.app.threadResumes...)
	starts := append([]appserver.ThreadStartParams(nil), h.app.threadStarts...)
	h.app.mu.Unlock()
	if len(starts) != 0 || len(resumes) != 1 {
		t.Fatalf("starts = %#v, resumes = %#v", starts, resumes)
	}
	resume := resumes[0]
	if resume.ThreadID != "thread-1" || !resume.ExcludeTurns {
		t.Fatalf("thread/resume identity = %#v", resume)
	}
	if resume.CWD == nil || *resume.CWD != h.cfg.CWD || resume.ApprovalPolicy != string(appserver.ApprovalNever) {
		t.Fatalf("thread/resume policy = %#v", resume)
	}
	if resume.Sandbox == nil || *resume.Sandbox != appserver.SandboxWorkspaceWrite {
		t.Fatalf("thread/resume sandbox = %#v", resume.Sandbox)
	}
	if resume.DeveloperInstructions == nil || !containsAll(*resume.DeveloperInstructions, "reviewer", "send_message") {
		t.Fatalf("thread/resume developer instructions = %#v", resume.DeveloperInstructions)
	}
	stopHarness(t, cancel, done)
}

func TestControllerQueuesDeliveriesFIFO(t *testing.T) {
	for _, terminalStatus := range []string{appserver.TurnStatusCompleted, appserver.TurnStatusFailed, appserver.TurnStatusInterrupted} {
		t.Run(terminalStatus, func(t *testing.T) {
			h := newControllerHarness(t)
			h.app.turnStartResponses = []appserver.TurnStartResponse{
				{Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInProgress}},
				{Turn: appserver.Turn{ID: "turn-2", Status: appserver.TurnStatusInProgress}},
			}
			cancel, done := runHarness(t, h)
			h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "first", Timestamp: "2026-07-13T18:42:00Z"})
			waitFor(t, "first turn", func() bool { return turnStartCount(h.app) == 1 })
			h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-2", From: "bob", Message: "second", Timestamp: "2026-07-13T18:43:00Z"})
			time.Sleep(25 * time.Millisecond)
			if got := turnStartCount(h.app); got != 1 {
				t.Fatalf("second delivery started while first active: %d starts", got)
			}

			h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSON(t, appserver.TurnCompletedNotification{
				ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: terminalStatus},
			})})
			waitFor(t, "second turn", func() bool { return turnStartCount(h.app) == 2 })
			h.app.mu.Lock()
			starts := append([]appserver.TurnStartParams(nil), h.app.turnStarts...)
			h.app.mu.Unlock()
			if starts[0].ClientUserMessageID == nil || *starts[0].ClientUserMessageID != "delivery-1" ||
				starts[1].ClientUserMessageID == nil || *starts[1].ClientUserMessageID != "delivery-2" {
				t.Fatalf("turn order = %#v", starts)
			}
			h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSON(t, appserver.TurnCompletedNotification{
				ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-2", Status: appserver.TurnStatusCompleted},
			})})
			stopHarness(t, cancel, done)
		})
	}
}

func TestControllerReconnectsBroker(t *testing.T) {
	h := newControllerHarness(t)
	cancel, done := runHarness(t, h)
	h.broker.events <- brokerclient.ConnectionEvent{
		State: brokerclient.ConnectionStateDisconnected, Generation: 1, Cause: brokerclient.ConnectionEventCauseEOF,
	}
	waitFor(t, "broker reconnect", func() bool {
		h.broker.mu.Lock()
		defer h.broker.mu.Unlock()
		return h.broker.connects >= 2
	})
	stopHarness(t, cancel, done)
}

func TestControllerDoesNotLoseDisconnectDuringReconnect(t *testing.T) {
	h := newControllerHarness(t)
	cancel, done := runHarness(t, h)
	h.broker.mu.Lock()
	h.broker.connectHook = func(connects int) {
		if connects == 2 {
			h.broker.events <- brokerclient.ConnectionEvent{
				State: brokerclient.ConnectionStateDisconnected, Generation: 2, Cause: brokerclient.ConnectionEventCauseEOF,
			}
		}
	}
	h.broker.mu.Unlock()
	h.broker.events <- brokerclient.ConnectionEvent{
		State: brokerclient.ConnectionStateDisconnected, Generation: 1, Cause: brokerclient.ConnectionEventCauseEOF,
	}
	waitFor(t, "second broker reconnect", func() bool {
		h.broker.mu.Lock()
		defer h.broker.mu.Unlock()
		return h.broker.connects >= 3
	})
	stopHarness(t, cancel, done)
}

func TestControllerShutdownBoundsBlockedReconnectAndBrokerClose(t *testing.T) {
	h := newControllerHarness(t)
	h.cfg.ControlTimeout = 40 * time.Millisecond
	cancel, done := runHarness(t, h)

	reconnectStarted := make(chan struct{})
	reconnectCanceled := make(chan struct{})
	closeStarted := make(chan struct{})
	release := make(chan struct{})
	var reconnectOnce, canceledOnce, closeOnce, releaseOnce sync.Once
	releaseBlocked := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseBlocked()

	h.broker.mu.Lock()
	h.broker.connectContextHook = func(ctx context.Context, connects int) {
		if connects != 2 {
			return
		}
		reconnectOnce.Do(func() { close(reconnectStarted) })
		<-ctx.Done()
		canceledOnce.Do(func() { close(reconnectCanceled) })
		<-release
	}
	h.broker.closeHook = func() {
		closeOnce.Do(func() { close(closeStarted) })
		<-release
	}
	h.broker.mu.Unlock()

	h.broker.events <- brokerclient.ConnectionEvent{
		State: brokerclient.ConnectionStateDisconnected, Generation: 1, Cause: brokerclient.ConnectionEventCauseEOF,
	}
	waitForSignal(t, "blocked broker reconnect", reconnectStarted)

	started := time.Now()
	cancel()
	waitForSignal(t, "reconnect cancellation", reconnectCanceled)
	waitForSignal(t, "broker close", closeStarted)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("controller shutdown remained blocked behind broker reconnect")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("controller shutdown took %s, want one bounded control budget", elapsed)
	}

	// Let the intentionally context-insensitive fake operations exit so the
	// regression test itself leaves no goroutines behind.
	releaseBlocked()
	waitForSignal(t, "eventual broker close", h.broker.closed)
}

func TestControllerShutdownReservesCleanupBudgetWhenBrokerCloseBlocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const controlTimeout = time.Hour
		app := newFakeApp("/tmp/project")
		broker := newFakeBroker(brokerclient.ClientOptions{})
		closeStarted := make(chan struct{})
		releaseClose := make(chan struct{})
		var closeStartOnce, releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseClose) }) }
		defer release()
		broker.closeHook = func() {
			closeStartOnce.Do(func() { close(closeStarted) })
			<-releaseClose
		}

		var c *controller
		var interruptAt, handlersAt time.Time
		app.interruptHook = func(params appserver.TurnInterruptParams) error {
			select {
			case <-closeStarted:
			default:
				t.Fatal("turn interrupt ran before broker Close started")
			}
			interruptAt = time.Now()
			c.notifications <- appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSONBytes(appserver.TurnCompletedNotification{
				ThreadID: params.ThreadID,
				Turn:     appserver.Turn{ID: params.TurnID, Status: appserver.TurnStatusInterrupted},
			})}
			return nil
		}
		app.waitHandlersHook = func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				t.Fatalf("handler drain received expired reserved context: %v", err)
			}
			handlersAt = time.Now()
			return nil
		}
		c = &controller{
			cfg:           Config{ControlTimeout: controlTimeout},
			logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
			app:           app,
			broker:        broker,
			notifications: make(chan appserver.Notification, 1),
			phase:         phaseActive,
			threadID:      "thread-1",
			turnID:        "turn-1",
		}

		started := time.Now()
		c.shutdown(nil)
		elapsed := time.Since(started)
		if interruptAt.IsZero() {
			t.Fatal("blocked broker Close prevented TurnInterrupt")
		}
		if handlersAt.IsZero() {
			t.Fatal("blocked broker Close prevented WaitHandlers")
		}
		if interruptDelay, want := interruptAt.Sub(started), controlTimeout/2; interruptDelay != want {
			t.Fatalf("TurnInterrupt started after %v, want broker reservation %v", interruptDelay, want)
		}
		if handlersAt.Before(interruptAt) {
			t.Fatalf("WaitHandlers ran at %v before TurnInterrupt at %v", handlersAt, interruptAt)
		}
		if want := controlTimeout / 2; elapsed != want {
			t.Fatalf("shutdown returned after %v, want %v with immediate app cleanup", elapsed, want)
		}
		select {
		case <-broker.closed:
			t.Fatal("blocked broker Close unexpectedly completed before release")
		default:
		}

		release()
		synctest.Wait()
		select {
		case <-broker.closed:
		default:
			t.Fatal("broker Close goroutine did not finish after release")
		}
	})
}

func TestControllerStartupGateClosesRaceDuringBrokerConnect(t *testing.T) {
	h := newControllerHarness(t)
	var broker *fakeBroker
	h.cfg.newBroker = func(opts brokerclient.ClientOptions) brokerConnection {
		broker = newFakeBroker(opts)
		broker.connectHook = func(int) {
			h.app.opts.OnReverseRequestReceived(appserver.MethodDynamicToolCall)
		}
		h.brokerReady <- broker
		return broker
	}

	err := Run(t.Context(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "before adapter ownership") {
		t.Fatalf("Run() error = %v", err)
	}
	if broker == nil {
		t.Fatal("broker was not constructed")
	}
	select {
	case <-broker.closed:
	case <-time.After(time.Second):
		t.Fatal("broker remained registered after startup violation")
	}
}

func TestControllerAppServerDisconnectIsFatal(t *testing.T) {
	h := newControllerHarness(t)
	cancel, done := runHarness(t, h)
	defer cancel()
	if err := h.app.Close(); err != nil {
		t.Fatal(err)
	}
	err := waitRunError(t, done)
	if err == nil || !strings.Contains(err.Error(), "app-server disconnected") {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-h.broker.closed:
	case <-time.After(time.Second):
		t.Fatal("broker remained registered after app-server disconnect")
	}
}

func TestControllerActiveTurnWatchdogClosesPresenceAndInterrupts(t *testing.T) {
	h := newControllerHarness(t)
	h.cfg.ActivityTimeout = 20 * time.Millisecond
	cancel, done := runHarness(t, h)
	defer cancel()
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "hang", Timestamp: "2026-07-13T18:42:00Z"})
	waitForSignal(t, "active turn", h.turnActive)
	err := waitRunError(t, done)
	if err == nil || !strings.Contains(err.Error(), "no app-server activity") {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-h.broker.closed:
	case <-time.After(time.Second):
		t.Fatal("broker remained registered after watchdog")
	}
	h.app.mu.Lock()
	interrupts := append([]appserver.TurnInterruptParams(nil), h.app.interrupts...)
	h.app.mu.Unlock()
	if len(interrupts) != 1 || interrupts[0].ThreadID != "thread-1" || interrupts[0].TurnID != "turn-1" {
		t.Fatalf("turn interrupts = %#v", interrupts)
	}
}

func TestControllerTurnStartDeadlineClosesPresence(t *testing.T) {
	h := newControllerHarness(t)
	h.cfg.ControlTimeout = 20 * time.Millisecond
	h.app.turnStartHook = func(ctx context.Context, _ appserver.TurnStartParams) error {
		<-ctx.Done()
		return ctx.Err()
	}
	cancel, done := runHarness(t, h)
	defer cancel()
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "hang start", Timestamp: "2026-07-13T18:42:00Z"})
	err := waitRunError(t, done)
	if err == nil || !containsAll(err.Error(), "start delivery delivery-1", "deadline exceeded") {
		t.Fatalf("Run() error = %v", err)
	}
	select {
	case <-h.broker.closed:
	case <-time.After(time.Second):
		t.Fatal("broker remained registered after turn/start deadline")
	}
}

func TestControllerTurnStartDeadlineDrainsAmbiguousTerminal(t *testing.T) {
	h := newControllerHarness(t)
	interruptCalled := make(chan appserver.TurnInterruptParams, 1)
	allowCompletion := make(chan struct{})
	handlersWaiting := make(chan struct{})
	var completionOnce sync.Once
	releaseCompletion := func() { completionOnce.Do(func() { close(allowCompletion) }) }
	defer releaseCompletion()

	h.app.turnStartHook = func(context.Context, appserver.TurnStartParams) error {
		return context.DeadlineExceeded
	}
	h.app.interruptHook = func(params appserver.TurnInterruptParams) error {
		interruptCalled <- params
		go func() {
			<-allowCompletion
			h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSONBytes(appserver.TurnCompletedNotification{
				ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInterrupted},
			})})
		}()
		return nil
	}
	h.app.waitHandlersHook = func(context.Context) error {
		close(handlersWaiting)
		return nil
	}

	cancel, done := runHarness(t, h)
	defer cancel()
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "ambiguous start", Timestamp: "2026-07-13T18:42:00Z"})
	select {
	case params := <-interruptCalled:
		if params.ThreadID != "thread-1" || params.TurnID != "" {
			t.Fatalf("ambiguous interrupt = %#v", params)
		}
	case <-time.After(time.Second):
		t.Fatal("ambiguous turn/start was not interrupted")
	}
	select {
	case <-h.broker.closed:
	default:
		t.Fatal("turn interrupt ran before broker presence closed")
	}
	select {
	case <-handlersWaiting:
		t.Fatal("reverse-handler drain began before ambiguous turn became terminal")
	case err := <-done:
		t.Fatalf("controller exited before ambiguous turn became terminal: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	releaseCompletion()
	waitForSignal(t, "reverse-handler drain", handlersWaiting)
	if err := waitRunError(t, done); err == nil || !containsAll(err.Error(), "start delivery delivery-1", "deadline exceeded") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestControllerRejectsNonInProgressTurnStartResponse(t *testing.T) {
	h := newControllerHarness(t)
	h.app.turnStart = appserver.TurnStartResponse{Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusCompleted}}
	cancel, done := runHarness(t, h)
	defer cancel()
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "bad status", Timestamp: "2026-07-13T18:42:00Z"})
	err := waitRunError(t, done)
	if err == nil || !strings.Contains(err.Error(), "started with status") {
		t.Fatalf("Run() error = %v", err)
	}
}

func TestControllerRejectsMalformedLifecycleStatuses(t *testing.T) {
	c := &controller{phase: phaseStarting, threadID: "thread-1", current: wire.Deliver{ID: "delivery-1"}}
	if err := c.reconcileTurn("thread-1", "turn-1", appserver.TurnStatusCompleted); err == nil {
		t.Fatal("turn/started accepted terminal status")
	}
	if err := c.completeTurn(appserver.TurnCompletedNotification{
		ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInProgress},
	}); err == nil {
		t.Fatal("turn/completed accepted in-progress status")
	}
	if got := c.currentPhase(); got != phaseStarting {
		t.Fatalf("phase = %s, want starting", got)
	}
}

func TestControllerDropsUnconsumedProgressNotifications(t *testing.T) {
	c := &controller{
		notifications: make(chan appserver.Notification, 1),
		activity:      make(chan struct{}, 1),
		fatal:         make(chan error, 1),
	}
	for range 1_000 {
		c.enqueueNotification(appserver.Notification{Method: appserver.NotificationItemStarted})
	}
	if got := len(c.notifications); got != 0 {
		t.Fatalf("queued ignored notifications = %d", got)
	}
	if got := len(c.activity); got != 1 {
		t.Fatalf("coalesced activity signals = %d, want 1", got)
	}
	c.enqueueNotification(appserver.Notification{Method: appserver.NotificationTurnStarted})
	if got := len(c.notifications); got != 1 {
		t.Fatalf("queued lifecycle notifications = %d, want 1", got)
	}
	select {
	case err := <-c.fatal:
		t.Fatalf("ignored progress caused fatal overflow: %v", err)
	default:
	}
}

func TestControllerShutdownInterruptsActiveTurn(t *testing.T) {
	h := newControllerHarness(t)
	cancel, done := runHarness(t, h)
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "work", Timestamp: "2026-07-13T18:42:00Z"})
	waitForSignal(t, "active turn", h.turnActive)
	stopHarness(t, cancel, done)
	h.app.mu.Lock()
	interrupts := append([]appserver.TurnInterruptParams(nil), h.app.interrupts...)
	h.app.mu.Unlock()
	if len(interrupts) != 1 || interrupts[0].TurnID != "turn-1" {
		t.Fatalf("turn interrupts = %#v", interrupts)
	}
}

func TestControllerShutdownDrainsTerminalAndReverseHandlers(t *testing.T) {
	h := newControllerHarness(t)
	interruptCalled := make(chan bool, 1)
	allowCompletion := make(chan struct{})
	handlersWaiting := make(chan struct{})
	allowHandlers := make(chan struct{})
	h.app.interruptHook = func(params appserver.TurnInterruptParams) error {
		select {
		case <-h.broker.closed:
			interruptCalled <- params.TurnID == "turn-1"
		default:
			interruptCalled <- false
		}
		go func() {
			<-allowCompletion
			h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSONBytes(appserver.TurnCompletedNotification{
				ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInterrupted},
			})})
		}()
		return nil
	}
	h.app.waitHandlersHook = func(ctx context.Context) error {
		close(handlersWaiting)
		select {
		case <-allowHandlers:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cancel, done := runHarness(t, h)
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "work", Timestamp: "2026-07-13T18:42:00Z"})
	waitForSignal(t, "active turn", h.turnActive)
	cancel()
	select {
	case ordered := <-interruptCalled:
		if !ordered {
			t.Fatal("turn interrupt ran before broker close or used the wrong turn id")
		}
	case <-time.After(time.Second):
		t.Fatal("turn interrupt was not called")
	}
	select {
	case err := <-done:
		t.Fatalf("controller exited before terminal event: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowCompletion)
	select {
	case <-handlersWaiting:
	case <-time.After(time.Second):
		t.Fatal("controller did not begin draining reverse handlers")
	}
	select {
	case err := <-done:
		t.Fatalf("controller exited before reverse handlers drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowHandlers)
	if err := waitRunError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
}

func TestControllerShutdownStartingTurnUsesEmptyInterruptAndDrainsResponse(t *testing.T) {
	h := newControllerHarness(t)
	startAwaiting := make(chan struct{})
	allowStartResponse := make(chan struct{})
	interruptCalled := make(chan appserver.TurnInterruptParams, 1)
	allowCompletion := make(chan struct{})
	h.app.turnStartHook = func(ctx context.Context, _ appserver.TurnStartParams) error {
		close(startAwaiting)
		select {
		case <-allowStartResponse:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.app.interruptHook = func(params appserver.TurnInterruptParams) error {
		interruptCalled <- params
		close(allowStartResponse)
		go func() {
			<-allowCompletion
			h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSONBytes(appserver.TurnCompletedNotification{
				ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInterrupted},
			})})
		}()
		return nil
	}
	cancel, done := runHarness(t, h)
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "starting", Timestamp: "2026-07-13T18:42:00Z"})
	select {
	case <-startAwaiting:
	case <-time.After(time.Second):
		t.Fatal("turn/start await did not begin")
	}
	cancel()
	select {
	case params := <-interruptCalled:
		if params.ThreadID != "thread-1" || params.TurnID != "" {
			t.Fatalf("starting interrupt = %#v", params)
		}
	case <-time.After(time.Second):
		t.Fatal("starting turn was not interrupted")
	}
	select {
	case err := <-done:
		t.Fatalf("controller exited before starting turn terminal event: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(allowCompletion)
	if err := waitRunError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
}

func TestControllerShutdownStartingTurnDrainsAfterAwaitFailure(t *testing.T) {
	h := newControllerHarness(t)
	startAwaiting := make(chan struct{})
	failStart := make(chan struct{})
	interruptCalled := make(chan appserver.TurnInterruptParams, 1)
	allowCompletion := make(chan struct{})
	handlersWaiting := make(chan struct{})
	var startOnce, failOnce, completionOnce sync.Once
	releaseStart := func() { failOnce.Do(func() { close(failStart) }) }
	releaseCompletion := func() { completionOnce.Do(func() { close(allowCompletion) }) }
	defer releaseStart()
	defer releaseCompletion()

	h.app.turnStartHook = func(ctx context.Context, _ appserver.TurnStartParams) error {
		startOnce.Do(func() { close(startAwaiting) })
		select {
		case <-failStart:
			return context.DeadlineExceeded
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.app.interruptHook = func(params appserver.TurnInterruptParams) error {
		interruptCalled <- params
		releaseStart()
		go func() {
			<-allowCompletion
			h.app.opts.OnNotification(appserver.Notification{Method: appserver.NotificationTurnCompleted, Params: mustJSONBytes(appserver.TurnCompletedNotification{
				ThreadID: "thread-1", Turn: appserver.Turn{ID: "turn-1", Status: appserver.TurnStatusInterrupted},
			})})
		}()
		return nil
	}
	h.app.waitHandlersHook = func(context.Context) error {
		close(handlersWaiting)
		return nil
	}

	cancel, done := runHarness(t, h)
	h.broker.opts.OnDeliver(wire.Deliver{ID: "delivery-1", From: "alice", Message: "cancel starting", Timestamp: "2026-07-13T18:42:00Z"})
	waitForSignal(t, "turn/start await", startAwaiting)
	cancel()
	select {
	case params := <-interruptCalled:
		if params.ThreadID != "thread-1" || params.TurnID != "" {
			t.Fatalf("starting interrupt = %#v", params)
		}
	case <-time.After(time.Second):
		t.Fatal("starting turn was not interrupted")
	}
	select {
	case <-handlersWaiting:
		t.Fatal("reverse-handler drain began after await failure but before terminal event")
	case err := <-done:
		t.Fatalf("controller exited after await failure but before terminal event: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	releaseCompletion()
	waitForSignal(t, "reverse-handler drain", handlersWaiting)
	if err := waitRunError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
}

func TestControllerResumeUnmaterializedDoesNotTrustPath(t *testing.T) {
	h := newControllerHarness(t)
	state := validState()
	state.CWD = h.cfg.CWD
	state.CodexHome = h.app.init.CodexHome
	state.ServerUserAgent = h.app.init.UserAgent
	state.ThreadID = "thread-1"
	store, err := AcquireStateStore(h.cfg.StatePath, h.cfg.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	h.app.readErr = &appserver.RPCError{
		Code:    appserver.ErrorCodeInvalidRequest,
		Message: "thread thread-1 is not materialized yet; includeTurns is unavailable before first user message",
	}

	cancel, done := runHarness(t, h)
	got, err := readManagedState(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.Materialized {
		t.Fatal("unmaterialized state was incorrectly promoted")
	}
	stopHarness(t, cancel, done)
}

func TestControllerResumePendingThreadFailsOnUnexpectedReadError(t *testing.T) {
	h := newControllerHarness(t)
	state := validState()
	state.CWD = h.cfg.CWD
	state.CodexHome = h.app.init.CodexHome
	state.ServerUserAgent = h.app.init.UserAgent
	state.ThreadID = "thread-1"
	writeManagedState(t, h.cfg, state)
	h.app.readErr = errors.New("database unavailable")

	err := Run(t.Context(), h.cfg)
	if err == nil || !containsAll(err.Error(), "verify pending thread materialization", "database unavailable") {
		t.Fatalf("Run() error = %v", err)
	}
	broker := <-h.brokerReady
	select {
	case <-broker.connected:
		t.Fatal("controller registered before pending thread verification")
	default:
	}
}

func TestControllerReplacesMissingUnmaterializedThread(t *testing.T) {
	h := newControllerHarness(t)
	state := validState()
	state.CWD = h.cfg.CWD
	state.CodexHome = h.app.init.CodexHome
	state.ServerUserAgent = h.app.init.UserAgent
	state.ThreadID = "old-thread"
	store, err := AcquireStateStore(h.cfg.StatePath, h.cfg.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	h.app.resumeErr = &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "no rollout found for thread id old-thread"}

	cancel, done := runHarness(t, h)
	got, err := readManagedState(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadID != "thread-1" || got.Materialized {
		t.Fatalf("replacement state = %#v", got)
	}
	stopHarness(t, cancel, done)
}

func TestControllerNeverReplacesMissingMaterializedThread(t *testing.T) {
	h := newControllerHarness(t)
	state := validState()
	state.CWD = h.cfg.CWD
	state.CodexHome = h.app.init.CodexHome
	state.ServerUserAgent = h.app.init.UserAgent
	state.ThreadID = "old-thread"
	state.Materialized = true
	writeManagedState(t, h.cfg, state)
	h.app.resumeErr = &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "no rollout found for thread id old-thread"}

	err := Run(t.Context(), h.cfg)
	if err == nil || !containsAll(err.Error(), "resume thread old-thread", "no rollout found") {
		t.Fatalf("Run() error = %v", err)
	}
	h.app.mu.Lock()
	starts := len(h.app.threadStarts)
	h.app.mu.Unlock()
	if starts != 0 {
		t.Fatalf("materialized binding was replaced with %d thread/start calls", starts)
	}
	got, err := readManagedState(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadID != "old-thread" || !got.Materialized {
		t.Fatalf("materialized binding changed: %#v", got)
	}
}

func TestControllerNewPreservesOldBindingUntilReplacementStarts(t *testing.T) {
	h := newControllerHarness(t)
	state := validState()
	state.CWD = h.cfg.CWD
	state.CodexHome = h.app.init.CodexHome
	state.ServerUserAgent = h.app.init.UserAgent
	state.ThreadID = "old-thread"
	state.Materialized = true
	writeManagedState(t, h.cfg, state)
	h.cfg.New = true
	h.app.startErr = errors.New("model configuration unavailable")

	err := Run(t.Context(), h.cfg)
	if err == nil || !strings.Contains(err.Error(), "start thread") {
		t.Fatalf("Run() error = %v", err)
	}
	got, err := readManagedState(h.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got.ThreadID != "old-thread" || !got.Materialized {
		t.Fatalf("old binding was changed after failed --new: %#v", got)
	}
}

func TestControllerQueueOverflowIsFatal(t *testing.T) {
	broker := newFakeBroker(brokerclient.ClientOptions{})
	c := &controller{deliveries: make(chan wire.Deliver, 1), fatal: make(chan error, 1), broker: broker}
	c.enqueueDelivery(wire.Deliver{ID: "one"})
	c.enqueueDelivery(wire.Deliver{ID: "two"})
	select {
	case err := <-c.fatal:
		if err == nil {
			t.Fatal("nil fatal error")
		}
	case <-time.After(time.Second):
		t.Fatal("queue overflow did not signal fatal")
	}
	select {
	case <-broker.closed:
	case <-time.After(time.Second):
		t.Fatal("queue overflow did not close broker")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSONBytes(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}

func readManagedState(path string) (ManagedState, error) {
	var state ManagedState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return ManagedState{}, err
	}
	return state, nil
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}

func writeManagedState(t *testing.T, cfg Config, state ManagedState) {
	t.Helper()
	store, err := AcquireStateStore(cfg.StatePath, cfg.LockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state); err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func turnStartCount(app *fakeAppServer) int {
	app.mu.Lock()
	defer app.mu.Unlock()
	return len(app.turnStarts)
}

func waitFor(t *testing.T, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func waitForSignal(t *testing.T, description string, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func waitRunError(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not stop")
		return nil
	}
}
