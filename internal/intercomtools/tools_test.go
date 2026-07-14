package intercomtools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dpemmons/intercom/internal/wire"
)

func TestDecodeSendMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    SendMessageArgs
		wantErr string
	}{
		{name: "valid", raw: `{"to":"reviewer","message":"hello"}`, want: SendMessageArgs{To: "reviewer", Message: "hello"}},
		{name: "missing destination", raw: `{"message":"hello"}`, wantErr: `"to" is required`},
		{name: "invalid destination", raw: `{"to":"not valid","message":"hello"}`, wantErr: "invalid destination peer"},
		{name: "missing message", raw: `{"to":"reviewer"}`, wantErr: `"message" is required`},
		{name: "unknown property", raw: `{"to":"reviewer","message":"hello","extra":true}`, wantErr: "unknown field"},
		{name: "trailing value", raw: `{"to":"reviewer","message":"hello"} {}`, wantErr: "decode args"},
		{name: "array is not object", raw: `[]`, wantErr: "send_message arguments must be an object"},
		{name: "null is not object", raw: `null`, wantErr: "send_message arguments must be an object"},
		{name: "scalar is not object", raw: `42`, wantErr: "send_message arguments must be an object"},
		{name: "bad json", raw: `{`, wantErr: "decode args"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeSendMessage(json.RawMessage(tt.raw))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("DecodeSendMessage() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeSendMessage() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("DecodeSendMessage() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDecodeSendMessageOversize(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(SendMessageArgs{To: "reviewer", Message: strings.Repeat("x", MaxOutboundMessageBytes+1)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = DecodeSendMessage(raw)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("DecodeSendMessage() error = %v, want oversize error", err)
	}
}

func TestDecodeSendMessageRejectsJSONExpansionBeyondWireFrame(t *testing.T) {
	t.Parallel()

	for _, message := range []string{
		strings.Repeat(`"`, 150*1024),
		strings.Repeat("\x00", 50*1024),
	} {
		raw, err := json.Marshal(SendMessageArgs{To: "reviewer", Message: message})
		if err != nil {
			t.Fatal(err)
		}
		_, err = DecodeSendMessage(raw)
		if err == nil || !strings.Contains(err.Error(), "wire frame") {
			t.Fatalf("DecodeSendMessage() error = %v, want encoded wire-frame error", err)
		}
	}
}

func TestDecodeListPeers(t *testing.T) {
	t.Parallel()
	if err := DecodeListPeers(json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `[]`, `{"extra":true}`, `{`, "{}\f"} {
		if err := DecodeListPeers(json.RawMessage(raw)); err == nil {
			t.Fatalf("DecodeListPeers(%s) unexpectedly succeeded", raw)
		}
	}
}

func TestFormatPeers(t *testing.T) {
	t.Parallel()

	if got := FormatPeers(nil); got != "No other peers are connected." {
		t.Fatalf("FormatPeers(nil) = %q", got)
	}
	if got := FormatPeers([]string{"alice", "bob"}); got != "Connected peers: alice, bob" {
		t.Fatalf("FormatPeers() = %q", got)
	}
}

func FuzzDecodeSendMessage(f *testing.F) {
	for _, seed := range []string{
		`{"to":"bob","message":"hello"}`,
		`{}`,
		`null`,
		`{"to":"../bob","message":"hello"}`,
		`{"to":"bob","message":""}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxOutboundMessageBytes+16<<10 {
			t.Skip()
		}
		args, err := DecodeSendMessage(data)
		if err != nil {
			return
		}
		if !wire.ValidName(args.To) || args.Message == "" || len(args.Message) > MaxOutboundMessageBytes {
			t.Fatalf("decoder accepted invalid args: %#v", args)
		}
		size, err := wire.EncodedFrameSize(wire.Deliver{
			ID:        "0000000000000000",
			From:      strings.Repeat("a", wire.MaxNameLen),
			Message:   args.Message,
			Timestamp: representativeTimestamp,
		})
		if err != nil || size > wire.MaxFrameSize {
			t.Fatalf("decoder accepted oversize delivery: size=%d err=%v", size, err)
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		again, err := DecodeSendMessage(encoded)
		if err != nil {
			t.Fatalf("decode canonical args %s: %v", encoded, err)
		}
		if again != args {
			t.Fatalf("round trip = %#v, want %#v", again, args)
		}
	})
}

func FuzzDecodeListPeers(f *testing.F) {
	for _, seed := range []string{`{}`, `{ }`, `null`, `[]`, `{"extra":true}`, ``} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		if err := DecodeListPeers(data); err != nil {
			return
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil || len(object) != 0 {
			t.Fatalf("accepted non-empty object %q: map=%v err=%v", data, object, err)
		}
	})
}
