// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// AddBreakpoint adds client c's breakpoint (at a line, an anchor's line or
// a function) and, once the session is configured, sends it to the
// adapter. The result says whether the adapter verified it and on which
// line it actually landed. Adding one where c already has one replaces its
// condition; other clients' breakpoints at the same place are separate and
// share it (see slotsFor).
func (s *Session) AddBreakpoint(ctx context.Context, c api.Client, spec api.BreakpointSpec) (api.Breakpoint, error) {
	spec, err := resolveSpec(spec, true)
	if err != nil {
		return api.Breakpoint{}, err
	}

	s.mu.Lock()
	if s.state == api.StateExited {
		s.mu.Unlock()

		return api.Breakpoint{}, stateError(s.ID, s.state, "adding a breakpoint needs a live session")
	}

	if err := s.checkBreakpointCapsLocked(spec); err != nil {
		s.mu.Unlock()

		return api.Breakpoint{}, err
	}

	b, isNew, changed := s.addBreakpointLocked(c.ID, spec)
	s.mu.Unlock()

	if err := s.syncKey(ctx, spec.File); err != nil {
		return api.Breakpoint{}, err
	}

	s.mu.Lock()

	switch {
	case isNew:
		s.logBreakpointLocked("added", c.ID, b)
	case changed:
		s.logBreakpointLocked("changed", c.ID, b)
	}

	out := b.Breakpoint
	s.mu.Unlock()

	return s.annotate([]api.Breakpoint{out})[0], nil
}

// resolveSpec checks a breakpoint spec by its kind and resolves an anchor
// (Anchor set, Line 0) to its line. Function breakpoints are refused
// unless allowFunction.
func resolveSpec(spec api.BreakpointSpec, allowFunction bool) (api.BreakpointSpec, error) {
	if spec.Function != "" {
		switch {
		case !allowFunction:
			return spec, api.NewError(api.CodeInvalidRequest, "run-until needs a line, not a function",
				`use FILE:LINE or FILE@"TEXT"`)
		case spec.File != "" || spec.Line != 0 || spec.Anchor != "":
			return spec, api.NewError(api.CodeInvalidRequest, "a function breakpoint has no file, line or anchor", "use func:NAME alone")
		case spec.HitCondition != "" || spec.LogMessage != "":
			return spec, api.NewError(api.CodeInvalidRequest, "--hit and --log work on line breakpoints only",
				"put a FILE:LINE breakpoint on the function's first line instead")
		}

		return spec, nil
	}

	if !filepath.IsAbs(spec.File) {
		return spec, api.NewError(api.CodeInvalidRequest, "invalid breakpoint file "+spec.File+": want an absolute path",
			`use FILE:LINE, FILE@"TEXT" or func:NAME`)
	}

	// Adapters match breakpoints to the debug symbols' source paths, which
	// have symlinks resolved (on macOS /tmp and /var are symlinks into
	// /private): a breakpoint through a symlink would never bind.
	spec.File = realPath(spec.File)

	if err := checkSourceFile(spec.File); err != nil {
		return spec, err
	}

	if spec.Anchor != "" && spec.Line == 0 {
		line, err := resolveAnchor(spec.File, spec.Anchor)
		if err != nil {
			return spec, err
		}

		spec.Line = line
	}

	if spec.Line < 1 {
		return spec, api.NewError(api.CodeInvalidRequest,
			fmt.Sprintf("invalid breakpoint %s:%d", spec.File, spec.Line), "use FILE:LINE with a line number from 1")
	}

	if _, _, err := parseEmulated(spec); err != nil {
		return spec, err
	}

	return spec, nil
}

// checkSourceFile refuses a breakpoint file that isn't there: the adapter
// would leave its breakpoint pending for good.
func checkSourceFile(path string) error {
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return api.NewError(api.CodeInvalidRequest, "no source file "+path,
			"FILE is a path from the working directory, e.g. src/App/Program.cs:20, not just the file name")
	}

	return nil
}

