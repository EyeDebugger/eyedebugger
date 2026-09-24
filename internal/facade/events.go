// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"strconv"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Breakpoint event actions (the log's, and DAP's reasons).
const (
	actionChanged = "changed"
	actionRemoved = "removed"
)

// logpointCategory is the output category of the session's emulated
// logpoints; DAP has no such category.
const logpointCategory = "logpoint"

// followState is what translating the event log needs besides the events.
type followState struct {
	// self is the connection's client id.
	self string
	// invalidated: the client declared supportsInvalidatedEvent.
	invalidated bool
	// owns reports whether a breakpoint id is one of the connection's.
	owns func(id int) bool
	// lastThread is the thread of the last stop seen.
	lastThread int
}

func event(name string) godap.Event { return godap.Event{Event: name} }

// translate turns one session event into the DAP events the connection's
// client gets (docs/adr/0012): stops, resumes (another client's execution
// request, or the adapter's), output, threads, the end, and changes to the
// connection's own breakpoints made by the adapter or forced by another
// client. Its own execution requests are covered by their responses.
func translate(ev *api.Event, st *followState) []godap.EventMessage {
	switch ev.Kind {
	case api.EventStopped:
		return stoppedEvent(ev.Stop, st)
	case api.EventExec:
		return execEvent(ev, st)
	case api.EventContinued:
		return []godap.EventMessage{continuedEvent(ev.ThreadID, st)}
	case api.EventOutput:
		category := ev.Category
		if category == logpointCategory {
			category = "console"
		}

		return []godap.EventMessage{&godap.OutputEvent{Event: event("output"), Body: godap.OutputEventBody{Category: category, Output: ev.Text}}}
	case api.EventThread:
		return []godap.EventMessage{&godap.ThreadEvent{Event: event("thread"), Body: godap.ThreadEventBody{Reason: ev.Reason, ThreadId: ev.ThreadID}}}
	case api.EventExited:
		if ev.ExitCode == nil {
			return nil
		}

		return []godap.EventMessage{&godap.ExitedEvent{Event: event("exited"), Body: godap.ExitedEventBody{ExitCode: *ev.ExitCode}}}
	case api.EventEnded:
		return []godap.EventMessage{&godap.TerminatedEvent{Event: event("terminated")}}
	case api.EventBreakpoint:
		return breakpointEvent(ev, st)
	case api.EventStarted, api.EventClient, api.EventLease, api.EventExceptions:
		return nil
	default:
		return nil
	}
}

func stoppedEvent(stop *api.StopInfo, st *followState) []godap.EventMessage {
	if stop == nil {
		return nil
	}

	st.lastThread = stop.ThreadID

	return []godap.EventMessage{&godap.StoppedEvent{Event: event("stopped"), Body: godap.StoppedEventBody{
		Reason: stop.Reason, ThreadId: stop.ThreadID, Description: stop.Description, Text: stop.Text,
		AllThreadsStopped: stop.AllThreadsStopped,
	}}}
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

// breakpointEvent reports what the adapter changed about one of the
// connection's breakpoints (e.g. verified once its module loaded), and
// another client's forced removal of one.
func breakpointEvent(ev *api.Event, st *followState) []godap.EventMessage {
	b := ev.Breakpoint
	if b == nil || b.Owner != st.self || !st.owns(b.ID) {
		return nil
	}

	switch {
	case ev.Action == actionChanged && ev.Client == "":
		return []godap.EventMessage{&godap.BreakpointEvent{
			Event: event("breakpoint"), Body: godap.BreakpointEventBody{Reason: actionChanged, Breakpoint: dapBreakpoint(b)},
		}}
	case ev.Action == actionRemoved && ev.Client != st.self:
		return []godap.EventMessage{&godap.BreakpointEvent{
			Event: event("breakpoint"), Body: godap.BreakpointEventBody{Reason: actionRemoved, Breakpoint: godap.Breakpoint{Id: b.ID}},
		}}
	default:
		return nil
	}
}

// resync is what a client that missed events gets: a console notice, then
// the program's state now.
func resync(dropped int, info api.SessionInfo, st *followState) []godap.EventMessage {
	out := []godap.EventMessage{&godap.OutputEvent{Event: event("output"), Body: godap.OutputEventBody{
		Category: "console", Output: "eyedbg: " + strconv.Itoa(dropped) + " session events were missed\n",
	}}}

	switch info.State {
	case api.StateStopped:
		out = append(out, stoppedEvent(info.Stop, st)...)
	case api.StateRunning:
		out = append(out, continuedEvent(0, st))
	case api.StateStarting, api.StateExited, api.StateLost:
	}

	return out
}

// dapBreakpoint is b as DAP shows it: its message joins the adapter's and
// eyedbg's notes.
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
// response of the request that caused it, and a breakpoint's owner is
// decided once that request recorded it.
func (c *connection) follow(ctx context.Context, since int, st *followState) {
	for {
		res := c.sess.Follow(ctx, since)
		if ctx.Err() != nil || len(res.Events) == 0 {
			return
		}

		if res.Dropped > 0 && !c.relay(func() []godap.EventMessage { return resync(res.Dropped, c.sess.Info(), st) }) {
			return
		}

		for i := range res.Events {
			ev := &res.Events[i]
			if !c.relay(func() []godap.EventMessage { return c.translate(ev, st) }) || ev.Kind == api.EventEnded {
				return
			}
		}

		since = res.Events[len(res.Events)-1].Seq
	}
}

// translate is [translate] for the connection: another client's removal of
// one of its breakpoints also drops it from the connection's.
func (c *connection) translate(ev *api.Event, st *followState) []godap.EventMessage {
	out := translate(ev, st)

	if ev.Kind == api.EventBreakpoint && ev.Action == actionRemoved && ev.Client != c.client.ID && ev.Breakpoint != nil {
		c.forget(ev.Breakpoint.ID)
	}

	return out
}

// relay runs events and writes what it returns, all under the event gate;
// false means the connection is broken (and now closed).
func (c *connection) relay(events func() []godap.EventMessage) bool {
	c.gate.Lock()
	defer c.gate.Unlock()

	for _, ev := range events() {
		if err := c.srv.Send(ev); err != nil {
			c.close()

			return false
		}
	}

	return true
}
