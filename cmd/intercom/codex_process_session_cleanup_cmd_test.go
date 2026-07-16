//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type codexProcessCleanupHarness struct {
	clock  time.Duration
	sleeps []time.Duration

	processes     map[int]codexProcess
	sids          map[int]int
	getSIDErr     map[int]error
	getsidFunc    func(int) (int, error)
	processesFunc func(context.Context) ([]codexProcess, error)

	enumerationErrors []error
	enumerationCalls  int

	signals    []codexProcessCleanupSignal
	signalFunc func(int, syscall.Signal) error
	ignored    []os.Signal
}

type codexProcessCleanupSignal struct {
	pid int
	sig syscall.Signal
}

func newCodexProcessCleanupHarness(uid int, sid int, pids ...int) *codexProcessCleanupHarness {
	_ = uid
	h := &codexProcessCleanupHarness{
		processes: make(map[int]codexProcess),
		sids:      make(map[int]int),
		getSIDErr: make(map[int]error),
	}
	for _, pid := range pids {
		h.processes[pid] = codexProcess{pid: pid, stat: "S"}
		h.sids[pid] = sid
	}
	return h
}

func (h *codexProcessCleanupHarness) ops() codexProcessSessionCleanupOps {
	return codexProcessSessionCleanupOps{
		processes: func(ctx context.Context) ([]codexProcess, error) {
			if h.processesFunc != nil {
				return h.processesFunc(ctx)
			}
			call := h.enumerationCalls
			h.enumerationCalls++
			if call < len(h.enumerationErrors) && h.enumerationErrors[call] != nil {
				return nil, h.enumerationErrors[call]
			}
			result := make([]codexProcess, 0, len(h.processes))
			for _, process := range h.processes {
				result = append(result, process)
			}
			return result, nil
		},
		getsid: func(pid int) (int, error) {
			if h.getsidFunc != nil {
				return h.getsidFunc(pid)
			}
			if err := h.getSIDErr[pid]; err != nil {
				return 0, err
			}
			sid, ok := h.sids[pid]
			if !ok {
				return 0, syscall.ESRCH
			}
			return sid, nil
		},
		kill: func(pid int, sig syscall.Signal) error {
			h.signals = append(h.signals, codexProcessCleanupSignal{pid: pid, sig: sig})
			if h.signalFunc != nil {
				return h.signalFunc(pid, sig)
			}
			delete(h.processes, pid)
			delete(h.sids, pid)
			return nil
		},
		monotonicNow: func() time.Duration { return h.clock },
		sleep: func(_ context.Context, duration time.Duration) error {
			h.sleeps = append(h.sleeps, duration)
			h.clock += duration
			return nil
		},
		ignoreSignals: func(signals ...os.Signal) {
			h.ignored = append([]os.Signal(nil), signals...)
		},
	}
}

func runCodexProcessCleanupCommand(
	h *codexProcessCleanupHarness,
	sid int,
	timeout string,
) (string, error) {
	cmd := newCodexProcessSessionCleanupCmdWithOps(h.ops())
	cmd.SetArgs([]string{"--sid", strconv.Itoa(sid), "--leader", strconv.Itoa(sid), "--timeout", timeout})
	cmd.SetOut(io.Discard)
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return stderr.String(), err
}

func TestCodexProcessSessionCleanupCmdValidatesArgumentsBeforeIgnoringSignals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing sid", want: "--sid must be a positive process ID"},
		{name: "negative sid", args: []string{"--sid", "-1"}, want: "--sid must be a positive process ID"},
		{name: "missing leader", args: []string{"--sid", "10"}, want: "--leader must be a positive process ID"},
		{name: "mismatched leader", args: []string{"--sid", "10", "--leader", "11", "--timeout", "1s"}, want: "--leader must equal --sid"},
		{name: "missing timeout", args: []string{"--sid", "10", "--leader", "10"}, want: "--timeout must be a positive duration"},
		{name: "negative timeout", args: []string{"--sid", "10", "--leader", "10", "--timeout", "-1s"}, want: "--timeout must be a positive duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ignored := false
			ops := codexProcessSessionCleanupOps{ignoreSignals: func(...os.Signal) { ignored = true }}
			cmd := newCodexProcessSessionCleanupCmdWithOps(ops)
			cmd.SetArgs(tt.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			err := cmd.Execute()
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if ignored {
				t.Fatal("terminal signals ignored for invalid invocation")
			}
		})
	}
}

