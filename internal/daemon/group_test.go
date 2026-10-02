// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// startGroupMember starts a fake session of group app: stopped at entry,
// or running to its end.
func startGroupMember(t *testing.T, p Paths, stopped bool) api.Snapshot {
	t.Helper()

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var snap api.Snapshot

	mustCall(t, p, api.MethodSessionStart, api.StartParams{
		Lang: "fake", Group: "app", Wait: api.Duration(20 * time.Second),
		LaunchSpec: api.LaunchSpec{Program: prog, StopOnEntry: stopped},
	}, &snap)

	return snap
}

func TestGroupWait(t *testing.T) {
	t.Parallel()

	p := startServer(t).paths
	a := startGroupMember(t, p, true)
	startGroupMember(t, p, false)

	var res api.GroupWaitResult

	mustCall(t, p, api.MethodGroupWait, api.GroupWaitParams{Group: "app", Wait: api.Duration(20 * time.Second)}, &res)

	if res.Group != "app" || res.Session == nil || res.Session.Session.ID != a.Session.ID || res.Session.Frame == nil || res.TimedOut {
		t.Fatalf("group.wait = %+v, want %s stopped at entry", res, a.Session.ID)
	}
}

func TestGroupEvents(t *testing.T) {
	t.Parallel()

	p := startServer(t).paths
	a := startGroupMember(t, p, true)
	b := startGroupMember(t, p, false)
	started := []api.EventKind{api.EventStarted}

	var events api.GroupEventsResult

	mustCall(t, p, api.MethodGroupEvents, api.GroupEventsParams{Group: "app", Kinds: started}, &events)

	if len(events.Events) != 2 || events.Events[0].SessionID != a.Session.ID || events.Events[1].SessionID != b.Session.ID {
		t.Fatalf("group.events = %+v, want both members' started events, oldest first", events)
	}

	if want := a.Session.ID + ":"; !strings.HasPrefix(events.Cursor, want) || !strings.Contains(events.Cursor, ","+b.Session.ID+":") {
		t.Errorf("cursor %q, want both members", events.Cursor)
	}

	// The cursor resumes: nothing new of that kind. b may still be logging
	// other events, which the cursor moves past; it never moves back.
	var next api.GroupEventsResult

	mustCall(t, p, api.MethodGroupEvents, api.GroupEventsParams{Group: "app", Since: events.Cursor, Kinds: started}, &next)

	if len(next.Events) != 0 {
		t.Errorf("group.events at the cursor = %+v, want nothing new", next)
	}

	before, after := cursorSeqs(t, events.Cursor), cursorSeqs(t, next.Cursor)
	for _, id := range []string{a.Session.ID, b.Session.ID} {
		if after[id] < before[id] {
			t.Errorf("cursor %q moved back from %q", next.Cursor, events.Cursor)
		}
	}
}

// cursorSeqs is a group events cursor's seq per session.
func cursorSeqs(t *testing.T, cursor string) map[string]int {
	t.Helper()

	seqs := make(map[string]int)

	for entry := range strings.SplitSeq(cursor, ",") {
		id, seq, _ := strings.Cut(entry, ":")

		n, err := strconv.Atoi(seq)
		if err != nil {
			t.Fatalf("cursor %q: %v", cursor, err)
		}

		seqs[id] = n
	}

	return seqs
}

// TestGroupEventsWire checks the fields the CLI reads, by their JSON names.
func TestGroupEventsWire(t *testing.T) {
	t.Parallel()

	p := startServer(t).paths
	startGroupMember(t, p, true)

	var raw struct {
		Events []map[string]json.RawMessage `json:"events"`
		Cursor *string                      `json:"cursor"`
		More   int                          `json:"more"`
	}

	mustCall(t, p, api.MethodGroupEvents, api.GroupEventsParams{Group: "app", Limit: 1}, &raw)

	if raw.Cursor == nil || raw.More == 0 || len(raw.Events) != 1 || raw.Events[0]["sessionId"] == nil || raw.Events[0]["event"] == nil {
		t.Errorf("group.events = %+v, want cursor, more and one event with sessionId and event", raw)
	}
}

func TestGroupMethodErrors(t *testing.T) {
	t.Parallel()

	p := startServer(t).paths
	startGroupMember(t, p, true)

	tests := []struct {
		name   string
		method string
		params any
		code   api.Code
	}{
		{"wait, no group", api.MethodGroupWait, api.GroupWaitParams{}, api.CodeInvalidRequest},
		{"wait, unknown group", api.MethodGroupWait, api.GroupWaitParams{Group: "other"}, api.CodeNoSession},
		{"events, no group", api.MethodGroupEvents, api.GroupEventsParams{}, api.CodeInvalidRequest},
		{"events, unknown group", api.MethodGroupEvents, api.GroupEventsParams{Group: "other"}, api.CodeNoSession},
		{"events, bad cursor", api.MethodGroupEvents, api.GroupEventsParams{Group: "app", Since: "s-ab:-1"}, api.CodeInvalidRequest},
		{"events, bad kind", api.MethodGroupEvents, api.GroupEventsParams{Group: "app", Kinds: []api.EventKind{"bogus"}}, api.CodeInvalidRequest},
		{"wait, bad client", api.MethodGroupWait, api.GroupWaitParams{Group: "app", Client: "Human"}, api.CodeInvalidRequest},
		{"bad params", api.MethodGroupEvents, json.RawMessage(`{"group": 3}`), api.CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := callAs(t, p, tt.method, tt.params, nil); api.CodeOf(err) != tt.code {
				t.Errorf("err = %v, want %s", err, tt.code)
			}
		})
	}
}
