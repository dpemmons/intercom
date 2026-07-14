package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/broker"
	"github.com/dpemmons/intercom/internal/paths"
)

func newBrokerCmd() *cobra.Command {
	var (
		idleAfter  time.Duration
		foreground bool
	)
	cmd := &cobra.Command{
		Use:   "broker",
		Short: "Runs the local message router",
		Long: `intercom broker runs the single in-memory router used by all Intercom adapters.
The first adapter normally starts the broker. Direct invocation supports diagnostics and custom idle timeouts.

Only one broker holds a socket lock. A second broker invocation exits successfully when that lock is held.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolvedIdleAfter, err := resolveBrokerIdleAfter(cmd, idleAfter)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			defer stop()

			sock, err := paths.Socket()
			if err != nil {
				return err
			}
			lock, err := paths.Lock()
			if err != nil {
				return err
			}
			logPath, err := paths.Log()
			if err != nil {
				return err
			}

			logger, closeLog, err := openLogger(logPath, foreground)
			if err != nil {
				return err
			}
			defer closeLog()

			err = broker.Run(ctx, broker.Options{
				SocketPath: sock,
				LockPath:   lock,
				IdleAfter:  resolvedIdleAfter,
				Logger:     logger,
			})
			if errors.Is(err, broker.ErrLockHeld) {
				// Another broker is already running. From the shim's perspective this
				// is success (auto-spawn is idempotent); exit 0.
				return nil
			}
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&idleAfter, "idle-after", broker.DefaultIdleAfter, "exit after this long with zero connected peers (0 disables)")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "log to stderr instead of the broker log file")
	return cmd
}

func resolveBrokerIdleAfter(cmd *cobra.Command, flagValue time.Duration) (time.Duration, error) {
	value := flagValue
	if !cmd.Flags().Changed("idle-after") {
		if raw := os.Getenv("INTERCOM_IDLE_EXIT"); raw != "" {
			parsed, err := time.ParseDuration(raw)
			if err != nil {
				return 0, fmt.Errorf("broker: invalid INTERCOM_IDLE_EXIT %q: %w", raw, err)
			}
			value = parsed
		}
	}
	if value < 0 {
		return 0, fmt.Errorf("broker: idle exit must be non-negative, got %s", value)
	}
	return brokerIdleAfter(value), nil
}

func brokerIdleAfter(value time.Duration) time.Duration {
	if value == 0 {
		return broker.IdleExitDisabled
	}
	return value
}

// openLogger returns a slog.Logger configured for the broker. In the default
// detached mode, output goes to the broker log file (append-only). With
// --foreground, output goes to stderr (handy when running the broker in a
// terminal for debugging).
func openLogger(path string, foreground bool) (*slog.Logger, func(), error) {
	if foreground {
		return slog.New(slog.NewTextHandler(os.Stderr, nil)), func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open broker log %s: %w", path, err)
	}
	return slog.New(slog.NewTextHandler(f, nil)), func() { _ = f.Close() }, nil
}
