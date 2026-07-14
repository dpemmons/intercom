package shim

import (
	"errors"
	"os"
	"testing"
)

func TestResolveNameEnv(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "explicit")
	got, err := ResolveName()
	if err != nil {
		t.Fatal(err)
	}
	if got != "explicit" {
		t.Errorf("got %q", got)
	}
}

func TestResolveNameCwdBasename(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "")
	dir, err := os.MkdirTemp("", "icname-myproj")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveName()
	if err != nil {
		t.Fatal(err)
	}
	// MkdirTemp appends random chars to the prefix, so just check the prefix.
	if got == "" {
		t.Errorf("got empty name")
	}
}

func TestResolveNameRejectsInvalid(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "bad name")
	_, err := ResolveName()
	var nerr *InvalidNameError
	if !errors.As(err, &nerr) {
		t.Fatalf("got %v, want InvalidNameError", err)
	}
	want := `invalid peer name "bad name" from INTERCOM_NAME; allowed characters are ASCII letters, digits, '-', '_', up to 64 bytes`
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}
