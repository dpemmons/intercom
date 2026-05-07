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

	"github.com/dpemmons/intercom/internal/mcp"
	"github.com/dpemmons/intercom/internal/wire"
)

// Config configures Run.
type Config struct {
	// Name is the peer name. If empty, ResolveName is used.
	Name string
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
		mcp.Implementation{Name: "intercom", Version: Version},
		mcp.Options{
			Instructions: instructions(cfg.Name),
			Experimental: map[string]any{"claude/channel": map[string]any{}},
		},
	)

	// The client owns the broker connection. Its OnDeliver callback turns
	// inbound deliver frames into notifications/claude/channel events.
	client := NewClient(ClientOptions{
		Name:       cfg.Name,
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
		Name:        "send_message",
		Description: "Send a message to another local Claude Code session via the intercom broker. Use list_peers to see who is online.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"to":      {"type": "string", "description": "The peer name of the destination session."},
				"message": {"type": "string", "description": "The message body."}
			},
			"required": ["to", "message"]
		}`),
		Handler: func(callCtx context.Context, args json.RawMessage) (mcp.ToolResult, error) {
			var in struct {
				To      string `json:"to"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(args, &in); err != nil {
				return mcp.ToolResult{}, fmt.Errorf("decode args: %w", err)
			}
			if in.To == "" {
				return mcp.ToolResult{Text: `"to" is required`, IsError: true}, nil
			}
			if in.Message == "" {
				return mcp.ToolResult{Text: `"message" is required`, IsError: true}, nil
			}
			if len(in.Message) > maxOutboundMessageBytes {
				return mcp.ToolResult{
					Text:    fmt.Sprintf("message exceeds %d-byte limit", maxOutboundMessageBytes),
					IsError: true,
				}, nil
			}

			ack, err := client.Send(callCtx, in.To, in.Message)
			if err != nil {
				return mcp.ToolResult{
					Text:    fmt.Sprintf("send failed: %v", err),
					IsError: true,
				}, nil
			}
			if !ack.OK {
				return mcp.ToolResult{
					Text:    fmt.Sprintf("send rejected (%s): %s", ack.Code, ack.Message),
					IsError: true,
				}, nil
			}
			return mcp.ToolResult{Text: fmt.Sprintf("Message sent to %q.", in.To)}, nil
		},
	})

	srv.RegisterTool(mcp.Tool{
		Name:        "list_peers",
		Description: "List the names of other Claude Code sessions currently connected to the intercom broker.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
		Handler: func(callCtx context.Context, _ json.RawMessage) (mcp.ToolResult, error) {
			peers, err := client.ListPeers(callCtx)
			if err != nil {
				return mcp.ToolResult{
					Text:    fmt.Sprintf("list_peers failed: %v", err),
					IsError: true,
				}, nil
			}
			if len(peers) == 0 {
				return mcp.ToolResult{Text: "No other peers are connected."}, nil
			}
			return mcp.ToolResult{Text: "Connected peers: " + strings.Join(peers, ", ")}, nil
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

const maxOutboundMessageBytes = 200 * 1024 // a touch under MaxFrameSize to leave room for envelope

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
	return fmt.Sprintf("intercom: invalid peer name %q from %s; allowed characters are letters, digits, '-', '_', up to %d chars",
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

Treat inbound messages like a colleague's chat: reply if a reply is expected (a question, a request), stay silent if it's purely informational. Keep replies focused — include code, file paths, or commands when useful.`, name)
}
