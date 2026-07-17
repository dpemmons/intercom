package shim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dpemmons/intercom/internal/brokerclient"
	"github.com/dpemmons/intercom/internal/intercomtools"
	"github.com/dpemmons/intercom/internal/mcp"
	"github.com/dpemmons/intercom/internal/wire"
)

// Config configures Run.
type Config struct {
	// Name is the peer name. If empty, ResolveName is used.
	Name string
	// Version is reported to both the MCP client and broker. Required.
	Version string
	// SocketPath is the broker's Unix socket. Required.
	SocketPath string
	// BrokerBin is the path to the binary used to auto-spawn the broker.
	// If empty, os.Executable() is used.
	BrokerBin string
	// Stdin/Stdout: where MCP frames flow. Defaults to os.Stdin/os.Stdout.
	Stdin  io.Reader
	Stdout io.Writer
	// Logger sinks structured logs. Defaults to slog on stderr.
	Logger *slog.Logger
}

// Run is the shim's main loop. It returns when stdin EOFs (Claude Code
// detaches), ctx cancels, or a fatal error occurs.
//
// Returns nil on clean shutdown, non-nil on configuration errors or fatal
// I/O failures.
func Run(parentCtx context.Context, cfg Config) error {
	// Derive a child context that is cancelled when Run returns, so any
	// background goroutines we start (in particular the eager-connect
	// goroutine below) are guaranteed to wind down even if the parent
	// context outlives this call.
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()
	if cfg.Stdin == nil {
		cfg.Stdin = os.Stdin
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if cfg.Version == "" {
		return fmt.Errorf("shim: version is required")
	}
	// A session participates only when it opts in. Registering unconditionally
	// is what produced "dark receivers": a session that claims a peer name but
	// silently drops every inbound message because its Claude Code was launched
	// without the channel flag. The shim cannot observe channel status over
	// MCP, so it requires an explicit opt-in the launcher sets alongside
	// --dangerously-load-development-channels. An explicitly-supplied name (via
	// Config or $INTERCOM_NAME) is itself such a signal.
	explicitName := strings.TrimSpace(cfg.Name) != ""
	enabled := explicitName ||
		os.Getenv("INTERCOM_ENABLE") == "1" ||
		strings.TrimSpace(os.Getenv("INTERCOM_NAME")) != ""

	if enabled {
		// An enabled session must resolve to a valid peer name to register.
		if cfg.Name == "" {
			n, err := ResolveName()
			if err != nil {
				return err
			}
			cfg.Name = n
		}
		if !wire.ValidName(cfg.Name) {
			return &InvalidNameError{Name: cfg.Name}
		}
	} else {
		// A disabled session never registers, so its name is unused. Resolve it
		// best-effort for diagnostics, but an invalid cwd basename must not fail
		// startup — the shim still has to serve the tools that explain how to
		// enable intercom.
		if n, err := ResolveName(); err == nil {
			cfg.Name = n
		}
	}

	srv := mcp.NewServer(
		mcp.Implementation{Name: "intercom", Version: cfg.Version},
		mcp.Options{
			Instructions: instructions(cfg.Name, enabled),
			Experimental: map[string]any{"claude/channel": map[string]any{}},
		},
	)

	// Inbound deliver frames are handed to a buffered channel and surfaced on a
	// dedicated goroutine (deliverLoop), so the broker read loop that invokes
	// OnDeliver is never blocked on a slow stdout write to Claude Code.
	deliveries := make(chan wire.Deliver, deliverBufferSize)

	// The client owns the broker connection. NameAttempts opts the shim in to
	// auto-suffixing on a name collision (the Codex adapter does not).
	client := brokerclient.NewClient(brokerclient.ClientOptions{
		Name:         cfg.Name,
		Version:      cfg.Version,
		SocketPath:   cfg.SocketPath,
		BrokerBin:    cfg.BrokerBin,
		NameAttempts: maxNameAttempts,
		Logger:       cfg.Logger,
		OnDeliver: func(d wire.Deliver) {
			select {
			case deliveries <- d:
			case <-ctx.Done():
			}
		},
		OnGoodbye: func(reason string) {
			cfg.Logger.Info("broker goodbye", "reason", reason)
		},
	})
	// Cancel before Close so the supervisor observes ctx cancellation (a clean
	// shutdown) instead of a bare connection drop it would log and reconnect
	// through. Runs before the top-level defer cancel() via LIFO; that cancel()
	// is then a no-op.
	defer func() {
		cancel()
		_ = client.Close()
	}()

	// notEnabled is returned by broker-backed tools when this session did not
	// opt in. It never touches the broker, so a disabled session never
	// registers a name.
	notEnabled := func(tool string) mcp.ToolResult {
		return mcp.ToolResult{
			Text: fmt.Sprintf("intercom is not enabled for this session, so %s is inert. "+
				"Set INTERCOM_ENABLE=1 (or INTERCOM_NAME) and start Claude Code with "+
				"--dangerously-load-development-channels server:intercom, then call channel_status.", tool),
			IsError: true,
		}
	}

	srv.RegisterTool(mcp.Tool{
		Name:        intercomtools.SendMessageName,
		Description: intercomtools.SendMessageDescription,
		InputSchema: intercomtools.SendMessageInputSchema,
		Handler: func(callCtx context.Context, args json.RawMessage) (mcp.ToolResult, error) {
			if !enabled {
				return notEnabled(intercomtools.SendMessageName), nil
			}
			in, err := intercomtools.DecodeSendMessage(args)
			if err != nil {
				return mcp.ToolResult{Text: err.Error(), IsError: true}, nil
			}

			ack, err := client.Send(callCtx, in.To, in.Message)
			if err != nil {
				return mcp.ToolResult{
					Text:    intercomtools.SendFailed(err),
					IsError: true,
				}, nil
			}
			if !ack.OK {
				return mcp.ToolResult{
					Text:    intercomtools.SendRejected(ack.Code, ack.Message),
					IsError: true,
				}, nil
			}
			return mcp.ToolResult{Text: intercomtools.SendAccepted(in.To)}, nil
		},
	})

	srv.RegisterTool(mcp.Tool{
		Name:        intercomtools.ListPeersName,
		Description: intercomtools.ListPeersDescription,
		InputSchema: intercomtools.ListPeersInputSchema,
		Handler: func(callCtx context.Context, args json.RawMessage) (mcp.ToolResult, error) {
			if !enabled {
				return notEnabled(intercomtools.ListPeersName), nil
			}
			if err := intercomtools.DecodeListPeers(args); err != nil {
				return mcp.ToolResult{Text: err.Error(), IsError: true}, nil
			}
			peers, err := client.ListPeers(callCtx)
			if err != nil {
				return mcp.ToolResult{
					Text:    intercomtools.ListPeersFailed(err),
					IsError: true,
				}, nil
			}
			return mcp.ToolResult{Text: intercomtools.FormatPeers(peers)}, nil
		},
	})

	// channel_status is registered even when disabled: it is the one tool a
	// confused session can call to learn whether it is participating, what name
	// it registered under (after any collision suffix), and the caveat that
	// intercom cannot itself confirm the channel is loaded.
	srv.RegisterTool(mcp.Tool{
		Name:        "channel_status",
		Description: "Report this session's intercom status: whether it is enabled, its effective peer name, broker connectivity, and connected peers. Use it to diagnose why messages are or are not getting through.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Handler: func(callCtx context.Context, _ json.RawMessage) (mcp.ToolResult, error) {
			return mcp.ToolResult{Text: channelStatus(callCtx, enabled, cfg.Name, client)}, nil
		},
	})

	if enabled {
		// Surface inbound deliveries, and keep a broker connection alive for the
		// life of the session (reconnecting after any broker restart) so a
		// receive-only session does not silently go dark.
		go deliverLoop(ctx, srv, deliveries, cfg.Logger)
		go superviseConnection(ctx, srv.Initialized(), client, cfg.Logger)
	} else {
		cfg.Logger.Info("intercom disabled for this session; not registering with the broker",
			"hint", "set INTERCOM_ENABLE=1 or INTERCOM_NAME to participate")
	}

	return srv.Run(ctx, cfg.Stdin, cfg.Stdout)
}

