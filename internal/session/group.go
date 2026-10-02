// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/present"
)

// A group's members are its sessions in the manager's map: live, or exited
// and not yet stopped or detached (lost sessions are never members). Group
// operations copy the members under the manager's mu, release it, then lock
// one member at a time; they never hold two members' locks at once, and
// never a member's while waiting.

// members returns the sessions of group g, oldest first (ties: by id). A
// session's group is set before it is shared and never changes, so it is
// read without the session's lock.
func (m *Manager) members(g string) []*Session {
	m.mu.Lock()

	var out []*Session

	for _, s := range m.sessions {
		if s.group == g {
			out = append(out, s)
		}
	}
	m.mu.Unlock()

	slices.SortFunc(out, func(a, b *Session) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}

		return strings.Compare(a.ID, b.ID)
	})

	return out
}

// groupMembers checks g and returns its members, each touched for c.
func (m *Manager) groupMembers(c api.Client, g string) ([]*Session, error) {
	if g == "" {
		return nil, api.NewError(api.CodeInvalidRequest, "a group is required", "see 'eyedbg sessions' for each session's group")
	}

	if err := api.CheckGroup(g); err != nil {
		return nil, err
	}

	members := m.members(g)
	if len(members) == 0 {
		return nil, api.NewError(api.CodeNoSession, "no group "+g, "see 'eyedbg sessions'")
	}

	for _, s := range members {
		s.Touch(c)
	}

	return members, nil
}

// service is s's compose service ("" if it has none). The container info is
// set before s is shared and never changes.
func (s *Session) service() string {
	if s.container == nil {
		return ""
	}

	return s.container.Service
}

// memberState is a member's state as one pass over the group saw it, with
// the channel its next state change closes (read under the same lock, so a
// change after the pass is never missed).
type memberState struct {
	s         *Session
	state     api.SessionState
	stops     int
	reason    string // the stop's, while stopped
	threadID  int
	stoppedAt time.Time
	changed   <-chan struct{}
}

func (s *Session) memberState() memberState {
	s.mu.Lock()
	defer s.mu.Unlock()

	return memberState{
		s: s, state: s.state, stops: s.stops, reason: s.stop.Reason, threadID: s.stop.ThreadID, stoppedAt: s.stoppedAt, changed: s.changed,
	}
}

// statesOf reads each member's state, one lock at a time.
func statesOf(members []*Session) []memberState {
	states := make([]memberState, len(members))
	for i, s := range members {
		states[i] = s.memberState()
	}

	return states
}

// stoppedFirst orders members by when they stopped, then by id.
func stoppedFirst(a, b memberState) int {
	if c := a.stoppedAt.Compare(b.stoppedAt); c != 0 {
		return c
	}

	return strings.Compare(a.s.ID, b.s.ID)
}

// GroupWait waits up to wait until a member of group p.Group stops or ends
// (wait-any), for client c. Without p.New a member already stopped answers
// at once: the one stopped longest. Members that ended before the call are
// not waited for; when all have, it answers Ended at once.
func (m *Manager) GroupWait(ctx context.Context, c api.Client, p api.GroupWaitParams, wait time.Duration) (api.GroupWaitResult, error) {
	members, err := m.groupMembers(c, p.Group)
	if err != nil {
		return api.GroupWaitResult{}, err
	}

	res := groupWait(ctx, members, p.New, wait, p.DumpSpec)
	res.Group = p.Group

	return res, nil
}

// groupWait is GroupWait on members.
func groupWait(ctx context.Context, members []*Session, newOnly bool, wait time.Duration, dump api.DumpSpec) api.GroupWaitResult {
	return groupWaitFrom(ctx, members, statesOf(members), newOnly, wait, dump)
}

