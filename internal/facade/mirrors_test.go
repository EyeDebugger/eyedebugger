// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

const (
	mirrorFile = "/work/app/Program.cs"
	// aliasFile is mirrorFile through a symlink.
	aliasFile = "/link/app/Program.cs"
	self      = "human:t"
)

// lineBP is a verified line breakpoint of owner in mirrorFile.
func lineBP(id int, owner string, line int) api.Breakpoint {
	return api.Breakpoint{ID: id, Owner: owner, File: mirrorFile, RequestedLine: line, Line: line, Verified: true}
}

// classifyBPs are the session's breakpoints of the classify table: the
// agent's at 6 (conditional), 12 and 14 (requested at 13); the human's CLI
// one at 8 and its editor one at 10, which the connection reported.
func classifyBPs() []api.Breakpoint {
	agent6 := lineBP(3, "agent", 6)
	agent6.Condition = "x > 1"
	moved := lineBP(9, "agent", 14)
	moved.RequestedLine = 13
	editor := lineBP(5, self, 10)
	editor.Editor = true

	return []api.Breakpoint{agent6, lineBP(4, self, 8), editor, lineBP(7, "agent", 12), moved}
}

// classifyView shows the agent's breakpoint 3 at line 6 and reports 5.
func classifyView() *view {
	v := newView()
	v.mirrors[3] = &mirror{key: mirrorFile, path: mirrorFile, line: 6, told: godap.Breakpoint{Id: 3, Line: 6, Column: 1, Source: &godap.Source{Path: mirrorFile}}}
	v.reported[5] = godap.Breakpoint{Id: 5, Line: 10}

	return v
}

// sb is a source breakpoint: "N" (plain, no column), "N@1" (column 1) or
// "N if C".
func sb(spec string) godap.SourceBreakpoint {
	var e godap.SourceBreakpoint

	spec, e.Condition, _ = strings.Cut(spec, " if ")
	if l, ok := strings.CutSuffix(spec, "@1"); ok {
		spec, e.Column = l, 1
	}

	_, _ = fmt.Sscan(spec, &e.Line)

	return e
}

// sbs is sb of each spec.
func sbs(specs []string) []godap.SourceBreakpoint {
	out := make([]godap.SourceBreakpoint, len(specs))
	for i, s := range specs {
		out[i] = sb(s)
	}

	return out
}

