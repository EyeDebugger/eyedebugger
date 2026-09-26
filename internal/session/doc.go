// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package session owns debug sessions (docs/DESIGN.md §3, ADRs 0009 and
// 0010): one debuggee driven through one DAP adapter process, shared by any
// number of clients. A session launches its program, attaches to a running
// one (mode attach), or debugs a test run (mode test: the driver's test
// command runs first, and the session attaches to the test host it names).
//
// A [Manager] starts, lists and stops sessions, persists their metadata
// through a [Store] so the sessions of a daemon that crashed show up as
// lost, and records each session's control events. A [Session] keeps:
//
//   - clients: who used it (a request names its client; see api.ParseClient);
//   - breakpoints, each owned by the client that added it; per file they are
//     merged into one DAP source breakpoint per line, and function
//     breakpoints into one per name (slotsFor). Anchors resolve to a line
//     when added (anchor.go); hit counts and logpoints are emulated, and
//     the conditions of a line whose breakpoints' conditions differ are
//     each evaluated, by a stop filter that decides, under execMu, whether
//     a breakpoint stop is published — naming the breakpoints it is for —
//     or continued (emulate.go);
//   - exception modes, one per client, sent to the adapter as the union of
//     their filters (exceptions.go);
//   - the control lease: execution-changing requests (continue, steps, pause,
//     run-until, terminate, detach, set, eval with side effects) run one at a
//     time under execMu, checking the state, then the lease, before any side
//     effect;
//   - the event log: a bounded ring of everything that happened, with one
//     sequence number, that output and snapshots are views of and that
//     clients read or long-poll;
//   - stop snapshots: where the program stopped, its output since it resumed
//     and, on request, locals (with changes since the previous stop) and the
//     stack.
//
// Editors join through the DAP facade (internal/facade, ADR 0012), which
// holds no policy of its own: it waits with [Session.Ready], joins at
// [Session.JoinPoint] and follows the event log with [Session.Follow], and
// acts through the rules the CLI gets — [Session.Exec] for execution
// requests, [Session.Forward] for every other adapter request (one table
// decides what is read, leased or refused), [Session.ReplaceBreakpoints]
// and [Session.ReplaceFunctionBreakpoints] for its client's own
// breakpoints (editor breakpoints: a replace never removes the client's
// CLI ones). [Session.Connect] and [Presence.Leave] count each client's
// open editor connections (presence, ADR 0014): when a client's last one
// closes, its editor breakpoints are removed, the exception mode an editor
// of it set is reset, its lease request dropped and its lease released —
// after a grace for a restart, and never once one of its connections came
// back.
//
// Lock order: execMu, syncMu, captureMu, mu, then the event log's and the
// recorder's own locks; the adapter's event goroutine takes only mu and
// below.
package session
