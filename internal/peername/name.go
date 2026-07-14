// Package peername resolves and validates Intercom peer identities.
package peername

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dpemmons/intercom/internal/wire"
)

// Resolve applies CLI precedence: explicit name, INTERCOM_NAME, then the
// basename of cwd. If cwd is empty, the process working directory is used.
func Resolve(explicit, cwd string) (string, error) {
	if name := strings.TrimSpace(explicit); name != "" {
		return validate(name, "--name")
	}
	if name := strings.TrimSpace(os.Getenv("INTERCOM_NAME")); name != "" {
		return validate(name, "INTERCOM_NAME")
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("peer name: get working directory: %w", err)
		}
	}
	return validate(filepath.Base(filepath.Clean(cwd)), "cwd basename")
}

func validate(name, source string) (string, error) {
	if !wire.ValidName(name) {
		return "", &InvalidError{Name: name, Source: source}
	}
	return name, nil
}

type InvalidError struct {
	Name   string
	Source string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("invalid peer name %q from %s; allowed characters are ASCII letters, digits, '-', '_', up to %d bytes", e.Name, e.Source, wire.MaxNameLen)
}
