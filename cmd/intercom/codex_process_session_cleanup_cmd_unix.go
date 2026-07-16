//go:build linux || darwin

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const codexProcessSessionCleanupPollInterval = 100 * time.Millisecond

type codexProcess struct {
	pid  int
	stat string
}

type codexProcessSessionCleanupOps struct {
	processes     func(context.Context) ([]codexProcess, error)
	getsid        func(int) (int, error)
	kill          func(int, syscall.Signal) error
	monotonicNow  func() time.Duration
	sleep         func(context.Context, time.Duration) error
	ignoreSignals func(...os.Signal)
}

func newCodexProcessSessionCleanupCmd() *cobra.Command {
	origin := time.Now()
	return newCodexProcessSessionCleanupCmdWithOps(codexProcessSessionCleanupOps{
		processes: listCodexProcesses,
		getsid:    codexProcessGetsid,
		kill:      syscall.Kill,
		monotonicNow: func() time.Duration {
			return time.Since(origin)
		},
		sleep:         sleepCodexProcessCleanup,
		ignoreSignals: signal.Ignore,
	})
}

func newCodexProcessSessionCleanupCmdWithOps(ops codexProcessSessionCleanupOps) *cobra.Command {
	var sid int
	var leader int
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:    "codex-process-session-cleanup --sid PID --leader PID --timeout DURATION",
		Short:  "Stops a dedicated Codex app-server process session",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sid <= 0 {
				return errors.New("--sid must be a positive process ID")
			}
			if leader <= 0 {
				return errors.New("--leader must be a positive process ID")
			}
			if leader != sid {
				return errors.New("--leader must equal --sid")
			}
			if timeout <= 0 {
				return errors.New("--timeout must be a positive duration")
			}
			if err := validateCodexProcessSessionCleanupOps(ops); err != nil {
				return err
			}

			// The helper is a cleanup boundary. Repeated terminal signals delivered
			// to the launcher must not interrupt it between TERM and KILL.
			ops.ignoreSignals(os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
			return cleanupCodexProcessSession(cmd.Context(), cmd.ErrOrStderr(), sid, leader, timeout, ops)
		},
	}
	cmd.Flags().IntVar(&sid, "sid", 0, "Process-session ID")
	cmd.Flags().IntVar(&leader, "leader", 0, "Process ID of the app-server session leader")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "TERM and KILL phase timeout")
	return cmd
}

func validateCodexProcessSessionCleanupOps(ops codexProcessSessionCleanupOps) error {
	switch {
	case ops.processes == nil:
		return errors.New("process-session cleanup enumeration operation is nil")
	case ops.getsid == nil:
		return errors.New("process-session cleanup getsid operation is nil")
	case ops.kill == nil:
		return errors.New("process-session cleanup signal operation is nil")
	case ops.monotonicNow == nil:
		return errors.New("process-session cleanup clock operation is nil")
	case ops.sleep == nil:
		return errors.New("process-session cleanup sleep operation is nil")
	case ops.ignoreSignals == nil:
		return errors.New("process-session cleanup signal-ignore operation is nil")
	default:
		return nil
	}
}

func cleanupCodexProcessSession(
	ctx context.Context,
	stderr io.Writer,
	sid int,
	leader int,
	timeout time.Duration,
	ops codexProcessSessionCleanupOps,
) error {
	term := runCodexProcessCleanupPhase(ctx, sid, leader, syscall.SIGTERM, timeout, ops)
	if term.stopped {
		return nil
	}
	if term.err != nil && !term.timedOut {
		return term.err
	}

	classificationTimeout := min(timeout, time.Second)
	classificationCtx, cancelClassification := context.WithTimeout(ctx, classificationTimeout)
	leaderPresent, _ := codexProcessSessionLeaderPresent(classificationCtx, sid, leader, ops)
	cancelClassification()
	if leaderPresent {
		fmt.Fprintln(stderr, "intercom-codex-project: app-server did not stop; killing it")
	} else {
		fmt.Fprintln(stderr, "intercom-codex-project: app-server descendants did not stop; killing them")
	}

	kill := runCodexProcessCleanupPhase(ctx, sid, leader, syscall.SIGKILL, timeout, ops)
	if kill.stopped {
		return nil
	}
	if kill.err != nil && !kill.timedOut {
		return kill.err
	}

	finalTimeout := min(timeout, time.Second)
	inspectCtx, cancelInspect := context.WithTimeout(ctx, finalTimeout)
	members, inspectErr := codexProcessSessionMembers(inspectCtx, sid, ops)
	cancelInspect()
	if inspectErr != nil {
		return fmt.Errorf("inspect app-server process session %d after SIGKILL: %w", sid, inspectErr)
	}
	if len(members) == 0 {
		return nil
	}
	if kill.err != nil {
		return fmt.Errorf("stop app-server process session %d with SIGKILL: %w", sid, kill.err)
	}
	return fmt.Errorf(
		"app-server process session %d still has processes after SIGKILL: %s",
		sid,
		formatCodexProcessIDs(members),
	)
}

func codexProcessSessionLeaderPresent(
	ctx context.Context,
	sid int,
	leader int,
	ops codexProcessSessionCleanupOps,
) (bool, error) {
	processes, err := ops.processes(ctx)
	if err != nil {
		return false, fmt.Errorf("enumerate processes: %w", err)
	}
	for _, process := range processes {
		if process.pid != leader {
			continue
		}
		if process.stat == "" || strings.HasPrefix(process.stat, "Z") {
			return false, nil
		}
		return codexProcessInSession(leader, sid, ops)
	}
	return false, nil
}

