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
//     setBreakpoints and setFunctionBreakpoints (replace the client's
//     editor breakpoints, but not those the connection set under another
//     path of the same file; at most 1000 entries; ids are session
//     breakpoint ids),
//     setExceptionBreakpoints (the client's exception mode) and the custom
//     eyedbg/* requests.
//   - Forwarded, re-encoded, through session.Session.Forward: reads
//     (threads, stack, scopes, variables, ...), evaluate in a read context
//     (behind the side-effect guard), and evaluate in the repl context,
//     setVariable and setExpression as execution requests.
//   - Refused: everything else (restart, stepping back, goto, memory
//     writes, data breakpoints, cancel, ...).
//
// Events come from the session's event log, not the adapter: stops,
// resumes by other clients, output, threads, the end, and changes to the
// breakpoints the editor knows. A request's response is always written
// before the events it causes (the event gate).
//
// Presence and leaving (ADR 0014): a connection is its client's presence
// in the session (session.Session.Connect). When a client's last
// connection closes, the session removes its editor breakpoints, resets
// the exception mode an editor of it set, drops its lease request and
// releases its lease — after [Config.RestartGrace] when the editor
// disconnected to restart, and not at all if one of its connections came
// back meanwhile.
//
// Shared breakpoints: the other clients' line breakpoints (and the
// client's own set outside the editor) are announced as DAP breakpoint
// events at column 1, the mark (mirrors.go); VS Code adopts them as the
// user's own and re-sends them, so setBreakpoints classifies each entry:
// a copy (marked, or plain where this connection announced one) is
// answered with the owner's breakpoint and never becomes the client's, a
// copy of a breakpoint that is gone is retracted, and the rest are the
// client's own. Copies are tracked by the source path the editor holds
// them under (new ones go under the path it set its own breakpoints in the
// file under), since an editor may have a file under several paths.
// Removing a copy only hides it on this connection; editing
// it makes a breakpoint of the client's own. Every change is reconciled
// against what the connection told, and the copies are retracted before
// the disconnect response and before terminated.
//
// Custom messages (custom.go): the requests eyedbg/lease (the lease
// commands), eyedbg/clients and eyedbg/breakpoints (list, or remove as
// 'eyedbg bp rm'), answered here and never forwarded, and the events
// eyedbg/lease, eyedbg/clients, eyedbg/breakpoints and eyedbg/activity
// (other clients' actions), for an editor extension.
//
// Output replay: configurationDone replays the newest output the session
// holds (at most 200 chunks, 64 KiB) before the program's state.
//
// Every goroutine recovers from panics: a bug closes one connection, never
// the daemon. Logs name commands, seqs and codes, never debuggee data.
package facade
