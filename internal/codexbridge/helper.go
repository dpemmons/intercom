package codexbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/dpemmons/intercom/internal/intercomtools"
	"github.com/dpemmons/intercom/internal/mcp"
)

// HelperOptions configures the stdio MCP process injected into an adopted
// Codex session.
type HelperOptions struct {
	SocketPath string
	Token      string
	Version    string
	Timeout    time.Duration
	Stdin      io.Reader
	Stdout     io.Writer
}

// RunHelper verifies the controller with an authenticated startup ping, then
// serves send_message and list_peers over MCP stdio until EOF or cancellation.
// It never opens an Intercom broker connection.
func RunHelper(ctx context.Context, opts HelperOptions) error {
	if ctx == nil {
		return errors.New("codex bridge helper: context is nil")
	}
	if opts.Stdin == nil {
		return errors.New("codex bridge helper: stdin is required")
	}
	if opts.Stdout == nil {
		return errors.New("codex bridge helper: stdout is required")
	}
	client, err := NewClient(ClientOptions{SocketPath: opts.SocketPath, Token: opts.Token, Timeout: opts.Timeout})
	if err != nil {
		return err
	}
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("codex bridge helper: startup ping: %w", err)
	}

	version := opts.Version
	if version == "" {
		version = "dev"
	}
	server := mcp.NewServer(mcp.Implementation{Name: "intercom-codex", Version: version}, mcp.Options{})
	server.RegisterTool(mcp.Tool{
		Name:        intercomtools.SendMessageName,
		Description: intercomtools.SendMessageDescription,
		InputSchema: intercomtools.SendMessageInputSchema,
		HandlerWithMeta: func(callCtx context.Context, args, metadata json.RawMessage) (mcp.ToolResult, error) {
			in, err := intercomtools.DecodeSendMessage(args)
			if err != nil {
				return mcp.ToolResult{Text: err.Error(), IsError: true}, nil
			}
			ack, err := client.SendMessage(callCtx, metadata, in.To, in.Message)
			if err != nil {
				return mcp.ToolResult{Text: intercomtools.SendFailed(err), IsError: true}, nil
			}
			if !ack.OK {
				return mcp.ToolResult{Text: intercomtools.SendRejected(ack.Code, ack.Message), IsError: true}, nil
			}
			return mcp.ToolResult{Text: intercomtools.SendAccepted(in.To)}, nil
		},
	})
	server.RegisterTool(mcp.Tool{
		Name:        intercomtools.ListPeersName,
		Description: intercomtools.ListPeersDescription,
		InputSchema: intercomtools.ListPeersInputSchema,
		HandlerWithMeta: func(callCtx context.Context, args, metadata json.RawMessage) (mcp.ToolResult, error) {
			if err := intercomtools.DecodeListPeers(args); err != nil {
				return mcp.ToolResult{Text: err.Error(), IsError: true}, nil
			}
			peers, err := client.ListPeers(callCtx, metadata)
			if err != nil {
				return mcp.ToolResult{Text: intercomtools.ListPeersFailed(err), IsError: true}, nil
			}
			return mcp.ToolResult{Text: intercomtools.FormatPeers(peers)}, nil
		},
	})
	return server.Run(ctx, opts.Stdin, opts.Stdout)
}
