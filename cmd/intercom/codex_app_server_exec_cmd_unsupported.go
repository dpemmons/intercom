//go:build !unix

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

func newCodexAppServerExecCmd() *cobra.Command {
	var readyFile string
	cmd := &cobra.Command{
		Use:    "codex-app-server-exec --ready-file FILE -- CODEX [ARG...]",
		Short:  "Executes a Codex app-server in a dedicated process session",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, _ []string) error {
			return errors.New("app-server process sessions are unavailable on this platform")
		},
	}
	cmd.Flags().StringVar(&readyFile, "ready-file", "", "absolute private session-readiness file")
	if err := cmd.MarkFlagRequired("ready-file"); err != nil {
		panic(err)
	}
	return cmd
}
