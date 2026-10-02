// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "time"

// Group methods act on every member of a group: the sessions started with
// that group (docs/adr/0020, D1), live or exited but not lost. Members that
// join after a request started are not part of it.
const (
	// MethodGroupWait waits until any member stops or ends (wait-any).
	MethodGroupWait = "group.wait"
	// MethodGroupEvents reads the members' event logs merged into one
	// stream, with a cursor to resume from.
	MethodGroupEvents = "group.events"
)

// GroupWaitParams are the params of [MethodGroupWait].
type GroupWaitParams struct {
	DumpSpec

	Client string `json:"client,omitempty"`
	Group  string `json:"group"`
	// New ignores members already stopped: only a stop after the call (or a
	// member ending) answers. Without it a member already stopped answers
	// at once, the one stopped longest.
	New  bool     `json:"new,omitempty"`
	Wait Duration `json:"wait,omitempty"`
}

// GroupStop is a stopped member, in a [GroupWaitResult].
type GroupStop struct {
	SessionID string    `json:"sessionId"`
	Service   string    `json:"service,omitempty"`
	Reason    string    `json:"reason"`
	ThreadID  int       `json:"threadId,omitempty"`
	StoppedAt time.Time `json:"stoppedAt"`
}

// GroupWaitResult is the result of [MethodGroupWait]. Session is the member
// that answered (nil when the wait timed out or every member had already
// ended); Stopped lists the other members stopped when it returned, the one
// stopped longest first.
type GroupWaitResult struct {
	Group   string    `json:"group"`
	Session *Snapshot `json:"session,omitempty"`
	// Service is Session's compose service, when it has one.
	Service string      `json:"service,omitempty"`
	Stopped []GroupStop `json:"stopped"`
	// TimedOut means no member stopped or ended in time.
	TimedOut bool `json:"timedOut,omitempty"`
	// Ended means every member has ended.
	Ended bool `json:"ended,omitempty"`
}

// MaxGroupCursor is the most members a group cursor names, and so the most
// members [MethodGroupEvents] follows.
const MaxGroupCursor = 64

// GroupEventsParams are the params of [MethodGroupEvents].
type GroupEventsParams struct {
	Client string `json:"client,omitempty"`
	Group  string `json:"group"`
	// Since is a cursor from an earlier result: comma-separated
	// "SESSION:SEQ" entries, each member at most once; a member it doesn't
	// name starts at its first event, an id that is no member is ignored.
	// Empty: from each member's first event.
	Since string `json:"since,omitempty"`
	// Newest keeps the newest Limit events instead of the oldest.
	Newest bool        `json:"newest,omitempty"`
	Limit  int         `json:"limit,omitempty"`
	Kinds  []EventKind `json:"kinds,omitempty"`
	// Wait blocks up to this long until a matching event exists on any
	// member; 0 returns at once.
	Wait   Duration `json:"wait,omitempty"`
	Budget int      `json:"budget,omitempty"`
}

// GroupEvent is one member's event in a [GroupEventsResult].
type GroupEvent struct {
	SessionID string `json:"sessionId"`
	Service   string `json:"service,omitempty"`
	Event     Event  `json:"event"`
}

// GroupEventsResult is the result of [MethodGroupEvents]: events ordered by
// time, then session id, then seq — never reordered within a member.
type GroupEventsResult struct {
	Events []GroupEvent `json:"events"`
	// Cursor is the next request's Since: it never skips a matching event
	// this result left out (with Newest it may repeat some instead).
	Cursor string `json:"cursor"`
	// More counts matching events not returned.
	More int `json:"more,omitempty"`
	// Dropped counts events after the cursor the members' logs no longer
	// hold.
	Dropped int `json:"dropped,omitempty"`
	// TimedOut means the wait ended with no matching event.
	TimedOut bool `json:"timedOut,omitempty"`
}
