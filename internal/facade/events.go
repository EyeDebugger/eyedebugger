// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"slices"
	"strconv"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Breakpoint event actions (the log's, and DAP's reasons).
const (
	actionChanged = "changed"
	actionRemoved = "removed"
	actionNew     = "new"
)

// logpointCategory is the output category of the session's emulated
// logpoints; DAP has no such category.
const logpointCategory = "logpoint"

// Output replayed at the join (docs/adr/0014): the newest chunks, within
// both bounds.
const (
	replayChunks = 200
	replayBytes  = 64 << 10
)

// followState is what translating the event log needs besides the events.
type followState struct {
	// self is the connection's client id.
	self string
	// invalidated: the client declared supportsInvalidatedEvent.
	invalidated bool
	// lastThread is the thread of the last stop seen.
	lastThread int
	// lastExec is the client of the latest execution request (a resume
	// or a pause) since the last stop; empty when none was seen.
	lastExec string
}

func event(name string) godap.Event { return godap.Event{Event: name} }

// translate turns one session event into the standard DAP events the
// connection's client gets for it alone (docs/adr/0012): stops, resumes
// (another client's execution request, or the adapter's), output, threads
// and the end. Its own execution requests are covered by their responses.
// Breakpoints, the lease and clients need the connection's view (see
// [connection.translate]).
func translate(ev *api.Event, st *followState) []godap.EventMessage {
	switch ev.Kind {
	case api.EventStopped:
		// A stop another client's execution request caused must not take
		// the client's focus (docs/adr/0014).
		hint := st.lastExec != "" && st.lastExec != st.self
		st.lastExec = ""

		return stoppedEvent(ev.Stop, st, hint)
	case api.EventExec:
		if runs(ev.Action) {
			st.lastExec = ev.Client
		}

		return execEvent(ev, st)
	case api.EventContinued:
		return []godap.EventMessage{continuedEvent(ev.ThreadID, st)}
	case api.EventOutput:
		return []godap.EventMessage{outputEvent(ev.Category, ev.Text)}
	case api.EventThread:
		return []godap.EventMessage{&godap.ThreadEvent{Event: event("thread"), Body: godap.ThreadEventBody{Reason: ev.Reason, ThreadId: ev.ThreadID}}}
	case api.EventExited:
		if ev.ExitCode == nil {
			return nil
		}

		return []godap.EventMessage{&godap.ExitedEvent{Event: event("exited"), Body: godap.ExitedEventBody{ExitCode: *ev.ExitCode}}}
	case api.EventEnded:
		return []godap.EventMessage{&godap.TerminatedEvent{Event: event("terminated")}}
	case api.EventStarted, api.EventClient, api.EventLease, api.EventExceptions, api.EventBreakpoint:
		return nil
	default:
		return nil
	}
}

// outputEvent is program output; the session's logpoints are console
// output (DAP has no logpoint category).
func outputEvent(category, text string) *godap.OutputEvent {
	if category == logpointCategory {
		category = "console"
	}

	return &godap.OutputEvent{Event: event("output"), Body: godap.OutputEventBody{Category: category, Output: text}}
}

// consoleLine is a line of eyedbg's own on the editor's console.
func consoleLine(text string) *godap.OutputEvent {
	return outputEvent("console", "eyedbg: "+text+"\n")
}

// stoppedEvent is the stop; hint marks one the client didn't cause (DAP's
// preserveFocusHint).
func stoppedEvent(stop *api.StopInfo, st *followState, hint bool) []godap.EventMessage {
	if stop == nil {
		return nil
	}

	st.lastThread = stop.ThreadID

	return []godap.EventMessage{&godap.StoppedEvent{Event: event("stopped"), Body: godap.StoppedEventBody{
		Reason: stop.Reason, ThreadId: stop.ThreadID, Description: stop.Description, Text: stop.Text,
		AllThreadsStopped: stop.AllThreadsStopped, PreserveFocusHint: hint,
	}}}
}

// runs reports whether an exec action makes the program run or stop (not
// an eval or a set, which leave it stopped).
func runs(action string) bool {
	switch action {
	case session.ExecContinue, session.ExecNext, session.ExecStepIn, session.ExecStepOut, session.ExecRunUntil, session.ExecPause:
		return true
	default:
		return false
	}
}

// execEvent is another client's execution request: a resume is continued,
// a change of state (eval with side effects, set) invalidates the
// variables of clients that understand it.
func execEvent(ev *api.Event, st *followState) []godap.EventMessage {
	if ev.Client == st.self {
		return nil
	}

	switch ev.Action {
	case session.ExecContinue, session.ExecNext, session.ExecStepIn, session.ExecStepOut, session.ExecRunUntil:
		return []godap.EventMessage{continuedEvent(ev.ThreadID, st)}
	case session.ExecEval, session.ExecSet:
		if !st.invalidated {
			return nil
		}

		return []godap.EventMessage{&godap.InvalidatedEvent{
			Event: event("invalidated"), Body: godap.InvalidatedEventBody{Areas: []godap.InvalidatedAreas{"variables"}},
		}}
	default:
		return nil
	}
}

// continuedEvent resumes every thread (the session's model is the whole
// program); without a thread, the last stop's.
func continuedEvent(thread int, st *followState) godap.EventMessage {
	if thread == 0 {
		thread = st.lastThread
	}

	return &godap.ContinuedEvent{Event: event("continued"), Body: godap.ContinuedEventBody{ThreadId: thread, AllThreadsContinued: true}}
}

