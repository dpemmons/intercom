package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/appserverproxy"
	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/codexbridge"
	"github.com/dpemmons/intercom/internal/codexsession"
	"github.com/dpemmons/intercom/internal/paths"
	"github.com/dpemmons/intercom/internal/wire"
)

const (
	defaultQueueSize       = 64
	defaultControlTimeout  = 30 * time.Second
	defaultReverseTimeout  = 10 * time.Second
	defaultActivityTimeout = 15 * time.Minute
	maxDescendantAncestry  = 64
	proxyEventQueueSize    = 256
	managedMCPServerName   = "intercom_managed"
	bridgeTokenEnvironment = "INTERCOM_CODEX_BRIDGE_TOKEN"
)

var errTurnAlreadyReserved = errors.New("codex: managed thread turn is already reserved")

type Config struct {
	Name              string
	Version           string
	CWD               string
	AppServerEndpoint string
	ClientEndpoint    string
	MCPBridgeSocket   string
	IntercomBin       string
	BrokerSocket      string
	BrokerBin         string
	New               bool
	AdoptThreadID     string
	ForkThreadID      string
	ReplaceBinding    bool
	ExecutionPolicy   ExecutionPolicy
	Logger            *slog.Logger

	QueueSize       int
	StartupTimeout  time.Duration
	ControlTimeout  time.Duration
	ReverseTimeout  time.Duration
	ActivityTimeout time.Duration
	StatePath       string
	LockPath        string
	OnReady         func(ReadyInfo) error
	OnStopping      func() error

	dialAppServer  func(context.Context, string, appserver.Options) (appServerClient, error)
	newBroker      func(brokerclient.ClientOptions) brokerConnection
	onTurnActive   func()
	threadLockPath func(string, string) (string, error)
}

// ReadyInfo describes the live managed thread after the broker and optional
// TUI proxy are ready. OnReady runs synchronously before Run enters its main
// service loop.
type ReadyInfo struct {
	Name            string
	CWD             string
	ThreadID        string
	ClientEndpoint  string
	CodexVersion    string
	ExecutionPolicy ExecutionPolicy
}

type appServerClient interface {
	Initialize(context.Context, appserver.InitializeParams) (appserver.InitializeResponse, error)
	Initialized(context.Context) error
	ThreadStart(context.Context, appserver.ThreadStartParams) (appserver.ThreadStartResponse, error)
	ThreadResume(context.Context, appserver.ThreadResumeParams) (appserver.ThreadResumeResponse, error)
	ThreadFork(context.Context, appserver.ThreadForkParams) (appserver.ThreadForkResponse, error)
	ThreadList(context.Context, appserver.ThreadListParams) (appserver.ThreadListResponse, error)
	ThreadRead(context.Context, appserver.ThreadReadParams) (appserver.ThreadReadResponse, error)
	MCPServerStatusList(context.Context, appserver.MCPServerStatusListParams) (appserver.MCPServerStatusListResponse, error)
	StartTurn(context.Context, appserver.TurnStartParams) (appserver.TurnStartAwait, error)
	TurnInterrupt(context.Context, appserver.TurnInterruptParams) error
	WaitHandlers(context.Context) error
	Done() <-chan struct{}
	Wait() error
	Close() error
}

type brokerConnection interface {
	Connect(context.Context) error
	Close() error
	Send(context.Context, string, string) (wire.SendAck, error)
	ListPeers(context.Context) ([]string, error)
	ConnectionEvents() <-chan brokerclient.ConnectionEvent
}

type controllerPhase uint8

type turnOwner uint8

type turnStartResult struct {
	response appserver.TurnStartResponse
	err      error
}

type queuedNotification struct {
	notification      appserver.Notification
	applied           bool
	managedCompletion bool
}

const (
	phaseBooting controllerPhase = iota
	phaseIdle
	phaseStarting
	phaseActive
	phaseAwaitingStartResponse
	phaseCompleting
	phaseFailed
)

const (
	turnOwnerNone turnOwner = iota
	turnOwnerIntercom
	turnOwnerTUI
)

func (p controllerPhase) String() string {
	switch p {
	case phaseBooting:
		return "booting"
	case phaseIdle:
		return "idle"
	case phaseStarting:
		return "starting"
	case phaseActive:
		return "active"
	case phaseAwaitingStartResponse:
		return "awaiting-start-response"
	case phaseCompleting:
		return "completing"
	case phaseFailed:
		return "failed"
	default:
		return "unknown"
	}
}

func (o turnOwner) String() string {
	switch o {
	case turnOwnerNone:
		return "none"
	case turnOwnerIntercom:
		return "intercom"
	case turnOwnerTUI:
		return "tui"
	default:
		return "unknown"
	}
}

type controller struct {
	cfg          Config
	ctx          context.Context
	logger       *slog.Logger
	store        *StateStore
	state        *ManagedState
	pendingState *ManagedState
	threadLock   *ThreadLock
	app          appServerClient
	broker       brokerConnection
	bridge       *codexbridge.Controller
	bridgeToken  string

	deliveries    chan wire.Deliver
	notifications chan queuedNotification
	fatal         chan error
	activity      chan struct{}
	stateChanged  chan struct{}
	reconnectDone chan error

	brokerCloseOnce sync.Once
	brokerCloseDone chan struct{}
	brokerCloseErr  error

	mu                    sync.Mutex
	phase                 controllerPhase
	ready                 bool
	threadID              string
	turnID                string
	current               wire.Deliver
	outboundCount         int
	reconnecting          bool
	reconnectPending      bool
	startupViolation      bool
	descendantThreads     map[string]struct{}
	sandbox               appserver.SandboxPolicy
	startResult           <-chan turnStartResult
	startAmbiguous        bool
	turnTerminalSeen      bool
	turnTerminalProcessed bool
	proxyEventGate        bool
	proxyEventQueue       []appserver.Notification

	reverse reverseHandler

	proxyMu sync.RWMutex
	proxy   *appserverproxy.Proxy

	syntheticResumeMu sync.RWMutex
	syntheticResume   json.RawMessage

	initializeResponse    appserver.InitializeResponse
	codexVersion          string
	turnOwner             turnOwner
	turnStartResponseSeen bool
}

// Run connects one externally supervised app-server thread to the Intercom
// broker and blocks until cancellation or a fatal lifecycle error.
func Run(parent context.Context, cfg Config) error {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	c := &controller{
		cfg:           cfg,
		ctx:           ctx,
		logger:        cfg.Logger,
		phase:         phaseBooting,
		deliveries:    make(chan wire.Deliver, cfg.QueueSize),
		notifications: make(chan queuedNotification, proxyEventQueueSize),
		fatal:         make(chan error, 1),
		activity:      make(chan struct{}, 1),
		stateChanged:  make(chan struct{}, 1),
		reconnectDone: make(chan error, 1),
	}

	store, err := AcquireStateStore(cfg.StatePath, cfg.LockPath)
	if err != nil {
		return err
	}
	c.store = store
	defer store.Close()
	defer func() {
		if err := c.releaseThreadLock(); err != nil {
			c.logger.Warn("release Codex thread lock", "err", err)
		}
	}()
	if !cfg.New {
		c.state, err = store.Load()
		if err != nil {
			return err
		}
	}
	if c.state != nil && (cfg.AdoptThreadID != "" || cfg.ForkThreadID != "") {
		if cfg.AdoptThreadID == c.state.ThreadID {
			// Re-selecting the current binding is an idempotent resume. Preserve
			// its established tool transport instead of layering a second one.
			c.cfg.AdoptThreadID = ""
		} else if !cfg.ReplaceBinding {
			return fmt.Errorf("codex: peer %q already binds thread %s; use --replace-binding to replace it", cfg.Name, c.state.ThreadID)
		}
	}

	brokerOptions := brokerclient.ClientOptions{
		Name:       cfg.Name,
		Version:    cfg.Version,
		SocketPath: cfg.BrokerSocket,
		BrokerBin:  cfg.BrokerBin,
		Logger:     cfg.Logger,
		OnDeliver:  c.enqueueDelivery,
	}
	if cfg.newBroker != nil {
		c.broker = cfg.newBroker(brokerOptions)
	} else {
		c.broker = brokerclient.NewClient(brokerOptions)
	}
	needsBridge := c.cfg.AdoptThreadID != "" || c.cfg.ForkThreadID != "" ||
		(c.state != nil && c.state.ToolTransport == ToolTransportMCPBridge)
	if needsBridge {
		if c.cfg.MCPBridgeSocket == "" || c.cfg.IntercomBin == "" {
			return errors.New("codex: MCP bridge socket and Intercom executable are required for this binding")
		}
		token, err := codexbridge.GenerateToken()
		if err != nil {
			return err
		}
		c.bridgeToken = token
		c.bridge, err = codexbridge.Listen(ctx, codexbridge.Options{
			SocketPath: c.cfg.MCPBridgeSocket,
			Token:      token,
			Handler: codexbridge.HandlerFuncs{
				SendMessageFunc: c.bridgeSendMessage,
				ListPeersFunc:   c.bridgeListPeers,
			},
			RequestTimeout: c.cfg.ReverseTimeout,
		})
		if err != nil {
			return err
		}
		defer func() {
			if err := c.bridge.Close(); err != nil {
				c.logger.Warn("close Codex MCP bridge", "err", err)
			}
		}()
	}
	shutdownOwnsBroker := false
	defer func() {
		if !shutdownOwnsBroker {
			<-c.startBrokerClose()
		}
	}()

	c.reverse = reverseHandler{
		broker:     c.broker,
		authorize:  c.authorizeReverse,
		onOutbound: c.noteOutbound,
		onActivity: c.touchActivity,
		onFatal:    c.signalFatal,
		timeout:    cfg.ReverseTimeout,
		logger:     cfg.Logger,
	}
	appOptions := appserver.Options{
		OnNotification:           c.enqueueNotification,
		OnReverseRequestReceived: c.observeReverseRequest,
		OnReverseRequest:         c.handleReverseRequest,
	}
	c.app, err = c.dialAppServer(ctx, appOptions)
	if err != nil {
		return err
	}
	defer c.app.Close()

	if err := c.startup(ctx); err != nil {
		return err
	}
	if err := c.startTUIProxy(ctx); err != nil {
		return err
	}
	if proxy := c.currentProxy(); proxy != nil {
		defer func() {
			if err := proxy.Close(); err != nil {
				c.logger.Warn("close Codex TUI proxy", "err", err)
			}
		}()
	}
	if c.pendingState != nil {
		if err := c.store.Save(*c.pendingState); err != nil {
			return fmt.Errorf("codex: commit replacement thread binding: %w", err)
		}
		c.state = c.pendingState
		c.pendingState = nil
	}
	if cfg.OnReady != nil {
		if err := cfg.OnReady(ReadyInfo{
			Name:            cfg.Name,
			CWD:             cfg.CWD,
			ThreadID:        c.threadID,
			ClientEndpoint:  cfg.ClientEndpoint,
			CodexVersion:    c.codexVersion,
			ExecutionPolicy: cfg.ExecutionPolicy,
		}); err != nil {
			return fmt.Errorf("codex: publish readiness: %w", err)
		}
	}
	defer c.shutdown(cancel)
	shutdownOwnsBroker = true
	return c.loop(ctx)
}

