---
status: proposed
date: 2026-09-24
decision-makers: Ijat (@ijat)
---

# Client identity, breakpoint ownership, the control lease and the event log

## Context and Problem Statement

ADR 0004 made the daemon the owner of every session and promised the model phase 2 needs: client
identity, breakpoint ownership, a control lease and an event log (docs/DESIGN.md §3, §9). Milestone
4 builds that model while the only client is the stateless CLI, whose every call is a fresh
process on a fresh connection. How does a request say who sent it, how do several clients'
breakpoints share one adapter, how is execution control arbitrated, what does a late joiner read to
catch up, and what is kept on disk?

## Decision Drivers

* The CLI is stateless: one request per connection, often from a fresh shell per command (agent
  harnesses run each command in a new shell, so a parent-pid based identity would change per call).
* netcoredbg keeps one active breakpoint per line and remaps a file's breakpoints by line: two
  entries for one line in one `setBreakpoints` orphan one of them (its
  `src/debugger/breakpoints_line.cpp`).
* netcoredbg sends `continued` for our own continue and step requests, before answering them.
* Execution-changing requests must be serialized per session (docs/DESIGN.md §3); a check of the
  lease that is not atomic with the send lets two clients both pass it.
* Responses travel as single lines of at most 1 MiB (`internal/api/wire.go`); program output is
  unbounded.
* Debuggee data can hold secrets (AGENTS.md rule 9) and no redaction exists yet.
* The hello exchange is frozen across protocol versions.

## Considered Options

Identity transport:

* Per request: a `client` field in every session method's params (chosen).
* In `hello`, per connection.
* A separate `client.identify` request.

Default identity:

* `agent` for anyone who doesn't say (chosen).
* `agent:<ppid>`.
* `human` when stdin is a terminal.

Sharing a line:

* One DAP source breakpoint per line, merged from every client's breakpoints there (chosen).
* One DAP entry per client breakpoint, lines repeated.
* Refuse a second client's breakpoint on an occupied line.

Lease enforcement:

* In `internal/session`, under a per-session execution lock, in the order state check → lease →
  side effects → send (chosen).
* In the daemon's request handlers.

Event log:

* One bounded ring per session with one global `seq`, output included; `output` and snapshot output
  are views of it; delivered by long-poll (chosen).
* A separate output buffer beside a control-event ring sharing a `seq`.
* Push subscriptions.

Persistence:

* Metadata and recordings in the private runtime directory (chosen).
* In `EYEDBG_DATA_DIR` or the user cache directory.

## Decision Outcome

**Identity.** Every session method's params carry `client` (`SessionRef.Client`,
`StartParams.Client`), taken from `--as`, else `$EYEDBG_CLIENT`, else empty. The daemon parses it
(`api.ParseClient`): `KIND[:NAME]` with KIND `agent` or `human` (lowercase) and NAME 1–64 of
`A-Za-z0-9._@+-`; the id is the canonical string (`agent`, `human:ijat`). Empty is `agent`: an
unidentified caller never gets a human's privileges, and two agents sharing a session must set
distinct names. The first request of a client on a session adds it to the session's clients (with
first and last seen) and logs a `client` event; the starter is recorded at start.