// resync is what a client that missed events gets: a console notice, then
// the program's state now. Its stop's cause is unknown, so it has no hint.
func resync(dropped int, info api.SessionInfo, st *followState) []godap.EventMessage {
	out := []godap.EventMessage{consoleLine(strconv.Itoa(dropped) + " session events were missed")}
	st.lastExec = ""

	switch info.State {
	case api.StateStopped:
		out = append(out, stoppedEvent(info.Stop, st, false)...)
	case api.StateRunning:
		out = append(out, continuedEvent(0, st))
	case api.StateStarting, api.StateExited, api.StateLost:
	}

	return out
}

// dapBreakpoint is b as DAP shows it to its client's editor: its message
// joins the adapter's and eyedbg's notes.
func dapBreakpoint(b *api.Breakpoint) godap.Breakpoint {
	msg := b.Message
	if b.Note != "" {
		if msg != "" {
			msg += "; "
		}

		msg += b.Note
	}

	line := b.Line
	if b.Function != "" {
		line = 0
	}

	return godap.Breakpoint{Id: b.ID, Verified: b.Verified, Line: line, Message: msg}
}

// follow sends the session's events after seq since, translated with st
// (as the join left it), until the session ends or ctx does. Each event is
// translated and written under the event gate, so it never overtakes the
// response of the request that caused it, and the connection's view of the
// breakpoints is only touched there. A batch that changed breakpoints ends
// with one eyedbg/breakpoints event.
func (c *connection) follow(ctx context.Context, since int, st *followState) {
	for {
		res := c.sess.Follow(ctx, since)
		if ctx.Err() != nil || len(res.Events) == 0 {
			return
		}

		if res.Dropped > 0 && !c.relay(func() []godap.EventMessage {
			return append(resync(res.Dropped, c.sess.Info(), st), c.reconcile()...)
		}) {
			return
		}

		breakpoints := false

		for i := range res.Events {
			ev := &res.Events[i]
			if !c.relay(func() []godap.EventMessage { return c.translate(ev, st) }) || ev.Kind == api.EventEnded {
				return
			}

			breakpoints = breakpoints || ev.Kind == api.EventBreakpoint
		}

		if breakpoints && !c.relay(func() []godap.EventMessage { return []godap.EventMessage{c.breakpointsEvent()} }) {
			return
		}

		since = res.Events[len(res.Events)-1].Seq
	}
}

// translate is [translate] for the connection (under the event gate): a
// breakpoint change reconciles the editor's view (docs/adr/0014), a lease
// or client change is also an eyedbg/lease or eyedbg/clients event, the end
// retracts the mirrors first, and another client's action is also an
// eyedbg/activity event.
func (c *connection) translate(ev *api.Event, st *followState) []godap.EventMessage {
	var out []godap.EventMessage

	if ev.Kind == api.EventEnded {
		out = retractMirrors(c.view)
	}

	out = append(out, translate(ev, st)...)

	switch ev.Kind {
	case api.EventBreakpoint:
		out = append(out, c.breakpointChanged(ev)...)
	case api.EventLease:
		if ev.Lease != nil {
			out = append(out, leaseEvent(*ev.Lease))
		}
	case api.EventClient:
		out = append(out, clientsEvent(c.sess.Info().Clients))
	case api.EventStarted, api.EventExec, api.EventContinued, api.EventStopped, api.EventOutput, api.EventThread,
		api.EventExited, api.EventEnded, api.EventExceptions:
	}

	if activity(ev, c.client.ID) {
		out = append(out, activityEvent(ev))
	}

	return out
}

// breakpointChanged is what a breakpoint event of the log sends: the
// reconcile, and, as M1 did, a changed event for an adapter's change to a
// breakpoint the connection reported even when its response already told
// the change (the adapter verified it while the request was in flight).
func (c *connection) breakpointChanged(ev *api.Event) []godap.EventMessage {
	out := c.reconcile()

	if ev.Action != actionChanged || ev.Client != "" || ev.Breakpoint == nil || !c.view.configured {
		return out
	}

	id := ev.Breakpoint.ID

	told, reported := c.view.reported[id]
	if !reported || slices.ContainsFunc(out, func(e godap.EventMessage) bool {
		b, ok := e.(*godap.BreakpointEvent)

		return ok && b.Body.Breakpoint.Id == id
	}) {
		return out
	}

	return append(out, breakpointEvent(actionChanged, told))
}

// reconcile is [reconcile] of the connection's view with the session's
// breakpoints now; nothing before configurationDone. Under the event gate.
func (c *connection) reconcile() []godap.EventMessage {
	if !c.view.configured {
		return nil
	}

	return reconcile(c.view, c.client.ID, c.sess.Breakpoints(""))
}

// breakpointsEvent is an eyedbg/breakpoints event listing every client's
// breakpoints now.
func (c *connection) breakpointsEvent() *BreakpointsEvent {
	return &BreakpointsEvent{Event: event(CommandBreakpoints), Body: breakpointsBody(c.sess.Breakpoints(""))}
}

// relay runs events and writes what it returns, all under the event gate;
// false means the connection is broken (and now closed). Once the
// connection is leaving it sends nothing.
func (c *connection) relay(events func() []godap.EventMessage) bool {
	c.gate.Lock()
	defer c.gate.Unlock()

	if c.view.leaving {
		return true
	}

	for _, ev := range events() {
		if err := c.srv.Send(ev); err != nil {
			c.close()

			return false
		}
	}

	return true
}
