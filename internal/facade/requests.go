// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// dispatch handles one request: lifecycle requests here, in order, the
// rest on their own goroutines once the handshake allows them. It reports
// whether the client disconnected.
func (c *connection) dispatch(ctx context.Context, req godap.RequestMessage, in inFlight) bool {
	switch r := req.(type) {
	case *godap.InitializeRequest:
		c.initialize(ctx, r)
	case *godap.AttachRequest:
		if c.launch != nil {
			c.fail(ctx, req, api.NewError(api.CodeInvalidRequest,
				"this connection launches a session: send launch, or join one with 'eyedbg dap -s ID'", ""), false)

			return false
		}

		c.attach(ctx, r)
	case *godap.LaunchRequest:
		if c.launch == nil {
			c.fail(ctx, req, api.NewError(api.CodeInvalidRequest,
				"this connection joined a session: attach to it, or launch with 'eyedbg dap --launch'", ""), false)

			return false
		}

		c.startLaunch(ctx, r)
	case *godap.ConfigurationDoneRequest:
		if c.launch != nil {
			c.launchConfigurationDone(ctx, r, in)

			return false
		}

		c.configurationDone(ctx, r)
	case *godap.DisconnectRequest:
		c.disconnect(ctx, r)

		return true
	case *godap.TerminateRequest:
		if c.launch != nil && c.launchTerminate(ctx, r, in) {
			return false
		}

		c.dispatchOther(ctx, req, in)
	default:
		c.dispatchOther(ctx, req, in)
	}

	return false
}

// dispatchOther hands a request that isn't a lifecycle one to the worker
// or its own goroutine, once the handshake allows it.
func (c *connection) dispatchOther(ctx context.Context, req godap.RequestMessage, in inFlight) {
	if err := c.admissible(req); err != nil {
		c.fail(ctx, req, err, false)

		return
	}

	c.spawn(ctx, in, isOrdered(req), func() { c.handle(ctx, req) })
}

// isExecution reports whether req changes the program's execution or
// state: it needs the lease and a finished handshake, and holds the event
// gate.
func isExecution(req godap.RequestMessage) bool {
	switch req.(type) {
	case *godap.ContinueRequest, *godap.NextRequest, *godap.StepInRequest, *godap.StepOutRequest,
		*godap.PauseRequest, *godap.TerminateRequest:
		return true
	default:
		return session.ForwardsAsExecution(req)
	}
}

// isOrdered reports whether req changes something, so that the
// connection's requests of its kind must apply in the order they arrived:
// breakpoints, the exception mode, the lease, and execution requests.
func isOrdered(req godap.RequestMessage) bool {
	if _, ok := req.(*godap.SetExceptionBreakpointsRequest); ok {
		return true
	}

	return isGated(req)
}

// isGated reports whether req holds the event gate from its session call
// until its response (and the events it causes) are written: execution
// requests, breakpoint changes and lease changes.
func isGated(req godap.RequestMessage) bool {
	switch r := req.(type) {
	case *godap.SetBreakpointsRequest, *godap.SetFunctionBreakpointsRequest:
		return true
	case *LeaseRequest:
		return r.Arguments.Action != LeaseActionStatus
	case *BreakpointsRequest:
		return changesBreakpoints(req)
	default:
		return isExecution(req)
	}
}

// changesBreakpoints reports whether req changes breakpoints: the editor's
// view is reconciled after its response.
func changesBreakpoints(req godap.RequestMessage) bool {
	switch r := req.(type) {
	case *godap.SetBreakpointsRequest, *godap.SetFunctionBreakpointsRequest:
		return true
	case *BreakpointsRequest:
		return r.Arguments.Action == BreakpointsActionRemove
	default:
		return false
	}
}

