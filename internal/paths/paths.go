// Package paths centralizes the on-disk paths the shim and broker share.
// Every path is overridable by an env var so tests can isolate to a temp dir.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	envSocket = "INTERCOM_SOCKET"
	envLog    = "INTERCOM_BROKER_LOG"

	dirName  = ".claude-intercom"
	sockName = "broker.sock"
	lockName = "broker.sock.lock"
	logName  = "broker.log"
	codexDir = "codex"
)

// Dir returns the runtime directory (~/.claude-intercom by default).
// It is created with mode 0700 if missing. The first non-nil error short-
// circuits subsequent helper calls in the same process.
func Dir() (string, error) {
	if d := os.Getenv("INTERCOM_DIR"); d != "" {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", fmt.Errorf("paths: mkdir %s: %w", d, err)
		}
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("paths: resolve home: %w", err)
	}
	d := filepath.Join(home, dirName)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", fmt.Errorf("paths: mkdir %s: %w", d, err)
	}
	return d, nil
}

// Socket returns the broker's Unix socket path. Honors $INTERCOM_SOCKET if
// set; otherwise <Dir>/broker.sock.
func Socket() (string, error) {
	if p := os.Getenv(envSocket); p != "" {
		return p, nil
	}
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, sockName), nil
}

// Lock returns the path of the broker's startup lock file. Always derived
// from Socket() (with .lock appended) so a custom socket path moves the lock
// file alongside it.
func Lock() (string, error) {
	s, err := Socket()
	if err != nil {
		return "", err
	}
	return s + ".lock", nil
}

// Log returns the broker's log file path. Honors $INTERCOM_BROKER_LOG if set;
// otherwise <Dir>/broker.log.
func Log() (string, error) {
	if p := os.Getenv(envLog); p != "" {
		return p, nil
	}
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, logName), nil
}

// CodexDir returns the directory containing managed Codex peer state.
func CodexDir() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(d, codexDir)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return "", fmt.Errorf("paths: mkdir %s: %w", p, err)
	}
	return p, nil
}

// CodexState returns the managed state path for peer. Callers must validate
// peer before using it as a path component.
func CodexState(peer string) (string, error) {
	d, err := CodexDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, peer+".json"), nil
}

// CodexLock returns the lifetime lock path for a managed Codex peer.
func CodexLock(peer string) (string, error) {
	d, err := CodexDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, peer+".lock"), nil
}
