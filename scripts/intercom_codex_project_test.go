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
	"slices"
	"strconv"
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
	adapterArgs := recordedAdapterArgs(t, result.events)
	appServerEndpoint := requiredFlagValue(t, adapterArgs, "--app-server")
	clientEndpoint := requiredFlagValue(t, adapterArgs, "--client-endpoint")
	mcpBridgePath := requiredFlagValue(t, adapterArgs, "--mcp-bridge")
	appServerPath := unixEndpointPath(t, appServerEndpoint)
	clientPath := unixEndpointPath(t, clientEndpoint)
	if filepath.Base(appServerPath) != "app-server.sock" || filepath.Base(clientPath) != "client.sock" || filepath.Base(mcpBridgePath) != "mcp-bridge.sock" {
		t.Fatalf("launcher socket paths = %q, %q, %q", appServerPath, clientPath, mcpBridgePath)
	}
	if filepath.Dir(appServerPath) != filepath.Dir(clientPath) || filepath.Dir(appServerPath) != filepath.Dir(mcpBridgePath) ||
		appServerPath == clientPath || appServerPath == mcpBridgePath || clientPath == mcpBridgePath {
		t.Fatalf("launcher endpoints are not distinct private siblings: %q, %q, %q", appServerPath, clientPath, mcpBridgePath)
	}
	if !strings.Contains(result.events, "\x1f--name\x1freviewer\x1f--cwd\x1f/tmp/project") {
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

func TestLauncherReservesStdoutForServiceReadiness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "stream-routing")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 17 {
		t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", code, stderr.String())
	}
	if got, want := stdout.String(), "service-ready\n"; got != want {
		t.Fatalf("launcher stdout = %q, want only service readiness %q", got, want)
	}
	if !strings.Contains(stderr.String(), "app-server-stdout") {
		t.Fatalf("app-server stdout was not routed to launcher stderr:\n%s", stderr.String())
	}
	wantEventsInOrder(t, readEvents(t, eventPath), "server-start", "adapter-start", "server-term")
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherSurvivesClientDisconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "client-disconnect")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	running := true
	t.Cleanup(func() {
		if running {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	waitForEvent(t, eventPath, "client-listen", 5*time.Second)
	clientEndpoint := requiredFlagValue(t, recordedAdapterArgs(t, readEvents(t, eventPath)), "--client-endpoint")
	conn, err := net.DialTimeout("unix", unixEndpointPath(t, clientEndpoint), time.Second)
	if err != nil {
		t.Fatalf("connect fake TUI client: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("disconnect fake TUI client: %v", err)
	}
	waitForEvent(t, eventPath, "client-disconnect", 5*time.Second)
	time.Sleep(100 * time.Millisecond)
	if events := readEvents(t, eventPath); strings.Contains(events, "adapter-term") || strings.Contains(events, "server-term") {
		t.Fatalf("client disconnect stopped the service group:\n%s", events)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal launcher: %v", err)
	}
	err = cmd.Wait()
	running = false
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", code, stderr.String())
	}
	wantEventsInOrder(t, readEvents(t, eventPath), "client-connect", "client-disconnect", "adapter-term", "server-term")
	assertRuntimeClean(t, runtimeDir)
}

func TestLaunchersUseDistinctEndpointsInSharedRuntimeBase(t *testing.T) {
	sharedRuntime, err := os.MkdirTemp("/tmp", "icx-shared-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sharedRuntime) })

	type runningLauncher struct {
		cmd         *exec.Cmd
		stderr      *bytes.Buffer
		eventPath   string
		runtimeBase string
		running     bool
	}
	launchers := make([]runningLauncher, 2)
	for index, name := range []string{"alpha", "beta"} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd, stderr, eventPath, runtimeBase := launcherCommand(t, ctx, "signal", "--name", name)
		replaceEnv(cmd, "XDG_RUNTIME_DIR", sharedRuntime)
		if err := cmd.Start(); err != nil {
			t.Fatalf("start launcher %s: %v", name, err)
		}
		launchers[index] = runningLauncher{cmd: cmd, stderr: stderr, eventPath: eventPath, runtimeBase: runtimeBase, running: true}
	}
	t.Cleanup(func() {
		for index := range launchers {
			launcher := &launchers[index]
			if launcher.running {
				_ = launcher.cmd.Process.Kill()
				_ = launcher.cmd.Wait()
			}
		}
	})

	runtimeDirs := make(map[string]struct{}, len(launchers))
	for index := range launchers {
		launcher := &launchers[index]
		waitForEvent(t, launcher.eventPath, "adapter-start", 5*time.Second)
		args := recordedAdapterArgs(t, readEvents(t, launcher.eventPath))
		appServerPath := unixEndpointPath(t, requiredFlagValue(t, args, "--app-server"))
		clientPath := unixEndpointPath(t, requiredFlagValue(t, args, "--client-endpoint"))
		runtimeDir := filepath.Dir(appServerPath)
		if filepath.Dir(clientPath) != runtimeDir {
			t.Fatalf("launcher %d sockets have different directories: %q, %q", index, appServerPath, clientPath)
		}
		if filepath.Dir(runtimeDir) != sharedRuntime {
			t.Fatalf("launcher %d runtime directory = %q, want child of %q", index, runtimeDir, sharedRuntime)
		}
		runtimeDirs[runtimeDir] = struct{}{}
	}
	if len(runtimeDirs) != len(launchers) {
		t.Fatalf("%d launchers used only %d runtime directories: %v", len(launchers), len(runtimeDirs), runtimeDirs)
	}

	for index := range launchers {
		launcher := &launchers[index]
		if err := launcher.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("signal launcher %d: %v", index, err)
		}
	}
	for index := range launchers {
		launcher := &launchers[index]
		err := launcher.cmd.Wait()
		launcher.running = false
		if code := processExitCode(err); code != 143 {
			t.Fatalf("launcher %d exit code = %d, want 143; stderr:\n%s", index, code, launcher.stderr.String())
		}
		assertRuntimeClean(t, launcher.runtimeBase)
	}
	assertRuntimeClean(t, sharedRuntime)
}

func TestLauncherCanonicalizesRelativeRuntimeBase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeBase := launcherCommand(t, ctx, "adapter-first")
	workingDir, err := os.MkdirTemp("/tmp", "icx-relative-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(workingDir) })
	relativeBase := "relative-runtime"
	absBase := filepath.Join(workingDir, relativeBase)
	if err := os.Mkdir(absBase, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd.Dir = workingDir
	replaceEnv(cmd, "XDG_RUNTIME_DIR", relativeBase)
	err = cmd.Run()
	if code := processExitCode(err); code != 17 {
		t.Fatalf("exit code = %d, want 17; stderr:\n%s", code, stderr.String())
	}
	args := recordedAdapterArgs(t, readEvents(t, eventPath))
	for _, flag := range []string{"--app-server", "--client-endpoint", "--mcp-bridge"} {
		path := requiredFlagValue(t, args, flag)
		if flag != "--mcp-bridge" {
			path = unixEndpointPath(t, path)
		}
		if filepath.Dir(filepath.Dir(path)) != absBase {
			t.Fatalf("%s path = %q, want child of %q", flag, path, absBase)
		}
	}
	assertRuntimeClean(t, absBase)
	assertRuntimeClean(t, runtimeBase)
}

func TestLauncherRejectsURLDelimiterInRuntimePath(t *testing.T) {
	for _, delimiter := range []string{"#", "?", "%"} {
		t.Run(delimiter, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd, stderr, eventPath, runtimeBase := launcherCommand(t, ctx, "signal")
			base := filepath.Join(t.TempDir(), "runtime"+delimiter+"base")
			if err := os.Mkdir(base, 0o700); err != nil {
				t.Fatal(err)
			}
			replaceEnv(cmd, "XDG_RUNTIME_DIR", base)
			err := cmd.Run()
			if code := processExitCode(err); code != 2 {
				t.Fatalf("exit code = %d, want 2; stderr:\n%s", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "runtime directory contains URL-delimiter byte") {
				t.Fatalf("missing runtime-path diagnostic; stderr:\n%s", stderr.String())
			}
			if events := readEvents(t, eventPath); events != "" {
				t.Fatalf("children started with invalid runtime path:\n%s", events)
			}
			assertRuntimeClean(t, base)
			assertRuntimeClean(t, runtimeBase)
		})
	}
}

func TestLauncherSignalStopsAdapterBeforeServer(t *testing.T) {
	result := startAndSignalLauncher(t, "signal", syscall.SIGTERM)
	if result.exitCode != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", result.exitCode, result.stderr)
	}
	wantEventsInOrder(t, result.events, "adapter-term", "server-term")
}

func TestLauncherSignalBeforeSessionPublicationStopsDirectChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal-before-session-publication")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, eventPath, "pre-session-pid=", 5*time.Second)
	pid := recordedEventPID(t, readEvents(t, eventPath), "pre-session-pid=")
	disarmCleanup := cleanupRecordedPIDs(t, []int{pid})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", code, stderr.String())
	}
	waitForProcessExit(t, pid, 5*time.Second)
	disarmCleanup()
	if strings.Contains(readEvents(t, eventPath), "adapter-start") {
		t.Fatal("adapter started after pre-session termination")
	}
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherRejectsWrongSessionMarkerWithoutHanging(t *testing.T) {
	result := runLauncher(t, "wrong-session-marker")
	pid := recordedEventPID(t, result.events, "pre-session-pid=")
	disarmCleanup := cleanupRecordedPIDs(t, []int{pid})
	if result.exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr:\n%s", result.exitCode, result.stderr)
	}
	for _, want := range []string{
		"app-server published process session",
		"app-server process-session cleanup left its direct child running; killing it",
	} {
		if !strings.Contains(result.stderr, want) {
			t.Fatalf("missing %q; stderr:\n%s", want, result.stderr)
		}
	}
	waitForProcessExit(t, pid, 5*time.Second)
	disarmCleanup()
	if strings.Contains(result.events, "adapter-start") {
		t.Fatal("adapter started after invalid process-session marker")
	}
}

