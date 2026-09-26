// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"fmt"
	"unicode/utf8"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/present"
)

// The facade's custom DAP messages (docs/adr/0014), for an editor extension:
// requests it answers itself (never forwarded to the adapter) and events it
// sends after configurationDone (eyedbg/session: before initialized).
const (
	// CommandLease is both a request (arguments [LeaseArguments], body
	// [LeaseBody]) and an event (body [LeaseBody], on every lease change).
	CommandLease = "eyedbg/lease"
	// CommandClients is both a request (no arguments, body [ClientsBody])
	// and an event (body [ClientsBody] without the lease, when a client
	// joins, connects or disconnects).
	CommandClients = "eyedbg/clients"
	// CommandBreakpoints is both a request (arguments
	// [BreakpointsArguments], body [BreakpointsBody]) and an event (body
	// [BreakpointsBody], when breakpoints changed).
	CommandBreakpoints = "eyedbg/breakpoints"
	// EventActivity is an event (body [ActivityBody]): what another client
	// did.
	EventActivity = "eyedbg/activity"
	// EventSession is an event (body [SessionBody]) a launch connection
	// sends once its launch started the session, before capabilities and
	// initialized (docs/adr/0019).
	EventSession = "eyedbg/session"
)

// eyedbg/lease actions.
const (
	LeaseActionStatus  = "status"
	LeaseActionTake    = "take"
	LeaseActionRelease = "release"
	LeaseActionRequest = "request"
	LeaseActionGrant   = "grant"
	LeaseActionPolicy  = "policy"
)

// eyedbg/breakpoints actions.
const (
	BreakpointsActionList   = "list"
	BreakpointsActionRemove = "remove"
)

// maxListedBreakpoints caps the breakpoints of one eyedbg/breakpoints body.
const maxListedBreakpoints = 1000

// LeaseArguments are eyedbg/lease's arguments: what the CLI's lease commands
// take. Action is one of status, take, release, request, grant (To) and
// policy (Policy); Force is as the CLI's --force, Message as 'lease request
// --message'.
type LeaseArguments struct {
	Action  string          `json:"action"`
	To      string          `json:"to,omitempty"`
	Policy  api.LeasePolicy `json:"policy,omitempty"`
	Force   bool            `json:"force,omitempty"`
	Message string          `json:"message,omitempty"`
}

// LeaseBody is eyedbg/lease's response and event body.
type LeaseBody struct {
	Lease api.LeaseInfo `json:"lease"`
}

// LeaseRequest is the eyedbg/lease request.
type LeaseRequest struct {
	godap.Request

	Arguments LeaseArguments `json:"arguments"`
}

// LeaseResponse is the eyedbg/lease response.
type LeaseResponse struct {
	godap.Response

	Body LeaseBody `json:"body"`
}

// LeaseEvent is the eyedbg/lease event.
type LeaseEvent struct {
	godap.Event

	Body LeaseBody `json:"body"`
}

// ClientsBody is eyedbg/clients' response and event body; the event has no
// lease.
type ClientsBody struct {
	Clients []api.ClientInfo `json:"clients"`
	Lease   *api.LeaseInfo   `json:"lease,omitempty"`
}

// ClientsRequest is the eyedbg/clients request.
type ClientsRequest struct {
	godap.Request
}

// ClientsResponse is the eyedbg/clients response.
type ClientsResponse struct {
	godap.Response

	Body ClientsBody `json:"body"`
}

// ClientsEvent is the eyedbg/clients event.
type ClientsEvent struct {
	godap.Event

	Body ClientsBody `json:"body"`
}

