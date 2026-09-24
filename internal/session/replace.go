// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// replaceItem is one breakpoint of a replace request: its spec, why it was
// refused (nil if not), and the breakpoint that serves it.
type replaceItem struct {
	spec    api.BreakpointSpec
	refused error
	b       *breakpoint
	isNew   bool
	changed bool
}

// ReplaceBreakpoints makes client c's editor breakpoints in file exactly
// specs (DAP setBreakpoints): c's existing permanent breakpoints there are
// matched to specs by requested line, then by the line the adapter placed
// them on (an editor re-sends a moved breakpoint there); a match keeps its
// id (and its hit count unless it changed) and, like every new one, becomes
// an editor breakpoint (api.Breakpoint.Editor); c's unmatched editor
// breakpoints are removed and new ones added, in one adapter round trip.
// c's other breakpoints (set with the CLI), temporary (run-until) ones and
// other clients' are never removed (docs/adr/0014), nor are c's editor
// breakpoints whose ids are in keep: the ones its editor lists under
// another path of the same file (a symlink), which a list under this path
// doesn't show. An id in keep that isn't one of c's editor breakpoints in
// the file has no effect.
//
// It returns one breakpoint per spec, in order. A spec that can't be
// placed — no such file, an invalid hit condition or log message, a
// condition the adapter can't evaluate, a second one on a line — comes back
// with ID 0, unverified, and why in Message. The request fails as a whole
// only for an exited session or an adapter error; after an adapter error
// the model keeps the change (as [Session.AddBreakpoint] does), and the
// results are returned with the error.
func (s *Session) ReplaceBreakpoints(ctx context.Context, c api.Client, file string, specs []api.BreakpointSpec, keep []int) ([]api.Breakpoint, error) {
	key, keyErr := FileKey(file)
	items := make([]replaceItem, len(specs))

	for i, spec := range specs {
		spec.File, spec.Function, spec.Anchor = file, "", ""
		items[i].spec = spec

		if keyErr != nil {
			items[i].refused = keyErr

			continue
		}

		items[i].spec, items[i].refused = resolveSpec(spec, false)
	}

	if keyErr != nil {
		return s.replaceResults(items), s.requireLive("setting breakpoints needs a live session")
	}

	return s.replace(ctx, c, key, items, keep)
}

// ReplaceFunctionBreakpoints makes client c's function breakpoints exactly
// specs (DAP setFunctionBreakpoints), matched by function name, with the
// rules of [Session.ReplaceBreakpoints]. Hit conditions are refused per
// breakpoint: they work on line breakpoints only.
func (s *Session) ReplaceFunctionBreakpoints(ctx context.Context, c api.Client, specs []api.BreakpointSpec) ([]api.Breakpoint, error) {
	items := make([]replaceItem, len(specs))

	for i, spec := range specs {
		spec.File, spec.Line, spec.Anchor, spec.LogMessage = "", 0, "", ""
		items[i].spec = spec

		switch {
		case spec.Function == "":
			items[i].refused = api.NewError(api.CodeInvalidRequest, "a function breakpoint needs a function name", "")
		case spec.HitCondition != "":
			items[i].refused = api.NewError(api.CodeInvalidRequest, "hit counts work on line breakpoints only", "")
		default:
			items[i].spec, items[i].refused = resolveSpec(spec, true)
		}
	}

	return s.replace(ctx, c, funcKey, items, nil)
}

// FileKey returns the key of file's breakpoints (api.Breakpoint.File): its
// path with symlinks resolved, as a breakpoint's file is. For a file that
// no longer exists, its directory's resolved path plus its name, so the
// breakpoints of a deleted file can still be cleared. The file must be an
// absolute path (INVALID_REQUEST otherwise).
func FileKey(file string) (string, error) {
	if file == "" {
		return "", api.NewError(api.CodeInvalidRequest, "breakpoints need a file on disk", "")
	}

	if !filepath.IsAbs(file) {
		return "", api.NewError(api.CodeInvalidRequest, "invalid breakpoint file "+file+": want an absolute path", "")
	}

	if resolved, err := filepath.EvalSymlinks(file); err == nil {
		return resolved, nil
	}

	return filepath.Join(realPath(filepath.Dir(file)), filepath.Base(file)), nil
}

// requireLive returns SESSION_EXITED (for what) once the session exited.
func (s *Session) requireLive(what string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state == api.StateExited {
		return stateError(s.ID, s.state, what)
	}

	return nil
}

// replace applies items to c's breakpoints under key (keeping the ids in
// keep), syncs key with the adapter once and logs what changed.
func (s *Session) replace(ctx context.Context, c api.Client, key string, items []replaceItem, keep []int) ([]api.Breakpoint, error) {
	s.mu.Lock()
	if s.state == api.StateExited {
		s.mu.Unlock()

		return nil, stateError(s.ID, s.state, "setting breakpoints needs a live session")
	}

	s.refuseLocked(items)
	gone := s.matchLocked(c.ID, key, items, keep)
	s.mu.Unlock()

	err := s.syncKey(ctx, key)

	s.mu.Lock()
	for _, b := range gone {
		s.logBreakpointLocked("removed", c.ID, b)
	}

	for i := range items {
		switch it := &items[i]; {
		case it.b == nil:
		case it.isNew:
			s.logBreakpointLocked("added", c.ID, it.b)
		case it.changed:
			s.logBreakpointLocked("changed", c.ID, it.b)
		}
	}
	s.mu.Unlock()

	return s.replaceResults(items), err
}

