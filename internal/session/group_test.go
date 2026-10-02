// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// bareMember is a session of group "app" with no adapter: the tests drive
// its state and its event log directly. Its log stamps events with at.
type bareMember struct {
	s  *Session
	at time.Time
}

var groupT0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func newBareMember(t *testing.T, id, service string) *bareMember {
	t.Helper()

	var l Launch
	if service != "" {
		l.Container = &api.ContainerInfo{Service: service}
	}

	mb := &bareMember{at: groupT0}
	mb.s = newSession(t.Context(), id, "fake", api.ModeAttach, l, slog.New(slog.DiscardHandler), agentC, api.LeaseFree)
	mb.s.group = "app"
	mb.s.log = newEventLog(maxEvents, maxEventBytes, func() time.Time { return mb.at })
	mb.setState(api.StateRunning)

	return mb
}

func (mb *bareMember) setState(st api.SessionState) {
	mb.s.mu.Lock()
	defer mb.s.mu.Unlock()

	mb.s.setStateLocked(st)
}

// emit appends an event of kind at offset ms from groupT0.
func (mb *bareMember) emit(ms int, kind api.EventKind) {
	mb.at = groupT0.Add(time.Duration(ms) * time.Millisecond)
	mb.s.log.append(api.Event{Kind: kind, Text: "x"})
}

func sessionsOf(mbs ...*bareMember) []*Session {
	out := make([]*Session, len(mbs))
	for i, mb := range mbs {
		out[i] = mb.s
	}

	return out
}

// groupIDs renders group events as "SESSION:SEQ".
func groupIDs(events []api.GroupEvent) []string {
	out := make([]string, len(events))
	for i := range events {
		out[i] = events[i].SessionID + ":" + strconv.Itoa(events[i].Event.Seq)
	}

	return out
}

func mustCursor(t *testing.T, s string) map[string]int {
	t.Helper()

	c, err := parseGroupCursor(s)
	if err != nil {
		t.Fatalf("cursor %q: %v", s, err)
	}

	return c
}

func TestGroupEventsMergeOrder(t *testing.T) {
	t.Parallel()

	a, b := newBareMember(t, "s-a", "web"), newBareMember(t, "s-b", "")
	a.emit(1, api.EventOutput)
	b.emit(2, api.EventOutput)
	a.emit(3, api.EventOutput)
	b.emit(3, api.EventOutput) // a tie: by session id

	// A member whose clock went back keeps its own order.
	c, d := newBareMember(t, "s-c", ""), newBareMember(t, "s-d", "")
	c.emit(5, api.EventOutput)
	c.emit(4, api.EventOutput)
	d.emit(4, api.EventOutput)

	tests := []struct {
		name    string
		members []*Session
		want    []string
	}{
		{"by time, then id", sessionsOf(b, a), []string{"s-a:1", "s-b:1", "s-a:2", "s-b:2"}},
		{"never reordered within a member", sessionsOf(c, d), []string{"s-d:1", "s-c:1", "s-c:2"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			res := groupEvents(t.Context(), tt.members, nil, api.GroupEventsParams{}, 0)
			if got := groupIDs(res.Events); !slices.Equal(got, tt.want) || res.More != 0 || res.TimedOut {
				t.Errorf("events = %v (more %d), want %v", got, res.More, tt.want)
			}
		})
	}

	res := groupEvents(t.Context(), sessionsOf(a, b), nil, api.GroupEventsParams{}, 0)
	if res.Events[0].Service != "web" || res.Events[1].Service != "" {
		t.Errorf("services = %q, %q; want web and none", res.Events[0].Service, res.Events[1].Service)
	}
}

