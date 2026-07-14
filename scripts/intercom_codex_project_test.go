package scripts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	helperEnv    = "INTERCOM_LAUNCHER_TEST_HELPER"
	helperMode   = "INTERCOM_LAUNCHER_TEST_MODE"
	helperEvents = "INTERCOM_LAUNCHER_TEST_EVENTS"
)

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		os.Exit(runLauncherHelper())
	}
	os.Exit(m.Run())
}

func TestLauncherAdapterExitStopsServer(t *testing.T) {
	result := runLauncher(t, "adapter-first", "--name", "reviewer", "--cwd", "/tmp/project")
	if result.exitCode != 17 {
		t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", result.exitCode, result.stderr)
	}
	wantEventsInOrder(t, result.events, "adapter-start", "server-term")
	if !strings.Contains(result.events, "adapter-args=codex\x1f--app-server\x1funix://") ||
		!strings.Contains(result.events, "\x1f--name\x1freviewer\x1f--cwd\x1f/tmp/project") {
		t.Fatalf("adapter arguments not forwarded as expected:\n%s", result.events)
	}
}

func TestLauncherServerExitStopsAdapter(t *testing.T) {
	result := runLauncher(t, "server-first")
	if result.exitCode != 23 {
		t.Fatalf("exit code = %d, want server code 23; stderr:\n%s", result.exitCode, result.stderr)
	}
	wantEventsInOrder(t, result.events, "server-exit", "adapter-term")
}

func TestLauncherReadinessTimeoutStopsServer(t *testing.T) {
	result := runLauncher(t, "readiness-timeout")
	if result.exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", result.exitCode, result.stderr)
	}
	if !strings.Contains(result.stderr, "app-server was not ready after 1s") {
		t.Fatalf("missing readiness error; stderr:\n%s", result.stderr)
	}
	if strings.Contains(result.events, "adapter-start") {
		t.Fatalf("adapter started before server readiness:\n%s", result.events)
	}
	wantEventsInOrder(t, result.events, "server-start-no-socket", "server-term")
}

func TestLauncherServerExitBeforeReadiness(t *testing.T) {
	result := runLauncher(t, "server-exit-before-ready")
	if result.exitCode != 19 {
		t.Fatalf("exit code = %d, want server code 19; stderr:\n%s", result.exitCode, result.stderr)
	}
	if strings.Contains(result.events, "adapter-start") {
		t.Fatalf("adapter started after early server exit:\n%s", result.events)
	}
	wantEventsInOrder(t, result.events, "server-exit-before-ready")
}

func TestLauncherSignalStopsAdapterBeforeServer(t *testing.T) {
	result := startAndSignalLauncher(t, "signal", syscall.SIGTERM)
	if result.exitCode != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", result.exitCode, result.stderr)
	}
	wantEventsInOrder(t, result.events, "adapter-term", "server-term")
}

func TestLauncherSignalExitCodes(t *testing.T) {
	for _, tt := range []struct {
		name     string
		signal   syscall.Signal
		exitCode int
	}{
		{name: "interrupt", signal: syscall.SIGINT, exitCode: 130},
		{name: "hangup", signal: syscall.SIGHUP, exitCode: 129},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := startAndSignalLauncher(t, "signal", tt.signal)
			if result.exitCode != tt.exitCode {
				t.Fatalf("exit code = %d, want %d; stderr:\n%s", result.exitCode, tt.exitCode, result.stderr)
			}
			wantEventsInOrder(t, result.events, "adapter-term", "server-term")
		})
	}
}

func TestLauncherEscalatesAdapterFromTermToKill(t *testing.T) {
	result := startAndSignalLauncher(t, "adapter-ignore-term", syscall.SIGTERM)
	if result.exitCode != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", result.exitCode, result.stderr)
	}
	if !strings.Contains(result.stderr, "adapter did not stop; killing it") {
		t.Fatalf("missing forced-kill diagnostic; stderr:\n%s", result.stderr)
	}
	if strings.Contains(result.events, "adapter-term") {
		t.Fatalf("TERM-ignoring adapter reported a graceful stop:\n%s", result.events)
	}
	wantEventsInOrder(t, result.events, "adapter-ignore-term", "server-term")
}