func TestLauncherDisablesInheritedJobControlBeforeSetsid(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "adapter-first")
	cmd.Args = append([]string{cmd.Args[0], "-m"}, cmd.Args[1:]...)
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 17 {
		t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", code, stderr.String())
	}
	wantEventsInOrder(t, readEvents(t, eventPath), "server-start", "adapter-start", "server-term")
	assertRuntimeClean(t, runtimeDir)
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

func TestLauncherForcedServerKillStopsDescendantTree(t *testing.T) {
	result := startAndSignalLauncher(t, "server-descendants-ignore-term", syscall.SIGTERM)
	pids := recordedServerTreePIDs(t, result.events)
	disarmCleanup := cleanupRecordedPIDs(t, pids)

	if result.exitCode != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", result.exitCode, result.stderr)
	}
	if !strings.Contains(result.stderr, "app-server did not stop; killing it") {
		t.Fatalf("missing forced-kill diagnostic; stderr:\n%s", result.stderr)
	}
	wantEventsInOrder(t, result.events, "server-tree-pids=", "adapter-start", "adapter-term")
	for _, pid := range pids {
		waitForProcessExit(t, pid, 5*time.Second)
	}
	disarmCleanup()
}

func TestLauncherStopsDescendantsAfterServerWrapperExits(t *testing.T) {
	result := startAndSignalLauncher(t, "server-wrapper-exits-descendants-ignore-term", syscall.SIGTERM)
	pids := recordedServerTreePIDs(t, result.events)
	disarmCleanup := cleanupRecordedPIDs(t, pids)

	if result.exitCode != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", result.exitCode, result.stderr)
	}
	if strings.Contains(result.stderr, "app-server did not stop; killing it") {
		t.Fatalf("wrapper that handled TERM was reported stuck; stderr:\n%s", result.stderr)
	}
	if !strings.Contains(result.stderr, "app-server descendants did not stop; killing them") {
		t.Fatalf("missing descendant forced-kill diagnostic; stderr:\n%s", result.stderr)
	}
	wantEventsInOrder(t, result.events, "server-tree-pids=", "adapter-start", "adapter-term", "server-wrapper-term")
	for _, pid := range pids {
		waitForProcessExit(t, pid, 5*time.Second)
	}
	disarmCleanup()
}