// parseEmulated parses spec's hit condition and log message (nil when
// unset).
func parseEmulated(spec api.BreakpointSpec) (*hitCondition, []logPart, error) {
	var (
		hit  *hitCondition
		logs []logPart
		err  error
	)

	if spec.HitCondition != "" {
		if hit, err = parseHit(spec.HitCondition); err != nil {
			return nil, nil, err
		}
	}

	if spec.LogMessage != "" {
		if logs, err = parseLog(spec.LogMessage); err != nil {
			return nil, nil, err
		}
	}

	return hit, logs, nil
}

// annotate adds a note to line breakpoints whose file changed after the
// session started (see staleNote); it reads each file's time once.
func (s *Session) annotate(bps []api.Breakpoint) []api.Breakpoint {
	notes := map[string]string{}

	for i := range bps {
		b := &bps[i]
		if b.File == "" {
			continue
		}

		note, ok := notes[b.File]
		if !ok {
			note = staleNote(b.File, s.CreatedAt)
			notes[b.File] = note
		}

		switch {
		case note == "":
		case b.Note == "":
			b.Note = note
		default:
			b.Note += "; " + note
		}
	}

	return bps
}

// RemoveBreakpoint removes breakpoint id for client c. Another client's
// breakpoint needs force (NOT_OWNER otherwise). id 0 removes all of c's
// breakpoints, or everyone's with force, and never fails for finding none.
// It returns how many were removed and, for id 0, how many other clients'
// breakpoints were kept.
func (s *Session) RemoveBreakpoint(ctx context.Context, c api.Client, id int, force bool) (removed, kept int, err error) {
	if id == 0 {
		return s.removeBreakpoints(ctx, c.ID, func(b *breakpoint) bool { return force || b.Owner == c.ID })
	}

	s.mu.Lock()
	target := s.findBreakpointLocked(id)
	s.mu.Unlock()

	if target == nil {
		return 0, 0, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("no breakpoint %d", id), "see 'eyedbg bp ls'")
	}

	if target.Owner != c.ID && !force {
		return 0, 0, api.NewError(api.CodeNotOwner, fmt.Sprintf("breakpoint %d belongs to %s", id, target.Owner),
			"'eyedbg bp ls --mine' lists yours; add --force to remove it anyway")
	}

	removed, _, err = s.removeBreakpoints(ctx, c.ID, func(b *breakpoint) bool { return b == target })

	return removed, 0, err
}

func (s *Session) findBreakpointLocked(id int) *breakpoint {
	for _, list := range s.bps {
		for _, b := range list {
			if b.ID == id {
				return b
			}
		}
	}

	return nil
}

// removeBreakpoints removes the breakpoints match selects, sends the files
// they were in to the adapter (unless the session has exited) and logs a
// removed event per breakpoint, by actor. It returns how many it removed
// and how many breakpoints are left.
func (s *Session) removeBreakpoints(ctx context.Context, actor string, match func(*breakpoint) bool) (removed, left int, err error) {
	s.mu.Lock()

	var gone []*breakpoint

	touched := map[string]bool{}

	for file, list := range s.bps {
		kept := make([]*breakpoint, 0, len(list))

		for _, b := range list {
			if match(b) {
				gone = append(gone, b)
				touched[file] = true

				continue
			}

			kept = append(kept, b)
		}

		left += len(kept)

		if len(kept) == 0 {
			delete(s.bps, file)
		} else {
			s.bps[file] = kept
		}
	}

	exited := s.state == api.StateExited
	s.mu.Unlock()

	if !exited {
		for key := range touched {
			if err = s.syncKey(ctx, key); err != nil {
				break
			}
		}
	}

	sort.Slice(gone, func(i, j int) bool { return gone[i].ID < gone[j].ID })

	s.mu.Lock()
	for _, b := range gone {
		s.logBreakpointLocked("removed", actor, b)
	}
	s.mu.Unlock()

	return len(gone), left, err
}

// addBreakpointLocked adds owner's breakpoint, or updates the one owner
// already has at that line or function (making it permanent). changed
// reports whether an existing one changed. Function breakpoints are kept
// under funcKey (their File is empty).
func (s *Session) addBreakpointLocked(owner string, spec api.BreakpointSpec) (b *breakpoint, isNew, changed bool) {
	key := slotKey{function: spec.Function, line: spec.Line}

	for _, b := range s.bps[spec.File] {
		if b.Owner == owner && b.key() == key {
			// An editor breakpoint stays one (Editor is left alone): the
			// editor still shows it, and its client's leaving removes it.
			changed = b.Condition != spec.Condition || b.Temporary || b.Anchor != spec.Anchor ||
				b.HitCondition != spec.HitCondition || b.LogMessage != spec.LogMessage
			b.Condition, b.Temporary, b.Anchor = spec.Condition, false, spec.Anchor
			b.setEmulated(spec)

			return b, false, changed
		}
	}

	return s.addNewLocked(owner, spec), true, false
}

