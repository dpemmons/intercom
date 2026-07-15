package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/codex"
	"github.com/dpemmons/intercom/internal/codexinstance"
	"github.com/dpemmons/intercom/internal/codexsession"
	"github.com/dpemmons/intercom/internal/paths"
	"github.com/dpemmons/intercom/internal/peername"
)

type codexRunner func(context.Context, codex.Config) error
type processExec func(string, []string, []string) error
type codexSessionDialer func(context.Context, string, appserver.Options) (codexSessionClient, error)

type codexSessionClient interface {
	codexsession.Client
	Initialize(context.Context, appserver.InitializeParams) (appserver.InitializeResponse, error)
	Initialized(context.Context) error
	Close() error
}

const codexBinEnv = "CODEX_BIN"

func newCodexCmd() *cobra.Command {
	return newCodexCmdWithDependencies(codex.Run, syscall.Exec)
}

func newCodexCmdWithRunner(run codexRunner) *cobra.Command {
	return newCodexCmdWithDependencies(run, syscall.Exec)
}

func newCodexCmdWithDependencies(run codexRunner, replaceProcess processExec) *cobra.Command {
	var (
		appServer      string
		clientEndpoint string
		mcpBridge      string
		name           string
		cwd            string
		fresh          bool
		adoptSession   string
		forkSession    string
		replaceBinding bool
		yolo           bool
	)

	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Runs a managed Codex peer",
		Long:  "intercom codex connects one externally supervised Codex app-server thread to the Intercom broker. The adapter runs in the foreground until interrupted.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint, err := normalizeAppServerEndpoint(appServer)
			if err != nil {
				return err
			}
			client, err := normalizeOptionalClientEndpoint(clientEndpoint)
			if err != nil {
				return err
			}
			if client != "" && client == endpoint {
				return errors.New("codex: --client-endpoint must differ from --app-server")
			}
			selectedCWD, err := absoluteCWD(cwd)
			if err != nil {
				return err
			}
			peer, err := peername.Resolve(name, selectedCWD)
			if err != nil {
				return err
			}
			brokerSocket, err := paths.Socket()
			if err != nil {
				return err
			}
			brokerBin, err := resolveBrokerBinary()
			if err != nil {
				return fmt.Errorf("codex: %w", err)
			}
			intercomBin, err := os.Executable()
			if err != nil {
				return fmt.Errorf("codex: locate intercom executable: %w", err)
			}
			intercomBin, err = filepath.Abs(intercomBin)
			if err != nil {
				return fmt.Errorf("codex: resolve intercom executable: %w", err)
			}
			executionPolicy := codex.ExecutionWorkspaceWrite
			if yolo {
				executionPolicy = codex.ExecutionDangerFullAccess
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			defer stop()

			cfg := codex.Config{
				Name:              peer,
				Version:           version,
				CWD:               selectedCWD,
				AppServerEndpoint: endpoint,
				ClientEndpoint:    client,
				MCPBridgeSocket:   mcpBridge,
				IntercomBin:       intercomBin,
				BrokerSocket:      brokerSocket,
				BrokerBin:         brokerBin,
				New:               fresh,
				AdoptThreadID:     adoptSession,
				ForkThreadID:      forkSession,
				ReplaceBinding:    replaceBinding,
				ExecutionPolicy:   executionPolicy,
				Logger:            slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			}

			var publication *codexPublication
			if client != "" {
				publication, err = newCodexPublication(peer, selectedCWD, brokerSocket, client, executionPolicy, cmd.OutOrStdout())
				if err != nil {
					return err
				}
				cfg.OnReady = publication.publish
				cfg.OnStopping = publication.remove
			}

			err = run(ctx, cfg)
			if errors.Is(err, context.Canceled) {
				err = nil
			}
			if publication != nil {
				err = errors.Join(err, publication.remove())
			}
			return err
		},
	}
	cmd.Flags().StringVar(&appServer, "app-server", "", "absolute app-server Unix socket endpoint or path")
	cmd.Flags().StringVar(&clientEndpoint, "client-endpoint", "", "absolute Unix socket endpoint or path exposed to Codex clients")
	cmd.Flags().StringVar(&mcpBridge, "mcp-bridge", "", "absolute private Unix socket path for managed-session MCP tools")
	cmd.Flags().StringVar(&name, "name", "", "peer name (default: INTERCOM_NAME, then cwd basename)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "managed project directory (default: current directory)")
	cmd.Flags().BoolVar(&fresh, "new", false, "start a new managed thread and replace the saved binding")
	cmd.Flags().StringVar(&adoptSession, "adopt-session", "", "adopt an existing interactive Codex session by thread id")
	cmd.Flags().StringVar(&forkSession, "fork-session", "", "fork an existing interactive Codex session by thread id")
	cmd.Flags().BoolVar(&replaceBinding, "replace-binding", false, "replace an existing peer binding during adoption or fork")
	cmd.Flags().BoolVar(&yolo, "yolo", false, "run all managed turns without approvals or a Codex sandbox")
	cmd.Flags().BoolVar(&yolo, "dangerously-bypass-approvals-and-sandbox", false, "run all managed turns without approvals or a Codex sandbox")
	if err := cmd.MarkFlagRequired("app-server"); err != nil {
		panic(err)
	}
	cmd.AddCommand(newCodexAttachCmd(replaceProcess), newCodexSessionsCmd())
	return cmd
}

