// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// memberSession is a stopped container session of service in group myapp.
func memberSession(id, service string, stoppedFor, unhealthyAfter time.Duration) api.SessionInfo {
	s := containerSession()
	at := renderTime.Add(-stoppedFor)

	s.ID, s.StoppedAt = id, &at
	s.Container.Service, s.Container.Name = service, "myapp-"+service+"-1"
	s.Container.UnhealthyAfter = api.Duration(unhealthyAfter)
	s.Program = service + " (compose myapp, container " + s.Container.Name + ", pid 1)"

	return s
}

func memberSnapshot(id, service string, stoppedFor time.Duration) api.Snapshot {
	snap := stoppedSnapshot()
	snap.Session = memberSession(id, service, stoppedFor, 18*time.Second)

	return snap
}

func TestComposeAttachRendering(t *testing.T) {
	t.Parallel()

	web := memberSession("s-k3f9", "web", 0, 18*time.Second)
	web.State, web.Stop, web.StoppedAt = api.StateRunning, nil, nil

	consumer := memberSession("s-m2n4", "consumer", 0, 0)
	consumer.State, consumer.Stop, consumer.StoppedAt = api.StateRunning, nil, nil
	consumer.Container.Map = []api.PathMapping{{Remote: "/app", Local: "/work/other"}}

	bpVerified := &api.Breakpoint{ID: 1, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 42, Line: 42, Verified: true}
	bpPending := &api.Breakpoint{ID: 2, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 42, Line: 44, Message: "module not loaded"}
	boom := api.NewError(api.CodeAttachFailed, "container myapp-api-1 is not running", "start it first")
	skip := "pid 1 runs postgres"

	shared := []attachedMember{
		{Service: "web", Container: "myapp-web-1", Session: &web, Breakpoints: []bpOutcome{
			{Location: "Program.cs:42", Breakpoint: bpVerified}, {Location: "Orders.cs:9", Skipped: "its file is outside this service's path map"},
		}},
		{Service: "consumer", Container: "myapp-consumer-1", Session: &consumer, Breakpoints: []bpOutcome{
			{Location: "Program.cs:42", Breakpoint: bpPending}, {Location: "Orders.cs:9", Error: boom},
		}},
		{Service: "db", Container: "myapp-db-1", Skipped: skip},
		{Service: "api", Container: "myapp-api-1", Error: boom},
	}

	consumerSame := memberSession("s-m2n4", "consumer", 0, 0)
	consumerSame.State, consumerSame.Stop, consumerSame.StoppedAt = api.StateRunning, nil, nil

	same := slices.Clone(shared)
	same[1].Session = &consumerSame

	nothing := []attachedMember{
		{Service: "db", Container: "myapp-db-1", Skipped: skip},
		{Service: "api", Container: "myapp-api-1", Error: boom},
	}

	tests := []struct {
		name, golden string
		members      []attachedMember
		asJSON       bool
	}{
		{"one map, breakpoints", "compose_attach.golden", same, false},
		{"different maps", "compose_attach_maps.golden", shared, false},
		{"nothing attached", "compose_attach_none.golden", nothing, false},
		{"json", "compose_attach_json.golden", same, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := writeComposeAttach(&b, "myapp", tt.members, tt.asJSON, renderBase); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

func TestComposeWaitRendering(t *testing.T) {
	t.Parallel()

	answered := memberSnapshot("s-k3f9", "web", 5*time.Second)
	answered.Session.Container.Map = []api.PathMapping{{Remote: "/src", Local: "/work/app"}}

	other := api.GroupStop{SessionID: "s-m2n4", Service: "consumer", Reason: "pause", ThreadID: 7, StoppedAt: renderTime.Add(-12 * time.Second)}
	bare := api.GroupStop{SessionID: "s-q8x1", Reason: "exception", StoppedAt: renderTime.Add(-3 * time.Minute)}

	code := 0
	exited := api.Snapshot{Session: memberSession("s-k3f9", "web", 0, 0)}
	exited.Session.State, exited.Session.Stop, exited.Session.StoppedAt, exited.Session.ExitCode = api.StateExited, nil, nil, &code
	exited.Session.EndReason = "the program terminated"

	tests := []struct {
		name, golden string
		res          api.GroupWaitResult
		asJSON       bool
	}{
		{"stopped, others stopped too", "compose_wait.golden", api.GroupWaitResult{Group: "myapp", Session: &answered, Service: "web", Stopped: []api.GroupStop{other, bare}}, false},
		{"stopped, json", "compose_wait_json.golden", api.GroupWaitResult{Group: "myapp", Session: &answered, Service: "web", Stopped: []api.GroupStop{other}}, true},
		{"timed out, some already stopped", "compose_wait_timeout.golden", api.GroupWaitResult{Group: "myapp", TimedOut: true, Stopped: []api.GroupStop{other}}, false},
		{"timed out", "compose_wait_timeout_none.golden", api.GroupWaitResult{Group: "myapp", TimedOut: true}, false},
		{"member exited", "compose_wait_exited.golden", api.GroupWaitResult{Group: "myapp", Session: &exited, Service: "web", Ended: true}, false},
		{"all ended", "compose_wait_ended.golden", api.GroupWaitResult{Group: "myapp", Ended: true}, false},
		{"timed out, json", "compose_wait_timeout_json.golden", api.GroupWaitResult{Group: "myapp", TimedOut: true}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := writeGroupWait(&b, tt.res, groupWaitView{timeout: 30 * time.Second}, tt.asJSON, renderBase, renderTime); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

func TestComposeEventsRendering(t *testing.T) {
	t.Parallel()

	code := 1
	events := []api.GroupEvent{
		{SessionID: "s-k3f9", Service: "web", Event: api.Event{Seq: 3, Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "breakpoint", ThreadID: 4}}},
		{SessionID: "s-m2n4", Service: "consumer", Event: api.Event{Seq: 2, Kind: api.EventOutput, Category: "stdout", Text: "tick 5\n"}},
		{SessionID: "s-q8x1", Event: api.Event{Seq: 9, Kind: api.EventExited, ExitCode: &code}},
	}
	scaled := []api.GroupEvent{
		{SessionID: "s-k3f9", Service: "web", Event: api.Event{Seq: 3, Kind: api.EventContinued}},
		{SessionID: "s-m2n4", Service: "web", Event: api.Event{Seq: 8, Kind: api.EventContinued}},
		{SessionID: "s-q8x1", Service: "db", Event: api.Event{Seq: 1, Kind: api.EventContinued}},
	}

	tests := []struct {
		name, golden string
		res          api.GroupEventsResult
		view         groupEventsView
		asJSON       bool
	}{
		{"oldest first", "compose_events.golden", api.GroupEventsResult{Events: events, Cursor: "s-k3f9:3,s-m2n4:2,s-q8x1:9", More: 4}, groupEventsView{group: "myapp"}, false},
		{"newest", "compose_events_newest.golden", api.GroupEventsResult{Events: events, Cursor: "s-k3f9:0,s-m2n4:2,s-q8x1:9", More: 12, Dropped: 2}, groupEventsView{group: "myapp", newest: true}, false},
		{"same service twice", "compose_events_scaled.golden", api.GroupEventsResult{Events: scaled, Cursor: "s-k3f9:3,s-m2n4:8,s-q8x1:1"}, groupEventsView{group: "myapp"}, false},
		{"nothing", "compose_events_none.golden", api.GroupEventsResult{Cursor: "s-k3f9:3"}, groupEventsView{group: "myapp"}, false},
		{"wait timed out", "compose_events_timeout.golden", api.GroupEventsResult{Cursor: "s-k3f9:3", TimedOut: true}, groupEventsView{group: "myapp", wait: true, timeout: 30 * time.Second}, false},
		{"json", "compose_events_json.golden", api.GroupEventsResult{Events: events[:1], Cursor: "s-k3f9:3"}, groupEventsView{group: "myapp"}, true},
		{"json nothing", "compose_events_none_json.golden", api.GroupEventsResult{Cursor: "s-k3f9:3"}, groupEventsView{group: "myapp"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := writeGroupEvents(&b, tt.res, tt.view, tt.asJSON, renderBase); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

func TestComposeBreakpointAndStopRendering(t *testing.T) {
	t.Parallel()

	bp := &api.Breakpoint{ID: 3, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 42, Line: 42, Verified: true, Condition: "i > 3"}
	moved := &api.Breakpoint{ID: 1, Owner: "agent", File: "/work/app/Program.cs", RequestedLine: 42, Line: 44, Message: "module not loaded"}
	boom := api.NewError(api.CodeLeaseHeld, "the lease is held by human:ijat", "ask for it")

	adds := []bpMember{
		{Service: "web", SessionID: "s-k3f9", bpOutcome: bpOutcome{Location: "Program.cs:42", Breakpoint: bp}},
		{Service: "consumer", SessionID: "s-m2n4", bpOutcome: bpOutcome{Location: "Program.cs:42", Breakpoint: moved}},
		{Service: "db", SessionID: "s-q8x1", bpOutcome: bpOutcome{Location: "Program.cs:42", Skipped: "its file is outside this service's path map"}},
		{SessionID: "s-z0z0", bpOutcome: bpOutcome{Location: "Program.cs:42", Error: boom}},
	}
	lists := []bpListMember{
		{Service: "web", SessionID: "s-k3f9", Breakpoints: []api.Breakpoint{*bp, *moved}},
		{Service: "consumer", SessionID: "s-m2n4"},
		{SessionID: "s-q8x1", Error: boom},
	}
	removes := []bpRemoveMember{
		{Service: "web", SessionID: "s-k3f9", Removed: 2, Kept: 1},
		{Service: "consumer", SessionID: "s-m2n4"},
		{SessionID: "s-q8x1", Error: boom},
	}

	detached := memberSession("s-k3f9", "web", 0, 0)
	detached.Mode, detached.State, detached.EndReason = api.ModeAttach, api.StateExited, "detached by agent (the program keeps running)"

	launched := memberSession("s-m2n4", "consumer", 0, 0)
	launched.Mode, launched.State, launched.EndReason = "", api.StateExited, "stopped by agent"
	launched.Container.Launched = true

	ended := memberSession("s-q8x1", "db", 0, 0)
	ended.Mode, ended.State, ended.EndReason = api.ModeAttach, api.StateExited, "the debug adapter exited"

	stops := []stopMember{
		{Service: "web", SessionID: "s-k3f9", Session: &detached},
		{Service: "consumer", SessionID: "s-m2n4", Session: &launched},
		{Service: "db", SessionID: "s-q8x1", Session: &ended},
		{SessionID: "s-z0z0", Error: boom},
	}

	tests := []struct {
		name, golden string
		render       func(*bytes.Buffer) error
	}{
		{"bp add", "compose_bp_add.golden", func(b *bytes.Buffer) error { return writeComposeBpAdd(b, "myapp", adds, false, renderBase) }},
		{"bp add json", "compose_bp_add_json.golden", func(b *bytes.Buffer) error { return writeComposeBpAdd(b, "myapp", adds, true, renderBase) }},
		{"bp ls", "compose_bp_ls.golden", func(b *bytes.Buffer) error { return writeComposeBpList(b, "myapp", lists, false, renderBase) }},
		{"bp ls json", "compose_bp_ls_json.golden", func(b *bytes.Buffer) error { return writeComposeBpList(b, "myapp", lists, true, renderBase) }},
		{"bp rm", "compose_bp_rm.golden", func(b *bytes.Buffer) error { return writeComposeBpRemove(b, "myapp", removes, false) }},
		{"bp rm json", "compose_bp_rm_json.golden", func(b *bytes.Buffer) error { return writeComposeBpRemove(b, "myapp", removes, true) }},
		{"stop", "compose_stop.golden", func(b *bytes.Buffer) error { return writeComposeStop(b, "myapp", stops, false) }},
		{"stop json", "compose_stop_json.golden", func(b *bytes.Buffer) error { return writeComposeStop(b, "myapp", stops, true) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := tt.render(&b); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

// TestLongStopNotes: a container session shows how long it has been
// stopped, a note once that passes its healthcheck's failure time, and a
// Kafka note from four minutes; a host session or one that has no
// healthcheck shows less.
func TestLongStopNotes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		stoppedFor     time.Duration
		unhealthyAfter time.Duration
		wantHeader     string
		wantNotes      int
		wantNote       string
	}{
		{"short stop", 5 * time.Second, 18 * time.Second, "(stopped 5s)", 0, ""},
		{"exactly at the healthcheck time", 18 * time.Second, 18 * time.Second, "(stopped 18s)", 1, "past the 18s"},
		{"no healthcheck", time.Minute, 0, "(stopped 1m0s)", 0, ""},
		{"kafka note", 4 * time.Minute, 18 * time.Second, "(stopped 4m0s)", 2, "Kafka"},
		{"kafka note without a healthcheck", 5 * time.Minute, 0, "(stopped 5m0s)", 1, "5 minutes by default"},
		{"just under the kafka note", 4*time.Minute - time.Second, 0, "(stopped 3m59s)", 0, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snap := stoppedSnapshot()
			snap.Session = memberSession("s-k3f9", "web", tt.stoppedFor, tt.unhealthyAfter)

			if got := snapshotHeader(snap, renderTime); !strings.HasSuffix(got, tt.wantHeader) {
				t.Errorf("header = %q, want it to end with %q", got, tt.wantHeader)
			}

			notes := longStopNotes(&snap.Session, renderTime)
			if len(notes) != tt.wantNotes || (tt.wantNote != "" && !strings.Contains(strings.Join(notes, "\n"), tt.wantNote)) {
				t.Errorf("notes = %q, want %d containing %q", notes, tt.wantNotes, tt.wantNote)
			}
		})
	}

	// Neither a host session, nor a running or exited container session.
	host := stoppedSnapshot()
	if got := snapshotHeader(host, renderTime.Add(time.Hour)); strings.Contains(got, "stopped 1h") || len(longStopNotes(&host.Session, renderTime.Add(time.Hour))) != 0 {
		t.Errorf("a host session shows how long it has been stopped: %q", got)
	}

	running := memberSession("s-k3f9", "web", time.Hour, time.Second)
	running.State = api.StateRunning

	if _, ok := stoppedFor(&running, renderTime); ok {
		t.Error("a running session has been stopped for some time")
	}

	// A clock that is behind the stop doesn't show a negative time.
	future := memberSession("s-k3f9", "web", -time.Hour, time.Second)
	if d, ok := stoppedFor(&future, renderTime); !ok || d != 0 {
		t.Errorf("stoppedFor = %v, %v; want 0, true", d, ok)
	}
}

func TestLongStopGoldens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, golden string
		stoppedFor   time.Duration
	}{
		{"unhealthy and kafka", "snapshot_container_kafka.golden", 5*time.Minute + 20*time.Second},
		{"short", "snapshot_container_short.golden", 5 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := writeSnapshotAt(&b, memberSnapshot("s-k3f9", "web", tt.stoppedFor), false, renderBase, renderTime); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

func TestEndedPhrase(t *testing.T) {
	t.Parallel()

	container := &api.ContainerInfo{Name: "web-1"}
	launchedIn := &api.ContainerInfo{Name: "web-1", Launched: true}

	tests := []struct {
		name string
		info api.SessionInfo
		want string
	}{
		{"detached from a host process", api.SessionInfo{Mode: api.ModeAttach, PID: 42, EndReason: "detached by agent"}, "detached; pid 42 keeps running"},
		{"detached from a container", api.SessionInfo{Mode: api.ModeAttach, Container: container, EndReason: "detached by agent"}, "detached; container web-1 keeps running"},
		{"an app launched in a container", api.SessionInfo{Container: launchedIn, EndReason: "stopped by agent"}, "app terminated; container web-1 idles"},
		{"a container session that launched nothing", api.SessionInfo{Container: container, EndReason: "stopped by agent"}, "ended"},
		{"a launched host program", api.SessionInfo{EndReason: "stopped by agent"}, "ended"},
		{"an attached program that exited", api.SessionInfo{Mode: api.ModeAttach, Container: container, EndReason: "the debug adapter exited"}, "ended"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := endedPhrase(tt.info); got != tt.want {
				t.Errorf("endedPhrase = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBpOutcomeError(t *testing.T) {
	t.Parallel()

	if got := (bpOutcome{}).text(renderBase); got != "no answer" {
		t.Errorf("empty outcome = %q", got)
	}

	e := api.NewError(api.CodeInvalidRequest, "bad\nthing", "do\tthis")
	if got := errorLine(e); got != "bad thing [INVALID_REQUEST] (do this)" {
		t.Errorf("errorLine = %q", got)
	}

	if got := jsonErr(errors.New("plain")); got == nil || got.Code != "ERROR" {
		t.Errorf("jsonErr(plain) = %+v", got)
	}

	if jsonErr(nil) != nil {
		t.Error("jsonErr(nil) is not nil")
	}
}
