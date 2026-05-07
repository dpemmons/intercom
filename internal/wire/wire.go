// Package wire defines the framing and message types used between the intercom
// shim and broker over a Unix domain socket.
//
// Frames are 4-byte big-endian length-prefixed UTF-8 JSON. Each JSON object
// carries a "kind" discriminator. The Go API uses one concrete type per kind
// (see Hello, Welcome, Send, SendAck, ListPeers, ListPeersReply, Deliver,
// Goodbye, Error), all satisfying the [Frame] interface. See DESIGN.md for the
// full protocol contract.
package wire

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"
	"time"
)

// MaxFrameSize bounds a single frame's JSON payload. Frames larger than this
// are refused by both shim and broker.
const MaxFrameSize = 256 * 1024

// Kind enumerates the discriminator values used on the wire.
type Kind string

const (
	KindHello          Kind = "hello"
	KindWelcome        Kind = "welcome"
	KindError          Kind = "error"
	KindGoodbye        Kind = "goodbye"
	KindSend           Kind = "send"
	KindSendAck        Kind = "send_ack"
	KindListPeers      Kind = "list_peers"
	KindListPeersReply Kind = "list_peers_reply"
	KindDeliver        Kind = "deliver"
)

// Code enumerates wire-level error codes carried on Error and SendAck frames.
type Code string

const (
	CodeBadHello      Code = "bad_hello"
	CodeBadName       Code = "bad_name"
	CodeNameTaken     Code = "name_taken"
	CodeHelloTimeout  Code = "hello_timeout"
	CodeNoSuchPeer    Code = "no_such_peer"
	CodeNoSelfSend    Code = "no_self_send"
	CodeDeliverFailed Code = "deliver_failed"
	CodeOversize      Code = "oversize"
	CodeBadFrame      Code = "bad_frame"
)

// Frame is the sealed sum type implemented by every concrete frame.
type Frame interface {
	Kind() Kind
	encode() ([]byte, error)
}

// ----- Concrete frames -----------------------------------------------------

// Hello: the first frame the shim sends after connecting.
type Hello struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (Hello) Kind() Kind                { return KindHello }
func (h Hello) encode() ([]byte, error) { return marshalEnvelope(KindHello, h) }

// Welcome: the broker's positive response to Hello.
type Welcome struct{}

func (Welcome) Kind() Kind                { return KindWelcome }
func (w Welcome) encode() ([]byte, error) { return marshalEnvelope(KindWelcome, w) }

// Goodbye: the broker is closing this connection (shutdown or idle exit).
type Goodbye struct {
	Reason string `json:"reason"`
}

func (Goodbye) Kind() Kind                { return KindGoodbye }
func (g Goodbye) encode() ([]byte, error) { return marshalEnvelope(KindGoodbye, g) }

// Send: the shim asks the broker to deliver Message to peer To.
type Send struct {
	ID      string `json:"id"`
	To      string `json:"to"`
	Message string `json:"message"`
}

func (Send) Kind() Kind                { return KindSend }
func (s Send) encode() ([]byte, error) { return marshalEnvelope(KindSend, s) }

// SendAck: the broker's reply to a Send. OK=true on success; on failure, Code
// and Message describe the reason.
type SendAck struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Code    Code   `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func (SendAck) Kind() Kind                { return KindSendAck }
func (s SendAck) encode() ([]byte, error) { return marshalEnvelope(KindSendAck, s) }

// ListPeers: the shim asks for the names of currently-connected peers.
type ListPeers struct {
	ID string `json:"id"`
}

func (ListPeers) Kind() Kind                { return KindListPeers }
func (l ListPeers) encode() ([]byte, error) { return marshalEnvelope(KindListPeers, l) }

// ListPeersReply: response to ListPeers. Peers excludes the requester and is
// sorted lexicographically.
type ListPeersReply struct {
	ID    string   `json:"id"`
	Peers []string `json:"peers"`
}

func (ListPeersReply) Kind() Kind                { return KindListPeersReply }
func (l ListPeersReply) encode() ([]byte, error) { return marshalEnvelope(KindListPeersReply, l) }

// Deliver: the broker pushes a message to a peer. From is the sender's
// registered (and validated) name.
type Deliver struct {
	From      string `json:"from"`
	Message   string `json:"message"`
	Timestamp string `json:"timestamp"`
}

func (Deliver) Kind() Kind                { return KindDeliver }
func (d Deliver) encode() ([]byte, error) { return marshalEnvelope(KindDeliver, d) }

// Error: a wire-level error. ID is set when responding to a request that
// carried one; omitted for unsolicited errors (oversize, bad_frame on a frame
// that couldn't be parsed, hello_timeout).
type Error struct {
	ID      string `json:"id,omitempty"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