func TestLauncherAcceptsLeadingZeroTimeouts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "adapter-first")
	replaceEnv(cmd, "INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS", "0001")
	replaceEnv(cmd, "INTERCOM_CODEX_SHUTDOWN_TIMEOUT_SECONDS", "0002")
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 17 {
		t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "value too great for base") || strings.Contains(stderr.String(), "unbound variable") {
		t.Fatalf("leading-zero timeout reached invalid Bash arithmetic; stderr:\n%s", stderr.String())
	}
	wantEventsInOrder(t, readEvents(t, eventPath), "adapter-start", "server-term")
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherEmptyTimeoutsSelectDefaults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "adapter-first")
	replaceEnv(cmd, "INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS", "")
	replaceEnv(cmd, "INTERCOM_CODEX_SHUTDOWN_TIMEOUT_SECONDS", "")
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 17 {
		t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", code, stderr.String())
	}
	wantEventsInOrder(t, readEvents(t, eventPath), "adapter-start", "server-term")
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherRejectsInvalidTimeout(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
	}{
		{name: "non-number", value: "not-a-number"},
		{name: "zero", value: "000"},
		{name: "overflow", value: "922337203685477581"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal")
			replaceEnv(cmd, "INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS", tt.value)
			err := cmd.Run()
			if code := processExitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2; stderr:\n%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "must be a positive integer") {
				t.Fatalf("missing timeout validation error; stderr:\n%s", stderr.String())
			}
			if events := readEvents(t, eventPath); events != "" {
				t.Fatalf("children started with invalid timeout:\n%s", events)
			}
			assertRuntimeClean(t, runtimeDir)
		})
	}
}

func TestLauncherRejectsAppServerOverrideBeforeStartingChildren(t *testing.T) {
	for _, args := range [][]string{
		{"--app-server", "unix:///tmp/other.sock"},
		{"--app-server=unix:///tmp/other.sock"},
		{"--help", "--app-server", "unix:///tmp/other.sock"},
		{"--app-server", "unix:///tmp/other.sock", "--help"},
		{"--help", "--app-server=unix:///tmp/other.sock"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal", args...)
		err := cmd.Run()
		cancel()
		if code := processExitCode(err); code != 2 {
			t.Fatalf("args %v exit code = %d, want 2; stderr:\n%s", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "--app-server is managed by the launcher") {
			t.Fatalf("args %v missing override error; stderr:\n%s", args, stderr.String())
		}
		if events := readEvents(t, eventPath); events != "" {
			t.Fatalf("args %v started children:\n%s", args, events)
		}
		assertRuntimeClean(t, runtimeDir)
	}
}

func TestLauncherHelpStartsNoChildren(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal", "--help")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if code := processExitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:") || !strings.Contains(stdout.String(), "--new") {
		t.Fatalf("incomplete help output:\n%s", stdout.String())
	}
	if events := readEvents(t, eventPath); events != "" {
		t.Fatalf("children started while printing help:\n%s", events)
	}
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherHelpSuppressesForwardedAndTimeoutValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal", "--bogus", "--help")
	replaceEnv(cmd, "INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS", "invalid")
	replaceEnv(cmd, "INTERCOM_CODEX_SHUTDOWN_TIMEOUT_SECONDS", "0")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if code := processExitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage:") {
		t.Fatalf("missing help output:\n%s", stdout.String())
	}
	if events := readEvents(t, eventPath); events != "" {
		t.Fatalf("children started while printing help:\n%s", events)
	}
	assertRuntimeClean(t, runtimeDir)
}

type launcherResult struct {
	exitCode int
	stderr   string
	events   string
	runtime  string
}

func runLauncher(t *testing.T, mode string, args ...string) launcherResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, mode, args...)
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	result := launcherResult{
		exitCode: processExitCode(err),
		stderr:   stderr.String(),
		events:   readEvents(t, eventPath),
		runtime:  runtimeDir,
	}
	assertRuntimeClean(t, result.runtime)
	return result
}

