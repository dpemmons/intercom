package main_test

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

var markdownLink = regexp.MustCompile(`\[[^]]*\]\(([^)]+)\)`)

func TestDocumentationSetAndLocalLinks(t *testing.T) {
	required := []string{
		"README.md",
		"docs/HANDBOOK.md",
		"docs/REFERENCE.md",
		"docs/ARCHITECTURE.md",
		"docs/BROKER_PROTOCOL.md",
		"docs/DEVELOPMENT.md",
	}
	for _, name := range required {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("required document %s: %v", name, err)
		}
	}
	for _, obsolete := range []string{"DESIGN.md", "CODEX_APP_SERVER_DESIGN.md"} {
		if _, err := os.Stat(obsolete); err == nil {
			t.Errorf("obsolete archival document still exists: %s", obsolete)
		} else if !os.IsNotExist(err) {
			t.Errorf("inspect obsolete document %s: %v", obsolete, err)
		}
	}

	for _, name := range required {
		content, err := os.ReadFile(name)
		if err != nil {
			continue
		}
		for _, match := range markdownLink.FindAllStringSubmatch(string(content), -1) {
			raw := strings.Trim(match[1], "<>")
			if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") ||
				strings.HasPrefix(raw, "mailto:") || strings.HasPrefix(raw, "#") {
				continue
			}
			parts := strings.SplitN(raw, "#", 2)
			target, err := url.PathUnescape(parts[0])
			if err != nil {
				t.Errorf("%s: invalid escaped link %q: %v", name, raw, err)
				continue
			}
			if filepath.IsAbs(target) {
				t.Errorf("%s: local link must be relative: %q", name, raw)
				continue
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), target))
			linked, err := os.ReadFile(resolved)
			if err != nil {
				t.Errorf("%s: link %q does not resolve: %v", name, raw, err)
				continue
			}
			if len(parts) == 2 && parts[1] != "" {
				anchors := markdownAnchors(string(linked))
				if _, ok := anchors[parts[1]]; !ok {
					t.Errorf("%s: link %q names no heading in %s", name, raw, resolved)
				}
			}
		}
	}
}

func TestDocumentationStyleContract(t *testing.T) {
	files := append([]string{"README.md"}, markdownFiles(t, "docs")...)
	pronoun := regexp.MustCompile(`(?i)\b(we|our|ours|you|your|yours|simply|easily|powerful)\b`)
	absoluteFragments := []string{"/home/", "/usr/", "/opt/", "/var/", "/run/", "/tmp/"}
	for _, name := range files {
		content, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		inFence := false
		for lineNumber, line := range strings.Split(string(content), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
				inFence = !inFence
				continue
			}
			if inFence {
				continue
			}
			if word := pronoun.FindString(line); word != "" {
				t.Errorf("%s:%d: disallowed conversational word %q", name, lineNumber+1, word)
			}
			for _, fragment := range absoluteFragments {
				if strings.Contains(line, fragment) {
					t.Errorf("%s:%d: absolute filesystem path fragment %q", name, lineNumber+1, fragment)
				}
			}
		}
	}
}

func TestDocumentationNavigationContract(t *testing.T) {
	for _, name := range []string{
		"docs/HANDBOOK.md",
		"docs/REFERENCE.md",
		"docs/ARCHITECTURE.md",
		"docs/BROKER_PROTOCOL.md",
		"docs/DEVELOPMENT.md",
	} {
		if content := readDocument(t, name); !strings.Contains(content, "## CONTENTS\n") {
			t.Errorf("%s: missing linked contents section", name)
		}
	}
}

func TestReferenceEntrySkeletons(t *testing.T) {
	content := readDocument(t, "docs/REFERENCE.md")
	for _, heading := range []string{
		"intercom root", "intercom shim", "intercom codex", "intercom codex attach", "intercom broker",
		"intercom name", "intercom peers", "intercom completion", "intercom help",
		"intercom-codex-project",
	} {
		block := headingBlock(t, content, "### "+heading, "### ")
		assertOrderedHeadings(t, heading, block, []string{
			"#### Synopsis", "#### Arguments", "#### Options", "#### Semantics",
			"#### Errors", "#### Exit status", "#### Example", "#### See also",
		})
	}
	for _, heading := range []string{"send_message", "list_peers", "channel_status"} {
		block := headingBlock(t, content, "### "+heading, "### ")
		assertOrderedHeadings(t, heading, block, []string{
			"#### Signature", "#### Arguments", "#### Semantics", "#### Errors",
			"#### Minimal example", "#### See also",
		})
	}
}

