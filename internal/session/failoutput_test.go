// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// TestFailedStartOutput: a launch that fails once the adapter logged
// stderr or important output carries it on the error (D3, D3b); stdout and
// console are excluded, and it is bounded to the last lines/bytes.
func TestFailedStartOutput(t *testing.T) {
	t.Parallel()

	var longLines []string
	for i := range 30 {
		longLines = append(longLines, "line "+strconv.Itoa(i))
	}

	tests := []struct {
		name    string
		outputs []daptest.OutputEvent
		want    string
	}{
		{
			name: "stderr kept, stdout excluded",
			outputs: []daptest.OutputEvent{
				{Category: "stdout", Text: "go build ./...\n"},
				{Category: "stderr", Text: "Build Error: go build\nsyntax error\n"},
			},
			want: "Build Error: go build\nsyntax error",
		},
		{
			name:    "important kept",
			outputs: []daptest.OutputEvent{{Category: "important", Text: "important note\n"}},
			want:    "important note",
		},
		{
			name:    "console excluded: no output at all",
			outputs: []daptest.OutputEvent{{Category: "console", Text: "noise\n"}},
			want:    "",
		},
		{
			name:    "more than 20 lines: the last 20 kept",
			outputs: []daptest.OutputEvent{{Category: "stderr", Text: strings.Join(longLines, "\n") + "\n"}},
			want:    strings.Join(longLines[len(longLines)-maxFailedStartLines:], "\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManagerWith(t, nil, fakeDriver{opts: daptest.Options{FailLaunch: true, LaunchOutputs: tt.outputs}})

			_, err := m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t)}})

			e, ok := err.(*api.Error) //nolint:errorlint // Returned unwrapped (a failed start's own error).
			if !ok {
				t.Fatalf("start err = %#v, want *api.Error", err)
			}

			if e.Output != tt.want {
				t.Errorf("Output = %q, want %q", e.Output, tt.want)
			}
		})
	}
}

// TestFailedStartOutputBytes: more than 4 KiB of output on a single line (so
// the 20-line cap never triggers) is cut to that many bytes, keeping the
// end, on a rune boundary: multi-byte runes so a naive byte cut could split
// one in half (F1).
func TestFailedStartOutputBytes(t *testing.T) {
	t.Parallel()

	line := strings.Repeat("é", 3000) // 6000 bytes, no newline, well over maxFailedStartBytes
	m := newTestManagerWith(t, nil, fakeDriver{opts: daptest.Options{
		FailLaunch:    true,
		LaunchOutputs: []daptest.OutputEvent{{Category: "stderr", Text: line}},
	}})

	_, err := m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t)}})

	e, ok := err.(*api.Error) //nolint:errorlint // Returned unwrapped.
	if !ok {
		t.Fatalf("start err = %#v, want *api.Error", err)
	}

	if len(e.Output) == 0 || len(e.Output) > maxFailedStartBytes {
		t.Errorf("Output is %d bytes, want (0, %d]", len(e.Output), maxFailedStartBytes)
	}

	if !strings.HasSuffix(line, e.Output) {
		t.Errorf("Output = %q, want a suffix of the original line", e.Output)
	}

	if !utf8.ValidString(e.Output) {
		t.Errorf("Output = %q is not valid UTF-8 (cut mid-rune)", e.Output)
	}
}

// TestFailedStartOutputNotLogged: the daemon log carries the failed
// start's message, never its adapter output (rule 9): only *api.Error's
// Message reaches slog (its Error() method), and JSONHandler special-cases
// errors the same way, calling it instead of marshaling the struct.
func TestFailedStartOutputNotLogged(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	m := NewManager(ctx, Config{
		Drivers: []Driver{fakeDriver{opts: daptest.Options{
			FailLaunch:    true,
			LaunchOutputs: []daptest.OutputEvent{{Category: "stderr", Text: "Build Error: secret-looking output\n"}},
		}}},
		Logger: slog.New(slog.NewJSONHandler(&buf, nil)),
		Stderr: bytes.NewBuffer(nil),
	})
	t.Cleanup(func() { m.StopAll(context.Background()) })

	_, err := m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", LaunchSpec: api.LaunchSpec{Program: fakeProgram(t)}})

	e, ok := err.(*api.Error) //nolint:errorlint // Returned unwrapped.
	if !ok || e.Output == "" {
		t.Fatalf("start err = %#v, want *api.Error with Output set", err)
	}

	if strings.Contains(buf.String(), "secret-looking output") {
		t.Errorf("the daemon log carries the adapter's output: %s", buf.String())
	}

	if !strings.Contains(buf.String(), "Failed to launch") {
		t.Errorf("the daemon log doesn't carry the failure at all: %s", buf.String())
	}
}
