package peername

import (
	"errors"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "from-env")

	got, err := Resolve("from-flag", "/tmp/from-cwd")
	if err != nil || got != "from-flag" {
		t.Fatalf("Resolve(explicit) = %q, %v", got, err)
	}
	got, err = Resolve("", "/tmp/from-cwd")
	if err != nil || got != "from-env" {
		t.Fatalf("Resolve(env) = %q, %v", got, err)
	}
	t.Setenv("INTERCOM_NAME", "")
	got, err = Resolve("", "/tmp/from-cwd")
	if err != nil || got != "from-cwd" {
		t.Fatalf("Resolve(cwd) = %q, %v", got, err)
	}
}

func TestResolveRejectsInvalidName(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "")
	_, err := Resolve("not valid", "/tmp/project")
	var invalid *InvalidError
	if !errors.As(err, &invalid) || invalid.Source != "--name" {
		t.Fatalf("Resolve() error = %#v", err)
	}
	want := `invalid peer name "not valid" from --name; allowed characters are ASCII letters, digits, '-', '_', up to 64 bytes`
	if err.Error() != want {
		t.Fatalf("Resolve() error = %q, want %q", err, want)
	}
}
