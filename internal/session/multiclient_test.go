// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

var (
	agentA = api.Client{ID: "agent:a", Kind: api.KindAgent, Name: "a"}
	agentB = api.Client{ID: "agent:b", Kind: api.KindAgent, Name: "b"}
)

func TestHandoffRefusesUntilGranted(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LeasePolicy: api.LeaseHandoff, LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	execs := len(eventsOf(s, api.EventExec))

	_, err := s.Resume(t.Context(), humanC, ExecNext, 0, testWait, api.DumpSpec{})
	expectCode(t, err, api.CodeLeaseHeld)

	if n := len(eventsOf(s, api.EventExec)); n != execs {
		t.Errorf("a refused request logged an exec event")
	}

	if l := s.Lease(); l.Holder != agentC.ID {
		t.Errorf("lease = %+v after a refused request, want still agent's", l)
	}

	if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
		t.Fatal(err)
	}

	expectStopped(t, resume(t, s, humanC, ExecNext), "step", 2)
}

func TestHumanPriorityLetsAHumanPreemptAnAgent(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LeasePolicy: api.LeaseHumanPriority, LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	expectStopped(t, resume(t, s, humanC, ExecNext), "step", 2)

	leases := eventsOf(s, api.EventLease)
	if last := leases[len(leases)-1]; last.Action != "auto" || last.Previous != agentC.ID || last.Lease.Holder != humanC.ID {
		t.Errorf("lease event = %+v, want human taking it from agent (auto)", last)
	}

	_, err := s.Resume(t.Context(), agentC, ExecNext, 0, testWait, api.DumpSpec{})
	expectCode(t, err, api.CodeLeaseHeld)
}

func TestHumanPriorityKeepsAgentsFromAgents(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentA, api.StartParams{LeasePolicy: api.LeaseHumanPriority, LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	_, err := s.Resume(t.Context(), agentB, ExecNext, 0, testWait, api.DumpSpec{})
	expectCode(t, err, api.CodeLeaseHeld)

	_, err = s.TakeLease(agentB, false)
	expectCode(t, err, api.CodeLeaseHeld)

	if l, err := s.TakeLease(agentB, true); err != nil || l.Holder != agentB.ID {
		t.Errorf("take --force = %+v, %v; want agent:b holding", l, err)
	}
}

func TestFreeTakesTheLeaseAutomatically(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	before := s.log.latest()

	expectStopped(t, resume(t, s, humanC, ExecNext), "step", 2)

	got := kindsAndActions(s.log.query(api.EventsParams{Since: before, Kinds: []api.EventKind{api.EventLease, api.EventExec}}).Events)
	if want := []string{"lease:auto", "exec:next"}; !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}

	// A state error takes nothing.
	_, err := s.Resume(t.Context(), agentC, ExecPause, 0, testWait, api.DumpSpec{})
	expectCode(t, err, api.CodeNotRunning)

	if l := s.Lease(); l.Holder != humanC.ID {
		t.Errorf("lease = %+v after a request refused by the state, want human's", l)
	}
}

func TestGrantAndPolicyAreTheHolders(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	_, err := s.GrantLease(humanC, humanC.ID, false)
	expectCode(t, err, api.CodeLeaseHeld)

	_, err = s.SetLeasePolicy(humanC, api.LeaseHandoff, false)
	expectCode(t, err, api.CodeLeaseHeld)

	_, err = s.GrantLease(agentC, "robot", false)
	if e, ok := errors.AsType[*api.Error](err); !ok || e.Code != api.CodeInvalidRequest || !strings.Contains(e.Hint, "lease grant human:NAME") {
		t.Errorf("grant to an invalid client: %v, want INVALID_REQUEST with a lease grant hint", err)
	}

	if l := s.ReleaseLease(humanC); l.Holder != agentC.ID {
		t.Errorf("release by a non-holder changed the lease: %+v", l)
	}

	if l := s.ReleaseLease(agentC); l.Holder != "" {
		t.Errorf("release by the holder: %+v, want nobody", l)
	}

	if l, err := s.SetLeasePolicy(humanC, api.LeaseHandoff, false); err != nil || l.Policy != api.LeaseHandoff {
		t.Errorf("policy while nobody holds: %+v, %v", l, err)
	}
}

