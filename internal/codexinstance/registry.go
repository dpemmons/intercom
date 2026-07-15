package codexinstance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/dpemmons/intercom/internal/wire"
)

const (
	registryLockName  = ".registry.lock"
	maxDescriptorSize = 64 * 1024
)

var (
	// ErrAlreadyLive identifies a Publish failure caused by a different
	// descriptor owner whose recorded PID still exists.
	ErrAlreadyLive = errors.New("codex instance is already live")

	// ErrStale identifies a descriptor that is structurally valid but whose
	// recorded owner PID no longer exists.
	ErrStale = errors.New("codex instance descriptor is stale")
)

// AlreadyLiveError reports the descriptor that prevented a new owner from
// claiming the same broker-and-peer key.
type AlreadyLiveError struct {
	Existing Descriptor
}

func (e *AlreadyLiveError) Error() string {
	return fmt.Sprintf("codex instance %q is already live as pid %d", e.Existing.Peer, e.Existing.PID)
}

func (e *AlreadyLiveError) Unwrap() error { return ErrAlreadyLive }

// StaleError reports the no-longer-live descriptor found by Load. Callers can
// use errors.As to include its PID or endpoint in a diagnostic. Publish, rather
// than Load, owns stale-record replacement.
type StaleError struct {
	Descriptor Descriptor
}

func (e *StaleError) Error() string {
	return fmt.Sprintf("codex instance %q descriptor is stale (pid %d does not exist)", e.Descriptor.Peer, e.Descriptor.PID)
}

func (e *StaleError) Unwrap() error { return ErrStale }

// Registry stores live descriptors in one mode-0700 directory. Registry
// values are safe for concurrent goroutines and cooperating processes.
type Registry struct {
	dir          string
	processAlive func(int) (bool, error)
	syncDir      func(string) error
}

// New creates or opens liveDir and forces its mode to 0700. The final path
// component must be a real directory, not a symlink. Later operations reject
// a directory whose type or permissions have changed.
func New(liveDir string) (*Registry, error) {
	if liveDir == "" {
		return nil, errors.New("codex instance registry: live directory is empty")
	}
	abs, err := filepath.Abs(liveDir)
	if err != nil {
		return nil, fmt.Errorf("codex instance registry: resolve live directory: %w", err)
	}
	abs = filepath.Clean(abs)
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("codex instance registry: create live directory: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("codex instance registry: inspect live directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("codex instance registry: live path %q is not a real directory", abs)
	}
	if err := os.Chmod(abs, 0o700); err != nil {
		return nil, fmt.Errorf("codex instance registry: secure live directory: %w", err)
	}
	return &Registry{dir: abs, processAlive: processExists, syncDir: syncDirectory}, nil
}

// Dir returns the canonical registry directory.
func (r *Registry) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

// Path returns the deterministic descriptor path for brokerSocket and peer.
// The full SHA-256 digest of the canonical broker-and-peer key makes
// cross-broker filename collisions negligible; the validated peer prefix keeps
// directory listings intelligible.
func (r *Registry) Path(brokerSocket, peer string) (string, error) {
	if err := r.ready(); err != nil {
		return "", err
	}
	if !wire.ValidName(peer) {
		return "", fmt.Errorf("codex instance registry: invalid peer %q", peer)
	}
	broker, err := CanonicalBrokerSocket(brokerSocket)
	if err != nil {
		return "", fmt.Errorf("codex instance registry: broker socket: %w", err)
	}
	sum := sha256.Sum256([]byte(broker + "\x00" + peer))
	name := peer + "-" + hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(r.dir, name), nil
}