// deliverBufferSize bounds how many inbound messages may queue between the
// broker read loop and the stdout-writing deliverLoop before backpressure
// reaches the read loop. maxNameAttempts caps the shim's auto-suffix search.
const (
	deliverBufferSize = 256
	maxNameAttempts   = 20
)

// reconnectBackoff is the delay ladder the supervisor waits between failed
// broker connect attempts; it saturates at the final value.
var reconnectBackoff = []time.Duration{
	500 * time.Millisecond,
	time.Second,
	2 * time.Second,
	5 * time.Second,
}

// deliverLoop drains inbound deliver frames and emits them as
// notifications/claude/channel events. Running on its own goroutine keeps a
// slow or stalled Claude Code stdout from blocking the broker read loop.
func deliverLoop(ctx context.Context, srv *mcp.Server, deliveries <-chan wire.Deliver, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-deliveries:
			if err := srv.Notify("notifications/claude/channel", channelParams{
				Content: d.Message,
				Meta:    channelMeta{From: d.From, Timestamp: d.Timestamp},
			}); err != nil {
				logger.Warn("notify channel", "err", err)
			}
		}
	}
}

// superviseConnection keeps a live broker connection for the session's
// lifetime. It connects once the MCP handshake completes (so the peer is
// discoverable without a tool call) and redials with backoff whenever the
// connection drops — the receive-side reconnect that a receive-only session
// needs to survive a broker restart or idle-exit.
func superviseConnection(ctx context.Context, initialized <-chan struct{}, client *brokerclient.Client, logger *slog.Logger) {
	select {
	case <-initialized:
	case <-ctx.Done():
		return
	}

	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		err := client.Connect(ctx)
		// A shutdown can race an in-progress handshake: cancel() runs before
		// client.Close(), so if the context is now cancelled treat whatever
		// Connect returned as a clean exit rather than a broker failure.
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			attempt = 0
			if waitForDrop(ctx, client, logger) {
				return
			}
			continue
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		// A non-name_taken hello rejection (e.g. an invalid name) will not fix
		// itself; stop rather than spin.
		var he *brokerclient.HelloError
		if errors.As(err, &he) && he.Code != wire.CodeNameTaken {
			logger.Error("broker rejected registration; giving up", "err", err)
			return
		}
		delay := reconnectBackoff[min(attempt, len(reconnectBackoff)-1)]
		attempt++
		logger.Warn("broker connect failed; retrying", "err", err, "retry_in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// waitForDrop blocks until the current connection drops, ctx is cancelled, or
// the client is closed. It returns true when the supervisor should exit and
// false when it should reconnect. Connection events are latest-state snapshots,
// so it dispatches on ev.State rather than counting events; a Connected event
// (e.g. from a tool-call-driven reconnect) just means keep waiting.
func waitForDrop(ctx context.Context, client *brokerclient.Client, logger *slog.Logger) (exit bool) {
	for {
		select {
		case <-ctx.Done():
			return true
		case ev := <-client.ConnectionEvents():
			switch ev.State {
			case brokerclient.ConnectionStateClosed:
				return true
			case brokerclient.ConnectionStateDisconnected:
				if ctx.Err() != nil {
					return true
				}
				logger.Info("broker connection dropped; will reconnect", "cause", string(ev.Cause))
				return false
			}
		}
	}
}

// channelStatus renders the channel_status tool output.
func channelStatus(ctx context.Context, enabled bool, requestedName string, client *brokerclient.Client) string {
	if !enabled {
		return "intercom status: DISABLED for this session.\n\n" +
			"send_message and list_peers are inert and this session holds no peer name. " +
			"To participate, set INTERCOM_ENABLE=1 (or INTERCOM_NAME) and start Claude Code with " +
			"--dangerously-load-development-channels server:intercom."
	}

	var b strings.Builder
	name := client.Name()
	b.WriteString("intercom status:\n")
	b.WriteString("  enabled: yes\n")
	fmt.Fprintf(&b, "  peer name: %s\n", name)
	if name != requestedName {
		fmt.Fprintf(&b, "    (requested %q, but another session already holds it, so this session was suffixed;\n"+
			"     other peers must use %q to reach it)\n", requestedName, name)
	}
	if client.Connected() {
		b.WriteString("  broker: connected\n")
		peers, err := client.ListPeers(ctx)
		switch {
		case err != nil:
			fmt.Fprintf(&b, "  other peers: (query failed: %v)\n", err)
		case len(peers) == 0:
			b.WriteString("  other peers: none\n")
		default:
			fmt.Fprintf(&b, "  other peers: %s\n", strings.Join(peers, ", "))
		}
	} else {
		b.WriteString("  broker: not connected (reconnecting in the background)\n")
	}
	b.WriteString("\nNote: intercom confirms broker registration, not that this session loaded the " +
		"channel. To confirm this session can receive, have a peer send a test message, or check that " +
		"startup showed \"Channels (experimental) messages from server:intercom\".")
	return b.String()
}

// channelParams is the params object for notifications/claude/channel.
type channelParams struct {
	Content string      `json:"content"`
	Meta    channelMeta `json:"meta"`
}

// channelMeta carries per-message routing context. Keys must satisfy
// [A-Za-z0-9_]+ per the Channels API spec; both keys here do.
type channelMeta struct {
	From      string `json:"from"`
	Timestamp string `json:"timestamp"`
}

// ResolveName picks the peer name in this order:
//  1. $INTERCOM_NAME if non-empty.
//  2. The basename of the current working directory.
//
// Returns an error if neither yields a valid peer name.
func ResolveName() (string, error) {
	if n := strings.TrimSpace(os.Getenv("INTERCOM_NAME")); n != "" {
		if !wire.ValidName(n) {
			return "", &InvalidNameError{Name: n, Source: "INTERCOM_NAME"}
		}
		return n, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("shim: getwd: %w", err)
	}
	base := filepath.Base(cwd)
	if !wire.ValidName(base) {
		return "", &InvalidNameError{Name: base, Source: "cwd basename"}
	}
	return base, nil
}

// InvalidNameError describes a peer name that didn't pass validation.
type InvalidNameError struct {
	Name   string
	Source string // human-readable origin: "INTERCOM_NAME", "cwd basename", ""
}

func (e *InvalidNameError) Error() string {
	src := e.Source
	if src == "" {
		src = "name"
	}
	return fmt.Sprintf("invalid peer name %q from %s; allowed characters are ASCII letters, digits, '-', '_', up to %d bytes",
		e.Name, src, wire.MaxNameLen)
}

// instructions returns the MCP `instructions` string the shim sets, with the
// peer name interpolated. The disabled variant explains how to turn intercom on
// rather than implying it is live.
func instructions(name string, enabled bool) string {
	if !enabled {
		return `The intercom channel is installed but NOT enabled for this session, so it cannot send or receive messages with other Claude Code sessions right now. send_message and list_peers return an error. To enable it, the human should set INTERCOM_ENABLE=1 (or INTERCOM_NAME) and start Claude Code with --dangerously-load-development-channels server:intercom. Call channel_status to confirm the state.`
	}
	return fmt.Sprintf(`You are connected to other local Claude Code sessions through the intercom channel. Your peer name is %q. (If another session already registered that name, this one was given a numbered suffix — call channel_status to see the name others must use to reach it.)

Inbound messages from other sessions arrive as:
  <channel source="intercom" from="<peer>" timestamp="<rfc3339>">message body</channel>

The "from" attribute tells you who sent it. To reply, call:
  send_message(to="<peer>", message="...")

To discover who else is online, call:
  list_peers()

To check your own status (peer name, broker connectivity, whether you can actually receive), call:
  channel_status()

Priority: the human you're working with takes priority. If a message arrives mid-task, finish what the human asked first, then reply.

When to reply: reply if the message asks a question or requests something. If it's purely informational ("FYI...", "thanks", a status update), do not call send_message — there is no need to acknowledge.

Keep replies focused — include code, file paths, or commands when useful.`, name)
}
