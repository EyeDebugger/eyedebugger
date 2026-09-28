---
status: proposed
date: 2026-09-26
decision-makers: Ijat (@ijat)
---

# Launch sessions and their terminals through the DAP facade

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
second session model in the facade — and how does an adapter's `runInTerminal` reach a terminal
without letting any adapter or client make an editor run a command it never asked for?

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
* A command reaches a human's terminal only as that human's own launch asked, exactly as it was
  checked, and never through a shell.

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
- **D13 Feature flags.** `eyedbg version --json` lists `dap.launch` and `dap.terminal`; an
  extension launches through the facade only with `dap.launch` (no fallback to `eyedbg start` +
  attach), and offers `console: integratedTerminal` only with `dap.terminal`.
- **D14 Cut.** Launch (S3a) shipped before terminals (S3b, D7–D11, D15, D16).
- **D7 Terminal executor.** The facade never sends the standard DAP `runInTerminal` to an editor. It
  sends the custom event `eyedbg/runInTerminal {id, title, cwd, args, env}` to the launching
  connection, and the editor extension runs `args[0]` itself as the terminal's process, the rest
  as its arguments — no shell — and answers with the request `eyedbg/runInTerminal {id,
  processId}` or `{id, error}`. VS Code's own `runInTerminal` types a command line built by
  `prepareCommand` into the user's shell (newlines unescaped for bash, `<`/`>` passed raw, `%VAR%`
  left to cmd, env names unquoted for PowerShell and cmd, a Ctrl+C first into a reused terminal):
  a parser differential no check in eyedbg can close across shells. Other DAP clients get no
  terminal (they launch with `internalConsole`).
- **D8 Routing.** An adapter's `runInTerminal` reaches only the connection whose `launch` created
  the session, only while that start runs (until `session.Manager.Launch` returns; the route is
  closed under the session's lock before it does), only when that launch asked for `console:
  integratedTerminal` and the adapter's manifest supports it, and at most once per start. Every
  other one — a CLI start's, one after the start, a second one, one on a joined connection — is
  refused as before ("not supported"). The lease is not consulted (DESIGN §3 amended): the command
  comes from the editor's own launch and goes back to it; a lease check could only fail the
  human's own launch when an agent took control in the start-up window, and routing to whichever
  human holds the lease would let an agent-started session make a human's editor run a command it
  never asked for. The adapter is told `supportsRunInTerminalRequest: true` only for a start with
  that route (D16: debugpy refuses a terminal console without it).
- **D9 Validation.** The facade checks the adapter's request against one table, sends the editor
  only values it checked (re-encoded, never the adapter's bytes), and the extension checks the
  event again against the same table; `internal/facade/testdata/terminal_requests.json` drives the
  Go and TypeScript tests. A failing rule refuses the whole request — nothing is cut or rewritten —
  and fails the launch with `ADAPTER_ERROR` "the debug adapter asked to run a command eyedbg won't
  run in a terminal: rule N: …" (hint `use "console": "internalConsole"`), nothing sent to the
  editor:
  1. `kind` is absent or `integrated`;
  2. `argsCanBeInterpretedByShell` is false;
  3. `args` has 1–1000 entries;
  4. no string in the title, cwd, args, env names or values holds U+0000–U+001F, U+007F–U+009F,
     U+2028 or U+2029;
  5. title, cwd, args and env together are at most 256 KiB of UTF-8;
  6. `args[0]` and `cwd` are local absolute paths — never two leading separators (`\\`, `//`:
     UNC, `\\?\`, `\\.\`); on Windows a drive root (`C:\` or `C:/`), elsewhere a leading `/`;
  7. on Windows `args[0]` ends in `.exe` (any case), no argument is a single space, and no argument
     longer than one character both starts and ends with `"` while holding a space (node-pty emits
     both unquoted — its `argsToCommandLine` quotes a spaced argument only when it is longer than
     one character and not enclosed in `"` — so the program's own parser would drop or split it;
     rule 4 already refuses tabs);
  8. `env` has at most 1000 entries, names `^[A-Za-z_][A-Za-z0-9_]{0,127}$`, values strings or
     `null` (unset), nothing else;
  9. the title is at most 200 characters (empty: "eyedbg").

  An adapter's request go-dap can't decode (an `args` string, a non-string argument) never reaches
  the facade: it stays unanswered, and the adapter's own timeout fails the launch.
