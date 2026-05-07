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
		Short: "Run the local message router",
		Long: `Run the broker: a single in-memory router process that all shims connect to.
Normally auto-spawned by the first shim; you can run it manually for debugging or to set a custom idle timeout.

Lock semantics: only one broker runs at a time. If another broker already holds the lock, this command exits 0 (so auto-spawn is idempotent).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				IdleAfter:  idleAfter,
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