// addNewLocked adds a new breakpoint of owner's, without looking for one
// to update.
func (s *Session) addNewLocked(owner string, spec api.BreakpointSpec) *breakpoint {
	s.nextBP++
	b := &breakpoint{Breakpoint: api.Breakpoint{
		ID: s.nextBP, File: spec.File, RequestedLine: spec.Line, Line: spec.Line, Condition: spec.Condition,
		Function: spec.Function, Anchor: spec.Anchor, Owner: owner, CreatedAt: time.Now(),
	}}
	b.setEmulated(spec)
	s.bps[spec.File] = append(s.bps[spec.File], b)

	return b
}

// setEmulated sets b's hit condition and log message from spec (checked
// already) and restarts its hit count.
func (b *breakpoint) setEmulated(spec api.BreakpointSpec) {
	b.HitCondition, b.LogMessage, b.Hits = spec.HitCondition, spec.LogMessage, 0
	b.hit, b.log, _ = parseEmulated(spec)
}

// logBreakpointLocked logs a breakpoint event with a copy of b as it is now.
func (s *Session) logBreakpointLocked(action, actor string, b *breakpoint) {
	cp := b.Breakpoint
	s.log.append(api.Event{Kind: api.EventBreakpoint, Action: action, Client: actor, Breakpoint: &cp})
}