func TestGroupEventsCursorRoundTrip(t *testing.T) {
	t.Parallel()

	a, b := newBareMember(t, "s-a", ""), newBareMember(t, "s-b", "")
	a.emit(1, api.EventOutput)
	a.emit(2, api.EventOutput)
	b.emit(3, api.EventOutput)

	members := sessionsOf(a, b)

	res := groupEvents(t.Context(), members, nil, api.GroupEventsParams{}, 0)
	if len(res.Events) != 3 || res.Cursor != "s-a:2,s-b:1" {
		t.Fatalf("first read = %v, cursor %q; want 3 events and s-a:2,s-b:1", groupIDs(res.Events), res.Cursor)
	}

	res = groupEvents(t.Context(), members, mustCursor(t, res.Cursor), api.GroupEventsParams{}, 0)
	if len(res.Events) != 0 || res.Cursor != "s-a:2,s-b:1" {
		t.Fatalf("read at the cursor = %v, cursor %q; want nothing, same cursor", groupIDs(res.Events), res.Cursor)
	}

	b.emit(4, api.EventOutput)

	res = groupEvents(t.Context(), members, mustCursor(t, res.Cursor), api.GroupEventsParams{}, 0)
	if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-b:2"}) || res.Cursor != "s-a:2,s-b:2" {
		t.Errorf("after an append = %v, cursor %q; want s-b:2 only", got, res.Cursor)
	}

	// An id that is no member is ignored, a member missing starts at its
	// first event, and a cursor never moves back.
	res = groupEvents(t.Context(), members, mustCursor(t, "s-zz:7,s-a:50"), api.GroupEventsParams{}, 0)
	if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-b:1", "s-b:2"}) || res.Cursor != "s-a:50,s-b:2" {
		t.Errorf("odd cursor = %v, cursor %q; want b's events, s-a:50,s-b:2", got, res.Cursor)
	}
}

// groupLog makes two members' logs: a has 5 events, b 4, interleaved in
// time, of kinds output and stopped.
func groupLog(t *testing.T) []*Session {
	t.Helper()

	a, b := newBareMember(t, "s-a", ""), newBareMember(t, "s-b", "")
	a.emit(1, api.EventOutput)
	a.emit(2, api.EventStopped)
	b.emit(3, api.EventOutput)
	a.emit(4, api.EventOutput)
	b.emit(5, api.EventStopped)
	b.emit(6, api.EventStopped)
	a.emit(7, api.EventStopped)
	b.emit(8, api.EventOutput)
	a.emit(9, api.EventOutput)

	return sessionsOf(a, b)
}

func TestGroupEventsPagingNeverSkips(t *testing.T) {
	t.Parallel()

	stopped := []api.EventKind{api.EventStopped}

	tests := []struct {
		name string
		p    api.GroupEventsParams
	}{
		{"limit 1", api.GroupEventsParams{Limit: 1}},
		{"limit 2", api.GroupEventsParams{Limit: 2}},
		{"limit 3", api.GroupEventsParams{Limit: 3}},
		{"limit 2 of a kind", api.GroupEventsParams{Limit: 2, Kinds: stopped}},
		{"budget of one event", api.GroupEventsParams{Budget: 1}},
		{"budget of one event, of a kind", api.GroupEventsParams{Budget: 1, Kinds: stopped}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			members := groupLog(t)
			all := groupEvents(t.Context(), members, nil, api.GroupEventsParams{Kinds: tt.p.Kinds}, 0)

			var (
				got    []string
				cursor map[string]int
			)

			for range 20 {
				res := groupEvents(t.Context(), members, cursor, tt.p, 0)
				if len(res.Events) == 0 {
					if res.More != 0 {
						t.Fatalf("an empty page with more %d", res.More)
					}

					break
				}

				got = append(got, groupIDs(res.Events)...)
				cursor = mustCursor(t, res.Cursor)
			}

			if want := groupIDs(all.Events); !slices.Equal(got, want) {
				t.Errorf("paged = %v, want %v (each once, in order)", got, want)
			}
		})
	}
}

func TestGroupEventsMore(t *testing.T) {
	t.Parallel()

	res := groupEvents(t.Context(), groupLog(t), nil, api.GroupEventsParams{Limit: 2}, 0)
	if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-a:1", "s-a:2"}) || res.More != 7 || res.Cursor != "s-a:2,s-b:0" {
		t.Errorf("limit 2 = %v, more %d, cursor %q; want s-a:1 s-a:2, more 7, s-a:2,s-b:0", got, res.More, res.Cursor)
	}
}