**Breakpoint ownership.** Each breakpoint records `owner` and `createdAt`. Per file, the
breakpoints are grouped by requested line into one DAP source breakpoint per line (a slot): any
unconditional breakpoint makes the slot unconditional; equal conditions are kept; different
conditions make it unconditional and every breakpoint of the slot gets a note saying so. The
adapter's answer for a slot applies to every breakpoint in it. The same owner at the same line
replaces the condition, as before. `setBreakpoints` round trips are serialized per session, so the
adapter always ends up with the newest list (concurrent syncs could apply an older one before).
`bp rm ID` of another client's breakpoint is `NOT_OWNER` unless `--force`; `bp rm all` removes
your own (with `--force`, everyone's) and says how many others were kept. Run-until's temporary
breakpoint belongs to its caller; the next execution command removes every client's leftovers.

**Control lease.** The lease lives in `internal/session` (the phase 2 facade reuses it). The
starter holds it first. Execution requests (continue, next, step-in, step-out, pause, run-until,
and stop while the program is live) run under a per-session lock, checking the state, then the
lease, before any side effect; a refused request changes nothing. With holder H and requester C
(nobody holding, H = C, or `--force` always allow):

| policy | exec / `lease take` by C ≠ H | `lease grant` / `lease policy` by C ≠ H |
|---|---|---|
| free (default) | allowed; exec takes it (action `auto`) | `LEASE_HELD` |
| handoff | `LEASE_HELD` | `LEASE_HELD` |
| human-priority | allowed iff C is a human and H an agent | `LEASE_HELD` |

`release` by a non-holder does nothing. Reads, `wait`, `events` and your own breakpoints need no
lease; `daemon stop --force` bypasses it. `LEASE_HELD` and `NOT_OWNER` exit 2; hints never suggest
`--force`, which `lease take --help` documents for a holder that is gone.

**Event log.** One in-memory ring per session, `seq` from 1, bounded by 10000 events and 8 MiB
(oldest dropped; queries report how many they missed). Kinds: `started`, `client`, `lease`
(`take`, `auto`, `force`, `grant`, `release`, `policy`), `exec`, `continued` (only when no
request of ours is in flight), `stopped`, `output`, `breakpoint` (`added`, `removed`, `changed`),
`thread`, `exited`, `ended`. `events --since N` pages forward, bare `events` shows the newest, and
`--wait` long-polls (the connection model stays one request per call). Output chunks are cut at
64 KiB; `output` and `events` results at 512 KiB, snapshot output at 20 chunks and 16 KiB, event
output text at 1000 characters, so no response nears the 1 MiB line limit.

**Persistence** (in the runtime directory, already private; on Linux a tmpfs that dies with the
daemon's login session): `sessions/<id>.json` is a live session's metadata, version 1:
`{"version", "id", "lang", "program", "createdAt", "daemonPid", "state", "exitCode", "endReason",
"recording"}`, written 0600 at start and at the end, removed when the session is stopped or the
daemon exits cleanly. A daemon reads the files left at its start once, as the sessions an earlier
daemon lost: listed as `lost`, other methods say how to forget them, and `stop -s ID` forgets one
(also with no daemon, reading the files directly). `sessions/<id>.jsonl` is the recording: one
`api.Event` per line, created 0600 and never overwritten, capped at 4 MiB, with control events only —
no output, no stop text or description (exception messages can hold data), no launch arguments or
environment, no variable values. On by default; `start --no-record` or `EYEDBG_NO_RECORD=1` turns it
off. Recordings of ended sessions are pruned 7 days after their last write.

**Protocol 2.** The `output` result became `{"lines", "more"}` and requests carry identity an older
daemon would ignore, so `ProtocolVersion` is 2 and session commands refuse a daemon of another
protocol with `VERSION_MISMATCH` (`hello` is unchanged).

### Consequences

* Good, because agents and humans share a session with explicit, testable rules, and the CLI stays
  stateless: identity travels with each request, events are pulled.
* Good, because the merge never sends a line twice, so netcoredbg can't orphan a breakpoint, and a
  shared line keeps stopping when one of its owners removes theirs.
* Good, because the lease is atomic with the send and a refused request leaves no trace.
* Good, because a crashed daemon's sessions are reported instead of vanishing, and recordings hold
  no program data.
* Bad, because the default identity is shared: two agents that don't name themselves are one client.
* Bad, because conflicting conditions on one line degrade to an unconditional stop (with a note)
  until drivers can combine conditions in their language.
* Bad, because protocol 2 makes running protocol-1 daemons unusable for session commands until
  restarted.
* Bad, because recording writes happen on the adapter's event path: a stalled disk would stall
  event handling (control events are rare and output is not recorded).

## Pros and Cons of the Options

### Identity in `hello`

* Bad, because `hello`'s shape is frozen, and identity per connection needs per-connection handler
  state for no gain with one request per connection.

### `agent:<ppid>` default

* Bad, because agent harnesses start a fresh shell per command: the identity, and with it the
  breakpoints' owner and the lease holder, would change on every call.

### `human` when stdin is a terminal

* Bad, because PTY-based agent harnesses would silently get human privileges under human-priority.

### One DAP entry per client breakpoint

* Bad, because netcoredbg keeps one breakpoint per line and remaps by line: duplicate lines orphan
  breakpoints.

### Refusing a second client's breakpoint on a line

* Bad, because a phase 2 IDE gutter can't surface that refusal.

### Enforcing the lease in the request handlers

* Bad, because the check would not be atomic with the send, and the phase 2 facade would need its
  own copy.

### A separate output buffer beside a control-event ring

* Bad, because it is more code for pollers that see every kind anyway.

### Push subscriptions

* Bad, because the connection serves one request at a time; long-poll fits the stateless CLI and
  phase 2 alike.

### Persisting in `EYEDBG_DATA_DIR` or the cache directory

* Bad, because `EYEDBG_DATA_DIR` is the adapters directory itself and would need redefining and a
  second privacy check, and recordings there would accumulate forever.

## More Information

* Core concepts, lease and concurrency rules: docs/DESIGN.md §3; CLI surface: §4; daemon and lost
  sessions: §6; phase 2: §9; recordings: §11.
* ADR 0004 (daemon-owned sessions and control lease) set the direction this ADR details.
* Out of scope: `events` on a lost session reading its recording, lease release when a holder
  disappears (needs phase 2 presence), condition combining per driver, redaction.
