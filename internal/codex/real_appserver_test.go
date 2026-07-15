package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
)

const baselineAppServerSchemaFingerprint = "7dd903e65b126caea5a0f136b613faf86bbafa2adb9cb06b319cbf2be767156b"

func TestGeneratedSchemaFingerprintDetectsContractDrift(t *testing.T) {
	t.Parallel()
	writeSchema := func(root, relative, schema string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(schema), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint := func(root string) string {
		t.Helper()
		got, err := generatedSchemaFingerprint(root)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	baseline := t.TempDir()
	writeSchema(baseline, "v2/Example.json", `{"$schema":"draft","title":"Example","description":"baseline","type":"object","properties":{"value":{"type":"string"}}}`)
	baselineFingerprint := fingerprint(baseline)

	annotationsOnly := t.TempDir()
	writeSchema(annotationsOnly, "v2/Example.json", `{"$schema":"draft","title":"Renamed","description":"changed","properties":{"value":{"description":"changed","type":"string"}},"type":"object"}`)
	if got := fingerprint(annotationsOnly); got != baselineFingerprint {
		t.Fatalf("annotation-only fingerprint = %s, want %s", got, baselineFingerprint)
	}

	changedType := t.TempDir()
	writeSchema(changedType, "v2/Example.json", `{"$schema":"draft","type":"object","properties":{"value":{"type":"integer"}}}`)
	if got := fingerprint(changedType); got == baselineFingerprint {
		t.Fatal("nested property type change did not change schema fingerprint")
	}

	addedSchema := t.TempDir()
	writeSchema(addedSchema, "v2/Example.json", `{"$schema":"draft","type":"object","properties":{"value":{"type":"string"}}}`)
	writeSchema(addedSchema, "DynamicToolCallResponse.json", `{"type":"object","required":["success"]}`)
	if got := fingerprint(addedSchema); got == baselineFingerprint {
		t.Fatal("added schema file did not change schema fingerprint")
	}
}

// TestCompatibleCodexAppServerSchema is an opt-in structural compatibility
// check against the installed Codex experimental app-server schema.
func TestCompatibleCodexAppServerSchema(t *testing.T) {
	if os.Getenv("INTERCOM_CODEX_SMOKE") != "1" {
		t.Skip("set INTERCOM_CODEX_SMOKE=1 to exercise the installed Codex app-server")
	}
	codexBin := os.Getenv("CODEX_BIN")
	if codexBin == "" {
		var err error
		codexBin, err = exec.LookPath("codex")
		if err != nil {
			t.Fatal(err)
		}
	}

	schemaDir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), codexBin, "app-server", "generate-json-schema", "--experimental", "--out", schemaDir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate Codex app-server schema: %v\n%s", err, output)
	}

	fingerprint, err := generatedSchemaFingerprint(schemaDir)
	if err != nil {
		t.Fatal(err)
	}
	if fingerprint != baselineAppServerSchemaFingerprint {
		t.Fatalf("Codex app-server schema fingerprint = %s, want %s; review the complete generated schema before updating the baseline", fingerprint, baselineAppServerSchemaFingerprint)
	}
}

// TestCompatibleCodexAppServerSmoke is an opt-in, no-model behavioral
// compatibility check against an installed supported Codex CLI. It uses an
// isolated CODEX_HOME and never starts a turn, so it needs neither credentials
// nor network access.
func TestCompatibleCodexAppServerSmoke(t *testing.T) {
	if os.Getenv("INTERCOM_CODEX_SMOKE") != "1" {
		t.Skip("set INTERCOM_CODEX_SMOKE=1 to exercise the installed Codex app-server")
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
	t.Logf("workspace-write response: %#v", started.Sandbox)

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

func generatedSchemaFingerprint(root string) (string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && filepath.Ext(path) == ".json" {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("enumerate generated Codex app-server schemas: %w", err)
	}
	if len(paths) == 0 {
		return "", errors.New("generated Codex app-server schema contains no JSON files")
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read generated Codex app-server schema %s: %w", path, err)
		}
		var schema any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&schema); err != nil {
			return "", fmt.Errorf("decode generated Codex app-server schema %s: %w", path, err)
		}
		normalizeGeneratedSchema(schema)
		canonical, err := json.Marshal(schema)
		if err != nil {
			return "", fmt.Errorf("canonicalize generated Codex app-server schema %s: %w", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", fmt.Errorf("relativize generated Codex app-server schema %s: %w", path, err)
		}
		_, _ = hash.Write([]byte(filepath.ToSlash(relative)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(canonical)
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func normalizeGeneratedSchema(value any) {
	switch value := value.(type) {
	case map[string]any:
		delete(value, "description")
		delete(value, "title")
		for _, child := range value {
			normalizeGeneratedSchema(child)
		}
	case []any:
		for _, child := range value {
			normalizeGeneratedSchema(child)
		}
	}
}