func TestGroupEventsNewest(t *testing.T) {
	t.Parallel()

	members := groupLog(t)

	// The newest two are b's last and a's last: the older ones were cut, so
	// neither member's cursor moves.
	res := groupEvents(t.Context(), members, nil, api.GroupEventsParams{Limit: 2, Newest: true}, 0)
	if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-b:4", "s-a:5"}) || res.More != 7 || res.Cursor != "s-a:0,s-b:0" {
		t.Fatalf("newest 2 = %v, more %d, cursor %q; want s-b:4 s-a:5, more 7, s-a:0,s-b:0", got, res.More, res.Cursor)
	}

	// From s-a:4, a's only event left is its newest; b's are all cut but
	// its newest, so only a moves.
	res = groupEvents(t.Context(), members, mustCursor(t, "s-a:4"), api.GroupEventsParams{Limit: 2, Newest: true}, 0)
	if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-b:4", "s-a:5"}) || res.Cursor != "s-a:5,s-b:0" {
		t.Errorf("newest 2 from s-a:4 = %v, cursor %q; want s-b:4 s-a:5, s-a:5,s-b:0", got, res.Cursor)
	}

	// Everything fits: every member moves to its newest.
	res = groupEvents(t.Context(), members, nil, api.GroupEventsParams{Newest: true}, 0)
	if len(res.Events) != 9 || res.Cursor != "s-a:5,s-b:4" {
		t.Errorf("newest, all = %d events, cursor %q; want 9, s-a:5,s-b:4", len(res.Events), res.Cursor)
	}
}

func TestGroupEventsKinds(t *testing.T) {
	t.Parallel()

	res := groupEvents(t.Context(), groupLog(t), nil, api.GroupEventsParams{Kinds: []api.EventKind{api.EventStopped}}, 0)
	if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-a:2", "s-b:2", "s-b:3", "s-a:4"}) || res.Cursor != "s-a:5,s-b:4" {
		t.Errorf("stopped = %v, cursor %q; want the 4 stops and each member past its newest", got, res.Cursor)
	}
}

func TestGroupEventsDropped(t *testing.T) {
	t.Parallel()

	a := newBareMember(t, "s-a", "")
	a.s.log.maxCount = 3

	for i := range 5 {
		a.emit(i, api.EventOutput)
	}

	res := groupEvents(t.Context(), sessionsOf(a), mustCursor(t, "s-a:1"), api.GroupEventsParams{}, 0)
	if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-a:3", "s-a:4", "s-a:5"}) || res.Dropped != 1 || res.Cursor != "s-a:5" {
		t.Errorf("events = %v, dropped %d, cursor %q; want 3..5, 1 dropped, s-a:5", got, res.Dropped, res.Cursor)
	}
}

// TestGroupEventsWait runs in a bubble: its clock is fake, and the bubble
// fails the test if a goroutine is left behind.
func TestGroupEventsWait(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		a, b := newBareMember(t, "s-a", ""), newBareMember(t, "s-b", "")
		a.emit(1, api.EventOutput)

		members := sessionsOf(a, b)
		p := api.GroupEventsParams{Kinds: []api.EventKind{api.EventStopped}}
		done := make(chan api.GroupEventsResult, 1)

		go func() { done <- groupEvents(t.Context(), members, nil, p, time.Minute) }()

		synctest.Wait()
		a.emit(2, api.EventOutput) // not of the kind: wakes it, it waits on
		synctest.Wait()

		select {
		case res := <-done:
			t.Fatalf("returned on a non-matching event: %+v", res)
		default:
		}

		b.emit(3, api.EventStopped)

		res := <-done
		if got := groupIDs(res.Events); !slices.Equal(got, []string{"s-b:1"}) || res.TimedOut || res.Cursor != "s-a:2,s-b:1" {
			t.Errorf("wait = %v, cursor %q, timed out %v; want b's stop", got, res.Cursor, res.TimedOut)
		}

		// Nothing comes: it times out, past what it saw.
		start := time.Now()
		a.emit(4, api.EventOutput)

		res = groupEvents(t.Context(), members, mustCursor(t, res.Cursor), p, time.Minute)
		if !res.TimedOut || len(res.Events) != 0 || res.Cursor != "s-a:3,s-b:1" || time.Since(start) != time.Minute {
			t.Errorf("wait = %+v after %v; want a timeout after 1m with s-a:3,s-b:1", res, time.Since(start))
		}
	})
}