func TestLauncherStopsDescendantsAfterUnexpectedServerWrapperExit(t *testing.T) {
	result := runLauncher(t, "server-wrapper-exits-unexpectedly")
	pids := recordedServerTreePIDs(t, result.events)
	disarmCleanup := cleanupRecordedPIDs(t, pids)

	if result.exitCode != 31 {
		t.Fatalf("exit code = %d, want wrapper code 31; stderr:\n%s", result.exitCode, result.stderr)
	}
	if !strings.Contains(result.stderr, "app-server descendants did not stop; killing them") {
		t.Fatalf("missing orphan-descendant forced-kill diagnostic; stderr:\n%s", result.stderr)
	}
	wantEventsInOrder(t, result.events, "server-tree-pids=", "adapter-start", "server-wrapper-exit", "adapter-term")
	for _, pid := range pids {
		waitForProcessExit(t, pid, 5*time.Second)
	}
	disarmCleanup()
}

func TestLauncherStopsLateServerDescendant(t *testing.T) {
	result := startAndSignalLauncher(t, "server-wrapper-exits-late-descendant", syscall.SIGTERM)
	pids := append(recordedServerTreePIDs(t, result.events), recordedLateServerPID(t, result.events))
	disarmCleanup := cleanupRecordedPIDs(t, pids)

	if result.exitCode != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", result.exitCode, result.stderr)
	}
	if strings.Contains(result.stderr, "app-server did not stop; killing it") {
		t.Fatalf("wrapper that handled TERM was reported stuck; stderr:\n%s", result.stderr)
	}
	if !strings.Contains(result.stderr, "app-server descendants did not stop; killing them") {
		t.Fatalf("missing late-descendant forced-kill diagnostic; stderr:\n%s", result.stderr)
	}
	wantEventsInOrder(t, result.events, "server-wrapper-term", "server-late-worker-pid=")
	for _, pid := range pids {
		waitForProcessExit(t, pid, 5*time.Second)
	}
	disarmCleanup()
}

func TestLauncherStopsSessionAfterLeaderExitsBeforeReadiness(t *testing.T) {
	result := runLauncher(t, "server-exit-before-ready-descendant")
	pids := recordedServerTreePIDs(t, result.events)
	disarmCleanup := cleanupRecordedPIDs(t, pids)

	if result.exitCode != 33 {
		t.Fatalf("exit code = %d, want leader code 33; stderr:\n%s", result.exitCode, result.stderr)
	}
	if strings.Contains(result.events, "adapter-start") {
		t.Fatalf("adapter started after app-server leader exited before readiness:\n%s", result.events)
	}
	if !strings.Contains(result.stderr, "app-server descendants did not stop; killing them") {
		t.Fatalf("missing descendant cleanup diagnostic; stderr:\n%s", result.stderr)
	}
	wantEventsInOrder(t, result.events, "server-tree-pids=", "server-wrapper-exit-before-ready")
	for _, pid := range pids {
		waitForProcessExit(t, pid, 5*time.Second)
	}
	disarmCleanup()
}

func TestLauncherSecondSignalDoesNotAbortSessionCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "server-descendants-ignore-term-second-signal")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, eventPath, "adapter-start", 5*time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, eventPath, "cleanup-kill-phase", 5*time.Second)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 143 {
		t.Fatalf("exit code = %d, want 143; stderr:\n%s", code, stderr.String())
	}
	events := readEvents(t, eventPath)
	pids := recordedServerTreePIDs(t, events)
	disarmCleanup := cleanupRecordedPIDs(t, pids)
	for _, pid := range pids {
		waitForProcessExit(t, pid, 5*time.Second)
	}
	disarmCleanup()
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherReportsSessionCleanupFailureAndOverridesSuccess(t *testing.T) {
	result := runLauncher(t, "server-cleanup-failure")
	pids := recordedServerTreePIDs(t, result.events)
	disarmCleanup := cleanupRecordedPIDs(t, pids)
	if result.exitCode != 1 {
		t.Fatalf("exit code = %d, want cleanup failure status 1; stderr:\n%s", result.exitCode, result.stderr)
	}
	if !strings.Contains(result.stderr, "simulated app-server process-session cleanup failure") {
		t.Fatalf("missing cleanup failure diagnostic; stderr:\n%s", result.stderr)
	}
	for _, pid := range pids {
		waitForProcessExit(t, pid, 5*time.Second)
	}
	disarmCleanup()
}

