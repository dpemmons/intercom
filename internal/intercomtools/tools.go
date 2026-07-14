// Package intercomtools defines the provider-neutral tool contract exposed to
// every agent adapter.
package intercomtools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/dpemmons/intercom/internal/wire"
)

const (
	SendMessageName = "send_message"
	ListPeersName   = "list_peers"

	// MaxOutboundMessageBytes is the raw UTF-8 message ceiling. Validation also
	// measures the encoded delivery frame because JSON escaping can expand a
	// message substantially.
	MaxOutboundMessageBytes = 200 * 1024
)

const representativeTimestamp = "2006-01-02T15:04:05Z"

const (
	SendMessageDescription = "Sends a message to another connected local agent session through the Intercom broker. list_peers returns the available destination names."
	ListPeersDescription   = "Lists the names of other agent sessions connected to the Intercom broker."
)

var (
	SendMessageInputSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"to":      {"type": "string", "description": "The peer name of the destination session."},
			"message": {"type": "string", "description": "The message body."}
		},
		"required": ["to", "message"],
		"additionalProperties": false
	}`)
	ListPeersInputSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
)

type SendMessageArgs struct {
	To      string `json:"to"`
	Message string `json:"message"`
}

// DecodeListPeers validates the empty-object contract shared by all adapters.
func DecodeListPeers(raw json.RawMessage) error {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return fmt.Errorf("decode args: %w", err)
	}
	if args == nil {
		return errors.New("list_peers arguments must be an object")
	}
	if len(args) != 0 {
		return errors.New("list_peers does not accept arguments")
	}
	return nil
}

// DecodeSendMessage decodes and validates the shared send_message contract.
func DecodeSendMessage(raw json.RawMessage) (SendMessageArgs, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return SendMessageArgs{}, errors.New("send_message arguments must be an object")
	}
	var args SendMessageArgs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return SendMessageArgs{}, fmt.Errorf("decode args: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return SendMessageArgs{}, fmt.Errorf("decode args: %w", err)
	}
	if args.To == "" {
		return SendMessageArgs{}, fmt.Errorf(`"to" is required`)
	}
	if !wire.ValidName(args.To) {
		return SendMessageArgs{}, fmt.Errorf("invalid destination peer %q", args.To)
	}
	if args.Message == "" {
		return SendMessageArgs{}, fmt.Errorf(`"message" is required`)
	}
	if len(args.Message) > MaxOutboundMessageBytes {
		return SendMessageArgs{}, fmt.Errorf("message exceeds %d-byte limit", MaxOutboundMessageBytes)
	}
	// Delivery has a larger envelope than Send. Size it with the maximum peer
	// name and the fixed-width IDs produced by wire.NewID so any message this
	// shared tool contract accepts can be delivered without crossing the wire
	// limit solely because of JSON escaping or broker-added metadata.
	deliverySize, err := wire.EncodedFrameSize(wire.Deliver{
		ID:        "0000000000000000",
		From:      strings.Repeat("a", wire.MaxNameLen),
		Message:   args.Message,
		Timestamp: representativeTimestamp,
	})
	if err != nil {
		return SendMessageArgs{}, fmt.Errorf("size message delivery: %w", err)
	}
	if deliverySize > wire.MaxFrameSize {
		return SendMessageArgs{}, fmt.Errorf("message expands beyond %d-byte wire frame limit", wire.MaxFrameSize)
	}
	return args, nil
}

func SendAccepted(to string) string {
	return fmt.Sprintf("Message sent to %q.", to)
}

func SendFailed(err error) string {
	return fmt.Sprintf("send failed: %v", err)
}

func SendRejected(code wire.Code, message string) string {
	return fmt.Sprintf("send rejected (%s): %s", code, message)
}

func ListPeersFailed(err error) string {
	return fmt.Sprintf("list_peers failed: %v", err)
}

func FormatPeers(peers []string) string {
	if len(peers) == 0 {
		return "No other peers are connected."
	}
	return "Connected peers: " + strings.Join(peers, ", ")
}
