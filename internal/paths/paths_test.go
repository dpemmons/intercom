package paths

import (
	"os"
	"path/filepath"
	"strings"
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

func TestCodexPaths(t *testing.T) {
	t.Setenv("INTERCOM_DIR", t.TempDir())

	dir, err := CodexDir()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("CodexDir() mode = %v, want directory 0700", info.Mode())
	}

	state, err := CodexState("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "reviewer.json"); state != want {
		t.Fatalf("CodexState() = %q, want %q", state, want)
	}
	lock, err := CodexLock("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "reviewer.lock"); lock != want {
		t.Fatalf("CodexLock() = %q, want %q", lock, want)
	}
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	threadLock, err := CodexThreadLock(codexHome, "019-thread")
	if err != nil {
		t.Fatal(err)
	}
	again, err := CodexThreadLock(codexHome, "019-thread")
	if err != nil {
		t.Fatal(err)
	}
	other, err := CodexThreadLock(codexHome, "020-thread")
	if err != nil {
		t.Fatal(err)
	}
	if threadLock != again || threadLock == other || filepath.Dir(threadLock) != filepath.Join(codexHome, ".intercom", "thread-locks") {
		t.Fatalf("CodexThreadLock() paths = %q, %q, %q", threadLock, again, other)
	}

	t.Setenv("INTERCOM_DIR", t.TempDir())
	acrossIntercomDirs, err := CodexThreadLock(codexHome, "019-thread")
	if err != nil {
		t.Fatal(err)
	}
	if acrossIntercomDirs != threadLock {
		t.Fatalf("CodexThreadLock() changed with INTERCOM_DIR: %q, want %q", acrossIntercomDirs, threadLock)
	}
}

func TestCodexThreadLockRejectsIncompleteIdentity(t *testing.T) {
	for _, test := range []struct {
		name      string
		codexHome string
		threadID  string
		wantError string
	}{
		{name: "empty home", threadID: "thread", wantError: "requires CODEX_HOME"},
		{name: "empty thread", codexHome: filepath.Join(t.TempDir(), "codex-home"), wantError: "requires CODEX_HOME"},
		{name: "relative home", codexHome: "codex-home", threadID: "thread", wantError: "must be absolute"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CodexThreadLock(test.codexHome, test.threadID); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("CodexThreadLock() error = %v, want fragment %q", err, test.wantError)
			}
		})
	}
}