// admissible checks the handshake order for a request that isn't a
// lifecycle one: after initialize and attach (a launch connection: once its
// launch bound the session), and for execution requests after
// configurationDone (a launch connection: after the launch response). A
// refusal never says "does not support": an editor extension takes that
// as the session not supporting the request.
func (c *connection) admissible(req godap.RequestMessage) error {
	switch ph := c.phase.load(); {
	case ph == phaseNew:
		return api.NewError(api.CodeInvalidRequest, "send initialize first", "")
	case ph == phaseInitialized && c.launch != nil:
		return api.NewError(api.CodeInvalidRequest, "send launch first", "")
	case ph == phaseInitialized:
		return api.NewError(api.CodeInvalidRequest, "attach to the session first", "")
	case ph == phaseLaunching:
		return api.NewError(api.CodeInvalidRequest, "the session is still starting: wait for the initialized event", "")
	case ph == phaseFailed:
		return api.NewError(api.CodeInvalidRequest, "this connection's launch failed: disconnect, then launch again", "")
	case ph == phaseAttached && isExecution(req):
		return api.NewError(api.CodeInvalidRequest, req.GetRequest().Command+" needs configurationDone first", "")
	default:
		return nil
	}
}

// initialize waits for the session to finish starting and answers with
// the facade's capabilities. The adapter speaks 1-based lines and columns
// and file paths to eyedbg, so a client must too.
func (c *connection) initialize(ctx context.Context, r *godap.InitializeRequest) {
	a := r.Arguments

	switch {
	case c.phase.load() != phaseNew:
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "initialize was already sent", ""), false)

		return
	case !a.LinesStartAt1 || !a.ColumnsStartAt1 || (a.PathFormat != "" && a.PathFormat != "path"):
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest,
			"eyedbg's DAP facade needs linesStartAt1, columnsStartAt1 and pathFormat path", ""), false)

		return
	}

	if c.launch != nil {
		// No session, no adapter yet: the forced-on set and both
		// exception filters; the adapter's own capabilities follow as a
		// capabilities event once the launch started it.
		c.invalidated = a.SupportsInvalidatedEvent
		c.phase.store(phaseInitialized)
		c.respond(ctx, r, &godap.InitializeResponse{Body: capabilities(godap.Capabilities{}, launchExceptionModes())})

		return
	}

	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	caps, err := c.session().Ready(readyCtx)
	if err != nil {
		c.fail(ctx, r, err, false)

		return
	}

	c.invalidated = a.SupportsInvalidatedEvent
	c.phase.store(phaseInitialized)
	c.respond(ctx, r, &godap.InitializeResponse{Body: capabilities(caps, c.session().SupportedExceptionModes())})
}

// attach joins the connection's session (it already is: the arguments
// may only name it), then sends initialized.
func (c *connection) attach(ctx context.Context, r *godap.AttachRequest) {
	if c.phase.load() != phaseInitialized {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "attach needs initialize first, once", ""), false)

		return
	}

	var args struct {
		Session *string `json:"session"`
	}

	if len(r.Arguments) > 0 {
		if err := json.Unmarshal(r.Arguments, &args); err != nil {
			c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "invalid attach arguments", ""), false)

			return
		}
	}

	if args.Session != nil && *args.Session != c.session().ID {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest,
			"this connection joined session "+c.session().ID+", not "+*args.Session, "run 'eyedbg dap -s ID' for another session"), false)

		return
	}

	c.phase.store(phaseAttached)
	if c.respond(ctx, r, &godap.AttachResponse{}) {
		c.send(ctx, &godap.InitializedEvent{Event: event("initialized")})
	}
}

// configurationDone joins the session's event log: it answers, then,
// holding the event gate (a setBreakpoints sent before it may still be
// running), replays the output held as of the join point, sends the
// program's state then, announces the other clients' breakpoints and what
// changed about the connection's own since their responses, and the lease,
// clients and breakpoints (eyedbg/*); then it follows the log from the join
// point. A session that exited gets its output and its end only.
func (c *connection) configurationDone(ctx context.Context, r *godap.ConfigurationDoneRequest) {
	if c.phase.load() != phaseAttached {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "configurationDone needs attach first, once", ""), false)

		return
	}

	info, seq := c.session().JoinPoint()
	c.phase.store(phaseConfigured)

	if !c.respond(ctx, r, &godap.ConfigurationDoneResponse{}) {
		return
	}

	c.gate.Lock()
	defer c.gate.Unlock()

	c.join(ctx, info, seq)
}

