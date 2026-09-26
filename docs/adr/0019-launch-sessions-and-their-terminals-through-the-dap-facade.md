---
status: proposed
date: 2026-09-26
decision-makers: Ijat (@ijat)
---

# Launch sessions through the DAP facade

## Context and Problem Statement

ADR 0012 lets a DAP client join a running session through `eyedbg dap`; a DAP `launch` is refused
there, and ADR 0015's VS Code extension launches by running `eyedbg start` and then attaching. That
leaves three gaps: nvim-dap and other DAP clients can't launch at all; the build's output isn't
shown while it runs (the editor sees nothing until `eyedbg start` returns); and VS Code's Restart
re-runs the same attach configuration, so it re-joins the session instead of restarting the program
(ADR 0015, "Bad"). Terminals are the second half of the problem: an adapter's `runInTerminal`
reverse request is refused today (DESIGN §3), so a Python program launched from an editor can't read
from a terminal.

How does a DAP client's `launch` start a session under the CLI's rules (ADR 0009, 0012), without a
second session model in the facade?

## Decision Drivers

* A DAP `launch` from any client — VS Code, nvim-dap, a script — not an extension-only protocol.
* Policy stays in `internal/session` (ADR 0009, 0012): the launch runs the same checks, time limit
  and order as `eyedbg start`; the facade only translates.
* The editor's breakpoints and exception filters are in place before the program runs, even though
  VS Code sends them and `configurationDone` without waiting for responses.
* The build's output reaches the editor while it runs, and only that editor.
* A Stop, a program exit and a Restart leave nothing behind in `eyedbg sessions`; a Restart
  restarts the program.
* The documented contract of plain `eyedbg dap` (joins; never starts the daemon; its exit codes) is
  unchanged.
* No change to the native protocol version, `session.start` or the auth path.

## Considered Options

* A launch connection: `eyedbg dap --launch`, whose DAP `launch` starts the session.
* `eyedbg start --for-editor` returning something the editor attaches with, the session paused
  before its launch.
* Plain `eyedbg dap` binding lazily: to a session at `attach`, or to a new one at `launch`.

## Decision Outcome

Chosen option: "A launch connection", because it is a real DAP launch (nvim-dap and scripts can use
it, the build streams as DAP `output`) and leaves the joining contract as it is.

- **D1 Transport.** `eyedbg dap --launch [--as CLIENT]` opens the connection with `facade.open
  {client, launch: {clientDir, virtualEnv, noRecord, dotnetAdapter}}` (`facadeVersion` 3): what
  `eyedbg start` adds from its environment — the caller's directory (absolute), `$VIRTUAL_ENV`
  (absolute), `EYEDBG_NO_RECORD=1`, `EYEDBG_DOTNET_ADAPTER`; NUL refused. It joins no session: `-s`
  is refused, `$EYEDBG_SESSION` ignored, and the daemon is started if needed, as `eyedbg start`
  does. A daemon answering with a session id or a facade version below 3 is reported as
  `VERSION_MISMATCH`. The DAP `launch` runs `session.Manager.Launch` as the connection's client
  (who holds the lease, as the starter of `eyedbg start` does) on a goroutine of the connection,
  while its reader keeps reading. A failed launch (a build error, say) is the `launch` response's
  error, not an exit code; a launch stopped by the editor is `INVALID_REQUEST` "the launch was
  stopped" (not shown to the user); an error without a code becomes `INTERNAL`.