// groupWaitFrom is groupWait begun when members were in states marks. It
// starts no goroutine that outlives it (see anyClosed) and holds no lock
// while it waits.
func groupWaitFrom(ctx context.Context, members []*Session, marks []memberState, newOnly bool, wait time.Duration, dump api.DumpSpec) api.GroupWaitResult {
	if !slices.ContainsFunc(marks, func(ms memberState) bool { return ms.state != api.StateExited }) {
		return api.GroupWaitResult{Stopped: []api.GroupStop{}, Ended: true}
	}

	if !newOnly {
		var stopped []memberState

		for _, ms := range marks {
			if ms.state == api.StateStopped {
				stopped = append(stopped, ms)
			}
		}

		if len(stopped) > 0 {
			return groupReport(ctx, members, slices.MinFunc(stopped, stoppedFirst).s, false, dump)
		}
	}

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	for {
		states := statesOf(members)
		if s := answered(marks, states); s != nil {
			return groupReport(ctx, members, s, true, dump)
		}

		watched := make([]<-chan struct{}, 0, len(states))

		for i, ms := range states {
			if marks[i].state != api.StateExited {
				watched = append(watched, ms.changed)
			}
		}

		if !anyClosed(waitCtx, watched) {
			break
		}
	}

	// A change racing the deadline still counts.
	if s := answered(marks, statesOf(members)); s != nil {
		return groupReport(ctx, members, s, true, dump)
	}

	return api.GroupWaitResult{Stopped: stoppedMembers(members, nil), TimedOut: true}
}

// answered is the member that answers a wait begun at marks, given states
// now: of those that stopped since, the one that stopped first (ties: by
// id); else the first that ended since; else nil.
func answered(marks, states []memberState) *Session {
	var (
		stopped []memberState
		ended   *Session
	)

	for i, ms := range states {
		switch {
		case marks[i].state == api.StateExited:
		case ms.stops > marks[i].stops:
			stopped = append(stopped, ms)
		case ms.state == api.StateExited && ended == nil:
			ended = ms.s
		}
	}

	if len(stopped) > 0 {
		return slices.MinFunc(stopped, stoppedFirst).s
	}

	return ended
}

// groupReport is the result naming member s: its snapshot (after its output
// settled when it answered just now), the other stopped members, and
// whether every member has ended.
func groupReport(ctx context.Context, members []*Session, s *Session, settle bool, dump api.DumpSpec) api.GroupWaitResult {
	if settle {
		s.settleOutput(ctx)
	}

	snap := s.Snapshot(ctx, dump)
	res := api.GroupWaitResult{Session: &snap, Service: s.service(), Stopped: stoppedMembers(members, s)}

	res.Ended = !slices.ContainsFunc(statesOf(members), func(ms memberState) bool { return ms.state != api.StateExited })

	return res
}

// stoppedMembers lists the members stopped now, except skip, the one
// stopped longest first.
func stoppedMembers(members []*Session, skip *Session) []api.GroupStop {
	var stopped []memberState

	for _, ms := range statesOf(members) {
		if ms.state == api.StateStopped && ms.s != skip {
			stopped = append(stopped, ms)
		}
	}

	slices.SortFunc(stopped, stoppedFirst)

	out := make([]api.GroupStop, 0, len(stopped))
	for _, ms := range stopped {
		out = append(out, api.GroupStop{
			SessionID: ms.s.ID, Service: ms.s.service(), Reason: ms.reason, ThreadID: ms.threadID, StoppedAt: ms.stoppedAt,
		})
	}

	return out
}

// anyClosed waits until one of chans is closed (true) or ctx ends (false).
// It waits for every goroutine it starts before returning.
func anyClosed(ctx context.Context, chans []<-chan struct{}) bool {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Buffered for all: a goroutine never blocks sending.
	closed := make(chan struct{}, len(chans))

	var wg sync.WaitGroup

	for _, ch := range chans {
		wg.Go(func() {
			select {
			case <-ch:
				closed <- struct{}{}
			case <-ctx.Done():
			}
		})
	}

	ok := false

	select {
	case <-closed:
		ok = true
	case <-ctx.Done():
	}

	cancel()
	wg.Wait()

	return ok
}

