package codex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/paths"
	"github.com/dpemmons/intercom/internal/wire"
)

const (
	defaultQueueSize       = 64
	defaultControlTimeout  = 30 * time.Second
	defaultReverseTimeout  = 10 * time.Second
	defaultActivityTimeout = 15 * time.Minute
)

type Config struct {
	Name              string
	Version           string
	CWD               string
	AppServerEndpoint string
	BrokerSocket      string
	BrokerBin         string
	New               bool
	Logger            *slog.Logger

	QueueSize       int
	StartupTimeout  time.Duration
	ControlTimeout  time.Duration
	ReverseTimeout  time.Duration
	ActivityTimeout time.Duration
	StatePath       string
	LockPath        string

	dialAppServer func(context.Context, string, appserver.Options) (appServerClient, error)
	newBroker     func(brokerclient.ClientOptions) brokerConnection
	onTurnActive  func()
}

type appServerClient interface {
	Initialize(context.Context, appserver.InitializeParams) (appserver.InitializeResponse, error)
	Initialized(context.Context) error
	ThreadStart(context.Context, appserver.ThreadStartParams) (appserver.ThreadStartResponse, error)
	ThreadResume(context.Context, appserver.ThreadResumeParams) (appserver.ThreadResumeResponse, error)
	ThreadRead(context.Context, appserver.ThreadReadParams) (appserver.ThreadReadResponse, error)
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

type turnStartResult struct {
	response appserver.TurnStartResponse
	err      error
}

const (
	phaseBooting controllerPhase = iota
	phaseIdle
	phaseStarting
	phaseActive
	phaseFailed
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
	case phaseFailed:
		return "failed"
	default:
		return "unknown"
	}
}