func normalizeConfig(cfg Config) (Config, error) {
	if cfg.Name == "" || !wire.ValidName(cfg.Name) {
		return Config{}, fmt.Errorf("codex: invalid peer name %q", cfg.Name)
	}
	if cfg.Version == "" {
		return Config{}, errors.New("codex: version is required")
	}
	if _, err := appserver.ParseUnixEndpoint(cfg.AppServerEndpoint); err != nil {
		return Config{}, err
	}
	if cfg.ClientEndpoint != "" {
		if _, err := appserver.ParseUnixEndpoint(cfg.ClientEndpoint); err != nil {
			return Config{}, fmt.Errorf("codex: client endpoint: %w", err)
		}
		if cfg.ClientEndpoint == cfg.AppServerEndpoint {
			return Config{}, errors.New("codex: client endpoint must differ from app-server endpoint")
		}
	}
	if cfg.MCPBridgeSocket != "" {
		if !filepath.IsAbs(cfg.MCPBridgeSocket) {
			return Config{}, errors.New("codex: MCP bridge socket path must be absolute")
		}
		cfg.MCPBridgeSocket = filepath.Clean(cfg.MCPBridgeSocket)
	}
	if cfg.IntercomBin != "" {
		if !filepath.IsAbs(cfg.IntercomBin) {
			return Config{}, errors.New("codex: Intercom executable path must be absolute")
		}
		cfg.IntercomBin = filepath.Clean(cfg.IntercomBin)
	}
	if cfg.BrokerSocket == "" {
		return Config{}, errors.New("codex: broker socket is required")
	}
	if cfg.ExecutionPolicy == "" {
		cfg.ExecutionPolicy = ExecutionWorkspaceWrite
	}
	if err := cfg.ExecutionPolicy.validate(); err != nil {
		return Config{}, err
	}
	selectionModes := 0
	if cfg.New {
		selectionModes++
	}
	if cfg.AdoptThreadID != "" {
		selectionModes++
	}
	if cfg.ForkThreadID != "" {
		selectionModes++
	}
	if selectionModes > 1 {
		return Config{}, errors.New("codex: --new, --adopt-session, and --fork-session are mutually exclusive")
	}
	if cfg.ReplaceBinding && cfg.AdoptThreadID == "" && cfg.ForkThreadID == "" {
		return Config{}, errors.New("codex: --replace-binding requires --adopt-session or --fork-session")
	}
	if (cfg.AdoptThreadID != "" || cfg.ForkThreadID != "") && (cfg.MCPBridgeSocket == "" || cfg.IntercomBin == "") {
		return Config{}, errors.New("codex: adopted and forked threads require the managed MCP bridge")
	}
	cwd, err := canonicalDirectory(cfg.CWD)
	if err != nil {
		return Config{}, err
	}
	cfg.CWD = cwd
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.StartupTimeout <= 0 {
		cfg.StartupTimeout = defaultControlTimeout
	}
	if cfg.ControlTimeout <= 0 {
		cfg.ControlTimeout = defaultControlTimeout
	}
	if cfg.ReverseTimeout <= 0 {
		cfg.ReverseTimeout = defaultReverseTimeout
	}
	if cfg.ActivityTimeout <= 0 {
		cfg.ActivityTimeout = defaultActivityTimeout
	}
	if cfg.StatePath == "" {
		cfg.StatePath, err = paths.CodexState(cfg.Name)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.LockPath == "" {
		cfg.LockPath, err = paths.CodexLock(cfg.Name)
		if err != nil {
			return Config{}, err
		}
	}
	if cfg.dialAppServer == nil {
		cfg.dialAppServer = func(ctx context.Context, endpoint string, opts appserver.Options) (appServerClient, error) {
			return appserver.DialUnix(ctx, endpoint, opts)
		}
	}
	if cfg.threadLockPath == nil {
		cfg.threadLockPath = paths.CodexThreadLock
	}
	return cfg, nil
}

func canonicalDirectory(value string) (string, error) {
	if value == "" {
		var err error
		value, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("codex: get working directory: %w", err)
		}
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("codex: resolve cwd: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("codex: resolve cwd symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("codex: stat cwd: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("codex: cwd is not a directory: %s", resolved)
	}
	return resolved, nil
}

func (c *controller) dialAppServer(ctx context.Context, opts appserver.Options) (appServerClient, error) {
	deadline := time.Now().Add(c.cfg.StartupTimeout)
	delay := 50 * time.Millisecond
	var lastErr error
	for {
		attemptCtx, cancel := context.WithDeadline(ctx, deadline)
		client, err := c.cfg.dialAppServer(attemptCtx, c.cfg.AppServerEndpoint, opts)
		cancel()
		if err == nil {
			return client, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("codex: app-server unavailable after %s: %w", c.cfg.StartupTimeout, lastErr)
		}
		if delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if delay < time.Second {
			delay *= 2
		}
	}
}

func (c *controller) startup(ctx context.Context) error {
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	init, err := c.app.Initialize(control, appserver.InitializeParams{
		ClientInfo: appserver.ClientInfo{Name: "intercom", Version: c.cfg.Version},
		Capabilities: &appserver.InitializeCapabilities{
			ExperimentalAPI: true,
		},
	})
	cancel()
	if err != nil {
		return fmt.Errorf("codex: initialize app-server: %w", err)
	}
	version, err := validateServerVersion(init.UserAgent)
	if err != nil {
		return err
	}
	c.initializeResponse = init
	c.codexVersion = version
	control, cancel = context.WithTimeout(ctx, c.cfg.ControlTimeout)
	err = c.app.Initialized(control)
	cancel()
	if err != nil {
		return fmt.Errorf("codex: send initialized: %w", err)
	}

	if c.cfg.AdoptThreadID != "" {
		if err := c.adoptThread(ctx, init, version); err != nil {
			return err
		}
	} else if c.cfg.ForkThreadID != "" {
		if err := c.forkThread(ctx, init, version); err != nil {
			return err
		}
	} else if c.state != nil {
		if err := c.validateStoredBinding(init); err != nil {
			return err
		}
		if err := c.resumeOrReplace(ctx, init, version); err != nil {
			return err
		}
	} else if err := c.startNewThread(ctx, init, version); err != nil {
		return err
	}
	if err := c.takeFatal(); err != nil {
		return err
	}
	if c.hasStartupViolation() {
		return errors.New("codex: dynamic tool request arrived before adapter ownership was established")
	}

	control, cancel = context.WithTimeout(ctx, c.cfg.ControlTimeout)
	err = c.broker.Connect(control)
	cancel()
	if err != nil {
		return fmt.Errorf("codex: register with broker: %w", err)
	}
	c.mu.Lock()
	if c.startupViolation {
		c.mu.Unlock()
		<-c.startBrokerClose()
		return errors.New("codex: dynamic tool request arrived before adapter ownership was established")
	}
	c.ready = true
	c.phase = phaseIdle
	c.mu.Unlock()
	c.logger.Info("codex peer ready", "peer", c.cfg.Name, "thread", c.threadID, "cwd", c.cfg.CWD, "codex_version", version)
	return nil
}

func (c *controller) startTUIProxy(ctx context.Context) error {
	if c.cfg.ClientEndpoint == "" {
		return nil
	}
	upstream, ok := c.app.(appserverproxy.Upstream)
	if !ok {
		return errors.New("codex: app-server client does not expose raw proxy calls")
	}
	proxy, err := appserverproxy.Listen(ctx, appserverproxy.Options{
		Endpoint:               c.cfg.ClientEndpoint,
		Upstream:               upstream,
		InitializeResponse:     c.initializeResponse,
		ExpectedClientVersion:  c.codexVersion,
		HandshakeTimeout:       c.cfg.ControlTimeout,
		WriteTimeout:           c.cfg.ControlTimeout,
		RequestTimeout:         c.cfg.ControlTimeout,
		TurnStartTimeout:       c.cfg.ControlTimeout,
		ReverseResponseTimeout: c.cfg.ControlTimeout,
		BeforeRequest:          c.beforeTUIRequest,
		LocalRequest:           c.localTUIRequest,
		AfterRequest:           c.afterTUIRequest,
		OnAttach: func() {
			c.logger.Info("Codex TUI attached", "peer", c.cfg.Name, "thread", c.threadID)
		},
		OnDetach: func() {
			c.logger.Info("Codex TUI detached", "peer", c.cfg.Name, "thread", c.threadID)
		},
		Logger: c.logger,
	})
	if err != nil {
		return fmt.Errorf("codex: start TUI proxy: %w", err)
	}
	c.proxyMu.Lock()
	c.proxy = proxy
	c.proxyMu.Unlock()
	return nil
}

func (c *controller) currentProxy() *appserverproxy.Proxy {
	c.proxyMu.RLock()
	defer c.proxyMu.RUnlock()
	return c.proxy
}

// localTUIRequest supplies the thread/start snapshot as a resume response
// until Codex persists the thread's first rollout. BeforeRequest has already
// validated the thread identity and pinned its settings when this hook runs.
func (c *controller) localTUIRequest(method string, _ json.RawMessage, _ any) (json.RawMessage, bool, error) {
	if method != appserver.MethodThreadResume {
		return nil, false, nil
	}
	c.syntheticResumeMu.RLock()
	result := append(json.RawMessage(nil), c.syntheticResume...)
	c.syntheticResumeMu.RUnlock()
	if len(result) == 0 {
		return nil, false, nil
	}
	return result, true, nil
}

func (c *controller) setSyntheticResume(response appserver.ThreadStartResponse) error {
	encoded, err := json.Marshal(appserver.ThreadResumeResponse{
		ThreadResponse:   response.ThreadResponse,
		InitialTurnsPage: json.RawMessage("null"),
	})
	if err != nil {
		return fmt.Errorf("codex: encode pending thread resume response: %w", err)
	}
	c.syntheticResumeMu.Lock()
	c.syntheticResume = encoded
	c.syntheticResumeMu.Unlock()
	return nil
}

func (c *controller) clearSyntheticResume() {
	c.syntheticResumeMu.Lock()
	c.syntheticResume = nil
	c.syntheticResumeMu.Unlock()
}

type tuiTurnReservation struct {
	result chan turnStartResult
}

func (c *controller) beforeTUIRequest(method string, params json.RawMessage) (json.RawMessage, any, *appserver.RPCError) {
	rewritten := params
	switch method {
	case appserver.MethodThreadResume:
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		threadID, err := threadIDFromObject(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if threadID != c.threadID {
			return nil, nil, wrongTUIThread(method, threadID, c.threadID)
		}
		developer, err := combinedTUIDeveloperInstructions(object["developerInstructions"], developerInstructions(c.cfg.Name))
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "cwd", c.cfg.CWD); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "runtimeWorkspaceRoots", []string{c.cfg.CWD}); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "approvalPolicy", string(appserver.ApprovalNever)); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "approvalsReviewer", string(appserver.ApprovalsReviewerUser)); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "sandbox", c.cfg.ExecutionPolicy.sandboxMode()); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		delete(object, "permissions")
		if err := setTUIField(object, "developerInstructions", developer); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "excludeTurns", false); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		rewritten = encoded
	case appserver.MethodTurnStart:
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		threadID, err := threadIDFromObject(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if threadID != c.threadID {
			return nil, nil, wrongTUIThread(method, threadID, c.threadID)
		}
		if err := setTUIField(object, "cwd", c.cfg.CWD); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "runtimeWorkspaceRoots", []string{c.cfg.CWD}); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "approvalPolicy", string(appserver.ApprovalNever)); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "approvalsReviewer", string(appserver.ApprovalsReviewerUser)); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		c.mu.Lock()
		sandbox := c.sandbox
		c.mu.Unlock()
		if err := setTUIField(object, "sandboxPolicy", sandbox); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		delete(object, "permissions")
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		reservation, rpcErr := c.reserveTUITurn()
		if rpcErr != nil {
			return nil, nil, rpcErr
		}
		return encoded, reservation, nil
	case "thread/settings/update":
		c.mu.Lock()
		phase := c.phase
		c.mu.Unlock()
		if phase != phaseIdle {
			return nil, nil, &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "thread/settings/update is allowed only while the managed thread is idle"}
		}
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		threadID, err := threadIDFromObject(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if threadID != c.threadID {
			return nil, nil, wrongTUIThread(method, threadID, c.threadID)
		}
		// Rebuild the request from the documented interactive settings. Unknown
		// future fields are dropped so a protocol extension cannot silently add
		// a permission escape to this allowlisted method.
		filtered := make(map[string]json.RawMessage, 12)
		for _, field := range []string{
			"threadId", "model", "serviceTier", "effort", "summary",
			"collaborationMode", "multiAgentMode", "personality",
		} {
			if raw, ok := object[field]; ok {
				filtered[field] = raw
			}
		}
		if err := setTUIField(filtered, "cwd", c.cfg.CWD); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(filtered, "approvalPolicy", string(appserver.ApprovalNever)); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(filtered, "approvalsReviewer", string(appserver.ApprovalsReviewerUser)); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		c.mu.Lock()
		sandbox := c.sandbox
		c.mu.Unlock()
		if err := setTUIField(filtered, "sandboxPolicy", sandbox); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		encoded, err := json.Marshal(filtered)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		rewritten = encoded
	case "skills/list", "hooks/list", "plugin/list", "plugin/installed":
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "cwds", []string{c.cfg.CWD}); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		rewritten = encoded
	case "config/read", "permissionProfile/list":
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "cwd", c.cfg.CWD); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		rewritten = encoded
	case "fuzzyFileSearch/sessionStart":
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "roots", []string{c.cfg.CWD}); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		rewritten = encoded
	case "account/read":
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if err := setTUIField(object, "refreshToken", false); err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		rewritten = encoded
	case appserver.MethodTurnInterrupt, "turn/steer":
		if rpcErr := c.authorizeTUITurnControl(method, params); rpcErr != nil {
			return nil, nil, rpcErr
		}
	case appserver.MethodThreadUnsubscribe,
		"thread/read", "thread/turns/list", "thread/items/list",
		"thread/name/set", "thread/metadata/update", "thread/memoryMode/set",
		"thread/goal/get", "thread/backgroundTerminals/list":
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		threadID, err := threadIDFromObject(object)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if threadID != c.threadID {
			return nil, nil, wrongTUIThread(method, threadID, c.threadID)
		}
	case "mcpServer/resource/read", "mcpServerStatus/list", "app/list", "experimentalFeature/list":
		object, err := tuiRequestObject(params)
		if err != nil {
			return nil, nil, invalidTUIParams(method, err)
		}
		if rawThreadID, ok := object["threadId"]; ok && string(rawThreadID) != "null" {
			var threadID string
			if err := json.Unmarshal(rawThreadID, &threadID); err != nil || threadID == "" {
				return nil, nil, invalidTUIParams(method, errors.New("threadId must be a nonempty string or null"))
			}
			if threadID != c.threadID {
				return nil, nil, wrongTUIThread(method, threadID, c.threadID)
			}
		}
	case "configRequirements/read",
		"model/list", "modelProvider/capabilities/read",
		"collaborationMode/list",
		"account/rateLimits/read", "account/usage/read", "account/workspaceMessages/read",
		"plugin/read", "plugin/skill/read", "plugin/share/list",
		"environment/info",
		"thread/realtime/listVoices", "fuzzyFileSearch/sessionUpdate", "fuzzyFileSearch/sessionStop":
		// These allowlisted operations either read state or update or stop a
		// project search session. They do not mutate managed turn ownership.
	default:
		return nil, nil, &appserver.RPCError{
			Code:    appserver.ErrorCodeInvalidRequest,
			Message: method + " is unavailable while attached to an Intercom-managed thread",
		}
	}
	return rewritten, nil, nil
}

