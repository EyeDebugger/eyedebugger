---
status: proposed
date: 2026-09-24
decision-makers: Ijat (@ijat)
---

# Serve DAP to editors through `eyedbg dap`

## Context and Problem Statement

Phase 2 lets a human in an editor join an agent's live session (docs/DESIGN.md §9, ADR 0004).
DESIGN §9 assumed a per-session DAP facade on its own socket or pipe. But Node (VS Code's
extension host) can't open AF_UNIX sockets on Windows, DAP has no place for eyedbg's per-start
token, and ADR 0009 requires the lease check to be atomic with the send, inside
`internal/session`. How does an editor reach a session, and how is its DAP mapped onto the shared
session model?

## Decision Drivers

* One trust boundary: the per-user socket and token (DESIGN §6, §11); no new listener, no TCP.
* Windows is first-class.
* Policy in one place: the lease, state checks, the side-effect guard and breakpoint ownership
  (ADR 0009, 0010) must be the CLI's, not a copy.
* Any stdio DAP client, not only VS Code (nvim-dap, scripts).
* A late joiner sees the current state.
* Client-controlled input is parsed bounded and fail-closed, and what eyedbg decides on is exactly
  what the adapter receives (no parser differential).
* The CLI's behaviour doesn't change.

## Considered Options

* Transport: an `eyedbg dap` stdio bridge plus a `facade.open` connection switch · a per-session
  Unix socket or named pipe · a TCP loopback port · a token inside DAP arguments.
* Policy placement: session methods plus one passthrough policy table · policy in the facade · a
  raw proxy that rewrites seqs and intercepts a few commands.
* Events: follow the session's event log from an atomic join point · tap the adapter's raw events.
* Breakpoints: a session-level per-file replace · per-entry add and remove.
* Leaving: remove what the connection set · keep it.

## Decision Outcome

Chosen: the `eyedbg dap` bridge and connection switch, session methods with one policy table, the
event log from a join point, per-file replace, and cleanup on leave — the only combination that
keeps one trust boundary and one copy of every rule (see below).