type codexPublication struct {
	registry        *codexinstance.Registry
	peer            string
	cwd             string
	intercomDir     string
	intercomBin     string
	brokerSocket    string
	clientEndpoint  string
	codexBin        string
	codexHome       string
	executionPolicy codex.ExecutionPolicy
	nonce           string
	output          io.Writer
	published       bool
}

func newCodexPublication(peer, cwd, brokerSocket, clientEndpoint string, executionPolicy codex.ExecutionPolicy, output io.Writer) (*codexPublication, error) {
	intercomDir, err := paths.Dir()
	if err != nil {
		return nil, fmt.Errorf("codex: resolve runtime directory: %w", err)
	}
	intercomDir, err = canonicalExistingDirectory(intercomDir)
	if err != nil {
		return nil, fmt.Errorf("codex: canonicalize runtime directory: %w", err)
	}
	codexDir, err := paths.CodexDir()
	if err != nil {
		return nil, fmt.Errorf("codex: resolve live instance directory: %w", err)
	}
	registry, err := codexinstance.New(filepath.Join(codexDir, "live"))
	if err != nil {
		return nil, fmt.Errorf("codex: open live instance registry: %w", err)
	}
	cwd, err = canonicalExistingDirectory(cwd)
	if err != nil {
		return nil, fmt.Errorf("codex: canonicalize live instance cwd: %w", err)
	}
	brokerSocket, err = codexinstance.CanonicalBrokerSocket(brokerSocket)
	if err != nil {
		return nil, fmt.Errorf("codex: canonicalize broker socket: %w", err)
	}
	clientEndpoint, err = codexinstance.CanonicalUnixEndpoint(clientEndpoint)
	if err != nil {
		return nil, fmt.Errorf("codex: canonicalize client endpoint: %w", err)
	}
	intercomBin, err := portableExecutableValue(os.Getenv("INTERCOM_BIN"), "intercom")
	if err != nil {
		return nil, fmt.Errorf("codex: resolve INTERCOM_BIN: %w", err)
	}
	codexBin, err := portableExecutableValue(os.Getenv(codexBinEnv), "codex")
	if err != nil {
		return nil, fmt.Errorf("codex: resolve %s: %w", codexBinEnv, err)
	}
	codexHome, err := portableOptionalPath(os.Getenv("CODEX_HOME"))
	if err != nil {
		return nil, fmt.Errorf("codex: resolve CODEX_HOME: %w", err)
	}
	nonce, err := codexinstance.NewNonce()
	if err != nil {
		return nil, err
	}
	return &codexPublication{
		registry:        registry,
		peer:            peer,
		cwd:             cwd,
		intercomDir:     intercomDir,
		intercomBin:     intercomBin,
		brokerSocket:    brokerSocket,
		clientEndpoint:  clientEndpoint,
		codexBin:        codexBin,
		codexHome:       codexHome,
		executionPolicy: executionPolicy,
		nonce:           nonce,
		output:          output,
	}, nil
}

