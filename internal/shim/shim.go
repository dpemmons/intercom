package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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

	srv := mcp.NewServer(
		mcp.Implementation{Name: "intercom", Version: cfg.Version},
		mcp.Options{
			Instructions: instructions(cfg.Name),
			Experimental: map[string]any{"claude/channel": map[string]any{}},
		},
	)

	// The client owns the broker connection. Its OnDeliver callback turns
	// inbound deliver frames into notifications/claude/channel events.
	client := brokerclient.NewClient(brokerclient.ClientOptions{
		Name:       cfg.Name,
		Version:    cfg.Version,
		SocketPath: cfg.SocketPath,
		BrokerBin:  cfg.BrokerBin,
		Logger:     cfg.Logger,
		OnDeliver: func(d wire.Deliver) {
			err := srv.Notify("notifications/claude/channel", channelParams{
				Content: d.Message,
				Meta: channelMeta{
					From:      d.From,
					Timestamp: d.Timestamp,
				},
			})
			if err != nil {
				cfg.Logger.Warn("notify channel", "err", err)
			}
		},
		OnGoodbye: func(reason string) {
			cfg.Logger.Info("broker goodbye", "reason", reason)
		},
	})
	defer client.Close()

	srv.RegisterTool(mcp.Tool{
		Name:        intercomtools.SendMessageName,
		Description: intercomtools.SendMessageDescription,
		InputSchema: intercomtools.SendMessageInputSchema,
		Handler: func(callCtx context.Context, args json.RawMessage) (mcp.ToolResult, error) {
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

	// Eagerly connect to the broker after MCP handshake completes, so this
	// peer is discoverable to others without needing to make a tool call
	// first. Failure is non-fatal: any subsequent tool call will retry the
	// connect itself.
	go func() {
		select {
		case <-srv.Initialized():
		case <-ctx.Done():
			return
		}
		if err := client.Connect(ctx); err != nil {
			cfg.Logger.Warn("eager connect failed", "err", err)
		}
	}()

	return srv.Run(ctx, cfg.Stdin, cfg.Stdout)
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
// peer name interpolated.
func instructions(name string) string {
	return fmt.Sprintf(`You are connected to other local Claude Code sessions through the intercom channel. Your peer name is %q.

Inbound messages from other sessions arrive as:
  <channel source="intercom" from="<peer>" timestamp="<rfc3339>">message body</channel>

The "from" attribute tells you who sent it. To reply, call:
  send_message(to="<peer>", message="...")

To discover who else is online, call:
  list_peers()

Priority: the human you're working with takes priority. If a message arrives mid-task, finish what the human asked first, then reply.

When to reply: reply if the message asks a question or requests something. If it's purely informational ("FYI...", "thanks", a status update), do not call send_message — there is no need to acknowledge.

Keep replies focused — include code, file paths, or commands when useful.`, name)
}
