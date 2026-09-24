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
  `"features": ["dap"]`.
- Stops carry `allThreadsStopped` when the adapter says so (`--json` snapshots and events).
- C, C++ and Rust debugging through LLVM's lldb-dap (`eyedbg start c|cpp|rust --program FILE`),
  manifest only (`lldb-dap-c.json`, `lldb-dap-cpp.json`, `lldb-dap-rust.json`; `EYEDBG_LLDB_DAP` or
  PATH; no managed download). Go debugging through Delve (`eyedbg start go --program DIR`,
  `--opt mode=debug|exec|test`, `--opt buildFlags`), served by a new
  `adapter.transport: "connect"` (a private Unix socket the adapter dials in to) since `dlv dap`
  never speaks stdio; `eyedbg adapters install delve` downloads the pinned release. A new
  `${envList}` launch template variable lets `lldb-dap`'s pre-20 `--env` array shape work from a
  manifest. Manifest schema, session and trust model unchanged otherwise (ADR 0013).

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
