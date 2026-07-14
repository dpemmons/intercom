package codex

import (
	"strings"
	"testing"
	"time"
)

func TestDeveloperInstructionsRequireExplicitReplyTool(t *testing.T) {
	t.Parallel()
	got := developerInstructions("reviewer")
	for _, want := range []string{"reviewer", "send_message", "list_peers", "not delivered to Intercom"} {
		if !strings.Contains(got, want) {
			t.Fatalf("developerInstructions() missing %q", want)
		}
	}
}

func TestInboundEnvelope(t *testing.T) {
	t.Parallel()
	sent := time.Date(2026, 7, 13, 18, 42, 0, 123, time.FixedZone("PDT", -7*60*60))
	got := inboundEnvelope("alice", "msg-1", "please review", sent)
	want := "Intercom message\nFrom: alice\nSent: 2026-07-14T01:42:00.000000123Z\nMessage-ID: msg-1\n\nplease review"
	if got != want {
		t.Fatalf("inboundEnvelope() = %q, want %q", got, want)
	}
}
