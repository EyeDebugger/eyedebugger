// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package present

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Size caps for results that grow with the program's output. They keep a
// response well under the wire's 1 MiB message limit.
const (
	// MaxResultBytes caps the events or output in one result, as JSON.
	MaxResultBytes = 512 << 10
	// EventTextLimit is the most characters of output text one event
	// carries in an events result.
	EventTextLimit = 1000
)

// ShapeEvents cuts each event's text to [EventTextLimit] characters and keeps
// as many events as fit budget tokens (0: no budget) and [MaxResultBytes]:
// the oldest ones, or the newest with newest. At least one event is kept. It
// returns the events kept, in order, and how many were left out.
func ShapeEvents(events []api.Event, budget int, newest bool) (kept []api.Event, omitted int) {
	limit := MaxResultBytes
	if budget > 0 {
		limit = min(limit, budget*CharsPerToken)
	}

	out := make([]api.Event, len(events))
	copy(out, events)

	for i := range out {
		if e := &out[i]; utf8.RuneCountInString(e.Text) > EventTextLimit {
			e.Text, e.Truncated = string([]rune(e.Text)[:EventTextLimit]), true
		}
	}

	n := fit(out, limit, newest, func(e *api.Event) int { return jsonSize(e) })
	if newest {
		return out[len(out)-n:], len(out) - n
	}

	return out[:n], len(out) - n
}

// CapOutput keeps as many output chunks as fit maxBytes of JSON: the oldest,
// or the newest with newest. At least one chunk is kept; if it alone is too
// big its text is cut to maxBytes bytes, keeping its end with newest (the
// latest output) and its start otherwise. It returns the chunks kept and how
// many were left out.
func CapOutput(lines []api.OutputLine, maxBytes int, newest bool) (kept []api.OutputLine, omitted int) {
	n := fit(lines, maxBytes, newest, func(l *api.OutputLine) int { return jsonSize(l) })

	if newest {
		kept = lines[len(lines)-n:]
	} else {
		kept = lines[:n]
	}

	if n == 1 && jsonSize(&kept[0]) > maxBytes {
		l := kept[0]
		l.Text = CutText(l.Text, maxBytes, newest)
		kept = []api.OutputLine{l}
	}

	return kept, len(lines) - n
}

// fit returns how many items, counted from the start (or the end with
// fromEnd), fit limit bytes by size; at least one if there are any.
func fit[T any](items []T, limit int, fromEnd bool, size func(*T) int) int {
	total := 0

	for n := range items {
		i := n
		if fromEnd {
			i = len(items) - 1 - n
		}

		total += size(&items[i])
		if total > limit && n > 0 {
			return n
		}
	}

	return len(items)
}

// CutText returns at most n bytes of s, cut on a rune boundary: its end if
// keepEnd, else its start.
func CutText(s string, n int, keepEnd bool) string {
	if len(s) <= n {
		return s
	}

	if keepEnd {
		i := len(s) - n
		for i < len(s) && !utf8.RuneStart(s[i]) {
			i++
		}

		return s[i:]
	}

	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}

	return s[:i]
}

// jsonSize is the size of v encoded as JSON.
func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}

	return len(b)
}
