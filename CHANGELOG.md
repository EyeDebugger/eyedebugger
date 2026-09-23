# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Agent ergonomics (milestone 3): every stop snapshot carries the program's output since it resumed and, via `--dump changed|locals|stack|none` (default `changed`), the locals that changed since the previous stop with their old values. `eyedbg run-until FILE:LINE [--if EXPR]` continues to a line through a temporary breakpoint and says whether it got there; `bp add --if EXPR` sets conditional breakpoints. `vars --changed` and `vars --expand PATH` (members first, then evaluation, e.g. `items[1]`). A global `--budget` (tokens, default 2000) cuts variables breadth-first and says so. `eyedbg help --all` prints every command's help at once; `--json` works on any help. With `--json`, errors are JSON on stdout; exit codes now map to error classes (1 usage/internal, 2 session state, 3 setup, 4 adapter).
- .NET debugging (milestone 2): `eyedbg start dotnet` builds a project (or takes `--program`) and runs it under netcoredbg, with `--bp FILE:LINE` breakpoints set before it runs. Session commands `sessions`, `status`, `continue`, `next`, `step-in`, `step-out`, `pause`, `wait`, `stack`, `vars`, `eval`, `output`, `bp add|ls|rm` and `stop`, each with text and `--json` output; every execution command reports where the program stopped. `eyedbg adapters install netcoredbg` downloads the pinned netcoredbg release (SHA-256 verified) and `eyedbg adapters doctor` checks the toolchain. New dependency: github.com/google/go-dap (Apache-2.0), the DAP message types delve uses.
- Daemon lifecycle (milestone 1): `eyedbgd` runs as one per-user daemon on a Unix socket in a user-only runtime directory, with a per-start token, an exclusive lifetime lock, idle exit (`--idle-timeout`, default 30m) and clean shutdown. `eyedbg daemon start|status|stop|logs` (text and `--json`); clients auto-start the daemon, replace an idle daemon of another build, and honor `EYEDBG_NO_AUTOSTART`, `EYEDBG_RUNTIME_DIR` and `EYEDBG_DAEMON_PATH`.
- Project scaffold following docs/DESIGN.md §12: `eyedbg` and `eyedbgd` binaries (only `version` is implemented). No MCP server by default; the CLI is the agent interface.
- `eyedbg version` with text and `--json` (`"schema": 1`) output; version injected at link time. Every command has tested `Short`/`Long`/`Example` help, reachable via `<path> --help` and `help <path>`.
- CI: lint, race-enabled tests on Linux, macOS and Windows, cross-builds for 6 OS/architecture targets, govulncheck, GoReleaser snapshot; Dependabot.
- Documentation: contributing guide (DCO), code conventions, maintainer guide, security policy, governance, code of conduct, ADRs 0001–0008.

[Unreleased]: https://github.com/EyeDebugger/eyedebugger/commits/main