func TestStopNeedsLeaseWhileLive(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	s := start(t, m, agentC, api.StartParams{LeasePolicy: api.LeaseHandoff, LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"lines=2"}}})

	_, err := m.Stop(t.Context(), humanC, s.ID)
	expectCode(t, err, api.CodeLeaseHeld)

	if snap := resume(t, s, agentC, ExecContinue); snap.Session.State != api.StateExited {
		t.Fatalf("after continue: %+v, want exited", snap.Session)
	}

	info, err := m.Stop(t.Context(), humanC, s.ID)
	if err != nil || info.State != api.StateExited {
		t.Fatalf("stop after exit = %+v, %v; want it forgotten", info, err)
	}

	if _, err := m.Get(s.ID); api.CodeOf(err) != api.CodeNoSession {
		t.Errorf("Get after stop: %v, want NO_SESSION", err)
	}
}

// TestStopSessionByIdentity: stopping a session again once it was forgotten
// never touches a newer session that reuses its id, whether the id was
// reused before the call or while it ran.
func TestStopSessionByIdentity(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	old := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	other := start(t, m, humanC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	if _, err := m.StopSession(t.Context(), agentC, old); err != nil {
		t.Fatal(err)
	}

	// A newer session gets the forgotten id (newID may reissue it).
	m.mu.Lock()
	delete(m.sessions, other.ID)
	m.sessions[old.ID] = other
	m.mu.Unlock()

	_, err := m.StopSession(t.Context(), agentC, old)
	expectCode(t, err, api.CodeNoSession)

	// The id reused after StopSession's check: stop must still leave it.
	if _, err := m.stop(t.Context(), agentC, old); err != nil {
		t.Fatal(err)
	}

	if got, err := m.Get(old.ID); err != nil || got != other {
		t.Errorf("Get(%s) = %p, %v; want the newer session %p", old.ID, got, err, other)
	}

	if st := other.Info().State; st == api.StateExited {
		t.Errorf("newer session state = %s, want it live", st)
	}
}

func TestStopEndsWithTheClient(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	if _, err := m.Stop(t.Context(), humanC, s.ID); err != nil {
		t.Fatal(err)
	}

	ended := eventsOf(s, api.EventEnded)
	if len(ended) != 1 || ended[0].Client != humanC.ID || ended[0].Reason != "stopped by "+humanC.ID {
		t.Errorf("ended events = %+v, want one, stopped by %s", ended, humanC.ID)
	}
}

func TestBreakpointOwnership(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	file := s.Program

	a3 := addBP(t, s, agentC, file, 3, "")
	h5 := addBP(t, s, humanC, file, 5, "")

	if a3.Owner != agentC.ID || h5.Owner != humanC.ID || a3.CreatedAt.IsZero() {
		t.Errorf("owners %q, %q (created %v); want agent and human:t", a3.Owner, h5.Owner, a3.CreatedAt)
	}

	_, _, err := s.RemoveBreakpoint(t.Context(), humanC, a3.ID, false)
	expectCode(t, err, api.CodeNotOwner)

	if removed, _, err := s.RemoveBreakpoint(t.Context(), humanC, a3.ID, true); err != nil || removed != 1 {
		t.Fatalf("rm --force = %d, %v; want 1", removed, err)
	}

	a7 := addBP(t, s, agentC, file, 7, "")

	if mine := s.Breakpoints(agentC.ID); len(mine) != 1 || mine[0].ID != a7.ID {
		t.Errorf("agent's breakpoints = %+v, want only %d", mine, a7.ID)
	}

	for range 2 { // the second time finds none of the human's: not an error
		removed, kept, err := s.RemoveBreakpoint(t.Context(), humanC, 0, false)
		if err != nil || kept != 1 || removed > 1 {
			t.Fatalf("human rm all = removed %d, kept %d, %v; want kept 1", removed, kept, err)
		}
	}

	expectAfterRemoveAll(t, s, h5.ID)
}

// expectAfterRemoveAll checks that only the agent's line 7 is left and the
// human's removal of its breakpoint id was logged last.
func expectAfterRemoveAll(t *testing.T, s *Session, id int) {
	t.Helper()

	if got := adapterBPs(t, s); got != "7" {
		t.Errorf("adapter breakpoints = %q, want 7", got)
	}

	removed := eventsOf(s, api.EventBreakpoint)
	if last := removed[len(removed)-1]; last.Action != "removed" || last.Client != humanC.ID || last.Breakpoint.ID != id {
		t.Errorf("last breakpoint event = %+v, want human removing %d", last, id)
	}
}

func TestSameLineBreakpointsShareASlot(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	file := s.Program

	// Two owners at one line: one adapter breakpoint, both verified.
	a5 := addBP(t, s, agentC, file, 5, "")
	h5 := addBP(t, s, humanC, file, 5, "")

	if !a5.Verified || !h5.Verified || a5.ID == h5.ID {
		t.Fatalf("breakpoints %+v and %+v, want two verified", a5, h5)
	}

	if got := adapterBPs(t, s); got != "5" {
		t.Errorf("adapter breakpoints = %q, want 5 once", got)
	}

	// Removing one keeps the stop.
	if _, _, err := s.RemoveBreakpoint(t.Context(), humanC, h5.ID, false); err != nil {
		t.Fatal(err)
	}

	if got := adapterBPs(t, s); got != "5" {
		t.Errorf("after removing one: %q, want 5", got)
	}

	// Unconditional wins.
	addBP(t, s, agentC, file, 5, "false")

	if got := adapterBPs(t, s); got != "5 if false" {
		t.Errorf("one conditional: %q", got)
	}

	addBP(t, s, humanC, file, 5, "")

	if got := adapterBPs(t, s); got != "5" {
		t.Errorf("conditional and unconditional: %q, want 5", got)
	}

	// Conflicting conditions: unconditional at the adapter (the stop filter
	// checks each), and no note.
	addBP(t, s, agentC, file, 7, "line == 7")
	addBP(t, s, humanC, file, 7, "line == 8")

	if got := adapterBPs(t, s); got != "5,7" {
		t.Errorf("conflicting conditions: %q, want 5,7", got)
	}

	for _, b := range s.Breakpoints("") {
		if b.Note != "" {
			t.Errorf("breakpoint %d at %d: note %q, want none", b.ID, b.RequestedLine, b.Note)
		}
	}

	expectStopped(t, resume(t, s, agentC, ExecContinue), "breakpoint", 5)
	expectStopped(t, resume(t, s, agentC, ExecContinue), "breakpoint", 7)
}

// TestConcurrentBreakpointSyncs adds breakpoints from many clients at once:
// every one must reach the adapter (serialized syncs keep the newest list).
func TestConcurrentBreakpointSyncs(t *testing.T) {
	t.Parallel()

	const n = 16

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"lines=40"}}})
	file := s.Program

	var wg sync.WaitGroup

	for i := range n {
		c := api.Client{ID: "agent:g" + strconv.Itoa(i), Kind: api.KindAgent, Name: "g" + strconv.Itoa(i)}

		wg.Go(func() {
			if _, err := s.AddBreakpoint(t.Context(), c, api.BreakpointSpec{File: file, Line: 2 + 2*i}); err != nil {
				t.Errorf("%s: %v", c.ID, err)
			}
		})
	}

	wg.Wait()

	want := make([]string, n)
	for i := range n {
		want[i] = strconv.Itoa(2 + 2*i)
	}

	if got := adapterBPs(t, s); got != strings.Join(want, ",") {
		t.Fatalf("adapter breakpoints = %q, want %q", got, strings.Join(want, ","))
	}

	for i := range n {
		expectStopped(t, resume(t, s, agentC, ExecContinue), "breakpoint", 2+2*i)
	}
}

