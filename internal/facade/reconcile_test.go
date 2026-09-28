// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"strconv"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// TestReconcileOnce: reconcileOnce runs reconcile once per batch — a later
// call while *ran is already true is skipped, and its output is nil — but
// a run that happened while the view wasn't configured doesn't set *ran,
// so the batch gets another try once it is (plan p2-followups step 4,
// ledger per-event-breakpoint-reconcile).
func TestReconcileOnce(t *testing.T) {
	t.Parallel()

	var (
		ran   bool
		calls int
	)

	reconcile := func() []godap.EventMessage {
		calls++

		return nil
	}

	for range 3 {
		reconcileOnce(&ran, false, reconcile)
	}

	if calls != 3 || ran {
		t.Fatalf("3 calls while unconfigured: calls = %d, ran = %v, want 3, false", calls, ran)
	}

	for range 5 {
		reconcileOnce(&ran, true, reconcile)
	}

	if calls != 4 || !ran {
		t.Fatalf("5 calls once configured: calls = %d, ran = %v, want 4 (one ran, four skipped), true", calls, ran)
	}

	reconcileOnce(&ran, false, reconcile)

	if calls != 4 {
		t.Fatalf("a call after ran: calls = %d, want 4 (still skipped)", calls)
	}
}

// TestReconcileOnceOutput: the output is reconcile's only on the call that
// ran; a skipped call gets nil.
func TestReconcileOnceOutput(t *testing.T) {
	t.Parallel()

	want := []godap.EventMessage{&godap.BreakpointEvent{}}
	reconcile := func() []godap.EventMessage { return want }

	var ran bool

	if out := reconcileOnce(&ran, true, reconcile); len(out) != 1 || out[0] != want[0] {
		t.Fatalf("first (running) call = %v, want %v", out, want)
	}

	if out := reconcileOnce(&ran, true, reconcile); out != nil {
		t.Fatalf("second (skipped) call = %v, want nil", out)
	}
}

// TestFollowBatchOrdersBreakpointsBeforeStopped: within one batch, a
// breakpoint event that precedes a stopped event naming it in the session's
// log is still relayed before that stopped event, and the batch's single
// reconcile (already covering every breakpoint the batch's later events
// announce) doesn't announce any of them twice (D5, F6). Calls
// [connection.translate] directly, as [connection.follow]'s inner loop
// does for one event at a time, so the batch's contents and order are
// exactly what this test picked, not whatever timing happened to deliver.
func TestFollowBatchOrdersBreakpointsBeforeStopped(t *testing.T) {
	t.Parallel()

	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, "", "lines=3"))

	placed, err := s.ReplaceBreakpoints(t.Context(), agentC, s.Program, []api.BreakpointSpec{{Line: 1}, {Line: 2}}, nil)
	if err != nil {
		t.Fatalf("ReplaceBreakpoints: %v", err)
	}

	if len(placed) != 2 {
		t.Fatalf("placed = %d, want 2", len(placed))
	}

	id1, id2 := placed[0].ID, placed[1].ID

	c := &connection{client: humanC, view: newView()}
	c.sess.Store(s)
	c.view.configured = true

	st := &followState{self: humanC.ID}

	// A batch as the log could plausibly deliver it: a breakpoint added,
	// then a stop the adapter reported at it, then another breakpoint
	// added — all before the follower's next poll.
	batch := []api.Event{
		{Kind: api.EventBreakpoint, Action: actionNew, Breakpoint: &api.Breakpoint{ID: id1}},
		{Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "breakpoint", ThreadID: 1, Breakpoints: []api.StopBreakpoint{{ID: id1}}}},
		{Kind: api.EventBreakpoint, Action: actionNew, Breakpoint: &api.Breakpoint{ID: id2}},
	}

	var (
		reconciled bool
		out        []godap.EventMessage
	)

	for i := range batch {
		out = append(out, c.translate(&batch[i], st, &reconciled)...)
	}

	if reconciles := c.reconciles.Load(); reconciles != 1 {
		t.Fatalf("reconciles for the batch = %d, want 1 (out = %v)", reconciles, out)
	}

	stoppedAt := -1
	newIDs := map[int]int{}

	for i, ev := range out {
		switch e := ev.(type) {
		case *godap.StoppedEvent:
			stoppedAt = i
		case *godap.BreakpointEvent:
			if e.Body.Reason == actionNew {
				newIDs[e.Body.Breakpoint.Id]++
			}
		}
	}

	if stoppedAt == -1 {
		t.Fatalf("no stopped event in %v", out)
	}

	for i, ev := range out {
		if _, ok := ev.(*godap.BreakpointEvent); ok && i > stoppedAt {
			t.Errorf("breakpoint event at %d came after stopped at %d: %v", i, stoppedAt, out)
		}
	}

	for _, id := range []int{id1, id2} {
		if newIDs[id] != 1 {
			t.Errorf("breakpoint %d announced new %d times, want 1 (out = %v)", id, newIDs[id], out)
		}
	}
}