// syncBreakpoints sends file's breakpoints to the adapter (DAP replaces per
// file), one source breakpoint per line, and records what the adapter
// answered on every breakpoint of that line. Round trips are serialized by
// syncMu, so the list the adapter keeps is always the newest one. Before the
// adapter is initialized there is nothing to do: configure sends them.
func (s *Session) syncBreakpoints(ctx context.Context, file string) error {
	select {
	case <-s.initialized:
	default:
		return nil
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	s.mu.Lock()
	slots := slotsFor(s.bps[file])
	s.mu.Unlock()

	sbps := make([]godap.SourceBreakpoint, len(slots))
	for i, sl := range slots {
		sbps[i] = godap.SourceBreakpoint{Line: sl.line, Condition: sl.condition}
	}

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := dap.Call[*godap.SetBreakpointsResponse](ctx, s.client, &setBreakpointsRequest{
		Request:   godap.Request{Command: "setBreakpoints"},
		Arguments: setBreakpointsArguments{Source: godap.Source{Path: file, Name: filepath.Base(file)}, Breakpoints: sbps},
	})
	if err != nil {
		return adapterErr(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.applySlotsLocked(slots, resp.Body.Breakpoints)

	return nil
}

// updateBreakpointLocked applies an adapter's breakpoint event (e.g. a
// pending breakpoint that resolved once the module loaded) to every
// breakpoint of that adapter id, and logs a changed event for each.
func (s *Session) updateBreakpointLocked(got godap.Breakpoint) {
	if got.Id == 0 {
		return
	}

	var hit []*breakpoint

	for _, list := range s.bps {
		for _, b := range list {
			if b.adapterID != got.Id {
				continue
			}

			b.Verified = got.Verified
			b.Message = got.Message

			if got.Line > 0 && b.Function == "" {
				b.Line = got.Line
			}

			if got.Source != nil && got.Source.Path != "" {
				b.source = got.Source.Path
			}

			hit = append(hit, b)
		}
	}

	sort.Slice(hit, func(i, j int) bool { return hit[i].ID < hit[j].ID })

	for _, b := range hit {
		s.logBreakpointLocked("changed", "", b)
	}
}

// setBreakpointsRequest is godap.SetBreakpointsRequest without omitempty on
// breakpoints: clearing a file's breakpoints must send "breakpoints": [],
// and some adapters (netcoredbg) reject a request without the key.
type setBreakpointsRequest struct {
	godap.Request

	Arguments setBreakpointsArguments `json:"arguments"`
}

type setBreakpointsArguments struct {
	Source      godap.Source             `json:"source"`
	Breakpoints []godap.SourceBreakpoint `json:"breakpoints"`
}

// RunUntil continues to spec's location for client c, via a temporary
// breakpoint of c's unless c already has one there. The program may stop
// elsewhere first (another breakpoint, an exception) or exit. The temporary
// breakpoint is removed when the program stops or exits; after a timeout it
// stays until the next execution command.
func (s *Session) RunUntil(ctx context.Context, c api.Client, spec api.BreakpointSpec, threadID int, wait time.Duration, dump api.DumpSpec) (api.Snapshot, error) {
	spec, err := resolveSpec(spec, false)
	if err != nil {
		return api.Snapshot{}, err
	}

	spec.HitCondition, spec.LogMessage = "", ""

	s.mu.Lock()
	err = s.checkBreakpointCapsLocked(spec)
	s.mu.Unlock()

	if err != nil {
		return api.Snapshot{}, err
	}

	x, err := s.execute(ctx, execRequest{client: c, kind: ExecRunUntil, thread: threadID, target: &spec})
	if err != nil {
		return api.Snapshot{}, err
	}

	snap := s.Wait(ctx, x.before, wait, dump)
	snap.Target = &x.target

	if snap.TimedOut {
		return snap, nil
	}

	// At the target, but stopped for other breakpoints there only (their
	// condition held, the run-until's didn't): not reached.
	reached := snap.Frame != nil && snap.Frame.Line == x.target.Line && sameFile(snap.Frame.File, x.target.File)
	if st := snap.Session.Stop; reached && st != nil && len(st.Breakpoints) > 0 {
		reached = slices.ContainsFunc(st.Breakpoints, func(b api.StopBreakpoint) bool { return b.ID == x.targetID })
	}

	snap.Reached = &reached

	// Only this request's own breakpoint: another client may have started
	// a run-until of its own meanwhile.
	if x.temp != nil {
		if _, _, err := s.removeBreakpoints(ctx, c.ID, stillTemporary(x.temp)); err != nil &&
			snap.Session.State != api.StateExited {
			return snap, err
		}
	}

	return snap, nil
}

// placeTarget places run-until's breakpoint at spec: c's own breakpoint at
// that line if it has one that stops (no hit condition or log message),
// else a new temporary one of c's (returned as temp). It returns where the
// adapter placed it, and its id.
func (s *Session) placeTarget(ctx context.Context, c api.Client, spec api.BreakpointSpec) (at api.BreakpointSpec, id int, temp *breakpoint, err error) {
	s.mu.Lock()

	var target *breakpoint

	for _, b := range s.bps[spec.File] {
		if b.Owner == c.ID && b.key() == (slotKey{line: spec.Line}) && !b.emulated() {
			target = b
		}
	}

	if target == nil {
		target = s.addNewLocked(c.ID, spec)
		target.Temporary = true
		temp = target
	}
	s.mu.Unlock()

	if temp != nil {
		if err := s.syncBreakpoints(ctx, spec.File); err != nil {
			_, _, _ = s.removeBreakpoints(ctx, c.ID, stillTemporary(temp))

			return api.BreakpointSpec{}, 0, nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if temp != nil {
		s.logBreakpointLocked("added", c.ID, temp)
	}

	return api.BreakpointSpec{File: target.File, Line: target.Line, Condition: target.Condition}, target.ID, temp, nil
}

// stillTemporary matches temp while it is still temporary: a bp add by its
// owner at that line meanwhile makes it permanent, and then it stays.
// removeBreakpoints calls the matcher under mu, which guards Temporary.
func stillTemporary(temp *breakpoint) func(*breakpoint) bool {
	return func(b *breakpoint) bool { return b == temp && b.Temporary }
}

// removeTemporary removes every client's run-until breakpoints: they belong
// to the execution that placed them, which a new one supersedes.
func (s *Session) removeTemporary(ctx context.Context, actor string) error {
	_, _, err := s.removeBreakpoints(ctx, actor, func(b *breakpoint) bool { return b.Temporary })

	return err
}
