package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/codex"
	"github.com/dpemmons/intercom/internal/codexinstance"
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

func TestCodexMCPBridgeCmdValidatesInternalOptions(t *testing.T) {
	newCommand := func(args ...string) *cobra.Command {
		cmd := newCodexMCPBridgeCmd()
		cmd.SetArgs(args)
		cmd.SetIn(strings.NewReader(""))
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd
	}

	t.Setenv(codexBridgeTokenEnv, "")
	err := newCommand("--socket", filepath.Join(t.TempDir(), "bridge.sock")).Execute()
	if err == nil || !strings.Contains(err.Error(), "token is not set") {
		t.Fatalf("missing-token error = %v", err)
	}

	t.Setenv(codexBridgeTokenEnv, strings.Repeat("a", 64))
	err = newCommand("--socket", filepath.Join(t.TempDir(), "bridge.sock"), "--timeout", "0s").Execute()
	if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("invalid-timeout error = %v", err)
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

func TestCodexCmdValidatesAndWiresClientEndpoint(t *testing.T) {
	project := t.TempDir()
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	clientSocket := filepath.Join(t.TempDir(), "client.sock")
	t.Setenv("INTERCOM_DIR", t.TempDir())
	t.Setenv("INTERCOM_NAME", "test-peer")
	t.Setenv("INTERCOM_SOCKET", filepath.Join(t.TempDir(), "broker.sock"))

	var got codex.Config
	err := executeCodexCommand(t, func(_ context.Context, cfg codex.Config) error {
		got = cfg
		return nil
	}, "--app-server", appSocket, "--client-endpoint", clientSocket, "--cwd", project)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := appserver.ParseUnixEndpoint(got.ClientEndpoint)
	if err != nil {
		t.Fatalf("ClientEndpoint = %q: %v", got.ClientEndpoint, err)
	}
	if parsed != clientSocket {
		t.Fatalf("client socket = %q, want %q", parsed, clientSocket)
	}
	if got.OnReady == nil {
		t.Fatal("OnReady is nil with --client-endpoint")
	}
	if got.OnStopping == nil {
		t.Fatal("OnStopping is nil with --client-endpoint")
	}

	tests := []struct {
		name   string
		client string
		want   string
	}{
		{name: "relative path", client: "client.sock", want: "invalid --client-endpoint"},
		{name: "relative endpoint", client: "unix://relative/client.sock", want: "invalid --client-endpoint"},
		{name: "wrong scheme", client: "ws://localhost/client.sock", want: "invalid --client-endpoint"},
		{name: "same as app-server", client: appSocket, want: "must differ from --app-server"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			err := executeCodexCommand(t, func(context.Context, codex.Config) error {
				called = true
				return nil
			}, "--app-server", appSocket, "--client-endpoint", tt.client, "--cwd", project)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want text %q", err, tt.want)
			}
			if called {
				t.Fatal("runner called for invalid client endpoint")
			}
		})
	}
}