// join joins the session's event log at seq, where the session was info
// (what [session.Session.JoinPoint] returned), once the response that
// joins was written; under the event gate. See configurationDone.
func (c *connection) join(ctx context.Context, info api.SessionInfo, seq int) {
	st := &followState{self: c.client.ID, invalidated: c.invalidated}

	c.sendAll(ctx, c.replayOutput(seq))

	switch info.State {
	case api.StateStopped:
		c.sendAll(ctx, stoppedEvent(info.Stop, st, false))
	case api.StateExited:
		if info.ExitCode != nil {
			c.send(ctx, &godap.ExitedEvent{Event: event("exited"), Body: godap.ExitedEventBody{ExitCode: *info.ExitCode}})
		}

		c.send(ctx, &godap.TerminatedEvent{Event: event("terminated")})

		return
	case api.StateStarting, api.StateRunning, api.StateLost:
	}

	c.view.configured = true
	c.sendAll(ctx, c.reconcile())

	if info.Lease != nil {
		c.send(ctx, leaseEvent(*info.Lease))
	}

	c.send(ctx, clientsEvent(info.Clients))
	c.send(ctx, c.breakpointsEvent())
	c.wg.Go(func() { c.protect(ctx, func() { c.follow(ctx, seq, st) }) })
}

// replayOutput returns the output events of the newest output the session
// holds up to seq, after a console line counting the older chunks left out.
func (c *connection) replayOutput(seq int) []godap.EventMessage {
	lines, omitted := c.session().OutputBefore(seq, replayChunks, replayBytes)
	out := make([]godap.EventMessage, 0, len(lines)+1)

	if omitted > 0 {
		out = append(out, consoleLine(fmt.Sprintf("%d earlier output chunks aren't shown ('eyedbg output' has them)", omitted)))
	}

	for _, l := range lines {
		out = append(out, outputEvent(l.Category, l.Text))
	}

	return out
}

// disconnect leaves: holding the event gate, it retracts the mirrors,
// stops every later event and answers. Leaving never ends the session,
// whatever the arguments ask; a restart asks the session to wait for the
// client's next connection. A launch connection first forgets the session
// it launched if that has exited, without the gate (the forget's events go
// through it), so the session is gone once the client has its response.
func (c *connection) disconnect(ctx context.Context, r *godap.DisconnectRequest) {
	c.restart = r.Arguments != nil && r.Arguments.Restart

	if c.launch != nil {
		c.forgetExited(ctx)
	}

	c.gate.Lock()
	defer c.gate.Unlock()

	c.sendAll(ctx, retractMirrors(c.view))
	c.view.leaving = true
	c.respond(ctx, r, &godap.DisconnectResponse{})
}

// handle handles a request that isn't a lifecycle one. Execution requests
// and breakpoint and lease changes hold the event gate until their response
// (and, for breakpoints, the events that bring the editor's view up to
// date) are written.
func (c *connection) handle(ctx context.Context, req godap.RequestMessage) {
	exec := isExecution(req)

	if isGated(req) {
		c.gate.Lock()
		defer c.gate.Unlock()
	}

	resp, err := c.serve(ctx, req)

	answered := false
	if err != nil {
		c.fail(ctx, req, err, exec)
	} else {
		answered = c.respond(ctx, req, resp)
	}

	if _, ok := req.(*godap.SetExceptionBreakpointsRequest); ok && answered {
		c.afterExceptions()
	}

	if changesBreakpoints(req) {
		c.afterBreakpoints(ctx, answered)
	}
}

// afterBreakpoints sends, after a breakpoint change's response (under the
// event gate), the events that response causes (retracted copies, if it was
// answered) and brings the editor's view up to date.
func (c *connection) afterBreakpoints(ctx context.Context, answered bool) {
	after := c.view.after
	c.view.after = nil

	if c.view.leaving {
		return
	}

	if answered {
		c.sendAll(ctx, after)
	}

	c.sendAll(ctx, c.reconcile())
}