// kinds renders classes as "own", "echo N", "retract", "dup".
func kinds(classes []entryClass) []string {
	out := make([]string, len(classes))

	for i, c := range classes {
		switch c.kind {
		case entryOwn:
			out[i] = "own"
		case entryEcho:
			out[i] = fmt.Sprintf("echo %d", c.id)
		case entryRetract:
			out[i] = "retract"
		case entryDuplicate:
			out[i] = "dup"
		}
	}

	return out
}

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		entries []string
		key     string
		// path is the request's source path ("" for mirrorFile).
		path string
		// edit changes the view or the breakpoints first.
		edit        func(v *view, bps []api.Breakpoint) []api.Breakpoint
		want        []string
		wantMirrors map[int]int // id: announced line, after settle
		wantHidden  []int
	}{
		{name: "marked, view mirror there", entries: []string{"6@1"}, want: []string{"echo 3"}, wantMirrors: map[int]int{3: 6}},
		{
			name: "marked, view mirror gone, another at the line", entries: []string{"6@1"},
			edit: func(_ *view, bps []api.Breakpoint) []api.Breakpoint {
				return append(slices.DeleteFunc(bps, func(b api.Breakpoint) bool { return b.ID == 3 }), lineBP(11, "agent:b", 6))
			},
			want: []string{"echo 11"}, wantMirrors: map[int]int{11: 6},
		},
		{name: "marked, nothing there", entries: []string{"20@1"}, want: []string{"retract"}, wantMirrors: map[int]int{}, wantHidden: []int{3}},
		{name: "two marked", entries: []string{"6@1", "6@1"}, want: []string{"echo 3", "retract"}, wantMirrors: map[int]int{3: 6}},
		{name: "own and marked", entries: []string{"6", "6@1"}, want: []string{"own", "retract"}, wantMirrors: map[int]int{}},
		{name: "rule A: plain at a view mirror", entries: []string{"6", "6"}, want: []string{"echo 3", "dup"}, wantMirrors: map[int]int{3: 6}},
		{
			name: "rule A: view mirror gone, falls back to it", entries: []string{"6"},
			edit: func(_ *view, bps []api.Breakpoint) []api.Breakpoint {
				return slices.DeleteFunc(bps, func(b api.Breakpoint) bool { return b.ID == 3 })
			},
			want: []string{"echo 3"}, wantMirrors: map[int]int{3: 6},
		},
		{
			name: "plain at a hidden mirror's line is own", entries: []string{"6"},
			edit: func(v *view, bps []api.Breakpoint) []api.Breakpoint {
				delete(v.mirrors, 3)
				v.hidden[3] = true

				return bps
			},
			want: []string{"own"}, wantMirrors: map[int]int{}, wantHidden: []int{3},
		},
		{
			// The rejected generalization: a plain breakpoint where another
			// client has one is the human's own, not a copy.
			name: "plain at another owner's line, no mirror", entries: []string{"12"},
			want: []string{"own"}, wantMirrors: map[int]int{}, wantHidden: []int{3},
		},
		{
			name: "conditional at a view mirror's line: own, mirror dropped", entries: []string{"6 if y"},
			want: []string{"own"}, wantMirrors: map[int]int{},
		},
		{name: "marked at own editor breakpoint's line", entries: []string{"10@1"}, want: []string{"retract"}, wantMirrors: map[int]int{}, wantHidden: []int{3}},
		{name: "marked at own CLI breakpoint's line", entries: []string{"8@1", "6@1"}, want: []string{"echo 4", "echo 3"}, wantMirrors: map[int]int{3: 6, 4: 8}},
		{name: "marked at a requested line", entries: []string{"13@1"}, want: []string{"echo 9"}, wantMirrors: map[int]int{9: 13}, wantHidden: []int{3}},
		{
			name: "one breakpoint echoed once", entries: []string{"13@1", "14@1"}, want: []string{"echo 9", "retract"},
			wantMirrors: map[int]int{9: 13}, wantHidden: []int{3},
		},
		{name: "no file key", entries: []string{"6@1", "6"}, key: "-", want: []string{"own", "own"}, wantMirrors: map[int]int{3: 6}},
		{name: "empty list hides", entries: nil, want: []string{}, wantMirrors: map[int]int{}, wantHidden: []int{3}},
		// The file under another path (a symlink): the editor holds the copy
		// under mirrorFile, not there.
		{name: "other path: plain at the mirror's line is own", entries: []string{"6"}, path: aliasFile, want: []string{"own"}, wantMirrors: map[int]int{3: 6}},
		{name: "other path: empty list hides nothing", entries: nil, path: aliasFile, want: []string{}, wantMirrors: map[int]int{3: 6}},
		{name: "other path: marked is an echo there", entries: []string{"6@1"}, path: aliasFile, want: []string{"echo 3"}, wantMirrors: map[int]int{3: 6}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, bps := classifyView(), classifyBPs()
			if tt.edit != nil {
				bps = tt.edit(v, bps)
			}

			key := map[string]string{"": mirrorFile, "-": ""}[tt.key]
			path := cmp.Or(tt.path, mirrorFile)
			entries := sbs(tt.entries)

			classes := classify(entries, key, path, v, bps, self)
			if got := kinds(classes); !slices.Equal(got, tt.want) {
				t.Fatalf("classify = %v, want %v", got, tt.want)
			}

			checkEchoes(t, classes, path)
			settle(v, key, path, entries, classes)
			checkSettled(t, v, tt.wantMirrors, tt.wantHidden)
		})
	}
}

// checkEchoes: every echo is answered with its mirror, at column 1, under
// the request's path.
func checkEchoes(t *testing.T, classes []entryClass, path string) {
	t.Helper()

	for _, c := range classes {
		if c.kind == entryEcho && (c.told.Id != c.id || c.told.Column != 1 || c.told.Source == nil || c.told.Source.Path != path) {
			t.Errorf("echo answer = %+v, want the mirror %d at column 1 under %s", c.told, c.id, path)
		}
	}
}