// refuseLocked refuses the items the adapter can't place, and every item
// after the first at the same line or function (one per client).
func (s *Session) refuseLocked(items []replaceItem) {
	seen := map[slotKey]bool{}

	for i := range items {
		it := &items[i]
		if it.refused != nil {
			continue
		}

		if err := s.checkBreakpointCapsLocked(it.spec); err != nil {
			it.refused = err

			continue
		}

		key := slotKey{function: it.spec.Function, line: it.spec.Line}
		if seen[key] {
			it.refused = api.NewError(api.CodeInvalidRequest,
				"another breakpoint of this request is on the same line: eyedbg keeps one per line per client (columns are ignored)", "")

			continue
		}

		seen[key] = true
	}
}

// matchLocked matches the accepted items to owner's permanent breakpoints
// under key, updates the matches, adds the unmatched items, marks both as
// editor breakpoints and removes owner's unmatched editor breakpoints not
// in keep, which it returns sorted by id. owner's unmatched other
// breakpoints stay.
func (s *Session) matchLocked(owner, key string, items []replaceItem, keep []int) []*breakpoint {
	var old []*breakpoint

	for _, b := range s.bps[key] {
		if b.Owner == owner && !b.Temporary {
			old = append(old, b)
		}
	}

	kept := make(map[int]bool, len(keep))
	for _, id := range keep {
		kept[id] = true
	}

	gone := slices.DeleteFunc(matchItems(old, items), func(b *breakpoint) bool { return !b.Editor || kept[b.ID] })
	if len(gone) > 0 {
		kept := slices.DeleteFunc(slices.Clone(s.bps[key]), func(b *breakpoint) bool { return slices.Contains(gone, b) })
		if len(kept) == 0 {
			delete(s.bps, key)
		} else {
			s.bps[key] = kept
		}
	}

	for i := range items {
		it := &items[i]
		if it.refused != nil {
			continue
		}

		if it.b == nil {
			it.b, it.isNew = s.addNewLocked(owner, it.spec), true
		} else if !it.b.Editor {
			// A breakpoint c set with the CLI, now in its editor's list.
			it.changed = true
		}

		it.b.Editor = true
	}

	return gone
}

// matchItems pairs the accepted items with breakpoints of old: by requested
// line or function first, then (line breakpoints) by the line the adapter
// placed them on. Matches are updated; it returns the unmatched
// breakpoints, sorted by id.
func matchItems(old []*breakpoint, items []replaceItem) []*breakpoint {
	byRequested := func(b *breakpoint, spec api.BreakpointSpec) bool {
		return b.key() == (slotKey{function: spec.Function, line: spec.Line})
	}
	byPlaced := func(b *breakpoint, spec api.BreakpointSpec) bool { return spec.Function == "" && b.Line == spec.Line }

	for _, same := range []func(*breakpoint, api.BreakpointSpec) bool{byRequested, byPlaced} {
		for i := range items {
			it := &items[i]
			if it.refused != nil || it.b != nil {
				continue
			}

			if j := slices.IndexFunc(old, func(b *breakpoint) bool { return b != nil && same(b, it.spec) }); j >= 0 {
				it.b, old[j] = old[j], nil
				it.changed = it.b.update(it.spec)
			}
		}
	}

	gone := slices.DeleteFunc(old, func(b *breakpoint) bool { return b == nil })
	sort.Slice(gone, func(i, j int) bool { return gone[i].ID < gone[j].ID })

	return gone
}

// update gives b spec's condition, hit condition and log message and
// reports whether they changed; an unchanged breakpoint keeps its hit
// count.
func (b *breakpoint) update(spec api.BreakpointSpec) bool {
	if b.Condition == spec.Condition && b.HitCondition == spec.HitCondition && b.LogMessage == spec.LogMessage && b.Anchor == "" {
		return false
	}

	b.Condition, b.Anchor = spec.Condition, ""
	b.setEmulated(spec)

	return true
}

// replaceResults returns each item's breakpoint as it is now, or its
// refusal: ID 0, no file, unverified, with why.
func (s *Session) replaceResults(items []replaceItem) []api.Breakpoint {
	out := make([]api.Breakpoint, len(items))

	s.mu.Lock()
	for i := range items {
		it := &items[i]
		if it.b != nil {
			out[i] = it.b.Breakpoint

			continue
		}

		out[i] = api.Breakpoint{
			RequestedLine: it.spec.Line, Line: it.spec.Line, Function: it.spec.Function,
			Message: refusalText(it.refused),
		}
	}
	s.mu.Unlock()

	return s.annotate(out)
}

// refusalText is why a breakpoint was refused, for its message.
func refusalText(err error) string {
	if e, ok := errors.AsType[*api.Error](err); ok {
		return e.Message
	}

	return fmt.Sprint(err)
}
