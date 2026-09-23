# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- .NET debugging (milestone 2): `eyedbg start dotnet` builds a project (or takes `--program`) and runs it under netcoredbg, with `--bp FILE:LINE` breakpoints set before it runs. Session commands `sessions`, `status`, `continue`, `next`, `step-in`, `step-out`, `pause`, `wait`, `stack`, `vars`, `eval`, `output`, `bp add|ls|rm` and `stop`, each with text and `--json` output; every execution command reports where the program stopped. `eyedbg adapters install netcoredbg` downloads the pinned netcoredbg release (SHA-256 verified) and `eyedbg adapters doctor` checks the toolchain. New dependency: github.com/google/go-dap (Apache-2.0), the DAP message types delve uses.
- Daemon lifecycle (milestone 1): `eyedbgd` runs as one per-user daemon on a Unix socket in a user-only runtime directory, with a per-start token, an exclusive lifetime lock, idle exit (`--idle-timeout`, default 30m) and clean shutdown. `eyedbg daemon start|status|stop|logs` (text and `--json`); clients auto-start the daemon, replace an idle daemon of another build, and honor `EYEDBG_NO_AUTOSTART`, `EYEDBG_RUNTIME_DIR` and `EYEDBG_DAEMON_PATH`.
- Project scaffold following docs/DESIGN.md §12: `eyedbg` and `eyedbgd` binaries (only `version` is implemented). No MCP server by default; the CLI is the agent interface.
- `eyedbg version` with text and `--json` (`"schema": 1`) output; version injected at link time. Every command has tested `Short`/`Long`/`Example` help, reachable via `<path> --help` and `help <path>`.
- CI: lint, race-enabled tests on Linux, macOS and Windows, cross-builds for 6 OS/architecture targets, govulncheck, GoReleaser snapshot; Dependabot.
- Documentation: contributing guide (DCO), code conventions, maintainer guide, security policy, governance, code of conduct, ADRs 0001–0008.

[Unreleased]: https://github.com/EyeDebugger/eyedebugger/commits/main
