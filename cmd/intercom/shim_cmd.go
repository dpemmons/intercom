package main

import (
	"context"
	"errors"
	"fmt"
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
		Short: "Runs the Claude Code MCP and Channels adapter",
		Long:  "intercom shim serves MCP over standard input and output, registers the send_message and list_peers tools, and forwards broker deliveries as Claude Code channel notifications.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			defer stop()

			sock, err := paths.Socket()
			if err != nil {
				return err
			}
			brokerBin, err := resolveBrokerBinary()
			if err != nil {
				return fmt.Errorf("shim: %w", err)
			}

			// Logging goes to stderr so Claude Code captures it in its
			// per-session debug log.
			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

			err = shim.Run(ctx, shim.Config{
				Version:    version,
				SocketPath: sock,
				BrokerBin:  brokerBin,
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
