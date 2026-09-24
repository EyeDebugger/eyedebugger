// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"encoding/json"
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
		c.attach(ctx, r)
	case *godap.LaunchRequest:
		c.fail(ctx, req, api.NewError(api.CodeInvalidRequest,
			"the DAP facade joins sessions: start one with 'eyedbg start'", "then attach to it"), false)
	case *godap.ConfigurationDoneRequest:
		c.configurationDone(ctx, r)
	case *godap.DisconnectRequest:
		// Leaving never ends the session, whatever the arguments ask.
		c.respond(ctx, req, &godap.DisconnectResponse{})

		return true
	default:
		if err := c.admissible(req); err != nil {
			c.fail(ctx, req, err, false)

			return false
		}

		c.spawn(ctx, in, isOrdered(req), func() { c.handle(ctx, req) })
	}

	return false
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
// breakpoints, the exception mode, and execution requests.
func isOrdered(req godap.RequestMessage) bool {
	switch req.(type) {
	case *godap.SetBreakpointsRequest, *godap.SetFunctionBreakpointsRequest, *godap.SetExceptionBreakpointsRequest:
		return true
	default:
		return isExecution(req)
	}
}

// admissible checks the handshake order for a request that isn't a
// lifecycle one: after initialize and attach, and for execution requests
// after configurationDone.
func (c *connection) admissible(req godap.RequestMessage) error {
	switch {
	case c.phase == phaseNew:
		return api.NewError(api.CodeInvalidRequest, "send initialize first", "")
	case c.phase == phaseInitialized:
		return api.NewError(api.CodeInvalidRequest, "attach to the session first", "")
	case c.phase == phaseAttached && isExecution(req):
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
	case c.phase != phaseNew:
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "initialize was already sent", ""), false)

		return
	case !a.LinesStartAt1 || !a.ColumnsStartAt1 || (a.PathFormat != "" && a.PathFormat != "path"):
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest,
			"eyedbg's DAP facade needs linesStartAt1, columnsStartAt1 and pathFormat path", ""), false)

		return
	}

	readyCtx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	caps, err := c.sess.Ready(readyCtx)
	if err != nil {
		c.fail(ctx, r, err, false)

		return
	}

	c.phase, c.invalidated = phaseInitialized, a.SupportsInvalidatedEvent
	c.respond(ctx, r, &godap.InitializeResponse{Body: capabilities(caps, c.sess.SupportedExceptionModes())})
}

// attach joins the connection's session (it already is: the arguments
// may only name it), then sends initialized.
func (c *connection) attach(ctx context.Context, r *godap.AttachRequest) {
	if c.phase != phaseInitialized {
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

	if args.Session != nil && *args.Session != c.sess.ID {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest,
			"this connection joined session "+c.sess.ID+", not "+*args.Session, "run 'eyedbg dap -s ID' for another session"), false)

		return
	}

	c.phase = phaseAttached
	if c.respond(ctx, r, &godap.AttachResponse{}) {
		c.send(ctx, &godap.InitializedEvent{Event: event("initialized")})
	}
}

// configurationDone joins the session's event log: it answers, sends the
// program's state as of the join point and what the adapter changed about
// the connection's breakpoints since their responses, then follows the
// log from the join point.
func (c *connection) configurationDone(ctx context.Context, r *godap.ConfigurationDoneRequest) {
	if c.phase != phaseAttached {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "configurationDone needs attach first, once", ""), false)

		return
	}

	info, seq := c.sess.JoinPoint()
	c.phase = phaseConfigured

	if !c.respond(ctx, r, &godap.ConfigurationDoneResponse{}) {
		return
	}

	st := &followState{self: c.client.ID, invalidated: c.invalidated, owns: c.owns}

	switch info.State {
	case api.StateStopped:
		c.sendAll(ctx, stoppedEvent(info.Stop, st))
	case api.StateExited:
		if info.ExitCode != nil {
			c.send(ctx, &godap.ExitedEvent{Event: event("exited"), Body: godap.ExitedEventBody{ExitCode: *info.ExitCode}})
		}

		c.send(ctx, &godap.TerminatedEvent{Event: event("terminated")})

		return
	case api.StateStarting, api.StateRunning, api.StateLost:
	}

	c.sendAll(ctx, c.reconcile())
	c.wg.Go(func() { c.protect(ctx, func() { c.follow(ctx, seq, st) }) })
}

// reconcile returns a breakpoint changed event for each of the
// connection's breakpoints whose state differs from what its response said.
func (c *connection) reconcile() []godap.EventMessage {
	now := c.sess.Breakpoints(c.client.ID)

	c.mu.Lock()
	defer c.mu.Unlock()

	var out []godap.EventMessage

	for i := range now {
		told, ok := c.reported[now[i].ID]
		if !ok {
			continue
		}

		if b := dapBreakpoint(&now[i]); b != told {
			c.reported[b.Id] = b
			out = append(out, &godap.BreakpointEvent{Event: event("breakpoint"), Body: godap.BreakpointEventBody{Reason: actionChanged, Breakpoint: b}})
		}
	}

	return out
}

