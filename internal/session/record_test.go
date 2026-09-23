// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// memFile is an in-memory io.WriteCloser.
type memFile struct {
	bytes.Buffer

	closed int
}

func (f *memFile) Close() error {
	f.closed++

	return nil
}

func decodeLines(t *testing.T, b []byte) []api.Event {
	t.Helper()

	var out []api.Event

	for line := range strings.Lines(string(b)) {
		var e api.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}

		out = append(out, e)
	}

	return out
}

// TestRecorder records control events only, without the program's data.
func TestRecorder(t *testing.T) {
	t.Parallel()

	f := &memFile{}
	r := newRecorder(f, maxRecording, slog.New(slog.DiscardHandler))

	r.record(api.Event{Seq: 1, Kind: api.EventStarted, Program: "/p/app.dll"})
	r.record(api.Event{Seq: 2, Kind: api.EventOutput, Category: "stdout", Text: "synthetic-output"})
	r.record(api.Event{Seq: 3, Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "exception", ThreadID: 7, Description: "synthetic-desc", Text: "synthetic-text"}})
	r.record(api.Event{Seq: 4, Kind: api.EventEnded, Reason: "the program terminated"})
	r.record(api.Event{Seq: 5, Kind: api.EventClient, Client: "human:late"}) // after ended: ignored

	if strings.Contains(f.String(), "synthetic") {
		t.Errorf("recording holds program data: %s", f.String())
	}

	got := decodeLines(t, f.Bytes())
	if len(got) != 3 || got[0].Kind != api.EventStarted || got[1].Kind != api.EventStopped || got[2].Kind != api.EventEnded {
		t.Fatalf("recorded %+v, want started, stopped, ended", got)
	}

	if s := got[1].Stop; s == nil || s.Reason != "exception" || s.ThreadID != 7 {
		t.Errorf("stop = %+v, want reason and thread kept", s)
	}

	if f.closed != 1 {
		t.Errorf("closed %d times after ended, want 1", f.closed)
	}

	r.close()

	if f.closed != 1 {
		t.Errorf("close after ended closed again")
	}
}

func TestRecorderSizeLimit(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer

	f := &memFile{}
	r := newRecorder(f, 150, slog.New(slog.NewTextHandler(&logs, nil)))

	for i := range 10 {
		r.record(api.Event{Seq: i + 1, Kind: api.EventThread, Reason: "started", ThreadID: i})
	}

	if f.Len() > 150 || f.Len() == 0 {
		t.Errorf("recorded %d bytes, want 1..150", f.Len())
	}

	if f.closed != 1 || strings.Count(logs.String(), "recording stopped") != 1 {
		t.Errorf("closed %d times, log %q; want closed once with one warning", f.closed, logs.String())
	}
}

func TestRecorderWriteAfterClose(t *testing.T) {
	t.Parallel()

	f := &memFile{}
	r := newRecorder(f, maxRecording, slog.New(slog.DiscardHandler))
	r.close()
	r.close()
	r.record(api.Event{Seq: 1, Kind: api.EventStarted})

	if f.Len() != 0 || f.closed != 1 {
		t.Errorf("wrote %d bytes, closed %d times; want 0 and 1", f.Len(), f.closed)
	}
}
