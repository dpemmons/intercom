package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestNameCommandReportsOutputFailure(t *testing.T) {
	t.Setenv("INTERCOM_NAME", "reviewer")
	cmd := newNameCmd()
	cmd.SetOut(failingWriter{})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "name: write output: write failed") {
		t.Fatalf("Execute() error = %v, want output failure", err)
	}
}

func TestWritePeerList(t *testing.T) {
	for _, test := range []struct {
		name  string
		peers []string
		want  string
	}{
		{name: "empty", want: "(no other peers connected)\n"},
		{name: "peers", peers: []string{"builder", "reviewer"}, want: "builder\nreviewer\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writePeerList(&output, test.peers); err != nil {
				t.Fatalf("writePeerList() error = %v", err)
			}
			if got := output.String(); got != test.want {
				t.Fatalf("writePeerList() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestWritePeerListReportsOutputFailure(t *testing.T) {
	for _, peers := range [][]string{nil, {"reviewer"}} {
		if err := writePeerList(failingWriter{}, peers); err == nil || err.Error() != "write failed" {
			t.Fatalf("writePeerList(%v) error = %v, want write failed", peers, err)
		}
	}
}