// TestPauseWhileAnotherClientWaits: one client's continue waits on a program
// that never stops; another client pauses it.
func TestPauseWhileAnotherClientWaits(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"hang"}}})
	since := s.log.latest()

	type result struct {
		snap api.Snapshot
		err  error
	}

	done := make(chan result, 1)

	go func() {
		snap, err := s.Resume(t.Context(), agentC, ExecContinue, 0, testWait, api.DumpSpec{})
		done <- result{snap, err}
	}()

	// The exec event is logged under execMu before the continue is sent, so
	// the pause below queues behind the send and finds the program running.
	res, err := s.Events(t.Context(), api.EventsParams{Since: since, Kinds: []api.EventKind{api.EventExec}}, testWait)
	if err != nil || len(res.Events) != 1 || res.Events[0].Client != agentC.ID {
		t.Fatalf("waiting for the agent's exec: %+v, %v", res, err)
	}

	expectStopped(t, resume(t, s, humanC, ExecPause), "pause", 10)

	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}

	expectStopped(t, r.snap, "pause", 10)

	if l := s.Lease(); l.Holder != humanC.ID {
		t.Errorf("lease = %+v, want the pausing human's (free policy)", l)
	}
}

func TestEventsWait(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	since := s.log.latest()

	done := make(chan api.EventsResult, 1)

	go func() {
		res, err := s.Events(t.Context(), api.EventsParams{Since: since, Kinds: []api.EventKind{api.EventBreakpoint}}, testWait)
		if err != nil {
			t.Error(err)
		}

		done <- res
	}()

	s.Touch(humanC)
	addBP(t, s, humanC, s.Program, 4, "")

	res := <-done
	if len(res.Events) != 1 || res.Events[0].Action != "added" || res.Events[0].Client != humanC.ID || res.TimedOut {
		t.Errorf("wait = %+v, want the human's added breakpoint", res)
	}

	// -1 starts from now: the events so far don't count.
	res, err := s.Events(t.Context(), api.EventsParams{Since: -1}, 30*time.Millisecond)
	if err != nil || !res.TimedOut || len(res.Events) != 0 || res.Latest < 3 {
		t.Errorf("wait from now = %+v, %v; want a timeout", res, err)
	}

	_, err = s.Events(t.Context(), api.EventsParams{Kinds: []api.EventKind{"bogus"}}, 0)
	expectCode(t, err, api.CodeInvalidRequest)

	clients := eventsOf(s, api.EventClient)
	if len(clients) != 1 || clients[0].Client != humanC.ID {
		t.Errorf("client events = %+v, want only the human joining (the starter is not an event)", clients)
	}
}