// serve runs a request against the session and returns its response.
func (c *connection) serve(ctx context.Context, req godap.RequestMessage) (godap.ResponseMessage, error) {
	switch r := req.(type) {
	case *godap.ContinueRequest:
		return &godap.ContinueResponse{Body: godap.ContinueResponseBody{AllThreadsContinued: true}},
			c.exec(ctx, session.ExecContinue, r.Arguments.ThreadId)
	case *godap.NextRequest:
		return &godap.NextResponse{}, c.exec(ctx, session.ExecNext, r.Arguments.ThreadId)
	case *godap.StepInRequest:
		return &godap.StepInResponse{}, c.exec(ctx, session.ExecStepIn, r.Arguments.ThreadId)
	case *godap.StepOutRequest:
		return &godap.StepOutResponse{}, c.exec(ctx, session.ExecStepOut, r.Arguments.ThreadId)
	case *godap.PauseRequest:
		return &godap.PauseResponse{}, c.exec(ctx, session.ExecPause, r.Arguments.ThreadId)
	case *godap.TerminateRequest:
		if c.launch != nil {
			// The session this connection launched: ended and forgotten,
			// as 'eyedbg stop' does (under the lease).
			return &godap.TerminateResponse{}, c.stopLaunched(ctx, c.session())
		}

		return &godap.TerminateResponse{}, c.session().Terminate(ctx, c.client)
	case *godap.SetBreakpointsRequest:
		return c.setBreakpoints(ctx, r)
	case *godap.SetFunctionBreakpointsRequest:
		return c.setFunctionBreakpoints(ctx, r)
	case *godap.SetExceptionBreakpointsRequest:
		return c.setExceptionBreakpoints(ctx, r)
	case *LeaseRequest:
		return c.lease(r)
	case *ClientsRequest:
		info := c.session().Info()

		return &ClientsResponse{Body: ClientsBody{Clients: clientsEvent(info.Clients).Body.Clients, Lease: info.Lease}}, nil
	case *BreakpointsRequest:
		return c.breakpoints(ctx, r)
	default:
		return c.session().Forward(ctx, c.client, req)
	}
}

func (c *connection) exec(ctx context.Context, kind string, thread int) error {
	_, err := c.session().Exec(ctx, c.client, kind, thread)

	return err
}

// setBreakpoints replaces the connection's client's editor breakpoints in
// one file (lines is used only without breakpoints). Each entry is
// classified first (docs/adr/0014): the editor's copies of mirrors are
// answered with the mirror and never become the client's, stale copies are
// retracted (a negative id, removed after the response), and the rest are
// the client's own. An editor keys its lists by source path: the client's
// breakpoints the connection's lists hold under the file's other paths (a
// symlink) are kept. Under the event gate.
func (c *connection) setBreakpoints(ctx context.Context, r *godap.SetBreakpointsRequest) (godap.ResponseMessage, error) {
	a := r.Arguments

	entries := a.Breakpoints
	if entries == nil {
		for _, l := range a.Lines {
			entries = append(entries, godap.SourceBreakpoint{Line: l})
		}
	}

	if len(entries) > maxRequestBreakpoints {
		return nil, tooManyBreakpoints(len(entries))
	}

	var bps []api.Breakpoint

	path := a.Source.Path

	key, keyErr := session.FileKey(path)
	if keyErr != nil {
		key = ""
	} else {
		bps = c.session().Breakpoints("")
	}

	classes := classify(entries, key, path, c.view, bps, c.client.ID)

	var (
		specs []api.BreakpointSpec
		own   []int
	)

	for i, e := range entries {
		if classes[i].kind == entryOwn {
			specs = append(specs, api.BreakpointSpec{Line: e.Line, Condition: e.Condition, HitCondition: e.HitCondition, LogMessage: e.LogMessage})
			own = append(own, i)
		}
	}

	var keep []int
	if key != "" {
		keep = c.view.keep(key, path)
		c.view.sourceSeen(path, key, len(own) > 0)
	}

	got, err := c.session().ReplaceBreakpoints(ctx, c.client, path, specs, keep)
	placed := c.view.track(bpsKey{path: path}, got)

	out := make([]godap.Breakpoint, len(entries))
	for j, i := range own {
		if j < len(placed) {
			out[i] = placed[j]
		}
	}

	c.answerCopies(path, entries, classes, out)
	settle(c.view, key, path, entries, classes)

	return &godap.SetBreakpointsResponse{Body: godap.SetBreakpointsResponseBody{Breakpoints: out}}, err
}