// checkSettled checks v's mirrors (id: announced line) and hidden ids.
func checkSettled(t *testing.T, v *view, wantMirrors map[int]int, wantHidden []int) {
	t.Helper()

	mirrors := map[int]int{}
	for id, m := range v.mirrors {
		mirrors[id] = m.line
	}

	if !maps.Equal(mirrors, wantMirrors) {
		t.Errorf("mirrors after = %v, want %v", mirrors, wantMirrors)
	}

	if hidden := slices.Sorted(maps.Keys(v.hidden)); !slices.Equal(hidden, wantHidden) {
		t.Errorf("hidden after = %v, want %v", hidden, wantHidden)
	}
}

// TestEchoTellsOwnCLIBreakpoint: the human's CLI breakpoint is shown as
// theirs, set outside the editor.
func TestEchoTellsOwnCLIBreakpoint(t *testing.T) {
	t.Parallel()

	classes := classify([]godap.SourceBreakpoint{sb("8@1")}, mirrorFile, mirrorFile, classifyView(), classifyBPs(), self)
	if msg := classes[0].told.Message; !strings.HasPrefix(msg, "your breakpoint, set outside this editor") {
		t.Errorf("message = %q", msg)
	}
}

// events renders breakpoint events as "reason id line:column".
func events(evs []godap.EventMessage) []string {
	out := make([]string, 0, len(evs))

	for _, ev := range evs {
		b, ok := ev.(*godap.BreakpointEvent)
		if !ok {
			out = append(out, ev.GetEvent().Event)

			continue
		}

		bp := b.Body.Breakpoint
		out = append(out, fmt.Sprintf("%s %d %d:%d", b.Body.Reason, bp.Id, bp.Line, bp.Column))
	}

	return out
}