func TestOutputIsALogView(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"lines=5"}}})

	if snap := s.Wait(t.Context(), 0, testWait, api.DumpSpec{}); snap.Session.State != api.StateExited {
		t.Fatalf("state = %s, want exited", snap.Session.State)
	}

	out := s.Output(0, 0)
	events := eventsOf(s, api.EventOutput)

	if len(out.Lines) != 5 || len(events) != 5 || out.More != 0 {
		t.Fatalf("output %+v, %d output events; want 5 and 5", out, len(events))
	}

	for i, l := range out.Lines {
		if l.Seq != events[i].Seq || l.Text != "line "+strconv.Itoa(i+1)+"\n" {
			t.Errorf("chunk %d = %+v, want seq %d", i, l, events[i].Seq)
		}
	}

	if tail := s.Output(0, 2); len(tail.Lines) != 2 || tail.Lines[1].Seq != events[4].Seq {
		t.Errorf("tail 2 = %+v", tail)
	}

	if since := s.Output(events[2].Seq, 0); len(since.Lines) != 2 {
		t.Errorf("since the 3rd = %+v, want 2 chunks", since)
	}
}

func TestOutputCaps(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	big := strings.Repeat("z", maxOutputChunk+100)

	s.mu.Lock()
	s.resumedAt = s.log.latest()
	for range 10 {
		s.appendOutputLocked("stdout", big)
	}
	s.mu.Unlock()

	last := eventsOf(s, api.EventOutput)
	if e := last[len(last)-1]; len(e.Text) != maxOutputChunk || !e.Truncated {
		t.Errorf("chunk of %d bytes (truncated %v), want %d and truncated", len(e.Text), e.Truncated, maxOutputChunk)
	}

	out := s.Output(0, 0)

	b, err := json.Marshal(out)
	if err != nil || len(b) > present512KiBPlus || out.More == 0 || len(out.Lines)+out.More != 10 {
		t.Errorf("output of 10 x 64 KiB: %d chunks, more %d, %d bytes of JSON; want it cut under 512 KiB", len(out.Lines), out.More, len(b))
	}

	snap := s.Snapshot(t.Context(), api.DumpSpec{})
	if n := len(snap.Output); n != 1 || len(snap.Output[0].Text) > snapshotOutputBytes || snap.OutputOmitted != 9 {
		t.Errorf("snapshot output: %d chunks (%d omitted), want 1 cut to %d bytes and 9 omitted", n, snap.OutputOmitted, snapshotOutputBytes)
	}
}

