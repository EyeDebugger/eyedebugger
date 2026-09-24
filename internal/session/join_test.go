// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// bareSession returns a session that never started an adapter: it stays
// starting until the test changes it.
func bareSession(t *testing.T) *Session {
	t.Helper()

	return newSession(t.Context(), "s-test", "fake", "", Launch{}, slog.New(slog.DiscardHandler), agentC, api.LeaseFree)
}

func TestReady(t *testing.T) {
	t.Parallel()

	t.Run("waits for the end of starting", func(t *testing.T) {
		t.Parallel()

		s := bareSession(t)
		done := make(chan error, 1)

		go func() {
			caps, err := s.Ready(t.Context())
			if err == nil && !caps.SupportsConfigurationDoneRequest {
				err = api.NewError(api.CodeInternal, "wrong capabilities", "")
			}

			done <- err
		}()

		select {
		case err := <-done:
			t.Fatalf("Ready returned %v while the session was starting", err)
		default:
		}

		s.setCaps(daptest.DefaultCaps())

		s.mu.Lock()
		s.setStateLocked(api.StateRunning)
		s.mu.Unlock()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Ready = %v", err)
			}
		case <-time.After(testWait):
			t.Fatal("Ready did not return once the session ran")
		}
	})

	t.Run("times out while starting", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()

		_, err := bareSession(t).Ready(ctx)
		expectCode(t, err, api.CodeNotStopped)
	})

	t.Run("exited before the adapter answered", func(t *testing.T) {
		t.Parallel()

		s := bareSession(t)
		s.mu.Lock()
		s.setStateLocked(api.StateExited)
		s.mu.Unlock()

		_, err := s.Ready(t.Context())
		expectCode(t, err, api.CodeSessionExited)
	})
}

