package codexinstance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validDescriptor(t *testing.T, root string) Descriptor {
	t.Helper()
	cwd, err := CanonicalCWD(filepath.Join(root, "project"))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := CanonicalBrokerSocket(filepath.Join(root, "broker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := CanonicalUnixEndpoint("unix://" + filepath.Join(root, "client.sock"))
	if err != nil {
		t.Fatal(err)
	}
	return Descriptor{
		SchemaVersion:          SchemaVersion,
		Peer:                   "PrologMotion",
		CWD:                    cwd,
		BrokerSocketIdentity:   broker,
		DownstreamUnixEndpoint: endpoint,
		ThreadID:               "019-thread-id",
		PID:                    os.Getpid(),
		InstanceNonce:          "0123456789abcdef0123456789abcdef",
		CodexVersion:           "0.144.1",
		ExecutionPolicy:        ExecutionWorkspaceWrite,
	}
}

func TestDescriptorValidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	good := validDescriptor(t, root)
	if err := good.Validate(); err != nil {
		t.Fatalf("valid descriptor: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Descriptor)
		wantErr string
	}{
		{name: "schema", mutate: func(d *Descriptor) { d.SchemaVersion++ }, wantErr: "unsupported schema"},
		{name: "peer empty", mutate: func(d *Descriptor) { d.Peer = "" }, wantErr: "invalid peer"},
		{name: "peer unsafe", mutate: func(d *Descriptor) { d.Peer = "../peer" }, wantErr: "invalid peer"},
		{name: "cwd relative", mutate: func(d *Descriptor) { d.CWD = "project" }, wantErr: "not canonical"},
		{name: "cwd unclean", mutate: func(d *Descriptor) { d.CWD += "/../project" }, wantErr: "not canonical"},
		{name: "broker relative", mutate: func(d *Descriptor) { d.BrokerSocketIdentity = "broker.sock" }, wantErr: "not canonical"},
		{name: "broker unclean", mutate: func(d *Descriptor) { d.BrokerSocketIdentity += "/../broker.sock" }, wantErr: "not canonical"},
		{name: "endpoint scheme", mutate: func(d *Descriptor) { d.DownstreamUnixEndpoint = "ws:///tmp/client.sock" }, wantErr: "scheme"},
		{name: "endpoint relative", mutate: func(d *Descriptor) { d.DownstreamUnixEndpoint = "unix:client.sock" }, wantErr: "opaque"},
		{name: "endpoint unclean", mutate: func(d *Descriptor) { d.DownstreamUnixEndpoint = "unix:///tmp/a/../client.sock" }, wantErr: "not canonical"},
		{name: "thread empty", mutate: func(d *Descriptor) { d.ThreadID = " " }, wantErr: "thread id is empty"},
		{name: "thread control", mutate: func(d *Descriptor) { d.ThreadID = "thread\nother" }, wantErr: "control"},
		{name: "pid", mutate: func(d *Descriptor) { d.PID = 0 }, wantErr: "pid must be positive"},
		{name: "nonce short", mutate: func(d *Descriptor) { d.InstanceNonce = "short" }, wantErr: "16..256"},
		{name: "nonce alphabet", mutate: func(d *Descriptor) { d.InstanceNonce = "0123456789abcde!" }, wantErr: "ASCII"},
		{name: "version empty", mutate: func(d *Descriptor) { d.CodexVersion = "" }, wantErr: "Codex version is empty"},
		{name: "version control", mutate: func(d *Descriptor) { d.CodexVersion = "0.1\n" }, wantErr: "control"},
		{name: "execution policy", mutate: func(d *Descriptor) { d.ExecutionPolicy = "other" }, wantErr: "execution policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := good
			tt.mutate(&d)
			if err := d.Validate(); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want text %q", err, tt.wantErr)
			}
		})
	}
}

func TestCanonicalIdentityHelpers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	want := filepath.Join(root, "socket.sock")
	got, err := CanonicalBrokerSocket(filepath.Join(root, "sub", "..", "socket.sock"))
	if err != nil || got != want {
		t.Fatalf("CanonicalBrokerSocket() = %q, %v; want %q", got, err, want)
	}
	cwd, err := CanonicalCWD(filepath.Join(root, "project", "."))
	if err != nil || cwd != filepath.Join(root, "project") {
		t.Fatalf("CanonicalCWD() = %q, %v", cwd, err)
	}
	endpoint, err := CanonicalUnixEndpoint("unix:///tmp/codex%20socket/../client.sock")
	if err != nil || endpoint != "unix:///tmp/client.sock" {
		t.Fatalf("CanonicalUnixEndpoint() = %q, %v", endpoint, err)
	}

	for _, value := range []string{"", "tcp:///tmp/x", "unix://host/tmp/x", "unix:///tmp/x?query=1", "unix:///tmp/x#fragment"} {
		if got, err := CanonicalUnixEndpoint(value); err == nil {
			t.Errorf("CanonicalUnixEndpoint(%q) = %q, want error", value, got)
		}
	}
}

func TestNewNonce(t *testing.T) {
	t.Parallel()
	first, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("two NewNonce calls collided: %q", first)
	}
	if len(first) != 32 || strings.ToLower(first) != first {
		t.Fatalf("NewNonce() = %q, want 32 lowercase hexadecimal characters", first)
	}
	d := validDescriptor(t, t.TempDir())
	d.InstanceNonce = first
	if err := d.Validate(); err != nil {
		t.Fatalf("generated nonce is invalid: %v", err)
	}
}

func TestCanonicalPathsRejectEmptyAndNUL(t *testing.T) {
	t.Parallel()
	for name, fn := range map[string]func(string) (string, error){
		"cwd":    CanonicalCWD,
		"broker": CanonicalBrokerSocket,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for _, input := range []string{"", "bad\x00path"} {
				if got, err := fn(input); err == nil {
					t.Errorf("helper(%q) = %q, want error", input, got)
				}
			}
		})
	}
}

func TestStaleErrorClassification(t *testing.T) {
	t.Parallel()
	d := validDescriptor(t, t.TempDir())
	err := &StaleError{Descriptor: d}
	if !errors.Is(err, ErrStale) {
		t.Fatalf("errors.Is(%v, ErrStale) = false", err)
	}
	var got *StaleError
	if !errors.As(err, &got) || got.Descriptor != d {
		t.Fatalf("errors.As() = %#v", got)
	}
}
