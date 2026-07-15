package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/codexbridge"
)

const codexBridgeTokenEnv = "INTERCOM_CODEX_BRIDGE_TOKEN"

func newCodexMCPBridgeCmd() *cobra.Command {
	var socket string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:    "codex-mcp-bridge",
		Short:  "Serves the private MCP transport for a managed Codex thread",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return errors.New("codex MCP bridge timeout must be positive")
			}
			token := os.Getenv(codexBridgeTokenEnv)
			if token == "" {
				return errors.New("codex MCP bridge token is not set")
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
			defer stop()
			err := codexbridge.RunHelper(ctx, codexbridge.HelperOptions{
				SocketPath: socket,
				Token:      token,
				Version:    version,
				Timeout:    timeout,
				Stdin:      cmd.InOrStdin(),
				Stdout:     cmd.OutOrStdout(),
			})
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&socket, "socket", "", "absolute private controller Unix socket path")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Second, "private controller request timeout")
	if err := cmd.MarkFlagRequired("socket"); err != nil {
		panic(err)
	}
	return cmd
}