// handle handles a request that isn't a lifecycle one. Execution requests
// and breakpoint changes hold the event gate until their response is
// written.
func (c *connection) handle(ctx context.Context, req godap.RequestMessage) {
	exec := isExecution(req)

	switch req.(type) {
	case *godap.SetBreakpointsRequest, *godap.SetFunctionBreakpointsRequest:
		c.gate.Lock()
		defer c.gate.Unlock()
	default:
		if exec {
			c.gate.Lock()
			defer c.gate.Unlock()
		}
	}

	resp, err := c.serve(ctx, req)
	if err != nil {
		c.fail(ctx, req, err, exec)

		return
	}

	c.respond(ctx, req, resp)
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
		return &godap.TerminateResponse{}, c.sess.Terminate(ctx, c.client)
	case *godap.SetBreakpointsRequest:
		return c.setBreakpoints(ctx, r)
	case *godap.SetFunctionBreakpointsRequest:
		return c.setFunctionBreakpoints(ctx, r)
	case *godap.SetExceptionBreakpointsRequest:
		return c.setExceptionBreakpoints(ctx, r)
	default:
		return c.sess.Forward(ctx, c.client, req)
	}
}

func (c *connection) exec(ctx context.Context, kind string, thread int) error {
	_, err := c.sess.Exec(ctx, c.client, kind, thread)

	return err
}

// setBreakpoints replaces the connection's client's breakpoints in one
// file (columns are ignored; lines is used only without breakpoints).
func (c *connection) setBreakpoints(ctx context.Context, r *godap.SetBreakpointsRequest) (godap.ResponseMessage, error) {
	a := r.Arguments

	specs := make([]api.BreakpointSpec, 0, len(a.Breakpoints))
	for _, b := range a.Breakpoints {
		specs = append(specs, api.BreakpointSpec{Line: b.Line, Condition: b.Condition, HitCondition: b.HitCondition, LogMessage: b.LogMessage})
	}

	if a.Breakpoints == nil {
		for _, l := range a.Lines {
			specs = append(specs, api.BreakpointSpec{Line: l})
		}
	}

	got, err := c.sess.ReplaceBreakpoints(ctx, c.client, a.Source.Path, specs)
	out := c.track(bpsKey{path: a.Source.Path}, got)

	return &godap.SetBreakpointsResponse{Body: godap.SetBreakpointsResponseBody{Breakpoints: out}}, err
}

// setFunctionBreakpoints replaces the connection's client's function
// breakpoints.
func (c *connection) setFunctionBreakpoints(ctx context.Context, r *godap.SetFunctionBreakpointsRequest) (godap.ResponseMessage, error) {
	specs := make([]api.BreakpointSpec, 0, len(r.Arguments.Breakpoints))
	for _, b := range r.Arguments.Breakpoints {
		specs = append(specs, api.BreakpointSpec{Function: b.Name, Condition: b.Condition, HitCondition: b.HitCondition})
	}

	got, err := c.sess.ReplaceFunctionBreakpoints(ctx, c.client, specs)
	out := c.track(funcBreakpoints, got)

	return &godap.SetFunctionBreakpointsResponse{Body: godap.SetFunctionBreakpointsResponseBody{Breakpoints: out}}, err
}

// track records the breakpoints a request placed under key (got is nil
// when it placed none: the session had exited), and what its response
// says of them, and returns them as DAP breakpoints (never nil).
func (c *connection) track(key bpsKey, got []api.Breakpoint) []godap.Breakpoint {
	out := make([]godap.Breakpoint, len(got))

	c.mu.Lock()
	defer c.mu.Unlock()

	ids := make([]int, 0, len(got))

	for i := range got {
		out[i] = dapBreakpoint(&got[i])
		if got[i].ID != 0 {
			ids = append(ids, got[i].ID)
			c.reported[got[i].ID] = out[i]
		}
	}

	if got == nil {
		return out
	}

	for _, id := range c.bps[key] {
		if !slices.Contains(ids, id) {
			delete(c.reported, id)
		}
	}

	c.bps[key] = ids

	return out
}

// setExceptionBreakpoints sets the connection's client's exception mode:
// all if the filters name it, else uncaught, else none.
func (c *connection) setExceptionBreakpoints(ctx context.Context, r *godap.SetExceptionBreakpointsRequest) (godap.ResponseMessage, error) {
	mode := exceptionMode(r.Arguments.Filters)
	if _, err := c.sess.Exceptions(ctx, c.client, api.ExceptionsParams{Mode: mode}); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.excMode = mode
	c.mu.Unlock()

	return &godap.SetExceptionBreakpointsResponse{}, nil
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

// fail writes an error response for err.
func (c *connection) fail(ctx context.Context, req godap.RequestMessage, err error, showUser bool) {
	command := req.GetRequest().Command
	holder := ""

	if api.CodeOf(err) == api.CodeLeaseHeld {
		holder = c.sess.Lease().Holder
	}

	if api.CodeOf(err) == "" {
		c.logger.WarnContext(ctx, "facade request failed", slog.String("command", command), slog.String("error_type", fmt.Sprintf("%T", err)))
	}

	message, body := errorResponse(command, err, holder, showUser)
	if werr := c.srv.Fail(req, message, body); werr != nil {
		c.broken(ctx, req)
	}
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
