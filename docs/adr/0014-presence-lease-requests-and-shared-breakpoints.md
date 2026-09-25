---
status: proposed
date: 2026-09-24
decision-makers: Ijat (@ijat)
---

# Presence, lease requests and sharing breakpoints with editors

## Context and Problem Statement

ADR 0012 lets a human's editor join an agent's session through `eyedbg dap`. What it left open:
the lease stays with a human who closed their editor (ADR 0009 deferred releasing it "until
presence exists"), so a `handoff` session stays stuck until someone forces it; an agent can't ask
for control without taking it; an editor extension has no channel for the lease, the clients and
the other clients' activity; a human never sees the agent's breakpoints; and an editor's
breakpoint list silently removes breakpoints its client set with the CLI. The user decided
(2026-09-24) that other clients' breakpoints reach the editor as DAP `breakpoint` events, knowing
that VS Code adopts such a breakpoint as the user's own and re-sends it — without its condition —
in later `setBreakpoints`, persisted across restarts. How do clients see each other, negotiate the
lease, and share breakpoints without corrupting each other's?

## Decision Drivers

* One place for policy: presence drives the lease and breakpoint removal, which are session state
  (ADR 0009).
* No rule changes for CLI-only use; the default lease policy stays `free`.
* Ownership (ADR 0009) holds whatever an editor sends.
* The worst case — an agent's conditional breakpoint re-sent by VS Code as the human's
  unconditional one — must not happen through any path the editor's own behaviour creates
  (disable/enable, a restart, a request built before an event was processed).
* No timers where an event will do; tests never sleep.
* Additive wire changes only.

## Considered Options

* Presence: counted in the session per client · in the facade or the daemon.
* Leaving: the client's last connection, with a reconnect guard and a restart grace · per
  connection · never.
* Lease requests: a pending request shown in every snapshot plus an event · an event only · a queue
  that grants on release.
* Other owners' breakpoints: `breakpoint` events with a column-1 mark, per-line classification and
  retracts · location matching against the announced set only · decorations only (the roadmap) ·
  removing the owner's breakpoint when the copy is deleted.
* The extension's channel: `eyedbg/*` custom DAP requests and events on the editor's connection ·
  a second, native connection.

## Decision Outcome

Chosen: presence in the session with last-connection leaving, pending lease requests, marked
mirrors with classification, and `eyedbg/*` custom messages — the only combination that keeps the
rules in the session, never lets an editor's copy change an owner's breakpoint, and needs no
second connection (Node can't open AF_UNIX on Windows).

**Presence.** An open `eyedbg dap` connection is its client's presence
(`Session.Connect`/`Presence.Leave`); `api.ClientInfo.connected` counts them (`sessions` shows a
`CONNECTED` column). Each connect and leave logs a `client` event with action `connected` /
`disconnected` (the first-seen `client` event stays). CLI clients are never connected. Nothing is
persisted.

**Leaving.** When a client's last connection closes (disconnect, close, crash) while the session is
live and the daemon isn't stopping, in this order, each step first re-checking under the session
lock that the client has no connection and opened none since (a session-wide connection number):

| step | effect |
|---|---|
| 1 | `client` `disconnected` (logged at every leave) |
| 2 | remove the client's **editor** breakpoints (its CLI ones and others' stay) |
| 3 | reset its exception mode to `none` if an editor of it last set one other than `none` |
| 4 | drop its pending lease request (no event) |
| 5 | release its lease: `lease` `release`, `reason: "disconnected"` — last, so an agent woken by it never meets the departed human's breakpoints |

After DAP `disconnect {restart: true}` (VS Code's Restart) the cleanup waits 10 s
(`facade.DefaultRestartGrace`, a timer on the manager's context); a connection of the client
within it cancels the cleanup, so the lease and breakpoint ids survive. Exception: the rejoined
connection starts with no path view, so for a file the editor holds under two paths (a symlink),
its first re-sent list removes the breakpoints listed under the other path, and the next list
re-creates them with new ids. Any other leave cleans up at once. A holder that never connected an editor is never released.

**Editor breakpoints.** Breakpoints created or matched by an editor's `setBreakpoints` /
`setFunctionBreakpoints` carry `api.Breakpoint.editor` (`bp ls` shows `(editor)`). A replace still
matches against all of its client's breakpoints in the file (a matched CLI breakpoint becomes an
editor one), but removes only the client's unmatched **editor** breakpoints: an editor's list never
removes what its client set with the CLI. An editor keys its lists by source path, and may have a
file under two (a symlinked workspace, macOS's `/tmp`): the replace also keeps the editor
breakpoints the connection's lists hold under the file's other paths, atomically under the session
lock (`ReplaceBreakpoints`' `keep`), so a list under one path never removes what the editor set
under another — nor re-creates one removed meanwhile. A breakpoint on a line listed under both
paths is one breakpoint, removed once neither lists it.

**Lease requests.** `eyedbg lease request [--message TEXT]` (native `lease.request`, DAP
`eyedbg/lease {action: "request"}`) records a pending request and logs `lease` `request` (text: the
message, ≤ 200 characters) — nothing else changes, under every policy. One per client (a repeat
replaces it), at most 16. They're cleared whenever the lease changes hands. `LeaseInfo.requests`
shows them in `status`, every stop's sharing line (`lease: H (P); requested by C: "msg"; clients: a,
b (connected)`), `lease` and `sessions --json`. Asking while holding the lease, or while nobody
does, records nothing. Recordings strip requests.

**Custom DAP messages** (registered on the connection's codec, answered by the facade, never
forwarded; allowed after `attach`; errors as ADR 0012's, `showUser: false`):

| message | direction | arguments / body |
|---|---|---|
| `eyedbg/lease` | request | `{action: status\|take\|release\|request\|grant\|policy, to?, policy?, force?, message?}` → `{lease: LeaseInfo}`; the CLI's semantics, `force` included; all but `status` are ordered and gated |
| `eyedbg/clients` | request | `{}` → `{clients: [ClientInfo], lease: LeaseInfo}` |
| `eyedbg/breakpoints` | request | `{action?: list\|remove, id?, force?}` → `{breakpoints: [Breakpoint] (≤ 1000), more?, removed?}`; `remove` is `eyedbg bp rm ID [--force]` (another owner's without `force`: `NOT_OWNER`) |
| `eyedbg/lease` | event | `{lease}` on every lease change (the event's snapshot) |
| `eyedbg/clients` | event | `{clients}` on every client join, connect or disconnect |
| `eyedbg/activity` | event | `{event: Event}`: another client's `exec`, `breakpoint`, `exceptions`, `lease` or `client` event (never output or the adapter's changes), text cut to 1000 characters |
| `eyedbg/breakpoints` | event | `{breakpoints, more?}` once per followed batch of log events that changed breakpoints |

Example: `{"type":"event","event":"eyedbg/lease","body":{"lease":{"policy":"handoff","holder":"agent",
"requests":[{"client":"human:ijat","message":"let me step","at":"2026-09-24T12:00:00Z"}]}}}`. At
the join, after the replayed state and the mirrors, one `eyedbg/lease`, `eyedbg/clients` and
`eyedbg/breakpoints`. They go to every DAP client (VS Code raises unknown events as custom events,
nvim-dap drops them).

**Mirrors.** The editor is sent, as `breakpoint` `new` events after `configurationDone`, every line
breakpoint it didn't set: other owners', and its client's CLI ones. Not mirrored: function
breakpoints (VS Code can't create them from events; `eyedbg/breakpoints` lists them), run-until
temporaries, a line where the connection has one of its own (placed or requested), and all but the
lowest id on a line. A mirror is `{id, verified, line: placed line, column: 1, source: {path},
message}`, the path being the one the editor last set a breakpoint of its own in that file under
(so the copy lands where the editor shows the file), else the resolved file; a mirror keeps its
path; the message says `"<owner>'s breakpoint"` (own CLI: `"your breakpoint,
set outside this editor"`), `" · if C"`, `" · hit H"`, `" · logs M, doesn't stop"`, the adapter's
message and sharing note, and ends `" — removing it here only hides it"` (≤ 300 characters).

**The mark.** VS Code keeps an adopted breakpoint's column and sends it back (`toDAP` sends the
column it was created with; `toJSON` persists it), and its UI practically never makes a plain
column-1 breakpoint (inline breakpoints use `column > 1 ? column : undefined`; gutter ones have
none) — verified in `microsoft/vscode` main, 2026-09-24. So a plain entry at column 1 is an eyedbg
copy, in any later `setBreakpoints`, after an editor restart too.

**Classification** of each line L of a `setBreakpoints` for source path P (*plain*: no condition,
hit condition or log message; *marked*: plain at column 1; a request of more than 1000 entries —
`setFunctionBreakpoints` too — is refused whole with `INVALID_REQUEST`):

| entries at L | first entry | the others |
|---|---|---|
| only marked | echo of L's candidate: the mirror the connection announced at L (under P first) if its breakpoint exists, else the lowest-id mirrorable breakpoint placed or requested at L (hidden ones too); none → retract | retract |
| only plain unmarked, and the connection shows a mirror at L under P (clients that drop columns, e.g. nvim-dap) | echo of L's candidate, else of that mirror | refused as a same-line duplicate |
| anything else | the client's own (to the session's replace) | own; marked ones retracted |

An **echo** never reaches the session: it's answered with the mirror under P (the owner's condition
survives) and kept in the connection's view at L, under P. A **retract** is answered `{id: <negative>,
verified: false, message: "eyedbg: a leftover copy of another client's breakpoint; removed"}`,
followed after the response by `breakpoint removed` for that id and one console line. The
connection's mirrors under P that weren't echoed leave its view (those under the file's other
paths stay: this list doesn't hold them): silently where the request has
an entry at their line (the human took the line: editing a copy — giving it a condition — makes a
breakpoint of the human's own), else **hidden** (the user removed or disabled the copy; never
re-announced on this connection; it stays its owner's). The mark or a new join shows it again.

**Reconcile.** Each connection keeps a view — the ids its responses reported, the mirrors it
announced (with the line announced), the hidden ids — only under its event gate. After every
breakpoint log event, every breakpoint request's response and at the join it diffs the view
against the session: `removed` (reported gone, mirror no longer wanted), `changed` (state changed;
mirrors keep column 1), `new` (wanted, not announced), in that order, each by id. Every mirror is
retracted before the `disconnect` response (after which the connection sends nothing) and before
`terminated`.

**Output replay.** At `configurationDone`, before the program's state: the newest ≤ 200 output
chunks up to the join point, within 64 KiB, as `output` events (after one console line counting the
ones left out); exactly once, since the log is followed from the join point.

**Versioning.** Native protocol stays 2, JSON `schema` stays 1. `version --json` `features` gains
`presence`, `lease.request` and `dap.collab`; `facade.open`'s `facadeVersion` is 2. New fields:
`ClientInfo.connected`, `LeaseInfo.requests`, `LeaseResult.holderConnected`, `Breakpoint.editor`,
`Event.reason` on a presence release. An older daemon answers `lease.request` with
`UNKNOWN_METHOD` and `eyedbg/*` with `INVALID_REQUEST` ("does not support"): an extension
feature-detects at run time.

**Lease default.** Stays `free` (the user, 2026-09-24): agent-only flows don't change; the editor
extension offers `human-priority` after a human takes over.

### Consequences

* Good, because a departed human never blocks a `handoff` session, and a Restart keeps the lease.
* Good, because agents see a human's request in every stop, and humans see the agent's
  breakpoints with their conditions in the hover.
* Good, because VS Code's copies never change an owner's breakpoint, also after a disable/enable, a
  race or an editor restart; an editor's list never removes its client's CLI breakpoints.
* Bad, because VS Code's copies are plain red dots: the condition is only in the hover.
* Bad, because a human's plain breakpoint on a line where the agent has a conditional one makes the
  line unconditional for both (the session's merge), without a note.
* Bad, because a Restart keeps a departed editor's breakpoints and lease for up to 10 s, and a
  restart slower than that loses the lease and churns ids.
* Bad, because two editors joined as one client share one breakpoint set.
* Bad, because the mark relies on VS Code behaviour verified from its source, not a protocol
  guarantee; a plain column-1 breakpoint the user made (a triggered breakpoint, or an emptied
  logpoint, created with the cursor at column 1) is taken for a copy, and indenting a copy's line
  shifts its column, making it the human's own.
* Bad, because any process of the same user can connect as `human:x` and trigger `human:x`'s
  leaving — what `eyedbg --as human:x lease release` / `bp rm` already allow.

### Confirmation

The session's presence, lease-request and replace tests; the facade's classification and reconcile
tables and its scenario tests (echoes keep the owner's condition, hide and re-enable, retracts,
a file under two paths, leaving, restart grace); `TestFacadeCollab` through the real binaries against the fake adapter,
debugpy and netcoredbg, on Linux, macOS and Windows (CI); and the editor extension's integration
tests of VS Code's round trip (announce, adopt at column 1, disable/enable, restart).

## Pros and Cons of the Options

### Presence and leaving in the session

* Good, because only the session sees all of a client's connections: "last connection" and the
  reconnect guard are atomic under its lock.
* Bad, because a per-connection cleanup in the facade would drop the lease and breakpoints of a
  client's other window, or of a restarting editor.

### Location matching against the announced set only

* Good, because it needs no mark.
* Bad, because it recognises a copy only while this connection's view still holds the mirror: it
  misses a disable/enable, a request VS Code built before processing the event, and every re-send
  after an editor restart — exactly the paths by which the worst case happens. Kept as the fallback
  (rule A) for clients that drop columns.

### Decorations only

* Good, because VS Code never adopts anything.
* Bad, because the user decided otherwise (2026-09-24).

### Removing the owner's breakpoint when the copy is deleted

* Bad, because it breaks ownership (ADR 0009); removing another owner's breakpoint stays an
  explicit, forced action (`eyedbg/breakpoints remove force`).

### A second, native connection for the extension

* Bad, because Node can't open AF_UNIX on Windows, and identity would be carried twice.

## More Information

Amends ADR 0012 (leaving releases the lease; its breakpoint and event rules); closes ADR 0009's
"release the lease when its holder disappears". DESIGN §3, §4, §9 and §14.

## Addendum (2026-09-25, P2-M5): stops caused by another client

- **Rule.** A DAP `stopped` whose latest execution request before it (continue, next, stepIn,
  stepOut, runUntil, pause — not eval or set) came from **another** client carries
  `preserveFocusHint: true`. The stop replayed at the join, a resync's stop after dropped events,
  and a stop with no execution request since the last one keep the plain body. ADR 0012's
  `stopped` row carries the hint for such stops; nothing else in its table changes.
- **Why.** VS Code focuses the editor on every stop without the hint and, by default
  (`debug.focusWindowOnBreak`), raises its window: every agent step moved the human's keyboard
  focus into the source (typing meant for a terminal landed in a file). The hint is DAP's own
  meaning ("the client should not change the focus"), true for any client of a shared session,
  so it goes to every connection. Following the agent without taking focus is then the extension's
  choice (ADR 0015 addendum, P2-M5).
- **Rejected.** An opt-in `attach` argument (keeps nvim-dap & co. unchanged, but one more contract
  for a hint every client should get; revisit if a client objects); an `eyedbg/follow` custom
  request (more facade surface for nothing the extension can't do); rewriting stops in an
  extension-side proxy (a second DAP parser); changing the user's `debug.focus*OnBreak` settings
  (global, they affect other debuggers).
- **Compatibility.** An added field in an outgoing event body: no `version --json` feature. With
  an older daemon, VS Code keeps focusing every stop, as before.
