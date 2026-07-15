package codexbridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dpemmons/intercom/internal/wire"
)

type helperDriver struct {
	input  io.WriteCloser
	frames chan []byte
	done   chan error
}

func startHelper(t *testing.T, socketPath, token string) *helperDriver {
	t.Helper()
	serverIn, clientIn := io.Pipe()
	clientOut, serverOut := io.Pipe()
	d := &helperDriver{input: clientIn, frames: make(chan []byte, 16), done: make(chan error, 1)}
	go func() {
		d.done <- RunHelper(t.Context(), HelperOptions{
			SocketPath: socketPath,
			Token:      token,
			Version:    "test",
			Timeout:    time.Second,
			Stdin:      serverIn,
			Stdout:     serverOut,
		})
		_ = serverOut.Close()
	}()
	go func() {
		defer close(d.frames)
		scanner := bufio.NewScanner(clientOut)
		scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
		for scanner.Scan() {
			d.frames <- append([]byte(nil), scanner.Bytes()...)
		}
	}()
	t.Cleanup(func() {
		_ = clientIn.Close()
		select {
		case err := <-d.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("RunHelper: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("helper did not exit")
		}
	})
	return d
}

func (d *helperDriver) send(t *testing.T, raw string) {
	t.Helper()
	if _, err := io.WriteString(d.input, raw+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (d *helperDriver) receive(t *testing.T, value any) {
	t.Helper()
	select {
	case raw, ok := <-d.frames:
		if !ok {
			t.Fatal("helper output closed")
		}
		if err := json.Unmarshal(raw, value); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for helper response")
	}
}

func TestHelperMCPToolsAndMetadataEndToEnd(t *testing.T) {
	t.Parallel()
	type observation struct {
		metadata json.RawMessage
		to       string
		message  string
	}
	seen := make(chan observation, 2)
	controller, _ := listenTestController(t, HandlerFuncs{
		SendMessageFunc: func(_ context.Context, metadata json.RawMessage, to, message string) (wire.SendAck, error) {
			seen <- observation{metadata: metadata, to: to, message: message}
			return wire.SendAckOK("id"), nil
		},
		ListPeersFunc: func(_ context.Context, metadata json.RawMessage) ([]string, error) {
			seen <- observation{metadata: metadata}
			return []string{"alice", "bob"}, nil
		},
	}, nil)
	d := startHelper(t, controller.SocketPath(), testToken)

	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	var initialize struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	d.receive(t, &initialize)
	if initialize.Result.ServerInfo.Name != "intercom-codex" || initialize.Result.ServerInfo.Version != "test" {
		t.Fatalf("server info = %+v", initialize.Result.ServerInfo)
	}
	d.send(t, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	d.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var list struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	d.receive(t, &list)
	if len(list.Result.Tools) != 2 {
		t.Fatalf("tools = %+v", list.Result.Tools)
	}
	names := map[string]bool{}
	for _, tool := range list.Result.Tools {
		names[tool.Name] = true
		if len(tool.InputSchema) == 0 {
			t.Fatalf("tool %q has no schema", tool.Name)
		}
	}
	if !names[methodSendMessage] || !names[methodListPeers] {
		t.Fatalf("tool names = %v", names)
	}

	metadata := `{"threadId":"thread-a","x-codex-turn-metadata":{"turnId":"turn-b","rootThreadId":"thread-a","nested":{"kept":1}},"extension":["v"]}`
	d.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"bob","message":"hello"},"_meta":`+metadata+`}}`)
	var sendResult struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	d.receive(t, &sendResult)
	if sendResult.Result.IsError || len(sendResult.Result.Content) != 1 || sendResult.Result.Content[0].Text != `Message sent to "bob".` {
		t.Fatalf("send result = %+v", sendResult.Result)
	}
	gotSend := <-seen
	if gotSend.to != "bob" || gotSend.message != "hello" {
		t.Fatalf("send observation = %+v", gotSend)
	}
	assertJSONEqual(t, gotSend.metadata, []byte(metadata))

	d.send(t, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list_peers","arguments":{},"_meta":`+metadata+`}}`)
	var peersResult struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	d.receive(t, &peersResult)
	if peersResult.Result.IsError || peersResult.Result.Content[0].Text != "Connected peers: alice, bob" {
		t.Fatalf("peers result = %+v", peersResult.Result)
	}
	assertJSONEqual(t, (<-seen).metadata, []byte(metadata))
}

func TestHelperMapsValidationRejectionAndHandlerErrors(t *testing.T) {
	t.Parallel()
	var sendCalls int
	controller, _ := listenTestController(t, HandlerFuncs{
		SendMessageFunc: func(_ context.Context, _ json.RawMessage, _, _ string) (wire.SendAck, error) {
			sendCalls++
			return wire.SendAck{OK: false, Code: wire.CodeNoSuchPeer, Message: "gone"}, nil
		},
		ListPeersFunc: func(context.Context, json.RawMessage) ([]string, error) {
			return nil, errors.New("broker offline")
		},
	}, nil)
	d := startHelper(t, controller.SocketPath(), testToken)

	d.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"bad peer","message":"x"}}}`)
	text, isError := receiveToolResult(t, d)
	if !isError || !strings.Contains(text, "invalid destination") || sendCalls != 0 {
		t.Fatalf("validation text=%q isError=%v calls=%d", text, isError, sendCalls)
	}

	d.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"bob","message":"x"}}}`)
	text, isError = receiveToolResult(t, d)
	if !isError || text != "send rejected (no_such_peer): gone" || sendCalls != 1 {
		t.Fatalf("rejection text=%q isError=%v calls=%d", text, isError, sendCalls)
	}

	d.send(t, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}`)
	text, isError = receiveToolResult(t, d)
	if !isError || !strings.Contains(text, "list_peers failed:") || !strings.Contains(text, "broker offline") {
		t.Fatalf("failure text=%q isError=%v", text, isError)
	}
}

func TestHelperStartupPingFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	err := RunHelper(t.Context(), HelperOptions{
		SocketPath: filepath.Join(dir, "missing.sock"),
		Token:      testToken,
		Timeout:    50 * time.Millisecond,
		Stdin:      strings.NewReader(""),
		Stdout:     io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "startup ping") {
		t.Fatalf("error = %v", err)
	}
}

func TestHelperOptionValidation(t *testing.T) {
	t.Parallel()
	if err := RunHelper(nil, HelperOptions{}); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("nil context error = %v", err)
	}
	if err := RunHelper(t.Context(), HelperOptions{}); err == nil || !strings.Contains(err.Error(), "stdin") {
		t.Fatalf("nil stdin error = %v", err)
	}
	if err := RunHelper(t.Context(), HelperOptions{Stdin: strings.NewReader("")}); err == nil || !strings.Contains(err.Error(), "stdout") {
		t.Fatalf("nil stdout error = %v", err)
	}
}

func receiveToolResult(t *testing.T, d *helperDriver) (string, bool) {
	t.Helper()
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	d.receive(t, &response)
	if len(response.Result.Content) != 1 {
		t.Fatalf("content = %+v", response.Result.Content)
	}
	return response.Result.Content[0].Text, response.Result.IsError
}
