package codexsession

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dpemmons/intercom/internal/appserver"
)

func pickerCandidates() []Candidate {
	first := thread("full-session-id-1", "/project/one", "cli", 1)
	first.Preview = "first\n\x1b[31mred"
	second := thread("full-session-id-2", "/project/two", "vscode", 2)
	second.Preview = "second"
	return []Candidate{
		{Thread: first, Source: appserver.ThreadSourceCLI},
		{Thread: second, Source: appserver.ThreadSourceVSCode},
	}
}

func TestPickDisplaysSafeCompleteMetadataAndRetries(t *testing.T) {
	var output bytes.Buffer
	selected, err := Pick(strings.NewReader("bad\n3\n2\n"), &output, pickerCandidates())
	if err != nil {
		t.Fatal(err)
	}
	if selected.Thread.ID != "full-session-id-2" {
		t.Fatalf("selected = %+v", selected)
	}
	text := output.String()
	for _, want := range []string{
		"full-session-id-1", "full-session-id-2", "/project/one", "/project/two",
		"[cli]", "[vscode]", "Enter a number from 1 to 2, or q.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("picker output does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "\x1b") || strings.Contains(text, "first\n") {
		t.Fatalf("picker output contains terminal control or metadata newline:\n%s", text)
	}
}

func TestPickCancellationAndEmptySet(t *testing.T) {
	for name, input := range map[string]string{"q": "q\n", "uppercase": "Q\n", "eof": ""} {
		t.Run(name, func(t *testing.T) {
			_, err := Pick(strings.NewReader(input), io.Discard, pickerCandidates())
			if !errors.Is(err, ErrCanceled) {
				t.Fatalf("Pick error = %v", err)
			}
		})
	}
	if _, err := Pick(strings.NewReader("1\n"), io.Discard, nil); !errors.Is(err, ErrNoCandidates) {
		t.Fatalf("empty Pick error = %v", err)
	}
}

func TestPickAcceptsFinalLineWithoutNewline(t *testing.T) {
	selected, err := Pick(strings.NewReader("1"), io.Discard, pickerCandidates())
	if err != nil || selected.Thread.ID != "full-session-id-1" {
		t.Fatalf("Pick = (%+v, %v)", selected, err)
	}
}

func TestPickRejectsNilIO(t *testing.T) {
	if _, err := Pick(nil, io.Discard, pickerCandidates()); err == nil {
		t.Fatal("Pick with nil input succeeded")
	}
	if _, err := Pick(strings.NewReader("1\n"), nil, pickerCandidates()); err == nil {
		t.Fatal("Pick with nil output succeeded")
	}
}

func TestSanitizeDisplay(t *testing.T) {
	input := "  alpha\n\t\x1b[31m beta\u202e  "
	got := SanitizeDisplay(input, 0)
	if got != "alpha �[31m beta�" {
		t.Fatalf("SanitizeDisplay = %q", got)
	}
	if got := SanitizeDisplay("abcdef", 4); got != "abc…" {
		t.Fatalf("truncated = %q", got)
	}
	if got := SanitizeDisplay("ab", 1); got != "…" {
		t.Fatalf("one-rune truncated = %q", got)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestPickReportsOutputFailure(t *testing.T) {
	if _, err := Pick(strings.NewReader("1\n"), failingWriter{}, pickerCandidates()); err == nil || !strings.Contains(err.Error(), "write picker") {
		t.Fatalf("Pick error = %v", err)
	}
}

func TestPickBoundsInputLine(t *testing.T) {
	_, err := Pick(strings.NewReader(strings.Repeat("1", 5<<10)), io.Discard, pickerCandidates())
	if err == nil || !strings.Contains(err.Error(), "read picker") {
		t.Fatalf("oversized input error = %v", err)
	}
}
