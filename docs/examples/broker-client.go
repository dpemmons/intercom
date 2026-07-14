// Command broker-client exercises the broker framing contract without using
// Intercom's wire or broker-client packages.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/dpemmons/intercom/internal/broker"
)

const frameLimit = 256 * 1024

type reply struct {
	Kind  string   `json:"kind"`
	ID    string   `json:"id"`
	Peers []string `json:"peers"`
}

func main() {
	runtimeDir, err := os.MkdirTemp("", "intercom-protocol-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(runtimeDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	socketPath := filepath.Join(runtimeDir, "broker.sock")
	brokerDone := make(chan error, 1)
	go func() {
		brokerDone <- broker.Run(ctx, broker.Options{
			SocketPath: socketPath,
			IdleAfter:  broker.IdleExitDisabled,
			Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()

	conn, err := dial(socketPath, brokerDone)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	mustWriteFrame(conn, map[string]any{
		"kind":    "hello",
		"name":    "protocol-client",
		"version": "example",
	})
	welcome := mustReadFrame(conn)
	if welcome.Kind != "welcome" {
		log.Fatalf("hello reply kind = %q, want welcome", welcome.Kind)
	}
	fmt.Println("welcome")

	mustWriteFrame(conn, map[string]any{
		"kind": "list_peers",
		"id":   "example-1",
	})
	peers := mustReadFrame(conn)
	if peers.Kind != "list_peers_reply" || peers.ID != "example-1" {
		log.Fatalf("list reply = %#v", peers)
	}
	fmt.Printf("peers %d\n", len(peers.Peers))

	_ = conn.Close()
	cancel()
	if err := <-brokerDone; err != nil {
		log.Fatal(err)
	}
}

func dial(socketPath string, brokerDone <-chan error) (net.Conn, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socketPath, 50*time.Millisecond)
		if err == nil {
			return conn, nil
		}
		select {
		case brokerErr := <-brokerDone:
			return nil, fmt.Errorf("broker exited before accepting connections: %w", brokerErr)
		default:
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to broker: %w", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustWriteFrame(conn net.Conn, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		log.Fatal(err)
	}
	if len(payload) > frameLimit {
		log.Fatalf("payload length %d exceeds %d", len(payload), frameLimit)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(conn, header[:]); err != nil {
		log.Fatal(err)
	}
	if err := writeAll(conn, payload); err != nil {
		log.Fatal(err)
	}
}

func mustReadFrame(conn net.Conn) reply {
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		log.Fatal(err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > frameLimit {
		log.Fatalf("reply length %d exceeds %d", size, frameLimit)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(conn, payload); err != nil {
		log.Fatal(err)
	}
	var result reply
	if err := json.Unmarshal(payload, &result); err != nil {
		log.Fatal(err)
	}
	return result
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