func portableExecutableValue(value, fallback string) (string, error) {
	if value == "" {
		value = fallback
	}
	if !strings.ContainsRune(value, filepath.Separator) || filepath.IsAbs(value) {
		return value, nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func portableOptionalPath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func (p *codexPublication) publish(info codex.ReadyInfo) error {
	if info.Name != p.peer {
		return fmt.Errorf("ready peer %q does not match configured peer %q", info.Name, p.peer)
	}
	readyCWD, err := canonicalExistingDirectory(info.CWD)
	if err != nil {
		return fmt.Errorf("canonicalize ready cwd: %w", err)
	}
	if readyCWD != p.cwd {
		return fmt.Errorf("ready cwd %q does not match configured cwd %q", readyCWD, p.cwd)
	}
	readyEndpoint, err := codexinstance.CanonicalUnixEndpoint(info.ClientEndpoint)
	if err != nil {
		return fmt.Errorf("canonicalize ready client endpoint: %w", err)
	}
	if readyEndpoint != p.clientEndpoint {
		return fmt.Errorf("ready client endpoint %q does not match configured endpoint %q", readyEndpoint, p.clientEndpoint)
	}
	if info.ExecutionPolicy != p.executionPolicy {
		return fmt.Errorf("ready execution policy %q does not match configured policy %q", info.ExecutionPolicy, p.executionPolicy)
	}

	descriptor := codexinstance.Descriptor{
		SchemaVersion:          codexinstance.SchemaVersion,
		Peer:                   p.peer,
		CWD:                    p.cwd,
		BrokerSocketIdentity:   p.brokerSocket,
		DownstreamUnixEndpoint: p.clientEndpoint,
		ThreadID:               info.ThreadID,
		PID:                    os.Getpid(),
		InstanceNonce:          p.nonce,
		CodexVersion:           info.CodexVersion,
		ExecutionPolicy:        codexinstance.ExecutionPolicy(info.ExecutionPolicy),
	}
	if _, err := p.registry.Publish(descriptor); err != nil {
		return fmt.Errorf("publish live instance: %w", err)
	}
	p.published = true

	if _, err := fmt.Fprintf(p.output, readinessText,
		p.peer,
		p.executionPolicy,
		p.attachCommand(),
		p.directCommand(info.ThreadID),
	); err != nil {
		removeErr := p.remove()
		return errors.Join(fmt.Errorf("write readiness instructions: %w", err), removeErr)
	}
	return nil
}

func (p *codexPublication) attachCommand() string {
	parts := []string{
		"INTERCOM_DIR=" + shellQuote(p.intercomDir),
		"INTERCOM_SOCKET=" + shellQuote(p.brokerSocket),
		codexBinEnv + "=" + shellQuote(p.codexBin),
	}
	if p.codexHome != "" {
		parts = append(parts, "CODEX_HOME="+shellQuote(p.codexHome))
	}
	parts = append(parts, shellCommandWord(p.intercomBin), "codex", "attach", "--name", shellQuote(p.peer))
	return strings.Join(parts, " ")
}

func (p *codexPublication) directCommand(threadID string) string {
	parts := make([]string, 0, 7)
	if p.codexHome != "" {
		parts = append(parts, "CODEX_HOME="+shellQuote(p.codexHome))
	}
	parts = append(parts,
		shellCommandWord(p.codexBin), "resume",
	)
	if p.executionPolicy.IsYolo() {
		parts = append(parts, "--dangerously-bypass-approvals-and-sandbox")
	}
	parts = append(parts, "--remote", shellQuote(p.clientEndpoint), shellQuote(threadID))
	return strings.Join(parts, " ")
}

func canonicalExistingDirectory(value string) (string, error) {
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat path: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", resolved)
	}
	return filepath.Clean(resolved), nil
}

func (p *codexPublication) remove() error {
	if p == nil || !p.published {
		return nil
	}
	removed, err := p.registry.Remove(p.brokerSocket, p.peer, p.nonce)
	if err != nil {
		return fmt.Errorf("codex: remove live instance descriptor: %w", err)
	}
	if removed {
		p.published = false
	}
	return nil
}

const readinessText = `Intercom Codex peer %s is ready.
Execution policy: %s

Attach from another terminal:
  %s

Direct Codex command:
  %s
`

func newCodexSessionsCmd() *cobra.Command {
	return newCodexSessionsCmdWithDependencies(
		func(ctx context.Context, endpoint string, opts appserver.Options) (codexSessionClient, error) {
			return appserver.DialUnix(ctx, endpoint, opts)
		},
		isTerminalInput,
	)
}

func newCodexSessionsCmdWithDependencies(dial codexSessionDialer, terminalInput func(io.Reader) bool) *cobra.Command {
	var (
		appServer string
		cwd       string
		all       bool
		listOnly  bool
	)
	cmd := &cobra.Command{
		Use:   "sessions",
		Short: "Lists or selects resumable interactive Codex sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint, err := normalizeAppServerEndpoint(appServer)
			if err != nil {
				return err
			}
			selectedCWD, err := absoluteCWD(cwd)
			if err != nil {
				return err
			}
			selectedCWD, err = canonicalExistingDirectory(selectedCWD)
			if err != nil {
				return fmt.Errorf("codex sessions: canonicalize project directory: %w", err)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			client, err := dial(ctx, endpoint, appserver.Options{})
			if err != nil {
				return fmt.Errorf("codex sessions: connect app-server: %w", err)
			}
			defer client.Close()
			if _, err := client.Initialize(ctx, appserver.InitializeParams{
				ClientInfo:   appserver.ClientInfo{Name: "intercom_session_picker", Version: version},
				Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
			}); err != nil {
				return fmt.Errorf("codex sessions: initialize app-server: %w", err)
			}
			if err := client.Initialized(ctx); err != nil {
				return fmt.Errorf("codex sessions: complete app-server initialization: %w", err)
			}
			candidates, err := codexsession.List(ctx, client, codexsession.Options{CWD: selectedCWD, AllCWDs: all})
			if err != nil {
				return err
			}
			if listOnly {
				for _, candidate := range candidates {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n",
						candidate.Thread.ID,
						candidate.Recency().UTC().Format(time.RFC3339),
						codexsession.SanitizeDisplay(candidate.Thread.CWD, 0),
						codexsession.SanitizeDisplay(candidate.Title(), 100),
					); err != nil {
						return fmt.Errorf("codex sessions: write list: %w", err)
					}
				}
				return nil
			}
			if !terminalInput(cmd.InOrStdin()) {
				return errors.New("codex sessions: interactive selection requires a terminal; supply an explicit session id")
			}
			candidate, err := codexsession.Pick(cmd.InOrStdin(), cmd.ErrOrStderr(), candidates)
			if err != nil {
				return err
			}
			if candidate.Thread.CWD != selectedCWD {
				return fmt.Errorf("codex sessions: selected session cwd is %q; rerun with --cwd %q", candidate.Thread.CWD, candidate.Thread.CWD)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), candidate.Thread.ID)
			return err
		},
	}
	cmd.Flags().StringVar(&appServer, "app-server", "", "absolute app-server Unix socket endpoint or path")
	cmd.Flags().StringVar(&cwd, "cwd", "", "project directory used to filter sessions")
	cmd.Flags().BoolVar(&all, "all", false, "include interactive sessions from every working directory")
	cmd.Flags().BoolVar(&listOnly, "list", false, "write all matching sessions without prompting")
	if err := cmd.MarkFlagRequired("app-server"); err != nil {
		panic(err)
	}
	return cmd
}