func TestLauncherReportsRuntimeDirectoryRemovalFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeBase := launcherCommand(t, ctx, "runtime-removal-failure")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	cleanupArmed := true
	t.Cleanup(func() {
		if cleanupArmed {
			cleanupRecordedTestSession(eventPath)
		}
	})
	err := cmd.Wait()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 1 {
		t.Fatalf("exit code = %d, want cleanup failure status 1; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "could not remove runtime directory") {
		t.Fatalf("missing runtime removal diagnostic; stderr:\n%s", stderr.String())
	}
	assertTestSessionStopped(t, eventPath)
	cleanupArmed = false
	entries, readErr := os.ReadDir(runtimeBase)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("runtime base entries = %v, want failed service directory", entries)
	}
	failedRuntime := filepath.Join(runtimeBase, entries[0].Name())
	if chmodErr := os.Chmod(failedRuntime, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if removeErr := os.RemoveAll(failedRuntime); removeErr != nil {
		t.Fatal(removeErr)
	}
	assertRuntimeClean(t, runtimeBase)
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
		{name: "duration overflow", value: "9223372037"},
		{name: "arithmetic overflow", value: "922337203685477581"},
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

func TestLauncherAcceptsMaximumDurationTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "server-exit-before-ready")
	replaceEnv(cmd, "INTERCOM_CODEX_STARTUP_TIMEOUT_SECONDS", "9223372036")
	replaceEnv(cmd, "INTERCOM_CODEX_SHUTDOWN_TIMEOUT_SECONDS", "9223372036")
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("launcher timed out; stderr:\n%s", stderr.String())
	}
	if code := processExitCode(err); code != 19 {
		t.Fatalf("exit code = %d, want app-server code 19; stderr:\n%s", code, stderr.String())
	}
	wantEventsInOrder(t, readEvents(t, eventPath), "server-exit-before-ready")
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherRejectsEndpointOverridesBeforeStartingChildren(t *testing.T) {
	for _, tt := range []struct {
		option string
		args   []string
	}{
		{option: "--app-server", args: []string{"--app-server", "unix:///tmp/other.sock"}},
		{option: "--app-server", args: []string{"--app-server=unix:///tmp/other.sock"}},
		{option: "--app-server", args: []string{"--help", "--app-server", "unix:///tmp/other.sock"}},
		{option: "--app-server", args: []string{"--app-server", "unix:///tmp/other.sock", "--help"}},
		{option: "--app-server", args: []string{"--help", "--app-server=unix:///tmp/other.sock"}},
		{option: "--client-endpoint", args: []string{"--client-endpoint", "unix:///tmp/other.sock"}},
		{option: "--client-endpoint", args: []string{"--client-endpoint=unix:///tmp/other.sock"}},
		{option: "--client-endpoint", args: []string{"--help", "--client-endpoint", "unix:///tmp/other.sock"}},
		{option: "--client-endpoint", args: []string{"--client-endpoint", "unix:///tmp/other.sock", "--help"}},
		{option: "--client-endpoint", args: []string{"--help", "--client-endpoint=unix:///tmp/other.sock"}},
		{option: "--mcp-bridge", args: []string{"--mcp-bridge", "/tmp/other.sock"}},
		{option: "--mcp-bridge", args: []string{"--mcp-bridge=/tmp/other.sock"}},
		{option: "--mcp-bridge", args: []string{"--help", "--mcp-bridge=/tmp/other.sock"}},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal", tt.args...)
		err := cmd.Run()
		cancel()
		if code := processExitCode(err); code != 2 {
			t.Fatalf("args %v exit code = %d, want 2; stderr:\n%s", tt.args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), tt.option+" is managed by the launcher") {
			t.Fatalf("args %v missing override error; stderr:\n%s", tt.args, stderr.String())
		}
		if events := readEvents(t, eventPath); events != "" {
			t.Fatalf("args %v started children:\n%s", tt.args, events)
		}
		assertRuntimeClean(t, runtimeDir)
	}
}