func TestCodexProcessSessionCleanupCmdSignalsEveryNonZombieMemberLeaderLast(t *testing.T) {
	const (
		uid = 1000
		sid = 40
	)
	h := newCodexProcessCleanupHarness(uid, sid, sid, 41)
	h.processes[42] = codexProcess{pid: 42, stat: "S"}
	h.sids[42] = sid
	h.processes[43] = codexProcess{pid: 43, stat: "Z+"}
	h.sids[43] = sid
	h.processes[44] = codexProcess{pid: 44, stat: "S"}
	h.sids[44] = 44

	stderr, err := runCodexProcessCleanupCommand(h, sid, "1s")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	wantSignals := []codexProcessCleanupSignal{
		{pid: 41, sig: syscall.SIGTERM},
		{pid: 42, sig: syscall.SIGTERM},
		{pid: sid, sig: syscall.SIGTERM},
	}
	if !reflect.DeepEqual(h.signals, wantSignals) {
		t.Fatalf("signals = %#v, want %#v", h.signals, wantSignals)
	}
	wantIgnored := []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
	if !reflect.DeepEqual(h.ignored, wantIgnored) {
		t.Fatalf("ignored signals = %v, want %v", h.ignored, wantIgnored)
	}
}

func TestCodexProcessSessionCleanupCmdReverifiesSessionImmediatelyBeforeSignal(t *testing.T) {
	h := newCodexProcessCleanupHarness(1000, 45, 45, 46)
	childChecks := 0
	h.getsidFunc = func(pid int) (int, error) {
		if pid == 46 {
			childChecks++
			if childChecks == 1 {
				return 45, nil
			}
			h.sids[pid] = 46
			return 46, nil
		}
		sid, ok := h.sids[pid]
		if !ok {
			return 0, syscall.ESRCH
		}
		return sid, nil
	}

	stderr, err := runCodexProcessCleanupCommand(h, 45, "1s")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	for _, call := range h.signals {
		if call.pid == 46 {
			t.Fatalf("reassigned process received %s", codexSignalName(call.sig))
		}
	}
	if childChecks < 2 {
		t.Fatalf("child getsid checks = %d, want enumeration and immediate recheck", childChecks)
	}
}

func TestCodexProcessSessionCleanupCmdClassifiesTermTimeout(t *testing.T) {
	tests := []struct {
		name       string
		removeTerm func(*codexProcessCleanupHarness, int)
		warning    string
	}{
		{
			name: "leader present",
			removeTerm: func(_ *codexProcessCleanupHarness, _ int) {
			},
			warning: "intercom-codex-project: app-server did not stop; killing it\n",
		},
		{
			name: "only descendant present",
			removeTerm: func(h *codexProcessCleanupHarness, pid int) {
				if pid == 50 {
					delete(h.processes, pid)
					delete(h.sids, pid)
				}
			},
			warning: "intercom-codex-project: app-server descendants did not stop; killing them\n",
		},
		{
			name: "zombie leader",
			removeTerm: func(h *codexProcessCleanupHarness, pid int) {
				if pid == 50 {
					process := h.processes[pid]
					process.stat = "Z+"
					h.processes[pid] = process
				}
			},
			warning: "intercom-codex-project: app-server descendants did not stop; killing them\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newCodexProcessCleanupHarness(1000, 50, 50, 51)
			h.signalFunc = func(pid int, sig syscall.Signal) error {
				if sig == syscall.SIGTERM {
					tt.removeTerm(h, pid)
					return nil
				}
				delete(h.processes, pid)
				delete(h.sids, pid)
				return nil
			}

			stderr, err := runCodexProcessCleanupCommand(h, 50, "100ms")
			if err != nil {
				t.Fatal(err)
			}
			if stderr != tt.warning {
				t.Fatalf("stderr = %q, want %q", stderr, tt.warning)
			}
			if !hasCodexCleanupSignal(h.signals, syscall.SIGKILL) {
				t.Fatalf("signals = %#v; no SIGKILL", h.signals)
			}
		})
	}
}