// BreakpointsArguments are eyedbg/breakpoints' arguments: Action list (the
// default) or remove (ID, Force as 'eyedbg bp rm ID --force').
type BreakpointsArguments struct {
	Action string `json:"action,omitempty"`
	ID     int    `json:"id,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

// BreakpointsBody is eyedbg/breakpoints' response and event body: every
// client's breakpoints, at most 1000 (More counts the rest), and for remove
// how many it removed.
type BreakpointsBody struct {
	Breakpoints []api.Breakpoint `json:"breakpoints"`
	More        int              `json:"more,omitempty"`
	Removed     int              `json:"removed,omitempty"`
}

// BreakpointsRequest is the eyedbg/breakpoints request.
type BreakpointsRequest struct {
	godap.Request

	Arguments BreakpointsArguments `json:"arguments"`
}

// BreakpointsResponse is the eyedbg/breakpoints response.
type BreakpointsResponse struct {
	godap.Response

	Body BreakpointsBody `json:"body"`
}

// BreakpointsEvent is the eyedbg/breakpoints event.
type BreakpointsEvent struct {
	godap.Event

	Body BreakpointsBody `json:"body"`
}

// ActivityBody is eyedbg/activity's body: the session event of another
// client, its text cut to 1000 characters.
type ActivityBody struct {
	Event api.Event `json:"event"`
}

// ActivityEvent is the eyedbg/activity event.
type ActivityEvent struct {
	godap.Event

	Body ActivityBody `json:"body"`
}

// SessionBody is eyedbg/session's body: the id of the session the launch
// started.
type SessionBody struct {
	SessionID string `json:"sessionId"`
}

// SessionEvent is the eyedbg/session event.
type SessionEvent struct {
	godap.Event

	Body SessionBody `json:"body"`
}

// RegisterMessages registers the facade's custom requests (with their
// responses) and events on codec, so that one codec decodes them in every
// direction.
func RegisterMessages(codec *godap.Codec) error {
	requests := []struct {
		command   string
		req, resp func() godap.Message
	}{
		{CommandLease, func() godap.Message { return &LeaseRequest{} }, func() godap.Message { return &LeaseResponse{} }},
		{CommandClients, func() godap.Message { return &ClientsRequest{} }, func() godap.Message { return &ClientsResponse{} }},
		{CommandBreakpoints, func() godap.Message { return &BreakpointsRequest{} }, func() godap.Message { return &BreakpointsResponse{} }},
	}

	for _, r := range requests {
		if err := codec.RegisterRequest(r.command, r.req, r.resp); err != nil {
			return fmt.Errorf("register %s: %w", r.command, err)
		}
	}

	events := []struct {
		name string
		ctor func() godap.Message
	}{
		{CommandLease, func() godap.Message { return &LeaseEvent{} }},
		{CommandClients, func() godap.Message { return &ClientsEvent{} }},
		{CommandBreakpoints, func() godap.Message { return &BreakpointsEvent{} }},
		{EventActivity, func() godap.Message { return &ActivityEvent{} }},
		{EventSession, func() godap.Message { return &SessionEvent{} }},
	}

	for _, e := range events {
		if err := codec.RegisterEvent(e.name, e.ctor); err != nil {
			return fmt.Errorf("register %s: %w", e.name, err)
		}
	}

	return nil
}

// activity reports whether ev is another client's action the connection of
// client self shows as eyedbg/activity: an execution request, a breakpoint,
// exception or lease change, or a client joining or leaving — never output
// or the adapter's own changes (no client).
func activity(ev *api.Event, self string) bool {
	if ev.Client == "" || ev.Client == self {
		return false
	}

	switch ev.Kind {
	case api.EventExec, api.EventBreakpoint, api.EventExceptions, api.EventLease, api.EventClient:
		return true
	case api.EventStarted, api.EventContinued, api.EventStopped, api.EventOutput, api.EventThread,
		api.EventExited, api.EventEnded:
		return false
	default:
		return false
	}
}

// activityEvent is ev as an eyedbg/activity event.
func activityEvent(ev *api.Event) *ActivityEvent {
	e := *ev
	if utf8.RuneCountInString(e.Text) > present.EventTextLimit {
		e.Text, e.Truncated = string([]rune(e.Text)[:present.EventTextLimit]), true
	}

	return &ActivityEvent{Event: event(EventActivity), Body: ActivityBody{Event: e}}
}

// leaseEvent is an eyedbg/lease event for lease.
func leaseEvent(lease api.LeaseInfo) *LeaseEvent {
	return &LeaseEvent{Event: event(CommandLease), Body: LeaseBody{Lease: lease}}
}

// clientsEvent is an eyedbg/clients event for clients.
func clientsEvent(clients []api.ClientInfo) *ClientsEvent {
	if clients == nil {
		clients = []api.ClientInfo{}
	}

	return &ClientsEvent{Event: event(CommandClients), Body: ClientsBody{Clients: clients}}
}

// breakpointsBody lists bps, at most maxListedBreakpoints of them.
func breakpointsBody(bps []api.Breakpoint) BreakpointsBody {
	if bps == nil {
		bps = []api.Breakpoint{}
	}

	if len(bps) > maxListedBreakpoints {
		return BreakpointsBody{Breakpoints: bps[:maxListedBreakpoints], More: len(bps) - maxListedBreakpoints}
	}

	return BreakpointsBody{Breakpoints: bps}
}