func TestLauncherRejectsInternalSessionFlagsBeforeStartingChildren(t *testing.T) {
	for _, args := range [][]string{
		{"--adopt-session", "019f-internal"},
		{"--adopt-session=019f-internal"},
		{"--fork-session", "019f-internal"},
		{"--fork-session=019f-internal", "--help"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal", args...)
		err := cmd.Run()
		cancel()
		if code := processExitCode(err); code != 2 {
			t.Fatalf("args %v exit code = %d, want 2; stderr:\n%s", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "is internal; use --adopt or --fork-from") {
			t.Fatalf("args %v missing internal-option error; stderr:\n%s", args, stderr.String())
		}
		if events := readEvents(t, eventPath); events != "" {
			t.Fatalf("args %v started children:\n%s", args, events)
		}
		assertRuntimeClean(t, runtimeDir)
	}
}

func TestLauncherForwardsExplicitSessionSelectionExactly(t *testing.T) {
	for _, tt := range []struct {
		name     string
		args     []string
		wantFlag string
		wantID   string
	}{
		{
			name:     "adopt separate argument",
			args:     []string{"--adopt", "019F-Adopt=Exact.Case", "--replace-binding", "--name", "reviewer"},
			wantFlag: "--adopt-session",
			wantID:   "019F-Adopt=Exact.Case",
		},
		{
			name:     "fork equals argument",
			args:     []string{"--fork-from=019F-Fork=Exact.Case", "--name", "reviewer"},
			wantFlag: "--fork-session",
			wantID:   "019F-Fork=Exact.Case",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := runLauncher(t, "adapter-first", tt.args...)
			if result.exitCode != 17 {
				t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", result.exitCode, result.stderr)
			}
			if strings.Contains(result.events, "sessions-start") {
				t.Fatalf("explicit session id invoked selector:\n%s", result.events)
			}
			adapterArgs := recordedAdapterArgs(t, result.events)
			if got := requiredFlagValue(t, adapterArgs, tt.wantFlag); got != tt.wantID {
				t.Fatalf("%s value = %q, want exact %q", tt.wantFlag, got, tt.wantID)
			}
			if !filepath.IsAbs(requiredFlagValue(t, adapterArgs, "--mcp-bridge")) {
				t.Fatalf("managed MCP bridge path is not absolute: %q", adapterArgs)
			}
		})
	}
}

func TestLauncherSelectsAdoptionSessionBeforeAdapter(t *testing.T) {
	result := runLauncher(t, "picker-adopt", "--adopt", "--cwd", "/tmp/project", "--name", "reviewer")
	if result.exitCode != 17 {
		t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", result.exitCode, result.stderr)
	}
	wantEventsInOrder(t, result.events, "server-start", "sessions-start", "adapter-start", "server-term")
	sessionArgs := recordedSessionsArgs(t, result.events)
	if got := requiredFlagValue(t, sessionArgs, "--cwd"); got != "/tmp/project" {
		t.Fatalf("selector cwd = %q, want /tmp/project", got)
	}
	if containsArg(sessionArgs, "--all") || containsArg(sessionArgs, "--list") {
		t.Fatalf("adoption selector received unexpected mode: %q", sessionArgs)
	}
	adapterArgs := recordedAdapterArgs(t, result.events)
	if got := requiredFlagValue(t, adapterArgs, "--adopt-session"); got != "019f-adopt-Exact" {
		t.Fatalf("selected adoption id = %q", got)
	}
	if got, want := requiredFlagValue(t, sessionArgs, "--app-server"), requiredFlagValue(t, adapterArgs, "--app-server"); got != want {
		t.Fatalf("selector app-server = %q, adapter app-server = %q", got, want)
	}
}

func TestLauncherSelectsForkSessionAcrossWorkingDirectories(t *testing.T) {
	result := runLauncher(t, "picker-fork", "--fork-from", "--all-sessions", "--cwd=/tmp/project")
	if result.exitCode != 17 {
		t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", result.exitCode, result.stderr)
	}
	wantEventsInOrder(t, result.events, "sessions-start", "adapter-start")
	sessionArgs := recordedSessionsArgs(t, result.events)
	if !containsArg(sessionArgs, "--all") {
		t.Fatalf("fork selector did not receive --all: %q", sessionArgs)
	}
	if got := requiredFlagValue(t, sessionArgs, "--cwd"); got != "/tmp/project" {
		t.Fatalf("selector cwd = %q, want /tmp/project", got)
	}
	adapterArgs := recordedAdapterArgs(t, result.events)
	if got := requiredFlagValue(t, adapterArgs, "--fork-session"); got != "019f-fork-Exact" {
		t.Fatalf("selected fork id = %q", got)
	}
}

func TestLauncherListsSessionsWithoutStartingAdapter(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "list-sessions", "--list-sessions", "--all-sessions", "--cwd", "/tmp/project")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	if code := processExitCode(err); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	events := readEvents(t, eventPath)
	wantEventsInOrder(t, events, "server-start", "sessions-start", "server-term")
	if strings.Contains(events, "adapter-start") {
		t.Fatalf("adapter started in list mode:\n%s", events)
	}
	if got, want := stdout.String(), "019f-list\t2026-07-14T00:00:00Z\t/tmp/project\tExample\n"; got != want {
		t.Fatalf("list output = %q, want %q", got, want)
	}
	sessionArgs := recordedSessionsArgs(t, events)
	if !containsArg(sessionArgs, "--all") || !containsArg(sessionArgs, "--list") {
		t.Fatalf("list selector arguments = %q", sessionArgs)
	}
	assertRuntimeClean(t, runtimeDir)
}

func TestLauncherListSessionsRejectsAdapterArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--list-sessions", "--name", "ignored"},
		{"--list-sessions", "--yolo"},
		{"--list-sessions", "positional"},
		{"--list-sessions", "--unknown"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal", args...)
		err := cmd.Run()
		cancel()
		if code := processExitCode(err); code != 2 {
			t.Fatalf("args %v exit code = %d, want 2; stderr:\n%s", args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), "--list-sessions does not accept adapter argument") {
			t.Fatalf("args %v missing list-mode validation; stderr:\n%s", args, stderr.String())
		}
		if events := readEvents(t, eventPath); events != "" {
			t.Fatalf("args %v started children:\n%s", args, events)
		}
		assertRuntimeClean(t, runtimeDir)
	}
}

func TestLauncherSelectorFailureStopsServerWithoutAdapter(t *testing.T) {
	result := runLauncher(t, "session-failure", "--adopt")
	if result.exitCode != 29 {
		t.Fatalf("exit code = %d, want selector code 29; stderr:\n%s", result.exitCode, result.stderr)
	}
	wantEventsInOrder(t, result.events, "sessions-start", "server-term")
	if strings.Contains(result.events, "adapter-start") {
		t.Fatalf("adapter started after selector failure:\n%s", result.events)
	}
	if !strings.Contains(result.stderr, "selector failed") {
		t.Fatalf("selector diagnostic was not retained; stderr:\n%s", result.stderr)
	}
}

func TestLauncherRejectsInvalidSelectorOutput(t *testing.T) {
	for _, mode := range []string{"selector-empty", "selector-multiline"} {
		t.Run(mode, func(t *testing.T) {
			result := runLauncher(t, mode, "--adopt")
			if result.exitCode != 1 {
				t.Fatalf("exit code = %d, want 1; stderr:\n%s", result.exitCode, result.stderr)
			}
			if !strings.Contains(result.stderr, "session selector returned an invalid session id") {
				t.Fatalf("missing invalid-selector diagnostic; stderr:\n%s", result.stderr)
			}
			wantEventsInOrder(t, result.events, "sessions-start", "server-term")
			if strings.Contains(result.events, "adapter-start") {
				t.Fatalf("adapter started after invalid selector output:\n%s", result.events)
			}
		})
	}
}

func TestLauncherForwardsExecutionPolicyAliases(t *testing.T) {
	for _, flag := range []string{"--yolo", "--dangerously-bypass-approvals-and-sandbox"} {
		t.Run(flag, func(t *testing.T) {
			result := runLauncher(t, "adapter-first", flag)
			if result.exitCode != 17 {
				t.Fatalf("exit code = %d, want adapter code 17; stderr:\n%s", result.exitCode, result.stderr)
			}
			if !containsArg(recordedAdapterArgs(t, result.events), flag) {
				t.Fatalf("execution policy flag %s not forwarded:\n%s", flag, result.events)
			}
		})
	}
}

