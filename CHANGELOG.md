# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `eyedbg dap [-s ID] [--as CLIENT]` (phase 2, ADR 0012): any DAP client — VS Code through the
  coming extension, nvim-dap, a script — joins a running session over stdio. It authenticates like
  every command and switches its daemon connection to DAP (`facade.open`); stdout carries DAP only
  and errors go to stderr. The editor attaches (launch is refused), sees the current stop, threads,
  stack, variables and output, and every later stop; it acts under the CLI's rules: continue,
  steps, pause, terminate, `setVariable`/`setExpression` and debug-console (repl) evaluation need
  the control lease (`LEASE_HELD`, with the code in `variables.code`), watch and hover evaluation
  refuse side effects, its breakpoints (hit counts and logpoints included) and exception filters
  are its own and are removed when it disconnects; the session keeps running. Other clients'
  resumes and state changes arrive as `continued`/`invalidated`. Frames are bounded (1 MiB, one
  `Content-Length` header) and malformed ones close the connection.
- `version --json` (both binaries) reports `"protocol"` (the native API version, 2) and
  `"features": ["dap", "presence", "lease.request", "dap.collab"]`.
- Editors and agents collaborate (phase 2, ADR 0014): presence — each `eyedbg dap` connection
  counts as its client's (`connected` in `sessions --json`, a `CONNECTED` column in `eyedbg
  sessions`, `client connected`/`disconnected` events); when a client's last editor connection
  closes, its editor breakpoints are removed, the exception mode its editor set is reset and its
  lease is released (`lease` `release`, reason `disconnected`; a DAP `disconnect {restart: true}`
  waits 10 s for it to come back). `eyedbg lease request [--message TEXT]` (native
  `lease.request`) asks the holder for the lease without moving it: a pending request shows in
  `status`, every stop's sharing line (`requested by C: "msg"`), `lease` and `sessions --json`
  (`lease.requests`) until the lease changes hands. The DAP facade answers custom `eyedbg/lease`,
  `eyedbg/clients` and `eyedbg/breakpoints` requests and sends `eyedbg/lease`, `eyedbg/clients`,
  `eyedbg/breakpoints` and `eyedbg/activity` events for editor extensions; shows other clients'
  line breakpoints in the editor as DAP `breakpoint` events at column 1, with whose they are and
  their condition in the message — the editor's re-sent copies stay the owner's (condition kept),
  stale copies are retracted, removing one only hides it, editing one makes the human's own — and
  retracts them on leaving; and replays the output from before the join (newest 200 chunks,
  64 KiB). `facade.open` reports `facadeVersion` 2. `eyedbg bp ls` marks breakpoints set through
  an editor `(editor)` (`editor` in `--json`).
- The VS Code extension (`extensions/vscode`, phase 2, ADR 0015; not published yet): debug type
  `eyedbg` joins a running session (`attach`, picked when not named; Run and Debug lists one
  configuration per session) or starts one as the human with F5 (`launch`: `eyedbg start`, then
  join; ending that debug session stops it); a status bar item shows who has control, the policy
  and pending requests, with take, take by force, request, release, give and policy actions; a
  refused step offers Request Control / Take Over; other clients' requests reach the holder; after
  taking control from an agent under `free` it offers `human-priority` (`eyedbg.lease.afterTakeOver`). Other
  clients' breakpoints, which VS Code shows as red dots, get an end-of-line note (owner, condition,
  hit count, log message), a hover, and *Copy as My Breakpoint* / *Remove Breakpoint for
  Everyone…* on the line-number menu. "EyeDebugger Activity" lists other clients' actions. The
  binary is the machine-scoped `eyedbg.path` or `eyedbg` on PATH; the extension doesn't run in
  Restricted Mode and shows session text as plain text only.
- Stops carry `allThreadsStopped` when the adapter says so (`--json` snapshots and events).
- C, C++ and Rust debugging through LLVM's lldb-dap (`eyedbg start c|cpp|rust --program FILE`),
  manifest only (`lldb-dap-c.json`, `lldb-dap-cpp.json`, `lldb-dap-rust.json`; `EYEDBG_LLDB_DAP` or
  PATH; no managed download). Go debugging through Delve (`eyedbg start go --program DIR`,
  `--opt mode=debug|exec|test`, `--opt buildFlags`), served by a new
  `adapter.transport: "connect"` (a private Unix socket the adapter dials in to) since `dlv dap`
  never speaks stdio; `eyedbg adapters install delve` downloads the pinned release. A new
  `${envList}` launch template variable lets `lldb-dap`'s pre-20 `--env` array shape work from a
  manifest. Manifest schema, session and trust model unchanged otherwise (ADR 0013). With
  lldb-dap 18–20, `set` reports the new value (their `setVariable` answer carries it as
  `"result"`: the session re-reads the variable), and on Linux a pause stops with reason `pause`,
  not `exception` (`signal SIGSTOP`).
- Releases include the VS Code extension as `eyedebugger_X.Y.Z_vscode.vsix`, listed in
  `checksums.txt` and covered by the provenance attestation; the extension's version follows
  eyedbg's. CI keeps each run's VSIX as the `vsix` artifact. A publish workflow for the VS Code
  Marketplace and Open VSX runs after each release and does nothing until its secrets are
  configured (`docs/MAINTAINING.md`).
- The VS Code extension shows what the agent does (ADR 0015 addendum, P2-M5): *EyeDebugger
  Activity* and *EyeDebugger Clients* views in Run and Debug — the agent's steps land on the line
  they stopped at (click to open), breakpoints, control changes, who is connected and last seen,
  with request/take/release/give actions per client; the editor follows the agent's stops without
  taking focus (`eyedbg.followAgent`); an agent's session of a program in the workspace is offered
  once with a prompt (`eyedbg.autoJoin`; the extension now starts with each trusted window and
  runs `eyedbg sessions` every 5 s while the window has focus and nothing is joined);
  `launch.json` snippets for Python and .NET; a *Get started* walkthrough; **EyeDebugger: Check
  eyedbg Installation**.
- `eyedbg dotnet ps` and `eyedbg dotnet counters` (phase 2, ADR 0016): inspect a running .NET
  process without pausing it. `dotnet ps` lists your processes with a .NET diagnostics endpoint
  (pid, name, and the session debugging each). `dotnet counters [-s ID | --pid N]` samples the
  System.Runtime counters (CPU, memory, GC, allocation, exceptions, thread pool, lock contention,
  JIT, …) for `--duration` (default 5s) every `--interval` (default 1s) and prints per counter the
  last value and range, or the total and rate; `--counters NAME,...` narrows them, `--watch` prints
  each sample, about one interval after it is taken (interrupting drops the newest), `--json`
  everywhere. Read-only (an EventPipe session inside the
  process), your own processes only (on a shared machine another user can pose as a process's
  diagnostics endpoint unless its TMPDIR is private; see `eyedbg help dotnet`), no control lease,
  nothing in the session's events; a session
  stopped at a breakpoint is refused (`NOT_RUNNING`: a stopped runtime can't start a diagnostics
  session). New error codes: `NOT_DOTNET`, `DIAGNOSTICS_DISABLED`, `DIAGNOSTICS_TIMEOUT` (exit 2),
  `HELPER_NOT_FOUND`, `HELPER_MISMATCH` (exit 3), `HELPER_FAILED` (exit 4). `version --json`
  features add `"dotnet.helper"`.
- The .NET side helper `eyedbg-dotnet-helper` (`helpers/dotnet`, C#, runs on .NET 8 or later) ships
  in every release archive under `helpers/dotnet/`, with `THIRD-PARTY-NOTICES.txt` for the MIT
  licensed Microsoft assemblies it carries; `$EYEDBG_DOTNET_HELPER` overrides where eyedbg looks
  for it.
- `eyedbg dotnet dump`, `eyedbg dotnet heap` and `eyedbg dotnet threads` (phase 2, ADR 0016
  addendum): `dump [-s ID | --pid N] [--type heap|mini|triage|full] [--out PATH]` has the runtime
  write a dump of the process (suspending it meanwhile) into eyedbg's private directory
  (`<EYEDBG_HOME or ~/.eyedbg>/dumps`, 0700, files 0600; eyedbg's dumps are removed after 7 days and
  beyond the newest 10) or to `--out`, which is never overwritten. `heap [DUMP | -s | --pid]` shows
  objects and bytes per generation and the top types by size (`--top`, `--type PATTERN`);
  `--gcroot TYPE|0xADDRESS` shows root paths — a static field, a thread's stack, a handle — that keep
  them alive (`--paths`). `threads [DUMP | -s | --pid]` shows each managed thread's top frames with
  source file:line (threads with the same stack grouped), and the contended locks with their owner
  and waiters. Given a process, heap and threads dump it first; a session stopped at a breakpoint
  can be dumped (unlike counters). Output never shows field values, strings, thread names or
  exception messages. New error codes: `DUMP_UNSUPPORTED` (exit 2), `DUMP_RUNTIME_MISSING` (exit 3),
  `DUMP_FAILED` (exit 4). `version --json` features add `"dotnet.dump"`.
- `eyedbg dotnet trace [FILE | -s ID | --pid N] [--profile cpu|gc] [--duration 10s] [--top 10]
  [--out PATH]` (phase 2, ADR 0016 addendum): records a short EventPipe trace of a running .NET
  process and summarizes it. `--profile cpu` (default) lists the hottest methods by samples at the
  top of the stack (exclusive) and anywhere in it (inclusive), where the other threads were waiting
  or in native code, and GC counts and pauses; `--profile gc` shows collections per generation,
  pauses and the top allocating types. The `.nettrace` file goes to eyedbg's private traces
  directory (`<EYEDBG_HOME or ~/.eyedbg>/traces`, 0700, files 0600, removed after 7 days and beyond
  the newest 10) or to `--out`, never overwritten, for PerfView or `dotnet-trace convert`; `trace
  FILE` summarizes an existing `.nettrace`. A program stopped at a breakpoint is refused
  (`NOT_RUNNING`); one paused mid-trace fails after 30 s (`DIAGNOSTICS_TIMEOUT`); Ctrl-C discards
  the trace. Output shows names and counts, never values. New error code: `TRACE_UNSUPPORTED`
  (exit 2). `version --json` features add `"dotnet.trace"`.
- The VS Code extension's .NET views (P2-M9, ADR 0015 addendum): an *EyeDebugger .NET* container
  in the activity bar, shown in workspaces with a .NET project (or after **EyeDebugger: Show .NET
  Views**), with **Counters** (live `eyedbg dotnet counters --watch`: values, changes, totals;
  Pause/Resume), **Memory** (take a heap dump, top types, GC root paths when a type is expanded,
  Show Threads of This Dump, Copy Dump Path, Open Dump File…), **Threads** (stacks grouped like the
  CLI's text, locks with owners and waiters; click a frame to open its source) and **CPU Trace**
  (CPU or GC trace for 1 s–5 min with a countdown, Open Trace File…). One target for all four — the
  active eyedbg debug session, a session or a process from `eyedbg dotnet ps` (**EyeDebugger:
  Choose .NET Target…**); every operation cancellable; dumps and traces stay in eyedbg's private
  directory. The extension's API gains `dotnet()` and the four trees in `views()`.
- SharpDbg as dotnet's alternative adapter (P2-M10, ADR 0017): `eyedbg adapters install sharpdbg`
  fetches SharpDbg.Cli 0.1.17 from nuget.org (pinned, SHA-256 and size checked; opt-in, never
  shipped with eyedbg: it bundles a Microsoft-licensed DAP library, not open source — see `eyedbg
  help adapters install`), run on your `dotnet` with the .NET 10+ runtime. `start`, `attach` and
  `test` take `--adapter netcoredbg|sharpdbg` (else `EYEDBG_DOTNET_ADAPTER`); where netcoredbg has
  no build an installed SharpDbg is the default on Intel Macs; on Windows on Arm, where its CI e2e
  fails intermittently, only `--adapter sharpdbg` picks it (unverified). Under SharpDbg, eval
  runs lambdas and LINQ and shows `[DebuggerDisplay]` values, `set` evaluates the assignment, and
  pause is refused (`UNSUPPORTED_BY_ADAPTER`: SharpDbg stops the program without saying where).
  Adapter manifests gain `adapter.runtime: "dotnet"` and a `dotnet.minRuntime` block; `adapters
  doctor dotnet` checks both adapters (one usable is enough) and says which one sessions use;
  `adapters ls` shows `for: dotnet (--adapter sharpdbg)` (`alternateFor` in `--json`). The VS Code
  extension's launch configuration takes `"adapter"` (it needs an eyedbg with `adapter.select`).
- `version --json` lists the feature `adapter.select`; sessions report their adapter
  (`SessionInfo.adapter` in `--json`).
- `eyedbg test dotnet` debugs Microsoft.Testing.Platform, xUnit v3 and TUnit test projects
  (ADR 0010's amendment): detected before the build (a `global.json`/`DOTNET_TEST_RUNNER` naming
  Microsoft.Testing.Platform, `EnableMSTestRunner`/`EnableNUnitRunner`/
  `TestingPlatformDotnetTestSupport`/`IsTestingPlatformApplication`/`UseMicrosoftTestingPlatformRunner`
  true, `MSTest.Sdk` without `UseVSTest`, a reference to `xunit.v3`/`xunit.v3.core`/their
  `mtp-vN` flavors, or a `TUnit`/`TUnit.Engine` reference — in the project or a
  `Directory.Build.props`/`.targets` above it), eyedbg builds the project and launches its
  self-hosting test app directly under the adapter instead of attaching to a separate VSTest
  host — no runner, no attach, like `eyedbg start`. FILTER becomes the app's own filter option
  (`--filter` for the Microsoft.Testing.Platform frameworks and MTP-mode xUnit v3, xUnit v3
  native's `-filterVSTest` on 4.0+, TUnit's `--treenode-filter`); everything after `--` reaches
  the app's argv directly (refused for a VSTest run). The session ends with the app's own exit
  code (0 pass / 2 a test failed / 8 no test matched for the Microsoft.Testing.Platform
  frameworks; 0 pass or no test matched / 1 a test failed for xUnit v3 native), except where the
  adapter's exit codes aren't trusted (see the Fixed entry below), when it ends without one. A
  VSTest project's flow (`dotnet test`, attach to the host) is unchanged. New sample apps
  `testdata/apps/dotnet/xunit3` and `dotnet/mstest`.
- Stops name the breakpoints they are for (ADR 0018): when eyedbg decided a breakpoint stop (a
  line whose clients' breakpoints have different conditions, or `--hit`/`--log`), the snapshot
  says `stopped for breakpoint 6 of human:ijat` (`breakpoints 5 of agent, 6 of human:ijat`) under
  its first line, `--json` carries `session.stop.breakpoints` (`[{"id", "owner"}]`, sorted by id;
  schema 1), `eyedbg events` appends `; for breakpoint 6 of human:ijat` to the stop, and the DAP
  facade's `stopped` carries `hitBreakpointIds` (live, at the join and on a resync), so VS Code
  selects the hit breakpoint. Absent when eyedbg didn't decide the stop. Recordings don't keep it.
- `eyedbg dap --launch [--as CLIENT]` (P2-S3a, ADR 0019): a DAP client's `launch` starts the
  session, as `eyedbg start` would, as that client (who holds the lease); the daemon is started if
  needed and `-s` is refused. Launch arguments: `lang` (required), `program`, `project`, `cwd`
  (relative paths against the command's directory), `args`, `env`, `opts`, `stopOnEntry`,
  `noBuild`, `leasePolicy`, `exceptions`, `adapter`; other keys are ignored, a key differing from
  one of these only in case is refused. `initialize` offers both exception filters (one the adapter
  can't serve is turned off for that client, with a console line); the build streams to that
  client's debug console as it runs (.NET: `dotnet build -tl:off`, then the program's path from
  `dotnet msbuild -getProperty:TargetPath`; bounded, never logged); once the adapter is ready the
  client gets an `eyedbg/session {sessionId}` event, the adapter's capabilities as a
  `capabilities` event, and `initialized`, and its breakpoints and exception filters are in place
  before the program runs. The launcher's `terminate` ends and forgets the session (`eyedbg stop`),
  so a Restart restarts the program; when the launcher leaves, an exited session is forgotten and a
  live one keeps running. A failed launch is the `launch` response's error. `facade.open` takes
  `launch` and reports `facadeVersion` 3; `version --json` lists `"dap.launch"`. An adapter's
  `runInTerminal` is still refused (terminals: S3b).

### Changed

- `eyedbg sessions` has an `ADAPTER` column after `LANG` ("-" for a lost session of an older
  daemon), and the first line of a stop snapshot names it: `session s-k3f9 (dotnet, netcoredbg)
  stopped: …`.

- The .NET helper ships ClrMD 4.1 (`Microsoft.Diagnostics.Runtime`) and its dependencies — among
  them Azure.Core, Azure.Identity and MSAL, which it never uses (ClrMD's symbol server, which the
  helper doesn't construct: dumps are analyzed without downloading anything); 35 files, 8.9 MB,
  all MIT, listed in `THIRD-PARTY-NOTICES.txt`. Some shared dependencies moved to 10.0.x. CI checks
  every archive carries `Microsoft.Diagnostics.Runtime.dll`.

- CI builds, tests (on .NET 8 and 10, Linux/macOS/Windows) and audits the .NET helper (`helper`
  job), publishes it in an unprivileged `helper-dist` job and checks every snapshot archive carries
  it and that the extracted Linux archive runs `eyedbg dotnet ps`; release.yml builds it in an
  unprivileged `helper` job and the release fails without it. The e2e job installs the .NET SDK on
  every platform (the helper e2e runs where netcoredbg has no build too). `task ci` also runs
  `helper:ci` (needs the .NET 10 SDK); `task build` builds the helper when `dotnet` is installed;
  Dependabot updates its NuGet packages.
- `task ci` now also runs the extension's checks (Node 24 and pnpm; `task ci:go` for Go alone),
  with `ext:*` tasks for each; CI has an `extension` job (Linux, macOS, Windows) running them and
  the extension's integration suite in VS Code 1.100.0 and 1.139.0.
- An editor's breakpoint list (DAP `setBreakpoints`) no longer removes breakpoints its client set
  with the CLI; only the ones set through an editor — and not the ones it set under another path
  of the same file (a symlink). A `setBreakpoints` or `setFunctionBreakpoints` of more than 1000
  entries is refused (`INVALID_REQUEST`). `eyedbg sessions` has a `CONNECTED` column
  before `PROGRAM`, and the sharing line of a stop also shows while an editor is connected or a
  lease request is pending.
- The DAP facade marks a stop caused by another client's continue, step, run-until or pause with
  `preserveFocusHint` (ADR 0014 addendum), so an editor keeps its focus when the agent steps; the
  replayed stop at the join and the editor's own stops are unchanged.
- Breakpoints of several clients at one line each keep their own condition, hit count and log
  message (ADR 0018): the program stops there when any of them would, and another client's
  logpoint never suppresses a stop. Where the conditions differ (or one is unconditional), the
  adapter still gets one unconditional breakpoint there and eyedbg evaluates each condition at
  every pass (a brief pause, as for `--hit`/`--log`) — before, the line stopped unconditionally,
  or, while some breakpoint had `--hit`/`--log`, only when one condition held. The "shares its
  line … it stops there unconditionally" note is gone; function breakpoints keep the
  unconditional merge, noted "shares its function with another client's breakpoint that has a
  different condition: it stops there unconditionally".
- A condition eyedbg evaluates itself (a shared line whose clients' conditions differ) holds
  unless its value reads as false (`false`, `0`, `None`, `null`, `nil`, an empty string or
  collection); before, only a `true` result did, which skipped stops for a C comparison (`1`) and
  Python truthy values. A condition that fails to evaluate still stops there; elsewhere —
  including a lone `--hit`/`--log` breakpoint's `--if` — the adapter still decides, and
  `bp add --help` now says that debugpy ignores a failing condition it evaluates itself.
- `run-until` at a line where another client has a breakpoint reports `reached: false` when the
  program stopped there only for that client's breakpoint (its condition held, the run-until's
  `--if` didn't); the missed line names the condition (`did not reach Program.cs:9 with i == 3`).

### Fixed

- Windows: the daemon no longer exits during start-up ("write token: … Access is denied",
  `DAEMON_START_FAILED`) when a client reads the token a killed daemon left behind while the new
  one replaces it; clients now open the token sharing delete, and the daemon replaces it with a
  POSIX-semantics rename, so the replace no longer waits on (or fails because of) readers. The
  2 s retry remains, as a backstop, for readers that don't share delete (an older `eyedbg` binary
  mid-upgrade, antivirus, indexers) and for filesystems where POSIX rename falls back to a plain
  one (FAT, older Windows).
- Windows: `run-until` reports `reached` when the adapter spells the file differently (Delve's
  `C:/…` with forward slashes). The daemon resolves a start's `clientDir` (native API) like
  `program` and `cwd`, so a client that sends it as an 8.3 short name (`C:\Users\RUNNER~1\…`) no
  longer gets Go's build refused with "outside main module" (the CLI already sent it resolved).
- macOS: sessions under netcoredbg no longer claim exit code 0 for `start`, `attach` and launched
  test runs regardless of the program's real exit code. netcoredbg reports every one as 0 there;
  `exitCode` is now absent instead (`--json`'s `SessionInfo`/`Event`, both already optional: no
  schema change) and `endReason` says it is unknown and why, with a "read the test output" hint
  for a test run. `--adapter sharpdbg` is unaffected.

## [0.1.2] - 2026-09-24

### Fixed

- A breakpoint in a file that doesn't exist (e.g. a bare `Orders.cs:20` from a directory without
  it) is refused with `INVALID_REQUEST` and a hint to give the path from the working directory,
  instead of staying pending forever.

## [0.1.1] - 2026-09-24

### Fixed

- `eyedbg test dotnet` refuses xUnit v3 projects with `NO_TEST_HOST` and a hint to debug the test
  app with `eyedbg start`: xUnit v3 runs the tests outside the test host eyedbg attaches to, so
  the run passed without ever stopping at a verified breakpoint.
- With no `-s`, a session that has exited no longer counts as another session: commands pick the
  one session that hasn't exited instead of failing with "several sessions exist".

## [0.1.0] - 2026-09-24

### Added

- Ship (milestone 6): `eyedbg skill print` (writes SKILL.md to stdout) and `eyedbg skill install [--dir ROOT] [--force]` (installs it to a skills root, Claude Code's personal one by default). CLI end-to-end tests drive the real `eyedbg`/`eyedbgd` binaries against the sample apps (`internal/e2e/`), covering autostart, session status, run-until, `vars`, `eval` (including `SIDE_EFFECTS`), lease contention, an unsupported `attach`, logpoints, `events` and daemon shutdown, on both a fake in-process adapter and real Python/.NET ones; `drivers/dotnet` and `drivers/generic` keep the driver-level e2e coverage for `eyedbg test` and a real `attach`. `task e2e` (`COUNT=5` before merging an e2e change) and `task lint:workflows` (actionlint). CI now runs on 6 OS/architecture targets (Linux, macOS, Windows × x64/arm64) with a real end-to-end job driving .NET (netcoredbg) and Python (debugpy) — .NET is skipped on Intel Macs and Windows on Arm, where netcoredbg has no build — plus a workflow-linting job.
- Python (milestone 7, ADR 0011): `eyedbg start python --program app.py` (or `--opt module=pytest`) debugs Python through debugpy 1.8.22, with breakpoints of every kind, exception stops, eval (refusing calls, assignments and f-strings without `--allow-side-effects`), `set`, stepping and `run-until`; child processes run undebugged and `attach python` is `UNSUPPORTED_BY_ADAPTER`. The interpreter is `--opt python=PATH`, `EYEDBG_PYTHON`, the caller's `$VIRTUAL_ENV` (read by the CLI and sent in the start request, like the client's directory), a project `.venv`/`venv`, or `python3`/`python`/`py -3`; `eyedbg adapters install python` downloads the pinned pure-Python debugpy wheel (SHA-256 and size checked) so no pip is needed. Python is served by an adapter manifest alone: a declarative JSON format (bundled, or yours in `~/.eyedbg/adapters` — `EYEDBG_CONFIG_DIR` overrides the `~/.eyedbg` part, `EYEDBG_HOME` overrides `~/.eyedbg` itself; permission-checked on Unix, never read from a project) and a generic driver add languages without Go code ([docs/adapter-manifests.md](docs/adapter-manifests.md)). `eyedbg adapters ls` (text and `--json`) lists adapters, languages, options and ignored manifests; `start --opt NAME=VALUE` passes a language's options. Sample app `testdata/apps/python/basic` and end-to-end tests.
- Breadth (milestone 5, ADR 0010): `bp add` takes `FILE@"TEXT"` (the line holding TEXT; `ANCHOR_NOT_FOUND` with the closest lines, `ANCHOR_AMBIGUOUS`) and `func:NAME` (function breakpoints), plus `--hit N|>=N|%N` and `--log "msg {expr}"` (logpoints, output category `logpoint`), both emulated by the daemon; `bp ls` shows hit counts and notes breakpoints in files changed since the session started. `bp exceptions [all|uncaught|none] [--force]` (per client, merged) and `start/attach/test --exceptions`; an exception stop shows the exception's type, message, stack and inner exceptions. `eval` refuses expressions that visibly change the program (`SIDE_EFFECTS`) unless `--allow-side-effects` (leased and logged), and takes `--depth`. `eyedbg set VAR VALUE`. `eyedbg attach dotnet --pid N` (own processes only; `ATTACH_FAILED`) and `eyedbg detach`. `eyedbg test dotnet [FILTER]` debugs a `dotnet test` (VSTest) run and ends with its exit code (`NO_TEST_HOST` for Microsoft.Testing.Platform projects). `UNSUPPORTED_BY_ADAPTER` (exit 4) for what the adapter can't do. Sample apps in `testdata/apps/dotnet` and end-to-end tests for all of it.
- Model for P2 (milestone 4, ADR 0009): several clients share a session. Every request says who sends it (`--as agent[:NAME]|human[:NAME]`, else `EYEDBG_CLIENT`, else `agent`). Breakpoints belong to the client that added them (`bp ls --mine`, `bp rm --force`, `NOT_OWNER`); clients' breakpoints on one line share one adapter breakpoint. A control lease decides who may run, step, pause or stop the program: `eyedbg lease [status|take|release|grant|policy]`, `start --lease-policy free|handoff|human-priority`, `LEASE_HELD` (exit 2). A per-session event log (who did what, stops, output, breakpoints, lease changes): `eyedbg events [--since N] [--kind ...] [--wait]`. Sessions a crashed daemon lost are listed as `lost` (also with no daemon) and forgotten with `stop -s ID`; each session's control events are recorded to a JSONL file in the runtime directory (no program output; `start --no-record` / `EYEDBG_NO_RECORD=1`). `sessions` and snapshots show the lease and clients.

- Agent ergonomics (milestone 3): every stop snapshot carries the program's output since it resumed and, via `--dump changed|locals|stack|none` (default `changed`), the locals that changed since the previous stop with their old values. `eyedbg run-until FILE:LINE [--if EXPR]` continues to a line through a temporary breakpoint and says whether it got there; `bp add --if EXPR` sets conditional breakpoints. `vars --changed` and `vars --expand PATH` (members first, then evaluation, e.g. `items[1]`). A global `--budget` (tokens, default 2000) cuts variables breadth-first and says so. `eyedbg help --all` prints every command's help at once; `--json` works on any help. With `--json`, errors are JSON on stdout; exit codes now map to error classes (1 usage/internal, 2 session state, 3 setup, 4 adapter).
- .NET debugging (milestone 2): `eyedbg start dotnet` builds a project (or takes `--program`) and runs it under netcoredbg, with `--bp FILE:LINE` breakpoints set before it runs. Session commands `sessions`, `status`, `continue`, `next`, `step-in`, `step-out`, `pause`, `wait`, `stack`, `vars`, `eval`, `output`, `bp add|ls|rm` and `stop`, each with text and `--json` output; every execution command reports where the program stopped. `eyedbg adapters install netcoredbg` downloads the pinned netcoredbg release (SHA-256 verified) and `eyedbg adapters doctor` checks the toolchain. New dependency: github.com/google/go-dap (Apache-2.0), the DAP message types delve uses.
- Daemon lifecycle (milestone 1): `eyedbgd` runs as one per-user daemon on a Unix socket in a user-only runtime directory, with a per-start token, an exclusive lifetime lock, idle exit (`--idle-timeout`, default 30m) and clean shutdown. `eyedbg daemon start|status|stop|logs` (text and `--json`); clients auto-start the daemon, replace an idle daemon of another build, and honor `EYEDBG_NO_AUTOSTART`, `EYEDBG_RUNTIME_DIR` and `EYEDBG_DAEMON_PATH`.
- Project scaffold following docs/DESIGN.md §12: `eyedbg` and `eyedbgd` binaries (only `version` is implemented). No MCP server by default; the CLI is the agent interface.
- `eyedbg version` with text and `--json` (`"schema": 1`) output; version injected at link time. Every command has tested `Short`/`Long`/`Example` help, reachable via `<path> --help` and `help <path>`.
- CI: lint, race-enabled tests, cross-builds for 6 OS/architecture targets, govulncheck, GoReleaser snapshot; Dependabot.
- Documentation: contributing guide (DCO), code conventions, maintainer guide, security policy, governance, code of conduct, ADRs 0001–0008; ADR 0011 and docs/adapter-manifests.md for adapter manifests.

### Changed

- eyedbg's config and data now live under `~/.eyedbg` on every OS (manifests in
  `~/.eyedbg/adapters/*.json`, installed adapters in `~/.eyedbg/tools/`), replacing the OS-specific
  user config/cache directories. A new `EYEDBG_HOME` overrides `~/.eyedbg`; `EYEDBG_CONFIG_DIR` and
  `EYEDBG_DATA_DIR` still override their own specific parts. The runtime directory
  (socket/token/sessions) is unaffected. This is unreleased, so there is no migration: reinstall
  adapters after upgrading (`eyedbg adapters install netcoredbg` / `python`), or set `EYEDBG_HOME`
  or `EYEDBG_DATA_DIR` to the old location.
- A native (non-.NET) manifest-driven adapter's `ATTACH_FAILED` now carries a generic hint about
  the OS's own attach restrictions (Linux `ptrace_scope`, macOS `task_for_pid`); .NET's hint is
  unchanged (CoreCLR's debugger doesn't need either).
- CI's `e2e` job runs on `ubuntu-latest` only for `push`/`pull_request`; the full 6-platform matrix
  now runs only on `workflow_dispatch` and the weekly `schedule` (the `test` job's unit-test matrix
  is unchanged). The matrix is computed by a small `e2e-matrix` job from `github.event_name` so
  `e2e` always has at least one entry and is never itself skipped, which would otherwise fail
  `ci-ok`.
- CI's unit-test matrix grew from Linux/macOS/Windows to 6 OS/architecture targets (adds `ubuntu-24.04-arm`, `macos-15-intel`, `windows-11-arm`; the race detector is skipped on windows/arm64).
- README replaces "there is nothing to install yet" with an Install section (build from a clone, install adapters) and a Quickstart for agents.
- netcoredbg's pinned release, digests, lookup order and launch arguments moved from Go code into its bundled manifest (`internal/adapters/manifests/netcoredbg.json`); install, doctor and start behave as before (an install whose archive lacks the adapter now also removes what it extracted).
- `adapters install` takes an adapter or a language (`install python`); `adapters doctor [adapter|language...]` checks every adapter, including debugpy and its interpreter. Without arguments, an adapter that isn't installed (or the dotnet host) is a `missing` line (`"missing": true` in `--json`) and doesn't fail the check; named ones, broken ones and broken user manifests exit 1.
- Stop snapshots and `vars --changed` read only the scope an adapter marks as locals (`presentationHint: "locals"`) when it marks any, so a Python snapshot doesn't list module globals; netcoredbg is unaffected. `vars` still shows every scope.
- `start` sends the caller's working directory (`clientDir`) and `--opt` values (`options`) in the start request (additive); dotnet refuses `--opt`.
- `stop` on an attached session detaches (the program keeps running); `detach` refuses launched sessions and test runs.
- The .NET end-to-end tests build the sample apps in `testdata/apps/dotnet` (copied to a temporary directory) instead of generating a project.
- `bp ls` and breakpoint events render `func:NAME`, anchors, `hit` and `log`; `events` shows exception modes, side-effecting evals, sets, attaches and test runs.
- Protocol version 2: session commands refuse a daemon of another protocol (`VERSION_MISMATCH`; stop it with `eyedbg daemon stop`), and the `output` result is `{"lines", "more"}`.
- `sessions` prints a header line and a LEASE column; `bp ls` shows each breakpoint's owner (and a note when a line is shared with a different condition).
- `bp rm all` removes only your own breakpoints (`--force`: everyone's) and never fails for finding none; it reports how many of other clients' it kept.
- `output` sequence numbers are event-log sequence numbers: still increasing, now with gaps.
- `output` and `events` results are capped at 512 KiB (a hint on stderr says how to read on), output chunks at 64 KiB, and a snapshot's output at 20 chunks and 16 KiB.

### Fixed

- Concurrent breakpoint changes on one file could leave the adapter with an older list; `setBreakpoints` round trips are now serialized per session.
- Execution requests from several clients are serialized per session (state check, lease and send are atomic).
- An auto-started daemon that loses the start race exits quietly instead of writing errors to the shared log.
- A response over 1 MiB (e.g. `output` after a lot of program output) dropped the connection.
- Stopping a session that is also ending by itself no longer races over its end reason.

[Unreleased]: https://github.com/EyeDebugger/eyedebugger/compare/v0.1.2...HEAD
[0.1.2]: https://github.com/EyeDebugger/eyedebugger/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/EyeDebugger/eyedebugger/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/EyeDebugger/eyedebugger/releases/tag/v0.1.0