func (Error) Kind() Kind                { return KindError }
func (e Error) encode() ([]byte, error) { return marshalEnvelope(KindError, e) }

// ----- Constructors -------------------------------------------------------
//
// Helpers that build commonly-used frames at call sites where struct literals
// would be noisy or error-prone (e.g., flipping the ok bool on SendAck).

// SendAckOK builds a successful send_ack for the given request id.
func SendAckOK(id string) SendAck { return SendAck{ID: id, OK: true} }

// SendAckErr builds a failure send_ack with the given code and message.
func SendAckErr(id string, code Code, msg string) SendAck {
	return SendAck{ID: id, OK: false, Code: code, Message: msg}
}

// marshalEnvelope wraps the per-kind body in {"kind": "...", ...body}. We
// merge by re-marshalling and patching, which is simpler than reflection.
func marshalEnvelope(k Kind, body any) ([]byte, error) {
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if string(bodyJSON) == "{}" {
		return []byte(fmt.Sprintf(`{"kind":%q}`, string(k))), nil
	}
	// bodyJSON starts with '{'; insert the kind field at the front.
	out := make([]byte, 0, len(bodyJSON)+len(k)+12)
	out = append(out, '{')
	out = append(out, fmt.Sprintf(`"kind":%q,`, string(k))...)
	out = append(out, bodyJSON[1:]...) // skip the opening '{'
	return out, nil
}

// decode parses a single frame body into the concrete type for k.
func decode(k Kind, body []byte) (Frame, error) {
	switch k {
	case KindHello:
		var f Hello
		err := json.Unmarshal(body, &f)
		return f, err
	case KindWelcome:
		return Welcome{}, nil
	case KindGoodbye:
		var f Goodbye
		err := json.Unmarshal(body, &f)
		return f, err
	case KindSend:
		var f Send
		err := json.Unmarshal(body, &f)
		return f, err
	case KindSendAck:
		var f SendAck
		err := json.Unmarshal(body, &f)
		return f, err
	case KindListPeers:
		var f ListPeers
		err := json.Unmarshal(body, &f)
		return f, err
	case KindListPeersReply:
		var f ListPeersReply
		err := json.Unmarshal(body, &f)
		return f, err
	case KindDeliver:
		var f Deliver
		err := json.Unmarshal(body, &f)
		return f, err
	case KindError:
		var f Error
		err := json.Unmarshal(body, &f)
		return f, err
	default:
		return nil, fmt.Errorf("wire: unknown kind %q", k)
	}
}

// ----- Codec ---------------------------------------------------------------

// ErrOversize is returned when a frame exceeds [MaxFrameSize]. The connection
// is unusable after this error and should be closed.
var ErrOversize = errors.New("wire: frame exceeds max size")

// ErrShortRead is returned when the underlying reader returns EOF mid-frame.
var ErrShortRead = errors.New("wire: short read")

// deadliner is implemented by io.ReadWriters that support write deadlines —
// notably net.Conn. Used by [Conn.WriteWithTimeout].
type deadliner interface {
	SetWriteDeadline(t time.Time) error
}