**The switch.** `eyedbg dap [-s ID] [--as CLIENT]` connects like every command (never starting a
daemon), sends `hello` (the token) and `facade.open {sessionId, client}`. The result
`{sessionId, facadeVersion: 1}` is the connection's last JSON-RPC line: from then on it speaks DAP,
for good, as that client (identity is bound to the connection; it grants nothing a CLI call can't).
Errors (`INVALID_REQUEST` client, `NO_SESSION`, `SESSION_EXITED`) leave it speaking JSON-RPC. The
hand-over is lossless: `api.Conn` reads lines with a `bufio.Reader`, which is handed to the facade
with whatever the client pipelined after the request. The bridge only copies bytes; stdout carries
DAP only and every error goes to stderr. An older daemon answers `UNKNOWN_METHOD`, reported as
`VERSION_MISMATCH`. The native protocol stays 2.

**Framing.** Exactly `Content-Length: N\r\n\r\n` (1–10 digits), checked byte by byte (the first
byte that can't belong to the header closes the connection; a JSON-RPC line after the switch
does); content ≤ 1 MiB from editors (4 MiB from adapters, where the same reader now replaces
go-dap's unbounded one), checked before allocating; any violation closes the connection without a
reply — except an adapter message over 4 MiB with a valid header, which is skipped (its request
times out) so one huge value can't end the session. ≤ 32 requests in flight per connection (reading pauses beyond); each write has a 30 s
deadline (a client that stops reading is dropped). **Re-encode, never relay:** what reaches the
adapter is encoded from the go-dap value the policy decided on; fields go-dap 0.12 doesn't model
are dropped.

**Capabilities.** The adapter's, with forced on: `configurationDone`, hit conditions, logpoints
(emulated, ADR 0010), `terminate`; forced off: restart, data/instruction breakpoints, step back,
goto, restart frame, cancel, terminate threads, write memory, exception options and filter
options, stepping granularity, single-thread execution, step-in targets, clipboard context
(debugpy doesn't truncate there), `supportTerminateDebuggee`, `supportSuspendDebuggee` (so VS Code
offers Disconnect only). Exception
filters: `all` and `uncaught`, for the modes the adapter can serve.

**Requests.**

| class | requests | lease | needs stopped |
|---|---|---|---|
| handled | `initialize` (waits ≤ 30 s for a starting session; 1-based lines and columns, `path` only), `attach` (may only name its own session; `launch` refused), `configurationDone`, `disconnect`, `terminate` | `terminate` while live | — |
| execution | `continue`, `next`, `stepIn`, `stepOut`, `pause` (`Session.Exec`); `evaluate` in `repl` or no context, `setVariable`, `setExpression` (`exec eval`/`exec set` events: the expression or name, never a value) | yes | yes (pause: running) |
| own breakpoints | `setBreakpoints`, `setFunctionBreakpoints`, `setExceptionBreakpoints` | no | no |
| read | `threads`, `source`, `loadedSources`, `modules`, `breakpointLocations`, `readMemory`, `disassemble` | no | no |
| read | `stackTrace`, `scopes`, `variables`, `exceptionInfo`, `completions` (needs a 1-based `column`; less than 1 — including omitted, which arrives as 0 — is `INVALID_REQUEST` before the adapter sees it: debugpy would otherwise eval the raw text) | no | yes |
| guarded read | `evaluate` in `watch`, `hover`, `variables`, `clipboard` (sent as `watch`), with a `frameId` (without one debugpy runs statements); the driver's side-effect check first | no | yes |
| refused | everything else: `restart`, `stepBack`, `reverseContinue`, `restartFrame`, `goto`, `gotoTargets`, `stepInTargets`, `terminateThreads`, `writeMemory`, `dataBreakpointInfo`, `setDataBreakpoints`, `setInstructionBreakpoints`, `cancel`, `runInTerminal`, `startDebugging`, unknown commands, other `evaluate` contexts | — | — |

Execution requests and `initialize`/`attach`/`configurationDone` follow DAP's order; execution
requests and `terminate` only after `configurationDone`. A read whose capability the adapter didn't
declare (`loadedSources`, `modules`, `breakpointLocations`, `readMemory`, `disassemble`,
`exceptionInfo`, `completions`) is `UNSUPPORTED_BY_ADAPTER` and never sent: debugpy answers nothing
more after one. A connection's breakpoint, exception-mode and execution requests are handled in
arrival order; reads run concurrently.

**Breakpoints.** `setBreakpoints` replaces the client's own permanent breakpoints in that file
(canonical path = the CLI's `resolveSpec`/`realPath`): matched by requested line, then by the line
the adapter placed them on; matches keep their session breakpoint id (the DAP id), the rest are
added or removed, in one adapter round trip. One breakpoint per line per client (columns ignored);
per-entry refusals come back unverified with a message. Other owners' and run-until breakpoints
are never touched. `setExceptionBreakpoints`: `all` if named, else `uncaught`, else `none`, as the
client's own mode.

**Events** (from the event log, after the join point):

| log event | DAP |
|---|---|
| `stopped` | `stopped` (reason, thread, description, text, `allThreadsStopped`) |
| another client's `exec` continue/next/stepIn/stepOut/runUntil | `continued` (`allThreadsContinued`) |
| another client's `exec` eval/set | `invalidated` (variables), to clients that declared `supportsInvalidatedEvent` |
| the adapter's `continued` | `continued` |
| `output` | `output` (`logpoint` → `console`) |
| `thread`, `exited` | `thread`, `exited` |
| `ended` | `terminated` (then no more events) |
| the adapter changed one of the connection's breakpoints | `breakpoint` changed |
| another client removed one of them (`--force`) | `breakpoint` removed |
| anything else | nothing |

`configurationDone` is the join point: the session's state and the log's newest seq are read
together; the state is replayed (`stopped`; or `exited` and `terminated`), breakpoints the adapter
verified since their responses are announced, and the log is followed from there. Missed events
(the ring dropped them) give a console notice and the current state. **A response always precedes
the events its request causes** (DAP's rule for steps and pause): requests that change something
hold a per-connection event gate from their session call until their response is written, and
the follower writes each event under it.

**Errors.** `message` is the stable code; `body.error` = `{id, format: "message — hint",
variables: {code, holder (LEASE_HELD)}, showUser}` with `showUser` for execution requests. Ids are
fixed and append-only: 7001 `INVALID_REQUEST`, 7002 `NO_SESSION`, 7003 `NOT_STOPPED`, 7004
`NOT_RUNNING`, 7005 `SESSION_EXITED`, 7006 `LEASE_HELD`, 7007 `NOT_OWNER`, 7008
`UNSUPPORTED_BY_ADAPTER`, 7009 `SIDE_EFFECTS`, 7010 `ADAPTER_ERROR`, 7099 `INTERNAL`, 7000 any
other. Clients match `variables.code`.

**Leaving.** `disconnect` (whatever its arguments) answers, then the connection's breakpoints and
exception mode are removed, then it closes; closing the connection does the same. The session
keeps running and the lease stays with its holder. `terminate` ends the session under the rules of
`eyedbg stop` (an attached program is detached); it stays listed as exited until `eyedbg stop`.

**Versioning.** `version --json` of both binaries gains `"protocol": 2` and `"features": ["dap"]`.

### Consequences

* Good, because it works for VS Code (through the extension) and nvim-dap today, on all six
  targets, with no new listener, port or file; the token never reaches the editor.
* Good, because the CLI and editors share every rule: a refused execution request leaves no event
  and sends nothing, whoever sent it.
* Good, because a malformed frame or a bug (every facade goroutine recovers from panics) costs one
  connection, never the daemon.
* Bad, because the lease survives a human's disconnect until phase 2's presence work.
* Bad, because columns are ignored (one breakpoint per line per client) and only 1-based,
  path-based clients are served.
* Bad, because output printed before the join isn't replayed, and two editors with one identity
  remove each other's breakpoints when one closes (until presence).
* Bad, because the capabilities an editor sees are bounded by go-dap 0.12's struct (e.g. no ANSI
  styling or breakpoint modes).

### Confirmation

`internal/api` hand-over tests; `internal/dap` framing tables; `internal/session` policy, join and
replace tests; `internal/facade` scenario tests against the fake adapter (handshake, replay,
ordering, lease, breakpoints, duplicate JSON keys, cleanup, framing violations); the daemon's
pipelined `facade.open` test; and `internal/e2e` driving the real `eyedbg dap` with a spec-following
DAP client against netcoredbg and debugpy on Linux, macOS and Windows (CI).

## Pros and Cons of the Options

### `eyedbg dap` bridge and a `facade.open` connection switch

* Good, because the socket, the token and their checks stay the only way in.
* Good, because every DAP client can run a command as its adapter (stdio), and Node never opens the
  socket.
* Neutral, because it costs one small process per editor connection.
* Bad, because a connection can't go back to JSON-RPC (a second one is needed for the native API).

### A per-session Unix socket or named pipe

* Good, because an editor connects directly.
* Bad, because Node can't open AF_UNIX on Windows, DAP has no place for the token, and it adds a
  listener per session.

### A TCP loopback port

* Bad, because any local user's process can connect (DESIGN §11), and the token still has no place.

### A token inside DAP arguments

* Bad, because it needs a new listener and puts the token into editor configurations.

### Policy in session methods and one passthrough table

* Good, because the lease check stays atomic with the send (ADR 0009) and the reviewer reads the
  whole policy in one place (`internal/session/forward.go`).

### Policy in the facade, or a raw proxy with interception

* Bad, because the rules would drift from the CLI's, and a proxy can't make the lease atomic; it
  would decide on its own parse of bytes the adapter parses again.

### Following the event log

* Good, because editors see what the CLI sees: only published stops (the stop filter's hit counts
  and logpoints included), and other clients' actions.

### Tapping the adapter's raw events

* Bad, because emulated hit-count and logpoint stops would leak, and it needs a second fan-out.

### Per-file replace

* Good, because one diff and one adapter round trip keep ids stable when an editor re-sends a
  moved breakpoint.

### Per-entry add and remove

* Bad, because of N round trips, no atomicity, event noise and changing ids.

### Removing what the connection set

* Good, because every DAP adapter drops an editor's breakpoints at disconnect, the editor re-sends
  them on the next join, and an agent never keeps stopping at a departed human's breakpoints.
* Bad, because breakpoints a human set are not persistent across joins in the session model
  (ADR 0009's identity persists; its editor breakpoints don't).

### Keeping them

* Bad, because an agent would keep stopping at breakpoints nobody is watching.

## More Information

Amends DESIGN §2, §3 and §9; extends ADR 0004 and ADR 0009 without superseding them. Phase-2
follow-ups: presence and releasing the lease on the last disconnect, `lease request`, `eyedbg/*`
custom requests and events, replaying output before the join, and other owners' breakpoints shown
to editors as DAP `breakpoint` events (decided 2026-09-24), which must keep adopted foreign
breakpoints from being re-sent as the human's own.
