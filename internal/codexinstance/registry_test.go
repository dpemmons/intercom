package codexinstance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	r, err := New(filepath.Join(root, "live"))
	if err != nil {
		t.Fatal(err)
	}
	return r, root
}

func TestNewSecuresLiveDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	live := filepath.Join(root, "live")
	if err := os.Mkdir(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(live, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := New(live)
	if err != nil {
		t.Fatal(err)
	}
	if r.Dir() != live {
		t.Fatalf("Dir() = %q, want %q", r.Dir(), live)
	}
	info, err := os.Stat(live)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("live directory mode = %04o, want 0700", info.Mode().Perm())
	}

	if err := os.Chmod(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Path(filepath.Join(root, "broker.sock"), "peer"); err == nil || !strings.Contains(err.Error(), "want 0700") {
		t.Fatalf("Path() after permission change error = %v", err)
	}
}

func TestNewRejectsSymlinkLiveDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "live")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if r, err := New(link); err == nil || r != nil || !strings.Contains(err.Error(), "not a real directory") {
		t.Fatalf("New(symlink) = %#v, %v", r, err)
	}
}

func TestPathIsDeterministicAndKeyedByBrokerAndPeer(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	broker := filepath.Join(root, "broker.sock")
	first, err := r.Path(broker, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Path(filepath.Join(root, "sub", "..", "broker.sock"), "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("equivalent broker paths produced %q and %q", first, again)
	}
	otherBroker, err := r.Path(filepath.Join(root, "other.sock"), "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	otherPeer, err := r.Path(broker, "writer")
	if err != nil {
		t.Fatal(err)
	}
	if first == otherBroker || first == otherPeer || otherBroker == otherPeer {
		t.Fatalf("distinct keys collided: %q, %q, %q", first, otherBroker, otherPeer)
	}
	base := filepath.Base(first)
	if !strings.HasPrefix(base, "reviewer-") || !strings.HasSuffix(base, ".json") || len(base) != len("reviewer-")+64+len(".json") {
		t.Fatalf("descriptor filename = %q", base)
	}
	if _, err := r.Path(broker, "../unsafe"); err == nil {
		t.Fatal("Path accepted unsafe peer")
	}
}

func TestPublishLoadRemoveRoundTripAndModes(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	r.processAlive = func(int) (bool, error) { return true, nil }
	d := validDescriptor(t, root)

	got, err := r.Load(d.BrokerSocketIdentity, d.Peer)
	if err != nil || got != nil {
		t.Fatalf("Load() before publish = %#v, %v; want nil, nil", got, err)
	}
	path, err := r.Publish(d)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := r.Path(d.BrokerSocketIdentity, d.Peer); path != want {
		t.Fatalf("Publish() path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("descriptor mode = %04o, want 0600", info.Mode().Perm())
	}
	lockInfo, err := os.Stat(filepath.Join(r.Dir(), registryLockName))
	if err != nil {
		t.Fatal(err)
	}
	if lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("registry lock mode = %04o, want 0600", lockInfo.Mode().Perm())
	}
	got, err = r.Load(d.BrokerSocketIdentity, d.Peer)
	if err != nil || got == nil || *got != d {
		t.Fatalf("Load() = %#v, %v; want %#v", got, err, d)
	}

	updated := d
	updated.ThreadID = "019-updated-thread"
	if _, err := r.Publish(updated); err != nil {
		t.Fatalf("same-nonce update: %v", err)
	}
	got, err = r.Load(d.BrokerSocketIdentity, d.Peer)
	if err != nil || got == nil || *got != updated {
		t.Fatalf("Load() after update = %#v, %v", got, err)
	}

	removed, err := r.Remove(d.BrokerSocketIdentity, d.Peer, "fedcba9876543210fedcba9876543210")
	if err != nil || removed {
		t.Fatalf("Remove(wrong nonce) = %v, %v; want false, nil", removed, err)
	}
	if got, err := r.Load(d.BrokerSocketIdentity, d.Peer); err != nil || got == nil {
		t.Fatalf("wrong nonce removed descriptor: %#v, %v", got, err)
	}
	removed, err = r.Remove(d.BrokerSocketIdentity, d.Peer, d.InstanceNonce)
	if err != nil || !removed {
		t.Fatalf("Remove(owner nonce) = %v, %v; want true, nil", removed, err)
	}
	removed, err = r.Remove(d.BrokerSocketIdentity, d.Peer, d.InstanceNonce)
	if err != nil || removed {
		t.Fatalf("second Remove() = %v, %v; want false, nil", removed, err)
	}

	entries, err := os.ReadDir(r.Dir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".instance-") {
			t.Errorf("temporary descriptor leaked: %s", entry.Name())
		}
	}
}

func TestPublishCleansDescriptorWhenDirectorySyncFailsAfterRename(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	d := validDescriptor(t, root)
	path, err := r.Path(d.BrokerSocketIdentity, d.Peer)
	if err != nil {
		t.Fatal(err)
	}

	publishSyncErr := errors.New("injected publish directory sync failure")
	cleanupSyncErr := errors.New("injected cleanup directory sync failure")
	syncCalls := 0
	r.syncDir = func(path string) error {
		if path != r.Dir() {
			t.Fatalf("syncDir(%q), want %q", path, r.Dir())
		}
		syncCalls++
		switch syncCalls {
		case 1:
			return publishSyncErr
		case 2:
			return cleanupSyncErr
		default:
			t.Fatalf("unexpected syncDir call %d", syncCalls)
			return nil
		}
	}

	gotPath, err := r.Publish(d)
	if gotPath != "" {
		t.Fatalf("Publish() path = %q, want empty path on failure", gotPath)
	}
	if !errors.Is(err, publishSyncErr) {
		t.Fatalf("Publish() error = %v, want original sync error", err)
	}
	if !errors.Is(err, cleanupSyncErr) {
		t.Fatalf("Publish() error = %v, want joined cleanup sync error", err)
	}
	if syncCalls != 2 {
		t.Fatalf("syncDir calls = %d, want 2", syncCalls)
	}
	if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("descriptor after failed Publish() stat error = %v, want not exist", statErr)
	}
}

