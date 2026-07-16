//go:build !linux && !darwin

package main

import (
	"errors"
	"time"

	"github.com/spf13/cobra"
)

func newCodexProcessSessionCleanupCmd() *cobra.Command {
	var sid int
	var leader int
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:    "codex-process-session-cleanup --sid PID --leader PID --timeout DURATION",
		Short:  "Stops a dedicated Codex app-server process session",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("app-server process-session cleanup is unavailable on this platform")
		},
	}
	cmd.Flags().IntVar(&sid, "sid", 0, "Process-session ID")
	cmd.Flags().IntVar(&leader, "leader", 0, "Process ID of the app-server session leader")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "TERM and KILL phase timeout")
	return cmd
}
