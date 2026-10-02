// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
)

// carry is what a service's old sessions leave for its new one: the caller's
// own breakpoints and exception mode (docs/adr/0021, D18).
type carry struct {
	bps     []api.BreakpointSpec
	mode    api.ExceptionMode
	dropped int
}

// addBreakpoint keeps one breakpoint per key.
func (c *carry) addBreakpoint(spec api.BreakpointSpec) {
	if !slices.ContainsFunc(c.bps, func(o api.BreakpointSpec) bool { return bpKey(o) == bpKey(spec) }) {
		c.bps = append(c.bps, spec)
	}
}

// modeRank orders exception modes by how much they stop at.
func modeRank(m api.ExceptionMode) int {
	switch m {
	case api.ExceptionsAll:
		return 2
	case api.ExceptionsUncaught:
		return 1
	case api.ExceptionsNone:
		return 0
	default:
		return 0
	}
}

// addMode keeps the mode that stops at the most.
func (c *carry) addMode(m api.ExceptionMode) {
	if modeRank(m) > modeRank(c.mode) {
		c.mode = m
	}
}

// bpKey identifies a breakpoint within a session: its file and line (or
// anchor text, when no line is known), or its function.
func bpKey(spec api.BreakpointSpec) string {
	switch {
	case spec.Function != "":
		return "func:" + spec.Function
	case spec.Line == 0 && spec.Anchor != "":
		return spec.File + "@" + spec.Anchor
	default:
		return fmt.Sprintf("%s:%d", spec.File, spec.Line)
	}
}

// carriedSpec is the request that sets b again in a new session: the line it
// was asked for (not where the adapter placed it), its condition, hit
// condition, log message and anchor. Temporary breakpoints (run-until) and
// the editor's are not carried.
func carriedSpec(b *api.Breakpoint) (api.BreakpointSpec, bool) {
	if b.Temporary || b.Editor {
		return api.BreakpointSpec{}, false
	}

	if b.Function != "" {
		return api.BreakpointSpec{Function: b.Function, Condition: b.Condition}, true
	}

	line := b.RequestedLine
	if line < 1 {
		line = b.Line
	}

	if b.File == "" || line < 1 {
		return api.BreakpointSpec{}, false
	}

	return api.BreakpointSpec{
		File: b.File, Line: line, Condition: b.Condition, Anchor: b.Anchor, HitCondition: b.HitCondition, LogMessage: b.LogMessage,
	}, true
}

// capture reads the caller's own breakpoints and exception mode from one live
// session of s, before it is stopped. What can't be read is not carried; the
// session is stopped anyway.
func (r *launchRun) capture(ctx context.Context, cl *daemon.Client, s *launchSvc, id string) {
	ref := api.SessionRef{SessionID: id, Client: r.g.clientID()}

	var list []api.Breakpoint
	if err := boundedCall(ctx, cl, api.MethodBreakpointLs, api.BreakpointListParams{SessionRef: ref, Mine: true}, &list); err == nil {
		for i := range list {
			if spec, ok := carriedSpec(&list[i]); ok {
				s.carry.addBreakpoint(spec)
			}
		}
	}

	me, err := api.ParseClient(r.g.clientID())
	if err != nil {
		return
	}

	var ex api.ExceptionsResult
	if err := boundedCall(ctx, cl, api.MethodBreakpointExceptions, api.ExceptionsParams{SessionRef: ref}, &ex); err != nil {
		return
	}

	for _, m := range ex.Modes {
		if m.Client == me.ID {
			s.carry.addMode(m.Mode)
		}
	}
}

// launchBatch is services that launch with the same breakpoints and
// exception mode: one container.launch.
type launchBatch struct {
	svcs []*launchSvc
	bps  []api.BreakpointSpec
	mode api.ExceptionMode
}

// breakpointsFor are the breakpoints service s launches with: what its old
// sessions had, minus those that can't be set any more (the file is gone, or
// outside the path map: counted, not fatal), plus every --bp (which wins over
// a carried one of the same place).
func (r *launchRun) breakpointsFor(s *launchSvc) []api.BreakpointSpec {
	var bps []api.BreakpointSpec

	for _, spec := range s.carry.bps {
		if r.settable(spec) {
			bps = append(bps, spec)
		} else {
			s.carry.dropped++
		}
	}

	for _, b := range r.req.bps {
		spec := b.spec
		if spec.Function == "" {
			spec.File = resolvedPath(spec.File)
		}

		bps = slices.DeleteFunc(bps, func(o api.BreakpointSpec) bool { return bpKey(o) == bpKey(spec) })
		bps = append(bps, spec)
	}

	return bps
}

// settable reports whether a carried line breakpoint's file still exists and
// has a container path.
func (r *launchRun) settable(spec api.BreakpointSpec) bool {
	if spec.Function != "" {
		return true
	}

	if info, err := os.Stat(spec.File); err != nil || info.IsDir() {
		return false
	}

	_, ok := r.pm.ToRemote(spec.File)

	return ok
}

