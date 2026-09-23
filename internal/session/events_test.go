// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func fixedNow() time.Time { return time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC) }

func seqs(events []api.Event) []int {
	out := []int{}
	for i := range events {
		out = append(out, events[i].Seq)
	}

	return out
}

// kindLog returns a log holding one event per kind given, seq 1 first.
func kindLog(kinds ...api.EventKind) *eventLog {
	l := newEventLog(maxEvents, maxEventBytes, fixedNow)
	for _, k := range kinds {
		l.append(api.Event{Kind: k})
	}

	return l
}

func TestEventLogQuery(t *testing.T) {
	t.Parallel()

	out, bp, lease := api.EventOutput, api.EventBreakpoint, api.EventLease
	// seq:           1    2   3      4    5   6    7
	l := kindLog(out, bp, lease, out, bp, out, out)

	tests := []struct {
		name      string
		q         api.EventsParams
		wantSeqs  []int
		wantMore  int
		wantLates int
	}{
		{"all", api.EventsParams{}, []int{1, 2, 3, 4, 5, 6, 7}, 0, 7},
		{"since", api.EventsParams{Since: 4}, []int{5, 6, 7}, 0, 7},
		{"since the newest", api.EventsParams{Since: 7}, []int{}, 0, 7},
		{"since beyond the newest", api.EventsParams{Since: 99}, []int{}, 0, 7},
		{"from now", api.EventsParams{Since: -1}, []int{}, 0, 7},
		{"limit keeps the oldest", api.EventsParams{Limit: 3}, []int{1, 2, 3}, 4, 7},
		{"newest keeps the newest", api.EventsParams{Limit: 3, Newest: true}, []int{5, 6, 7}, 4, 7},
		{"kinds", api.EventsParams{Kinds: []api.EventKind{bp, lease}}, []int{2, 3, 5}, 0, 7},
		{"kinds with limit", api.EventsParams{Kinds: []api.EventKind{out}, Limit: 2}, []int{1, 4}, 2, 7},
		{"kinds newest since", api.EventsParams{Since: 1, Kinds: []api.EventKind{out}, Limit: 2, Newest: true}, []int{6, 7}, 1, 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := l.query(tt.q)
			if got := seqs(res.Events); !reflect.DeepEqual(got, tt.wantSeqs) || res.More != tt.wantMore || res.Latest != tt.wantLates || res.Dropped != 0 {
				t.Errorf("query(%+v) = seqs %v more %d latest %d dropped %d; want %v, %d, %d, 0",
					tt.q, got, res.More, res.Latest, res.Dropped, tt.wantSeqs, tt.wantMore, tt.wantLates)
			}
		})
	}
}

