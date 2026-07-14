package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/codex"
	"github.com/dpemmons/intercom/internal/paths"
	"github.com/dpemmons/intercom/internal/peername"
)

type codexRunner func(context.Context, codex.Config) error

func newCodexCmd() *cobra.Command {
	return newCodexCmdWithRunner(codex.Run)
}

func newCodexCmdWithRunner(run codexRunner) *cobra.Command {
	var (
		appServer string
		name      string
		cwd       string
		fresh     bool
	)

	cmd := &cobra.Command{
		Use:   "codex",
		Short: "Runs a managed Codex peer",
		Long:  "intercom codex connects one externally supervised Codex app-server thread to the Intercom broker. The adapter runs in the foreground until interrupted.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			endpoint, err := normalizeAppServerEndpoint(appServer)
			if err != nil {
				return err
			}
			selectedCWD, err := absoluteCWD(cwd)
			if err != nil {
				return err
			}
			peer, err := peername.Resolve(name, selectedCWD)
			if err != nil {
				return err
			}
			brokerSocket, err := paths.Socket()
			if err != nil {
				return err
			}
			brokerBin, err := resolveBrokerBinary()
			if err != nil {
				return fmt.Errorf("codex: %w", err)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
			defer stop()

			err = run(ctx, codex.Config{
				Name:              peer,
				Version:           version,
				CWD:               selectedCWD,
				AppServerEndpoint: endpoint,
				BrokerSocket:      brokerSocket,
				BrokerBin:         brokerBin,
				New:               fresh,
				Logger:            slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
			})
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		},
	}
	cmd.Flags().StringVar(&appServer, "app-server", "", "absolute app-server Unix socket endpoint or path")
	cmd.Flags().StringVar(&name, "name", "", "peer name (default: INTERCOM_NAME, then cwd basename)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "managed project directory (default: current directory)")
	cmd.Flags().BoolVar(&fresh, "new", false, "start a new managed thread and replace the saved binding")
	if err := cmd.MarkFlagRequired("app-server"); err != nil {
		panic(err)
	}
	return cmd
}

func normalizeAppServerEndpoint(value string) (string, error) {
	candidate := value
	if filepath.IsAbs(value) {
		candidate = (&url.URL{Scheme: "unix", Path: filepath.Clean(value)}).String()
	}
	path, err := appserver.ParseUnixEndpoint(candidate)
	if err != nil {
		return "", fmt.Errorf("codex: invalid --app-server %q; want unix:///absolute/path.sock or an absolute socket path: %w", value, err)
	}
	return (&url.URL{Scheme: "unix", Path: path}).String(), nil
}

func absoluteCWD(value string) (string, error) {
	if value == "" {
		var err error
		value, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("codex: get working directory: %w", err)
		}
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("codex: resolve cwd: %w", err)
	}
	return filepath.Clean(abs), nil
}