func startAndSignalLauncher(t *testing.T, mode string, signal syscall.Signal) launcherResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, mode)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	waitForEvent(t, eventPath, "adapter-start", 5*time.Second)
	if err := cmd.Process.Signal(signal); err != nil {
		t.Fatalf("signal launcher: %v", err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	result := launcherResult{
		exitCode: processExitCode(err),
		stderr:   stderr.String(),
		events:   readEvents(t, eventPath),
		runtime:  runtimeDir,
	}
	assertRuntimeClean(t, result.runtime)
	return result
}

func launcherCommand(t *testing.T, ctx context.Context, mode string, args ...string) (*exec.Cmd, *bytes.Buffer, string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	script, err := filepath.Abs("intercom-codex-project")
	if err != nil {
		t.Fatalf("resolve launcher: %v", err)
	}
	runtimeDir, err := os.MkdirTemp("/tmp", "icx-")
	if err != nil {
		t.Fatalf("create short runtime directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	eventPath := filepath.Join(t.TempDir(), "events")

	cmd := exec.CommandContext(ctx, "bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(),
		helperEnv+"=1",
		helperMode+"="+mode,
		helperEvents+"="+eventPath,
		"CODEX_BIN="+executable,
		"INTERCOM_BIN="+executable,
		"XDG_RUNTIME_DIR="+runtimeDir,
		"INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS=1",
		"INTERCOM_CODEX_SHUTDOWN_TIMEOUT_SECONDS=1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd, &stderr, eventPath, runtimeDir
}

func replaceEnv(cmd *exec.Cmd, key, value string) {
	prefix := key + "="
	filtered := cmd.Env[:0]
	for _, entry := range cmd.Env {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	cmd.Env = append(filtered, prefix+value)
}

func assertRuntimeClean(t *testing.T, runtimeDir string) {
	t.Helper()
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatalf("read runtime base: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("launcher left runtime entries behind: %v", entries)
	}
}

func processExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func wantEventsInOrder(t *testing.T, events string, wants ...string) {
	t.Helper()
	position := 0
	for _, want := range wants {
		relative := strings.Index(events[position:], want)
		if relative < 0 {
			t.Fatalf("event %q not found after byte %d:\n%s", want, position, events)
		}
		position += relative + len(want)
	}
}

func waitForEvent(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for event %q; events:\n%s", want, readEvents(t, path))
}

func readEvents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read helper events: %v", err)
	}
	return string(data)
}

func runLauncherHelper() int {
	args := os.Args[1:]
	if len(args) == 0 {
		return 90
	}
	switch args[0] {
	case "app-server":
		return runServerHelper(args)
	case "codex":
		return runAdapterHelper(args)
	default:
		return 91
	}
}

func runServerHelper(args []string) int {
	mode := os.Getenv(helperMode)
	if mode == "server-exit-before-ready" {
		recordEvent("server-exit-before-ready")
		return 19
	}
	if mode == "readiness-timeout" {
		recordEvent("server-start-no-socket")
		waitForSignal()
		recordEvent("server-term")
		return 0
	}
	endpoint, ok := flagValue(args, "--listen")
	if !ok || !strings.HasPrefix(endpoint, "unix://") {
		return 92
	}
	listener, err := net.Listen("unix", strings.TrimPrefix(endpoint, "unix://"))
	if err != nil {
		recordEvent("server-listen-error=" + err.Error())
		return 93
	}
	defer listener.Close()
	recordEvent("server-start")
	if mode == "server-first" {
		if !helperWaitForEvent("adapter-start", 5*time.Second) {
			return 94
		}
		recordEvent("server-exit")
		return 23
	}
	waitForSignal()
	recordEvent("server-term")
	return 0
}

func runAdapterHelper(args []string) int {
	mode := os.Getenv(helperMode)
	if mode == "adapter-ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	recordEvent("adapter-start")
	recordEvent("adapter-args=" + strings.Join(args, "\x1f"))
	if mode == "adapter-first" {
		return 17
	}
	if mode == "adapter-ignore-term" {
		recordEvent("adapter-ignore-term")
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGHUP)
		<-signals
		signal.Stop(signals)
		return 0
	}
	waitForSignal()
	recordEvent("adapter-term")
	return 0
}

func waitForSignal() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	<-signals
	signal.Stop(signals)
}

func flagValue(args []string, name string) (string, bool) {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1], true
		}
	}
	return "", false
}

func recordEvent(event string) {
	path := os.Getenv(helperEvents)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return
	}
	_, _ = fmt.Fprintln(file, event)
	_ = file.Close()
}

func helperWaitForEvent(want string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(os.Getenv(helperEvents))
		if strings.Contains(string(data), want) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