type codexProcessCleanupPhaseResult struct {
	stopped  bool
	timedOut bool
	err      error
}

func runCodexProcessCleanupPhase(
	ctx context.Context,
	sid int,
	leader int,
	sig syscall.Signal,
	timeout time.Duration,
	ops codexProcessSessionCleanupOps,
) codexProcessCleanupPhaseResult {
	started := ops.monotonicNow()
	var lastErr error
	signaled := make(map[int]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return codexProcessCleanupPhaseResult{err: fmt.Errorf("clean app-server process session %d: %w", sid, err)}
		}
		remaining := timeout - (ops.monotonicNow() - started)
		if remaining <= 0 {
			return codexProcessCleanupPhaseResult{timedOut: true, err: lastErr}
		}

		inspectCtx, cancelInspect := context.WithTimeout(ctx, remaining)
		members, inspectErr := codexProcessSessionMembers(inspectCtx, sid, ops)
		cancelInspect()
		if len(members) == 0 && inspectErr == nil {
			return codexProcessCleanupPhaseResult{stopped: true}
		}
		if errors.Is(inspectErr, context.DeadlineExceeded) && ctx.Err() == nil {
			return codexProcessCleanupPhaseResult{timedOut: true, err: inspectErr}
		}
		if ops.monotonicNow()-started >= timeout {
			if inspectErr != nil {
				lastErr = inspectErr
			}
			return codexProcessCleanupPhaseResult{timedOut: true, err: lastErr}
		}

		passErr := inspectErr
		if len(members) != 0 {
			if err := signalCodexProcessSessionMembers(sid, leader, sig, members, signaled, ops); err != nil {
				passErr = err
			}
		}
		lastErr = passErr

		remaining = timeout - (ops.monotonicNow() - started)
		if remaining <= 0 {
			return codexProcessCleanupPhaseResult{timedOut: true, err: lastErr}
		}
		wait := min(codexProcessSessionCleanupPollInterval, remaining)
		if err := ops.sleep(ctx, wait); err != nil {
			return codexProcessCleanupPhaseResult{err: fmt.Errorf("wait while cleaning app-server process session %d: %w", sid, err)}
		}
	}
}

func codexProcessSessionMembers(
	ctx context.Context,
	sid int,
	ops codexProcessSessionCleanupOps,
) ([]int, error) {
	processes, err := ops.processes(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	members := make([]int, 0)
	var inspectErr error
	for _, process := range processes {
		if process.pid <= 0 || process.stat == "" || strings.HasPrefix(process.stat, "Z") {
			continue
		}
		member, err := codexProcessInSession(process.pid, sid, ops)
		if err != nil {
			if inspectErr == nil {
				inspectErr = fmt.Errorf("inspect process %d session: %w", process.pid, err)
			}
			continue
		}
		if member {
			members = append(members, process.pid)
		}
	}
	sort.Ints(members)
	return members, inspectErr
}

func codexProcessInSession(pid int, sid int, ops codexProcessSessionCleanupOps) (bool, error) {
	actual, err := ops.getsid(pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	return actual == sid, nil
}

func signalCodexProcessSessionMembers(
	sid int,
	leader int,
	sig syscall.Signal,
	members []int,
	signaled map[int]struct{},
	ops codexProcessSessionCleanupOps,
) error {
	ordered := make([]int, 0, len(members))
	for _, pid := range members {
		if pid != leader {
			ordered = append(ordered, pid)
		}
	}
	for _, pid := range members {
		if pid == leader {
			ordered = append(ordered, pid)
			break
		}
	}

	var firstErr error
	for _, pid := range ordered {
		if _, alreadySignaled := signaled[pid]; alreadySignaled {
			continue
		}
		// getsid(2) is repeated immediately before every signal. The process
		// table is only a candidate list and never authorizes a signal by itself.
		member, err := codexProcessInSession(pid, sid, ops)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("reverify process %d before %s: %w", pid, codexSignalName(sig), err)
			}
			continue
		}
		if !member {
			continue
		}
		if err := ops.kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
			if firstErr == nil {
				firstErr = fmt.Errorf("signal process %d with %s: %w", pid, codexSignalName(sig), err)
			}
			continue
		}
		signaled[pid] = struct{}{}
	}
	return firstErr
}

func codexSignalName(sig syscall.Signal) string {
	switch sig {
	case syscall.SIGTERM:
		return "SIGTERM"
	case syscall.SIGKILL:
		return "SIGKILL"
	default:
		return fmt.Sprintf("signal %d", sig)
	}
}

func formatCodexProcessIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return strings.Join(parts, ", ")
}

func listCodexProcesses(ctx context.Context) ([]codexProcess, error) {
	output, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "stat=").Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("run ps: %w", err)
	}
	return parseCodexProcessTable(output)
}

func parseCodexProcessTable(table []byte) ([]codexProcess, error) {
	var processes []codexProcess
	scanner := bufio.NewScanner(strings.NewReader(string(table)))
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("parse ps output line %d: expected pid and state", line)
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			return nil, fmt.Errorf("parse ps output line %d: invalid pid %q", line, fields[0])
		}
		processes = append(processes, codexProcess{pid: pid, stat: fields[1]})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ps output: %w", err)
	}
	return processes, nil
}

func sleepCodexProcessCleanup(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