func TestCodexProcessSessionCleanupCmdReturnsPersistentSignalError(t *testing.T) {
	permissionErr := errors.New("signal denied")
	h := newCodexProcessCleanupHarness(1000, 60, 60, 61)
	h.signalFunc = func(_ int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			return permissionErr
		}
		return nil
	}

	stderr, err := runCodexProcessCleanupCommand(h, 60, "100ms")
	if err == nil || !errors.Is(err, permissionErr) || !strings.Contains(err.Error(), "signal process 61 with SIGKILL") {
		t.Fatalf("error = %v, want persistent SIGKILL error wrapping %v", err, permissionErr)
	}
	if stderr != "intercom-codex-project: app-server did not stop; killing it\n" {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestCodexProcessSessionCleanupCmdReturnsFinalSurvivors(t *testing.T) {
	h := newCodexProcessCleanupHarness(1000, 70, 70, 71)
	h.signalFunc = func(int, syscall.Signal) error { return nil }

	_, err := runCodexProcessCleanupCommand(h, 70, "100ms")
	if err == nil || err.Error() != "app-server process session 70 still has processes after SIGKILL: 70, 71" {
		t.Fatalf("error = %v", err)
	}
}

func TestCodexProcessSessionCleanupCmdSignalsEachPIDOncePerPhase(t *testing.T) {
	h := newCodexProcessCleanupHarness(1000, 75, 75, 76)
	h.signalFunc = func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGKILL {
			delete(h.processes, pid)
			delete(h.sids, pid)
		}
		return nil
	}

	stderr, err := runCodexProcessCleanupCommand(h, 75, "250ms")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "intercom-codex-project: app-server did not stop; killing it\n" {
		t.Fatalf("stderr = %q", stderr)
	}
	counts := make(map[codexProcessCleanupSignal]int)
	for _, call := range h.signals {
		counts[call]++
	}
	for _, pid := range []int{75, 76} {
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			call := codexProcessCleanupSignal{pid: pid, sig: sig}
			if counts[call] != 1 {
				t.Fatalf("signal %s to PID %d occurred %d times, want 1; all signals = %#v", codexSignalName(sig), pid, counts[call], h.signals)
			}
		}
	}
}

func TestCodexProcessCleanupPhaseBoundsBlockingEnumeration(t *testing.T) {
	origin := time.Now()
	ops := codexProcessSessionCleanupOps{
		processes: func(ctx context.Context) ([]codexProcess, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
		getsid: func(int) (int, error) { return 0, syscall.ESRCH },
		kill:   func(int, syscall.Signal) error { return nil },
		monotonicNow: func() time.Duration {
			return time.Since(origin)
		},
		sleep: sleepCodexProcessCleanup,
	}

	started := time.Now()
	result := runCodexProcessCleanupPhase(context.Background(), 77, 77, syscall.SIGTERM, 30*time.Millisecond, ops)
	if !result.timedOut || !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("phase result = %#v, want bounded deadline", result)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocking enumeration returned after %s", elapsed)
	}
}

func TestCodexProcessSessionCleanupBoundsPostTermClassification(t *testing.T) {
	h := newCodexProcessCleanupHarness(1000, 78, 78)
	calls := 0
	h.processesFunc = func(ctx context.Context) ([]codexProcess, error) {
		calls++
		switch calls {
		case 1:
			return []codexProcess{{pid: 78, stat: "S"}}, nil
		case 2:
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("classification process enumeration has no deadline")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		default:
			return nil, nil
		}
	}
	h.signalFunc = func(int, syscall.Signal) error { return nil }
	var stderr bytes.Buffer
	started := time.Now()
	err := cleanupCodexProcessSession(context.Background(), &stderr, 78, 78, 30*time.Millisecond, h.ops())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("process enumeration calls = %d, want TERM, classification, and KILL", calls)
	}
	if stderr.String() != "intercom-codex-project: app-server descendants did not stop; killing them\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocking classification returned after %s", elapsed)
	}
}

