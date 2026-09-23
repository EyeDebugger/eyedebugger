// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"io"
	"log/slog"
	"sync"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// maxRecording bounds one session's recording; recording stops there.
const maxRecording = 4 << 20

// recorder writes a session's control events as JSON lines (docs/DESIGN.md
// §11). It records what happened, not the program's data: no output, no stop
// text (exception messages can hold data), no variable values. It stops for
// good at the size bound, on a write error, and after the ended event.
type recorder struct {
	logger *slog.Logger
	limit  int

	mu      sync.Mutex
	w       io.WriteCloser // nil once stopped
	written int
}

func newRecorder(w io.WriteCloser, limit int, logger *slog.Logger) *recorder {
	return &recorder{w: w, limit: limit, logger: logger}
}

// record writes e if its kind is recorded. It never blocks on anything but
// the write itself.
func (r *recorder) record(e api.Event) {
	e, ok := recordable(e)
	if !ok {
		return
	}

	line, err := json.Marshal(e)
	if err != nil {
		return
	}

	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.w == nil {
		return
	}

	if r.written+len(line) > r.limit {
		r.logger.Warn("recording stopped: it reached its size limit", slog.Int("limit_bytes", r.limit))
		r.closeLocked()

		return
	}

	n, err := r.w.Write(line)
	r.written += n

	if err != nil {
		r.logger.Warn("recording stopped: write failed", slog.Any("error", err))
		r.closeLocked()

		return
	}

	if e.Kind == api.EventEnded {
		r.closeLocked()
	}
}

// close stops recording; it is idempotent.
func (r *recorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closeLocked()
}

func (r *recorder) closeLocked() {
	if r.w == nil {
		return
	}

	if err := r.w.Close(); err != nil {
		r.logger.Warn("close recording", slog.Any("error", err))
	}

	r.w = nil
}

// recordable returns what of e is recorded, and false if nothing is.
func recordable(e api.Event) (api.Event, bool) {
	switch e.Kind {
	case api.EventOutput:
		return api.Event{}, false
	case api.EventStopped:
		if e.Stop != nil {
			e.Stop = &api.StopInfo{Reason: e.Stop.Reason, ThreadID: e.Stop.ThreadID}
		}
	case api.EventStarted, api.EventClient, api.EventLease, api.EventExec, api.EventContinued,
		api.EventBreakpoint, api.EventThread, api.EventExited, api.EventEnded:
	default:
		return api.Event{}, false
	}

	e.Text, e.Truncated = "", false

	return e, true
}