// Publish atomically claims or updates d's broker-and-peer key and returns its
// descriptor path. It behaves as follows while holding a cross-process lock:
//
//   - no prior descriptor: publish d;
//   - the same instance nonce: atomically update the owner's descriptor;
//   - a different nonce whose PID no longer exists: replace the stale record;
//   - a different nonce whose PID exists: return ErrAlreadyLive unchanged.
//
// A malformed or insecure prior descriptor is never silently discarded.
// If publication reaches rename but the following directory sync fails,
// Publish removes the renamed descriptor only while d's nonce still owns it.
func (r *Registry) Publish(d Descriptor) (string, error) {
	if err := r.ready(); err != nil {
		return "", err
	}
	if err := d.Validate(); err != nil {
		return "", err
	}
	path, err := r.Path(d.BrokerSocketIdentity, d.Peer)
	if err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return "", fmt.Errorf("codex instance registry: encode descriptor: %w", err)
	}
	b = append(b, '\n')

	err = r.withLock(func() error {
		existing, err := r.loadPath(path, d.BrokerSocketIdentity, d.Peer)
		if err != nil {
			return err
		}
		if existing != nil && existing.InstanceNonce != d.InstanceNonce {
			alive, err := r.processAlive(existing.PID)
			if err != nil {
				return fmt.Errorf("codex instance registry: probe pid %d: %w", existing.PID, err)
			}
			if alive {
				return &AlreadyLiveError{Existing: *existing}
			}
		}
		return r.writeAtomic(path, b, d.BrokerSocketIdentity, d.Peer, d.InstanceNonce)
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// Load returns the strictly validated, live descriptor for brokerSocket and
// peer. It returns nil, nil when no descriptor currently exists and ErrStale
// when a valid descriptor's PID no longer exists. Loads need no lock: Publish
// uses rename, so a reader observes either a complete old descriptor or a
// complete new one.
func (r *Registry) Load(brokerSocket, peer string) (*Descriptor, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	path, err := r.Path(brokerSocket, peer)
	if err != nil {
		return nil, err
	}
	broker, err := CanonicalBrokerSocket(brokerSocket)
	if err != nil {
		return nil, err
	}
	d, err := r.loadPath(path, broker, peer)
	if err != nil || d == nil {
		return d, err
	}
	alive, err := r.processAlive(d.PID)
	if err != nil {
		return nil, fmt.Errorf("codex instance registry: probe pid %d: %w", d.PID, err)
	}
	if !alive {
		return nil, &StaleError{Descriptor: *d}
	}
	return d, nil
}

// Remove deletes the descriptor only if nonce still owns it. The bool reports
// whether a file was removed. A missing descriptor or a nonce mismatch returns
// false, nil, making shutdown cleanup idempotent and safe after stale-record
// replacement. The nonce comparison and unlink occur under the Publish lock.
func (r *Registry) Remove(brokerSocket, peer, nonce string) (bool, error) {
	if err := r.ready(); err != nil {
		return false, err
	}
	if err := validateNonce(nonce); err != nil {
		return false, err
	}
	path, err := r.Path(brokerSocket, peer)
	if err != nil {
		return false, err
	}
	broker, err := CanonicalBrokerSocket(brokerSocket)
	if err != nil {
		return false, err
	}
	removed := false
	err = r.withLock(func() error {
		d, err := r.loadPath(path, broker, peer)
		if err != nil {
			return err
		}
		if d == nil || d.InstanceNonce != nonce {
			return nil
		}
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("codex instance registry: remove descriptor: %w", err)
		}
		removed = true
		if err := r.syncDir(r.dir); err != nil {
			return fmt.Errorf("codex instance registry: sync live directory after remove: %w", err)
		}
		return nil
	})
	return removed, err
}

func (r *Registry) ready() error {
	if r == nil || r.dir == "" || r.processAlive == nil || r.syncDir == nil {
		return errors.New("codex instance registry: uninitialized registry")
	}
	info, err := os.Lstat(r.dir)
	if err != nil {
		return fmt.Errorf("codex instance registry: inspect live directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("codex instance registry: live path %q is not a real directory", r.dir)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("codex instance registry: live directory %q has mode %04o, want 0700", r.dir, info.Mode().Perm())
	}
	return nil
}

func (r *Registry) loadPath(path, broker, peer string) (*Descriptor, error) {
	linfo, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex instance registry: inspect descriptor: %w", err)
	}
	if linfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("codex instance registry: descriptor %q is a symlink", path)
	}
	if !linfo.Mode().IsRegular() {
		return nil, fmt.Errorf("codex instance registry: descriptor %q is not a regular file", path)
	}
	if linfo.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("codex instance registry: descriptor %q has mode %04o, want 0600", path, linfo.Mode().Perm())
	}

	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex instance registry: open descriptor: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("codex instance registry: inspect open descriptor: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("codex instance registry: descriptor %q is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("codex instance registry: descriptor %q has mode %04o, want 0600", path, info.Mode().Perm())
	}
	if info.Size() <= 0 || info.Size() > maxDescriptorSize {
		return nil, fmt.Errorf("codex instance registry: descriptor size %d is outside 1..%d bytes", info.Size(), maxDescriptorSize)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxDescriptorSize+1))
	if err != nil {
		return nil, fmt.Errorf("codex instance registry: read descriptor: %w", err)
	}
	if len(b) > maxDescriptorSize {
		return nil, fmt.Errorf("codex instance registry: descriptor exceeds %d bytes", maxDescriptorSize)
	}
	d, err := decodeDescriptor(b)
	if err != nil {
		return nil, err
	}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if d.Peer != peer {
		return nil, fmt.Errorf("codex instance registry: descriptor peer %q does not match key %q", d.Peer, peer)
	}
	if d.BrokerSocketIdentity != broker {
		return nil, fmt.Errorf("codex instance registry: descriptor broker %q does not match key %q", d.BrokerSocketIdentity, broker)
	}
	return &d, nil
}

