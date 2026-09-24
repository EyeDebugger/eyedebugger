// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package facade serves the Debug Adapter Protocol to editors joining a
// session (docs/DESIGN.md §9, ADR 0012). An editor reaches it through
// 'eyedbg dap', which authenticates to the daemon like every command and
// switches its connection to DAP with facade.open; [Serve] then speaks DAP
// on that connection as one client, for one session, until the editor
// disconnects.
//
// The facade holds no policy: it translates DAP requests into the session
// methods the CLI uses, so the lease, state checks, the side-effect guard
// and breakpoint ownership are the same for both.
//
//   - Handled here: initialize (answered once the session finished
//     starting, with the adapter's capabilities adjusted to what eyedbg
//     provides and refuses), attach (joins the connection's session;
//     launch is refused), configurationDone (the join point: the program's
//     state is replayed and the session's event log followed from there),
//     disconnect (leaves; the session keeps running), terminate (ends the
//     session under the lease, like 'eyedbg stop'), continue, next,
//     stepIn, stepOut and pause (execution requests, under the lease),
//     setBreakpoints and setFunctionBreakpoints (replace the client's own
//     breakpoints; ids are session breakpoint ids) and
//     setExceptionBreakpoints (the client's exception mode).
//   - Forwarded, re-encoded, through session.Session.Forward: reads
//     (threads, stack, scopes, variables, ...), evaluate in a read context
//     (behind the side-effect guard), and evaluate in the repl context,
//     setVariable and setExpression as execution requests.
//   - Refused: everything else (restart, stepping back, goto, memory
//     writes, data breakpoints, cancel, ...).
//
// Events come from the session's event log, not the adapter: stops,
// resumes by other clients, output, threads, the end, and adapter changes
// to the connection's own breakpoints. A request's response is always
// written before the events it causes (the event gate). When the
// connection ends for any reason, the breakpoints and the exception mode it
// set are removed; the control lease stays with whoever holds it.
//
// Every goroutine recovers from panics: a bug closes one connection, never
// the daemon. Logs name commands, seqs and codes, never debuggee data.
package facade