// tooManyBreakpoints refuses a breakpoint list longer than
// maxRequestBreakpoints.
func tooManyBreakpoints(n int) error {
	return api.NewError(api.CodeInvalidRequest,
		fmt.Sprintf("%d breakpoints in one request: eyedbg takes at most %d", n, maxRequestBreakpoints), "")
}

// answerCopies fills out's answers to the entries that aren't the client's
// own: echoes get their mirror, retracted copies a negative id (and a
// removed event after the response, with one console line saying why),
// duplicates the session's refusal.
func (c *connection) answerCopies(path string, entries []godap.SourceBreakpoint, classes []entryClass, out []godap.Breakpoint) {
	retracts := 0

	for i, cl := range classes {
		switch cl.kind {
		case entryEcho:
			out[i] = cl.told
		case entryRetract:
			id := c.view.nextRetractID()
			out[i] = retracted(id, entries[i].Line)
			c.view.after = append(c.view.after, removedEvent(id))
			retracts++
		case entryDuplicate:
			out[i] = godap.Breakpoint{Line: entries[i].Line, Message: duplicateMessage}
		case entryOwn:
		}
	}

	if retracts > 0 {
		c.view.after = append(c.view.after, consoleLine(fmt.Sprintf(
			"removed %d leftover copies of other clients' breakpoints from %s (column 1 marks eyedbg's copies)", retracts, path)))
	}
}

// setFunctionBreakpoints replaces the connection's client's function
// breakpoints.
func (c *connection) setFunctionBreakpoints(ctx context.Context, r *godap.SetFunctionBreakpointsRequest) (godap.ResponseMessage, error) {
	if len(r.Arguments.Breakpoints) > maxRequestBreakpoints {
		return nil, tooManyBreakpoints(len(r.Arguments.Breakpoints))
	}

	specs := make([]api.BreakpointSpec, 0, len(r.Arguments.Breakpoints))
	for _, b := range r.Arguments.Breakpoints {
		specs = append(specs, api.BreakpointSpec{Function: b.Name, Condition: b.Condition, HitCondition: b.HitCondition})
	}

	got, err := c.session().ReplaceFunctionBreakpoints(ctx, c.client, specs)
	out := c.view.track(funcBreakpoints, got)

	return &godap.SetFunctionBreakpointsResponse{Body: godap.SetFunctionBreakpointsResponseBody{Breakpoints: out}}, err
}

// setExceptionBreakpoints sets the connection's client's exception mode:
// all if the filters name it, else uncaught, else none.
func (c *connection) setExceptionBreakpoints(ctx context.Context, r *godap.SetExceptionBreakpointsRequest) (godap.ResponseMessage, error) {
	mode := exceptionMode(r.Arguments.Filters)

	// A launch connection offered both filters before the adapter was
	// known: a mode it can't serve is none for the client, and said once.
	if c.launch != nil && mode != api.ExceptionsNone && !slices.Contains(c.session().SupportedExceptionModes(), mode) {
		c.launch.unservable = mode
		mode = api.ExceptionsNone
	}

	if _, err := c.session().Exceptions(ctx, c.client, api.ExceptionsParams{Mode: mode}); err != nil {
		return nil, err
	}

	c.presence.Load().ExceptionsSet(mode)

	return &godap.SetExceptionBreakpointsResponse{}, nil
}