func TestReconcile(t *testing.T) {
	t.Parallel()

	fn := api.Breakpoint{ID: 20, Owner: "agent", Function: "f", Verified: true}
	temp := lineBP(21, "agent", 30)
	temp.Temporary = true
	otherEditor := lineBP(22, self, 31)
	otherEditor.Editor = true

	tests := []struct {
		name string
		// setup fills the view; bps are the session's breakpoints.
		setup func(v *view)
		bps   []api.Breakpoint
		want  []string
		// check looks at the view after.
		check func(t *testing.T, v *view)
	}{
		{
			name: "new mirrors, one per line, lowest id",
			bps:  []api.Breakpoint{lineBP(7, "agent", 12), lineBP(3, "agent", 6), lineBP(4, "agent:b", 6)},
			want: []string{"new 3 6:1", "new 7 12:1"},
		},
		{
			name: "own breakpoint's requested and placed lines are taken",
			setup: func(v *view) {
				v.reported[5] = dapBreakpoint(&api.Breakpoint{ID: 5, Line: 10, Verified: true})
			},
			bps: []api.Breakpoint{
				{ID: 5, Owner: self, Editor: true, File: mirrorFile, RequestedLine: 9, Line: 10, Verified: true},
				lineBP(6, "agent", 10), lineBP(8, "agent", 9), lineBP(11, "agent", 11),
			},
			want: []string{"new 11 11:1"},
		},
		{
			name:  "hidden: not shown, forgotten once gone",
			setup: func(v *view) { v.hidden[3], v.hidden[40] = true, true },
			bps:   []api.Breakpoint{lineBP(3, "agent", 6), lineBP(4, "agent", 6)},
			want:  []string{},
			check: func(t *testing.T, v *view) {
				t.Helper()

				if !v.hidden[3] || v.hidden[40] {
					t.Errorf("hidden = %v, want 3 kept and 40 forgotten", v.hidden)
				}
			},
		},
		{
			name: "function, temporary and own editor breakpoints are never mirrored; own CLI ones are",
			bps:  []api.Breakpoint{fn, temp, otherEditor, lineBP(23, self, 32)},
			want: []string{"new 23 32:1"},
			check: func(t *testing.T, v *view) {
				t.Helper()

				if msg := v.mirrors[23].told.Message; !strings.HasPrefix(msg, "your breakpoint") {
					t.Errorf("own CLI mirror message = %q", msg)
				}
			},
		},
		{
			name: "reported: gone is removed, verified is changed",
			setup: func(v *view) {
				v.reported[1] = godap.Breakpoint{Id: 1, Line: 2}
				v.reported[2] = godap.Breakpoint{Id: 2, Line: 3}
				v.lists[bpsKey{path: mirrorFile}] = []int{1, 2}
			},
			bps:  []api.Breakpoint{{ID: 2, Owner: self, Editor: true, File: mirrorFile, RequestedLine: 3, Line: 3, Verified: true}},
			want: []string{"removed 1 0:0", "changed 2 3:0"},
			check: func(t *testing.T, v *view) {
				t.Helper()

				if ids := v.lists[bpsKey{path: mirrorFile}]; !slices.Equal(ids, []int{2}) {
					t.Errorf("list = %v, want [2]", ids)
				}
			},
		},
		{
			name: "mirror moved: changed at column 1, announced line kept",
			setup: func(v *view) {
				b := lineBP(3, "agent", 6)
				v.mirrors[3] = &mirror{key: mirrorFile, path: mirrorFile, line: 6, told: mirrorBreakpoint(&b, self, mirrorFile)}
			},
			bps:  []api.Breakpoint{{ID: 3, Owner: "agent", File: mirrorFile, RequestedLine: 6, Line: 7, Verified: true}},
			want: []string{"changed 3 7:1"},
			check: func(t *testing.T, v *view) {
				t.Helper()

				if m := v.mirrors[3]; m.line != 6 || m.told.Line != 7 {
					t.Errorf("mirror = %+v, want announced at 6, told 7", m)
				}
			},
		},
		{
			name: "order: removed, changed, new, each by id",
			setup: func(v *view) {
				for _, id := range []int{9, 8} {
					b := lineBP(id, "agent", id)
					v.mirrors[id] = &mirror{key: mirrorFile, path: mirrorFile, line: id, told: mirrorBreakpoint(&b, self, mirrorFile)}
				}

				b := lineBP(6, "agent", 6)
				v.mirrors[6] = &mirror{key: mirrorFile, path: mirrorFile, line: 6, told: mirrorBreakpoint(&b, self, mirrorFile)}
				v.mirrors[5] = &mirror{key: mirrorFile, path: mirrorFile, line: 5, told: godap.Breakpoint{Id: 5}}
			},
			bps: []api.Breakpoint{
				{ID: 5, Owner: "agent", File: mirrorFile, RequestedLine: 5, Line: 5},
				{ID: 6, Owner: "agent", File: mirrorFile, RequestedLine: 6, Line: 6},
				lineBP(10, "agent", 10), lineBP(2, "agent", 2),
			},
			want: []string{"removed 8 0:0", "removed 9 0:0", "changed 5 5:1", "changed 6 6:1", "new 2 2:1", "new 10 10:1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v := newView()
			if tt.setup != nil {
				tt.setup(v)
			}

			checkReconcile(t, v, tt.bps, tt.want)

			if tt.check != nil {
				tt.check(t, v)
			}
		})
	}
}

// TestReconcilePaths: new mirrors go under the path the editor set its own
// breakpoints in the file under; an announced mirror keeps its path.
func TestReconcilePaths(t *testing.T) {
	t.Parallel()

	b := lineBP(3, "agent", 6)

	v := newView()
	v.sourceSeen(aliasFile, mirrorFile, true)
	checkReconcile(t, v, []api.Breakpoint{b}, []string{"new 3 6:1"})

	if m := v.mirrors[3]; m.path != aliasFile || m.told.Source.Path != aliasFile {
		t.Errorf("mirror = %+v, want it under %s", m, aliasFile)
	}

	v = newView()
	v.mirrors[3] = &mirror{key: mirrorFile, path: mirrorFile, line: 6, told: mirrorBreakpoint(&b, self, mirrorFile)}
	v.sourceSeen(aliasFile, mirrorFile, true)
	checkReconcile(t, v, []api.Breakpoint{b}, []string{})
}

// checkReconcile: reconciling v with bps (sorted by id here) gives want,
// and a second reconcile gives nothing.
func checkReconcile(t *testing.T, v *view, bps []api.Breakpoint, want []string) {
	t.Helper()

	bps = slices.Clone(bps)
	slices.SortFunc(bps, func(a, b api.Breakpoint) int { return a.ID - b.ID })

	if got := events(reconcile(v, self, bps)); !slices.Equal(got, want) {
		t.Errorf("reconcile = %v, want %v", got, want)
	}

	if again := reconcile(v, self, bps); len(again) != 0 {
		t.Errorf("a second reconcile = %v, want nothing", events(again))
	}
}