// GroupEvents returns the events of group p.Group's members that p asks
// for, merged, for client c, waiting up to wait (0: not at all) for one to
// exist on any member. The result fits p.Budget tokens and
// present.MaxResultBytes.
func (m *Manager) GroupEvents(ctx context.Context, c api.Client, p api.GroupEventsParams, wait time.Duration) (api.GroupEventsResult, error) {
	if err := checkEventKinds(p.Kinds); err != nil {
		return api.GroupEventsResult{}, err
	}

	cursor, err := parseGroupCursor(p.Since)
	if err != nil {
		return api.GroupEventsResult{}, err
	}

	members, err := m.groupMembers(c, p.Group)
	if err != nil {
		return api.GroupEventsResult{}, err
	}

	if len(members) > api.MaxGroupCursor {
		return api.GroupEventsResult{}, api.NewError(api.CodeInvalidRequest,
			fmt.Sprintf("group %s has %d sessions: its events are read for at most %d", p.Group, len(members), api.MaxGroupCursor),
			"read some members' events with 'eyedbg events -s ID'")
	}

	return groupEvents(ctx, members, cursor, p, wait), nil
}

// groupEvents is GroupEvents on members from cursor.
func groupEvents(ctx context.Context, members []*Session, cursor map[string]int, p api.GroupEventsParams, wait time.Duration) api.GroupEventsResult {
	res, next, watch := groupEventsOnce(members, cursor, p)
	if len(res.Events) > 0 || wait <= 0 {
		return res
	}

	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	// Nothing matched: next moved each member past what it had, and the
	// dropped events were counted.
	dropped := res.Dropped

	for anyClosed(ctx, watch) {
		res, next, watch = groupEventsOnce(members, next, p)
		dropped += res.Dropped
		res.Dropped = dropped

		if len(res.Events) > 0 {
			return res
		}
	}

	// An append racing the deadline still counts.
	res, _, _ = groupEventsOnce(members, next, p)
	res.Dropped += dropped
	res.TimedOut = len(res.Events) == 0

	return res
}

// memberEvents is one member's part of a group events query.
type memberEvents struct {
	s      *Session
	events []api.Event
	latest int
	// more counts its matching events its own query left out.
	more int
}

// groupEventsOnce queries every member once, without waiting: it returns
// the result, the next cursor (as a map) and each member's log channel its
// next append closes, read with its query.
func groupEventsOnce(members []*Session, cursor map[string]int, p api.GroupEventsParams) (
	res api.GroupEventsResult, next map[string]int, watch []<-chan struct{},
) {
	limit := eventLimit(p.Limit)
	parts := make([]memberEvents, len(members))
	watch = make([]<-chan struct{}, len(members))

	for i, s := range members {
		r, ch := s.log.queryWatch(api.EventsParams{Since: cursor[s.ID], Kinds: p.Kinds, Limit: limit, Newest: p.Newest})
		parts[i] = memberEvents{s: s, events: r.Events, latest: r.Latest, more: r.More}
		watch[i] = ch
		res.Dropped += r.Dropped
		res.More += r.More
	}

	merged := mergeEvents(parts)

	kept := merged
	if len(kept) > limit {
		if p.Newest {
			kept = kept[len(kept)-limit:]
		} else {
			kept = kept[:limit]
		}
	}

	kept, _ = present.ShapeGroupEvents(kept, p.Budget, p.Newest)
	res.Events = kept
	res.More += len(merged) - len(kept) // what the limit and the budget cut

	next = nextCursor(parts, kept, cursor, p.Newest)
	res.Cursor = formatGroupCursor(members, next)

	return res, next, watch
}

// mergeEvents merges the members' events (each oldest first) by time, then
// session id, then seq. It is a k-way merge, so a member's events keep their
// own order whatever their times. O(n·k) for n events of k members.
func mergeEvents(parts []memberEvents) []api.GroupEvent {
	total := 0
	for _, pt := range parts {
		total += len(pt.events)
	}

	out := make([]api.GroupEvent, 0, total)
	heads := make([]int, len(parts))

	for len(out) < total {
		best := -1

		for i, pt := range parts {
			if heads[i] == len(pt.events) {
				continue
			}

			if best < 0 || mergeBefore(&pt.events[heads[i]], pt.s.ID, &parts[best].events[heads[best]], parts[best].s.ID) {
				best = i
			}
		}

		pt := parts[best]
		out = append(out, api.GroupEvent{SessionID: pt.s.ID, Service: pt.s.service(), Event: pt.events[heads[best]]})
		heads[best]++
	}

	return out
}