// lease answers eyedbg/lease: the lease commands of the CLI, force
// included.
func (c *connection) lease(r *LeaseRequest) (godap.ResponseMessage, error) {
	a := r.Arguments

	var (
		info api.LeaseInfo
		err  error
	)

	switch a.Action {
	case LeaseActionStatus:
		info = c.session().Lease()
	case LeaseActionTake:
		info, err = c.session().TakeLease(c.client, a.Force)
	case LeaseActionRelease:
		info = c.session().ReleaseLease(c.client)
	case LeaseActionRequest:
		info, err = c.session().RequestLease(c.client, a.Message)
	case LeaseActionGrant:
		info, err = c.session().GrantLease(c.client, a.To, a.Force)
	case LeaseActionPolicy:
		if a.Policy == "" {
			return nil, api.NewError(api.CodeInvalidRequest, CommandLease+" policy needs a policy: free, handoff or human-priority", "")
		}

		info, err = c.session().SetLeasePolicy(c.client, a.Policy, a.Force)
	default:
		return nil, api.NewError(api.CodeInvalidRequest,
			CommandLease+" needs an action: status, take, release, request, grant or policy", "")
	}

	if err != nil {
		return nil, err
	}

	return &LeaseResponse{Body: LeaseBody{Lease: info}}, nil
}

// breakpoints answers eyedbg/breakpoints: every client's breakpoints, or
// removing one as 'eyedbg bp rm ID [--force]' does.
func (c *connection) breakpoints(ctx context.Context, r *BreakpointsRequest) (godap.ResponseMessage, error) {
	a := r.Arguments

	switch a.Action {
	case "", BreakpointsActionList:
		return &BreakpointsResponse{Body: breakpointsBody(c.session().Breakpoints(""))}, nil
	case BreakpointsActionRemove:
		if a.ID < 1 {
			return nil, api.NewError(api.CodeInvalidRequest, CommandBreakpoints+" remove needs a breakpoint id from 1", "")
		}

		removed, _, err := c.session().RemoveBreakpoint(ctx, c.client, a.ID, a.Force)
		if err != nil {
			return nil, err
		}

		body := breakpointsBody(c.session().Breakpoints(""))
		body.Removed = removed

		return &BreakpointsResponse{Body: body}, nil
	default:
		return nil, api.NewError(api.CodeInvalidRequest, CommandBreakpoints+" needs an action: list or remove", "")
	}
}

// respond writes a successful response; false means the connection broke
// (and is now closed).
func (c *connection) respond(ctx context.Context, req godap.RequestMessage, resp godap.ResponseMessage) bool {
	if err := c.srv.Respond(req, resp); err != nil {
		c.broken(ctx, req)

		return false
	}

	return true
}

// fail writes an error response for err. A request the connection's end
// canceled isn't answered: its client left (or the daemon is stopping).
func (c *connection) fail(ctx context.Context, req godap.RequestMessage, err error, showUser bool) {
	command := req.GetRequest().Command

	if abandoned(ctx, err) {
		c.logger.DebugContext(ctx, "facade request abandoned: the connection is closing", slog.String("command", command))

		return
	}

	holder := ""

	if s := c.session(); s != nil && api.CodeOf(err) == api.CodeLeaseHeld {
		holder = s.Lease().Holder
	}

	if api.CodeOf(err) == "" {
		c.logger.WarnContext(ctx, "facade request failed", slog.String("command", command), slog.String("error_type", fmt.Sprintf("%T", err)))
	}

	message, body := errorResponse(command, err, holder, showUser)
	if werr := c.srv.Fail(req, message, body); werr != nil {
		c.broken(ctx, req)
	}
}

// abandoned reports whether err, with no code of its own, is ctx's end:
// the connection's context, canceled once its reader stopped. Any other
// error, a timeout of the request's own included, is still a failure.
func abandoned(ctx context.Context, err error) bool {
	cause := ctx.Err()

	return cause != nil && api.CodeOf(err) == "" && errors.Is(err, cause)
}

// send writes an event outside the follower (before it starts).
func (c *connection) send(ctx context.Context, ev godap.EventMessage) {
	if err := c.srv.Send(ev); err != nil {
		c.logger.DebugContext(ctx, "facade write failed", slog.String("event", ev.GetEvent().Event))
		c.close()
	}
}

func (c *connection) sendAll(ctx context.Context, events []godap.EventMessage) {
	for _, ev := range events {
		c.send(ctx, ev)
	}
}

// broken closes a connection whose write failed.
func (c *connection) broken(ctx context.Context, req godap.RequestMessage) {
	c.logger.DebugContext(ctx, "facade write failed", slog.String("command", req.GetRequest().Command), slog.Int("seq", req.GetRequest().Seq))
	c.close()
}