func TestCodexCmdPublishesReadinessAndRemovesDescriptor(t *testing.T) {
	project := t.TempDir()
	runtimeDir := t.TempDir()
	appSocket := filepath.Join(t.TempDir(), "app-server.sock")
	clientSocket := filepath.Join(t.TempDir(), "client.sock")
	brokerSocket := filepath.Join(t.TempDir(), "broker.sock")
	t.Setenv("INTERCOM_DIR", runtimeDir)
	t.Setenv("INTERCOM_SOCKET", brokerSocket)
	t.Setenv("INTERCOM_NAME", "")
	t.Setenv("INTERCOM_BIN", "intercom-test")
	t.Setenv(codexBinEnv, "codex-test")
	codexHome := filepath.Join(t.TempDir(), "Codex Home")
	t.Setenv("CODEX_HOME", codexHome)

	var output bytes.Buffer
	var live codexinstance.Descriptor
	run := func(_ context.Context, cfg codex.Config) error {
		if cfg.OnReady == nil {
			return errors.New("OnReady is nil")
		}
		if err := cfg.OnReady(codex.ReadyInfo{
			Name:            cfg.Name,
			CWD:             cfg.CWD,
			ThreadID:        "019-thread-id",
			ClientEndpoint:  cfg.ClientEndpoint,
			CodexVersion:    "codex-cli 0.144.1",
			ExecutionPolicy: cfg.ExecutionPolicy,
		}); err != nil {
			return err
		}
		registry := openTestCodexRegistry(t)
		descriptor, err := registry.Load(brokerSocket, cfg.Name)
		if err != nil {
			return err
		}
		if descriptor == nil {
			return errors.New("live descriptor was not published")
		}
		live = *descriptor
		return nil
	}

	cmd := newCodexCmdWithRunner(run)
	cmd.SetArgs([]string{
		"--app-server", appSocket,
		"--client-endpoint", clientSocket,
		"--name", "PrologMotion",
		"--cwd", project,
	})
	cmd.SetOut(&output)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	wantEndpoint, err := normalizeOptionalClientEndpoint(clientSocket)
	if err != nil {
		t.Fatal(err)
	}
	wantRuntimeDir, err := canonicalExistingDirectory(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	wantBrokerSocket, err := codexinstance.CanonicalBrokerSocket(brokerSocket)
	if err != nil {
		t.Fatal(err)
	}
	wantCodexHome, err := portableOptionalPath(codexHome)
	if err != nil {
		t.Fatal(err)
	}
	wantAttach := strings.Join([]string{
		"INTERCOM_DIR=" + shellQuote(wantRuntimeDir),
		"INTERCOM_SOCKET=" + shellQuote(wantBrokerSocket),
		"CODEX_BIN=codex-test",
		"CODEX_HOME=" + shellQuote(wantCodexHome),
		"'intercom-test' codex attach --name PrologMotion",
	}, " ")
	wantDirect := "CODEX_HOME=" + shellQuote(wantCodexHome) +
		" 'codex-test' resume --remote " + shellQuote(wantEndpoint) + " 019-thread-id"
	wantOutput := fmt.Sprintf(readinessText, "PrologMotion", codex.ExecutionWorkspaceWrite, wantAttach, wantDirect)
	if output.String() != wantOutput {
		t.Fatalf("readiness output:\n%s\nwant:\n%s", output.String(), wantOutput)
	}
	if live.SchemaVersion != codexinstance.SchemaVersion ||
		live.Peer != "PrologMotion" ||
		live.CWD != project ||
		live.BrokerSocketIdentity != brokerSocket ||
		live.DownstreamUnixEndpoint != wantEndpoint ||
		live.ThreadID != "019-thread-id" ||
		live.PID != os.Getpid() ||
		live.InstanceNonce == "" ||
		live.CodexVersion != "codex-cli 0.144.1" ||
		live.ExecutionPolicy != codexinstance.ExecutionWorkspaceWrite {
		t.Fatalf("live descriptor = %+v", live)
	}

	descriptor, err := openTestCodexRegistry(t).Load(brokerSocket, "PrologMotion")
	if err != nil {
		t.Fatal(err)
	}
	if descriptor != nil {
		t.Fatalf("descriptor remains after service exit: %+v", descriptor)
	}
}

func TestCodexCmdPublishesCanonicalCWDThroughSymlink(t *testing.T) {
	project := t.TempDir()
	link := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(project, link); err != nil {
		t.Fatal(err)
	}
	brokerSocket := filepath.Join(t.TempDir(), "broker.sock")
	t.Setenv("INTERCOM_DIR", t.TempDir())
	t.Setenv("INTERCOM_SOCKET", brokerSocket)

	var live codexinstance.Descriptor
	run := func(_ context.Context, cfg codex.Config) error {
		if err := cfg.OnReady(codex.ReadyInfo{
			Name: cfg.Name, CWD: project, ThreadID: "thread-id",
			ClientEndpoint: cfg.ClientEndpoint, CodexVersion: appserver.MinimumSupportedVersion,
			ExecutionPolicy: cfg.ExecutionPolicy,
		}); err != nil {
			return err
		}
		descriptor, err := openTestCodexRegistry(t).Load(brokerSocket, cfg.Name)
		if err != nil {
			return err
		}
		live = *descriptor
		return nil
	}
	cmd := newCodexCmdWithRunner(run)
	cmd.SetArgs([]string{
		"--app-server", filepath.Join(t.TempDir(), "app.sock"),
		"--client-endpoint", filepath.Join(t.TempDir(), "client.sock"),
		"--name", "symlinked", "--cwd", link,
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if live.CWD != project {
		t.Fatalf("descriptor cwd = %q, want canonical %q", live.CWD, project)
	}
}

func TestCodexCmdRemovesDescriptorWhenReadinessOutputFails(t *testing.T) {
	project := t.TempDir()
	brokerSocket := filepath.Join(t.TempDir(), "broker.sock")
	t.Setenv("INTERCOM_DIR", t.TempDir())
	t.Setenv("INTERCOM_SOCKET", brokerSocket)
	t.Setenv("INTERCOM_NAME", "test-peer")

	run := func(_ context.Context, cfg codex.Config) error {
		return cfg.OnReady(codex.ReadyInfo{
			Name:            cfg.Name,
			CWD:             cfg.CWD,
			ThreadID:        "thread-id",
			ClientEndpoint:  cfg.ClientEndpoint,
			CodexVersion:    "codex-cli 0.144.1",
			ExecutionPolicy: cfg.ExecutionPolicy,
		})
	}
	cmd := newCodexCmdWithRunner(run)
	cmd.SetArgs([]string{
		"--app-server", filepath.Join(t.TempDir(), "app-server.sock"),
		"--client-endpoint", filepath.Join(t.TempDir(), "client.sock"),
		"--cwd", project,
	})
	cmd.SetOut(codexFailingWriter{})
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "write readiness instructions: output unavailable") {
		t.Fatalf("error = %v", err)
	}
	descriptor, loadErr := openTestCodexRegistry(t).Load(brokerSocket, "test-peer")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if descriptor != nil {
		t.Fatalf("descriptor remains after output error: %+v", descriptor)
	}
}

func TestCodexAttachExecutesResumeInManagedCWD(t *testing.T) {
	runtimeDir := t.TempDir()
	project := t.TempDir()
	brokerSocket := filepath.Join(t.TempDir(), "broker.sock")
	codexBin := filepath.Join(t.TempDir(), "codex-test")
	if err := os.WriteFile(codexBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INTERCOM_DIR", runtimeDir)
	t.Setenv("INTERCOM_SOCKET", brokerSocket)
	t.Setenv(codexBinEnv, codexBin)
	t.Setenv("ATTACH_TEST_SENTINEL", "inherited")

	endpoint, err := codexinstance.CanonicalUnixEndpoint("unix:///tmp/intercom-client.sock")
	if err != nil {
		t.Fatal(err)
	}
	descriptor := codexinstance.Descriptor{
		SchemaVersion:          codexinstance.SchemaVersion,
		Peer:                   "PrologMotion",
		CWD:                    project,
		BrokerSocketIdentity:   brokerSocket,
		DownstreamUnixEndpoint: endpoint,
		ThreadID:               "019-thread-id",
		PID:                    os.Getpid(),
		InstanceNonce:          "attach-test-nonce",
		CodexVersion:           "codex-cli 0.144.1",
		ExecutionPolicy:        codexinstance.ExecutionWorkspaceWrite,
	}
	publishTestDescriptor(t, descriptor)

	before, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var gotPath string
	var gotArgv, gotEnv []string
	var gotCWD string
	replace := func(path string, argv, env []string) error {
		gotPath = path
		gotArgv = slices.Clone(argv)
		gotEnv = slices.Clone(env)
		gotCWD, _ = os.Getwd()
		return nil
	}
	cmd := newCodexCmdWithDependencies(func(context.Context, codex.Config) error {
		return errors.New("service runner must not run")
	}, replace)
	cmd.SetArgs([]string{"attach", "--name", "PrologMotion"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotPath != codexBin {
		t.Fatalf("exec path = %q, want %q", gotPath, codexBin)
	}
	wantArgv := []string{codexBin, "resume", "--remote", endpoint, "019-thread-id"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Fatalf("exec argv = %#v, want %#v", gotArgv, wantArgv)
	}
	if gotCWD != project {
		t.Fatalf("exec cwd = %q, want %q", gotCWD, project)
	}
	if !slices.Contains(gotEnv, "ATTACH_TEST_SENTINEL=inherited") {
		t.Fatalf("exec environment does not contain sentinel: %#v", gotEnv)
	}
	after, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("cwd after injected exec = %q, want restored %q", after, before)
	}

	descriptor.ExecutionPolicy = codexinstance.ExecutionDangerFullAccess
	publishTestDescriptor(t, descriptor)
	gotArgv = nil
	cmd = newCodexCmdWithDependencies(func(context.Context, codex.Config) error {
		return errors.New("service runner must not run")
	}, replace)
	cmd.SetArgs([]string{"attach", "--name", "PrologMotion"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	wantArgv = []string{codexBin, "resume", "--dangerously-bypass-approvals-and-sandbox", "--remote", endpoint, "019-thread-id"}
	if !slices.Equal(gotArgv, wantArgv) {
		t.Fatalf("danger-full-access exec argv = %#v, want %#v", gotArgv, wantArgv)
	}
}

func TestCodexAttachResolvesRelativeExecutableBeforeChangingDirectory(t *testing.T) {
	callerCWD := t.TempDir()
	project := t.TempDir()
	brokerSocket := filepath.Join(t.TempDir(), "broker.sock")
	codexBin := filepath.Join(callerCWD, "codex-relative")
	if err := os.WriteFile(codexBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(callerCWD); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalCWD); err != nil {
			t.Errorf("restore test cwd: %v", err)
		}
	})
	t.Setenv("INTERCOM_DIR", t.TempDir())
	t.Setenv("INTERCOM_SOCKET", brokerSocket)
	t.Setenv(codexBinEnv, "./codex-relative")
	publishTestDescriptor(t, codexinstance.Descriptor{
		SchemaVersion:          codexinstance.SchemaVersion,
		Peer:                   "relative-bin-peer",
		CWD:                    project,
		BrokerSocketIdentity:   brokerSocket,
		DownstreamUnixEndpoint: "unix:///tmp/intercom-relative-bin.sock",
		ThreadID:               "relative-bin-thread",
		PID:                    os.Getpid(),
		InstanceNonce:          "relative-bin-test-nonce",
		CodexVersion:           "codex-cli 0.144.1",
		ExecutionPolicy:        codexinstance.ExecutionWorkspaceWrite,
	})

	var gotPath, gotCWD string
	replace := func(path string, _ []string, _ []string) error {
		gotPath = path
		gotCWD, _ = os.Getwd()
		return nil
	}
	cmd := newCodexCmdWithDependencies(func(context.Context, codex.Config) error {
		return errors.New("service runner must not run")
	}, replace)
	cmd.SetArgs([]string{"attach", "--name", "relative-bin-peer"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if gotPath != codexBin {
		t.Fatalf("exec path = %q, want %q", gotPath, codexBin)
	}
	if gotCWD != project {
		t.Fatalf("exec cwd = %q, want %q", gotCWD, project)
	}
	if after, err := os.Getwd(); err != nil || after != callerCWD {
		t.Fatalf("cwd after injected exec = %q, %v; want %q", after, err, callerCWD)
	}
}

func TestCodexAttachReportsMissingAndStaleInstances(t *testing.T) {
	brokerSocket := filepath.Join(t.TempDir(), "broker.sock")
	t.Setenv("INTERCOM_DIR", t.TempDir())
	t.Setenv("INTERCOM_SOCKET", brokerSocket)
	t.Setenv(codexBinEnv, filepath.Join(t.TempDir(), "must-not-run"))

	called := false
	replace := func(string, []string, []string) error {
		called = true
		return nil
	}
	newAttach := func(name string) error {
		cmd := newCodexCmdWithDependencies(func(context.Context, codex.Config) error {
			return errors.New("service runner must not run")
		}, replace)
		cmd.SetArgs([]string{"attach", "--name", name})
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		return cmd.Execute()
	}

	err := newAttach("missing-peer")
	if err == nil || !strings.Contains(err.Error(), `no live Codex instance named "missing-peer"`) {
		t.Fatalf("missing error = %v", err)
	}

	publishTestDescriptor(t, codexinstance.Descriptor{
		SchemaVersion:          codexinstance.SchemaVersion,
		Peer:                   "stale-peer",
		CWD:                    t.TempDir(),
		BrokerSocketIdentity:   brokerSocket,
		DownstreamUnixEndpoint: "unix:///tmp/intercom-stale.sock",
		ThreadID:               "thread-id",
		PID:                    1 << 30,
		InstanceNonce:          "stale-test-nonce",
		CodexVersion:           "codex-cli 0.144.1",
		ExecutionPolicy:        codexinstance.ExecutionWorkspaceWrite,
	})
	err = newAttach("stale-peer")
	if err == nil || !errors.Is(err, codexinstance.ErrStale) {
		t.Fatalf("stale error = %v, want ErrStale", err)
	}
	if called {
		t.Fatal("exec called for missing or stale instance")
	}
}

func TestCodexAttachRequiresExplicitValidName(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "environment-peer")
	for _, args := range [][]string{
		{"attach"},
		{"attach", "--name", " "},
		{"attach", "--name", "not valid"},
	} {
		cmd := newCodexCmdWithDependencies(func(context.Context, codex.Config) error { return nil }, func(string, []string, []string) error {
			t.Fatal("exec called")
			return nil
		})
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("Execute(%q) succeeded", args)
		}
	}
}

func TestShellQuote(t *testing.T) {
	tests := map[string]string{
		"unix:///tmp/client.sock": "unix:///tmp/client.sock",
		"PrologMotion":            "PrologMotion",
		"":                        "''",
		"two words":               "'two words'",
		"assignment=value":        "'assignment=value'",
		"thread'id":               `'thread'"'"'id'`,
		"line\nbreak":             "'line\nbreak'",
	}
	for input, want := range tests {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestShellCommandWordAlwaysQuotesShellSyntax(t *testing.T) {
	for input, want := range map[string]string{
		"codex":       "'codex'",
		"if":          "'if'",
		"FOO=bar":     "'FOO=bar'",
		"path/tool":   "'path/tool'",
		"tool's path": `'tool'"'"'s path'`,
	} {
		if got := shellCommandWord(input); got != want {
			t.Errorf("shellCommandWord(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestReadinessCommandsOmitUnsetCodexHomeAndResolveRelativeExecutables(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	intercomBin, err := portableExecutableValue("./bin/intercom", "intercom")
	if err != nil {
		t.Fatal(err)
	}
	codexBin, err := portableExecutableValue("./bin/codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	p := codexPublication{
		peer:           "reviewer",
		intercomDir:    filepath.Join(cwd, "state dir"),
		intercomBin:    intercomBin,
		brokerSocket:   filepath.Join(cwd, "broker socket"),
		clientEndpoint: "unix:///runtime/client.sock",
		codexBin:       codexBin,
	}
	wantAttach := "INTERCOM_DIR=" + shellQuote(p.intercomDir) +
		" INTERCOM_SOCKET=" + shellQuote(p.brokerSocket) +
		" CODEX_BIN=" + shellQuote(codexBin) +
		" " + shellCommandWord(intercomBin) + " codex attach --name reviewer"
	if got := p.attachCommand(); got != wantAttach {
		t.Fatalf("attachCommand() = %q, want %q", got, wantAttach)
	}
	if strings.Contains(p.attachCommand(), "CODEX_HOME") {
		t.Fatalf("attachCommand() includes unset CODEX_HOME: %q", p.attachCommand())
	}
	wantDirect := shellCommandWord(codexBin) + " resume --remote unix:///runtime/client.sock thread-1"
	if got := p.directCommand("thread-1"); got != wantDirect {
		t.Fatalf("directCommand() = %q, want %q", got, wantDirect)
	}
	if intercomBin != filepath.Join(cwd, "bin", "intercom") || codexBin != filepath.Join(cwd, "bin", "codex") {
		t.Fatalf("portable executables = %q, %q", intercomBin, codexBin)
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

type codexFailingWriter struct{}

func (codexFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

func openTestCodexRegistry(t *testing.T) *codexinstance.Registry {
	t.Helper()
	dir := os.Getenv("INTERCOM_DIR")
	registry, err := codexinstance.New(filepath.Join(dir, "codex", "live"))
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func publishTestDescriptor(t *testing.T, descriptor codexinstance.Descriptor) {
	t.Helper()
	if _, err := openTestCodexRegistry(t).Publish(descriptor); err != nil {
		t.Fatal(err)
	}
}