func TestEventLogLimits(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct{ in, want int }{{0, defaultEventLimit}, {-3, defaultEventLimit}, {5, 5}, {5000, api.MaxEventsLimit}} {
		if got := eventLimit(tt.in); got != tt.want {
			t.Errorf("eventLimit(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestEventLogCaps(t *testing.T) {
	t.Parallel()

	t.Run("count", func(t *testing.T) {
		t.Parallel()

		l := newEventLog(5, maxEventBytes, fixedNow)
		for range 8 {
			l.append(api.Event{Kind: api.EventThread})
		}

		res := l.query(api.EventsParams{Since: 1})
		if got := seqs(res.Events); !reflect.DeepEqual(got, []int{4, 5, 6, 7, 8}) || res.Dropped != 2 || res.Latest != 8 {
			t.Errorf("after 8 appends to a log of 5: seqs %v dropped %d latest %d; want [4..8], 2, 8", got, res.Dropped, res.Latest)
		}

		if res := l.query(api.EventsParams{Since: 5}); res.Dropped != 0 {
			t.Errorf("since a held event: dropped = %d, want 0", res.Dropped)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		t.Parallel()

		text := strings.Repeat("x", 1000)
		size := eventSize(&api.Event{Kind: api.EventOutput, Text: text})
		l := newEventLog(maxEvents, 3*size, fixedNow)

		for range 5 {
			l.append(api.Event{Kind: api.EventOutput, Text: text})
		}

		res := l.query(api.EventsParams{})
		if got := seqs(res.Events); !reflect.DeepEqual(got, []int{3, 4, 5}) || res.Dropped != 2 {
			t.Errorf("seqs %v dropped %d; want [3 4 5], 2", got, res.Dropped)
		}

		// One event bigger than the whole bound is still kept.
		l.append(api.Event{Kind: api.EventOutput, Text: strings.Repeat("y", 10*size)})

		if got := seqs(l.query(api.EventsParams{}).Events); !reflect.DeepEqual(got, []int{6}) {
			t.Errorf("after a huge event: seqs %v, want [6]", got)
		}
	})
}

func TestEventLogOutputSince(t *testing.T) {
	t.Parallel()

	l := newEventLog(maxEvents, maxEventBytes, fixedNow)
	l.append(api.Event{Kind: api.EventOutput, Category: "stdout", Text: "a"})
	l.append(api.Event{Kind: api.EventStopped})
	l.append(api.Event{Kind: api.EventOutput, Category: "stderr", Text: "b"})

	want := []api.OutputLine{{Seq: 3, Category: "stderr", Text: "b"}}
	if got := l.outputSince(1); !reflect.DeepEqual(got, want) {
		t.Errorf("outputSince(1) = %+v, want %+v", got, want)
	}
}

func TestEventLogWait(t *testing.T) {
	t.Parallel()

	t.Run("wakes on append from another goroutine", func(t *testing.T) {
		t.Parallel()

		l := kindLog(api.EventStarted)
		done := make(chan api.EventsResult)

		go func() {
			done <- l.wait(t.Context(), api.EventsParams{Since: 1, Kinds: []api.EventKind{api.EventLease}})
		}()

		// Whether the waiter waits already or only starts after these
		// appends, it must skip the output event and return the lease one.
		l.append(api.Event{Kind: api.EventOutput})
		l.append(api.Event{Kind: api.EventLease, Action: "take"})

		res := <-done
		if len(res.Events) != 1 || res.Events[0].Kind != api.EventLease || res.TimedOut {
			t.Errorf("wait = %+v, want the lease event", res)
		}
	})

	t.Run("returns at once when events exist", func(t *testing.T) {
		t.Parallel()

		l := kindLog(api.EventStarted, api.EventThread)

		res := l.wait(t.Context(), api.EventsParams{Since: 1})
		if got := seqs(res.Events); !reflect.DeepEqual(got, []int{2}) {
			t.Errorf("wait = %v, want [2]", got)
		}
	})

	t.Run("times out", func(t *testing.T) {
		t.Parallel()

		l := kindLog(api.EventStarted)

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()

		res := l.wait(ctx, api.EventsParams{Since: 1})
		if !res.TimedOut || len(res.Events) != 0 || res.Latest != 1 {
			t.Errorf("wait = %+v, want a timeout with latest 1", res)
		}
	})

	t.Run("from now", func(t *testing.T) {
		t.Parallel()

		l := kindLog(api.EventStarted, api.EventThread)

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()

		if res := l.wait(ctx, api.EventsParams{Since: -1}); !res.TimedOut || len(res.Events) != 0 {
			t.Errorf("wait from now = %+v, want no events (the existing ones are before now)", res)
		}
	})

	t.Run("ctx canceled", func(t *testing.T) {
		t.Parallel()

		l := kindLog()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if res := l.wait(ctx, api.EventsParams{Since: -1}); !res.TimedOut {
			t.Errorf("wait = %+v, want TimedOut", res)
		}
	})
}

// TestEventLogSinkOrder appends from many goroutines: the sink must see
// every event once, in seq order.
func TestEventLogSinkOrder(t *testing.T) {
	t.Parallel()

	l := newEventLog(maxEvents, maxEventBytes, fixedNow)

	var got []int

	l.setSink(func(e api.Event) { got = append(got, e.Seq) }) // runs under the log's lock

	const n = 200

	var wg sync.WaitGroup

	for range n {
		wg.Go(func() { l.append(api.Event{Kind: api.EventThread}) })
	}

	wg.Wait()

	for i, s := range got {
		if s != i+1 {
			t.Fatalf("sink saw seq %d at position %d, want %d", s, i, i+1)
		}
	}

	if len(got) != n {
		t.Errorf("sink saw %d events, want %d", len(got), n)
	}
}
