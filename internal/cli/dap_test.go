// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// TestDapErrors: without a daemon, and for an unknown session, errors go
// to stderr, text or JSON, and stdout stays empty. Not parallel: it sets
// environment variables.
func TestDapErrors(t *testing.T) {
	isolate(t)

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"dap"})
	if code != exitState || len(stdout) != 0 || !strings.Contains(string(stderr), "[NO_SESSION]") {
		t.Errorf("dap without a daemon: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	stdout, stderr, code = execute(t, NewEyedbgCommand(testInfo), []string{"dap", "--json"})

	var doc errorOutput
	if code != exitState || len(stdout) != 0 || json.Unmarshal(stderr, &doc) != nil || doc.Error.Code != api.CodeNoSession {
		t.Errorf("dap --json without a daemon: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestDapUnknownSession(t *testing.T) {
	serveInProcess(t, isolate(t))

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"dap", "-s", "s-none"})
	if code != exitState || len(stdout) != 0 || !strings.Contains(string(stderr), "[NO_SESSION]") {
		t.Errorf("dap -s s-none: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestDapOpenError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   error
		want api.Code
	}{
		{in: api.NewError(api.CodeUnknownMethod, "unknown method facade.open", ""), want: api.CodeVersionMismatch},
		{in: api.NewError(api.CodeSessionExited, "exited", ""), want: api.CodeSessionExited},
		{in: errors.New("broken pipe"), want: ""},
	}

	for _, tt := range tests {
		if got := api.CodeOf(openError(tt.in)); got != tt.want {
			t.Errorf("openError(%v) code = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestDapBridge joins a session through 'eyedbg dap': a DAP request on
// stdin gets its response on stdout, and closing stdin ends the command
// with 0 once the daemon closed the connection.
func TestDapBridge(t *testing.T) {
	serveInProcess(t, isolate(t))

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	run(t, 0, "start", "fake", "--program", prog, "--stop-on-entry")

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	var stderr syncBuffer

	root := NewEyedbgCommand(testInfo)
	root.SetIn(inR)
	root.SetOut(outW)
	root.SetErr(&stderr)

	done := make(chan int, 1)

	go func() {
		code := Run(t.Context(), root, []string{"dap", "--as", "human:t"})
		_ = outW.Close()
		done <- code
	}()

	req := `{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"x"}}`
	if _, err := io.WriteString(inW, "Content-Length: "+strconv.Itoa(len(req))+"\r\n\r\n"+req); err != nil {
		t.Fatal(err)
	}

	raw, err := dap.ReadMessage(bufio.NewReader(outR), 1<<20)
	if err != nil || !bytes.Contains(raw, []byte(`"command":"initialize"`)) || !bytes.Contains(raw, []byte(`"success":true`)) {
		t.Fatalf("stdout = %s, %v; stderr %s", raw, err, stderr.String())
	}

	_ = inW.Close()

	go func() { _, _ = io.Copy(io.Discard, outR) }()

	select {
	case code := <-done:
		if code != 0 || stderr.String() != "" {
			t.Errorf("dap after stdin closed: exit %d, stderr %q", code, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("eyedbg dap did not end after stdin closed")
	}

	out := run(t, 0, "events", "--as", "human:t")
	expectOutput(t, out, "human:t")
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}