// batches groups the live services by what they launch with.
func (r *launchRun) batches() []launchBatch {
	var (
		out   []launchBatch
		index = map[string]int{}
	)

	for _, s := range r.live() {
		bps := r.breakpointsFor(s)

		mode := r.req.params.Exceptions
		if mode == "" {
			mode = s.carry.mode
		}

		slices.SortFunc(bps, func(a, b api.BreakpointSpec) int { return cmp.Compare(bpKey(a), bpKey(b)) })

		raw, err := json.Marshal([]any{bps, mode})
		if err != nil {
			raw = []byte(s.row.Service)
		}

		i, ok := index[string(raw)]
		if !ok {
			i = len(out)
			index[string(raw)] = i
			out = append(out, launchBatch{bps: bps, mode: mode})
		}

		out[i].svcs = append(out[i].svcs, s)
	}

	return out
}

// launch starts the apps of the services still in the run, one
// container.launch per batch, then reads back what each --bp did.
func (r *launchRun) launch(ctx context.Context, cl *daemon.Client) {
	for _, b := range r.batches() {
		r.launchBatch(ctx, cl, b)
	}

	r.verifyBreakpoints(ctx, cl)
}

// launchBatch is one container.launch for services that share breakpoints.
func (r *launchRun) launchBatch(ctx context.Context, cl *daemon.Client, b launchBatch) {
	params := r.req.params
	params.Group = r.project
	params.Breakpoints = b.bps
	params.Exceptions = b.mode
	params.Members = make([]api.ContainerLaunchSpec, len(b.svcs))

	for i, s := range b.svcs {
		params.Members[i] = api.ContainerLaunchSpec{
			Engine: r.engineSpec(), Ref: s.fi.ID, Service: s.row.Service, Project: r.project,
			Map: []api.PathMapping{{Remote: defaultMapRemote, Local: r.workDir}}, StopOnEntry: r.req.entry,
		}
	}

	ctx, cancel := context.WithTimeout(ctx, startBuildBudget+callSlack)
	defer cancel()

	var res api.ContainerLaunchResult
	if err := cl.Call(ctx, api.MethodContainerLaunch, params, &res); err != nil {
		for _, s := range b.svcs {
			r.fail(s, launchCallError(err))
		}

		return
	}

	if len(res.Members) != len(b.svcs) {
		for _, s := range b.svcs {
			r.fail(s, api.NewError(api.CodeInternal, fmt.Sprintf("eyedbgd answered %d members for %d containers", len(res.Members), len(b.svcs)), ""))
		}

		return
	}

	for i, m := range res.Members {
		switch {
		case m.Error != nil:
			b.svcs[i].res.Error = m.Error
		case m.Session != nil:
			sess := m.Session.Session
			b.svcs[i].res.Session = &sess
		default:
			r.fail(b.svcs[i], api.NewError(api.CodeInternal, "eyedbgd launched nothing and said no more", ""))
		}
	}
}

// launchCallError is the error of a container.launch call: a daemon that
// doesn't know the method predates it.
func launchCallError(err error) error {
	if api.CodeOf(err) == api.CodeUnknownMethod {
		return api.NewError(api.CodeVersionMismatch, "the running eyedbgd is too old to launch in containers",
			"stop it with 'eyedbg daemon stop' (--force ends its sessions); the next command starts a current one")
	}

	return err
}

// verifyBreakpoints reads, for each launched service, what its session did
// with each --bp.
func (r *launchRun) verifyBreakpoints(ctx context.Context, cl *daemon.Client) {
	if len(r.req.bps) == 0 {
		return
	}

	for _, s := range r.svcs {
		if s.res.Session == nil {
			continue
		}

		var list []api.Breakpoint

		ref := api.SessionRef{SessionID: s.res.Session.ID, Client: r.g.clientID()}
		err := boundedCall(ctx, cl, api.MethodBreakpointLs, api.BreakpointListParams{SessionRef: ref, Mine: true}, &list)

		for _, b := range r.req.bps {
			s.res.Breakpoints = append(s.res.Breakpoints, outcomeOf(b, list, err))
		}
	}
}

// outcomeOf is what a session did with one --bp, found in its breakpoint
// list.
func outcomeOf(b breakpointArg, list []api.Breakpoint, listErr error) bpOutcome {
	if listErr != nil {
		return bpOutcome{Location: b.location, Error: jsonErr(listErr)}
	}

	file := resolvedPath(b.spec.File)

	for i := range list {
		bp := &list[i]

		switch {
		case b.spec.Function != "" && bp.Function == b.spec.Function,
			b.spec.Function == "" && bp.File == file && (bp.RequestedLine == b.spec.Line || (b.spec.Line == 0 && bp.Anchor == b.spec.Anchor)):
			return bpOutcome{Location: b.location, Breakpoint: bp}
		}
	}

	return bpOutcome{Location: b.location, Error: api.NewError(api.CodeInternal, "the session doesn't list the breakpoint", "see 'eyedbg bp ls'")}
}
