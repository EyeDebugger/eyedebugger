# EyeDebugger

`eyedbg`: an AI-native, CLI-first debugger. Coding agents and humans drive the same live debug session.

[![CI](https://github.com/EyeDebugger/eyedebugger/actions/workflows/ci.yml/badge.svg)](https://github.com/EyeDebugger/eyedebugger/actions/workflows/ci.yml)

> **Alpha, unreleased.** Debug .NET (netcoredbg) and Python (debugpy) programs on Linux, macOS and
> Windows (x64 and arm64; .NET not on Intel Macs or Windows on Arm). No release yet: build from
> source.

## Why

Every existing agent debugger is either an MCP server or IDE-bound, and none lets an agent and a
human both *drive* one session: mcp-debugger's IDE view is read-only, and delve's multiclient has
no event fan-out. EyeDebugger's daemon owns the session, so a stateless CLI — with complete
built-in help, the agent interface — and (phase 2) a VS Code extension can both attach to the same
live debug session.

## How it works

```
eyedbg CLI (stateless) ──┐
                         ├── local IPC (JSON-RPC 2.0) ──► eyedbgd (daemon, per user)
VS Code extension (P2) ──┘     + per-session DAP facade (P2)       │
                                                                   ├── Session ── DAP ──► adapter process
                                                                   │   (netcoredbg | sharpdbg | debugpy | dlv | lldb-dap | js-debug)
                                                                   └── Side helpers (JSON-RPC over stdio)
                                                                       └── eyedbg-dotnet-helper (C#): ClrMD, EventPipe, dumps
```

- The CLI is stateless; every invocation talks to the daemon over local IPC.
- A per-user daemon owns sessions: clients, the control lease, breakpoint ownership, the event log
  and stop snapshots.
- Languages plug in via Debug Adapter Protocol (DAP) adapters, each described by a JSON adapter
  manifest ([docs/adapter-manifests.md](docs/adapter-manifests.md)). .NET is first, via netcoredbg —
  never vsdbg (see [ADR 0005](docs/adr/0005-never-use-vsdbg.md)); Python works through debugpy with a
  manifest alone, no language-specific Go ([ADR 0011](docs/adr/0011-declarative-adapter-manifests-and-their-trust-model.md)).

Full design: [docs/DESIGN.md](docs/DESIGN.md).

## Roadmap

1. ✅ Skeleton: daemon auto-start, IPC + token, version handshake, `daemon start/status/stop/logs`.
2. ✅ DAP core: go-dap client, netcoredbg install/doctor, `start` a console app, `bp add` (line), `continue`, `status`, `stack`, `vars`, `stop`.
3. ✅ Agent ergonomics: stop snapshot, `--dump`, `run-until`, `wait`, `--changed`, budgets, JSON schema, errors, `help --all`.
4. ✅ Model for P2: client identity, breakpoint ownership merge, lease, event log + `events --since`.
5. ✅ Breadth: hit counts, logpoints, function and exception breakpoints, eval with side effects, `set`, `attach`/`detach`, `test`, anchors.
6. ✅ Ship: SKILL.md, CI matrix (6 os/arch), e2e tests driving sample apps.
7. ✅ Second language via manifest only: Python through debugpy (`eyedbg start python`), adapter manifests, `adapters ls`, `--opt`.

Phase 1 (MVP) is complete. Phase 2: DAP facade + VS Code extension; .NET side helper; SharpDbg
adapter; more languages.

## Install

Requires Go 1.26+. No published release yet — build from a clone:

```sh
task build            # or: go build -trimpath -o bin/ ./cmd/...
```

Keep `eyedbg` and `eyedbgd` together on `PATH`. Then, once per machine:

```sh
eyedbg adapters install netcoredbg   # or: python
eyedbg adapters doctor
```

`task snapshot` (GoReleaser) builds release-shaped archives into `dist/` locally, for testing the
packaging; there is no `go install …@latest` yet since the repo is private.

## Quickstart for agents

```sh
eyedbg skill install                              # writes SKILL.md, once per machine
eyedbg start dotnet --project src/App --bp 'Orders.cs@"var total ="'
eyedbg run-until Orders.cs:42 --if 'i == 3'
eyedbg vars --changed
eyedbg eval 'order.Items.Count'
eyedbg stop                                       # always, when done
```

`eyedbg help --all` prints every command's help in one read. Full guide: [skill/eyedbg/SKILL.md](skill/eyedbg/SKILL.md).

## For AI agents

- [AGENTS.md](AGENTS.md): rules for contributing as, or with, an AI agent.
- [skill/eyedbg/SKILL.md](skill/eyedbg/SKILL.md): using `eyedbg` from an agent.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

See [SECURITY.md](SECURITY.md).

## Code of Conduct

See [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