func (c *controller) authorizeTUITurnControl(method string, params json.RawMessage) *appserver.RPCError {
	object, err := tuiRequestObject(params)
	if err != nil {
		return invalidTUIParams(method, err)
	}
	threadID, err := threadIDFromObject(object)
	if err != nil {
		return invalidTUIParams(method, err)
	}
	turnField := "turnId"
	if method == "turn/steer" {
		turnField = "expectedTurnId"
	}
	var requestedTurn string
	raw, ok := object[turnField]
	if !ok || json.Unmarshal(raw, &requestedTurn) != nil {
		return invalidTUIParams(method, fmt.Errorf("%s must be a string", turnField))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if threadID != c.threadID {
		return wrongTUIThread(method, threadID, c.threadID)
	}
	if !c.ready || c.turnOwner != turnOwnerTUI || (c.phase != phaseStarting && c.phase != phaseActive) {
		return &appserver.RPCError{
			Code:    appserver.ErrorCodeInvalidRequest,
			Message: "the attached TUI does not own the managed thread's active turn",
		}
	}
	if method == "turn/steer" && requestedTurn == "" {
		return invalidTUIParams(method, errors.New("expectedTurnId must not be empty"))
	}
	if c.turnID != "" && requestedTurn != c.turnID {
		return &appserver.RPCError{
			Code:    appserver.ErrorCodeInvalidParams,
			Message: fmt.Sprintf("%s targets turn %q; the TUI-owned turn is %q", method, requestedTurn, c.turnID),
		}
	}
	return nil
}

func (c *controller) afterTUIRequest(method string, state any, result json.RawMessage, err error) {
	if method != appserver.MethodTurnStart {
		return
	}
	reservation, ok := state.(tuiTurnReservation)
	if !ok {
		return
	}
	outcome := turnStartResult{err: err}
	defer c.wakeStateChange()
	defer func() { c.finishTUIStartResult(reservation, outcome) }()
	if err != nil {
		var rpcErr *appserver.RPCError
		definitive := errors.As(err, &rpcErr)
		c.mu.Lock()
		if definitive && c.turnOwner == turnOwnerTUI && c.phase == phaseStarting && c.turnID == "" {
			c.phase = phaseIdle
			c.turnOwner = turnOwnerNone
			c.startAmbiguous = false
			c.turnTerminalSeen = false
			c.turnTerminalProcessed = false
			c.turnStartResponseSeen = false
			c.mu.Unlock()
			return
		}
		phase := c.phase
		c.mu.Unlock()
		if definitive {
			c.signalFatal(fmt.Errorf("codex: TUI turn/start was rejected after lifecycle activity while controller is %s: %w", phase, err))
		} else {
			c.signalFatal(fmt.Errorf("codex: TUI turn/start failed with an ambiguous lifecycle outcome: %w", err))
		}
		return
	}
	var response appserver.TurnStartResponse
	if decodeErr := json.Unmarshal(result, &response); decodeErr != nil {
		outcome.err = fmt.Errorf("codex: decode TUI turn/start response: %w", decodeErr)
		c.signalFatal(outcome.err)
		return
	}
	outcome.response = response
	c.mu.Lock()
	var fatalErr error
	releaseProxyEvents := false
	if c.turnOwner != turnOwnerTUI || (c.phase != phaseStarting && c.phase != phaseActive && c.phase != phaseAwaitingStartResponse) {
		fatalErr = fmt.Errorf("codex: TUI turn/start response arrived while controller is %s", c.phase)
	} else if response.Turn.ID == "" {
		fatalErr = errors.New("codex: TUI turn/start returned an empty turn id")
	} else if c.turnID != "" && c.turnID != response.Turn.ID {
		fatalErr = fmt.Errorf("codex: TUI turn response %s does not match event %s", response.Turn.ID, c.turnID)
	} else if response.Turn.Status != appserver.TurnStatusInProgress {
		fatalErr = fmt.Errorf("codex: TUI turn %s started with status %q", response.Turn.ID, response.Turn.Status)
	} else if c.turnTerminalSeen {
		c.turnStartResponseSeen = true
		if c.turnTerminalProcessed {
			c.settleCompletedTurnLocked()
			releaseProxyEvents = true
		} else {
			c.phase = phaseCompleting
		}
	} else {
		c.turnID = response.Turn.ID
		c.phase = phaseActive
		c.startAmbiguous = false
		c.turnStartResponseSeen = true
	}
	c.mu.Unlock()
	if releaseProxyEvents {
		c.releaseProxyEventGate()
	}
	if fatalErr != nil {
		c.signalFatal(fatalErr)
	}
}

func (c *controller) finishTUIStartResult(reservation tuiTurnReservation, result turnStartResult) {
	if reservation.result == nil {
		return
	}
	reservation.result <- result
	c.mu.Lock()
	if c.startResult == reservation.result {
		c.startResult = nil
	}
	c.mu.Unlock()
}

func (c *controller) reserveTUITurn() (tuiTurnReservation, *appserver.RPCError) {
	c.mu.Lock()
	if !c.ready {
		c.mu.Unlock()
		return tuiTurnReservation{}, &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "Intercom peer is not ready"}
	}
	if c.phase != phaseIdle {
		c.mu.Unlock()
		return tuiTurnReservation{}, &appserver.RPCError{Code: appserver.ErrorCodeInvalidRequest, Message: "managed thread already has an active turn"}
	}
	result := make(chan turnStartResult, 1)
	c.phase = phaseStarting
	c.turnOwner = turnOwnerTUI
	c.turnID = ""
	c.current = wire.Deliver{}
	c.outboundCount = 0
	c.startResult = result
	c.startAmbiguous = true
	c.turnTerminalSeen = false
	c.turnTerminalProcessed = false
	c.turnStartResponseSeen = false
	c.mu.Unlock()
	c.wakeStateChange()
	return tuiTurnReservation{result: result}, nil
}