func TestPublishFailureCleanupPreservesDescriptorWithDifferentNonce(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	d := validDescriptor(t, root)
	path, err := r.Path(d.BrokerSocketIdentity, d.Peer)
	if err != nil {
		t.Fatal(err)
	}
	replacement := d
	replacement.InstanceNonce = "fedcba9876543210fedcba9876543210"
	replacement.ThreadID = "019-external-replacement"
	replacementJSON, err := json.MarshalIndent(replacement, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	replacementJSON = append(replacementJSON, '\n')

	wantErr := errors.New("injected publish directory sync failure")
	syncCalls := 0
	r.syncDir = func(string) error {
		syncCalls++
		if syncCalls != 1 {
			t.Fatalf("unexpected syncDir call %d", syncCalls)
		}
		if err := os.WriteFile(path, replacementJSON, 0o600); err != nil {
			t.Fatalf("replace descriptor during sync: %v", err)
		}
		return wantErr
	}

	if _, err := r.Publish(d); !errors.Is(err, wantErr) {
		t.Fatalf("Publish() error = %v, want original sync error", err)
	}
	if syncCalls != 1 {
		t.Fatalf("syncDir calls = %d, want 1", syncCalls)
	}
	got, err := r.Load(replacement.BrokerSocketIdentity, replacement.Peer)
	if err != nil || got == nil || *got != replacement {
		t.Fatalf("replacement after failed Publish() = %#v, %v; want %#v", got, err, replacement)
	}
}

func TestLoadReportsStaleDescriptor(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	d := validDescriptor(t, root)
	if _, err := r.Publish(d); err != nil {
		t.Fatal(err)
	}
	r.processAlive = func(pid int) (bool, error) {
		if pid != d.PID {
			t.Fatalf("process probe pid = %d, want %d", pid, d.PID)
		}
		return false, nil
	}
	got, err := r.Load(d.BrokerSocketIdentity, d.Peer)
	if got != nil || !errors.Is(err, ErrStale) {
		t.Fatalf("Load(stale) = %#v, %v; want ErrStale", got, err)
	}
	var stale *StaleError
	if !errors.As(err, &stale) || stale.Descriptor != d {
		t.Fatalf("stale detail = %#v", stale)
	}
}

func TestPublishRejectsDifferentLiveOwner(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	first := validDescriptor(t, root)
	if _, err := r.Publish(first); err != nil {
		t.Fatal(err)
	}
	r.processAlive = func(pid int) (bool, error) {
		if pid != first.PID {
			t.Fatalf("process probe pid = %d, want %d", pid, first.PID)
		}
		return true, nil
	}
	second := first
	second.InstanceNonce = "fedcba9876543210fedcba9876543210"
	second.ThreadID = "019-second"
	if _, err := r.Publish(second); !errors.Is(err, ErrAlreadyLive) {
		t.Fatalf("Publish(second) error = %v, want ErrAlreadyLive", err)
	} else {
		var live *AlreadyLiveError
		if !errors.As(err, &live) || live.Existing != first {
			t.Fatalf("AlreadyLiveError = %#v", live)
		}
	}
	got, err := r.Load(first.BrokerSocketIdentity, first.Peer)
	if err != nil || got == nil || *got != first {
		t.Fatalf("live collision changed descriptor: %#v, %v", got, err)
	}
}

func TestPublishReplacesStaleOwnerAndOldNonceCannotRemoveIt(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	first := validDescriptor(t, root)
	first.PID = 111
	if _, err := r.Publish(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.PID = 222
	second.InstanceNonce = "fedcba9876543210fedcba9876543210"
	second.ThreadID = "019-replacement"
	r.processAlive = func(pid int) (bool, error) { return pid == second.PID, nil }
	if _, err := r.Publish(second); err != nil {
		t.Fatalf("Publish(stale replacement): %v", err)
	}
	removed, err := r.Remove(first.BrokerSocketIdentity, first.Peer, first.InstanceNonce)
	if err != nil || removed {
		t.Fatalf("Remove(old nonce) = %v, %v; want false, nil", removed, err)
	}
	got, err := r.Load(second.BrokerSocketIdentity, second.Peer)
	if err != nil || got == nil || *got != second {
		t.Fatalf("replacement = %#v, %v; want %#v", got, err, second)
	}
}

func TestConcurrentPublishHasExactlyOneLiveWinner(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	r.processAlive = func(int) (bool, error) { return true, nil }
	base := validDescriptor(t, root)
	const publishers = 12
	registries := make([]*Registry, publishers)
	for i := range registries {
		var err error
		registries[i], err = New(r.Dir())
		if err != nil {
			t.Fatal(err)
		}
		registries[i].processAlive = func(int) (bool, error) { return true, nil }
	}
	start := make(chan struct{})
	type result struct {
		d   Descriptor
		err error
	}
	results := make(chan result, publishers)
	for i := 0; i < publishers; i++ {
		d := base
		d.PID = 1000 + i
		d.InstanceNonce = fmt.Sprintf("instance-%016d", i)
		d.ThreadID = fmt.Sprintf("thread-%d", i)
		go func(registry *Registry) {
			<-start
			_, err := registry.Publish(d)
			results <- result{d: d, err: err}
		}(registries[i])
	}
	close(start)
	var winner *Descriptor
	for i := 0; i < publishers; i++ {
		res := <-results
		switch {
		case res.err == nil:
			if winner != nil {
				t.Fatalf("multiple publishers succeeded: %#v and %#v", *winner, res.d)
			}
			winner = &res.d
		case errors.Is(res.err, ErrAlreadyLive):
		default:
			t.Fatalf("Publish() error = %v", res.err)
		}
	}
	if winner == nil {
		t.Fatal("no publisher succeeded")
	}
	got, err := r.Load(base.BrokerSocketIdentity, base.Peer)
	if err != nil || got == nil || *got != *winner {
		t.Fatalf("stored winner = %#v, %v; want %#v", got, err, *winner)
	}
}

func TestNonceRemovalCannotRaceStaleReplacement(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	first := validDescriptor(t, root)
	first.PID = 111
	if _, err := r.Publish(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.PID = 222
	second.InstanceNonce = "fedcba9876543210fedcba9876543210"
	second.ThreadID = "019-replacement"

	probeStarted := make(chan struct{})
	releaseProbe := make(chan struct{})
	var once sync.Once
	r.processAlive = func(pid int) (bool, error) {
		if pid == first.PID {
			once.Do(func() { close(probeStarted) })
			<-releaseProbe
			return false, nil
		}
		return true, nil
	}
	publishDone := make(chan error, 1)
	go func() {
		_, err := r.Publish(second)
		publishDone <- err
	}()
	select {
	case <-probeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("replacement never reached stale probe")
	}
	type removeResult struct {
		removed bool
		err     error
	}
	removeDone := make(chan removeResult, 1)
	go func() {
		removed, err := r.Remove(first.BrokerSocketIdentity, first.Peer, first.InstanceNonce)
		removeDone <- removeResult{removed: removed, err: err}
	}()
	close(releaseProbe)
	if err := <-publishDone; err != nil {
		t.Fatalf("Publish(replacement): %v", err)
	}
	removed := <-removeDone
	if removed.err != nil || removed.removed {
		t.Fatalf("concurrent Remove(old nonce) = %v, %v", removed.removed, removed.err)
	}
	got, err := r.Load(second.BrokerSocketIdentity, second.Peer)
	if err != nil || got == nil || *got != second {
		t.Fatalf("replacement after old cleanup = %#v, %v", got, err)
	}
}

func TestAtomicPublishNeverExposesPartialDescriptor(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	r.processAlive = func(int) (bool, error) { return true, nil }
	base := validDescriptor(t, root)
	if _, err := r.Publish(base); err != nil {
		t.Fatal(err)
	}
	writerDone := make(chan error, 1)
	go func() {
		for i := 0; i < 50; i++ {
			d := base
			d.ThreadID = fmt.Sprintf("thread-%d", i)
			if _, err := r.Publish(d); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()
	for {
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
			d, err := r.Load(base.BrokerSocketIdentity, base.Peer)
			if err != nil || d == nil {
				t.Fatalf("Load during publish = %#v, %v", d, err)
			}
			if d.InstanceNonce != base.InstanceNonce || !strings.HasPrefix(d.ThreadID, "thread-") && d.ThreadID != base.ThreadID {
				t.Fatalf("Load observed invalid descriptor: %#v", *d)
			}
		}
	}
}

func TestDifferentBrokerInstancesCanUseSamePeer(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	r.processAlive = func(int) (bool, error) { return true, nil }
	first := validDescriptor(t, root)
	second := first
	second.BrokerSocketIdentity = filepath.Join(root, "broker-two.sock")
	second.DownstreamUnixEndpoint = "unix://" + filepath.Join(root, "client-two.sock")
	second.InstanceNonce = "fedcba9876543210fedcba9876543210"
	if _, err := r.Publish(first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(second); err != nil {
		t.Fatal(err)
	}
	firstPath, _ := r.Path(first.BrokerSocketIdentity, first.Peer)
	secondPath, _ := r.Path(second.BrokerSocketIdentity, second.Peer)
	if firstPath == secondPath {
		t.Fatalf("different brokers shared path %q", firstPath)
	}
	for _, d := range []Descriptor{first, second} {
		got, err := r.Load(d.BrokerSocketIdentity, d.Peer)
		if err != nil || got == nil || *got != d {
			t.Fatalf("Load(%s) = %#v, %v", d.BrokerSocketIdentity, got, err)
		}
	}
}

func TestLoadStrictlyRejectsMalformedDescriptors(t *testing.T) {
	t.Parallel()
	baseRoot := t.TempDir()
	base := validDescriptor(t, baseRoot)
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := append([]byte(`{"peer":"duplicate",`), encoded[1:]...)
	unknown := append(append([]byte{}, encoded[:len(encoded)-1]...), []byte(`,"extra":true}`)...)
	caseMismatch := append(append([]byte{}, encoded[:len(encoded)-1]...), []byte(`,"Peer":"other"}`)...)
	future := base
	future.SchemaVersion++
	futureJSON, _ := json.Marshal(future)
	missing := base
	missing.ThreadID = ""
	missingJSON, _ := json.Marshal(missing)

	tests := []struct {
		name    string
		content []byte
		wantErr string
	}{
		{name: "malformed", content: []byte(`{`), wantErr: "decode"},
		{name: "not object", content: []byte(`[]`), wantErr: "JSON object"},
		{name: "unknown", content: unknown, wantErr: `unknown field "extra"`},
		{name: "case mismatch", content: caseMismatch, wantErr: `unknown field "Peer"`},
		{name: "duplicate", content: duplicate, wantErr: `duplicate field "peer"`},
		{name: "trailing", content: append(encoded, []byte(` {}`)...), wantErr: "unexpected data"},
		{name: "future schema", content: futureJSON, wantErr: "unsupported schema"},
		{name: "missing field", content: missingJSON, wantErr: "thread id is empty"},
		{name: "wrong type", content: []byte(`{"schemaVersion":"one"}`), wantErr: "cannot unmarshal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, root := newTestRegistry(t)
			d := validDescriptor(t, root)
			path, err := r.Path(d.BrokerSocketIdentity, d.Peer)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tt.content, 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := r.Load(d.BrokerSocketIdentity, d.Peer); got != nil || err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load() = %#v, %v; want error containing %q", got, err, tt.wantErr)
			}
		})
	}
}

func TestLoadRejectsDescriptorWithWrongKey(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"peer", "broker"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			r, root := newTestRegistry(t)
			key := validDescriptor(t, root)
			stored := key
			if field == "peer" {
				stored.Peer = "OtherPeer"
			} else {
				stored.BrokerSocketIdentity = filepath.Join(root, "other-broker.sock")
			}
			b, _ := json.Marshal(stored)
			path, _ := r.Path(key.BrokerSocketIdentity, key.Peer)
			if err := os.WriteFile(path, b, 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := r.Load(key.BrokerSocketIdentity, key.Peer); got != nil || err == nil || !strings.Contains(err.Error(), "does not match key") {
				t.Fatalf("Load() = %#v, %v", got, err)
			}
		})
	}
}

func TestLoadRejectsInsecureOrNonRegularDescriptor(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"mode", "symlink", "directory", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			r, root := newTestRegistry(t)
			d := validDescriptor(t, root)
			path, _ := r.Path(d.BrokerSocketIdentity, d.Peer)
			switch kind {
			case "mode":
				b, _ := json.Marshal(d)
				if err := os.WriteFile(path, b, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(root, "target.json")
				b, _ := json.Marshal(d)
				if err := os.WriteFile(target, b, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				if err := os.WriteFile(path, make([]byte, maxDescriptorSize+1), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := r.Load(d.BrokerSocketIdentity, d.Peer); got != nil || err == nil {
				t.Fatalf("Load(%s) = %#v, %v; want error", kind, got, err)
			}
		})
	}
}

func TestInvalidPublishPreservesExistingDescriptor(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	r.processAlive = func(int) (bool, error) { return true, nil }
	d := validDescriptor(t, root)
	if _, err := r.Publish(d); err != nil {
		t.Fatal(err)
	}
	invalid := d
	invalid.InstanceNonce = "another-valid-nonce"
	invalid.ThreadID = ""
	if _, err := r.Publish(invalid); err == nil {
		t.Fatal("Publish accepted invalid replacement")
	}
	got, err := r.Load(d.BrokerSocketIdentity, d.Peer)
	if err != nil || got == nil || *got != d {
		t.Fatalf("descriptor after invalid publish = %#v, %v", got, err)
	}
}

func TestPublishDoesNotDiscardCorruptExistingDescriptor(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	d := validDescriptor(t, root)
	path, err := r.Path(d.BrokerSocketIdentity, d.Peer)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"incomplete":`)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	r.processAlive = func(int) (bool, error) {
		t.Fatal("process probe called for corrupt descriptor")
		return false, nil
	}
	if _, err := r.Publish(d); err == nil || !strings.Contains(err.Error(), "decode descriptor") {
		t.Fatalf("Publish() error = %v, want decode error", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("corrupt descriptor changed to %q, want %q", got, want)
	}
}

func TestPublishPreservesOwnerWhenProcessProbeFails(t *testing.T) {
	t.Parallel()
	r, root := newTestRegistry(t)
	first := validDescriptor(t, root)
	if _, err := r.Publish(first); err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("injected process probe failure")
	r.processAlive = func(int) (bool, error) { return false, wantErr }
	replacement := first
	replacement.InstanceNonce = "fedcba9876543210fedcba9876543210"
	if _, err := r.Publish(replacement); !errors.Is(err, wantErr) {
		t.Fatalf("Publish() error = %v, want process probe failure", err)
	}
	r.processAlive = func(int) (bool, error) { return true, nil }
	got, err := r.Load(first.BrokerSocketIdentity, first.Peer)
	if err != nil || got == nil || *got != first {
		t.Fatalf("descriptor after probe failure = %#v, %v", got, err)
	}
}
