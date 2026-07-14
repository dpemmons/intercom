package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// roundTripCases exercises every concrete frame's marshal/unmarshal path
// through a real Conn. Each entry's frame must equal itself after a wire round
// trip.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
	}{
		{"hello", Hello{Name: "alice", Version: "0.1.0"}},
		{"welcome", Welcome{}},
		{"goodbye", Goodbye{Reason: "shutdown"}},
		{"send", Send{ID: "deadbeef", To: "bob", Message: "hi"}},
		{"send_ack ok", SendAck{ID: "deadbeef", OK: true}},
		{"send_ack err", SendAck{ID: "deadbeef", OK: false, Code: CodeNoSuchPeer, Message: "no such peer"}},
		{"list_peers", ListPeers{ID: "cafebabe"}},
		{"list_peers_reply", ListPeersReply{ID: "cafebabe", Peers: []string{"alice", "bob"}}},
		{"deliver", Deliver{ID: "deadbeef", From: "bob", Message: "hi back", Timestamp: "2026-05-06T10:30:00Z"}},
		{"error correlated", Error{ID: "deadbeef", Code: CodeBadFrame, Message: "nope"}},
		{"error unsolicited", Error{Code: CodeOversize, Message: "too big"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := NewConn(&buf)
			if err := c.Write(tc.f); err != nil {
				t.Fatalf("write: %v", err)
			}
			got, err := c.Read()
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if got.Kind() != tc.f.Kind() {
				t.Fatalf("kind: got %q want %q", got.Kind(), tc.f.Kind())
			}
			// Concrete equality: marshal both sides and compare. Avoids needing
			// reflect.DeepEqual handling of zero-value slices vs nil.
			a, _ := json.Marshal(tc.f)
			b, _ := json.Marshal(got)
			if !bytes.Equal(a, b) {
				t.Fatalf("body mismatch:\n got:  %s\n want: %s", b, a)
			}
		})
	}
}