// present512KiBPlus is the output cap plus room for the result's framing.
const present512KiBPlus = 512<<10 + 1024

// holdsProgramData reports whether a recorded event holds what recordings
// leave out: output, text, a stop's text, description or attribution.
func holdsProgramData(e api.Event) bool {
	return e.Kind == api.EventOutput || e.Text != "" ||
		(e.Stop != nil && (e.Stop.Text != "" || e.Stop.Description != "" || e.Stop.Breakpoints != nil))
}

func TestRecordingIsMinimal(t *testing.T) {
	t.Parallel()

	store := newStubStore()
	s := start(t, newTestManager(t, store), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"lines=3"}}})

	// The human's differing condition makes the stop an attributed one.
	addBP(t, s, agentC, s.Program, 2, "")
	addBP(t, s, humanC, s.Program, 2, "false")

	snap := resume(t, s, agentC, ExecContinue)
	expectStopped(t, snap, "breakpoint", 2)

	if len(snap.Session.Stop.Breakpoints) != 1 {
		t.Fatalf("stop = %+v, want it attributed", snap.Session.Stop)
	}

	if snap := resume(t, s, agentC, ExecContinue); snap.Session.State != api.StateExited {
		t.Fatalf("state = %s, want exited", snap.Session.State)
	}

	if info := s.Info(); info.Recording != "mem:"+s.ID {
		t.Errorf("recording = %q", info.Recording)
	}

	var kinds []string

	for line := range strings.Lines(string(store.recording(s.ID).contents())) {
		var e api.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("recording line %q: %v", line, err)
		}

		if holdsProgramData(e) {
			t.Errorf("recorded program data: %s", line)
		}

		kinds = append(kinds, string(e.Kind))
	}

	if len(kinds) < 3 || kinds[0] != "started" || kinds[len(kinds)-1] != "ended" {
		t.Errorf("recorded kinds %v, want started first and ended last", kinds)
	}

	for _, k := range []api.EventKind{api.EventBreakpoint, api.EventExec, api.EventStopped, api.EventExited} {
		if !strings.Contains(strings.Join(kinds, ","), string(k)) {
			t.Errorf("recorded kinds %v lack %s", kinds, k)
		}
	}
}

func TestLostSessionsFromStore(t *testing.T) {
	t.Parallel()

	lostInfo := api.SessionInfo{
		ID: "s-lost", Lang: "fake", Program: "/x/prog", State: api.StateRunning,
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Recording: "mem:s-lost",
	}
	store := newStubStore(lostInfo)
	m := newTestManager(t, store)

	list := m.List()
	if len(list) != 1 || list[0].ID != "s-lost" || list[0].State != api.StateLost {
		t.Fatalf("List = %+v, want the lost session", list)
	}

	_, err := m.Get("s-lost")
	expectCode(t, err, api.CodeNoSession)

	if e, ok := errors.AsType[*api.Error](err); !ok || !strings.Contains(e.Hint, "eyedbg stop -s s-lost") || !strings.Contains(e.Hint, "mem:s-lost") {
		t.Errorf("hint = %v, want how to forget it and its recording", err)
	}

	if _, err := m.Get(""); api.CodeOf(err) != api.CodeNoSession {
		t.Errorf("Get(\"\") = %v; a lost session is never the only session", err)
	}

	info, err := m.Stop(t.Context(), agentC, "s-lost")
	if err != nil || info.State != api.StateLost {
		t.Fatalf("Stop = %+v, %v; want it forgotten", info, err)
	}

	if ops := store.opsFor("s-lost"); !reflect.DeepEqual(ops, []string{"remove s-lost"}) {
		t.Errorf("store ops = %v, want [remove s-lost]", ops)
	}

	if len(m.List()) != 0 {
		t.Errorf("List after forgetting = %+v", m.List())
	}
}

