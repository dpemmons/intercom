// Package codexinstance publishes and discovers live, attachable Codex
// instances.
//
// A registry is scoped to a private runtime directory. Descriptor filenames
// are derived from both the canonical Intercom broker socket and the validated
// peer name, so independent brokers and peers can safely share one registry.
// Publish is a cross-process claim operation: a different live owner is never
// overwritten, while an owner whose PID no longer exists is replaced. A
// repeated publish with the same instance nonce is an idempotent owner update.
//
// Remove is similarly ownership-aware. It removes a descriptor only while its
// nonce still matches, under the same lock used by Publish. Cleanup from an old
// instance therefore cannot unlink a concurrently published replacement.
package codexinstance

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/dpemmons/intercom/internal/wire"
)

// SchemaVersion is the only descriptor schema this package currently accepts.
const SchemaVersion = 2

type ExecutionPolicy string

const (
	ExecutionWorkspaceWrite   ExecutionPolicy = "workspace-write"
	ExecutionDangerFullAccess ExecutionPolicy = "danger-full-access"
)

const (
	minNonceLength    = 16
	maxNonceLength    = 256
	maxIdentityLength = 4096
)

// Descriptor is the complete discovery record for one live Codex instance.
// CWD and BrokerSocketIdentity are canonical absolute filesystem paths.
// DownstreamUnixEndpoint is the canonical unix:///absolute/path form.
type Descriptor struct {
	SchemaVersion          int             `json:"schemaVersion"`
	Peer                   string          `json:"peer"`
	CWD                    string          `json:"cwd"`
	BrokerSocketIdentity   string          `json:"brokerSocketIdentity"`
	DownstreamUnixEndpoint string          `json:"downstreamUnixEndpoint"`
	ThreadID               string          `json:"threadId"`
	PID                    int             `json:"pid"`
	InstanceNonce          string          `json:"instanceNonce"`
	CodexVersion           string          `json:"codexVersion"`
	ExecutionPolicy        ExecutionPolicy `json:"executionPolicy"`
}

// Validate checks descriptor compatibility, identity fields, and canonical
// path/endpoint representation. It does not probe the process or either Unix
// socket; those are lifecycle concerns handled by the publisher and attacher.
func (d Descriptor) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("codex instance descriptor: unsupported schema version %d (want %d)", d.SchemaVersion, SchemaVersion)
	}
	if !wire.ValidName(d.Peer) {
		return fmt.Errorf("codex instance descriptor: invalid peer %q", d.Peer)
	}

	cwd, err := CanonicalCWD(d.CWD)
	if err != nil {
		return fmt.Errorf("codex instance descriptor: cwd: %w", err)
	}
	if cwd != d.CWD {
		return fmt.Errorf("codex instance descriptor: cwd %q is not canonical (want %q)", d.CWD, cwd)
	}

	broker, err := CanonicalBrokerSocket(d.BrokerSocketIdentity)
	if err != nil {
		return fmt.Errorf("codex instance descriptor: broker socket identity: %w", err)
	}
	if broker != d.BrokerSocketIdentity {
		return fmt.Errorf("codex instance descriptor: broker socket identity %q is not canonical (want %q)", d.BrokerSocketIdentity, broker)
	}

	endpoint, err := CanonicalUnixEndpoint(d.DownstreamUnixEndpoint)
	if err != nil {
		return fmt.Errorf("codex instance descriptor: downstream Unix endpoint: %w", err)
	}
	if endpoint != d.DownstreamUnixEndpoint {
		return fmt.Errorf("codex instance descriptor: downstream Unix endpoint %q is not canonical (want %q)", d.DownstreamUnixEndpoint, endpoint)
	}

	if err := validateText("thread id", d.ThreadID, maxIdentityLength); err != nil {
		return err
	}
	if d.PID <= 0 {
		return fmt.Errorf("codex instance descriptor: pid must be positive, got %d", d.PID)
	}
	if err := validateNonce(d.InstanceNonce); err != nil {
		return err
	}
	if err := validateText("Codex version", d.CodexVersion, maxIdentityLength); err != nil {
		return err
	}
	switch d.ExecutionPolicy {
	case ExecutionWorkspaceWrite, ExecutionDangerFullAccess:
	default:
		return fmt.Errorf("codex instance descriptor: invalid execution policy %q", d.ExecutionPolicy)
	}
	return nil
}

// CanonicalCWD resolves path against the current working directory and returns
// its clean absolute spelling. It intentionally does not resolve symlinks: the
// Codex thread identity uses the lexical absolute cwd supplied at startup.
func CanonicalCWD(path string) (string, error) {
	return canonicalAbsolutePath("cwd", path)
}

// CanonicalBrokerSocket resolves path against the current working directory
// and returns the clean absolute socket identity used in descriptor keys.
func CanonicalBrokerSocket(path string) (string, error) {
	return canonicalAbsolutePath("broker socket", path)
}

func canonicalAbsolutePath(label, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%s path is empty", label)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", fmt.Errorf("%s path contains NUL", label)
	}
	if len(path) > maxIdentityLength {
		return "", fmt.Errorf("%s path exceeds %d bytes", label, maxIdentityLength)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path: %w", label, err)
	}
	return filepath.Clean(abs), nil
}

// CanonicalUnixEndpoint validates endpoint and returns its normalized
// unix:///absolute/path spelling. Host, user info, query, and fragment
// components are forbidden.
func CanonicalUnixEndpoint(endpoint string) (string, error) {
	if endpoint == "" {
		return "", errors.New("endpoint is empty")
	}
	if len(endpoint) > maxIdentityLength {
		return "", fmt.Errorf("endpoint exceeds %d bytes", maxIdentityLength)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse endpoint: %w", err)
	}
	if u.Scheme != "unix" {
		return "", fmt.Errorf("endpoint scheme must be unix, got %q", u.Scheme)
	}
	if u.Opaque != "" || u.Host != "" || u.User != nil {
		return "", errors.New("Unix endpoint must not contain an opaque path, host, or user info")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Unix endpoint must not contain a query or fragment")
	}
	path, err := url.PathUnescape(u.EscapedPath())
	if err != nil {
		return "", fmt.Errorf("decode Unix socket path: %w", err)
	}
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("Unix socket path must be absolute: %q", path)
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("Unix socket path contains NUL")
	}
	path = filepath.Clean(path)
	return (&url.URL{Scheme: "unix", Path: path}).String(), nil
}

// NewNonce returns a cryptographically random, 128-bit instance nonce in
// lowercase hexadecimal form.
func NewNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("codex instance: generate nonce: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func validateText(label, value string, max int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("codex instance descriptor: %s is empty", label)
	}
	if len(value) > max {
		return fmt.Errorf("codex instance descriptor: %s exceeds %d bytes", label, max)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("codex instance descriptor: %s contains a control character", label)
		}
	}
	return nil
}

func validateNonce(nonce string) error {
	if len(nonce) < minNonceLength || len(nonce) > maxNonceLength {
		return fmt.Errorf("codex instance descriptor: instance nonce must be %d..%d bytes", minNonceLength, maxNonceLength)
	}
	for _, r := range nonce {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return errors.New("codex instance descriptor: instance nonce must use only ASCII letters, digits, '-' or '_'")
		}
	}
	return nil
}
