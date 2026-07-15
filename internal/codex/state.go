package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

const (
	StateSchemaVersion  = 1
	ToolContractVersion = 1
)

// ManagedState binds an Intercom peer to the Codex thread it exclusively
// manages. Conversation data remains in CODEX_HOME; this record contains only
// binding identity, compatibility metadata, and last-validated runtime
// diagnostics.
type ManagedState struct {
	SchemaVersion       int    `json:"schemaVersion"`
	Peer                string `json:"peer"`
	ThreadID            string `json:"threadId"`
	CWD                 string `json:"cwd"`
	CodexHome           string `json:"codexHome"`
	ServerUserAgent     string `json:"serverUserAgent"`
	CodexVersion        string `json:"codexVersion"`
	ToolContractVersion int    `json:"toolContractVersion"`
	Materialized        bool   `json:"materialized"`
}

func (s ManagedState) Validate() error {
	if s.SchemaVersion != StateSchemaVersion {
		return fmt.Errorf("codex state: unsupported schema version %d (want %d)", s.SchemaVersion, StateSchemaVersion)
	}
	if s.Peer == "" || s.ThreadID == "" || s.CWD == "" || s.CodexHome == "" || s.ServerUserAgent == "" || s.CodexVersion == "" {
		return errors.New("codex state: missing required identity field")
	}
	if s.ToolContractVersion != ToolContractVersion {
		return fmt.Errorf("codex state: tool contract version %d is incompatible with %d; start with --new", s.ToolContractVersion, ToolContractVersion)
	}
	return nil
}

// StateStore owns the non-blocking lifetime lock for one peer and provides
// atomic state reads and writes.
type StateStore struct {
	statePath string
	lockFile  *os.File
	syncDir   func(string) error
}

// AcquireStateStore acquires lockPath without waiting. The returned store must
// be closed for the process lifetime of the managed peer.
func AcquireStateStore(statePath, lockPath string) (*StateStore, error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, fmt.Errorf("codex state: create directory: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("codex state: open lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("codex state: peer is already managed: %w", err)
		}
		return nil, fmt.Errorf("codex state: acquire lock: %w", err)
	}
	return &StateStore{statePath: statePath, lockFile: f, syncDir: syncDirectory}, nil
}

// Load returns nil, nil when no binding exists.
func (s *StateStore) Load() (*ManagedState, error) {
	b, err := os.ReadFile(s.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex state: read: %w", err)
	}
	var state ManagedState
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("codex state: decode: %w", err)
	}
	if err := state.Validate(); err != nil {
		return nil, err
	}
	return &state, nil
}

// Save durably and atomically replaces the state file with mode 0600.
func (s *StateStore) Save(state ManagedState) error {
	if err := state.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("codex state: encode: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(s.statePath)
	tmp, err := os.CreateTemp(dir, ".codex-state-*")
	if err != nil {
		return fmt.Errorf("codex state: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex state: chmod temporary file: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex state: write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex state: sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codex state: close temporary file: %w", err)
	}
	if err := os.Rename(tmpName, s.statePath); err != nil {
		return fmt.Errorf("codex state: replace: %w", err)
	}
	syncDir := s.syncDir
	if syncDir == nil {
		syncDir = syncDirectory
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("codex state: sync directory: %w", err)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	// Darwin filesystems may reject fsync on a directory even though rename
	// is already atomic. Treat only the documented unsupported forms as a
	// best-effort portability case; propagate every other durability error.
	if runtime.GOOS == "darwin" && (errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP)) {
		syncErr = nil
	}
	return errors.Join(syncErr, dir.Close())
}

func (s *StateStore) Remove() error {
	err := os.Remove(s.statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("codex state: remove: %w", err)
	}
	return nil
}

func (s *StateStore) Close() error {
	if s == nil || s.lockFile == nil {
		return nil
	}
	f := s.lockFile
	s.lockFile = nil
	unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	return errors.Join(unlockErr, closeErr)
}