- **D2 Capabilities.** No adapter runs at `initialize`, so a launch connection answers it at once
  with the forced-on capabilities (ADR 0012) and both exception filters (`all`, `uncaught`); the
  adapter's own set follows as a DAP `capabilities` event before `initialized`. VS Code reads the
  exception filters from the `initialize` response only (read at 1.100.0 and 1.139.0), so both
  are always offered: a filter the adapter can't serve is answered successfully, turned off for
  that client, and said once on the debug console ("the debug adapter can't stop on … exceptions:
  exception stops are off for you").
- **D3 Configuration phase.** `Manager.Launch` takes hooks. After the adapter's `initialized` and
  the session's own configuration, before its `configurationDone`, its `Configure` hook binds the
  session to the connection, which sends `eyedbg/session`, `capabilities` and `initialized` in
  that order and waits for the editor's `configurationDone`. That request runs on the connection's
  ordered worker, behind every earlier `setBreakpoints`, `setFunctionBreakpoints` and
  `setExceptionBreakpoints`, so all of them are in place before the program runs; it is answered
  before the `launch` response. `launch` is answered when `Manager.Launch` returns (after
  `configurationDone`, whichever order the adapter answers its own launch in), and the connection
  then joins as an attach does at `configurationDone` (output replay, the state, the event log).
  Before the session is bound only `terminate` and `disconnect` are taken; other requests are
  refused with "the session is still starting: wait for the initialized event" — never a "does
  not support" an extension would read as a missing capability.
- **D4 The session id** reaches the editor in the custom event `eyedbg/session {sessionId}`, at the
  bind, before `initialized`.
- **D5 Lifecycle of a launched session.** The launcher's `terminate` ends the session and forgets
  it, as `eyedbg stop` does (under the lease), before its response; a `terminate` while the launch
  runs stops the launch (nothing it started stays), and one with nothing launched succeeds. When
  the launching connection ends, its session is forgotten if the program has exited, and keeps
  running otherwise (ADR 0012), with ADR 0014's presence cleanup. Other connections' `terminate`
  is unchanged: it ends the program, and the session stays listed as exited. So a Restart — the
  editor's terminate and a new launch — restarts the program.
- **D6 Build output.** A driver may stream its build (`session.OptionsPreparer`). The .NET driver
  then builds in two steps, `dotnet build <project> -c Debug -nologo -tl:off` into the stream and
  `dotnet msbuild <project> -nologo -getProperty:TargetPath -p:Configuration=Debug` for the
  program (`-getProperty` suppresses the build log, verified); `eyedbg start` keeps its single
  build and output. Each line becomes a `console` `output` event on the launching connection only
  — never the session's event log or the daemon's log — bounded: 4096 bytes a line (cut with "…"),
  10,000 lines and 1 MiB a launch, then one "more build output isn't shown". A failed streamed
  build is `BUILD_FAILED` "dotnet build <project> failed (its output is above)".
- **D12 Launch arguments.** Known keys only, each with its type: `lang` (required), `program`,
  `project`, `cwd`, `args` (strings), `env` and `opts` (objects of strings), `stopOnEntry`,
  `noBuild` (booleans), `leasePolicy`, `exceptions`, `adapter`. Other keys are ignored (VS Code adds
  `__sessionId`, `__restart`, …; `noDebug` is ignored, so it debugs, as the extension always did).
  A key equal to a known one but for case is refused (`INVALID_REQUEST`: Go's JSON would match it,
  the client didn't mean it), as are `null` for a typed key, NUL in any value, and an `env` or
  `opts` name that is empty or holds `=` (`eyedbg start`'s `KEY=VALUE` rule). Relative paths
  resolve against the `eyedbg dap` process's directory (`eyedbg start`'s rule); on Windows a path
  relative to a drive or to the current drive's root is refused. Neither `program` nor `project`:
  the project is that directory.
- **D13 Feature flag.** `eyedbg version --json` lists `dap.launch`; an extension launches through
  the facade only when it is there (no fallback to `eyedbg start` + attach).
- **D14 Cut.** Launch (this record's S3a) ships before terminals (S3b). Terminal routing is not
  built yet: the adapter's `runInTerminal` stays refused, as every reverse request is (DESIGN §3).
  The intended design — route it only to the connection whose launch started the session, only
  while that launch runs, only when it asked for an integrated terminal, and have the extension run
  the program without a shell — will be recorded here when it is built.

### Consequences

* Good, because any DAP client can launch: nvim-dap with `{type = 'executable', command = 'eyedbg',
  args = {'dap', '--launch', '--as', 'human:NAME'}}` and `request = 'launch'`.
* Good, because a Restart restarts the program, and a Stop, a program exit (once the editor
  leaves) and a Restart leave nothing listed — what the extension did with `eyedbg stop` now holds
  for every client.
* Good, because the build's output shows while it runs, in the launching editor only.
* Good, because the editor's breakpoints are in place before the program runs, whatever order its
  requests' responses come in.
* Bad, because a C, C++ or Rust launch offers exception filters its adapter can't serve (D2); they
  are refused softly, with one console line.
* Bad, because a streamed .NET launch runs `dotnet` twice (the build, then the property query).
* Bad, because two facade modes now share one connection type: a launch connection has a phase
  before its session exists, a launch goroutine and a late session binding.
* Neutral: a session launched by an editor that stays connected behaves as one joined by it; the
  agent sees who started it (`eyedbg events --kind started`).

### Confirmation

`internal/session/launch_test.go` (the hook's place, its errors), `internal/facade/launch_test.go`
(the handshake, the configuration barrier with VS Code's pipelined requests, terminate while
launching and after, build output bounds, the argument rules) and `internal/e2e/
facade_launch_test.go`, which drives `eyedbg dap --launch` through the real binaries: the fake
adapter always; Python and .NET (netcoredbg and SharpDbg: the build's output before `initialized`)
with `EYEDBG_E2E=1`.

## Pros and Cons of the Options

### A launch connection

* Good, because it is a standard DAP launch with live build output.
* Good, because joining connections are unchanged.
* Bad, because capabilities are known only after `initialize` (D2).

### `eyedbg start --for-editor`

* Good, because the editor would get the exact capabilities at `initialize`.
* Bad, because it isn't a DAP launch: no nvim-dap launch, no live build output, a "waiting for an
  editor" state and an editor-only CLI flag.

### Plain `eyedbg dap` binding lazily

* Good, because it needs no flag.
* Bad, because it changes the documented default-session choice and exit codes of `eyedbg dap`.

## More Information

Amends ADR 0012 (a launch connection besides joining ones; the launcher's terminate forgets its
session) and ADR 0015 (F5 launches through the facade, so Restart restarts the program), and
DESIGN §2, §4, §6, §9. Planned in `p2-ext-launch` (plan decisions D1–D6, D12–D14); the terminal
decisions (D7–D11, D15, D16) follow with S3b.
