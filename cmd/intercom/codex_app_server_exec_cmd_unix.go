//go:build unix

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
)

type codexAppServerExecOps struct {
	lookPath func(string) (string, error)
	setsid   func() (int, error)
	writePID func(string, int) error
	exec     func(string, []string, []string) error
}

func newCodexAppServerExecCmd() *cobra.Command {
	return newCodexAppServerExecCmdWithOps(codexAppServerExecOps{
		lookPath: exec.LookPath,
		setsid:   syscall.Setsid,
		writePID: writeSessionPID,
		exec:     syscall.Exec,
	})
}

func newCodexAppServerExecCmdWithOps(ops codexAppServerExecOps) *cobra.Command {
	var readyFile string
	cmd := &cobra.Command{
		Use:    "codex-app-server-exec --ready-file FILE -- CODEX [ARG...]",
		Short:  "Executes a Codex app-server in a dedicated process session",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if !filepath.IsAbs(readyFile) {
				return fmt.Errorf("app-server session ready file must be absolute: %q", readyFile)
			}
			path, err := ops.lookPath(args[0])
			if err != nil {
				return fmt.Errorf("resolve Codex executable %q: %w", args[0], err)
			}
			if _, err := ops.setsid(); err != nil {
				return fmt.Errorf("create app-server process session: %w", err)
			}
			if err := ops.writePID(readyFile, os.Getpid()); err != nil {
				return fmt.Errorf("publish app-server process session: %w", err)
			}
			if err := ops.exec(path, args, os.Environ()); err != nil {
				return fmt.Errorf("exec Codex app-server: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&readyFile, "ready-file", "", "absolute private session-readiness file")
	if err := cmd.MarkFlagRequired("ready-file"); err != nil {
		panic(err)
	}
	return cmd
}

func writeSessionPID(path string, pid int) error {
	temporary := fmt.Sprintf("%s.%d.tmp", path, pid)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	_, writeErr := fmt.Fprintf(file, "%s\n", strconv.Itoa(pid))
	chmodErr := file.Chmod(0o600)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if chmodErr != nil {
		return chmodErr
	}
	if closeErr != nil {
		return closeErr
	}
	// A hard link publishes the already-closed file atomically without replacing
	// an existing marker. Both paths are in the same private runtime directory.
	if err := os.Link(temporary, path); err != nil {
		return err
	}
	if err := os.Remove(temporary); err != nil {
		return err
	}
	removeTemporary = false
	return nil
}
