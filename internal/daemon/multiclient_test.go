// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

const humanID = "human:test"

// callAs sends one request on a fresh connection, like the CLI does.
func callAs(t *testing.T, p Paths, method string, params, result any) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cl, err := Dial(ctx, p, testInfo)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cl.Close()

	return cl.Call(ctx, method, params, result)
}

func mustCall(t *testing.T, p Paths, method string, params, result any) {
	t.Helper()

	if err := callAs(t, p, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// startFake starts a fake session stopped at entry, for client.
func startFake(t *testing.T, p Paths, client string, policy api.LeasePolicy, args ...string) api.Snapshot {
	t.Helper()

	// The program exists because breakpoints need their file to.
	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var snap api.Snapshot

	mustCall(t, p, api.MethodSessionStart, api.StartParams{
		Lang: "fake", Client: client, LeasePolicy: policy, Wait: api.Duration(20 * time.Second),
		LaunchSpec: api.LaunchSpec{Program: prog, StopOnEntry: true, Args: args},
	}, &snap)

	if snap.Session.State != api.StateStopped {
		t.Fatalf("start: %+v, want stopped at entry", snap.Session)
	}

	return snap
}

func eventKinds(events []api.Event) string {
	var b strings.Builder

	for i := range events {
		b.WriteString(string(events[i].Kind))

		if events[i].Action != "" {
			b.WriteString(":" + events[i].Action)
		}

		b.WriteString(" ")
	}

	return b.String()
}

func TestMultiClientSession(t *testing.T) {
	t.Parallel()

	ts := startServer(t)
	p := ts.paths
	snap := startFake(t, p, "", api.LeaseHandoff)
	id, file := snap.Session.ID, snap.Session.Program
	agent, human := api.SessionRef{SessionID: id}, api.SessionRef{SessionID: id, Client: humanID}

	var bp api.Breakpoint

	mustCall(t, p, api.MethodBreakpointAdd, api.BreakpointAddParams{SessionRef: agent, BreakpointSpec: api.BreakpointSpec{File: file, Line: 3}}, &bp)
	mustCall(t, p, api.MethodBreakpointAdd, api.BreakpointAddParams{SessionRef: human, BreakpointSpec: api.BreakpointSpec{File: file, Line: 5}}, &bp)

	var mine []api.Breakpoint

	mustCall(t, p, api.MethodBreakpointLs, api.BreakpointListParams{SessionRef: human, Mine: true}, &mine)

	if len(mine) != 1 || mine[0].Owner != humanID || mine[0].Line != 5 {
		t.Errorf("human's breakpoints = %+v", mine)
	}

	err := callAs(t, p, api.MethodExec, api.ExecParams{SessionRef: human, Kind: "continue", Wait: api.Duration(20 * time.Second)}, &snap)
	if api.CodeOf(err) != api.CodeLeaseHeld {
		t.Fatalf("human continue under handoff: %v, want LEASE_HELD", err)
	}

	var lease api.LeaseResult

	mustCall(t, p, api.MethodLeaseGrant, api.LeaseGrantParams{SessionRef: agent, To: humanID}, &lease)

	if lease.Lease.Holder != humanID || lease.SessionID != id {
		t.Errorf("grant = %+v", lease)
	}

	mustCall(t, p, api.MethodExec, api.ExecParams{SessionRef: human, Kind: "continue", Wait: api.Duration(20 * time.Second)}, &snap)

	if snap.Session.Stop == nil || snap.Frame == nil || snap.Frame.Line != 3 {
		t.Errorf("human continue: %+v (frame %+v), want stopped at 3", snap.Session, snap.Frame)
	}

	var res api.EventsResult

	mustCall(t, p, api.MethodEvents, api.EventsParams{SessionRef: agent, Kinds: []api.EventKind{
		api.EventStarted, api.EventClient, api.EventLease, api.EventExec, api.EventStopped, api.EventBreakpoint,
	}}, &res)

	want := "started stopped breakpoint:added client breakpoint:added lease:grant exec:continue stopped "
	if got := eventKinds(res.Events); got != want {
		t.Errorf("events = %q\nwant     %q", got, want)
	}

	waitForBreakpointEvent(t, p, agent, res.Latest, file)
	assertListed(t, p, id)
}

// waitForBreakpointEvent long-polls for a breakpoint event on one
// connection while another adds one.
func waitForBreakpointEvent(t *testing.T, p Paths, ref api.SessionRef, since int, file string) {
	t.Helper()

	done := make(chan api.EventsResult, 1)

	go func() {
		var res api.EventsResult
		if err := callAs(t, p, api.MethodEvents, api.EventsParams{
			SessionRef: ref, Since: since, Kinds: []api.EventKind{api.EventBreakpoint}, Wait: api.Duration(20 * time.Second),
		}, &res); err != nil {
			t.Error(err)
		}

		done <- res
	}()

	var bp api.Breakpoint

	mustCall(t, p, api.MethodBreakpointAdd, api.BreakpointAddParams{SessionRef: ref, BreakpointSpec: api.BreakpointSpec{File: file, Line: 7}}, &bp)

	if res := <-done; len(res.Events) != 1 || res.Events[0].Breakpoint == nil || res.Events[0].Breakpoint.ID != bp.ID {
		t.Errorf("events wait = %+v, want breakpoint %d", res, bp.ID)
	}
}

func assertListed(t *testing.T, p Paths, id string) {
	t.Helper()

	var list []api.SessionInfo

	mustCall(t, p, api.MethodSessionList, nil, &list)

	if len(list) != 1 || list[0].ID != id || list[0].Lease == nil || list[0].Lease.Holder != humanID || len(list[0].Clients) != 2 {
		t.Fatalf("list = %+v, want the session with its lease and 2 clients", list)
	}

	var ids []string
	for _, c := range list[0].Clients {
		ids = append(ids, c.ID)
	}

	if !reflect.DeepEqual(ids, []string{"agent", humanID}) {
		t.Errorf("clients = %v", ids)
	}
}

func TestInvalidClient(t *testing.T) {
	t.Parallel()

	ts := startServer(t)

	for _, tt := range []struct {
		method string
		params any
	}{
		{api.MethodExec, api.ExecParams{SessionRef: api.SessionRef{Client: "robot"}, Kind: "continue"}},
		{api.MethodSessionStart, api.StartParams{Lang: "fake", Client: "Human"}},
		{api.MethodSessionStop, api.SessionRef{Client: "agent:"}},
	} {
		err := callAs(t, ts.paths, tt.method, tt.params, nil)

		e, ok := errors.AsType[*api.Error](err)
		if !ok || e.Code != api.CodeInvalidRequest || !strings.Contains(e.Hint, "--as") {
			t.Errorf("%s: %v, want INVALID_REQUEST with a hint about --as", tt.method, err)
		}
	}
}

func TestLostSessions(t *testing.T) {
	t.Parallel()

	paths := PathsIn(shortTempDir(t))
	if err := os.MkdirAll(paths.Sessions, 0o700); err != nil {
		t.Fatal(err)
	}

	store := NewFileStore(paths.Sessions, 1)
	if err := store.Save(api.SessionInfo{ID: "s-lost", Lang: "fake", Program: "/x/prog", State: api.StateRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	w, recording, err := store.Record("s-lost")
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	ts := startServerIn(t, paths, time.Hour)

	var list []api.SessionInfo

	mustCall(t, ts.paths, api.MethodSessionList, nil, &list)

	if len(list) != 1 || list[0].ID != "s-lost" || list[0].State != api.StateLost {
		t.Fatalf("list = %+v, want s-lost as lost", list)
	}

	err = callAs(t, ts.paths, api.MethodSessionStatus, api.StatusParams{SessionRef: api.SessionRef{SessionID: "s-lost"}}, nil)
	if api.CodeOf(err) != api.CodeNoSession {
		t.Errorf("status of a lost session: %v, want NO_SESSION", err)
	}

	var info api.SessionInfo

	mustCall(t, ts.paths, api.MethodSessionStop, api.SessionRef{SessionID: "s-lost"}, &info)

	if _, err := os.Stat(filepath.Join(paths.Sessions, "s-lost.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("metadata after forgetting: %v, want it gone", err)
	}

	if _, err := os.Stat(recording); err != nil {
		t.Errorf("recording after forgetting: %v, want it kept", err)
	}
}

func TestCleanShutdownLeavesNoMetadata(t *testing.T) {
	t.Parallel()

	ts := startServer(t)
	snap := startFake(t, ts.paths, "", "")
	meta := filepath.Join(ts.paths.Sessions, snap.Session.ID+".json")

	if _, err := os.Stat(meta); err != nil {
		t.Fatalf("metadata of a live session: %v", err)
	}

	ts.stop()

	if err := ts.wait(t); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(meta); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("metadata after a clean shutdown: %v, want it gone", err)
	}

	if _, err := os.Stat(snap.Session.Recording); err != nil {
		t.Errorf("recording after a clean shutdown: %v, want it kept", err)
	}
}

func TestRecordingFile(t *testing.T) {
	t.Parallel()

	ts := startServer(t)
	snap := startFake(t, ts.paths, "", "", "lines=2")
	ref := api.SessionRef{SessionID: snap.Session.ID}

	mustCall(t, ts.paths, api.MethodExec, api.ExecParams{SessionRef: ref, Kind: "continue", Wait: api.Duration(20 * time.Second)}, &snap)

	if snap.Session.State != api.StateExited {
		t.Fatalf("after continue: %+v, want exited", snap.Session)
	}

	var info api.SessionInfo

	mustCall(t, ts.paths, api.MethodSessionStop, ref, &info)

	b, err := os.ReadFile(info.Recording)
	if err != nil {
		t.Fatal(err)
	}

	var kinds []string

	for line := range strings.Lines(string(b)) {
		var e api.Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}

		if e.Kind == api.EventOutput {
			t.Errorf("recorded output: %s", line)
		}

		kinds = append(kinds, string(e.Kind))
	}

	if len(kinds) < 2 || kinds[0] != "started" || kinds[len(kinds)-1] != "ended" {
		t.Errorf("recorded %v, want started ... ended", kinds)
	}

	if runtime.GOOS != "windows" {
		if st, err := os.Stat(info.Recording); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("recording mode = %v, %v; want 0600", st.Mode(), err)
		}
	}
}
