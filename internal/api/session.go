// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"time"
)

// Session methods (docs/DESIGN.md §4). Every method that acts on a session
// takes a SessionID; empty means "the only session".
const (
	MethodSessionStart  = "session.start"
	MethodSessionList   = "session.list"
	MethodSessionStatus = "session.status"
	MethodSessionStop   = "session.stop"
	MethodExec          = "exec"
	MethodWait          = "wait"
	MethodBreakpointAdd = "bp.add"
	MethodBreakpointLs  = "bp.list"
	MethodBreakpointRm  = "bp.remove"
	MethodStack         = "stack"
	MethodVars          = "vars"
	MethodEval          = "eval"
	MethodOutput        = "output"
	MethodRunUntil      = "runUntil"
)

// SessionState is where a session is in its life.
type SessionState string

// Session states.
const (
	StateStarting SessionState = "starting"
	StateRunning  SessionState = "running"
	StateStopped  SessionState = "stopped"
	StateExited   SessionState = "exited"
)

// LaunchSpec is what to debug. Paths are absolute (the CLI resolves them).
type LaunchSpec struct {
	// Project is a project file or directory to build (driver-specific).
	Project string `json:"project,omitempty"`
	// Program is an already-built program; the build is skipped.
	Program     string            `json:"program,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	NoBuild     bool              `json:"noBuild,omitempty"`
	StopOnEntry bool              `json:"stopOnEntry,omitempty"`
}

// StartParams are the params of [MethodSessionStart].
type StartParams struct {
	LaunchSpec

	DumpSpec

	Lang        string           `json:"lang"`
	Breakpoints []BreakpointSpec `json:"breakpoints,omitempty"`
	// Wait is how long to wait for a first stop when one is expected
	// (stop-on-entry or breakpoints); 0 returns as soon as it runs.
	Wait Duration `json:"wait,omitempty"`
}

// SessionRef names a session.
type SessionRef struct {
	SessionID string `json:"sessionId,omitempty"`
}

// Ref returns r; params that embed a SessionRef inherit it.
func (r SessionRef) Ref() SessionRef { return r }

// StopInfo describes why the program stopped.
type StopInfo struct {
	Reason      string `json:"reason"`
	ThreadID    int    `json:"threadId"`
	Description string `json:"description,omitempty"`
	Text        string `json:"text,omitempty"`
}

// SessionInfo summarizes a session.
type SessionInfo struct {
	ID        string       `json:"id"`
	Lang      string       `json:"lang"`
	Program   string       `json:"program"`
	State     SessionState `json:"state"`
	PID       int          `json:"pid,omitempty"`
	CreatedAt time.Time    `json:"createdAt"`
	Stop      *StopInfo    `json:"stop,omitempty"`
	ExitCode  *int         `json:"exitCode,omitempty"`
	EndReason string       `json:"endReason,omitempty"`
}

// What a snapshot can include beyond the stop location ([DumpSpec]).
const (
	DumpLocals  = "locals"  // every local of frame 0
	DumpChanged = "changed" // locals of frame 0 that changed since the previous stop
	DumpStack   = "stack"   // the stopped thread's top frames
	DumpNone    = "none"
)

// DumpSpec asks for more in a snapshot. Budget caps the variables in it, in
// tokens (about 4 characters each); 0 means no cap.
type DumpSpec struct {
	Dump   []string `json:"dump,omitempty"`
	Budget int      `json:"budget,omitempty"`
}

// Wants reports whether d asks for what.
func (d DumpSpec) Wants(what string) bool { return slices.Contains(d.Dump, what) }

// StatusParams are the params of [MethodSessionStatus].
type StatusParams struct {
	SessionRef
	DumpSpec
}

// Snapshot is a session's state plus, when stopped, where; and what a
// [DumpSpec] asked for.
type Snapshot struct {
	Session SessionInfo  `json:"session"`
	Frame   *Frame       `json:"frame,omitempty"`
	Source  []SourceLine `json:"source,omitempty"`
	// TimedOut means the wait ended before the program stopped or exited;
	// it is still running.
	TimedOut bool `json:"timedOut,omitempty"`
	// Output is what the program printed since it last resumed (the last
	// few chunks; OutputOmitted counts the older ones left out).
	Output        []OutputLine `json:"output,omitempty"`
	OutputOmitted int          `json:"outputOmitted,omitempty"`
	Stack         []Frame      `json:"stack,omitempty"`
	// Reached is set by run-until: whether the program stopped at the
	// target (false: it stopped elsewhere first, or exited).
	Reached *bool `json:"reached,omitempty"`
	// Target is run-until's location, as the adapter placed it.
	Target  *BreakpointSpec `json:"target,omitempty"`
	Locals  *Scope          `json:"locals,omitempty"`
	Changes *Changes        `json:"changes,omitempty"`
}

// Changes are the locals of frame 0 that differ from the previous stop.
type Changes struct {
	// NewFrame means the previous stop was in another function (or there
	// was none): every local is new and listed.
	NewFrame bool  `json:"newFrame,omitempty"`
	Vars     []Var `json:"vars"`
	// More and Truncated are as in [Scope].
	More      int  `json:"more,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

// SourceLine is one line of source text.
type SourceLine struct {
	Line    int    `json:"line"`
	Text    string `json:"text"`
	Current bool   `json:"current,omitempty"`
}

// Frame is one stack frame. Index 0 is the innermost.
type Frame struct {
	Index  int    `json:"index"`
	Name   string `json:"name"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
}

// ExecParams are the params of [MethodExec].
type ExecParams struct {
	SessionRef

	// Kind is continue, next, stepIn, stepOut or pause.
	DumpSpec

	Kind     string   `json:"kind"`
	ThreadID int      `json:"threadId,omitempty"`
	Wait     Duration `json:"wait,omitempty"`
}

// RunUntilParams are the params of [MethodRunUntil]: continue to a location
// (a temporary breakpoint, removed afterwards).
type RunUntilParams struct {
	SessionRef
	BreakpointSpec
	DumpSpec

	ThreadID int      `json:"threadId,omitempty"`
	Wait     Duration `json:"wait,omitempty"`
}

// WaitParams are the params of [MethodWait].
type WaitParams struct {
	SessionRef
	DumpSpec

	// AfterStops waits for a stop beyond this count; -1 means "the next one
	// from now", and a stopped program returns at once for any value lower
	// than its current count.
	AfterStops int      `json:"afterStops"`
	Wait       Duration `json:"wait,omitempty"`
}

// BreakpointSpec asks for a breakpoint at File:Line, optionally only when
// Condition (an expression in the program's language) is true.
type BreakpointSpec struct {
	File      string `json:"file"`
	Line      int    `json:"line"`
	Condition string `json:"condition,omitempty"`
}

// Breakpoint is a breakpoint as the adapter resolved it.
type Breakpoint struct {
	ID            int    `json:"id"`
	File          string `json:"file"`
	RequestedLine int    `json:"requestedLine"`
	// Line is where the adapter placed it (may differ from RequestedLine).
	Line      int    `json:"line"`
	Verified  bool   `json:"verified"`
	Message   string `json:"message,omitempty"`
	Condition string `json:"condition,omitempty"`
	// Temporary marks run-until's breakpoint, removed once the program
	// stops or exits.
	Temporary bool `json:"temporary,omitempty"`
}

// BreakpointAddParams are the params of [MethodBreakpointAdd].
type BreakpointAddParams struct {
	SessionRef
	BreakpointSpec
}

// BreakpointRemoveParams are the params of [MethodBreakpointRm]; ID 0
// removes all.
type BreakpointRemoveParams struct {
	SessionRef

	ID int `json:"id"`
}

// BreakpointRemoveResult is the result of [MethodBreakpointRm].
type BreakpointRemoveResult struct {
	Removed int `json:"removed"`
}

// StackParams are the params of [MethodStack].
type StackParams struct {
	SessionRef

	ThreadID int `json:"threadId,omitempty"`
	Levels   int `json:"levels,omitempty"`
}

// VarsParams are the params of [MethodVars].
type VarsParams struct {
	SessionRef

	Frame int `json:"frame"`
	Depth int `json:"depth,omitempty"`
	// Expand shows only the variable at this path (e.g. order.Items[0]).
	Expand string `json:"expand,omitempty"`
	// Changed shows only the locals of frame 0 that changed since the
	// previous stop.
	Changed bool `json:"changed,omitempty"`
	// Budget is as in [DumpSpec].
	Budget int `json:"budget,omitempty"`
}

// Scope is a group of variables (e.g. Locals).
type Scope struct {
	Name string `json:"name"`
	Vars []Var  `json:"vars,omitempty"`
	// More is how many variables were left out.
	More      int  `json:"more,omitempty"`
	Expensive bool `json:"expensive,omitempty"`
	// Truncated means the token budget cut this scope short: some
	// variables or members were left out or not expanded.
	Truncated bool `json:"truncated,omitempty"`
}

// Var is a variable; Children are filled only up to the requested depth.
type Var struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Value       string `json:"value"`
	HasChildren bool   `json:"hasChildren,omitempty"`
	Children    []Var  `json:"children,omitempty"`
	More        int    `json:"more,omitempty"`
	// Change is "new" or "changed" in a [Changes] list; Previous is the
	// value at the previous stop.
	Change   string `json:"change,omitempty"`
	Previous string `json:"previous,omitempty"`
}

// EvalParams are the params of [MethodEval].
type EvalParams struct {
	SessionRef

	Expression string `json:"expression"`
	Frame      int    `json:"frame"`
}

// EvalResult is the result of [MethodEval].
type EvalResult struct {
	Expression  string `json:"expression"`
	Value       string `json:"value"`
	Type        string `json:"type,omitempty"`
	HasChildren bool   `json:"hasChildren,omitempty"`
}

// OutputParams are the params of [MethodOutput].
type OutputParams struct {
	SessionRef

	Since int `json:"since,omitempty"`
	Tail  int `json:"tail,omitempty"`
}

// OutputLine is a chunk of program output.
type OutputLine struct {
	Seq      int    `json:"seq"`
	Category string `json:"category"`
	Text     string `json:"text"`
}