func decodeDescriptor(b []byte) (Descriptor, error) {
	if err := validateJSONKeys(b); err != nil {
		return Descriptor{}, fmt.Errorf("codex instance registry: decode descriptor: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d Descriptor
	if err := dec.Decode(&d); err != nil {
		return Descriptor{}, fmt.Errorf("codex instance registry: decode descriptor: %w", err)
	}
	if err := requireJSONEOF(dec); err != nil {
		return Descriptor{}, fmt.Errorf("codex instance registry: decode descriptor: %w", err)
	}
	return d, nil
}

func validateJSONKeys(b []byte) error {
	allowed := map[string]struct{}{
		"schemaVersion":          {},
		"peer":                   {},
		"cwd":                    {},
		"brokerSocketIdentity":   {},
		"downstreamUnixEndpoint": {},
		"threadId":               {},
		"pid":                    {},
		"instanceNonce":          {},
		"codexVersion":           {},
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errors.New("descriptor must be a JSON object")
	}
	seen := make(map[string]struct{}, len(allowed))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("descriptor object key is not a string")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return err
	}
	return requireJSONEOF(dec)
}

func requireJSONEOF(dec *json.Decoder) error {
	var trailing json.RawMessage
	err := dec.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	return errors.New("unexpected data after descriptor object")
}

func (r *Registry) writeAtomic(path string, b []byte, broker, peer, nonce string) error {
	tmp, err := os.CreateTemp(r.dir, ".instance-*")
	if err != nil {
		return fmt.Errorf("codex instance registry: create temporary descriptor: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex instance registry: chmod temporary descriptor: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex instance registry: write temporary descriptor: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("codex instance registry: sync temporary descriptor: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("codex instance registry: close temporary descriptor: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("codex instance registry: publish descriptor: %w", err)
	}
	if err := r.syncDir(r.dir); err != nil {
		publishErr := fmt.Errorf("codex instance registry: sync live directory after publish: %w", err)
		cleanupErr := r.removeOwnedPath(path, broker, peer, nonce)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("codex instance registry: clean up failed publication: %w", cleanupErr)
		}
		return errors.Join(publishErr, cleanupErr)
	}
	return nil
}

// removeOwnedPath runs while the registry lock is held. It intentionally
// re-reads the descriptor after publication: a non-cooperating writer that
// replaced the path with another nonce must not lose its record to cleanup.
func (r *Registry) removeOwnedPath(path, broker, peer, nonce string) error {
	d, err := r.loadPath(path, broker, peer)
	if err != nil {
		return err
	}
	if d == nil || d.InstanceNonce != nonce {
		return nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove descriptor: %w", err)
	}
	if err := r.syncDir(r.dir); err != nil {
		return fmt.Errorf("sync live directory after cleanup: %w", err)
	}
	return nil
}

func (r *Registry) withLock(fn func() error) (retErr error) {
	lockPath := filepath.Join(r.dir, registryLockName)
	if info, err := os.Lstat(lockPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("codex instance registry: lock path %q is not a regular file", lockPath)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("codex instance registry: inspect lock: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("codex instance registry: open lock: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("codex instance registry: chmod lock: %w", err)
	}
	if err := flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return fmt.Errorf("codex instance registry: acquire lock: %w", err)
	}
	defer func() {
		unlockErr := flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		retErr = errors.Join(retErr, unlockErr, closeErr)
	}()
	return fn()
}

func flock(fd int, how int) error {
	for {
		err := syscall.Flock(fd, how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

func processExists(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	switch {
	case err == nil, errors.Is(err, syscall.EPERM):
		return true, nil
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	default:
		return false, err
	}
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	if runtime.GOOS == "darwin" && (errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP)) {
		syncErr = nil
	}
	return errors.Join(syncErr, dir.Close())
}
