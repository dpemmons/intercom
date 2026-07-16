//go:build unix

package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestCodexAppServerExecCmdCreatesSessionAndExecsExactArguments(t *testing.T) {
	var calls []string
	var gotPath string
	var gotArgs []string
	var gotEnv []string
	readyFile := filepath.Join(t.TempDir(), "session.ready")
	cmd := newCodexAppServerExecCmdWithOps(codexAppServerExecOps{
		lookPath: func(name string) (string, error) {
			calls = append(calls, "lookPath")
			if name != "codex-wrapper" {
				t.Fatalf("lookPath name = %q", name)
			}
			return "/resolved/codex-wrapper", nil
		},
		setsid: func() (int, error) {
			calls = append(calls, "setsid")
			return 1234, nil
		},
		writePID: func(path string, pid int) error {
			calls = append(calls, "writePID")
			if path != readyFile || pid != os.Getpid() {
				t.Fatalf("writePID(%q, %d), want (%q, %d)", path, pid, readyFile, os.Getpid())
			}
			return nil
		},
		exec: func(path string, args, env []string) error {
			calls = append(calls, "exec")
			gotPath = path
			gotArgs = append([]string(nil), args...)
			gotEnv = append([]string(nil), env...)
			return nil
		},
	})
	cmd.SetArgs([]string{"--ready-file", readyFile, "--", "codex-wrapper", "app-server", "--listen", "unix:///tmp/app.sock"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	if !cmd.Hidden {
		t.Fatal("command is not hidden")
	}
	if want := []string{"lookPath", "setsid", "writePID", "exec"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %q, want %q", calls, want)
	}
	if gotPath != "/resolved/codex-wrapper" {
		t.Fatalf("exec path = %q", gotPath)
	}
	if want := []string{"codex-wrapper", "app-server", "--listen", "unix:///tmp/app.sock"}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("exec args = %q, want %q", gotArgs, want)
	}
	if len(gotEnv) == 0 {
		t.Fatal("exec environment is empty")
	}
}

func TestCodexAppServerExecCmdReportsStageErrors(t *testing.T) {
	stageErr := errors.New("stage failed")
	tests := []struct {
		name    string
		ops     codexAppServerExecOps
		want    string
		wantRun []string
	}{
		{
			name: "lookup",
			ops: codexAppServerExecOps{
				lookPath: func(string) (string, error) { return "", stageErr },
				setsid:   func() (int, error) { t.Fatal("setsid called"); return 0, nil },
				writePID: func(string, int) error { t.Fatal("writePID called"); return nil },
				exec:     func(string, []string, []string) error { t.Fatal("exec called"); return nil },
			},
			want: "resolve Codex executable",
		},
		{
			name: "setsid",
			ops: codexAppServerExecOps{
				lookPath: func(string) (string, error) { return "/codex", nil },
				setsid:   func() (int, error) { return 0, stageErr },
				writePID: func(string, int) error { t.Fatal("writePID called"); return nil },
				exec:     func(string, []string, []string) error { t.Fatal("exec called"); return nil },
			},
			want: "create app-server process session",
		},
		{
			name: "publish",
			ops: codexAppServerExecOps{
				lookPath: func(string) (string, error) { return "/codex", nil },
				setsid:   func() (int, error) { return 1234, nil },
				writePID: func(string, int) error { return stageErr },
				exec:     func(string, []string, []string) error { t.Fatal("exec called"); return nil },
			},
			want: "publish app-server process session",
		},
		{
			name: "exec",
			ops: codexAppServerExecOps{
				lookPath: func(string) (string, error) { return "/codex", nil },
				setsid:   func() (int, error) { return 1234, nil },
				writePID: func(string, int) error { return nil },
				exec:     func(string, []string, []string) error { return stageErr },
			},
			want: "exec Codex app-server",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newCodexAppServerExecCmdWithOps(tt.ops)
			cmd.SetArgs([]string{"--ready-file", filepath.Join(t.TempDir(), "ready"), "--", "codex", "app-server"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.want) || !errors.Is(err, stageErr) {
				t.Fatalf("error = %v, want stage %q wrapping %v", err, tt.want, stageErr)
			}
		})
	}
}

func TestWriteSessionPIDPublishesCompleteOwnerOnlyMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-server.session")
	oldUmask := syscall.Umask(0o777)
	err := writeSessionPID(path, 12345)
	syscall.Umask(oldUmask)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "12345\n"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("marker mode = %04o, want 0600", got)
	}
	if err := writeSessionPID(path, 67890); err == nil {
		t.Fatal("second marker publication succeeded")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "12345\n"; got != want {
		t.Fatalf("marker after duplicate publish = %q, want %q", got, want)
	}
}