// TestJoinPointAndFollow: events after the join point come in order, none
// from before it.
func TestJoinPointAndFollow(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	addBP(t, s, agentC, s.Program, 3, "")

	info, seq := s.JoinPoint()
	if info.State != api.StateStopped || info.Stop == nil || !info.Stop.AllThreadsStopped {
		t.Fatalf("join info = %+v, want stopped with allThreadsStopped", info)
	}

	if seq != s.log.latest() {
		t.Fatalf("join seq = %d, log latest = %d", seq, s.log.latest())
	}

	before, err := s.Exec(t.Context(), agentC, ExecContinue, 0)
	if err != nil || before != 1 {
		t.Fatalf("Exec = %d, %v; want 1 stop before", before, err)
	}

	got := followUntilStop(t, s, seq)

	if got[0].Kind != api.EventExec || got[0].Action != ExecContinue || got[0].Client != agentC.ID {
		t.Errorf("first event after the join = %+v, want the agent's exec continue", got[0])
	}

	stop := got[len(got)-1].Stop
	if stop == nil || stop.Reason != "breakpoint" || !stop.AllThreadsStopped {
		t.Errorf("stop = %+v, want a breakpoint stop with allThreadsStopped", stop)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if res := s.Follow(ctx, s.log.latest()); !res.TimedOut || len(res.Events) != 0 {
		t.Errorf("Follow with nothing new and ctx done = %+v", res)
	}
}

// followUntilStop follows s's log after seq until a stopped event, checking
// that seqs are consecutive, and returns the events.
func followUntilStop(t *testing.T, s *Session, seq int) []api.Event {
	t.Helper()

	var got []api.Event

	for !slices.ContainsFunc(got, func(e api.Event) bool { return e.Kind == api.EventStopped }) {
		ctx, cancel := context.WithTimeout(t.Context(), testWait)
		res := s.Follow(ctx, seq)
		cancel()

		if res.TimedOut || res.Dropped != 0 {
			t.Fatalf("Follow = %+v, events so far %v", res, kindsAndActions(got))
		}

		for i := range res.Events {
			if res.Events[i].Seq != seq+1+i {
				t.Fatalf("event seqs %v are not consecutive from %d", res.Events, seq+1)
			}
		}

		got = append(got, res.Events...)
		seq = got[len(got)-1].Seq
	}

	return got
}

func TestFollowReportsDropped(t *testing.T) {
	t.Parallel()

	s := bareSession(t)
	s.log = newEventLog(5, 1<<20, time.Now)

	for range 8 {
		s.log.append(api.Event{Kind: api.EventOutput, Text: "x"})
	}

	res := s.Follow(t.Context(), 0)
	if res.Dropped != 3 || len(res.Events) != 5 || res.Events[0].Seq != 4 {
		t.Errorf("Follow(0) = %+v, want 3 dropped and seqs 4..8", res)
	}
}

func TestSupportedExceptionModes(t *testing.T) {
	t.Parallel()

	noFilters := daptest.DefaultCaps()
	noFilters.ExceptionBreakpointFilters = nil

	tests := []struct {
		name string
		drv  fakeDriver
		want []api.ExceptionMode
	}{
		{name: "mode names as filters", drv: fakeDriver{}, want: []api.ExceptionMode{api.ExceptionsAll}},
		{
			name: "driver filters",
			drv:  fakeDriver{excFilters: map[api.ExceptionMode][]string{api.ExceptionsUncaught: {"user-unhandled"}}},
			want: []api.ExceptionMode{api.ExceptionsAll, api.ExceptionsUncaught},
		},
		{name: "no filters", drv: fakeDriver{opts: daptest.Options{Caps: &noFilters}}, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := start(t, newTestManagerWith(t, nil, tt.drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
			if got := s.SupportedExceptionModes(); !slices.Equal(got, tt.want) {
				t.Errorf("SupportedExceptionModes = %v, want %v", got, tt.want)
			}
		})
	}

	if got := bareSession(t).SupportedExceptionModes(); got != nil {
		t.Errorf("SupportedExceptionModes before initialize = %v, want none", got)
	}
}

// TestExecUnderHandoff: a refused Exec leaves no exec event.
func TestExecUnderHandoff(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{
		LaunchSpec: api.LaunchSpec{StopOnEntry: true}, LeasePolicy: api.LeaseHandoff,
	})

	_, err := s.Exec(t.Context(), humanC, ExecNext, 0)
	expectCode(t, err, api.CodeLeaseHeld)

	if n := len(eventsOf(s, api.EventExec)); n != 0 {
		t.Errorf("%d exec events after a refused Exec", n)
	}

	caps, known := s.Capabilities()
	if !known || !caps.SupportsConfigurationDoneRequest {
		t.Errorf("Capabilities = %+v, %v", caps, known)
	}

	caps.ExceptionBreakpointFilters[0] = godap.ExceptionBreakpointsFilter{Filter: "changed"}
	if again, _ := s.Capabilities(); again.ExceptionBreakpointFilters[0].Filter == "changed" {
		t.Error("Capabilities shares its filter list with the caller")
	}
}

func TestOutputBefore(t *testing.T) {
	t.Parallel()

	// chunks appends n output events of text size bytes and returns the
	// seq of the last one.
	chunks := func(l *eventLog, n, size int) int {
		for range n {
			l.append(api.Event{Kind: api.EventOutput, Category: "stdout", Text: strings.Repeat("x", size)})
		}

		return l.latest()
	}

	tests := []struct {
		name string
		// fill fills the log and returns the seq to ask for.
		fill         func(l *eventLog) int
		maxChunks    int
		maxBytes     int
		wantSeqs     []int
		wantOmitted  int
		wantCutFirst bool
	}{
		{
			name: "none", fill: func(l *eventLog) int { l.append(api.Event{Kind: api.EventLease}); return 1 },
			maxChunks: 10, maxBytes: 1 << 16,
		},
		{
			name: "fewer than max", fill: func(l *eventLog) int { return chunks(l, 3, 1) },
			maxChunks: 10, maxBytes: 1 << 16, wantSeqs: []int{1, 2, 3},
		},
		{
			name: "more than max", fill: func(l *eventLog) int { return chunks(l, 5, 1) },
			maxChunks: 2, maxBytes: 1 << 16, wantSeqs: []int{4, 5}, wantOmitted: 3,
		},
		{
			name: "bytes cap cuts the oldest", fill: func(l *eventLog) int { return chunks(l, 4, 100) },
			maxChunks: 10, maxBytes: 300, wantSeqs: []int{3, 4}, wantOmitted: 2,
		},
		{
			name: "one too big is cut", fill: func(l *eventLog) int { return chunks(l, 1, 1000) },
			maxChunks: 10, maxBytes: 100, wantSeqs: []int{1}, wantCutFirst: true,
		},
		{name: "after seq excluded", fill: func(l *eventLog) int {
			seq := chunks(l, 2, 1)
			l.append(api.Event{Kind: api.EventStopped})
			chunks(l, 2, 1)

			return seq
		}, maxChunks: 10, maxBytes: 1 << 16, wantSeqs: []int{1, 2}},
		{
			name: "only held ones counted", fill: func(l *eventLog) int { return chunks(l, 6, 1) },
			maxChunks: 2, maxBytes: 1 << 16, wantSeqs: []int{5, 6}, wantOmitted: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := bareSession(t)
			if tt.name == "only held ones counted" {
				// The ring holds 4 events: seqs 1 and 2 are dropped.
				s.log = newEventLog(4, maxEventBytes, fixedNow)
			}

			seq := tt.fill(s.log)

			got, omitted := s.OutputBefore(seq, tt.maxChunks, tt.maxBytes)

			var gotSeqs []int
			for _, l := range got {
				gotSeqs = append(gotSeqs, l.Seq)
			}

			if !slices.Equal(gotSeqs, tt.wantSeqs) || omitted != tt.wantOmitted {
				t.Errorf("OutputBefore = seqs %v, omitted %d; want %v, %d", gotSeqs, omitted, tt.wantSeqs, tt.wantOmitted)
			}

			if tt.wantCutFirst && len(got[0].Text) >= 1000 {
				t.Errorf("a chunk too big alone was not cut: %d bytes", len(got[0].Text))
			}
		})
	}
}