func TestLauncherRejectsConflictingSessionModesBeforeStartingChildren(t *testing.T) {
	for _, args := range [][]string{
		{"--new", "--adopt", "019f-one"},
		{"--adopt", "019f-one", "--fork-from", "019f-two"},
		{"--list-sessions", "--fork-from", "019f-two"},
		{"--adopt", "019f-one", "--adopt", "019f-two"},
		{"--all-sessions"},
		{"--all-sessions", "--adopt", "019f-one"},
		{"--replace-binding"},
		{"--adopt="},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd, stderr, eventPath, runtimeDir := launcherCommand(t, ctx, "signal", args...)
		err := cmd.Run()
		cancel()
		if code := processExitCode(err); code != 2 {
			t.Fatalf("args %v exit code = %d, want 2; stderr:\n%s", args, code, stderr.String())
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
	for _, want := range []string{"Usage:", "--new", "--adopt [ID]", "--fork-from [ID]", "--list-sessions", "--all-sessions", "--yolo"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("help output does not document %q:\n%s", want, stdout.String())
		}
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
	if err := cmd.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	cleanupArmed := true
	t.Cleanup(func() {
		if cleanupArmed {
			cleanupRecordedTestSession(eventPath)
		}
	})
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
	assertTestSessionStopped(t, eventPath)
	cleanupArmed = false
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
	cleanupArmed := true
	t.Cleanup(func() {
		if cleanupArmed {
			cleanupRecordedTestSession(eventPath)
		}
	})
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
	assertTestSessionStopped(t, eventPath)
	cleanupArmed = false
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
	case "codex-app-server-exec":
		return runSessionExecHelper(args)
	case "codex-process-session-cleanup":
		return runProcessSessionCleanupHelper(args)
	case "launcher-native-server":
		return runNativeServerHelper(args)
	case "launcher-server-worker":
		return runServerWorkerHelper()
	case "codex":
		if len(args) > 1 && args[1] == "sessions" {
			return runSessionsHelper(args)
		}
		return runAdapterHelper(args)
	default:
		return 91
	}
}

func runSessionExecHelper(args []string) int {
	if len(args) < 6 || args[1] != "--ready-file" || args[2] == "" || args[3] != "--" {
		return 103
	}
	mode := os.Getenv(helperMode)
	if mode == "signal-before-session-publication" || mode == "wrong-session-marker" {
		recordEvent(fmt.Sprintf("pre-session-pid=%d", os.Getpid()))
		if mode == "wrong-session-marker" {
			if err := os.WriteFile(args[2], []byte(fmt.Sprintf("%d\n", os.Getpid()+1)), 0o600); err != nil {
				return 108
			}
		}
		waitForSignal()
		return 0
	}
	path, err := exec.LookPath(args[4])
	if err != nil {
		recordEvent("session-exec-lookup-error=" + err.Error())
		return 104
	}
	if _, err := syscall.Setsid(); err != nil {
		recordEvent("session-exec-setsid-error=" + err.Error())
		return 105
	}
	recordEvent(fmt.Sprintf("server-session-pid=%d", os.Getpid()))
	readyTemp := fmt.Sprintf("%s.%d.tmp", args[2], os.Getpid())
	if err := os.WriteFile(readyTemp, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		recordEvent("session-exec-ready-error=" + err.Error())
		return 108
	}
	if err := os.Rename(readyTemp, args[2]); err != nil {
		recordEvent("session-exec-ready-error=" + err.Error())
		return 108
	}
	if err := syscall.Exec(path, args[4:], os.Environ()); err != nil {
		recordEvent("session-exec-error=" + err.Error())
		return 106
	}
	return 0
}

func runProcessSessionCleanupHelper(args []string) int {
	sidText, sidOK := flagValue(args, "--sid")
	leaderText, leaderOK := flagValue(args, "--leader")
	timeoutText, timeoutOK := flagValue(args, "--timeout")
	sid, sidErr := strconv.Atoi(sidText)
	leader, leaderErr := strconv.Atoi(leaderText)
	timeout, timeoutErr := time.ParseDuration(timeoutText)
	if !sidOK || !leaderOK || !timeoutOK || sidErr != nil || leaderErr != nil || sid <= 0 || leader != sid || timeoutErr != nil || timeout <= 0 {
		return 109
	}
	mode := os.Getenv(helperMode)
	finish := func() int {
		if mode == "server-cleanup-failure" {
			_, _ = fmt.Fprintln(os.Stderr, "intercom: simulated app-server process-session cleanup failure")
			return 110
		}
		return 0
	}
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	termDeadline := time.Now().Add(timeout)
	var members []int
	for {
		members = helperSessionMembers(sid)
		if len(members) == 0 {
			return finish()
		}
		helperSignalSessionMembers(members, leader, syscall.SIGTERM)
		if !time.Now().Before(termDeadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if slices.Contains(members, leader) {
		_, _ = fmt.Fprintln(os.Stderr, "intercom-codex-project: app-server did not stop; killing it")
	} else {
		_, _ = fmt.Fprintln(os.Stderr, "intercom-codex-project: app-server descendants did not stop; killing them")
	}
	if mode == "server-descendants-ignore-term-second-signal" {
		recordEvent("cleanup-kill-phase")
		time.Sleep(250 * time.Millisecond)
	}

	killDeadline := time.Now().Add(timeout)
	for {
		members = helperSessionMembers(sid)
		if len(members) == 0 {
			return finish()
		}
		helperSignalSessionMembers(members, leader, syscall.SIGKILL)
		if !time.Now().Before(killDeadline) {
			return 110
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func helperSessionMembers(sid int) []int {
	output, err := exec.Command("ps", "-A", "-o", "pid=", "-o", "stat=").Output()
	if err != nil {
		return []int{sid}
	}
	members := make([]int, 0)
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.HasPrefix(fields[1], "Z") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		got, err := testGetSID(pid)
		if err == nil && got == sid {
			members = append(members, pid)
		}
	}
	return members
}

func helperSignalSessionMembers(members []int, leader int, signal syscall.Signal) {
	for _, pid := range members {
		if pid != leader {
			if sid, err := testGetSID(pid); err == nil && sid == leader {
				_ = syscall.Kill(pid, signal)
			}
		}
	}
	if slices.Contains(members, leader) {
		if sid, err := testGetSID(leader); err == nil && sid == leader {
			_ = syscall.Kill(leader, signal)
		}
	}
}

func runSessionsHelper(args []string) int {
	mode := os.Getenv(helperMode)
	recordEvent("sessions-start")
	recordEvent("sessions-args=" + strings.Join(args, "\x1f"))
	switch mode {
	case "picker-adopt":
		_, _ = fmt.Fprintln(os.Stdout, "019f-adopt-Exact")
		return 0
	case "picker-fork":
		_, _ = fmt.Fprintln(os.Stdout, "019f-fork-Exact")
		return 0
	case "list-sessions":
		_, _ = fmt.Fprintln(os.Stdout, "019f-list\t2026-07-14T00:00:00Z\t/tmp/project\tExample")
		return 0
	case "session-failure":
		_, _ = fmt.Fprintln(os.Stderr, "selector failed")
		return 29
	case "selector-empty":
		return 0
	case "selector-multiline":
		_, _ = fmt.Fprintln(os.Stdout, "019f-one")
		_, _ = fmt.Fprintln(os.Stdout, "019f-two")
		return 0
	default:
		return 97
	}
}

func runServerHelper(args []string) int {
	mode := os.Getenv(helperMode)
	if mode == "server-descendants-ignore-term" || mode == "server-wrapper-exits-descendants-ignore-term" ||
		mode == "server-wrapper-exits-unexpectedly" || mode == "server-wrapper-exits-late-descendant" ||
		mode == "server-exit-before-ready-descendant" || mode == "server-descendants-ignore-term-second-signal" ||
		mode == "server-cleanup-failure" {
		return runServerWrapperHelper(args)
	}
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
	if mode == "stream-routing" {
		_, _ = fmt.Fprintln(os.Stdout, "app-server-stdout")
	}
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

func runServerWrapperHelper(args []string) int {
	endpoint, ok := flagValue(args, "--listen")
	if !ok || !strings.HasPrefix(endpoint, "unix://") {
		return 92
	}
	var signals chan os.Signal
	mode := os.Getenv(helperMode)
	if mode == "server-wrapper-exits-descendants-ignore-term" || mode == "server-wrapper-exits-late-descendant" {
		signals = make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		defer signal.Stop(signals)
	}
	child := exec.Command(os.Args[0], "launcher-native-server", endpoint)
	child.Env = os.Environ()
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		recordEvent("server-native-start-error=" + err.Error())
		return 98
	}
	go func() { _ = child.Wait() }()
	if mode == "server-exit-before-ready-descendant" {
		if !helperWaitForEvent("server-tree-pids=", 5*time.Second) {
			return 111
		}
		recordEvent("server-wrapper-exit-before-ready")
		return 33
	}
	if mode == "server-wrapper-exits-unexpectedly" {
		if !helperWaitForEvent("adapter-start", 5*time.Second) {
			return 102
		}
		recordEvent("server-wrapper-exit")
		return 31
	}
	if signals != nil {
		<-signals
		recordEvent("server-wrapper-term")
		return 0
	}
	signal.Ignore(syscall.SIGTERM)
	for {
		time.Sleep(time.Hour)
	}
}

func runNativeServerHelper(args []string) int {
	if len(args) != 2 || !strings.HasPrefix(args[1], "unix://") {
		return 99
	}
	worker := exec.Command(os.Args[0], "launcher-server-worker")
	worker.Env = os.Environ()
	worker.Stdout = os.Stdout
	worker.Stderr = os.Stderr
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := worker.Start(); err != nil {
		recordEvent("server-worker-start-error=" + err.Error())
		return 100
	}
	go func() { _ = worker.Wait() }()
	var listener net.Listener
	if os.Getenv(helperMode) != "server-exit-before-ready-descendant" {
		var err error
		listener, err = net.Listen("unix", strings.TrimPrefix(args[1], "unix://"))
		if err != nil {
			recordEvent("server-listen-error=" + err.Error())
			return 101
		}
		defer listener.Close()
	}
	recordEvent(fmt.Sprintf("server-tree-pids=%d,%d,%d", os.Getppid(), os.Getpid(), worker.Process.Pid))
	if os.Getenv(helperMode) == "server-wrapper-exits-late-descendant" {
		go func() {
			if !helperWaitForEvent("server-wrapper-term", 5*time.Second) {
				return
			}
			late := exec.Command(os.Args[0], "launcher-server-worker")
			late.Env = os.Environ()
			late.Stdout = os.Stdout
			late.Stderr = os.Stderr
			late.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			if err := late.Start(); err != nil {
				recordEvent("server-late-worker-start-error=" + err.Error())
				return
			}
			recordEvent(fmt.Sprintf("server-late-worker-pid=%d", late.Process.Pid))
			_ = late.Wait()
		}()
	}
	signal.Ignore(syscall.SIGTERM)
	for {
		time.Sleep(time.Hour)
	}
}

func runServerWorkerHelper() int {
	signal.Ignore(syscall.SIGTERM)
	for {
		time.Sleep(time.Hour)
	}
}

func runAdapterHelper(args []string) int {
	mode := os.Getenv(helperMode)
	if mode == "adapter-ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	recordEvent("adapter-start")
	recordEvent("adapter-args=" + strings.Join(args, "\x1f"))
	if mode == "adapter-first" || mode == "stream-routing" || mode == "picker-adopt" || mode == "picker-fork" ||
		mode == "server-cleanup-failure" || mode == "runtime-removal-failure" {
		if mode == "stream-routing" {
			_, _ = fmt.Fprintln(os.Stdout, "service-ready")
		}
		if mode == "server-cleanup-failure" {
			return 0
		}
		if mode == "runtime-removal-failure" {
			endpoint, ok := flagValue(args, "--app-server")
			if !ok || !strings.HasPrefix(endpoint, "unix://") {
				return 95
			}
			if err := os.Chmod(filepath.Dir(strings.TrimPrefix(endpoint, "unix://")), 0o500); err != nil {
				recordEvent("runtime-chmod-error=" + err.Error())
				return 96
			}
			return 0
		}
		return 17
	}
	if mode == "client-disconnect" {
		endpoint, ok := flagValue(args, "--client-endpoint")
		if !ok || !strings.HasPrefix(endpoint, "unix://") {
			return 95
		}
		listener, err := net.Listen("unix", strings.TrimPrefix(endpoint, "unix://"))
		if err != nil {
			recordEvent("client-listen-error=" + err.Error())
			return 96
		}
		recordEvent("client-listen")
		clientDone := make(chan struct{})
		go func() {
			defer close(clientDone)
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			recordEvent("client-connect")
			buffer := make([]byte, 1)
			_, _ = conn.Read(buffer)
			_ = conn.Close()
			recordEvent("client-disconnect")
		}()
		waitForSignal()
		_ = listener.Close()
		<-clientDone
		recordEvent("adapter-term")
		return 0
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

func recordedAdapterArgs(t *testing.T, events string) []string {
	t.Helper()
	for _, line := range strings.Split(events, "\n") {
		if strings.HasPrefix(line, "adapter-args=") {
			return strings.Split(strings.TrimPrefix(line, "adapter-args="), "\x1f")
		}
	}
	t.Fatalf("adapter arguments not found in events:\n%s", events)
	return nil
}

func recordedSessionsArgs(t *testing.T, events string) []string {
	t.Helper()
	for _, line := range strings.Split(events, "\n") {
		if strings.HasPrefix(line, "sessions-args=") {
			return strings.Split(strings.TrimPrefix(line, "sessions-args="), "\x1f")
		}
	}
	t.Fatalf("session-selector arguments not found in events:\n%s", events)
	return nil
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func requiredFlagValue(t *testing.T, args []string, name string) string {
	t.Helper()
	value, ok := flagValue(args, name)
	if !ok {
		t.Fatalf("flag %s not found in arguments: %q", name, args)
	}
	return value
}

func unixEndpointPath(t *testing.T, endpoint string) string {
	t.Helper()
	const prefix = "unix://"
	if !strings.HasPrefix(endpoint, prefix) {
		t.Fatalf("endpoint = %q, want %s prefix", endpoint, prefix)
	}
	path := strings.TrimPrefix(endpoint, prefix)
	if !filepath.IsAbs(path) {
		t.Fatalf("endpoint path = %q, want absolute", path)
	}
	return path
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

func recordedServerTreePIDs(t *testing.T, events string) []int {
	t.Helper()
	for _, line := range strings.Split(events, "\n") {
		if !strings.HasPrefix(line, "server-tree-pids=") {
			continue
		}
		var wrapper, native, worker int
		if _, err := fmt.Sscanf(strings.TrimPrefix(line, "server-tree-pids="), "%d,%d,%d", &wrapper, &native, &worker); err != nil {
			t.Fatalf("parse server tree PIDs from %q: %v", line, err)
		}
		return []int{wrapper, native, worker}
	}
	t.Fatalf("server tree PIDs not found in events:\n%s", events)
	return nil
}

func recordedLateServerPID(t *testing.T, events string) int {
	t.Helper()
	for _, line := range strings.Split(events, "\n") {
		if !strings.HasPrefix(line, "server-late-worker-pid=") {
			continue
		}
		var pid int
		if _, err := fmt.Sscanf(strings.TrimPrefix(line, "server-late-worker-pid="), "%d", &pid); err != nil {
			t.Fatalf("parse late server PID from %q: %v", line, err)
		}
		return pid
	}
	t.Fatalf("late server PID not found in events:\n%s", events)
	return 0
}

func recordedEventPID(t *testing.T, events, prefix string) int {
	t.Helper()
	for _, line := range strings.Split(events, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(line, prefix))
		if err != nil || pid <= 0 {
			t.Fatalf("parse PID from %q: %v", line, err)
		}
		return pid
	}
	t.Fatalf("event %q not found:\n%s", prefix, events)
	return 0
}

func cleanupRecordedPIDs(t *testing.T, pids []int) func() {
	t.Helper()
	armed := true
	t.Cleanup(func() {
		if !armed {
			return
		}
		for index := len(pids) - 1; index >= 0; index-- {
			_ = syscall.Kill(pids[index], syscall.SIGKILL)
		}
	})
	return func() { armed = false }
}

func assertTestSessionStopped(t *testing.T, eventPath string) {
	t.Helper()
	sid, ok := optionalRecordedEventPID(readEvents(t, eventPath), "server-session-pid=")
	if !ok {
		return
	}
	if members := helperSessionMembers(sid); len(members) != 0 {
		cleanupRecordedTestSession(eventPath)
		t.Fatalf("launcher left app-server session %d members running: %v", sid, members)
	}
}

func cleanupRecordedTestSession(eventPath string) {
	data, _ := os.ReadFile(eventPath)
	events := string(data)
	if pid, ok := optionalRecordedEventPID(events, "pre-session-pid="); ok {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	sid, ok := optionalRecordedEventPID(events, "server-session-pid=")
	if ok {
		for range 10 {
			members := helperSessionMembers(sid)
			if len(members) == 0 {
				break
			}
			helperSignalSessionMembers(members, sid, syscall.SIGKILL)
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func optionalRecordedEventPID(events, prefix string) (int, bool) {
	for _, line := range strings.Split(events, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(line, prefix))
		return pid, err == nil && pid > 0
	}
	return 0, false
}

func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d survived launcher shutdown", pid)
}
