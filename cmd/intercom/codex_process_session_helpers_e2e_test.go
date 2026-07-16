//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	codexProcessHelpersE2ERoleEnv    = "INTERCOM_CODEX_PROCESS_HELPERS_E2E_ROLE"
	codexProcessHelpersE2EPIDsEnv    = "INTERCOM_CODEX_PROCESS_HELPERS_E2E_PIDS"
	codexProcessHelpersE2EReadyEnv   = "INTERCOM_CODEX_PROCESS_HELPERS_E2E_READY"
	codexProcessHelpersE2EServer     = "server"
	codexProcessHelpersE2EDescendant = "descendant"
)

func TestCodexProcessSessionHelpersE2E(t *testing.T) {
	switch os.Getenv(codexProcessHelpersE2ERoleEnv) {
	case codexProcessHelpersE2EServer:
		runCodexProcessHelpersE2EServer(t)
		return
	case codexProcessHelpersE2EDescendant:
		waitForCodexProcessHelpersE2ETermination(t)
		return
	}
	if testing.Short() {
		t.Skip("process-level helper test")
	}

	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	intercomBinary := filepath.Join(t.TempDir(), "intercom")
	buildCodexProcessHelpersE2EBinary(t, intercomBinary)

	runtimeDir := t.TempDir()
	readyFile := filepath.Join(runtimeDir, "session.ready")
	pidsFile := filepath.Join(runtimeDir, "session.pids")
	server := exec.Command(
		intercomBinary,
		"codex-app-server-exec",
		"--ready-file", readyFile,
		"--",
		testBinary,
		"-test.run=^TestCodexProcessSessionHelpersE2E$",
	)
	server.Env = replaceCodexProcessHelpersE2EEnv(os.Environ(),
		codexProcessHelpersE2ERoleEnv+"="+codexProcessHelpersE2EServer,
		codexProcessHelpersE2EPIDsEnv+"="+pidsFile,
	)
	var serverOutput bytes.Buffer
	server.Stdout = &serverOutput
	server.Stderr = &serverOutput
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Wait()
	}()

	directChildArmed := true
	sessionArmed := false
	sessionID := 0
	serverReaped := false
	defer func() {
		if sessionArmed {
			forceStopCodexProcessHelpersE2ESession(sessionID)
		} else if directChildArmed && server.Process != nil {
			_ = server.Process.Kill()
		}
		if !serverReaped {
			select {
			case <-serverDone:
			case <-time.After(3 * time.Second):
			}
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	readyPID, err := waitForCodexProcessHelpersE2EPIDFile(readyFile, 1, deadline, serverDone)
	if err != nil {
		t.Fatalf("wait for app-server session marker: %v; output:\n%s", err, serverOutput.String())
	}
	if readyPID[0] != server.Process.Pid {
		t.Fatalf("session marker PID = %d, direct child PID = %d", readyPID[0], server.Process.Pid)
	}
	if readyPID[0] <= 1 || readyPID[0] == os.Getpid() {
		t.Fatalf("unsafe session leader PID %d", readyPID[0])
	}
	actualSID, err := codexProcessGetsid(readyPID[0])
	if err != nil {
		t.Fatalf("inspect session leader %d: %v", readyPID[0], err)
	}
	if actualSID != readyPID[0] {
		t.Fatalf("session leader %d belongs to session %d", readyPID[0], actualSID)
	}
	sessionID = actualSID
	sessionArmed = true
	directChildArmed = false

	pids, err := waitForCodexProcessHelpersE2EPIDFile(pidsFile, 2, deadline, serverDone)
	if err != nil {
		t.Fatalf("wait for app-server stand-in: %v; output:\n%s", err, serverOutput.String())
	}
	leader, descendant := pids[0], pids[1]
	if leader != sessionID {
		t.Fatalf("recorded leader PID = %d, session ID = %d", leader, sessionID)
	}
	for _, pid := range pids {
		sid, err := codexProcessGetsid(pid)
		if err != nil {
			t.Fatalf("inspect recorded PID %d: %v", pid, err)
		}
		if sid != sessionID {
			t.Fatalf("recorded PID %d belongs to session %d, want %d", pid, sid, sessionID)
		}
	}
	leaderPGID, err := syscall.Getpgid(leader)
	if err != nil {
		t.Fatalf("inspect leader process group: %v", err)
	}
	descendantPGID, err := syscall.Getpgid(descendant)
	if err != nil {
		t.Fatalf("inspect descendant process group: %v", err)
	}
	if descendantPGID == leaderPGID {
		t.Fatalf("descendant PID %d remained in leader process group %d", descendant, leaderPGID)
	}

	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelCleanup()
	cleanup := exec.CommandContext(
		cleanupCtx,
		intercomBinary,
		"codex-process-session-cleanup",
		"--sid", strconv.Itoa(sessionID),
		"--leader", strconv.Itoa(leader),
		"--timeout", "2s",
	)
	cleanupOutput, err := cleanup.CombinedOutput()
	if err != nil {
		t.Fatalf("production cleanup helper: %v; output:\n%s", err, cleanupOutput)
	}
	wantCleanupOutput := "intercom-codex-project: app-server did not stop; killing it\n"
	if string(cleanupOutput) != wantCleanupOutput {
		t.Fatalf("production cleanup output = %q, want %q", cleanupOutput, wantCleanupOutput)
	}

	select {
	case err := <-serverDone:
		serverReaped = true
		if !codexProcessHelpersE2EKilled(err) {
			t.Fatalf("app-server stand-in exit = %v, want SIGKILL; output:\n%s", err, serverOutput.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app-server stand-in was not reaped")
	}
	for _, pid := range pids {
		if err := waitForCodexProcessHelpersE2EESRCH(pid, sessionID, time.Now().Add(5*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	sessionArmed = false
}

func runCodexProcessHelpersE2EServer(t *testing.T) {
	pidsFile := os.Getenv(codexProcessHelpersE2EPIDsEnv)
	if !filepath.IsAbs(pidsFile) {
		t.Fatalf("PID record path is not absolute: %q", pidsFile)
	}
	testBinary, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	descendant := exec.Command(testBinary, "-test.run=^TestCodexProcessSessionHelpersE2E$")
	descendantReady := pidsFile + ".descendant-ready"
	descendant.Env = replaceCodexProcessHelpersE2EEnv(os.Environ(),
		codexProcessHelpersE2ERoleEnv+"="+codexProcessHelpersE2EDescendant,
		codexProcessHelpersE2EReadyEnv+"="+descendantReady,
	)
	descendant.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := descendant.Start(); err != nil {
		t.Fatal(err)
	}
	descendantDone := make(chan error, 1)
	go func() {
		descendantDone <- descendant.Wait()
	}()
	defer func() {
		_ = descendant.Process.Kill()
		select {
		case <-descendantDone:
		case <-time.After(3 * time.Second):
		}
	}()
	readyPID, err := waitForCodexProcessHelpersE2EPIDFile(
		descendantReady,
		1,
		time.Now().Add(5*time.Second),
		descendantDone,
	)
	if err != nil {
		t.Fatalf("wait for descendant readiness: %v", err)
	}
	if readyPID[0] != descendant.Process.Pid {
		t.Fatalf("descendant readiness PID = %d, child PID = %d", readyPID[0], descendant.Process.Pid)
	}
	signal.Ignore(syscall.SIGTERM)

	record := []byte(fmt.Sprintf("%d\n%d\n", os.Getpid(), descendant.Process.Pid))
	temporary := pidsFile + ".tmp"
	if err := os.WriteFile(temporary, record, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temporary, pidsFile); err != nil {
		t.Fatal(err)
	}

	select {}
}

func waitForCodexProcessHelpersE2ETermination(t *testing.T) {
	signal.Ignore(syscall.SIGTERM)
	readyFile := os.Getenv(codexProcessHelpersE2EReadyEnv)
	if !filepath.IsAbs(readyFile) {
		t.Fatalf("descendant readiness path is not absolute: %q", readyFile)
	}
	if err := os.WriteFile(readyFile, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {}
}

func codexProcessHelpersE2EKilled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func buildCodexProcessHelpersE2EBinary(t *testing.T, output string) {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate process-helper test source")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", output, ".")
	build.Dir = filepath.Dir(sourceFile)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build intercom: %v; output:\n%s", err, output)
	}
}

func waitForCodexProcessHelpersE2EPIDFile(
	path string,
	want int,
	deadline time.Time,
	processDone <-chan error,
) ([]int, error) {
	for time.Now().Before(deadline) {
		select {
		case err := <-processDone:
			return nil, fmt.Errorf("process exited before publishing %s: %v", filepath.Base(path), err)
		default:
		}
		data, err := os.ReadFile(path)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) != want {
				return nil, fmt.Errorf("%s contains %d PIDs, want %d", filepath.Base(path), len(fields), want)
			}
			pids := make([]int, len(fields))
			for i, field := range fields {
				pid, err := strconv.Atoi(field)
				if err != nil || pid <= 0 {
					return nil, fmt.Errorf("%s contains invalid PID %q", filepath.Base(path), field)
				}
				pids[i] = pid
			}
			return pids, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for %s", filepath.Base(path))
}

func waitForCodexProcessHelpersE2EESRCH(pid int, sid int, deadline time.Time) error {
	for time.Now().Before(deadline) {
		actual, err := codexProcessGetsid(pid)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect terminated PID %d: %w", pid, err)
		}
		if actual != sid {
			return fmt.Errorf("PID %d was reused by session %d before ESRCH was observed", pid, actual)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("PID %d in session %d did not reach ESRCH", pid, sid)
}

func forceStopCodexProcessHelpersE2ESession(sid int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		processes, err := listCodexProcesses(context.Background())
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		members := make([]int, 0)
		for _, process := range processes {
			if process.stat == "" || strings.HasPrefix(process.stat, "Z") {
				continue
			}
			actual, err := codexProcessGetsid(process.pid)
			if err == nil && actual == sid {
				members = append(members, process.pid)
			}
		}
		if len(members) == 0 {
			return
		}
		sort.Ints(members)
		for _, pid := range members {
			if pid == sid {
				continue
			}
			killCodexProcessHelpersE2EMember(pid, sid)
		}
		killCodexProcessHelpersE2EMember(sid, sid)
		time.Sleep(10 * time.Millisecond)
	}
}

func killCodexProcessHelpersE2EMember(pid int, sid int) {
	actual, err := codexProcessGetsid(pid)
	if err != nil || actual != sid {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

func replaceCodexProcessHelpersE2EEnv(environment []string, values ...string) []string {
	keys := make(map[string]struct{}, len(values))
	for _, value := range values {
		key, _, _ := strings.Cut(value, "=")
		keys[key] = struct{}{}
	}
	result := make([]string, 0, len(environment)+len(values))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := keys[key]; !replaced {
			result = append(result, entry)
		}
	}
	return append(result, values...)
}
