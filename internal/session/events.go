// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Event log bounds: the oldest events are dropped beyond either.
const (
	maxEvents     = 10000
	maxEventBytes = 8 << 20
	// eventOverhead is the size counted per event besides its strings.
	eventOverhead = 128
)

// Limits on how many events one query returns.
const (
	defaultEventLimit = 100
)

// eventLog is a session's event log: a bounded, in-memory ring of events
// with a global sequence number from 1. Events are immutable once appended
// (their pointer fields are copies owned by the log), so queries hand out
// shallow copies.
type eventLog struct {
	now      func() time.Time
	maxCount int
	maxBytes int

	mu      sync.Mutex
	events  []api.Event // oldest first; their seqs are consecutive
	bytes   int
	seq     int           // the newest seq; 0 before the first event
	changed chan struct{} // closed and replaced by every append
	// sink sees every event, in order, under mu (the recording).
	sink func(api.Event)
}

func newEventLog(maxCount, maxBytes int, now func() time.Time) *eventLog {
	return &eventLog{now: now, maxCount: maxCount, maxBytes: maxBytes, changed: make(chan struct{})}
}

// setSink sets the function that sees every event appended from now on.
func (l *eventLog) setSink(sink func(api.Event)) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sink = sink
}

// append assigns e its seq and time, adds it, drops the oldest events beyond
// the bounds and wakes waiters. It returns e as stored.
func (l *eventLog) append(e api.Event) api.Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	e.Seq, e.Time = l.seq, l.now()

	l.events = append(l.events, e)
	l.bytes += eventSize(&e)
	l.trimLocked()

	close(l.changed)
	l.changed = make(chan struct{})

	if l.sink != nil {
		l.sink(e)
	}

	return e
}

// trimLocked drops the oldest events beyond the bounds, always keeping the
// newest one.
func (l *eventLog) trimLocked() {
	n := 0
	for len(l.events)-n > 1 && (len(l.events)-n > l.maxCount || l.bytes > l.maxBytes) {
		l.bytes -= eventSize(&l.events[n])
		n++
	}

	if n > 0 {
		clear(l.events[:n]) // let the dropped events' strings go
		l.events = l.events[n:]
	}
}

// latest returns the newest seq (0: no events yet).
func (l *eventLog) latest() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.seq
}

// query returns the events q asks for, without waiting.
func (l *eventLog) query(q api.EventsParams) api.EventsResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.queryLocked(q)
}

// wait returns once an event matching q exists or ctx ends (TimedOut). A
// Since of -1 means events after the newest one now.
func (l *eventLog) wait(ctx context.Context, q api.EventsParams) api.EventsResult {
	l.mu.Lock()

	if q.Since < 0 {
		q.Since = l.seq
	}

	for {
		res := l.queryLocked(q)
		ch := l.changed
		l.mu.Unlock()

		if len(res.Events) > 0 {
			return res
		}

		select {
		case <-ch:
		case <-ctx.Done():
			res = l.query(q)
			res.TimedOut = len(res.Events) == 0

			return res
		}

		l.mu.Lock()
	}
}

// queryLocked selects the events after q.Since of q.Kinds (all if empty):
// the oldest q.Limit of them, or the newest with q.Newest.
func (l *eventLog) queryLocked(q api.EventsParams) api.EventsResult {
	res := api.EventsResult{Events: []api.Event{}, Latest: l.seq}

	since := q.Since
	if since < 0 {
		since = l.seq
	}

	first := l.seq - len(l.events) + 1 // seq of events[0]
	if since+1 < first {
		res.Dropped = first - since - 1
	}

	held := l.events[min(max(since+1-first, 0), len(l.events)):]
	limit := eventLimit(q.Limit)
	match := func(e *api.Event) bool { return len(q.Kinds) == 0 || slices.Contains(q.Kinds, e.Kind) }

	if q.Newest {
		for i := len(held) - 1; i >= 0; i-- {
			if !match(&held[i]) {
				continue
			}

			if len(res.Events) < limit {
				res.Events = append(res.Events, held[i])
			} else {
				res.More++
			}
		}

		slices.Reverse(res.Events)

		return res
	}

	for i := range held {
		if !match(&held[i]) {
			continue
		}

		if len(res.Events) < limit {
			res.Events = append(res.Events, held[i])
		} else {
			res.More++
		}
	}

	return res
}

// outputSince returns the output events after seq since as output lines.
func (l *eventLog) outputSince(since int) []api.OutputLine {
	l.mu.Lock()
	defer l.mu.Unlock()

	first := l.seq - len(l.events) + 1

	var out []api.OutputLine

	held := l.events[min(max(since+1-first, 0), len(l.events)):]
	for i := range held {
		if e := &held[i]; e.Kind == api.EventOutput {
			out = append(out, api.OutputLine{Seq: e.Seq, Category: e.Category, Text: e.Text})
		}
	}

	return out
}

// outputUntil returns the held output events with seq at most seq as output
// lines, oldest first.
func (l *eventLog) outputUntil(seq int) []api.OutputLine {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []api.OutputLine

	for i := range l.events {
		e := &l.events[i]
		if e.Seq > seq {
			break
		}

		if e.Kind == api.EventOutput {
			out = append(out, api.OutputLine{Seq: e.Seq, Category: e.Category, Text: e.Text})
		}
	}

	return out
}

// eventLimit applies the default and the maximum to a requested limit.
func eventLimit(n int) int {
	if n <= 0 {
		return defaultEventLimit
	}

	return min(n, api.MaxEventsLimit)
}

// eventSize is what an event counts against the log's byte bound: its
// strings plus a fixed overhead.
func eventSize(e *api.Event) int {
	n := eventOverhead + len(e.Client) + len(e.Action) + len(e.Program) + len(e.Previous) +
		len(e.Reason) + len(e.Category) + len(e.Text)

	if e.Stop != nil {
		n += len(e.Stop.Reason) + len(e.Stop.Description) + len(e.Stop.Text)
		for _, b := range e.Stop.Breakpoints {
			n += len(b.Owner) + 8
		}
	}

	if b := e.Breakpoint; b != nil {
		n += len(b.File) + len(b.Condition) + len(b.Message) + len(b.Note) + len(b.Owner)
	}

	if e.Lease != nil {
		n += len(e.Lease.Holder)
	}

	return n
}