func TestInteractiveCodexDocumentationContract(t *testing.T) {
	reference := readDocument(t, "docs/REFERENCE.md")
	for _, required := range []string{
		"--client-endpoint ENDPOINT",
		"intercom codex attach --name NAME",
		"Intercom Codex peer NAME is ready.",
		"CODEX_EXECUTABLE resume --remote CLIENT_ENDPOINT THREAD_ID",
		"INTERCOM_DIR=STATE_DIRECTORY INTERCOM_SOCKET=BROKER_SOCKET CODEX_BIN=CODEX_EXECUTABLE",
		"$INTERCOM_DIR/codex/live/NAME-DIGEST.json",
		"one downstream TUI session at a time",
		"Codex app-server JSON message | 134217728",
		"Codex TUI proxy JSON message | 134217728",
		"Expired Codex TUI reverse-response ID history | 1024",
		"Codex TUI interactive reverse-response relay | 30",
		"A TUI disconnect does not stop the adapter or app-server.",
		"`/new`",
		"`/fork`",
		"`review/start`",
		"`thread/compact/start`",
		"`thread/rollback`",
		"`thread/shellCommand`",
		"`thread/realtime/start`",
		"`thread/settings/update`",
		"`turn/interrupt` and `turn/steer`",
		"`initialTurnsPage: null`",
		"A verified or cached descendant is accepted.",
	} {
		if !strings.Contains(reference, required) {
			t.Errorf("docs/REFERENCE.md: missing interactive Codex contract %q", required)
		}
	}

	architecture := readDocument(t, "docs/ARCHITECTURE.md")
	for _, required := range []string{
		"sole upstream subscriber",
		"remapped request IDs",
		"App-server dynamic-tool reverse requests never reach the TUI.",
		"Codex TUI turn",
		"fresh control-timeout context",
		"removes its owned live descriptor",
		"bounded `thread/read` parent or fork ancestry",
	} {
		if !strings.Contains(architecture, required) {
			t.Errorf("docs/ARCHITECTURE.md: missing interactive Codex architecture %q", required)
		}
	}
}

func TestHandbookChapterSkeletons(t *testing.T) {
	content := readDocument(t, "docs/HANDBOOK.md")
	for chapter := 1; chapter <= 8; chapter++ {
		prefix := "## " + strconv.Itoa(chapter) + ". "
		block := headingBlock(t, content, prefix, "## ")
		assertOrderedHeadings(t, prefix, block, []string{
			"### Purpose", "### Prerequisites", "### Concepts", "### Procedure",
			"### Verification", "### Notes", "### See also",
		})
	}
}

func TestProtocolFrameEntrySkeletons(t *testing.T) {
	content := readDocument(t, "docs/BROKER_PROTOCOL.md")
	for _, heading := range []string{
		"hello", "welcome", "send", "send_ack", "list_peers",
		"list_peers_reply", "deliver", "goodbye", "error",
	} {
		block := headingBlock(t, content, "### "+heading, "### ")
		assertOrderedHeadings(t, heading, block, []string{
			"#### Signature", "#### Fields", "#### Semantics", "#### Errors",
			"#### Example", "#### See also",
		})
	}
}

func markdownFiles(t *testing.T, root string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func readDocument(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func headingBlock(t *testing.T, document, start, nextPrefix string) string {
	t.Helper()
	startAt := strings.Index(document, start)
	if startAt < 0 {
		t.Fatalf("heading %q is absent", start)
	}
	rest := document[startAt+len(start):]
	if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
		rest = rest[newline+1:]
	}
	if next := strings.Index(rest, "\n"+nextPrefix); next >= 0 {
		rest = rest[:next]
	}
	return rest
}

func assertOrderedHeadings(t *testing.T, entry, block string, headings []string) {
	t.Helper()
	position := 0
	for _, heading := range headings {
		relative := strings.Index(block[position:], heading)
		if relative < 0 {
			t.Errorf("%s: missing ordered heading %q", entry, heading)
			return
		}
		position += relative + len(heading)
	}
}

func markdownAnchors(content string) map[string]struct{} {
	anchors := make(map[string]struct{})
	counts := make(map[string]int)
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(line, "#") {
			continue
		}
		level := 0
		for level < len(line) && line[level] == '#' {
			level++
		}
		if level == 0 || level > 6 || level == len(line) || line[level] != ' ' {
			continue
		}
		base := githubHeadingSlug(strings.TrimSpace(line[level+1:]))
		anchor := base
		if count := counts[base]; count > 0 {
			anchor += "-" + strconv.Itoa(count)
		}
		counts[base]++
		anchors[anchor] = struct{}{}
	}
	return anchors
}

func githubHeadingSlug(heading string) string {
	var slug strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_':
			slug.WriteRune(r)
		case unicode.IsSpace(r):
			slug.WriteByte('-')
		}
	}
	return slug.String()
}
