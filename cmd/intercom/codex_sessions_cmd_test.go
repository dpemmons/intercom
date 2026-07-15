package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/codex"
)

type fakeSessionClient struct {
	initParams       []appserver.InitializeParams
	initialized      int
	closed           int
	listParams       []appserver.ThreadListParams
	listResponses    []appserver.ThreadListResponse
	listErr          error
	initializeErr    error
	initializedError error
}

func (f *fakeSessionClient) Initialize(_ context.Context, params appserver.InitializeParams) (appserver.InitializeResponse, error) {
	f.initParams = append(f.initParams, params)
	return appserver.InitializeResponse{UserAgent: "codex_cli_rs/0.144.4"}, f.initializeErr
}

func (f *fakeSessionClient) Initialized(context.Context) error {
	f.initialized++
	return f.initializedError
}

func (f *fakeSessionClient) ThreadList(_ context.Context, params appserver.ThreadListParams) (appserver.ThreadListResponse, error) {
	f.listParams = append(f.listParams, params)
	if f.listErr != nil {
		return appserver.ThreadListResponse{}, f.listErr
	}
	if len(f.listResponses) == 0 {
		return appserver.ThreadListResponse{}, nil
	}
	response := f.listResponses[0]
	f.listResponses = f.listResponses[1:]
	return response, nil
}

func (f *fakeSessionClient) ThreadRead(context.Context, appserver.ThreadReadParams) (appserver.ThreadReadResponse, error) {
	return appserver.ThreadReadResponse{}, errors.New("unexpected thread/read")
}

func (f *fakeSessionClient) Close() error {
	f.closed++
	return nil
}

func interactiveThread(id, cwd string, updated int64) appserver.Thread {
	return appserver.Thread{
		ID: id, CWD: cwd, Source: json.RawMessage(`"cli"`),
		Status:    appserver.ThreadStatus{Type: appserver.ThreadStatusNotLoaded},
		UpdatedAt: updated, Preview: "review the parser",
	}
}

func TestCodexSessionsListIsMachineReadable(t *testing.T) {
	project := t.TempDir()
	projectLink := filepath.Join(t.TempDir(), "project-link")
	if err := os.Symlink(project, projectLink); err != nil {
		t.Fatal(err)
	}
	client := &fakeSessionClient{listResponses: []appserver.ThreadListResponse{{
		Data: []appserver.Thread{interactiveThread("thread-new", project, 20), interactiveThread("thread-old", project, 10)},
	}}}
	dial := func(_ context.Context, endpoint string, _ appserver.Options) (codexSessionClient, error) {
		if endpoint != "unix:///tmp/app-server.sock" {
			t.Fatalf("endpoint = %q", endpoint)
		}
		return client, nil
	}
	cmd := newCodexSessionsCmdWithDependencies(dial, func(io.Reader) bool { return false })
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--app-server", "/tmp/app-server.sock", "--cwd", projectLink, "--list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "thread-new\t") || !strings.HasPrefix(lines[1], "thread-old\t") {
		t.Fatalf("session list = %q", stdout.String())
	}
	if stderr.Len() != 0 || len(client.initParams) != 1 || client.initialized != 1 || client.closed != 1 {
		t.Fatalf("client lifecycle: init=%d initialized=%d closed=%d stderr=%q", len(client.initParams), client.initialized, client.closed, stderr.String())
	}
	if len(client.listParams) != 1 || client.listParams[0].CWD != project {
		t.Fatalf("thread/list params = %#v", client.listParams)
	}
}

func TestCodexSessionsPickerUsesDiagnosticStreamAndOnlyIDOnStdout(t *testing.T) {
	project := t.TempDir()
	client := &fakeSessionClient{listResponses: []appserver.ThreadListResponse{{
		Data: []appserver.Thread{interactiveThread("selected-thread", project, 20)},
	}}}
	cmd := newCodexSessionsCmdWithDependencies(
		func(context.Context, string, appserver.Options) (codexSessionClient, error) { return client, nil },
		func(io.Reader) bool { return true },
	)
	var stdout, stderr bytes.Buffer
	cmd.SetIn(strings.NewReader("1\n"))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"--app-server", "/tmp/app-server.sock", "--cwd", project})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "selected-thread\n" {
		t.Fatalf("selector stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "Resumable Codex sessions") || !strings.Contains(stderr.String(), "selected-thread") {
		t.Fatalf("selector diagnostics = %q", stderr.String())
	}
}

func TestCodexSessionsPickerRejectsNonTerminalInput(t *testing.T) {
	project := t.TempDir()
	client := &fakeSessionClient{listResponses: []appserver.ThreadListResponse{{
		Data: []appserver.Thread{interactiveThread("selected-thread", project, 20)},
	}}}
	cmd := newCodexSessionsCmdWithDependencies(
		func(context.Context, string, appserver.Options) (codexSessionClient, error) { return client, nil },
		func(io.Reader) bool { return false },
	)
	cmd.SetIn(strings.NewReader("1\n"))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--app-server", "/tmp/app-server.sock", "--cwd", project})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("Execute() error = %v", err)
	}
}

func TestCodexCommandForwardsSelectionAndExecutionPolicy(t *testing.T) {
	project := t.TempDir()
	t.Setenv("INTERCOM_DIR", t.TempDir())
	t.Setenv("INTERCOM_SOCKET", filepath.Join(t.TempDir(), "broker.sock"))
	t.Setenv("INTERCOM_NAME", "")

	for _, flag := range []string{"--yolo", "--dangerously-bypass-approvals-and-sandbox"} {
		t.Run(flag, func(t *testing.T) {
			var got codex.Config
			cmd := newCodexCmdWithRunner(func(_ context.Context, cfg codex.Config) error {
				got = cfg
				return nil
			})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs([]string{
				"--app-server", "/tmp/app-server.sock",
				"--mcp-bridge", "/tmp/private-mcp.sock",
				"--cwd", project,
				"--name", "selected",
				"--adopt-session", "thread-123",
				"--replace-binding",
				flag,
			})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got.AdoptThreadID != "thread-123" || !got.ReplaceBinding || got.ExecutionPolicy != codex.ExecutionDangerFullAccess ||
				got.MCPBridgeSocket != "/tmp/private-mcp.sock" || !filepath.IsAbs(got.IntercomBin) {
				t.Fatalf("codex config = %#v", got)
			}
		})
	}
}
