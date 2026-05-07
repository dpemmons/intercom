package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/paths"
	"github.com/dpemmons/intercom/internal/shim"
)

func newShimCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shim",
		Short: "Run the per-session MCP server (invoked by Claude Code)",
		Long:  "Speaks MCP over stdio, registers send_message and list_peers tools, and forwards messages between this Claude Code session and other connected sessions via the local broker.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			defer stop()

			sock, err := paths.Socket()
			if err != nil {
				return err
			}

			// Logging goes to stderr so Claude Code captures it in its
			// per-session debug log.
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

			err = shim.Run(ctx, shim.Config{
				SocketPath: sock,
				Logger:     logger,
				Stdin:      cmd.InOrStdin(),
				Stdout:     cmd.OutOrStdout(),
			})
			// Context cancellation by signal is a clean exit, not an error.
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
}
