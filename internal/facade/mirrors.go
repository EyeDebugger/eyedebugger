// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"slices"
	"strings"
	"unicode/utf8"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// What an editor sees of the session's breakpoints (docs/adr/0014): the ones
// its responses reported (its client's editor breakpoints), and mirrors —
// the line breakpoints it didn't set, announced with DAP breakpoint events
// at column 1. VS Code adopts an announced breakpoint as the user's own,
// keeps its column and re-sends it in later setBreakpoints without its
// condition; column 1 is the mark that tells such a copy from a breakpoint
// the user made, so a copy never becomes the human's breakpoint.

// Texts of the breakpoint mirroring.
const (
	retractMessage = "eyedbg: a leftover copy of another client's breakpoint; removed"
	// duplicateMessage is the session's refusal of a second breakpoint on
	// a line in one request.
	duplicateMessage = "another breakpoint of this request is on the same line: eyedbg keeps one per line per client (columns are ignored)"
	hideHint         = " — removing it here only hides it"
	// maxMirrorMessage bounds a mirror's message, in characters.
	maxMirrorMessage = 300
	// markColumn is the column mirrors carry: VS Code never makes a plain
	// breakpoint there itself.
	markColumn = 1
)

// mirror is a breakpoint the connection announced: its file key, the
// source path the editor holds its copy under (the one it was announced or
// last echoed under: an editor keys breakpoints by path, and may have the
// file under several — a symlink), the line it was announced (or echoed)
// at — the line the editor's copy keeps and re-sends — and what the editor
// was last told.
type mirror struct {
	key  string
	path string
	line int
	told godap.Breakpoint
}

// view is what the connection told its editor about the session's
// breakpoints. It is read and written only under the connection's event
// gate.
type view struct {
	// lists are the ids each of the connection's setBreakpoints (by
	// source path) and setFunctionBreakpoints answered last; reported is
	// what those answers, or later events, said of each.
	lists    map[bpsKey][]int
	reported map[int]godap.Breakpoint
	// files is the file key of each source path the connection's
	// setBreakpoints named; latest, per file key, the path of the last one
	// that set a breakpoint of the client's own there: new mirrors of the
	// file are announced under it, so the copies land where the editor
	// shows the file (else under the file key).
	files  map[string]string
	latest map[string]string
	// mirrors are the other breakpoints announced, by id; hidden are the
	// ones the editor dropped, never announced again on this connection.
	mirrors map[int]*mirror
	hidden  map[int]bool
	// lastRetract is the last id given to a retracted copy (negative).
	lastRetract int
	// configured: configurationDone was answered; mirrors are announced.
	configured bool
	// leaving: disconnect was answered; nothing more is sent.
	leaving bool
	// after holds the events the response being written causes.
	after []godap.EventMessage
}

func newView() *view {
	return &view{
		lists: make(map[bpsKey][]int), reported: make(map[int]godap.Breakpoint),
		files: make(map[string]string), latest: make(map[string]string),
		mirrors: make(map[int]*mirror), hidden: make(map[int]bool),
	}
}

// fileLine is a line of a file (a breakpoint file key).
type fileLine struct {
	file string
	line int
}

// track records the breakpoints a request placed under key (got is nil
// when it placed none: the session had exited), and what its response
// says of them, and returns them as DAP breakpoints (never nil). An id
// another list still has (the file's other path) stays reported.
func (v *view) track(key bpsKey, got []api.Breakpoint) []godap.Breakpoint {
	out := make([]godap.Breakpoint, len(got))
	ids := make([]int, 0, len(got))

	for i := range got {
		out[i] = dapBreakpoint(&got[i])
		if got[i].ID != 0 {
			ids = append(ids, got[i].ID)
			v.reported[got[i].ID] = out[i]
		}
	}

	if got == nil {
		return out
	}

	old := v.lists[key]
	v.lists[key] = ids

	if len(old) == 0 {
		return out
	}

	listed := map[int]bool{}
	for _, l := range v.lists {
		for _, id := range l {
			listed[id] = true
		}
	}

	for _, id := range old {
		if !listed[id] {
			delete(v.reported, id)
		}
	}

	return out
}

