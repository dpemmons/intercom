package paths

import (
	"path/filepath"
	"testing"
)

func TestDirHonorsEnv(t *testing.T) {
	d := t.TempDir()
	t.Setenv("INTERCOM_DIR", d)
	got, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	if got != d {
		t.Fatalf("Dir = %q want %q", got, d)
	}
}

func TestSocketHonorsEnv(t *testing.T) {
	t.Setenv("INTERCOM_SOCKET", "/tmp/custom.sock")
	got, err := Socket()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/custom.sock" {
		t.Fatalf("Socket = %q", got)
	}
}

func TestLockDerivesFromSocket(t *testing.T) {
	t.Setenv("INTERCOM_SOCKET", "/tmp/x.sock")
	got, err := Lock()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/x.sock.lock" {
		t.Fatalf("Lock = %q", got)
	}
}

func TestLogHonorsEnv(t *testing.T) {
	t.Setenv("INTERCOM_BROKER_LOG", "/tmp/custom.log")
	got, err := Log()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/custom.log" {
		t.Fatalf("Log = %q", got)
	}
}

func TestDefaultsUseRuntimeDir(t *testing.T) {
	d := t.TempDir()
	t.Setenv("INTERCOM_DIR", d)
	t.Setenv("INTERCOM_SOCKET", "")
	t.Setenv("INTERCOM_BROKER_LOG", "")

	sock, _ := Socket()
	if sock != filepath.Join(d, sockName) {
		t.Errorf("Socket = %q", sock)
	}
	logp, _ := Log()
	if logp != filepath.Join(d, logName) {
		t.Errorf("Log = %q", logp)
	}
	lock, _ := Lock()
	if lock != filepath.Join(d, sockName+".lock") {
		t.Errorf("Lock = %q", lock)
	}
}
