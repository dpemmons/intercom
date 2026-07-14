package codex

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
)

// TestPinnedCodexAppServerSmoke is an opt-in, no-model compatibility check
// against an installed codex-cli 0.144.1. It uses an isolated CODEX_HOME and
// never starts a turn, so it needs neither credentials nor network access.
func TestPinnedCodexAppServerSmoke(t *testing.T) {
	if os.Getenv("INTERCOM_CODEX_SMOKE") != "1" {
		t.Skip("set INTERCOM_CODEX_SMOKE=1 to exercise the installed pinned Codex app-server")
	}
	codexBin := os.Getenv("CODEX_BIN")
	if codexBin == "" {
		var err error
		codexBin, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "app-server.sock")
	endpoint := "unix://" + socket
	cmd := exec.CommandContext(t.Context(), codexBin, "app-server", "--listen", endpoint)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	processDone := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(processDone)
	}()
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-processDone:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-processDone
		}
	})

	var client *appserver.Client
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		var err error
		client, err = appserver.DialUnix(ctx, endpoint, appserver.Options{})
		cancel()
		if err == nil {
			break
		}
		select {
		case <-processDone:
			t.Fatalf("codex app-server exited before readiness: %v\n%s", processErr, stderr.String())
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	if client == nil {
		t.Fatalf("codex app-server did not accept a WebSocket connection\n%s", stderr.String())
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	init, err := client.Initialize(ctx, appserver.InitializeParams{
		ClientInfo:   appserver.ClientInfo{Name: "intercom-smoke", Version: "test"},
		Capabilities: &appserver.InitializeCapabilities{ExperimentalAPI: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateServerVersion(init.UserAgent); err != nil {
		t.Fatal(err)
	}
	if err := client.Initialized(ctx); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(dir, "project")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	sandbox := appserver.SandboxWorkspaceWrite
	ephemeral := false
	instructions := developerInstructions("smoke")
	started, err := client.ThreadStart(ctx, appserver.ThreadStartParams{
		CWD:                   &cwd,
		ApprovalPolicy:        string(appserver.ApprovalNever),
		Sandbox:               &sandbox,
		DeveloperInstructions: &instructions,
		Ephemeral:             &ephemeral,
		DynamicTools:          dynamicToolSpecs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.Thread.ID == "" || started.Thread.CWD != cwd || started.CWD != cwd {
		t.Fatalf("thread/start response = %#v", started)
	}
	if started.Sandbox.Type != "workspaceWrite" {
		t.Fatalf("thread/start sandbox = %#v", started.Sandbox)
	}
	if started.ApprovalPolicy != string(appserver.ApprovalNever) || len(started.Sandbox.WritableRoots) != 0 {
		t.Fatalf("thread/start policy = approval %#v, sandbox %#v", started.ApprovalPolicy, started.Sandbox)
	}
	if _, ok := started.Sandbox.NetworkAccess.(bool); !ok {
		t.Fatalf("thread/start networkAccess type = %T", started.Sandbox.NetworkAccess)
	}
	t.Logf("pinned workspace-write response: %#v", started.Sandbox)

	_, err = client.ThreadRead(ctx, appserver.ThreadReadParams{ThreadID: started.Thread.ID, IncludeTurns: true})
	if err == nil || !isBeforeFirstUserMessage(err, started.Thread.ID) {
		var rpcErr *appserver.RPCError
		if errors.As(err, &rpcErr) {
			t.Fatalf("unexpected pre-materialization error: code=%d message=%q", rpcErr.Code, rpcErr.Message)
		}
		t.Fatalf("unexpected pre-materialization result: %v", err)
	}

	_, err = client.ThreadResume(ctx, appserver.ThreadResumeParams{
		ThreadID:              started.Thread.ID,
		CWD:                   &cwd,
		ApprovalPolicy:        string(appserver.ApprovalNever),
		Sandbox:               &sandbox,
		DeveloperInstructions: &instructions,
		ExcludeTurns:          true,
	})
	if err == nil || !isMissingRollout(err, started.Thread.ID) {
		t.Fatalf("unexpected pending-thread resume result: %v", err)
	}
}