// TestStopDoesNotResurrectMetadata: a session that ended on its own is
// stopped; its metadata must end up removed whatever the order of the final
// save and the stop.
func TestStopDoesNotResurrectMetadata(t *testing.T) {
	t.Parallel()

	store := newStubStore()
	lives := make(chan int, 16)

	m := NewManager(t.Context(), Config{
		Drivers: []Driver{fakeDriver{}}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard, Store: store,
		OnLive: func(n int) { lives <- n },
	})
	defer m.StopAll(t.Context())

	s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"lines=2"}}})

	if snap := s.Wait(t.Context(), 0, testWait, api.DumpSpec{}); snap.Session.State != api.StateExited {
		t.Fatalf("state = %s, want exited", snap.Session.State)
	}

	if _, err := m.Stop(t.Context(), agentC, s.ID); err != nil {
		t.Fatal(err)
	}

	// add, watch (after its final save) and remove each report the count.
	for range 3 {
		<-lives
	}

	ops := store.opsFor(s.ID)
	if len(ops) == 0 || ops[len(ops)-1] != "remove "+s.ID || ops[0] != "save "+s.ID+" starting" {
		t.Errorf("store ops = %v, want a save at start and a remove last", ops)
	}
}

// TestDefaultSession: with no -s, an exited session (kept for its output)
// gives way to the one that hasn't exited, and is the default only alone.
func TestDefaultSession(t *testing.T) {
	t.Parallel()

	// ended starts a session and runs it to its exit.
	ended := func(t *testing.T, m *Manager) *Session {
		t.Helper()

		s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
		if snap := resume(t, s, agentC, ExecContinue); snap.Session.State != api.StateExited {
			t.Fatalf("after continue: %+v, want exited", snap.Session)
		}

		return s
	}
	live := func(t *testing.T, m *Manager) *Session {
		t.Helper()

		return start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	}

	tests := []struct {
		name  string
		sets  []func(*testing.T, *Manager) *Session
		want  int // index into sets of the default; -1 for NO_SESSION
		inErr int // how many ids the NO_SESSION error lists
	}{
		{name: "none", want: -1},
		{name: "one live", sets: []func(*testing.T, *Manager) *Session{live}, want: 0},
		{name: "one exited", sets: []func(*testing.T, *Manager) *Session{ended}, want: 0},
		{name: "exited and live", sets: []func(*testing.T, *Manager) *Session{ended, live}, want: 1},
		{name: "two live", sets: []func(*testing.T, *Manager) *Session{ended, live, live}, want: -1, inErr: 2},
		{name: "two exited", sets: []func(*testing.T, *Manager) *Session{ended, ended}, want: -1, inErr: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManager(t, nil)
			ss := make([]*Session, len(tt.sets))

			for i, set := range tt.sets {
				ss[i] = set(t, m)
			}

			got, err := m.Get("")
			if tt.want >= 0 {
				if err != nil || got != ss[tt.want] {
					t.Fatalf("Get(\"\") = %v, %v; want session %d", got, err, tt.want)
				}

				return
			}

			expectCode(t, err, api.CodeNoSession)

			if n := strings.Count(err.Error(), "s-"); n != tt.inErr {
				t.Errorf("error %q names %d sessions, want %d", err, n, tt.inErr)
			}
		})
	}
}