func TestCodexProcessSessionCleanupBoundsFinalInspection(t *testing.T) {
	h := newCodexProcessCleanupHarness(1000, 79, 79)
	calls := 0
	h.processesFunc = func(ctx context.Context) ([]codexProcess, error) {
		calls++
		if calls == 4 {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("final process inspection has no deadline")
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return []codexProcess{{pid: 79, stat: "S"}}, nil
	}
	h.signalFunc = func(int, syscall.Signal) error { return nil }
	var stderr bytes.Buffer
	started := time.Now()
	err := cleanupCodexProcessSession(context.Background(), &stderr, 79, 79, 30*time.Millisecond, h.ops())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "inspect app-server process session 79 after SIGKILL") {
		t.Fatalf("error = %v, want bounded final-inspection deadline", err)
	}
	if calls != 4 {
		t.Fatalf("process enumeration calls = %d, want TERM, classification, KILL, and final inspection", calls)
	}
	if stderr.String() != "intercom-codex-project: app-server did not stop; killing it\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocking final inspection returned after %s", elapsed)
	}
}

func TestCodexProcessSessionCleanupCmdRetriesProcessEnumeration(t *testing.T) {
	transientErr := errors.New("temporary ps failure")
	h := newCodexProcessCleanupHarness(1000, 80, 80)
	h.enumerationErrors = []error{transientErr}

	stderr, err := runCodexProcessCleanupCommand(h, 80, "1s")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q", stderr)
	}
	if h.enumerationCalls < 3 {
		t.Fatalf("enumeration calls = %d, want retry and final empty observation", h.enumerationCalls)
	}
}

func TestCodexProcessSessionCleanupCmdReturnsPersistentEnumerationError(t *testing.T) {
	psErr := errors.New("ps unavailable")
	h := newCodexProcessCleanupHarness(1000, 90, 90)
	h.enumerationErrors = make([]error, 20)
	for i := range h.enumerationErrors {
		h.enumerationErrors[i] = psErr
	}

	stderr, err := runCodexProcessCleanupCommand(h, 90, "100ms")
	if err == nil || !errors.Is(err, psErr) || !strings.Contains(err.Error(), "inspect app-server process session 90 after SIGKILL: enumerate processes") {
		t.Fatalf("error = %v, want persistent enumeration error wrapping %v", err, psErr)
	}
	if stderr != "intercom-codex-project: app-server descendants did not stop; killing them\n" {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestCodexProcessSessionCleanupCmdUsesIndependentMonotonicPhaseDeadlines(t *testing.T) {
	h := newCodexProcessCleanupHarness(1000, 100, 100)
	h.signalFunc = func(int, syscall.Signal) error { return nil }

	_, err := runCodexProcessCleanupCommand(h, 100, "250ms")
	if err == nil || !strings.Contains(err.Error(), "still has processes after SIGKILL") {
		t.Fatalf("error = %v", err)
	}
	wantSleeps := []time.Duration{
		100 * time.Millisecond,
		100 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		100 * time.Millisecond,
		50 * time.Millisecond,
	}
	if !reflect.DeepEqual(h.sleeps, wantSleeps) {
		t.Fatalf("sleeps = %v, want %v", h.sleeps, wantSleeps)
	}
	if h.clock != 500*time.Millisecond {
		t.Fatalf("monotonic clock = %s, want 500ms", h.clock)
	}
}

func TestParseCodexProcessTable(t *testing.T) {
	processes, err := parseCodexProcessTable([]byte(" 10 S+\n11 Z\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []codexProcess{
		{pid: 10, stat: "S+"},
		{pid: 11, stat: "Z"},
	}
	if !reflect.DeepEqual(processes, want) {
		t.Fatalf("processes = %#v, want %#v", processes, want)
	}

	for _, table := range []string{"x S\n", "10\n", "0 S\n", "10 S extra\n"} {
		if _, err := parseCodexProcessTable([]byte(table)); err == nil {
			t.Fatalf("parseCodexProcessTable(%q) succeeded", table)
		}
	}
}

func TestCodexProcessSessionProductionInspectionFindsCurrentProcess(t *testing.T) {
	processes, err := listCodexProcesses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, process := range processes {
		if process.pid == os.Getpid() {
			found = true
			if process.stat == "" || strings.HasPrefix(process.stat, "Z") {
				t.Fatalf("current process state = %q", process.stat)
			}
			break
		}
	}
	if !found {
		t.Fatalf("ps output does not contain current process %d", os.Getpid())
	}
	sid, err := codexProcessGetsid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if sid <= 0 {
		t.Fatalf("current process session ID = %d", sid)
	}
}

func hasCodexCleanupSignal(signals []codexProcessCleanupSignal, want syscall.Signal) bool {
	for _, signal := range signals {
		if signal.sig == want {
			return true
		}
	}
	return false
}