// sourceSeen records that the connection's setBreakpoints named path, of
// file key, and whether it set a breakpoint of the client's own there.
func (v *view) sourceSeen(path, key string, own bool) {
	v.files[path] = key
	if own {
		v.latest[key] = path
	}
}

// keep returns the ids the connection's lists hold under the other paths
// of file key than path: a list under path mustn't remove them.
func (v *view) keep(key, path string) []int {
	var out []int

	for p, k := range v.files {
		if k == key && p != path {
			out = append(out, v.lists[bpsKey{path: p}]...)
		}
	}

	return out
}

// announcePath is the path new mirrors of file key are announced under.
func (v *view) announcePath(key string) string {
	if p, ok := v.latest[key]; ok {
		return p
	}

	return key
}

// forget drops a reported id that no longer exists.
func (v *view) forget(id int) {
	delete(v.reported, id)

	for key, ids := range v.lists {
		v.lists[key] = slices.DeleteFunc(ids, func(i int) bool { return i == id })
	}
}

// mirrorable reports whether b may be mirrored to a connection of client
// self that holds v: a permanent line breakpoint the connection didn't
// report, and not one of self's editor breakpoints (another connection of
// self has those).
func mirrorable(b *api.Breakpoint, self string, v *view) bool {
	_, reported := v.reported[b.ID]

	return b.ID > 0 && b.Function == "" && b.File != "" && !b.Temporary && !reported && (b.Owner != self || !b.Editor)
}

// wanted returns the mirrors the connection should show now, by id: per
// file and placed line the lowest-id mirrorable breakpoint, unless the
// connection has one of its own there (at its placed or requested line).
// A hidden breakpoint isn't shown, but still takes its line.
func wanted(self string, bps []api.Breakpoint, v *view) map[int]godap.Breakpoint {
	own := map[fileLine]bool{}

	for i := range bps {
		if _, ok := v.reported[bps[i].ID]; ok && bps[i].Function == "" {
			own[fileLine{bps[i].File, bps[i].Line}] = true
			own[fileLine{bps[i].File, bps[i].RequestedLine}] = true
		}
	}

	out := map[int]godap.Breakpoint{}
	taken := map[fileLine]bool{}

	for i := range bps {
		b := &bps[i]
		at := fileLine{b.File, b.Line}

		if !mirrorable(b, self, v) || own[at] || taken[at] {
			continue
		}

		taken[at] = true

		if v.hidden[b.ID] {
			continue
		}

		path := v.announcePath(b.File)
		if m, ok := v.mirrors[b.ID]; ok {
			path = m.path
		}

		out[b.ID] = mirrorBreakpoint(b, self, path)
	}

	return out
}

// mirrorBreakpoint is b as a mirror under source path: at its placed
// line, column 1, with a message that says whose it is and what it does
// (the editor's copy loses its condition, hit count and log message).
func mirrorBreakpoint(b *api.Breakpoint, self, path string) godap.Breakpoint {
	return godap.Breakpoint{
		Id: b.ID, Verified: b.Verified, Line: b.Line, Column: markColumn,
		Source: &godap.Source{Path: path}, Message: mirrorMessage(b, self),
	}
}

// mirrorMessage is a mirror's message, at most maxMirrorMessage
// characters, always ending with the hint that removing it only hides it.
func mirrorMessage(b *api.Breakpoint, self string) string {
	parts := []string{b.Owner + "'s breakpoint"}
	if b.Owner == self {
		parts[0] = "your breakpoint, set outside this editor"
	}

	if b.Condition != "" {
		parts = append(parts, "if "+b.Condition)
	}

	if b.HitCondition != "" {
		parts = append(parts, "hit "+b.HitCondition)
	}

	if b.LogMessage != "" {
		parts = append(parts, "logs "+b.LogMessage+", doesn't stop")
	}

	for _, s := range []string{b.Message, b.Note} {
		if s != "" {
			parts = append(parts, s)
		}
	}

	body := strings.Join(parts, " · ")
	if limit := maxMirrorMessage - utf8.RuneCountInString(hideHint); utf8.RuneCountInString(body) > limit {
		body = string([]rune(body)[:limit-1]) + "…"
	}

	return body + hideHint
}

