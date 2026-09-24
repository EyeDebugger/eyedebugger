# EyeDebugger

`eyedbg`: an AI-native, CLI-first debugger. Coding agents and humans drive the same live debug session.

[![CI](https://github.com/EyeDebugger/eyedebugger/actions/workflows/ci.yml/badge.svg)](https://github.com/EyeDebugger/eyedebugger/actions/workflows/ci.yml)

> **Alpha (v0.1).** Debug .NET (netcoredbg), Python (debugpy), C, C++, Rust (lldb-dap) and Go
> (Delve) programs on Linux, macOS and Windows (x64 and arm64; .NET not on Intel Macs or Windows
> on Arm; C/C++/Rust need lldb-dap on PATH or `EYEDBG_LLDB_DAP`, no managed download). Install
> below.

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
                                                                   │   (netcoredbg | debugpy | lldb-dap | delve; sharpdbg, js-debug planned)
                                                                   └── Side helpers (JSON-RPC over stdio)
                                                                       └── eyedbg-dotnet-helper (C#): ClrMD, EventPipe, dumps
```

- The CLI is stateless; every invocation talks to the daemon over local IPC.
- A per-user daemon owns sessions: clients, the control lease, breakpoint ownership, the event log
  and stop snapshots.
- Languages plug in via Debug Adapter Protocol (DAP) adapters, each described by a JSON adapter
  manifest ([docs/adapter-manifests.md](docs/adapter-manifests.md)). .NET is first, via netcoredbg —
  never vsdbg (see [ADR 0005](docs/adr/0005-never-use-vsdbg.md)); Python, C, C++, Rust and Go work
  through a manifest alone, no language-specific Go code
  ([ADR 0011](docs/adr/0011-declarative-adapter-manifests-and-their-trust-model.md),
  [ADR 0012](docs/adr/0012-socket-transport-and-native-manifest-languages.md)).

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

More languages, done independently of phase 2's own sequencing: C, C++ and Rust via lldb-dap, and
Go via Delve over a new socket transport (`adapter.transport: "connect"`) — manifest only, no new
Go driver ([ADR 0012](docs/adr/0012-socket-transport-and-native-manifest-languages.md)). Ruby,
Java, Kotlin, JS/TS and PHP were scoped and descoped in the same pass; each needs more than a
manifest.

## Install

Download an archive for your platform from
[Releases](https://github.com/EyeDebugger/eyedebugger/releases) (`gh attestation verify <archive>
-R EyeDebugger/eyedebugger` checks it was built by this repo's release workflow), or with Go 1.26+:

```sh
go install github.com/eyedebugger/eyedebugger/cmd/eyedbg@latest github.com/eyedebugger/eyedebugger/cmd/eyedbgd@latest
```

Keep `eyedbg` and `eyedbgd` together on `PATH` (`go install` puts both in `$(go env GOPATH)/bin`).
Then, once per machine:

```sh
eyedbg adapters install netcoredbg   # or: python, delve (c, cpp and rust use lldb-dap on PATH)
eyedbg adapters doctor
```

From a clone: `task build` (or `go build -trimpath -o bin/ ./cmd/...`); `task snapshot` builds
release-shaped archives into `dist/`.

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