func isTerminalInput(input io.Reader) bool {
	file, ok := input.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func newCodexAttachCmd(replaceProcess processExec) *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Attaches the Codex TUI to a managed peer",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(name) == "" {
				return errors.New("codex attach: --name must not be empty")
			}
			peer, err := peername.Resolve(name, "")
			if err != nil {
				return err
			}
			brokerSocket, err := paths.Socket()
			if err != nil {
				return fmt.Errorf("codex attach: resolve broker socket: %w", err)
			}
			codexDir, err := paths.CodexDir()
			if err != nil {
				return fmt.Errorf("codex attach: resolve live instance directory: %w", err)
			}
			registry, err := codexinstance.New(filepath.Join(codexDir, "live"))
			if err != nil {
				return fmt.Errorf("codex attach: open live instance registry: %w", err)
			}
			descriptor, err := registry.Load(brokerSocket, peer)
			if err != nil {
				return fmt.Errorf("codex attach: load live instance: %w", err)
			}
			if descriptor == nil {
				return fmt.Errorf("codex attach: no live Codex instance named %q is registered for broker %q", peer, brokerSocket)
			}

			codexBin := os.Getenv(codexBinEnv)
			if codexBin == "" {
				codexBin = "codex"
			}
			executable, err := exec.LookPath(codexBin)
			if err != nil {
				return fmt.Errorf("codex attach: locate %q: %w", codexBin, err)
			}
			locatedExecutable := executable
			executable, err = filepath.Abs(locatedExecutable)
			if err != nil {
				return fmt.Errorf("codex attach: resolve executable %q: %w", locatedExecutable, err)
			}
			argv := []string{
				codexBin,
				"resume",
			}
			if descriptor.ExecutionPolicy == codexinstance.ExecutionDangerFullAccess {
				argv = append(argv, "--dangerously-bypass-approvals-and-sandbox")
			}
			argv = append(argv, "--remote", descriptor.DownstreamUnixEndpoint, descriptor.ThreadID)
			return replaceProcessInDirectory(descriptor.CWD, executable, argv, os.Environ(), replaceProcess)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "managed peer name")
	if err := cmd.MarkFlagRequired("name"); err != nil {
		panic(err)
	}
	return cmd
}

