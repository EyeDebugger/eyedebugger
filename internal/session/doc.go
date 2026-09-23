// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package session owns debug sessions (docs/DESIGN.md §3, ADR 0009): one
// debuggee driven through one DAP adapter process, shared by any number of
// clients.
//
// A [Manager] starts, lists and stops sessions, persists their metadata
// through a [Store] so the sessions of a daemon that crashed show up as
// lost, and records each session's control events. A [Session] keeps:
//
//   - clients: who used it (a request names its client; see api.ParseClient);
//   - breakpoints, each owned by the client that added it; per file they are
//     merged into one DAP source breakpoint per line (slotsFor);
//   - the control lease: execution-changing requests (continue, steps, pause,
//     run-until, terminate) run one at a time under execMu, checking the
//     state, then the lease, before any side effect;
//   - the event log: a bounded ring of everything that happened, with one
//     sequence number, that output and snapshots are views of and that
//     clients read or long-poll;
//   - stop snapshots: where the program stopped, its output since it resumed
//     and, on request, locals (with changes since the previous stop) and the
//     stack.
//
// Lock order: execMu, syncMu, captureMu, mu, then the event log's and the
// recorder's own locks; the adapter's event goroutine takes only mu and
// below.
package session