func TestGroupEventsWaitCountsDropped(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		a := newBareMember(t, "s-a", "")
		a.s.log.maxCount = 2

		for i := range 4 {
			a.emit(i, api.EventOutput)
		}

		done := make(chan api.GroupEventsResult, 1)

		go func() {
			done <- groupEvents(t.Context(), sessionsOf(a), nil, api.GroupEventsParams{Kinds: []api.EventKind{api.EventStopped}}, time.Minute)
		}()

		synctest.Wait()
		a.emit(5, api.EventStopped)

		if res := <-done; len(res.Events) != 1 || res.Dropped != 2 {
			t.Errorf("wait = %v, dropped %d; want the stop and the 2 dropped before the wait", groupIDs(res.Events), res.Dropped)
		}
	})
}

func TestParseGroupCursor(t *testing.T) {
	t.Parallel()

	many := make([]string, api.MaxGroupCursor+1)
	for i := range many {
		many[i] = fmt.Sprintf("s-%d:1", i)
	}

	tests := []struct {
		in   string
		want map[string]int
		ok   bool
	}{
		{"", map[string]int{}, true},
		{"s-ab12:0", map[string]int{"s-ab12": 0}, true},
		{"s-ab12:40,s-cd34:7", map[string]int{"s-ab12": 40, "s-cd34": 7}, true},
		{strings.Join(many[:api.MaxGroupCursor], ","), nil, true},
		{strings.Join(many, ","), nil, false},
		{"s-ab12", nil, false},
		{"s-ab12:", nil, false},
		{":3", nil, false},
		{"s-:3", nil, false},
		{"s-ab12:-1", nil, false},
		{"s-ab12:+1", nil, false},
		{"s-ab12:1.5", nil, false},
		{"s-ab12:99999999999999999999999", nil, false},
		{"s-ab12:1,s-ab12:2", nil, false},
		{"S-AB12:1", nil, false},
		{"s-ab12:1,", nil, false},
		{",s-ab12:1", nil, false},
		{" s-ab12:1", nil, false},
		{"s-ab12:1:2", nil, false},
		{"x-ab12:1", nil, false},
		{"s-" + strings.Repeat("a", 31) + ":1", nil, false},
	}

	for _, tt := range tests {
		got, err := parseGroupCursor(tt.in)
		if tt.ok != (err == nil) {
			t.Errorf("parse %q: err %v, want ok %v", tt.in, err, tt.ok)

			continue
		}

		if !tt.ok {
			expectCode(t, err, api.CodeInvalidRequest)
		}

		if tt.want != nil && fmt.Sprint(got) != fmt.Sprint(tt.want) {
			t.Errorf("parse %q = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// newGroupManager is a manager holding sessions directly (no adapters):
// what the group methods do around the members.
func newGroupManager(t *testing.T, members ...*Session) *Manager {
	t.Helper()

	m := NewManager(t.Context(), Config{Logger: slog.New(slog.DiscardHandler)})
	for _, s := range members {
		m.sessions[s.ID] = s
	}

	return m
}

func TestGroupMembers(t *testing.T) {
	t.Parallel()

	a, b, other := newBareMember(t, "s-a", ""), newBareMember(t, "s-b", ""), newBareMember(t, "s-c", "")
	other.s.group = "else"
	b.s.CreatedAt = a.s.CreatedAt.Add(-time.Second)

	m := newGroupManager(t, a.s, b.s, other.s)

	got, err := m.groupMembers(humanC, "app")
	if err != nil || len(got) != 2 || got[0] != b.s || got[1] != a.s {
		t.Fatalf("members = %v, %v; want s-b then s-a (oldest first)", got, err)
	}

	for _, s := range got {
		if !slices.ContainsFunc(s.Info().Clients, func(ci api.ClientInfo) bool { return ci.ID == humanC.ID }) {
			t.Errorf("%s: the caller was not recorded as one of its clients", s.ID)
		}
	}

	for _, tt := range []struct {
		group string
		code  api.Code
	}{
		{"", api.CodeInvalidRequest},
		{"App", api.CodeInvalidRequest},
		{"none", api.CodeNoSession},
	} {
		_, err := m.GroupWait(t.Context(), agentC, api.GroupWaitParams{Group: tt.group}, time.Millisecond)
		expectCode(t, err, tt.code)

		_, err = m.GroupEvents(t.Context(), agentC, api.GroupEventsParams{Group: tt.group}, 0)
		expectCode(t, err, tt.code)
	}

	_, err = m.GroupEvents(t.Context(), agentC, api.GroupEventsParams{Group: "app", Since: "s-a:x"}, 0)
	expectCode(t, err, api.CodeInvalidRequest)

	_, err = m.GroupEvents(t.Context(), agentC, api.GroupEventsParams{Group: "app", Kinds: []api.EventKind{"bogus"}}, 0)
	expectCode(t, err, api.CodeInvalidRequest)
}

func TestGroupEventsFollowsAtMostACursorOfMembers(t *testing.T) {
	t.Parallel()

	var members []*Session
	for i := range api.MaxGroupCursor + 1 {
		members = append(members, newBareMember(t, fmt.Sprintf("s-%d", i), "").s)
	}

	m := newGroupManager(t, members...)

	_, err := m.GroupEvents(t.Context(), agentC, api.GroupEventsParams{Group: "app"}, 0)
	expectCode(t, err, api.CodeInvalidRequest)

	delete(m.sessions, members[0].ID)

	res, err := m.GroupEvents(t.Context(), agentC, api.GroupEventsParams{Group: "app"}, 0)
	if err != nil || len(strings.Split(res.Cursor, ",")) != api.MaxGroupCursor {
		t.Errorf("64 members: %v, cursor %q; want a cursor of 64 entries", err, res.Cursor)
	}

	if _, err := parseGroupCursor(res.Cursor); err != nil {
		t.Errorf("the cursor of 64 members doesn't parse: %v", err)
	}
}

// startMember starts a fake session in group "app": stopped at entry, or
// running (it hangs after its last line).
func startMember(t *testing.T, m *Manager, stopped bool) *Session {
	t.Helper()

	p := api.StartParams{Group: "app", LaunchSpec: api.LaunchSpec{StopOnEntry: stopped}}
	if !stopped {
		p.Args = []string{"hang"}
	}

	return start(t, m, agentC, p)
}

func setStoppedAt(s *Session, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.stoppedAt = at
}

// answeredBy fails the test unless res names session id (and didn't time
// out), and returns its snapshot.
func answeredBy(t *testing.T, res api.GroupWaitResult, id string) *api.Snapshot {
	t.Helper()

	if res.TimedOut || res.Session == nil || res.Session.Session.ID != id {
		t.Fatalf("wait = %+v, want %s to answer", res, id)
	}

	return res.Session
}

// waitAround runs a group wait on members, begun before fn runs, and
// returns its result.
func waitAround(ctx context.Context, members []*Session, newOnly bool, fn func()) api.GroupWaitResult {
	marks := statesOf(members)
	done := make(chan api.GroupWaitResult, 1)

	go func() { done <- groupWaitFrom(ctx, members, marks, newOnly, testWait, api.DumpSpec{}) }()

	fn()

	return <-done
}

func TestGroupWaitAlreadyStopped(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	a, _ := startMember(t, m, true), startMember(t, m, false)

	// A wait of 1ns would time out at once: only the already-stopped
	// member can answer it.
	res, err := m.GroupWait(t.Context(), agentC, api.GroupWaitParams{Group: "app"}, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}

	snap := answeredBy(t, res, a.ID)
	if snap.Frame == nil || snap.Frame.Line != 1 || len(res.Stopped) != 0 || res.Ended || res.Group != "app" {
		t.Errorf("wait = %+v (frame %+v); want its entry stop and nobody else stopped", res, snap.Frame)
	}
}

func TestGroupWaitNewIgnoresEarlierStops(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	a, _ := startMember(t, m, true), startMember(t, m, false)

	res, err := m.GroupWait(t.Context(), agentC, api.GroupWaitParams{Group: "app", New: true}, time.Nanosecond)
	if err != nil || !res.TimedOut || res.Session != nil || len(res.Stopped) != 1 {
		t.Fatalf("new wait = %+v, %v; want a timeout listing one stopped member", res, err)
	}

	if st := res.Stopped[0]; st.SessionID != a.ID || st.Reason != "entry" || st.StoppedAt.IsZero() {
		t.Errorf("stopped = %+v, want %s at entry", st, a.ID)
	}
}

func TestGroupWaitStoppedLongestWins(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	a, b := startMember(t, m, true), startMember(t, m, true)
	members := []*Session{a, b}

	low, high := a, b
	if b.ID < a.ID {
		low, high = b, a
	}

	tests := []struct {
		name         string
		atA, atB     time.Time
		winner, also *Session
	}{
		{"a first", groupT0, groupT0.Add(time.Second), a, b},
		{"b first", groupT0.Add(time.Second), groupT0, b, a},
		{"a tie: the lower id", groupT0, groupT0, low, high},
	}

	for _, tt := range tests {
		setStoppedAt(a, tt.atA)
		setStoppedAt(b, tt.atB)

		res := groupWait(t.Context(), members, false, time.Nanosecond, api.DumpSpec{})
		answeredBy(t, res, tt.winner.ID)

		if len(res.Stopped) != 1 || res.Stopped[0].SessionID != tt.also.ID {
			t.Errorf("%s: stopped = %+v; want %s also stopped", tt.name, res.Stopped, tt.also.ID)
		}
	}
}

func TestGroupWaitNewWaitsForTheNextStop(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	a, b := startMember(t, m, true), startMember(t, m, false)

	res := waitAround(t.Context(), []*Session{a, b}, true, func() { resume(t, a, agentC, ExecNext) })
	if snap := answeredBy(t, res, a.ID); snap.Frame == nil || snap.Frame.Line != 2 {
		t.Errorf("wait = %+v; want %s's next stop, at line 2", res, a.ID)
	}
}

func TestGroupWaitTimesOut(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	startMember(t, m, false)
	startMember(t, m, false)

	res, err := m.GroupWait(t.Context(), agentC, api.GroupWaitParams{Group: "app"}, time.Nanosecond)
	if err != nil || !res.TimedOut || res.Session != nil || len(res.Stopped) != 0 || res.Ended {
		t.Errorf("wait = %+v, %v; want a plain timeout", res, err)
	}
}

func TestGroupWaitAMemberEnding(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)

	// ended ran to its end before the calls: it is never waited for.
	ended := start(t, m, agentC, api.StartParams{Group: "app", LaunchSpec: api.LaunchSpec{Args: []string{"lines=2"}}})
	if snap := ended.Wait(t.Context(), 0, testWait, api.DumpSpec{}); snap.Session.State != api.StateExited {
		t.Fatalf("program = %+v, want exited", snap.Session)
	}

	a, b := startMember(t, m, false), startMember(t, m, false)
	stop := func(s *Session) func() {
		return func() {
			if _, err := m.Stop(t.Context(), agentC, s.ID); err != nil {
				t.Error(err)
			}
		}
	}

	// b is stopped, and so removed from the manager, mid-wait.
	res := waitAround(t.Context(), m.members("app"), false, stop(b))
	if snap := answeredBy(t, res, b.ID); snap.Session.State != api.StateExited || res.Ended {
		t.Fatalf("wait = %+v; want %s ended, the group not", res, b.ID)
	}

	// The last live member ending ends the group.
	res = waitAround(t.Context(), m.members("app"), false, stop(a))
	answeredBy(t, res, a.ID)

	if !res.Ended {
		t.Fatalf("wait = %+v; want %s ended, and the group with it", res, a.ID)
	}

	// Every member has ended: answered at once.
	res, err := m.GroupWait(t.Context(), agentC, api.GroupWaitParams{Group: "app", New: true}, testWait)
	if err != nil || !res.Ended || res.TimedOut || res.Session != nil {
		t.Errorf("wait = %+v, %v; want ended at once", res, err)
	}
}

func TestGroupWaitCanceled(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	startMember(t, m, false)

	ctx, cancel := context.WithCancel(t.Context())

	if res := waitAround(ctx, m.members("app"), false, cancel); !res.TimedOut || res.Session != nil {
		t.Errorf("wait = %+v; want it to end with the request", res)
	}
}

// TestGroupWaitManyMembers waits on many members at once, from several
// callers, in a bubble: the bubble fails the test if any goroutine is left
// behind, and its clock is fake.
func TestGroupWaitManyMembers(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const n = 48

		all := make([]*bareMember, n)
		for i := range all {
			all[i] = newBareMember(t, fmt.Sprintf("s-%02d", i), "")
		}

		members := sessionsOf(all...)

		// Five callers wait: 0-2 for an hour, 3 for a second, 4 until
		// canceled.
		canceled, cancel := context.WithCancel(t.Context())
		ctxs := []context.Context{t.Context(), t.Context(), t.Context(), t.Context(), canceled}
		waits := []time.Duration{time.Hour, time.Hour, time.Hour, time.Second, time.Hour}
		done := make([]chan api.GroupWaitResult, len(ctxs))

		for i := range done {
			done[i] = make(chan api.GroupWaitResult, 1)

			go func() { done[i] <- groupWait(ctxs[i], members, false, waits[i], api.DumpSpec{}) }()
		}

		synctest.Wait()
		cancel()

		results := make([]api.GroupWaitResult, len(done))
		results[4] = <-done[4]
		results[3] = <-done[3] // the bubble's clock moves on to its deadline

		all[17].setState(api.StateExited)

		for i := range 3 {
			results[i] = <-done[i]
		}

		for i, res := range results {
			timedOut := i >= 3
			if res.TimedOut != timedOut || (!timedOut && (res.Session == nil || res.Session.Session.ID != "s-17" || res.Ended)) {
				t.Errorf("caller %d = %+v, want timed out %v, else s-17 ended", i, res, timedOut)
			}
		}
	})
}

func TestAnyClosed(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		chans := make([]chan struct{}, 8)
		watch := make([]<-chan struct{}, len(chans))

		for i := range chans {
			chans[i] = make(chan struct{})
			watch[i] = chans[i]
		}

		done := make(chan bool, 1)

		go func() { done <- anyClosed(t.Context(), watch) }()

		synctest.Wait()
		close(chans[5])
		close(chans[2])

		if !<-done {
			t.Error("anyClosed = false with channels closed")
		}

		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()

		if anyClosed(ctx, watch[6:]) {
			t.Error("anyClosed = true with none closed")
		}
	})
}