// sameBreakpoint reports whether a and b tell an editor the same.
func sameBreakpoint(a, b godap.Breakpoint) bool {
	path := func(bp godap.Breakpoint) string {
		if bp.Source == nil {
			return ""
		}

		return bp.Source.Path
	}

	as, bs := a, b
	as.Source, bs.Source = nil, nil

	return as == bs && path(a) == path(b)
}

// breakpointEvent is a DAP breakpoint event.
func breakpointEvent(reason string, b godap.Breakpoint) *godap.BreakpointEvent {
	return &godap.BreakpointEvent{Event: event("breakpoint"), Body: godap.BreakpointEventBody{Reason: reason, Breakpoint: b}}
}

// removedEvent is a breakpoint removed event for id.
func removedEvent(id int) *godap.BreakpointEvent {
	return breakpointEvent(actionRemoved, godap.Breakpoint{Id: id})
}

// reconcile brings the editor up to date with bps (every client's
// breakpoints now, by id) and returns the events that do it, updating v:
// removed for reported breakpoints that are gone and for mirrors no longer
// wanted, changed for reported breakpoints and mirrors whose state changed,
// new for wanted mirrors not announced yet — removed first, then changed,
// then new, each by id. Hidden ids whose breakpoint is gone are forgotten.
func reconcile(v *view, self string, bps []api.Breakpoint) []godap.EventMessage {
	byID := make(map[int]*api.Breakpoint, len(bps))
	for i := range bps {
		byID[bps[i].ID] = &bps[i]
	}

	removed, changed := reconcileReported(v, byID)
	want := wanted(self, bps, v)

	for id, m := range v.mirrors {
		w, ok := want[id]
		if !ok {
			removed = append(removed, godap.Breakpoint{Id: id})
			delete(v.mirrors, id)

			continue
		}

		if !sameBreakpoint(w, m.told) {
			changed = append(changed, w)
			m.told = w
		}
	}

	var added []godap.Breakpoint

	for id, w := range want {
		if _, ok := v.mirrors[id]; !ok {
			added = append(added, w)
			v.mirrors[id] = &mirror{key: byID[id].File, path: w.Source.Path, line: w.Line, told: w}
		}
	}

	for id := range v.hidden {
		if _, ok := byID[id]; !ok {
			delete(v.hidden, id)
		}
	}

	out := make([]godap.EventMessage, 0, len(removed)+len(changed)+len(added))

	for _, group := range []struct {
		reason string
		bps    []godap.Breakpoint
	}{{actionRemoved, removed}, {actionChanged, changed}, {actionNew, added}} {
		slices.SortFunc(group.bps, func(a, b godap.Breakpoint) int { return a.Id - b.Id })

		for _, b := range group.bps {
			out = append(out, breakpointEvent(group.reason, b))
		}
	}

	return out
}

// reconcileReported updates what v reported with the breakpoints byID and
// returns the ones gone and the ones changed.
func reconcileReported(v *view, byID map[int]*api.Breakpoint) (removed, changed []godap.Breakpoint) {
	for id, told := range v.reported {
		b, ok := byID[id]
		if !ok {
			removed = append(removed, godap.Breakpoint{Id: id})
			v.forget(id)

			continue
		}

		if now := dapBreakpoint(b); now != told {
			changed = append(changed, now)
			v.reported[id] = now
		}
	}

	return removed, changed
}