// TestFilePaths: the lists under a file's other paths are kept, an id
// listed under two paths stays reported until neither lists it, and only
// a request with a breakpoint of the client's own moves where new mirrors
// are announced.
func TestFilePaths(t *testing.T) {
	t.Parallel()

	v := newView()
	other := "/work/app/Other.cs"
	bp := func(id, line int) api.Breakpoint { return api.Breakpoint{ID: id, File: mirrorFile, Line: line} }

	v.sourceSeen(mirrorFile, mirrorFile, false)
	v.track(bpsKey{path: mirrorFile}, []api.Breakpoint{bp(1, 4), bp(2, 8)})
	v.sourceSeen(aliasFile, mirrorFile, true)
	v.track(bpsKey{path: aliasFile}, []api.Breakpoint{bp(1, 4), bp(3, 9)})
	v.sourceSeen(other, other, true)
	v.track(bpsKey{path: other}, []api.Breakpoint{{ID: 7, File: other, Line: 1}})

	if keep := slices.Sorted(slices.Values(v.keep(mirrorFile, aliasFile))); !slices.Equal(keep, []int{1, 2}) {
		t.Errorf("keep under %s = %v, want [1 2]", aliasFile, keep)
	}

	if got := v.announcePath(mirrorFile); got != aliasFile {
		t.Errorf("announce path = %q, want %q", got, aliasFile)
	}

	v.sourceSeen(mirrorFile, mirrorFile, false)

	if got := v.announcePath(mirrorFile); got != aliasFile {
		t.Errorf("announce path after a copies-only list = %q, want %q", got, aliasFile)
	}

	v.track(bpsKey{path: mirrorFile}, []api.Breakpoint{})

	if _, ok := v.reported[1]; !ok {
		t.Error("1 is no longer reported, though the alias still lists it")
	}

	if _, ok := v.reported[2]; ok {
		t.Error("2 is still reported, though no list has it")
	}

	v.track(bpsKey{path: aliasFile}, []api.Breakpoint{})

	if got := slices.Sorted(maps.Keys(v.reported)); !slices.Equal(got, []int{7}) {
		t.Errorf("reported = %v, want [7]", got)
	}
}

func TestMirrorMessage(t *testing.T) {
	t.Parallel()

	b := lineBP(3, "agent", 6)
	b.Condition, b.HitCondition, b.LogMessage = "x > 1", ">=2", "x={x}"
	b.Message, b.Note = "moved by the adapter", "shares its line"

	msg := mirrorBreakpoint(&b, self, mirrorFile).Message
	for _, want := range []string{"agent's breakpoint", "if x > 1", "hit >=2", "logs x={x}, doesn't stop", "moved by the adapter", "shares its line", "only hides it"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}

	b.Condition = strings.Repeat("é", 400)

	long := mirrorBreakpoint(&b, self, mirrorFile).Message
	if n := utf8.RuneCountInString(long); n != maxMirrorMessage || !strings.HasSuffix(long, hideHint) || !strings.Contains(long, "…") {
		t.Errorf("long message: %d characters, %q", n, long)
	}
}

func TestActivityFilter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ev   api.Event
		want bool
	}{
		{api.Event{Kind: api.EventExec, Client: "agent", Action: "next"}, true},
		{api.Event{Kind: api.EventBreakpoint, Client: "agent", Action: "added"}, true},
		{api.Event{Kind: api.EventExceptions, Client: "agent", Action: "all"}, true},
		{api.Event{Kind: api.EventLease, Client: "agent", Action: "grant"}, true},
		{api.Event{Kind: api.EventClient, Client: "agent", Action: "connected"}, true},
		{api.Event{Kind: api.EventExec, Client: self, Action: "next"}, false},
		{api.Event{Kind: api.EventBreakpoint, Action: "changed"}, false},
		{api.Event{Kind: api.EventOutput, Client: "agent", Text: "x"}, false},
		{api.Event{Kind: api.EventStopped, Client: "agent"}, false},
		{api.Event{Kind: api.EventEnded, Client: "agent"}, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.ev.Kind)+" by "+tt.ev.Client, func(t *testing.T) {
			t.Parallel()

			if got := activity(&tt.ev, self); got != tt.want {
				t.Errorf("activity = %v, want %v", got, tt.want)
			}
		})
	}

	long := activityEvent(&api.Event{Kind: api.EventExec, Client: "agent", Action: "eval", Text: strings.Repeat("x", 1500)})
	if n := len(long.Body.Event.Text); n != 1000 || !long.Body.Event.Truncated {
		t.Errorf("activity text: %d characters, truncated %v", n, long.Body.Event.Truncated)
	}
}

