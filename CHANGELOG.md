# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Breadth (milestone 5, ADR 0010): `bp add` takes `FILE@"TEXT"` (the line holding TEXT; `ANCHOR_NOT_FOUND` with the closest lines, `ANCHOR_AMBIGUOUS`) and `func:NAME` (function breakpoints), plus `--hit N|>=N|%N` and `--log "msg {expr}"` (logpoints, output category `logpoint`), both emulated by the daemon; `bp ls` shows hit counts and notes breakpoints in files changed since the session started. `bp exceptions [all|uncaught|none] [--force]` (per client, merged) and `start/attach/test --exceptions`; an exception stop shows the exception's type, message, stack and inner exceptions. `eval` refuses expressions that visibly change the program (`SIDE_EFFECTS`) unless `--allow-side-effects` (leased and logged), and takes `--depth`. `eyedbg set VAR VALUE`. `eyedbg attach dotnet --pid N` (own processes only; `ATTACH_FAILED`) and `eyedbg detach`. `eyedbg test dotnet [FILTER]` debugs a `dotnet test` (VSTest) run and ends with its exit code (`NO_TEST_HOST` for Microsoft.Testing.Platform projects). `UNSUPPORTED_BY_ADAPTER` (exit 4) for what the adapter can't do. Sample apps in `testdata/apps/dotnet` and end-to-end tests for all of it.
- Model for P2 (milestone 4, ADR 0009): several clients share a session. Every request says who sends it (`--as agent[:NAME]|human[:NAME]`, else `EYEDBG_CLIENT`, else `agent`). Breakpoints belong to the client that added them (`bp ls --mine`, `bp rm --force`, `NOT_OWNER`); clients' breakpoints on one line share one adapter breakpoint. A control lease decides who may run, step, pause or stop the program: `eyedbg lease [status|take|release|grant|policy]`, `start --lease-policy free|handoff|human-priority`, `LEASE_HELD` (exit 2). A per-session event log (who did what, stops, output, breakpoints, lease changes): `eyedbg events [--since N] [--kind ...] [--wait]`. Sessions a crashed daemon lost are listed as `lost` (also with no daemon) and forgotten with `stop -s ID`; each session's control events are recorded to a JSONL file in the runtime directory (no program output; `start --no-record` / `EYEDBG_NO_RECORD=1`). `sessions` and snapshots show the lease and clients.

- Agent ergonomics (milestone 3): every stop snapshot carries the program's output since it resumed and, via `--dump changed|locals|stack|none` (default `changed`), the locals that changed since the previous stop with their old values. `eyedbg run-until FILE:LINE [--if EXPR]` continues to a line through a temporary breakpoint and says whether it got there; `bp add --if EXPR` sets conditional breakpoints. `vars --changed` and `vars --expand PATH` (members first, then evaluation, e.g. `items[1]`). A global `--budget` (tokens, default 2000) cuts variables breadth-first and says so. `eyedbg help --all` prints every command's help at once; `--json` works on any help. With `--json`, errors are JSON on stdout; exit codes now map to error classes (1 usage/internal, 2 session state, 3 setup, 4 adapter).
- .NET debugging (milestone 2): `eyedbg start dotnet` builds a project (or takes `--program`) and runs it under netcoredbg, with `--bp FILE:LINE` breakpoints set before it runs. Session commands `sessions`, `status`, `continue`, `next`, `step-in`, `step-out`, `pause`, `wait`, `stack`, `vars`, `eval`, `output`, `bp add|ls|rm` and `stop`, each with text and `--json` output; every execution command reports where the program stopped. `eyedbg adapters install netcoredbg` downloads the pinned netcoredbg release (SHA-256 verified) and `eyedbg adapters doctor` checks the toolchain. New dependency: github.com/google/go-dap (Apache-2.0), the DAP message types delve uses.
- Daemon lifecycle (milestone 1): `eyedbgd` runs as one per-user daemon on a Unix socket in a user-only runtime directory, with a per-start token, an exclusive lifetime lock, idle exit (`--idle-timeout`, default 30m) and clean shutdown. `eyedbg daemon start|status|stop|logs` (text and `--json`); clients auto-start the daemon, replace an idle daemon of another build, and honor `EYEDBG_NO_AUTOSTART`, `EYEDBG_RUNTIME_DIR` and `EYEDBG_DAEMON_PATH`.
- Project scaffold following docs/DESIGN.md §12: `eyedbg` and `eyedbgd` binaries (only `version` is implemented). No MCP server by default; the CLI is the agent interface.
- `eyedbg version` with text and `--json` (`"schema": 1`) output; version injected at link time. Every command has tested `Short`/`Long`/`Example` help, reachable via `<path> --help` and `help <path>`.
- CI: lint, race-enabled tests on Linux, macOS and Windows, cross-builds for 6 OS/architecture targets, govulncheck, GoReleaser snapshot; Dependabot.
- Documentation: contributing guide (DCO), code conventions, maintainer guide, security policy, governance, code of conduct, ADRs 0001–0008.

### Changed

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

[Unreleased]: https://github.com/EyeDebugger/eyedebugger/commits/main