// mergeBefore reports whether event a of session aID merges before event b
// of session bID (different sessions).
func mergeBefore(a *api.Event, aID string, b *api.Event, bID string) bool {
	if c := a.Time.Compare(b.Time); c != 0 {
		return c < 0
	}

	return aID < bID
}

// nextCursor is the cursor after kept, the events returned from parts
// queried at cursor. A member all of whose matching events were returned
// moves to its newest seq (past non-matching ones too). One with some left
// out moves to its last returned event — those left out come after it,
// as the limit and the budget keep the oldest — or, with newest (they
// keep the newest, so those left out come before), stays. A member's
// cursor never moves back.
func nextCursor(parts []memberEvents, kept []api.GroupEvent, cursor map[string]int, newest bool) map[string]int {
	returned := make(map[string]int, len(parts))
	last := make(map[string]int, len(parts))

	for i := range kept {
		returned[kept[i].SessionID]++
		last[kept[i].SessionID] = kept[i].Event.Seq
	}

	next := make(map[string]int, len(parts))

	for _, pt := range parts {
		id, at := pt.s.ID, cursor[pt.s.ID]

		switch {
		case pt.more == 0 && returned[id] == len(pt.events):
			at = max(at, pt.latest)
		case !newest && returned[id] > 0:
			at = max(at, last[id])
		}

		next[id] = at
	}

	return next
}

// maxCursorID is the longest session id a group cursor accepts.
const maxCursorID = 32

// parseGroupCursor parses a group events cursor: comma-separated
// "SESSION:SEQ", at most api.MaxGroupCursor entries, each session once, SEQ
// a decimal number of at least 0. Empty is the empty cursor.
func parseGroupCursor(s string) (map[string]int, error) {
	cursor := make(map[string]int)
	if s == "" {
		return cursor, nil
	}

	entries := strings.Split(s, ",")
	if len(entries) > api.MaxGroupCursor {
		return nil, cursorError(fmt.Sprintf("it has %d entries, at most %d", len(entries), api.MaxGroupCursor))
	}

	for _, e := range entries {
		id, seq, ok := strings.Cut(e, ":")
		if !ok || !validCursorID(id) || !allDigits(seq) {
			return nil, cursorError(fmt.Sprintf("entry %q is not SESSION:SEQ", present.CutText(e, 40, false)))
		}

		n, err := strconv.Atoi(seq)
		if err != nil {
			return nil, cursorError(fmt.Sprintf("entry %q: %v", present.CutText(e, 40, false), err))
		}

		if _, dup := cursor[id]; dup {
			return nil, cursorError("it names " + id + " twice")
		}

		cursor[id] = n
	}

	return cursor, nil
}

// formatGroupCursor is the cursor naming each member at its seq in at, in
// members' order.
func formatGroupCursor(members []*Session, at map[string]int) string {
	entries := make([]string, len(members))
	for i, s := range members {
		entries[i] = s.ID + ":" + strconv.Itoa(at[s.ID])
	}

	return strings.Join(entries, ",")
}

func cursorError(why string) error {
	return api.NewError(api.CodeInvalidRequest, "invalid group events cursor: "+why,
		"pass the cursor an earlier result gave, or none to start from the first events")
}

// validCursorID reports whether id looks like a session id: "s-" and 1 to
// 30 lowercase letters or digits.
func validCursorID(id string) bool {
	rest, ok := strings.CutPrefix(id, "s-")
	if !ok || rest == "" || len(id) > maxCursorID {
		return false
	}

	return !strings.ContainsFunc(rest, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') })
}

func allDigits(s string) bool {
	return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
}