// TestEnvelopeShape verifies the on-the-wire JSON for representative frames so
// downstream consumers and docs/BROKER_PROTOCOL.md stay in sync with the
// implementation.
func TestEnvelopeShape(t *testing.T) {
	cases := []struct {
		name string
		f    Frame
		want string
	}{
		{"welcome no body", Welcome{}, `{"kind":"welcome"}`},
		{"hello", Hello{Name: "alice", Version: "0.1.0"},
			`{"kind":"hello","name":"alice","version":"0.1.0"}`},
		{"send_ack failure carries code+message",
			SendAck{ID: "abc", OK: false, Code: CodeNoSelfSend, Message: "self"},
			`{"kind":"send_ack","id":"abc","ok":false,"code":"no_self_send","message":"self"}`},
		{"send_ack success omits code/message",
			SendAck{ID: "abc", OK: true},
			`{"kind":"send_ack","id":"abc","ok":true}`},
		{"deliver carries correlation id",
			Deliver{ID: "abc", From: "alice", Message: "hi", Timestamp: "2026-05-06T10:30:00Z"},
			`{"kind":"deliver","id":"abc","from":"alice","message":"hi","timestamp":"2026-05-06T10:30:00Z"}`},
		{"deliver omits empty correlation id",
			Deliver{From: "alice", Message: "hi", Timestamp: "2026-05-06T10:30:00Z"},
			`{"kind":"deliver","from":"alice","message":"hi","timestamp":"2026-05-06T10:30:00Z"}`},
		{"unsolicited error omits id",
			Error{Code: CodeOversize, Message: "too big"},
			`{"kind":"error","code":"oversize","message":"too big"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.f.encode()
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("envelope:\n got:  %s\n want: %s", got, tc.want)
			}
		})
	}
}

func TestDeliverMixedVersionCompatibility(t *testing.T) {
	t.Run("old delivery without id decodes in new reader", func(t *testing.T) {
		body := []byte(`{"kind":"deliver","from":"alice","message":"hi","timestamp":"2026-05-06T10:30:00Z"}`)
		var framed bytes.Buffer
		if err := binary.Write(&framed, binary.BigEndian, uint32(len(body))); err != nil {
			t.Fatalf("write header: %v", err)
		}
		if _, err := framed.Write(body); err != nil {
			t.Fatalf("write body: %v", err)
		}

		got, err := NewConn(&framed).Read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		d, ok := got.(Deliver)
		if !ok {
			t.Fatalf("got %T, want Deliver", got)
		}
		if d.ID != "" || d.From != "alice" || d.Message != "hi" || d.Timestamp != "2026-05-06T10:30:00Z" {
			t.Fatalf("deliver = %+v", d)
		}
	})

	t.Run("old-style decoder ignores new id", func(t *testing.T) {
		body, err := (Deliver{
			ID:        "deadbeef",
			From:      "alice",
			Message:   "hi",
			Timestamp: "2026-05-06T10:30:00Z",
		}).encode()
		if err != nil {
			t.Fatalf("encode: %v", err)
		}

		var old struct {
			Kind      Kind   `json:"kind"`
			From      string `json:"from"`
			Message   string `json:"message"`
			Timestamp string `json:"timestamp"`
		}
		if err := json.Unmarshal(body, &old); err != nil {
			t.Fatalf("old-style decode: %v", err)
		}
		if old.Kind != KindDeliver || old.From != "alice" || old.Message != "hi" || old.Timestamp != "2026-05-06T10:30:00Z" {
			t.Fatalf("old-style deliver = %+v", old)
		}
	})
}

// TestReadErrors covers the failure paths of Conn.Read against synthetic input.
func TestReadErrors(t *testing.T) {
	t.Run("clean EOF", func(t *testing.T) {
		c := NewConn(bytes.NewBuffer(nil))
		_, err := c.Read()
		if !errors.Is(err, io.EOF) {
			t.Fatalf("got %v want EOF", err)
		}
	})

	t.Run("short read on header", func(t *testing.T) {
		c := NewConn(bytes.NewBuffer([]byte{0x00, 0x01})) // 2 of 4 header bytes
		_, err := c.Read()
		if !errors.Is(err, ErrShortRead) {
			t.Fatalf("got %v want ErrShortRead", err)
		}
	})

	t.Run("oversize", func(t *testing.T) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], MaxFrameSize+1)
		c := NewConn(bytes.NewBuffer(hdr[:]))
		_, err := c.Read()
		if !errors.Is(err, ErrOversize) {
			t.Fatalf("got %v want ErrOversize", err)
		}
	})

	t.Run("short read on payload", func(t *testing.T) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], 100)
		c := NewConn(bytes.NewBuffer(append(hdr[:], []byte("not 100 bytes")...)))
		_, err := c.Read()
		if !errors.Is(err, ErrShortRead) {
			t.Fatalf("got %v want ErrShortRead", err)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		body := []byte("not json")
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
		c := NewConn(bytes.NewBuffer(append(hdr[:], body...)))
		_, err := c.Read()
		if err == nil || !strings.Contains(err.Error(), "decode kind") {
			t.Fatalf("got %v want decode kind error", err)
		}
	})

	t.Run("missing kind", func(t *testing.T) {
		body := []byte(`{"name":"alice"}`)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
		c := NewConn(bytes.NewBuffer(append(hdr[:], body...)))
		_, err := c.Read()
		if err == nil || !strings.Contains(err.Error(), "missing kind") {
			t.Fatalf("got %v want missing-kind error", err)
		}
	})

	t.Run("unknown kind", func(t *testing.T) {
		body := []byte(`{"kind":"bogus"}`)
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
		c := NewConn(bytes.NewBuffer(append(hdr[:], body...)))
		_, err := c.Read()
		if err == nil || !strings.Contains(err.Error(), "unknown kind") {
			t.Fatalf("got %v want unknown-kind error", err)
		}
	})
}

// FuzzConnRead exercises the untrusted broker framing boundary. Successful
// decodes must remain encodable and readable as the same concrete frame kind;
// malformed inputs may return an error but must never panic or allocate beyond
// the protocol's announced frame limit.
func FuzzConnRead(f *testing.F) {
	seeds := []Frame{
		Hello{Name: "alice", Version: "test"},
		Welcome{},
		Goodbye{Reason: "shutdown"},
		Send{ID: "1", To: "bob", Message: "hello"},
		SendAckOK("1"),
		ListPeers{ID: "2"},
		ListPeersReply{ID: "2", Peers: []string{"alice", "bob"}},
		Deliver{ID: "1", From: "alice", Message: "hello", Timestamp: "2026-07-13T00:00:00Z"},
		Error{ID: "1", Code: CodeBadFrame, Message: "bad"},
	}
	for _, seed := range seeds {
		var framed bytes.Buffer
		if err := NewConn(&framed).Write(seed); err != nil {
			f.Fatalf("encode seed %s: %v", seed.Kind(), err)
		}
		f.Add(framed.Bytes())
	}
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 1, '{'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxFrameSize+4 {
			t.Skip()
		}
		got, err := NewConn(&readWriteAdapter{r: bytes.NewReader(data)}).Read()
		if err != nil {
			return
		}
		var roundTrip bytes.Buffer
		if err := NewConn(&roundTrip).Write(got); err != nil {
			t.Fatalf("re-encode %s: %v", got.Kind(), err)
		}
		again, err := NewConn(&roundTrip).Read()
		if err != nil {
			t.Fatalf("read re-encoded %s: %v", got.Kind(), err)
		}
		if again.Kind() != got.Kind() {
			t.Fatalf("round-trip kind = %q, want %q", again.Kind(), got.Kind())
		}
	})
}

// TestWriteOversize ensures we refuse to send giant frames at marshal time, so
// neither side has to parse oversized garbage. The big string is sized so the
// JSON envelope crosses MaxFrameSize.
func TestWriteOversize(t *testing.T) {
	huge := strings.Repeat("x", MaxFrameSize)
	c := NewConn(&bytes.Buffer{})
	err := c.Write(Send{ID: "id", To: "bob", Message: huge})
	if !errors.Is(err, ErrOversize) {
		t.Fatalf("got %v want ErrOversize", err)
	}
}

func TestWriteWithTimeoutIncludesQueueWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		blocked := &firstWriteBlocker{
			started: make(chan struct{}),
			release: make(chan struct{}),
		}
		c := NewConn(blocked)
		firstDone := make(chan error, 1)
		go func() {
			firstDone <- c.Write(ListPeersReply{ID: "blocking"})
		}()
		<-blocked.started

		const budget = time.Second
		start := time.Now()
		err := c.WriteWithTimeout(Goodbye{Reason: "shutdown"}, budget)
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("queued write error = %v, want deadline exceeded", err)
		}
		if elapsed := time.Since(start); elapsed != budget {
			t.Fatalf("queued write elapsed = %v, want total budget %v", elapsed, budget)
		}

		close(blocked.release)
		if err := <-firstDone; err != nil {
			t.Fatalf("blocking write after release: %v", err)
		}
	})
}

type firstWriteBlocker struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *firstWriteBlocker) Write(p []byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
		<-w.release
	})
	return len(p), nil
}

func (*firstWriteBlocker) Read([]byte) (int, error) { return 0, io.EOF }

func TestWriteAllCompletesShortWrites(t *testing.T) {
	w := &chunkWriter{max: 2}
	c := NewConn(w)
	want := Hello{Name: "alice", Version: "test"}
	if err := c.Write(want); err != nil {
		t.Fatalf("write through short writer: %v", err)
	}
	if w.writes <= 2 {
		t.Fatalf("underlying writes = %d, want retries for short writes", w.writes)
	}
	frame, err := NewConn(&w.Buffer).Read()
	if err != nil {
		t.Fatalf("read completed short writes: %v", err)
	}
	if got, ok := frame.(Hello); !ok || got != want {
		t.Fatalf("round trip = %#v, want %#v", frame, want)
	}
}

type chunkWriter struct {
	bytes.Buffer
	max    int
	writes int
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.writes++
	if len(p) > w.max {
		p = p[:w.max]
	}
	return w.Buffer.Write(p)
}

func TestWriteAllRejectsNoProgress(t *testing.T) {
	err := NewConn(zeroProgressWriter{}).Write(Welcome{})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("no-progress write error = %v, want io.ErrShortWrite", err)
	}
}

type zeroProgressWriter struct{}

func (zeroProgressWriter) Write([]byte) (int, error) { return 0, nil }
func (zeroProgressWriter) Read([]byte) (int, error)  { return 0, io.EOF }

// TestConcurrentWrites verifies the per-Conn write gate serializes frames
// even when many goroutines call Write concurrently. We expect to see N
// well-formed frames back-to-back, no interleaving.
func TestConcurrentWrites(t *testing.T) {
	const N = 50
	var buf safeBuf
	c := NewConn(&buf)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Write(Hello{Name: "alice", Version: "0.1.0"}); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wg.Wait()

	// Re-read from the buffer with a fresh Conn; we should get N hellos with
	// no decode errors.
	r := NewConn(&readWriteAdapter{r: bytes.NewReader(buf.Bytes())})
	count := 0
	for {
		f, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read at %d: %v", count, err)
		}
		if f.Kind() != KindHello {
			t.Fatalf("frame %d: kind %q want hello", count, f.Kind())
		}
		count++
	}
	if count != N {
		t.Fatalf("got %d frames, want %d", count, N)
	}
}

// readWriteAdapter pairs an io.Reader with a no-op writer so it satisfies
// io.ReadWriter for tests that only need to read back what's already buffered.
type readWriteAdapter struct {
	r io.Reader
}

func (a *readWriteAdapter) Read(p []byte) (int, error) { return a.r.Read(p) }
func (a *readWriteAdapter) Write([]byte) (int, error)  { return 0, errors.New("read-only adapter") }

// safeBuf is a goroutine-safe wrapper around bytes.Buffer for concurrent-write
// tests. bytes.Buffer is not safe for concurrent Write; the Conn's gate is,
// but we still need the underlying Writer to handle concurrent direct calls
// from the Conn implementation across goroutines after the lock is released.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuf) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Read(p)
}

func (s *safeBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

func TestValidName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"alice", true},
		{"alice-bob", true},
		{"alice_bob", true},
		{"AliceBob123", true},
		{"a", true},
		{strings.Repeat("a", MaxNameLen), true},

		{"", false},
		{strings.Repeat("a", MaxNameLen+1), false},
		{"alice bob", false},
		{"alice.bob", false},
		{"alice/bob", false},
		{"../etc/passwd", false},
		{"\x00alice", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidName(tc.name); got != tc.ok {
				t.Fatalf("ValidName(%q) = %v, want %v", tc.name, got, tc.ok)
			}
		})
	}
}

// TestWriteWithTimeoutAppliesDeadlineUnderGate verifies that the deadline is set and
// cleared inside the per-Conn write gate, so two concurrent writers can't
// clobber each other's deadlines. The deadlinerProbe tracks every
// SetWriteDeadline call.
func TestWriteWithTimeoutAppliesDeadlineUnderGate(t *testing.T) {
	probe := &deadlinerProbe{}
	c := NewConn(probe)

	if err := c.WriteWithTimeout(Hello{Name: "alice", Version: "v"}, 50*time.Millisecond); err != nil {
		t.Fatalf("write: %v", err)
	}

	probe.mu.Lock()
	defer probe.mu.Unlock()
	// Expect: one Set with a future deadline, one Set clearing it (zero time).
	if len(probe.deadlines) != 2 {
		t.Fatalf("got %d SetWriteDeadline calls, want 2: %v", len(probe.deadlines), probe.deadlines)
	}
	if probe.deadlines[0].IsZero() {
		t.Errorf("first call cleared deadline; want non-zero")
	}
	if !probe.deadlines[1].IsZero() {
		t.Errorf("second call set deadline; want zero (clear)")
	}
}

// deadlinerProbe is an io.ReadWriter + deadliner that records every
// SetWriteDeadline call.
type deadlinerProbe struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	deadlines []time.Time
}

func (d *deadlinerProbe) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Write(p)
}

func (d *deadlinerProbe) Read(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.Read(p)
}

func (d *deadlinerProbe) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadlines = append(d.deadlines, t)
	return nil
}

func TestNewIDFormat(t *testing.T) {
	id := NewID()
	if len(id) != 16 {
		t.Fatalf("len = %d, want 16", len(id))
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("id contains non-hex %q: %s", r, id)
		}
	}
}

// TestNewIDUnique probabilistically verifies uniqueness across many calls.
// 8 bytes of crypto/rand makes collisions astronomically unlikely; this test
// guards against regressions like accidentally truncating to 1 byte.
func TestNewIDUnique(t *testing.T) {
	const N = 10_000
	seen := make(map[string]struct{}, N)
	var collisions atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := NewID()
			mu.Lock()
			if _, dup := seen[id]; dup {
				collisions.Add(1)
			}
			seen[id] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if c := collisions.Load(); c != 0 {
		t.Fatalf("%d collisions in %d ids", c, N)
	}
}