// retractMirrors returns a removed event for every mirror and forgets them
// (the connection is leaving, or the session ended).
func retractMirrors(v *view) []godap.EventMessage {
	ids := make([]int, 0, len(v.mirrors))
	for id := range v.mirrors {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	out := make([]godap.EventMessage, 0, len(ids))
	for _, id := range ids {
		out = append(out, removedEvent(id))
		delete(v.mirrors, id)
	}

	return out
}

// entryKind is how classify takes one entry of a setBreakpoints request.
type entryKind int

const (
	// entryOwn: the client's own breakpoint, placed by the session.
	entryOwn entryKind = iota
	// entryEcho: the editor's copy of a mirror; answered with the mirror.
	entryEcho
	// entryRetract: a copy of a breakpoint no longer there; answered with a
	// negative id, then removed.
	entryRetract
	// entryDuplicate: a second entry on an echo's line; refused.
	entryDuplicate
)

// entryClass is classify's verdict on one entry: for an echo, the mirror's
// id and state.
type entryClass struct {
	kind entryKind
	id   int
	told godap.Breakpoint
}

// plain reports whether e has no condition, hit condition or log message;
// marked, whether it is plain at the mark column (an eyedbg copy).
func plain(e *godap.SourceBreakpoint) bool {
	return e.Condition == "" && e.HitCondition == "" && e.LogMessage == ""
}

func marked(e *godap.SourceBreakpoint) bool { return plain(e) && e.Column == markColumn }

// classify takes each entry of a setBreakpoints request for source path
// path, of the file with key key, line by line: a line with only marked
// entries is an echo of the line's candidate (then retracts); a line with
// only plain unmarked entries where v shows a mirror under path is an echo
// too (clients that drop the column), then duplicates; any other line is
// the client's own, its marked entries retracted. An empty key (no such
// file) makes every entry own. Echoes are answered under path.
func classify(entries []godap.SourceBreakpoint, key, path string, v *view, bps []api.Breakpoint, self string) []entryClass {
	classes := make([]entryClass, len(entries))
	if key == "" {
		return classes
	}

	var lines []int

	byLine := map[int][]int{}

	for i := range entries {
		l := entries[i].Line
		if _, seen := byLine[l]; !seen {
			lines = append(lines, l)
		}

		byLine[l] = append(byLine[l], i)
	}

	f := indexFile(key, path, v, bps, self)

	for _, l := range lines {
		f.classifyLine(entries, byLine[l], l, classes)
	}

	return classes
}

// fileIndex is what classify looks up per line of one file, built once
// per request so that it costs O(entries + breakpoints + mirrors): v's
// mirrors of the file by announced line (lowest id; here: those under the
// request's path), and the file's breakpoints by id and by placed and
// requested line (in id order). used are the ids echoed already.
type fileIndex struct {
	path, self string
	v          *view
	shown      map[int]int
	here       map[int]int
	byID       map[int]*api.Breakpoint
	at         map[int][]*api.Breakpoint
	used       map[int]bool
}

func indexFile(key, path string, v *view, bps []api.Breakpoint, self string) *fileIndex {
	f := &fileIndex{
		path: path, self: self, v: v, shown: map[int]int{}, here: map[int]int{},
		byID: map[int]*api.Breakpoint{}, at: map[int][]*api.Breakpoint{}, used: map[int]bool{},
	}

	lowest := func(m map[int]int, line, id int) {
		if cur, ok := m[line]; !ok || id < cur {
			m[line] = id
		}
	}

	for id, m := range v.mirrors {
		if m.key != key {
			continue
		}

		lowest(f.shown, m.line, id)

		if m.path == path {
			lowest(f.here, m.line, id)
		}
	}

	for i := range bps {
		b := &bps[i]
		if b.File != key {
			continue
		}

		f.byID[b.ID] = b
		f.at[b.Line] = append(f.at[b.Line], b)

		if b.RequestedLine != b.Line {
			f.at[b.RequestedLine] = append(f.at[b.RequestedLine], b)
		}
	}

	return f
}

// classifyLine classifies the entries idx, all at line l (see classify).
func (f *fileIndex) classifyLine(entries []godap.SourceBreakpoint, idx []int, l int, classes []entryClass) {
	allMarked, allUnmarked := true, true

	for _, i := range idx {
		allMarked = allMarked && marked(&entries[i])
		allUnmarked = allUnmarked && plain(&entries[i]) && !marked(&entries[i])
	}

	shown := f.here[l]

	switch {
	case allMarked:
		classes[idx[0]] = echoOf(f.candidate(l), nil, f.used)
		for _, i := range idx[1:] {
			classes[i] = entryClass{kind: entryRetract}
		}
	case allUnmarked && shown != 0:
		classes[idx[0]] = echoOf(f.candidate(l), f.v.mirrors[shown], f.used)

		for _, i := range idx[1:] {
			classes[i] = entryClass{kind: entryDuplicate}
		}
	default:
		for _, i := range idx {
			if marked(&entries[i]) {
				classes[i] = entryClass{kind: entryRetract}
			}
		}
	}
}

// echoOf is an echo of b (or, without b, of the mirror fallback), or a
// retract without either; it marks the id used.
func echoOf(b *godap.Breakpoint, fallback *mirror, used map[int]bool) entryClass {
	switch {
	case b != nil:
		used[b.Id] = true

		return entryClass{kind: entryEcho, id: b.Id, told: *b}
	case fallback != nil && !used[fallback.told.Id]:
		used[fallback.told.Id] = true

		return entryClass{kind: entryEcho, id: fallback.told.Id, told: fallback.told}
	default:
		return entryClass{kind: entryRetract}
	}
}

// candidate returns, as a mirror under the request's path, the breakpoint
// an editor's copy at line l stands for: the mirror v announced there
// (under the request's path first) if its breakpoint is still there, else
// the lowest-id mirrorable breakpoint placed or requested at l (hidden
// ones too); nil if none. Ids echoed already are skipped.
func (f *fileIndex) candidate(l int) *godap.Breakpoint {
	var best *api.Breakpoint

	for _, id := range []int{f.here[l], f.shown[l]} {
		if b := f.byID[id]; best == nil && id != 0 && b != nil && !f.used[id] {
			best = b
		}
	}

	for _, b := range f.at[l] {
		if best == nil && mirrorable(b, f.self, f.v) && !f.used[b.ID] {
			best = b
		}
	}

	if best == nil {
		return nil
	}

	m := mirrorBreakpoint(best, f.self, f.path)

	return &m
}

// settle updates v after a setBreakpoints request (file key, source path
// as sent): echoes become (or stay) mirrors under path at the line the
// editor sent; v's other mirrors under path leave v — silently where the
// request has an entry at their line (the user took the line over), else
// hidden (the user removed or disabled the copy). The file's mirrors under
// its other paths stay: this list doesn't hold them.
func settle(v *view, key, path string, entries []godap.SourceBreakpoint, classes []entryClass) {
	if key == "" {
		return
	}

	lines := map[int]bool{}
	echoed := map[int]bool{}

	for i := range entries {
		lines[entries[i].Line] = true

		if c := classes[i]; c.kind == entryEcho {
			echoed[c.id] = true
			v.mirrors[c.id] = &mirror{key: key, path: path, line: entries[i].Line, told: c.told}
			delete(v.hidden, c.id)
		}
	}

	for id, m := range v.mirrors {
		if m.key != key || m.path != path || echoed[id] {
			continue
		}

		delete(v.mirrors, id)

		if !lines[m.line] {
			v.hidden[id] = true
		}
	}
}

// retracted is the answer to a retracted entry at line l: id is negative.
func retracted(id, l int) godap.Breakpoint {
	return godap.Breakpoint{Id: id, Line: l, Message: retractMessage}
}

// nextRetractID returns a fresh negative id for a retracted copy.
func (v *view) nextRetractID() int {
	v.lastRetract--

	return v.lastRetract
}