// TestRegisterMessages: one codec decodes the custom requests, their
// responses and the custom events; unknown commands still fail.
func TestRegisterMessages(t *testing.T) {
	t.Parallel()

	codec := godap.NewCodec()
	if err := RegisterMessages(codec); err != nil {
		t.Fatal(err)
	}

	if err := RegisterMessages(codec); err == nil {
		t.Error("registering twice succeeded")
	}

	for _, tt := range codecCases() {
		m, err := codec.DecodeMessage([]byte(tt.raw))
		if err != nil || !tt.check(m) {
			t.Errorf("decode %s = %#v, %v", tt.raw, m, err)
		}
	}

	_, err := codec.DecodeMessage([]byte(`{"seq":9,"type":"request","command":"eyedbg/nothing"}`))
	if _, ok := errors.AsType[*godap.DecodeProtocolMessageFieldError](err); !ok {
		t.Errorf("unknown command: %v, want a field error", err)
	}
}

// codecCase is a raw message and a check of what it decodes to.
type codecCase struct {
	raw   string
	check func(m godap.Message) bool
}

// codecCases are a request, a response and events of each custom message.
func codecCases() []codecCase {
	return []codecCase{
		{`{"seq":1,"type":"request","command":"eyedbg/lease","arguments":{"action":"request","message":"why"}}`, func(m godap.Message) bool {
			r, ok := m.(*LeaseRequest)

			return ok && r.Arguments.Action == LeaseActionRequest && r.Arguments.Message == "why"
		}},
		{`{"seq":2,"type":"response","request_seq":1,"command":"eyedbg/lease","success":true,"body":{"lease":{"policy":"free","holder":"agent"}}}`, func(m godap.Message) bool {
			r, ok := m.(*LeaseResponse)

			return ok && r.Body.Lease.Holder == "agent"
		}},
		{`{"seq":3,"type":"event","event":"eyedbg/lease","body":{"lease":{"policy":"handoff"}}}`, func(m godap.Message) bool {
			e, ok := m.(*LeaseEvent)

			return ok && e.Body.Lease.Policy == api.LeaseHandoff
		}},
		{`{"seq":4,"type":"request","command":"eyedbg/breakpoints","arguments":{"action":"remove","id":3,"force":true}}`, func(m godap.Message) bool {
			r, ok := m.(*BreakpointsRequest)

			return ok && r.Arguments.ID == 3 && r.Arguments.Force
		}},
		{`{"seq":5,"type":"event","event":"eyedbg/activity","body":{"event":{"seq":9,"kind":"exec","client":"agent","action":"next"}}}`, func(m godap.Message) bool {
			e, ok := m.(*ActivityEvent)

			return ok && e.Body.Event.Action == "next"
		}},
		{`{"seq":6,"type":"request","command":"eyedbg/clients"}`, func(m godap.Message) bool { _, ok := m.(*ClientsRequest); return ok }},
		{`{"seq":7,"type":"event","event":"eyedbg/clients","body":{"clients":[{"id":"human:t","kind":"human","connected":1}]}}`, func(m godap.Message) bool {
			e, ok := m.(*ClientsEvent)

			return ok && len(e.Body.Clients) == 1 && e.Body.Clients[0].Connected == 1
		}},
		{`{"seq":8,"type":"event","event":"eyedbg/breakpoints","body":{"breakpoints":[{"id":3,"owner":"agent","editor":true}],"more":2}}`, func(m godap.Message) bool {
			e, ok := m.(*BreakpointsEvent)

			return ok && e.Body.More == 2 && e.Body.Breakpoints[0].Editor
		}},
	}
}