- **D10 Which adapters.** A manifest says how its adapter runs in a terminal with `launch.terminal`,
  arguments merged over `launch.arguments` (docs/adapter-manifests.md): debugpy's is `{"console":
  "integratedTerminal"}`. A launch asking for a terminal with an adapter without it is
  `UNSUPPORTED_BY_ADAPTER`; the .NET driver always refuses (netcoredbg has no `runInTerminal`;
  SharpDbg's is unverified). lldb-dap and Delve are untouched (follow-ups).
- **D11 Terminal failure.** The editor's error (control characters as spaces, at most 1000
  characters), an invalid answer (neither or both of a `processId` in 1..2147483647 and an
  `error`), or no answer within 30 s fails the start at once with `ADAPTER_ERROR` "the editor
  didn't start the program's terminal: …" (not debugpy's 60 s launcher timeout); the adapter gets
  the failure as its `runInTerminal` error. The event's `id` counts from 1 per connection and one
  waits at a time; an answer with another id, a second answer, one on a joined connection or before
  a launch is `INVALID_REQUEST` and changes nothing. Nothing of the request (args, env, cwd, title)
  is logged.
- **D15 `console`.** The launch argument `console` is `internalConsole` (the default) or
  `integratedTerminal`; anything else, `externalTerminal` included, is `INVALID_REQUEST` (there is
  no shell-free way to start an external terminal).
- **D16** See D8: `supportsRunInTerminalRequest` only with the route.

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
* Good, because a Python program launched from VS Code can read its input in a terminal, started
  without a shell from values eyedbg checked.
* Bad, because only the EyeDebugger extension answers `eyedbg/runInTerminal`: nvim-dap and other
  clients have no terminal (follow-up: the standard request for clients that opt in), and only
  debugpy asks (D10).
* Bad, because the table refuses some legitimate commands (a relative program, a UNC path, a
  single-space or quote-enclosed spaced argument on Windows): such a launch uses `internalConsole`.
* Neutral: a session launched by an editor that stays connected behaves as one joined by it; the
  agent sees who started it (`eyedbg events --kind started`).

### Confirmation

`internal/session/launch_test.go` (the hook's place, its errors; the terminal route: once, only
while the start runs, only with the hook, the capability, a failing or canceled terminal),
`internal/dap/client_test.go` (reverse requests answered later, once), `internal/facade/
launch_test.go` (the handshake, the configuration barrier with VS Code's pipelined requests,
terminate while launching and after, build output bounds, the argument rules),
`internal/facade/terminal_test.go` (the shared vectors on each platform, parsing like the enforcer,
the exchange and its refusals) and `internal/e2e/facade_launch_test.go`, which drives `eyedbg dap
--launch` through the real binaries: the fake adapter always (a terminal answered, one refused by
the table, none on a joined connection); Python (a program started without a shell reading its
input, its arguments exact) and .NET (netcoredbg and SharpDbg: the build's output before
`initialized`) with `EYEDBG_E2E=1`.

Security-relevant surfaces, for review: `facade.open {launch}` (a new way to create a session over
an authenticated connection, the power of `session.start`); launch arguments (key and type checks,
case variants, path resolution, NUL); build output (the launching editor only, bounded, not
logged); the launcher's terminate forgetting its session; the terminal route (D8); the table (D9);
the `eyedbg/runInTerminal` exchange (D11); the extension's runner (no shell, no `sendText`, its
own acceptance checks); `eyedbg dap --launch` starting the daemon; `launch.terminal` in manifests.

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
DESIGN §2, §3 (reverse requests), §4, §6, §9, §11. Planned in `p2-ext-launch` (plan decisions
D1–D16).
