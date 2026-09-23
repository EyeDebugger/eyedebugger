// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"time"
)

// MethodEvents reads a session's event log, optionally waiting for new
// events.
const MethodEvents = "events"

// EventKind is what an [Event] records.
type EventKind string

// Event kinds and the [Event] fields each one sets.
const (
	// EventStarted: Client (who started it), Program, Lease.
	EventStarted EventKind = "started"
	// EventClient: Client, on its first request to the session.
	EventClient EventKind = "client"
	// EventLease: Client (who acted), Action (take, auto, force, grant,
	// release, policy), Lease (after), Previous (the holder before).
	EventLease EventKind = "lease"
	// EventExec: Client, Action (continue, next, stepIn, stepOut, pause,
	// runUntil), ThreadID when one was given.
	EventExec EventKind = "exec"
	// EventContinued: ThreadID. The adapter resumed the program without an
	// execution request in flight.
	EventContinued EventKind = "continued"
	// EventStopped: Stop.
	EventStopped EventKind = "stopped"
	// EventOutput: Category, Text, Truncated.
	EventOutput EventKind = "output"
	// EventBreakpoint: Action (added, removed, changed), Client (for added
	// and removed), Breakpoint (as it was then).
	EventBreakpoint EventKind = "breakpoint"
	// EventThread: Reason (started, exited), ThreadID.
	EventThread EventKind = "thread"
	// EventExited: ExitCode.
	EventExited EventKind = "exited"
	// EventEnded: Reason, Client when a client stopped the session.
	EventEnded EventKind = "ended"
)

// MaxEventsLimit is the most events one [MethodEvents] request returns.
const MaxEventsLimit = 1000

// EventKindNames returns every event kind, comma-separated, for messages.
func EventKindNames() string {
	kinds := EventKinds()

	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}

	return strings.Join(names, ", ")
}

// EventKinds returns every event kind.
func EventKinds() []EventKind {
	return []EventKind{
		EventStarted, EventClient, EventLease, EventExec, EventContinued, EventStopped,
		EventOutput, EventBreakpoint, EventThread, EventExited, EventEnded,
	}
}

// Event is one entry of a session's event log. Seq increases by one per
// event from 1; Kind decides which other fields are set.
type Event struct {
	Seq        int         `json:"seq"`
	Time       time.Time   `json:"time"`
	Kind       EventKind   `json:"kind"`
	Client     string      `json:"client,omitempty"`
	Action     string      `json:"action,omitempty"`
	Program    string      `json:"program,omitempty"`
	Lease      *LeaseInfo  `json:"lease,omitempty"`
	Previous   string      `json:"previous,omitempty"`
	Stop       *StopInfo   `json:"stop,omitempty"`
	ThreadID   int         `json:"threadId,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Category   string      `json:"category,omitempty"`
	Text       string      `json:"text,omitempty"`
	Truncated  bool        `json:"truncated,omitempty"`
	Breakpoint *Breakpoint `json:"breakpoint,omitempty"`
	ExitCode   *int        `json:"exitCode,omitempty"`
}

// EventsParams are the params of [MethodEvents].
type EventsParams struct {
	SessionRef

	// Since returns events with Seq > Since; -1 means "from now" (only
	// useful with Wait).
	Since int `json:"since"`
	// Newest keeps the newest Limit events after Since instead of the
	// oldest.
	Newest bool `json:"newest,omitempty"`
	// Limit is the most events to return: 0 means 100; at most
	// [MaxEventsLimit].
	Limit int         `json:"limit,omitempty"`
	Kinds []EventKind `json:"kinds,omitempty"`
	// Wait blocks up to this long until a matching event exists; 0 returns
	// at once.
	Wait Duration `json:"wait,omitempty"`
	// Budget caps the result in tokens (about 4 characters each); 0 means
	// only the hard cap (512 KiB).
	Budget int `json:"budget,omitempty"`
}

// EventsResult is the result of [MethodEvents].
type EventsResult struct {
	Events []Event `json:"events"`
	// Latest is the newest seq in the log (0: none yet).
	Latest int `json:"latest"`
	// Dropped counts events after Since that the log no longer holds.
	Dropped int `json:"dropped,omitempty"`
	// More counts matching events not returned: newer ones, or older ones
	// with Newest.
	More int `json:"more,omitempty"`
	// TimedOut means the wait ended with no matching event.
	TimedOut bool `json:"timedOut,omitempty"`
}