func tuiRequestObject(params json.RawMessage) (map[string]json.RawMessage, error) {
	if len(params) == 0 || string(params) == "null" {
		return nil, errors.New("params must be an object")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(params, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("params must be an object")
	}
	return object, nil
}

func threadIDFromObject(object map[string]json.RawMessage) (string, error) {
	raw, ok := object["threadId"]
	if !ok {
		return "", nil
	}
	var threadID string
	if err := json.Unmarshal(raw, &threadID); err != nil {
		return "", errors.New("threadId must be a string")
	}
	return threadID, nil
}

func setTUIField(object map[string]json.RawMessage, field string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	object[field] = encoded
	return nil
}

func combinedTUIDeveloperInstructions(raw json.RawMessage, binding string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return binding, nil
	}
	var client string
	if err := json.Unmarshal(raw, &client); err != nil {
		return "", errors.New("developerInstructions must be a string or null")
	}
	if strings.TrimSpace(client) == "" {
		return binding, nil
	}
	return client + "\n\n" + binding, nil
}

func invalidTUIParams(method string, err error) *appserver.RPCError {
	return &appserver.RPCError{Code: appserver.ErrorCodeInvalidParams, Message: fmt.Sprintf("invalid %s params: %v", method, err)}
}

func wrongTUIThread(method, got, want string) *appserver.RPCError {
	return &appserver.RPCError{Code: appserver.ErrorCodeInvalidParams, Message: fmt.Sprintf("%s targets thread %q; managed thread is %q", method, got, want)}
}

var appServerUserAgentVersion = regexp.MustCompile(`^[^/[:space:]]+/(\d+\.\d+\.\d+)(?:[[:space:]]|$)`)

func validateServerVersion(userAgent string) (string, error) {
	match := appServerUserAgentVersion.FindStringSubmatch(userAgent)
	if len(match) != 2 {
		return "", fmt.Errorf("codex: cannot determine app-server version from user agent %q", userAgent)
	}
	comparison, err := compareSemanticVersions(match[1], appserver.MinimumSupportedVersion)
	if err != nil {
		return "", fmt.Errorf("codex: compare app-server version: %w", err)
	}
	if comparison < 0 {
		return "", fmt.Errorf("codex: unsupported app-server version %s (requires %s or later)", match[1], appserver.MinimumSupportedVersion)
	}
	return match[1], nil
}

func compareSemanticVersions(left, right string) (int, error) {
	leftParts, err := parseSemanticVersion(left)
	if err != nil {
		return 0, err
	}
	rightParts, err := parseSemanticVersion(right)
	if err != nil {
		return 0, err
	}
	for i := range leftParts {
		if leftParts[i] < rightParts[i] {
			return -1, nil
		}
		if leftParts[i] > rightParts[i] {
			return 1, nil
		}
	}
	return 0, nil
}

func parseSemanticVersion(version string) ([3]uint64, error) {
	var parsed [3]uint64
	parts := strings.Split(version, ".")
	if len(parts) != len(parsed) {
		return parsed, fmt.Errorf("invalid semantic version %q", version)
	}
	for i, part := range parts {
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return parsed, fmt.Errorf("invalid semantic version %q: %w", version, err)
		}
		parsed[i] = value
	}
	return parsed, nil
}

func (c *controller) validateStoredBinding(init appserver.InitializeResponse) error {
	state := c.state
	if state.Peer != c.cfg.Name {
		return fmt.Errorf("codex: state belongs to peer %q, not %q; use --new to replace the binding", state.Peer, c.cfg.Name)
	}
	if state.CWD != c.cfg.CWD {
		return fmt.Errorf("codex: state cwd is %q, not %q; use --new to replace it", state.CWD, c.cfg.CWD)
	}
	if state.CodexHome != init.CodexHome {
		return fmt.Errorf("codex: app-server CODEX_HOME changed from %q to %q; use --new to replace the binding", state.CodexHome, init.CodexHome)
	}
	return nil
}

func (c *controller) acquireThreadLock(codexHome, threadID string) error {
	if c.threadLock != nil {
		return errors.New("codex: managed thread lock is already held")
	}
	resolve := c.cfg.threadLockPath
	if resolve == nil {
		resolve = paths.CodexThreadLock
	}
	lockPath, err := resolve(codexHome, threadID)
	if err != nil {
		return err
	}
	lock, err := AcquireThreadLock(lockPath)
	if err != nil {
		return fmt.Errorf("codex: lock thread %s: %w", threadID, err)
	}
	c.threadLock = lock
	return nil
}

func (c *controller) managedMCPConfig() map[string]any {
	startupSeconds := int(c.cfg.ControlTimeout.Round(time.Second) / time.Second)
	if startupSeconds < 1 {
		startupSeconds = 1
	}
	toolSeconds := int(c.cfg.ReverseTimeout.Round(time.Second) / time.Second)
	if toolSeconds < 1 {
		toolSeconds = 1
	}
	return map[string]any{
		"mcp_servers." + managedMCPServerName: map[string]any{
			"command":                      c.cfg.IntercomBin,
			"args":                         []string{"codex-mcp-bridge", "--socket", c.cfg.MCPBridgeSocket, "--timeout", c.cfg.ReverseTimeout.String()},
			"env":                          map[string]string{bridgeTokenEnvironment: c.bridgeToken},
			"required":                     true,
			"supports_parallel_tool_calls": true,
			"startup_timeout_sec":          startupSeconds,
			"tool_timeout_sec":             toolSeconds,
			"default_tools_approval_mode":  "approve",
			"enabled_tools":                []string{"send_message", "list_peers"},
		},
	}
}

func (c *controller) releaseThreadLock() error {
	lock := c.threadLock
	c.threadLock = nil
	if lock == nil {
		return nil
	}
	return lock.Close()
}

func (c *controller) startNewThread(ctx context.Context, init appserver.InitializeResponse, version string) error {
	cwd := c.cfg.CWD
	developer := developerInstructions(c.cfg.Name)
	ephemeral := false
	sandbox := c.cfg.ExecutionPolicy.sandboxMode()
	reviewer := appserver.ApprovalsReviewerUser
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	response, err := c.app.ThreadStart(control, appserver.ThreadStartParams{
		CWD:                   &cwd,
		RuntimeWorkspaceRoots: []string{cwd},
		ApprovalPolicy:        string(appserver.ApprovalNever),
		ApprovalsReviewer:     &reviewer,
		Sandbox:               &sandbox,
		DeveloperInstructions: &developer,
		Ephemeral:             &ephemeral,
		DynamicTools:          dynamicToolSpecs(),
	})
	cancel()
	if err != nil {
		return fmt.Errorf("codex: start thread: %w", err)
	}
	if err := c.acceptThread(response.Thread, response.CWD, response.RuntimeWorkspaceRoots, response.ApprovalPolicy, response.ApprovalsReviewer, response.Sandbox); err != nil {
		return err
	}
	if err := c.acquireThreadLock(init.CodexHome, response.Thread.ID); err != nil {
		return err
	}
	c.state = &ManagedState{
		SchemaVersion:       StateSchemaVersion,
		Peer:                c.cfg.Name,
		ThreadID:            response.Thread.ID,
		CWD:                 c.cfg.CWD,
		CodexHome:           init.CodexHome,
		ServerUserAgent:     init.UserAgent,
		CodexVersion:        version,
		ToolContractVersion: ToolContractVersion,
		ToolTransport:       ToolTransportDynamic,
		Materialized:        false,
	}
	if err := c.store.Save(*c.state); err != nil {
		return fmt.Errorf("codex: persist new thread binding: %w", err)
	}
	if err := c.setSyntheticResume(response); err != nil {
		return err
	}
	return nil
}