// TestSeveralSessionsHintNamesTheGroup: when every live session is a member
// of one group, the "several sessions" error points at 'compose wait'.
func TestSeveralSessionsHintNamesTheGroup(t *testing.T) {
	t.Parallel()

	a, b, other := newBareMember(t, "s-a", "web"), newBareMember(t, "s-b", "db"), newBareMember(t, "s-c", "")
	other.s.group = "else"

	ended := newBareMember(t, "s-d", "")
	ended.s.group = "else"
	ended.setState(api.StateExited)

	tests := []struct {
		name         string
		members      []*Session
		wantGroup    bool
		wantContains string
	}{
		{"one group", sessionsOf(a, b), true, "s-a, s-b"},
		{"two groups", sessionsOf(a, b, other), false, "s-a, s-b, s-c"},
		{"an exited one of another group doesn't count", sessionsOf(a, b, ended), true, "s-a, s-b"},
		{"no group", func() []*Session {
			x, y := newBareMember(t, "s-x", ""), newBareMember(t, "s-y", "")
			x.s.group, y.s.group = "", ""

			return sessionsOf(x, y)
		}(), false, "s-x, s-y"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := newGroupManager(t, tt.members...).Get("")

			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Code != api.CodeNoSession || !strings.Contains(apiErr.Message, tt.wantContains) {
				t.Fatalf("Get(\"\") = %v, want NO_SESSION naming %q", err, tt.wantContains)
			}

			if got := strings.Contains(apiErr.Hint, "eyedbg compose wait -g app"); got != tt.wantGroup {
				t.Errorf("hint = %q; names the group = %v, want %v", apiErr.Hint, got, tt.wantGroup)
			}
		})
	}
}
