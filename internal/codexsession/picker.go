package codexsession

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

var ErrCanceled = errors.New("codexsession: selection canceled")

// Pick writes a numbered session list and reads a selection. Terminal
// detection and the choice of output stream belong to the caller.
func Pick(in io.Reader, out io.Writer, candidates []Candidate) (Candidate, error) {
	if len(candidates) == 0 {
		return Candidate{}, ErrNoCandidates
	}
	if in == nil {
		return Candidate{}, errors.New("codexsession: nil picker input")
	}
	if out == nil {
		return Candidate{}, errors.New("codexsession: nil picker output")
	}

	if _, err := fmt.Fprintln(out, "Resumable Codex sessions:"); err != nil {
		return Candidate{}, fmt.Errorf("codexsession: write picker: %w", err)
	}
	for index, candidate := range candidates {
		if _, err := fmt.Fprintf(out, "%d) %s  [%s]  %s\n   ID:  %s\n   CWD: %s\n",
			index+1,
			candidate.Recency().UTC().Format("2006-01-02 15:04:05Z"),
			SanitizeDisplay(string(candidate.Source), 24),
			SanitizeDisplay(candidate.Title(), 100),
			SanitizeDisplay(candidate.Thread.ID, 0),
			SanitizeDisplay(candidate.Thread.CWD, 0),
		); err != nil {
			return Candidate{}, fmt.Errorf("codexsession: write picker: %w", err)
		}
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 256), 4<<10)
	for {
		if _, err := fmt.Fprintf(out, "Select a session [1-%d, q]: ", len(candidates)); err != nil {
			return Candidate{}, fmt.Errorf("codexsession: write picker prompt: %w", err)
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return Candidate{}, fmt.Errorf("codexsession: read picker: %w", err)
			}
			return Candidate{}, ErrCanceled
		}
		choice := strings.TrimSpace(scanner.Text())
		if strings.EqualFold(choice, "q") {
			return Candidate{}, ErrCanceled
		}
		index, parseErr := strconv.Atoi(choice)
		if parseErr == nil && index >= 1 && index <= len(candidates) {
			return candidates[index-1], nil
		}
		if _, writeErr := fmt.Fprintf(out, "Enter a number from 1 to %d, or q.\n", len(candidates)); writeErr != nil {
			return Candidate{}, fmt.Errorf("codexsession: write picker error: %w", writeErr)
		}
	}
}

// SanitizeDisplay converts untrusted session metadata to one printable line.
// maxRunes <= 0 disables truncation. Unicode format characters and terminal
// controls are replaced, and all whitespace runs collapse to one ASCII space.
func SanitizeDisplay(value string, maxRunes int) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := false
	for _, r := range value {
		switch {
		case unicode.IsSpace(r):
			space = builder.Len() != 0
		case unicode.IsPrint(r):
			if space {
				builder.WriteByte(' ')
				space = false
			}
			builder.WriteRune(r)
		default:
			if space {
				builder.WriteByte(' ')
				space = false
			}
			builder.WriteRune('\uFFFD')
		}
	}
	result := strings.TrimSpace(builder.String())
	if maxRunes <= 0 {
		return result
	}
	runes := []rune(result)
	if len(runes) <= maxRunes {
		return result
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}