func (c *controller) adoptThread(ctx context.Context, init appserver.InitializeResponse, version string) error {
	threadID := c.cfg.AdoptThreadID
	if err := codexsession.ValidateID(threadID); err != nil {
		return fmt.Errorf("codex: adopt session: %w", err)
	}
	if err := c.acquireThreadLock(init.CodexHome, threadID); err != nil {
		return err
	}
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	candidate, err := codexsession.Read(control, c.app, threadID, codexsession.Options{CWD: c.cfg.CWD})
	cancel()
	if err != nil {
		return fmt.Errorf("codex: adopt session %s: %w", threadID, err)
	}
	if candidate.Thread.ParentThreadID != nil {
		return fmt.Errorf("codex: adopt session %s: subagent threads cannot be adopted as managed roots", threadID)
	}
	if candidate.Thread.Status.Type == appserver.ThreadStatusActive || candidate.Thread.Status.Type == appserver.ThreadStatusSystemError {
		return fmt.Errorf("codex: adopt session %s: thread status is %s, want idle or notLoaded", threadID, candidate.Thread.Status.Type)
	}

	desired := c.managedState(init, version, threadID, ToolTransportMCPBridge, true)
	c.state = desired
	cwd := c.cfg.CWD
	developer := developerInstructions(c.cfg.Name)
	sandbox := c.cfg.ExecutionPolicy.sandboxMode()
	reviewer := appserver.ApprovalsReviewerUser
	control, cancel = context.WithTimeout(ctx, c.cfg.ControlTimeout)
	response, err := c.app.ThreadResume(control, appserver.ThreadResumeParams{
		ThreadID:              threadID,
		CWD:                   &cwd,
		RuntimeWorkspaceRoots: []string{cwd},
		ApprovalPolicy:        string(appserver.ApprovalNever),
		ApprovalsReviewer:     &reviewer,
		Sandbox:               &sandbox,
		Config:                c.managedMCPConfig(),
		DeveloperInstructions: &developer,
		ExcludeTurns:          true,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("codex: adopt session %s: resume: %w", threadID, err)
	}
	if err := c.acceptThread(response.Thread, response.CWD, response.RuntimeWorkspaceRoots, response.ApprovalPolicy, response.ApprovalsReviewer, response.Sandbox); err != nil {
		return err
	}
	if err := c.verifyManagedMCP(ctx); err != nil {
		return err
	}
	c.pendingState = desired
	return nil
}

func (c *controller) forkThread(ctx context.Context, init appserver.InitializeResponse, version string) error {
	sourceID := c.cfg.ForkThreadID
	if err := codexsession.ValidateID(sourceID); err != nil {
		return fmt.Errorf("codex: fork session: %w", err)
	}
	if err := c.acquireThreadLock(init.CodexHome, sourceID); err != nil {
		return err
	}
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	candidate, err := codexsession.Read(control, c.app, sourceID, codexsession.Options{CWD: c.cfg.CWD})
	cancel()
	if err != nil {
		return fmt.Errorf("codex: fork session %s: %w", sourceID, err)
	}
	if candidate.Thread.ParentThreadID != nil {
		return fmt.Errorf("codex: fork session %s: subagent threads cannot be selected as managed roots", sourceID)
	}
	if candidate.Thread.Status.Type == appserver.ThreadStatusActive || candidate.Thread.Status.Type == appserver.ThreadStatusSystemError {
		return fmt.Errorf("codex: fork session %s: thread status is %s, want idle or notLoaded", sourceID, candidate.Thread.Status.Type)
	}
	cwd := c.cfg.CWD
	developer := developerInstructions(c.cfg.Name)
	sandbox := c.cfg.ExecutionPolicy.sandboxMode()
	reviewer := appserver.ApprovalsReviewerUser
	control, cancel = context.WithTimeout(ctx, c.cfg.ControlTimeout)
	response, err := c.app.ThreadFork(control, appserver.ThreadForkParams{
		ThreadID:              sourceID,
		CWD:                   &cwd,
		RuntimeWorkspaceRoots: []string{cwd},
		ApprovalPolicy:        string(appserver.ApprovalNever),
		ApprovalsReviewer:     &reviewer,
		Sandbox:               &sandbox,
		Config:                c.managedMCPConfig(),
		DeveloperInstructions: &developer,
		Ephemeral:             false,
		ExcludeTurns:          true,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("codex: fork session %s: %w", sourceID, err)
	}
	if response.Thread.ID == "" || response.Thread.ID == sourceID {
		return fmt.Errorf("codex: fork session %s: app-server returned invalid fork id %q", sourceID, response.Thread.ID)
	}
	if response.Thread.ForkedFromID == nil || *response.Thread.ForkedFromID != sourceID {
		return fmt.Errorf("codex: fork session %s: app-server returned forkedFromId %#v", sourceID, response.Thread.ForkedFromID)
	}
	if err := c.releaseThreadLock(); err != nil {
		return fmt.Errorf("codex: release source session lock: %w", err)
	}
	if err := c.acquireThreadLock(init.CodexHome, response.Thread.ID); err != nil {
		return err
	}
	desired := c.managedState(init, version, response.Thread.ID, ToolTransportMCPBridge, true)
	c.state = desired
	if err := c.acceptThread(response.Thread, response.CWD, response.RuntimeWorkspaceRoots, response.ApprovalPolicy, response.ApprovalsReviewer, response.Sandbox); err != nil {
		return err
	}
	if err := c.verifyManagedMCP(ctx); err != nil {
		return err
	}
	c.pendingState = desired
	return nil
}

func (c *controller) managedState(init appserver.InitializeResponse, version, threadID string, transport ToolTransport, materialized bool) *ManagedState {
	return &ManagedState{
		SchemaVersion:       StateSchemaVersion,
		Peer:                c.cfg.Name,
		ThreadID:            threadID,
		CWD:                 c.cfg.CWD,
		CodexHome:           init.CodexHome,
		ServerUserAgent:     init.UserAgent,
		CodexVersion:        version,
		ToolContractVersion: ToolContractVersion,
		ToolTransport:       transport,
		Materialized:        materialized,
	}
}

func (c *controller) verifyManagedMCP(ctx context.Context) error {
	threadID := c.threadID
	detail := appserver.MCPServerStatusToolsAndAuthOnly
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	defer cancel()
	var cursor *string
	seen := make(map[string]struct{})
	for {
		response, err := c.app.MCPServerStatusList(control, appserver.MCPServerStatusListParams{
			Cursor: cursor, Detail: &detail, ThreadID: &threadID,
		})
		if err != nil {
			return fmt.Errorf("codex: verify managed MCP server: %w", err)
		}
		for _, server := range response.Data {
			if server.Name != managedMCPServerName {
				continue
			}
			for _, tool := range []string{"send_message", "list_peers"} {
				if _, ok := server.Tools[tool]; !ok {
					return fmt.Errorf("codex: managed MCP server is missing required tool %q", tool)
				}
			}
			return nil
		}
		if response.NextCursor == nil {
			return fmt.Errorf("codex: managed MCP server %q is not active for thread %s", managedMCPServerName, threadID)
		}
		if *response.NextCursor == "" {
			return errors.New("codex: managed MCP status pagination returned an invalid cursor")
		}
		if _, duplicate := seen[*response.NextCursor]; duplicate {
			return errors.New("codex: managed MCP status pagination repeated a cursor")
		}
		seen[*response.NextCursor] = struct{}{}
		cursor = response.NextCursor
	}
}

func (c *controller) resumeOrReplace(ctx context.Context, init appserver.InitializeResponse, version string) error {
	if err := c.acquireThreadLock(init.CodexHome, c.state.ThreadID); err != nil {
		return err
	}
	cwd := c.cfg.CWD
	developer := developerInstructions(c.cfg.Name)
	sandbox := c.cfg.ExecutionPolicy.sandboxMode()
	reviewer := appserver.ApprovalsReviewerUser
	var requestConfig map[string]any
	if c.state.ToolTransport == ToolTransportMCPBridge {
		requestConfig = c.managedMCPConfig()
	}
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	response, err := c.app.ThreadResume(control, appserver.ThreadResumeParams{
		ThreadID:              c.state.ThreadID,
		CWD:                   &cwd,
		RuntimeWorkspaceRoots: []string{cwd},
		ApprovalPolicy:        string(appserver.ApprovalNever),
		ApprovalsReviewer:     &reviewer,
		Sandbox:               &sandbox,
		Config:                requestConfig,
		DeveloperInstructions: &developer,
		ExcludeTurns:          true,
	})
	cancel()
	if err != nil {
		if !c.state.Materialized && isMissingRollout(err, c.state.ThreadID) {
			c.logger.Info("replace unmaterialized Codex thread", "old_thread", c.state.ThreadID)
			if lockErr := c.releaseThreadLock(); lockErr != nil {
				return errors.Join(fmt.Errorf("codex: release missing thread lock: %w", lockErr), err)
			}
			c.state = nil
			return c.startNewThread(ctx, init, version)
		}
		return fmt.Errorf("codex: resume thread %s: %w", c.state.ThreadID, err)
	}
	if err := c.acceptThread(response.Thread, response.CWD, response.RuntimeWorkspaceRoots, response.ApprovalPolicy, response.ApprovalsReviewer, response.Sandbox); err != nil {
		return err
	}
	if c.state.ToolTransport == ToolTransportMCPBridge {
		if err := c.verifyManagedMCP(ctx); err != nil {
			return err
		}
	}
	if !c.state.Materialized {
		if _, err := c.confirmMaterialized(ctx, false); err != nil {
			return fmt.Errorf("codex: verify pending thread materialization: %w", err)
		}
	}
	if c.state.ServerUserAgent != init.UserAgent || c.state.CodexVersion != version {
		updated := *c.state
		updated.ServerUserAgent = init.UserAgent
		updated.CodexVersion = version
		if err := c.store.Save(updated); err != nil {
			return fmt.Errorf("codex: persist validated app-server diagnostics: %w", err)
		}
		c.state = &updated
	}
	return nil
}

func isMissingRollout(err error, threadID string) bool {
	var rpcErr *appserver.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == appserver.ErrorCodeInvalidRequest &&
		rpcErr.Message == "no rollout found for thread id "+threadID
}

func (c *controller) acceptThread(thread appserver.Thread, cwd string, runtimeWorkspaceRoots []string, approval any, reviewer appserver.ApprovalsReviewer, sandbox appserver.SandboxPolicy) error {
	if thread.ID == "" || thread.ID != c.stateThreadIDOr(thread.ID) {
		return fmt.Errorf("codex: app-server returned unexpected thread id %q", thread.ID)
	}
	if filepath.Clean(cwd) != c.cfg.CWD || filepath.Clean(thread.CWD) != c.cfg.CWD {
		return fmt.Errorf("codex: app-server returned cwd %q/%q, want %q", cwd, thread.CWD, c.cfg.CWD)
	}
	if len(runtimeWorkspaceRoots) != 1 || filepath.Clean(runtimeWorkspaceRoots[0]) != c.cfg.CWD {
		return fmt.Errorf("codex: app-server runtime workspace roots are %v, want [%s]", runtimeWorkspaceRoots, c.cfg.CWD)
	}
	if thread.Ephemeral {
		return errors.New("codex: app-server returned an ephemeral managed thread")
	}
	if thread.Status.Type != appserver.ThreadStatusIdle {
		return fmt.Errorf("codex: managed thread is %s, want idle; recycle the dedicated service group", thread.Status.Type)
	}
	policy, ok := approval.(string)
	if !ok || policy != string(appserver.ApprovalNever) {
		return fmt.Errorf("codex: app-server approval policy is %#v, want never", approval)
	}
	if reviewer != appserver.ApprovalsReviewerUser {
		return fmt.Errorf("codex: app-server approvals reviewer is %q, want user", reviewer)
	}
	wantSandbox := c.cfg.ExecutionPolicy.sandboxType()
	if sandbox.Type != wantSandbox {
		return fmt.Errorf("codex: app-server sandbox is %q, want %s", sandbox.Type, wantSandbox)
	}
	if len(sandbox.WritableRoots) != 0 {
		return fmt.Errorf("codex: app-server sandbox grants additional writable roots: %v", sandbox.WritableRoots)
	}
	if wantSandbox == "workspaceWrite" {
		if _, ok := sandbox.NetworkAccess.(bool); !ok {
			return fmt.Errorf("codex: app-server workspace-write networkAccess has type %T, want bool", sandbox.NetworkAccess)
		}
	} else if sandbox.NetworkAccess != nil {
		return fmt.Errorf("codex: app-server danger-full-access returned unexpected networkAccess %#v", sandbox.NetworkAccess)
	}
	c.mu.Lock()
	c.threadID = thread.ID
	c.sandbox = sandbox
	c.mu.Unlock()
	return nil
}

func (c *controller) stateThreadIDOr(fallback string) string {
	if c.state == nil {
		return fallback
	}
	return c.state.ThreadID
}

func (c *controller) confirmMaterialized(ctx context.Context, retry bool) (bool, error) {
	deadline := time.Now().Add(c.cfg.ControlTimeout)
	for {
		control, cancel := context.WithDeadline(ctx, deadline)
		response, err := c.app.ThreadRead(control, appserver.ThreadReadParams{ThreadID: c.threadID, IncludeTurns: true})
		cancel()
		if err == nil {
			if response.Thread.ID != c.threadID {
				return false, fmt.Errorf("codex: thread/read returned thread %q, want %q", response.Thread.ID, c.threadID)
			}
			if !c.state.Materialized {
				updated := *c.state
				updated.Materialized = true
				if err := c.store.Save(updated); err != nil {
					return false, err
				}
				c.state = &updated
			}
			c.clearSyntheticResume()
			return true, nil
		}
		if !isBeforeFirstUserMessage(err, c.threadID) {
			return false, err
		}
		if !retry {
			return false, nil
		}
		if time.Until(deadline) <= 0 || ctx.Err() != nil {
			return false, err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func isBeforeFirstUserMessage(err error, threadID string) bool {
	var rpcErr *appserver.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == appserver.ErrorCodeInvalidRequest &&
		rpcErr.Message == "thread "+threadID+" is not materialized yet; includeTurns is unavailable before first user message"
}

func (c *controller) loop(ctx context.Context) error {
	var watchdog *time.Timer
	var pendingDelivery *wire.Deliver
	for {
		phase := c.currentPhase()
		if phase == phaseIdle && pendingDelivery != nil {
			err := c.startDelivery(ctx, *pendingDelivery)
			if errors.Is(err, errTurnAlreadyReserved) {
				continue
			}
			pendingDelivery = nil
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.setFailed()
				return err
			}
			continue
		}
		var delivery <-chan wire.Deliver
		if phase == phaseIdle && pendingDelivery == nil {
			delivery = c.deliveries
		}
		var watchdogC <-chan time.Time
		if phase == phaseActive {
			if watchdog == nil {
				watchdog = time.NewTimer(c.cfg.ActivityTimeout)
			}
			watchdogC = watchdog.C
		} else if watchdog != nil {
			if !watchdog.Stop() {
				select {
				case <-watchdog.C:
				default:
				}
			}
			watchdog = nil
		}
		var proxyDone <-chan struct{}
		if proxy := c.currentProxy(); proxy != nil {
			proxyDone = proxy.Done()
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.app.Done():
			err := c.app.Wait()
			if err == nil {
				err = appserver.ErrClosed
			}
			return fmt.Errorf("codex: app-server disconnected: %w", err)
		case <-proxyDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("codex: TUI proxy listener stopped")
		case err := <-c.fatal:
			c.setFailed()
			return err
		case d := <-delivery:
			if err := c.startDelivery(ctx, d); err != nil {
				if errors.Is(err, errTurnAlreadyReserved) {
					pending := d
					pendingDelivery = &pending
					continue
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.setFailed()
				return err
			}
		case notification := <-c.notifications:
			if err := c.finishNotification(ctx, notification); err != nil {
				c.setFailed()
				return err
			}
		case event := <-c.broker.ConnectionEvents():
			c.handleBrokerEvent(ctx, event)
		case <-c.reconnectDone:
			c.mu.Lock()
			pending := c.reconnectPending
			c.reconnectPending = false
			if !pending {
				c.reconnecting = false
			}
			c.mu.Unlock()
			if pending {
				go c.reconnectBroker(ctx)
			}
		case <-c.activity:
			if watchdog != nil {
				if !watchdog.Stop() {
					select {
					case <-watchdog.C:
					default:
					}
				}
				watchdog.Reset(c.cfg.ActivityTimeout)
			}
		case <-c.stateChanged:
			// Rebuild phase-dependent select cases after a proxy request changes
			// turn ownership or returns the controller to idle.
		case <-watchdogC:
			c.setFailed()
			return fmt.Errorf("codex: active turn had no app-server activity for %s", c.cfg.ActivityTimeout)
		}
	}
}

func (c *controller) startDelivery(ctx context.Context, delivery wire.Deliver) error {
	if delivery.ID == "" {
		delivery.ID = wire.NewID()
	}
	sent, err := time.Parse(time.RFC3339Nano, delivery.Timestamp)
	if err != nil {
		c.logger.Warn("invalid broker delivery timestamp", "id", delivery.ID, "err", err)
		sent = time.Now().UTC()
	}
	c.mu.Lock()
	if c.phase != phaseIdle {
		c.mu.Unlock()
		return errTurnAlreadyReserved
	}
	c.phase = phaseStarting
	c.turnOwner = turnOwnerIntercom
	c.turnID = ""
	c.current = delivery
	c.outboundCount = 0
	c.startResult = nil
	c.startAmbiguous = false
	c.turnTerminalSeen = false
	c.turnTerminalProcessed = false
	c.turnStartResponseSeen = false
	sandbox := c.sandbox
	threadID := c.threadID
	reviewer := appserver.ApprovalsReviewerUser
	c.mu.Unlock()

	clientID := delivery.ID
	cwd := c.cfg.CWD
	writeCtx, cancelWrite := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	await, err := c.app.StartTurn(writeCtx, appserver.TurnStartParams{
		ThreadID:              threadID,
		ClientUserMessageID:   &clientID,
		Input:                 []appserver.UserInput{appserver.TextInput(inboundEnvelope(delivery.From, delivery.ID, delivery.Message, sent))},
		CWD:                   &cwd,
		RuntimeWorkspaceRoots: []string{cwd},
		ApprovalPolicy:        string(appserver.ApprovalNever),
		ApprovalsReviewer:     &reviewer,
		SandboxPolicy:         &sandbox,
	})
	cancelWrite()
	if err != nil {
		return fmt.Errorf("codex: start delivery %s: %w", delivery.ID, err)
	}
	result := make(chan turnStartResult, 1)
	awaitCtx, cancelAwait := context.WithTimeout(context.Background(), c.cfg.ControlTimeout)
	go func() {
		response, err := await(awaitCtx)
		cancelAwait()
		result <- turnStartResult{response: response, err: err}
	}()
	c.mu.Lock()
	c.startResult = result
	c.startAmbiguous = true
	c.mu.Unlock()

	var started turnStartResult
	select {
	case started = <-result:
		c.mu.Lock()
		if c.startResult == result {
			c.startResult = nil
		}
		c.mu.Unlock()
	case <-ctx.Done():
		return ctx.Err()
	}
	if started.err != nil {
		if !turnStartMayHaveSucceeded(started.err) {
			c.mu.Lock()
			c.startAmbiguous = false
			c.mu.Unlock()
		}
		return fmt.Errorf("codex: start delivery %s: %w", delivery.ID, started.err)
	}
	response := started.response
	c.touchActivity()
	c.mu.Lock()
	if c.turnID != "" && c.turnID != response.Turn.ID {
		got := c.turnID
		c.mu.Unlock()
		return fmt.Errorf("codex: turn id mismatch for delivery %s: event %s, response %s", delivery.ID, got, response.Turn.ID)
	}
	if response.Turn.ID == "" {
		c.mu.Unlock()
		return fmt.Errorf("codex: empty turn id for delivery %s", delivery.ID)
	}
	c.turnID = response.Turn.ID
	c.startAmbiguous = false
	if response.Turn.Status != appserver.TurnStatusInProgress {
		c.mu.Unlock()
		return fmt.Errorf("codex: turn %s started with status %q, want %q", response.Turn.ID, response.Turn.Status, appserver.TurnStatusInProgress)
	}
	completedBeforeResponse := c.turnTerminalSeen
	c.turnStartResponseSeen = true
	releaseProxyEvents := false
	if completedBeforeResponse {
		if c.turnTerminalProcessed {
			c.settleCompletedTurnLocked()
			releaseProxyEvents = true
		} else {
			c.phase = phaseCompleting
		}
	} else {
		c.phase = phaseActive
	}
	c.mu.Unlock()
	if completedBeforeResponse {
		if releaseProxyEvents {
			c.releaseProxyEventGate()
		}
		return nil
	}
	if c.cfg.onTurnActive != nil {
		c.cfg.onTurnActive()
	}
	c.logger.Info("codex turn started", "delivery", delivery.ID, "from", delivery.From, "thread", threadID, "turn", response.Turn.ID)
	return nil
}

func (c *controller) applyNotification(notification appserver.Notification) (bool, error) {
	switch notification.Method {
	case appserver.NotificationThreadStarted:
		var params appserver.ThreadStartedNotification
		if err := notification.DecodeParams(&params); err != nil {
			return false, err
		}
		if params.Thread.ID == "" {
			return false, errors.New("codex: thread/started carried an empty thread id")
		}
		c.observeStartedThread(params.Thread)
		if params.Thread.ID != c.managedThreadID() {
			return false, nil
		}
	case appserver.NotificationTurnStarted:
		var params appserver.TurnStartedNotification
		if err := notification.DecodeParams(&params); err != nil {
			return false, err
		}
		if params.ThreadID == "" {
			return false, errors.New("codex: turn/started carried an empty thread id")
		}
		if params.ThreadID != c.managedThreadID() {
			return false, nil
		}
		if err := c.reconcileTurn(params.ThreadID, params.Turn.ID, params.Turn.Status); err != nil {
			return false, err
		}
		// Once a managed turn is observable, an attach must resume upstream so
		// the TUI receives that turn's current snapshot. The zero-turn synthetic
		// response is valid only before the first lifecycle event.
		c.clearSyntheticResume()
	case appserver.NotificationTurnCompleted:
		var params appserver.TurnCompletedNotification
		if err := notification.DecodeParams(&params); err != nil {
			return false, err
		}
		if params.ThreadID == "" {
			return false, errors.New("codex: turn/completed carried an empty thread id")
		}
		if params.ThreadID != c.managedThreadID() {
			return false, nil
		}
		if err := c.completeTurn(params); err != nil {
			return false, err
		}
		c.clearSyntheticResume()
		return true, nil
	case appserver.NotificationError:
		var params appserver.ErrorNotification
		if err := notification.DecodeParams(&params); err != nil {
			return false, err
		}
		c.logger.Warn("app-server turn error", "thread", params.ThreadID, "turn", params.TurnID, "retry", params.WillRetry, "err", params.Error.Message)
	}
	return false, nil
}

func (c *controller) finishNotification(ctx context.Context, queued queuedNotification) error {
	managedCompletion := queued.managedCompletion
	if !queued.applied {
		var err error
		managedCompletion, err = c.applyNotification(queued.notification)
		if err != nil {
			return err
		}
	}
	if !managedCompletion {
		return nil
	}
	if c.state != nil && !c.state.Materialized {
		if _, err := c.confirmMaterialized(ctx, true); err != nil {
			return fmt.Errorf("codex: confirm thread materialization: %w", err)
		}
	}
	releaseProxyEvents, err := c.finishManagedCompletion()
	if err != nil {
		return err
	}
	if releaseProxyEvents {
		c.releaseProxyEventGate()
		c.wakeStateChange()
	}
	return nil
}

func (c *controller) finishManagedCompletion() (bool, error) {
	c.mu.Lock()
	if !c.turnTerminalSeen {
		phase := c.phase
		c.mu.Unlock()
		return false, fmt.Errorf("codex: finish managed completion while controller is %s without a terminal event", phase)
	}
	c.turnTerminalProcessed = true
	if !c.turnStartResponseSeen {
		if c.phase != phaseAwaitingStartResponse {
			phase := c.phase
			c.mu.Unlock()
			return false, fmt.Errorf("codex: finish managed completion while controller is %s awaiting a turn/start response", phase)
		}
		c.mu.Unlock()
		return false, nil
	}
	if c.phase != phaseCompleting {
		phase := c.phase
		c.mu.Unlock()
		return false, fmt.Errorf("codex: finish managed completion while controller is %s", phase)
	}
	c.settleCompletedTurnLocked()
	c.mu.Unlock()
	return true, nil
}

// settleCompletedTurnLocked releases a terminal turn after both its start
// response and controller-side completion processing have finished. c.mu must
// be held by the caller.
func (c *controller) settleCompletedTurnLocked() {
	c.phase = phaseIdle
	c.turnOwner = turnOwnerNone
	c.turnID = ""
	c.startAmbiguous = false
	c.turnTerminalSeen = false
	c.turnTerminalProcessed = false
	c.turnStartResponseSeen = false
}

func (c *controller) handleNotification(ctx context.Context, notification appserver.Notification) error {
	return c.finishNotification(ctx, queuedNotification{notification: notification})
}

func (c *controller) managedThreadID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.threadID
}

func (c *controller) observeStartedThread(thread appserver.Thread) {
	if thread.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if thread.ID == c.threadID {
		return
	}
	isOwnedAncestor := func(candidate *string) bool {
		if candidate == nil || *candidate == "" {
			return false
		}
		if *candidate == c.threadID {
			return true
		}
		_, ok := c.descendantThreads[*candidate]
		return ok
	}
	if !isOwnedAncestor(thread.ParentThreadID) && !isOwnedAncestor(thread.ForkedFromID) {
		return
	}
	if c.descendantThreads == nil {
		c.descendantThreads = make(map[string]struct{})
	}
	c.descendantThreads[thread.ID] = struct{}{}
}

func (c *controller) reconcileTurn(threadID, turnID, status string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if threadID != c.threadID {
		return fmt.Errorf("codex: event thread %s does not match managed thread %s", threadID, c.threadID)
	}
	if c.phase != phaseStarting && c.phase != phaseActive {
		return fmt.Errorf("codex: turn %s started while controller is %s", turnID, c.phase)
	}
	if turnID == "" {
		return errors.New("codex: turn/started carried an empty turn id")
	}
	if status != appserver.TurnStatusInProgress {
		return fmt.Errorf("codex: turn/started for %s has status %q, want %q", turnID, status, appserver.TurnStatusInProgress)
	}
	if c.turnID != "" && c.turnID != turnID {
		return fmt.Errorf("codex: event turn %s does not match active turn %s", turnID, c.turnID)
	}
	c.turnID = turnID
	c.phase = phaseActive
	return nil
}

func (c *controller) completeTurn(params appserver.TurnCompletedNotification) error {
	c.mu.Lock()
	if params.ThreadID != c.threadID {
		c.mu.Unlock()
		return fmt.Errorf("codex: completion thread %s does not match %s", params.ThreadID, c.threadID)
	}
	if c.phase != phaseStarting && c.phase != phaseActive {
		c.mu.Unlock()
		return fmt.Errorf("codex: turn %s completed while controller is %s", params.Turn.ID, c.phase)
	}
	if params.Turn.ID == "" {
		c.mu.Unlock()
		return errors.New("codex: turn/completed carried an empty turn id")
	}
	switch params.Turn.Status {
	case appserver.TurnStatusCompleted, appserver.TurnStatusFailed, appserver.TurnStatusInterrupted:
	default:
		c.mu.Unlock()
		return fmt.Errorf("codex: turn/completed for %s has non-terminal status %q", params.Turn.ID, params.Turn.Status)
	}
	if c.turnID != "" && c.turnID != params.Turn.ID {
		got := c.turnID
		c.mu.Unlock()
		return fmt.Errorf("codex: completion turn %s does not match %s", params.Turn.ID, got)
	}
	delivery := c.current
	outbound := c.outboundCount
	owner := c.turnOwner
	c.turnID = params.Turn.ID
	c.turnTerminalSeen = true
	c.proxyEventGate = true
	if !c.turnStartResponseSeen {
		// The app-server can publish terminal notifications before the proxy
		// goroutine or broker-delivery path receives the corresponding turn/start
		// response. Retain the reservation until that response is validated so no
		// subsequent turn can overtake it.
		c.phase = phaseAwaitingStartResponse
	} else {
		c.phase = phaseCompleting
	}
	c.current = wire.Deliver{}
	c.outboundCount = 0
	c.startAmbiguous = false
	c.mu.Unlock()
	var durationMS any
	if params.Turn.DurationMS != nil {
		durationMS = *params.Turn.DurationMS
	}
	c.logger.Info("codex turn completed", "owner", owner.String(), "delivery", delivery.ID, "from", delivery.From, "turn", params.Turn.ID, "status", params.Turn.Status, "duration_ms", durationMS, "outbound_sends", outbound, "replied", outbound > 0)
	return nil
}

func (c *controller) enqueueDelivery(delivery wire.Deliver) {
	select {
	case c.deliveries <- delivery:
	default:
		c.signalFatal(fmt.Errorf("codex: inbound delivery queue is full (%d)", cap(c.deliveries)))
	}
}

func (c *controller) enqueueNotification(notification appserver.Notification) {
	c.touchActivity()
	queued := queuedNotification{notification: notification}
	switch notification.Method {
	case appserver.NotificationThreadStarted,
		appserver.NotificationTurnStarted,
		appserver.NotificationTurnCompleted,
		appserver.NotificationError:
		managedCompletion, err := c.applyNotification(notification)
		if err != nil {
			c.signalFatal(err)
			return
		}
		queued.applied = true
		queued.managedCompletion = managedCompletion
	default:
		if !c.queueProxyEvent(notification) {
			if proxy := c.currentProxy(); proxy != nil {
				proxy.Notify(notification)
			}
		}
		return
	}
	// Lifecycle state is reconciled on the ordered app-server reader before the
	// TUI observes the event. Materialization reads remain on the controller
	// loop so this callback never waits for a response behind the notification.
	if !c.queueProxyEvent(notification) {
		if proxy := c.currentProxy(); proxy != nil {
			proxy.Notify(notification)
		}
	}
	select {
	case c.notifications <- queued:
	default:
		c.signalFatal(errors.New("codex: app-server notification queue is full"))
	}
}

func (c *controller) queueProxyEvent(notification appserver.Notification) bool {
	c.mu.Lock()
	if !c.proxyEventGate {
		c.mu.Unlock()
		return false
	}
	if len(c.proxyEventQueue) >= proxyEventQueueSize {
		c.mu.Unlock()
		c.signalFatal(errors.New("codex: deferred TUI notification queue is full"))
		return true
	}
	c.proxyEventQueue = append(c.proxyEventQueue, notification)
	c.mu.Unlock()
	return true
}

func (c *controller) releaseProxyEventGate() {
	proxy := c.currentProxy()
	for {
		c.mu.Lock()
		if !c.proxyEventGate {
			c.mu.Unlock()
			return
		}
		if len(c.proxyEventQueue) == 0 {
			c.proxyEventGate = false
			c.mu.Unlock()
			return
		}
		queued := c.proxyEventQueue
		c.proxyEventQueue = nil
		c.mu.Unlock()

		if proxy != nil {
			for _, notification := range queued {
				proxy.Notify(notification)
			}
		}
	}
}

func (c *controller) handleReverseRequest(request *appserver.ReverseRequest) {
	proxy := c.currentProxy()
	if request.Method != appserver.MethodDynamicToolCall && proxy != nil && forwardToTUI(request.Method) {
		parent := c.ctx
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, c.cfg.ActivityTimeout)
		handled, err := proxy.ForwardReverse(ctx, request)
		cancel()
		if handled {
			if err != nil {
				c.signalFatal(fmt.Errorf("codex: relay %s through TUI: %w", request.Method, err))
			}
			return
		}
		if err != nil && !errors.Is(err, appserverproxy.ErrNoAttachedTUI) {
			c.logger.Warn("TUI reverse-request relay failed; apply headless policy", "method", request.Method, "err", err)
		}
	}
	c.reverse.Handle(request)
}

func forwardToTUI(method string) bool {
	switch method {
	case appserver.MethodCommandExecutionApproval,
		appserver.MethodFileChangeApproval,
		appserver.MethodPermissionsApproval,
		appserver.MethodToolRequestUserInput,
		appserver.MethodMCPServerElicitation:
		return true
	default:
		return false
	}
}

func (c *controller) authorizeReverse(ctx context.Context, threadID, turnID string) error {
	c.mu.Lock()
	if !c.ready {
		c.startupViolation = true
		c.mu.Unlock()
		return errors.New("adapter ownership is not established")
	}
	if threadID != c.threadID {
		if _, ok := c.descendantThreads[threadID]; ok {
			c.mu.Unlock()
			return nil
		}
		rootThreadID := c.threadID
		app := c.app
		c.mu.Unlock()
		return c.authorizeDescendant(ctx, app, rootThreadID, threadID)
	}
	if c.phase != phaseStarting && c.phase != phaseActive {
		phase := c.phase
		c.mu.Unlock()
		return fmt.Errorf("tool call arrived while controller is %s", phase)
	}
	if c.turnID != "" && c.turnID != turnID {
		managedTurnID := c.turnID
		c.mu.Unlock()
		return fmt.Errorf("tool call turn %s does not match %s", turnID, managedTurnID)
	}
	c.turnID = turnID
	c.mu.Unlock()
	return nil
}

type descendantCandidate struct {
	threadID string
	path     []string
}

func (c *controller) authorizeDescendant(ctx context.Context, app appServerClient, rootThreadID, requestedThreadID string) error {
	if app == nil {
		return fmt.Errorf("tool call thread %s does not match %s and ancestry cannot be inspected", requestedThreadID, rootThreadID)
	}
	queue := []descendantCandidate{{threadID: requestedThreadID}}
	visited := make(map[string]struct{}, maxDescendantAncestry)
	var firstReadErr error
	for len(queue) > 0 {
		candidate := queue[0]
		queue = queue[1:]
		if candidate.threadID == "" {
			continue
		}

		c.mu.Lock()
		_, cached := c.descendantThreads[candidate.threadID]
		currentRoot := c.threadID
		ready := c.ready
		c.mu.Unlock()
		if !ready || currentRoot != rootThreadID {
			return errors.New("adapter ownership changed while inspecting dynamic tool ancestry")
		}
		if candidate.threadID == rootThreadID || cached {
			c.mu.Lock()
			if c.ready && c.threadID == rootThreadID {
				if c.descendantThreads == nil {
					c.descendantThreads = make(map[string]struct{})
				}
				for _, descendant := range candidate.path {
					c.descendantThreads[descendant] = struct{}{}
				}
				c.mu.Unlock()
				return nil
			}
			c.mu.Unlock()
			return errors.New("adapter ownership changed while caching dynamic tool ancestry")
		}
		if _, seen := visited[candidate.threadID]; seen {
			continue
		}
		if len(visited) >= maxDescendantAncestry {
			return fmt.Errorf("dynamic tool ancestry exceeds %d threads", maxDescendantAncestry)
		}
		visited[candidate.threadID] = struct{}{}

		response, err := app.ThreadRead(ctx, appserver.ThreadReadParams{ThreadID: candidate.threadID})
		if err != nil {
			if firstReadErr == nil {
				firstReadErr = fmt.Errorf("read thread %s: %w", candidate.threadID, err)
			}
			continue
		}
		if response.Thread.ID != candidate.threadID {
			return fmt.Errorf("thread/read for %s returned thread %s", candidate.threadID, response.Thread.ID)
		}
		path := append(append([]string(nil), candidate.path...), candidate.threadID)
		if parent := response.Thread.ParentThreadID; parent != nil && *parent != "" {
			queue = append(queue, descendantCandidate{threadID: *parent, path: path})
		}
		if forkedFrom := response.Thread.ForkedFromID; forkedFrom != nil && *forkedFrom != "" {
			queue = append(queue, descendantCandidate{threadID: *forkedFrom, path: path})
		}
	}
	if firstReadErr != nil {
		return fmt.Errorf("inspect dynamic tool ancestry for thread %s: %w", requestedThreadID, firstReadErr)
	}
	return fmt.Errorf("tool call thread %s has no parent or fork ancestry to %s", requestedThreadID, rootThreadID)
}

func (c *controller) observeReverseRequest(method string) {
	if method != appserver.MethodDynamicToolCall {
		return
	}
	c.mu.Lock()
	if !c.ready {
		c.startupViolation = true
	}
	c.mu.Unlock()
}

func (c *controller) hasStartupViolation() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.startupViolation
}

func (c *controller) noteOutbound() {
	c.mu.Lock()
	if c.phase == phaseStarting || c.phase == phaseActive {
		c.outboundCount++
	}
	c.mu.Unlock()
}

func (c *controller) touchActivity() {
	select {
	case c.activity <- struct{}{}:
	default:
	}
}

func (c *controller) wakeStateChange() {
	select {
	case c.stateChanged <- struct{}{}:
	default:
	}
}

func (c *controller) signalFatal(err error) {
	if err == nil {
		return
	}
	select {
	case c.fatal <- err:
		if c.broker != nil {
			c.startBrokerClose()
		}
	default:
	}
}

func (c *controller) takeFatal() error {
	select {
	case err := <-c.fatal:
		return err
	default:
		return nil
	}
}

func (c *controller) currentPhase() controllerPhase {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase
}

func (c *controller) setFailed() {
	c.mu.Lock()
	c.phase = phaseFailed
	c.ready = false
	c.mu.Unlock()
}

func (c *controller) handleBrokerEvent(ctx context.Context, event brokerclient.ConnectionEvent) {
	if event.State != brokerclient.ConnectionStateDisconnected {
		return
	}
	c.mu.Lock()
	if c.reconnecting {
		c.reconnectPending = true
		c.mu.Unlock()
		return
	}
	c.reconnecting = true
	c.mu.Unlock()
	c.logger.Warn("broker disconnected", "generation", event.Generation, "cause", event.Cause, "reason", event.Reason, "err", event.Err)
	go c.reconnectBroker(ctx)
}

func (c *controller) reconnectBroker(ctx context.Context) {
	delay := 100 * time.Millisecond
	for {
		if err := c.broker.Connect(ctx); err == nil {
			select {
			case c.reconnectDone <- nil:
			default:
			}
			return
		} else {
			c.logger.Warn("reconnect broker", "err", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 5*time.Second {
			delay *= 2
		}
	}
}

func (c *controller) shutdown(stop context.CancelFunc) {
	started := time.Now()
	deadline := started.Add(c.cfg.ControlTimeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	c.mu.Lock()
	c.ready = false
	phase := c.phase
	threadID := c.threadID
	turnID := c.turnID
	hasDelivery := c.current.ID != ""
	startResult := c.startResult
	startAmbiguous := c.startAmbiguous
	turnTerminalSeen := c.turnTerminalSeen
	owner := c.turnOwner
	c.mu.Unlock()
	if c.cfg.OnStopping != nil {
		if err := c.cfg.OnStopping(); err != nil {
			c.logger.Warn("unpublish Codex TUI endpoint during shutdown", "err", err)
		}
	}
	// Stop reconnect attempts before waiting for broker deregistration. Close has
	// no context-aware interface and may be serialized behind an in-flight
	// Connect, so wait for it only within the shared shutdown budget.
	if stop != nil {
		stop()
	}
	// Broker Close has no context. Give it the first half of the single
	// shutdown budget, preserving close-before-interrupt when it completes
	// normally while reserving the remainder for app-server cleanup.
	brokerDeadline := started.Add(c.cfg.ControlTimeout / 2)
	brokerCtx, cancelBrokerWait := context.WithDeadline(ctx, brokerDeadline)
	if err := c.waitBrokerClose(brokerCtx); err != nil {
		c.logger.Warn("close broker during shutdown", "err", err)
	}
	cancelBrokerWait()

	if turnTerminalSeen || phase == phaseAwaitingStartResponse {
		if startResult != nil {
			// The terminal notification was already reconciled. Only the outstanding
			// start response remains to be drained.
			c.drainShutdownTurn(ctx, threadID, turnID, startResult, startAmbiguous, true)
		}
	} else if phase == phaseStarting || phase == phaseActive || hasDelivery || owner != turnOwnerNone || turnID != "" || startAmbiguous {
		if err := c.app.TurnInterrupt(ctx, appserver.TurnInterruptParams{ThreadID: threadID, TurnID: turnID}); err != nil {
			c.logger.Warn("interrupt Codex turn during shutdown", "thread", threadID, "turn", turnID, "err", err)
		}
		if turnID != "" || startResult != nil || startAmbiguous {
			c.drainShutdownTurn(ctx, threadID, turnID, startResult, startAmbiguous, false)
		}
	}
	if err := c.app.WaitHandlers(ctx); err != nil {
		c.logger.Warn("drain app-server reverse requests during shutdown", "err", err)
	}
}

func (c *controller) drainShutdownTurn(ctx context.Context, threadID, turnID string, startResult <-chan turnStartResult, ambiguous, terminal bool) {
	for !terminal || startResult != nil {
		select {
		case started := <-startResult:
			startResult = nil
			if started.err != nil {
				ambiguous = turnStartMayHaveSucceeded(started.err)
				if turnID == "" && !ambiguous {
					return
				}
				c.logger.Warn("drain turn/start response during shutdown", "err", started.err)
				continue
			}
			if started.response.Turn.ID == "" {
				c.logger.Warn("drain turn/start response with empty turn id")
				continue
			}
			if turnID != "" && turnID != started.response.Turn.ID {
				c.logger.Warn("drain mismatched turn/start response", "expected", turnID, "actual", started.response.Turn.ID)
				return
			}
			turnID = started.response.Turn.ID
			ambiguous = false
		case queued := <-c.notifications:
			notification := queued.notification
			switch notification.Method {
			case appserver.NotificationTurnStarted:
				var params appserver.TurnStartedNotification
				if err := notification.DecodeParams(&params); err != nil || params.ThreadID != threadID || params.Turn.ID == "" {
					continue
				}
				if turnID == "" {
					turnID = params.Turn.ID
				}
			case appserver.NotificationTurnCompleted:
				var params appserver.TurnCompletedNotification
				if err := notification.DecodeParams(&params); err != nil || params.ThreadID != threadID || params.Turn.ID == "" {
					continue
				}
				switch params.Turn.Status {
				case appserver.TurnStatusCompleted, appserver.TurnStatusFailed, appserver.TurnStatusInterrupted:
				default:
					continue
				}
				if turnID == "" {
					turnID = params.Turn.ID
				}
				if params.Turn.ID == turnID {
					terminal = true
				}
			}
		case <-c.app.Done():
			return
		case <-ctx.Done():
			c.logger.Warn("timed out draining Codex turn during shutdown", "thread", threadID, "turn", turnID)
			return
		}
	}
}

// turnStartMayHaveSucceeded distinguishes a definitive JSON-RPC rejection from
// transport, protocol, decode, and deadline failures whose mutation outcome is
// ambiguous. Ambiguous failures must still be interrupted and terminally
// drained before the app-server connection is closed.
func turnStartMayHaveSucceeded(err error) bool {
	var rpcErr *appserver.RPCError
	return !errors.As(err, &rpcErr)
}

// startBrokerClose starts the non-contextual broker close exactly once. A
// buffered completion path is unnecessary because closing brokerCloseDone
// publishes brokerCloseErr to all waiters.
func (c *controller) startBrokerClose() <-chan struct{} {
	c.brokerCloseOnce.Do(func() {
		c.brokerCloseDone = make(chan struct{})
		go func() {
			c.brokerCloseErr = c.broker.Close()
			close(c.brokerCloseDone)
		}()
	})
	return c.brokerCloseDone
}

func (c *controller) waitBrokerClose(ctx context.Context) error {
	done := c.startBrokerClose()
	select {
	case <-done:
		return c.brokerCloseErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
