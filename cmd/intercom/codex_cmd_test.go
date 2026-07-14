package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/codex"
)

func TestRootRegistersCodexWithBackendNeutralDescription(t *testing.T) {
	root := newRootCmd()
	command, _, err := root.Find([]string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Name() != "codex" {
		t.Fatalf("Find(codex) = %q", command.Name())
	}
	description := strings.ToLower(root.Short + " " + root.Long)
	if strings.Contains(description, "claude") || strings.Contains(description, "codex") {
		t.Fatalf("root description is backend-specific: %q", description)
	}
}

func TestCodexCmdWiresFlagsAndRuntimePaths(t *testing.T) {
	project := t.TempDir()
	appSocket := filepath.Join(t.TempDir(), "codex socket.sock")
	brokerSocket := filepath.Join(t.TempDir(), "broker.sock")
	t.Setenv("INTERCOM_NAME", "from-env")
	t.Setenv("INTERCOM_SOCKET", brokerSocket)

	var got codex.Config
	err := executeCodexCommand(t, func(_ context.Context, cfg codex.Config) error {
		got = cfg
		return nil
	}, "--app-server", appSocket, "--name", "from-flag", "--cwd", project, "--new")
	if err != nil {
		t.Fatal(err)
	}

	if got.Name != "from-flag" || got.Version != version || got.CWD != project || !got.New {
		t.Fatalf("identity config = %+v", got)
	}
	parsedSocket, err := appserver.ParseUnixEndpoint(got.AppServerEndpoint)
	if err != nil {
		t.Fatalf("AppServerEndpoint = %q: %v", got.AppServerEndpoint, err)
	}
	if parsedSocket != appSocket {
		t.Fatalf("app-server socket = %q, want %q", parsedSocket, appSocket)
	}
	if got.BrokerSocket != brokerSocket {
		t.Fatalf("BrokerSocket = %q, want %q", got.BrokerSocket, brokerSocket)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got.BrokerBin != executable {
		t.Fatalf("BrokerBin = %q, want %q", got.BrokerBin, executable)
	}
	if got.Logger == nil {
		t.Fatal("Logger is nil")
	}
}

func TestCodexCmdUsesBrokerBinaryEnvironmentOverride(t *testing.T) {
	project := t.TempDir()
	brokerBin := filepath.Join(t.TempDir(), "pinned-intercom")
	t.Setenv(brokerBinEnv, brokerBin)
	t.Setenv("INTERCOM_NAME", "test-peer")
	t.Setenv("INTERCOM_SOCKET", filepath.Join(t.TempDir(), "broker.sock"))

	var got codex.Config
	err := executeCodexCommand(t, func(_ context.Context, cfg codex.Config) error {
		got = cfg
		return nil
	}, "--app-server", "unix:///tmp/intercom-codex-test.sock", "--cwd", project)
	if err != nil {
		t.Fatal(err)
	}
	if got.BrokerBin != brokerBin {
		t.Fatalf("BrokerBin = %q, want environment override %q", got.BrokerBin, brokerBin)
	}
}

func TestCodexCmdNamePrecedence(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "cwd-peer")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	appServer := "unix:///tmp/intercom-codex-test.sock"
	t.Setenv("INTERCOM_SOCKET", filepath.Join(root, "broker.sock"))

	tests := []struct {
		name     string
		env      string
		explicit string
		want     string
	}{
		{name: "flag", env: "env-peer", explicit: "flag-peer", want: "flag-peer"},
		{name: "environment", env: "env-peer", want: "env-peer"},
		{name: "cwd", want: "cwd-peer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("INTERCOM_NAME", tt.env)
			var got codex.Config
			args := []string{"--app-server", appServer, "--cwd", project}
			if tt.explicit != "" {
				args = append(args, "--name", tt.explicit)
			}
			if err := executeCodexCommand(t, func(_ context.Context, cfg codex.Config) error {
				got = cfg
				return nil
			}, args...); err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.want {
				t.Fatalf("Name = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestCodexCmdDefaultsCWDToCurrentDirectory(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	t.Setenv("INTERCOM_NAME", "")
	t.Setenv("INTERCOM_SOCKET", filepath.Join(t.TempDir(), "broker.sock"))

	var got codex.Config
	err := executeCodexCommand(t, func(_ context.Context, cfg codex.Config) error {
		got = cfg
		return nil
	}, "--app-server", "unix:///tmp/intercom-codex-test.sock")
	if err != nil {
		t.Fatal(err)
	}
	if got.CWD != project {
		t.Fatalf("CWD = %q, want %q", got.CWD, project)
	}
	if got.Name != filepath.Base(project) {
		t.Fatalf("Name = %q, want cwd basename %q", got.Name, filepath.Base(project))
	}
}

func TestCodexCmdRequiresAbsoluteUnixAppServer(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "test-peer")
	t.Setenv("INTERCOM_SOCKET", filepath.Join(t.TempDir(), "broker.sock"))

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing", want: "required flag"},
		{name: "relative path", args: []string{"--app-server", "relative.sock"}, want: "invalid --app-server"},
		{name: "relative endpoint", args: []string{"--app-server", "unix://relative/socket.sock"}, want: "invalid --app-server"},
		{name: "wrong scheme", args: []string{"--app-server", "ws://localhost/socket"}, want: "invalid --app-server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			err := executeCodexCommand(t, func(context.Context, codex.Config) error {
				called = true
				return nil
			}, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want text %q", err, tt.want)
			}
			if called {
				t.Fatal("runner called for invalid app-server")
			}
		})
	}
}

func TestCodexCmdSignalCancellationIsClean(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "test-peer")
	t.Setenv("INTERCOM_SOCKET", filepath.Join(t.TempDir(), "broker.sock"))
	started := make(chan struct{})
	run := func(ctx context.Context, _ codex.Config) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}

	cmd := configuredCodexCommand(t, run, "--app-server", "unix:///tmp/intercom-codex-test.sock")
	done := make(chan error, 1)
	go func() { done <- cmd.Execute() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start")
	}
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute after SIGHUP = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("command did not stop after SIGHUP")
	}
}

func TestCodexCmdPropagatesStartupError(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "test-peer")
	t.Setenv("INTERCOM_SOCKET", filepath.Join(t.TempDir(), "broker.sock"))
	want := errors.New("codex: app-server unavailable: connection refused")
	err := executeCodexCommand(t, func(context.Context, codex.Config) error {
		return want
	}, "--app-server", "unix:///tmp/intercom-codex-test.sock")
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func executeCodexCommand(t *testing.T, run codexRunner, args ...string) error {
	t.Helper()
	return configuredCodexCommand(t, run, args...).Execute()
}

func configuredCodexCommand(t *testing.T, run codexRunner, args ...string) *cobra.Command {
	t.Helper()
	cmd := newCodexCmdWithRunner(run)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd
}