func replaceProcessInDirectory(cwd, executable string, argv, env []string, replaceProcess processExec) error {
	previousCWD, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("codex attach: get working directory: %w", err)
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("codex attach: change directory to %q: %w", cwd, err)
	}
	if err := replaceProcess(executable, argv, env); err != nil {
		restoreErr := os.Chdir(previousCWD)
		return errors.Join(
			fmt.Errorf("codex attach: execute %q: %w", executable, err),
			wrapRestoreCWDError(previousCWD, restoreErr),
		)
	}
	if err := os.Chdir(previousCWD); err != nil {
		return fmt.Errorf("codex attach: restore working directory to %q after process replacement returned: %w", previousCWD, err)
	}
	return nil
}

func wrapRestoreCWDError(cwd string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("codex attach: restore working directory to %q: %w", cwd, err)
}

func shellQuote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			strings.ContainsRune("_@%+:,./-", r))
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func shellCommandWord(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func normalizeAppServerEndpoint(value string) (string, error) {
	return normalizeUnixEndpoint("--app-server", value)
}

func normalizeOptionalClientEndpoint(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return normalizeUnixEndpoint("--client-endpoint", value)
}

func normalizeUnixEndpoint(flag, value string) (string, error) {
	candidate := value
	if filepath.IsAbs(value) {
		candidate = (&url.URL{Scheme: "unix", Path: filepath.Clean(value)}).String()
	}
	path, err := appserver.ParseUnixEndpoint(candidate)
	if err != nil {
		return "", fmt.Errorf("codex: invalid %s %q; want unix:///absolute/path.sock or an absolute socket path: %w", flag, value, err)
	}
	return (&url.URL{Scheme: "unix", Path: path}).String(), nil
}

func absoluteCWD(value string) (string, error) {
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
	return filepath.Clean(abs), nil
}