type controller struct {
	cfg    Config
	logger *slog.Logger
	store  *StateStore
	state  *ManagedState
	app    appServerClient
	broker brokerConnection

	deliveries    chan wire.Deliver
	notifications chan appserver.Notification
	fatal         chan error
	activity      chan struct{}
	reconnectDone chan error

	brokerCloseOnce sync.Once
	brokerCloseDone chan struct{}
	brokerCloseErr  error

	mu               sync.Mutex
	phase            controllerPhase
	ready            bool
	threadID         string
	turnID           string
	current          wire.Deliver
	outboundCount    int
	reconnecting     bool
	reconnectPending bool
	startupViolation bool
	sandbox          appserver.SandboxPolicy
	startResult      <-chan turnStartResult
	startAmbiguous   bool

	reverse reverseHandler
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
		logger:        cfg.Logger,
		phase:         phaseBooting,
		deliveries:    make(chan wire.Deliver, cfg.QueueSize),
		notifications: make(chan appserver.Notification, 256),
		fatal:         make(chan error, 1),
		activity:      make(chan struct{}, 1),
		reconnectDone: make(chan error, 1),
	}

	store, err := AcquireStateStore(cfg.StatePath, cfg.LockPath)
	if err != nil {
		return err
	}
	c.store = store
	defer store.Close()
	if !cfg.New {
		c.state, err = store.Load()
		if err != nil {
			return err
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
		OnReverseRequest:         c.reverse.Handle,
	}
	c.app, err = c.dialAppServer(ctx, appOptions)
	if err != nil {
		return err
	}
	defer c.app.Close()

	if err := c.startup(ctx); err != nil {
		return err
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
	if cfg.BrokerSocket == "" {
		return Config{}, errors.New("codex: broker socket is required")
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
	control, cancel = context.WithTimeout(ctx, c.cfg.ControlTimeout)
	err = c.app.Initialized(control)
	cancel()
	if err != nil {
		return fmt.Errorf("codex: send initialized: %w", err)
	}

	if c.state != nil {
		if err := c.validateStoredIdentity(init, version); err != nil {
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

var semanticVersion = regexp.MustCompile(`(?:^|[^0-9])(\d+\.\d+\.\d+)(?:[^0-9]|$)`)

func validateServerVersion(userAgent string) (string, error) {
	match := semanticVersion.FindStringSubmatch(userAgent)
	if len(match) != 2 {
		return "", fmt.Errorf("codex: cannot determine app-server version from user agent %q", userAgent)
	}
	if match[1] != appserver.ProtocolVersion {
		return "", fmt.Errorf("codex: unsupported app-server version %s (requires %s)", match[1], appserver.ProtocolVersion)
	}
	return match[1], nil
}

func (c *controller) validateStoredIdentity(init appserver.InitializeResponse, version string) error {
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
	if state.ServerUserAgent != init.UserAgent || state.CodexVersion != version {
		return fmt.Errorf("codex: app-server identity changed from %q to %q; use --new to replace the binding", state.ServerUserAgent, init.UserAgent)
	}
	return nil
}

func (c *controller) startNewThread(ctx context.Context, init appserver.InitializeResponse, version string) error {
	cwd := c.cfg.CWD
	developer := developerInstructions(c.cfg.Name)
	ephemeral := false
	sandbox := appserver.SandboxWorkspaceWrite
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	response, err := c.app.ThreadStart(control, appserver.ThreadStartParams{
		CWD:                   &cwd,
		ApprovalPolicy:        string(appserver.ApprovalNever),
		Sandbox:               &sandbox,
		DeveloperInstructions: &developer,
		Ephemeral:             &ephemeral,
		DynamicTools:          dynamicToolSpecs(),
	})
	cancel()
	if err != nil {
		return fmt.Errorf("codex: start thread: %w", err)
	}
	if err := c.acceptThread(response.Thread, response.CWD, response.ApprovalPolicy, response.Sandbox); err != nil {
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
		Materialized:        false,
	}
	if err := c.store.Save(*c.state); err != nil {
		return fmt.Errorf("codex: persist new thread binding: %w", err)
	}
	return nil
}

func (c *controller) resumeOrReplace(ctx context.Context, init appserver.InitializeResponse, version string) error {
	cwd := c.cfg.CWD
	developer := developerInstructions(c.cfg.Name)
	sandbox := appserver.SandboxWorkspaceWrite
	control, cancel := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	response, err := c.app.ThreadResume(control, appserver.ThreadResumeParams{
		ThreadID:              c.state.ThreadID,
		CWD:                   &cwd,
		ApprovalPolicy:        string(appserver.ApprovalNever),
		Sandbox:               &sandbox,
		DeveloperInstructions: &developer,
		ExcludeTurns:          true,
	})
	cancel()
	if err != nil {
		if !c.state.Materialized && isMissingRollout(err, c.state.ThreadID) {
			c.logger.Info("replace unmaterialized Codex thread", "old_thread", c.state.ThreadID)
			c.state = nil
			return c.startNewThread(ctx, init, version)
		}
		return fmt.Errorf("codex: resume thread %s: %w", c.state.ThreadID, err)
	}
	if err := c.acceptThread(response.Thread, response.CWD, response.ApprovalPolicy, response.Sandbox); err != nil {
		return err
	}
	if !c.state.Materialized {
		if _, err := c.confirmMaterialized(ctx, false); err != nil {
			return fmt.Errorf("codex: verify pending thread materialization: %w", err)
		}
	}
	return nil
}

func isMissingRollout(err error, threadID string) bool {
	var rpcErr *appserver.RPCError
	return errors.As(err, &rpcErr) && rpcErr.Code == appserver.ErrorCodeInvalidRequest &&
		rpcErr.Message == "no rollout found for thread id "+threadID
}

func (c *controller) acceptThread(thread appserver.Thread, cwd string, approval any, sandbox appserver.SandboxPolicy) error {
	if thread.ID == "" || thread.ID != c.stateThreadIDOr(thread.ID) {
		return fmt.Errorf("codex: app-server returned unexpected thread id %q", thread.ID)
	}
	if filepath.Clean(cwd) != c.cfg.CWD || filepath.Clean(thread.CWD) != c.cfg.CWD {
		return fmt.Errorf("codex: app-server returned cwd %q/%q, want %q", cwd, thread.CWD, c.cfg.CWD)
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
	if sandbox.Type != "workspaceWrite" {
		return fmt.Errorf("codex: app-server sandbox is %q, want workspaceWrite", sandbox.Type)
	}
	if len(sandbox.WritableRoots) != 0 {
		return fmt.Errorf("codex: app-server sandbox grants additional writable roots: %v", sandbox.WritableRoots)
	}
	if _, ok := sandbox.NetworkAccess.(bool); !ok {
		return fmt.Errorf("codex: app-server workspace-write networkAccess has type %T, want bool", sandbox.NetworkAccess)
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
	for {
		phase := c.currentPhase()
		var delivery <-chan wire.Deliver
		if phase == phaseIdle {
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

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.app.Done():
			err := c.app.Wait()
			if err == nil {
				err = appserver.ErrClosed
			}
			return fmt.Errorf("codex: app-server disconnected: %w", err)
		case err := <-c.fatal:
			c.setFailed()
			return err
		case d := <-delivery:
			if err := c.startDelivery(ctx, d); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				c.setFailed()
				return err
			}
		case notification := <-c.notifications:
			if err := c.handleNotification(ctx, notification); err != nil {
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
	c.phase = phaseStarting
	c.turnID = ""
	c.current = delivery
	c.outboundCount = 0
	c.startAmbiguous = false
	sandbox := c.sandbox
	threadID := c.threadID
	c.mu.Unlock()

	clientID := delivery.ID
	cwd := c.cfg.CWD
	writeCtx, cancelWrite := context.WithTimeout(ctx, c.cfg.ControlTimeout)
	await, err := c.app.StartTurn(writeCtx, appserver.TurnStartParams{
		ThreadID:            threadID,
		ClientUserMessageID: &clientID,
		Input:               []appserver.UserInput{appserver.TextInput(inboundEnvelope(delivery.From, delivery.ID, delivery.Message, sent))},
		CWD:                 &cwd,
		ApprovalPolicy:      string(appserver.ApprovalNever),
		SandboxPolicy:       &sandbox,
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
	c.phase = phaseActive
	c.mu.Unlock()
	if c.cfg.onTurnActive != nil {
		c.cfg.onTurnActive()
	}
	c.logger.Info("codex turn started", "delivery", delivery.ID, "from", delivery.From, "thread", threadID, "turn", response.Turn.ID)
	return nil
}

func (c *controller) handleNotification(ctx context.Context, notification appserver.Notification) error {
	switch notification.Method {
	case appserver.NotificationThreadStarted:
		var params appserver.ThreadStartedNotification
		if err := notification.DecodeParams(&params); err != nil {
			return err
		}
		if c.threadID != "" && params.Thread.ID != c.threadID {
			return fmt.Errorf("codex: notification for unexpected thread %s", params.Thread.ID)
		}
	case appserver.NotificationTurnStarted:
		var params appserver.TurnStartedNotification
		if err := notification.DecodeParams(&params); err != nil {
			return err
		}
		if err := c.reconcileTurn(params.ThreadID, params.Turn.ID, params.Turn.Status); err != nil {
			return err
		}
	case appserver.NotificationTurnCompleted:
		var params appserver.TurnCompletedNotification
		if err := notification.DecodeParams(&params); err != nil {
			return err
		}
		if err := c.completeTurn(params); err != nil {
			return err
		}
		if c.state != nil && !c.state.Materialized {
			if _, err := c.confirmMaterialized(ctx, true); err != nil {
				return fmt.Errorf("codex: confirm thread materialization: %w", err)
			}
		}
	case appserver.NotificationError:
		var params appserver.ErrorNotification
		if err := notification.DecodeParams(&params); err != nil {
			return err
		}
		c.logger.Warn("app-server turn error", "thread", params.ThreadID, "turn", params.TurnID, "retry", params.WillRetry, "err", params.Error.Message)
	}
	return nil
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
	c.phase = phaseIdle
	c.turnID = ""
	c.current = wire.Deliver{}
	c.outboundCount = 0
	c.startAmbiguous = false
	c.mu.Unlock()
	var durationMS any
	if params.Turn.DurationMS != nil {
		durationMS = *params.Turn.DurationMS
	}
	c.logger.Info("codex turn completed", "delivery", delivery.ID, "from", delivery.From, "turn", params.Turn.ID, "status", params.Turn.Status, "duration_ms", durationMS, "outbound_sends", outbound, "replied", outbound > 0)
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
	switch notification.Method {
	case appserver.NotificationThreadStarted,
		appserver.NotificationTurnStarted,
		appserver.NotificationTurnCompleted,
		appserver.NotificationError:
	default:
		return
	}
	select {
	case c.notifications <- notification:
	default:
		c.signalFatal(errors.New("codex: app-server notification queue is full"))
	}
}

func (c *controller) authorizeReverse(threadID, turnID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ready {
		c.startupViolation = true
		return errors.New("adapter ownership is not established")
	}
	if threadID != c.threadID {
		return fmt.Errorf("tool call thread %s does not match %s", threadID, c.threadID)
	}
	if c.phase != phaseStarting && c.phase != phaseActive {
		return fmt.Errorf("tool call arrived while controller is %s", c.phase)
	}
	if c.turnID != "" && c.turnID != turnID {
		return fmt.Errorf("tool call turn %s does not match %s", turnID, c.turnID)
	}
	c.turnID = turnID
	return nil
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
	c.mu.Unlock()
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

	if phase == phaseStarting || phase == phaseActive || hasDelivery {
		if err := c.app.TurnInterrupt(ctx, appserver.TurnInterruptParams{ThreadID: threadID, TurnID: turnID}); err != nil {
			c.logger.Warn("interrupt Codex turn during shutdown", "thread", threadID, "turn", turnID, "err", err)
		}
		if turnID != "" || startResult != nil || startAmbiguous {
			c.drainShutdownTurn(ctx, threadID, turnID, startResult, startAmbiguous)
		}
	}
	if err := c.app.WaitHandlers(ctx); err != nil {
		c.logger.Warn("drain app-server reverse requests during shutdown", "err", err)
	}
}

func (c *controller) drainShutdownTurn(ctx context.Context, threadID, turnID string, startResult <-chan turnStartResult, ambiguous bool) {
	terminal := false
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
		case notification := <-c.notifications:
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