// Conn is a length-prefixed JSON frame channel over an [io.ReadWriter].
//
// Write is goroutine-safe (a per-Conn mutex serializes writes). Read is not;
// drive a single read goroutine per connection.
type Conn struct {
	rw     io.ReadWriter
	writeM sync.Mutex
}

// NewConn wraps rw in a length-prefixed JSON frame channel.
func NewConn(rw io.ReadWriter) *Conn { return &Conn{rw: rw} }

// Write encodes f and sends it as a single length-prefixed JSON frame. If the
// encoded payload exceeds MaxFrameSize, Write returns ErrOversize and nothing
// is written.
func (c *Conn) Write(f Frame) error {
	return c.WriteWithTimeout(f, 0)
}

// WriteWithTimeout is like [Conn.Write] but applies a write deadline of
// time.Now()+timeout for the duration of this single frame. timeout <= 0
// means no deadline.
//
// The deadline is applied inside the per-Conn write mutex, so concurrent
// callers don't clobber each other's deadlines: each frame either finishes
// or trips its own deadline.
//
// If the underlying io.ReadWriter doesn't support SetWriteDeadline (e.g. a
// bytes.Buffer in tests), the timeout is silently ignored.
func (c *Conn) WriteWithTimeout(f Frame, timeout time.Duration) error {
	payload, err := f.encode()
	if err != nil {
		return fmt.Errorf("wire: marshal %s: %w", f.Kind(), err)
	}
	if len(payload) > MaxFrameSize {
		return ErrOversize
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))

	c.writeM.Lock()
	defer c.writeM.Unlock()

	if timeout > 0 {
		if dc, ok := c.rw.(deadliner); ok {
			_ = dc.SetWriteDeadline(time.Now().Add(timeout))
			defer dc.SetWriteDeadline(time.Time{})
		}
	}
	if _, err := c.rw.Write(hdr[:]); err != nil {
		return fmt.Errorf("wire: write header: %w", err)
	}
	if _, err := c.rw.Write(payload); err != nil {
		return fmt.Errorf("wire: write payload: %w", err)
	}
	return nil
}

// Read consumes one frame from the connection. It returns:
//
//   - (frame, nil) on success;
//   - (nil, io.EOF) at clean stream end (no bytes read);
//   - (nil, ErrShortRead) on EOF mid-frame;
//   - (nil, ErrOversize) when the announced length exceeds MaxFrameSize. The
//     connection is unusable and the caller should close it after writing any
//     final error frame;
//   - (nil, other error) on any other I/O or decode error.
func (c *Conn) Read() (Frame, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.rw, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrShortRead
		}
		return nil, fmt.Errorf("wire: read header: %w", err)
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrameSize {
		return nil, ErrOversize
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.rw, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrShortRead
		}
		return nil, fmt.Errorf("wire: read payload: %w", err)
	}
	// Peek the kind, then decode into the concrete type.
	var head struct {
		Kind Kind `json:"kind"`
	}
	if err := json.Unmarshal(buf, &head); err != nil {
		return nil, fmt.Errorf("wire: decode kind: %w", err)
	}
	if head.Kind == "" {
		return nil, errors.New("wire: missing kind")
	}
	f, err := decode(head.Kind, buf)
	if err != nil {
		return nil, fmt.Errorf("wire: decode %s: %w", head.Kind, err)
	}
	return f, nil
}

// ----- Identifiers and validation -----------------------------------------

// nameRE is the validation regex for peer names: letters, digits, hyphen,
// underscore; non-empty; up to 64 characters.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// MaxNameLen caps peer name length to keep error messages and meta values
// bounded.
const MaxNameLen = 64

// ValidName reports whether s is acceptable as a peer name.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > MaxNameLen {
		return false
	}
	return nameRE.MatchString(s)
}

// NewID returns a fresh request id: 16 hex characters from crypto/rand. Used
// by the shim to correlate send and list_peers requests with their replies.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a system-level event we cannot meaningfully
		// recover from in this context.
		panic(fmt.Sprintf("wire: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}
