package codex

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validState() ManagedState {
	return ManagedState{
		SchemaVersion:       StateSchemaVersion,
		Peer:                "reviewer",
		ThreadID:            "019-test",
		CWD:                 "/tmp/project",
		CodexHome:           "/tmp/codex-home",
		ServerUserAgent:     "codex-cli/0.144.1",
		CodexVersion:        "0.144.1",
		ToolContractVersion: ToolContractVersion,
	}
}

func TestStateStoreRoundTripAndMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := AcquireStateStore(filepath.Join(dir, "reviewer.json"), filepath.Join(dir, "reviewer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	got, err := store.Load()
	if err != nil || got != nil {
		t.Fatalf("Load() = %#v, %v; want nil, nil", got, err)
	}
	want := validState()
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if *got != want {
		t.Fatalf("Load() = %#v, want %#v", *got, want)
	}
	info, err := os.Stat(filepath.Join(dir, "reviewer.json"))
	if err != nil {
		t.Fatal(err)
	}
	if gotMode := info.Mode().Perm(); gotMode != 0o600 {
		t.Fatalf("state mode = %o, want 600", gotMode)
	}
}

func TestStateStoreSaveSyncsParentDirectoryAfterRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "reviewer.json")
	store, err := AcquireStateStore(statePath, filepath.Join(dir, "reviewer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	want := validState()
	calls := 0
	store.syncDir = func(path string) error {
		calls++
		if path != dir {
			t.Fatalf("sync directory = %q, want %q", path, dir)
		}
		// The seam runs after rename: the durable path must already expose the
		// complete replacement, not the prior file or a temporary name.
		got, err := store.Load()
		if err != nil {
			t.Fatalf("load during directory sync: %v", err)
		}
		if got == nil || *got != want {
			t.Fatalf("state visible during directory sync = %#v, want %#v", got, want)
		}
		return nil
	}
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("directory sync calls = %d, want 1", calls)
	}
}

func TestStateStoreSaveReportsDirectorySyncFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := AcquireStateStore(filepath.Join(dir, "reviewer.json"), filepath.Join(dir, "reviewer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	wantErr := errors.New("injected directory sync failure")
	store.syncDir = func(string) error { return wantErr }
	err = store.Save(validState())
	if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "sync directory") {
		t.Fatalf("Save() error = %v, want wrapped directory sync failure", err)
	}
}

func TestStateStoreRejectsSecondOwner(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "reviewer.json")
	lockPath := filepath.Join(dir, "reviewer.lock")
	first, err := AcquireStateStore(statePath, lockPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second, err := AcquireStateStore(statePath, lockPath)
	if second != nil {
		_ = second.Close()
		t.Fatal("second AcquireStateStore() unexpectedly succeeded")
	}
	if err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("second AcquireStateStore() error = %v", err)
	}
}

func TestStateStoreRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := AcquireStateStore(filepath.Join(dir, "reviewer.json"), filepath.Join(dir, "reviewer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Save(validState()); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil || got != nil {
		t.Fatalf("Load() after Remove() = %#v, %v", got, err)
	}
}

func TestManagedStateValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mutate  func(*ManagedState)
		wantErr string
	}{
		{name: "schema", mutate: func(s *ManagedState) { s.SchemaVersion++ }, wantErr: "unsupported schema"},
		{name: "missing identity", mutate: func(s *ManagedState) { s.ThreadID = "" }, wantErr: "missing required"},
		{name: "tool contract", mutate: func(s *ManagedState) { s.ToolContractVersion++ }, wantErr: "--new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := validState()
			tc.mutate(&state)
			if err := state.Validate(); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestStateStoreLoadRejectsCorruptOrIncompatibleState(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		content string
		wantErr string
	}{
		{name: "malformed JSON", content: `{`, wantErr: "decode"},
		{name: "missing fields", content: `{"schemaVersion":1,"toolContractVersion":1}`, wantErr: "missing required"},
		{name: "future schema", content: `{"schemaVersion":2}`, wantErr: "unsupported schema"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			statePath := filepath.Join(dir, "reviewer.json")
			if err := os.WriteFile(statePath, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := AcquireStateStore(statePath, filepath.Join(dir, "reviewer.lock"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			if got, err := store.Load(); got != nil || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() = %#v, %v; want error containing %q", got, err, tt.wantErr)
			}
		})
	}
}

func TestStateStoreInvalidSavePreservesBinding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := AcquireStateStore(filepath.Join(dir, "reviewer.json"), filepath.Join(dir, "reviewer.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	want := validState()
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	invalid := want
	invalid.ThreadID = ""
	if err := store.Save(invalid); err == nil {
		t.Fatal("Save() accepted invalid replacement")
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != want {
		t.Fatalf("binding after failed Save() = %#v, want %#v", got, want)
	}
}