// TestReconcileOncePerFollowBatch: many breakpoint events logged in one
// setBreakpoints-sized batch reconcile the mirroring connection's view once,
// not once per event, and every one of them still arrives correctly (ledger
// per-event-breakpoint-reconcile: reconcile is O(every breakpoint), so
// before this it cost that once per event in the batch instead of once per
// batch).
func TestReconcileOncePerFollowBatch(t *testing.T) {
	t.Parallel()

	const n = 500

	// Distinct lines, so every one gets its own mirror (same file, same
	// line collapses to one mirror — not what this measures).
	s := startSession(t, fakeDriver{}, startParams(stopOnEntry, "", "lines="+strconv.Itoa(n+1)))

	var observer *connection

	tc := joinWith(t, Config{Session: s, Client: humanC, testHook: func(c *connection) { observer = c }})
	tc.handshake("")
	tc.waitEvent("stopped", nil) // the entry stop, replayed

	base := observer.reconciles.Load() // the join's own reconcile

	specs := make([]api.BreakpointSpec, n)
	for i := range specs {
		specs[i] = api.BreakpointSpec{Line: i + 1}
	}

	// One setBreakpoints-equivalent round trip to the adapter, then n
	// breakpoint events logged in a tight loop under one lock
	// (session.replace) — unlike n separate AddBreakpoint calls, which
	// each round-trip the adapter and so give the observer's follower n
	// natural chances to catch up one event at a time regardless of this
	// step's change.
	placed, err := s.ReplaceBreakpoints(t.Context(), agentC, s.Program, specs, nil)
	if err != nil {
		t.Fatalf("ReplaceBreakpoints: %v", err)
	}

	if len(placed) != n {
		t.Fatalf("placed = %d, want %d", len(placed), n)
	}

	// flush (send + wait a "threads") only orders against whatever the
	// follower already wrote through the gate; for a batch this size it
	// can return before the follower finishes relaying it. Waiting for
	// the last placed id's own "new" event does not have that gap: the
	// follower relays log entries strictly in order, so seeing it means
	// every earlier one already arrived — reconciled into that "new" or
	// an earlier reconcile's, either way present exactly once.
	tc.waitBreakpointEvent("new", placed[len(placed)-1].ID)

	seen := make(map[int]bool, n)

	for _, ev := range named(tc.events[:tc.cursor], "breakpoint") {
		b, ok := ev.(*godap.BreakpointEvent)
		if !ok || b.Body.Reason != "new" {
			t.Fatalf("event = %+v, want a new breakpoint event", ev)
		}

		if seen[b.Body.Breakpoint.Id] {
			t.Errorf("breakpoint %d announced new twice", b.Body.Breakpoint.Id)
		}

		seen[b.Body.Breakpoint.Id] = true
	}

	if len(seen) != n {
		t.Errorf("distinct breakpoint ids announced new = %d, want %d (a miss)", len(seen), n)
	}

	// The tight bound (at most one reconcile per batch) is
	// [TestReconcileOnce]'s; this integration check only needs to catch a
	// return to reconciling every event, which would reconcile exactly n
	// times (n/2 leaves a wide, non-flaky margin around however many
	// batches the scheduler actually produced).
	if reconciles := observer.reconciles.Load() - base; reconciles >= n/2 {
		t.Errorf("reconciles for %d breakpoint events = %d, want well under %d", n, reconciles, n/2)
	}
}
