# EyeDebugger

`eyedbg`: an AI-native, CLI-first debugger. Coding agents and humans drive the same live debug session.

[![CI](https://github.com/EyeDebugger/eyedebugger/actions/workflows/ci.yml/badge.svg)](https://github.com/EyeDebugger/eyedebugger/actions/workflows/ci.yml)

> **Beta (v0.2.1).** Debug .NET (netcoredbg, or the opt-in SharpDbg), Python (debugpy), C, C++,
> Rust (lldb-dap) and Go (Delve) programs on Linux, macOS and Windows (x64 and arm64; .NET on Intel
> Macs through SharpDbg only, `eyedbg adapters install sharpdbg`, and on Windows on Arm with
> `--adapter sharpdbg`, unverified; C/C++/Rust need
> lldb-dap on PATH or `EYEDBG_LLDB_DAP`, no managed download). Install below.

## Why

Every existing agent debugger is either an MCP server or IDE-bound, and none lets an agent and a
human both *drive* one session: mcp-debugger's IDE view is read-only, and delve's multiclient has
no event fan-out. EyeDebugger's daemon owns the session, so a stateless CLI — with complete
built-in help, the agent interface — and a VS Code extension can both attach to the same live
debug session.

## How it works

```
eyedbg CLI (stateless) ──┐
                         ├── local IPC (JSON-RPC 2.0) ──► eyedbgd (daemon, per user)
VS Code extension      ──┘     + per-session DAP facade            │
                                                                   ├── Session ── DAP ──► adapter process
                                                                   │   (netcoredbg | sharpdbg | debugpy | lldb-dap | delve)
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
  [ADR 0013](docs/adr/0013-socket-transport-and-native-manifest-languages.md)).

Full design: [docs/DESIGN.md](docs/DESIGN.md).

## VS Code

Join an agent's live session from VS Code, or start your own and let the agent join you:

```sh
code --install-extension eyedebugger_0.2.1_vscode.vsix   # from a GitHub release
```

```json
{ "type": "eyedbg", "request": "attach", "name": "Join eyedbg session" }
```

```json
{ "type": "eyedbg", "request": "launch", "name": "Debug app.py", "lang": "python", "program": "${workspaceFolder}/app.py" }
```

Full guide: [extensions/vscode/README.md](extensions/vscode/README.md), `eyedbg help vscode`.

## Install

Give this prompt to your AI agent and it installs eyedbg for you:

<!-- install-prompt:start -->
```text
Please install eyedbg (EyeDebugger) on this machine for me. It's a free, open-source debugger that
AI coding agents drive from the terminal, and I can join the same debug session from VS Code.
Project: https://github.com/EyeDebugger/eyedebugger

1. Work out my OS (Linux, macOS or Windows) and CPU (x64 or arm64).

2. Before installing anything, ask me which of these I want. Use your question tool if you have
   one, otherwise show a numbered list; I can pick several.
   - eyedbg itself: the eyedbg CLI and its eyedbgd daemon (always installed)
   - .NET: netcoredbg (the default), and optionally SharpDbg (it bundles a Microsoft library under
     a license that isn't open source and may send data to Microsoft; `eyedbg help adapters
     install` explains)
   - Python: debugpy
   - Go: Delve
   - C, C++ and Rust: LLVM's lldb-dap (from my OS's LLVM package, not from eyedbg)
   - The VS Code extension
   - The eyedbg skill for you (the agent)

3. Open https://github.com/EyeDebugger/eyedebugger/releases. It lists releases newest first, each
   with its assets. Pick the release to use: the newest stable release whose assets include a file
   named exactly eyedebugger_<version>_vscode.vsix; if no stable release has one yet, the newest
   pre-release that does. Use that release's own <version> for everything below, and don't mix
   assets from two different releases. Download checksums.txt and the archive for my system:
   eyedebugger_<version>_<os>_<arch>.tar.gz on Linux and macOS, .zip on Windows, where <os> is
   linux, darwin or windows and <arch> is amd64 or arm64.

4. Check the archive before using it. checksums.txt has one "<sha256>  <file name>" line per file.
   Find the line whose file name is exactly the archive's name, compute the archive's SHA-256, and
   compare (ignore upper/lower case). If there is no such line, more than one, or the hashes differ,
   stop and tell me; don't continue without that check. If the gh CLI is installed, also run
   gh attestation verify <archive> -R EyeDebugger/eyedebugger and stop if it fails.

5. Unpack the whole archive into a folder of its own that my user owns (for example
   ~/.local/share/eyedbg, or %LOCALAPPDATA%\Programs\eyedbg on Windows). Keep eyedbg, eyedbgd and
   the helpers folder together in it. Add that folder to my PATH for new terminals (or link eyedbg
   and eyedbgd into a folder already on PATH). Don't use sudo or admin rights unless I say so.

6. Run: eyedbg version

7. For each language I picked, run the matching command:
   eyedbg adapters install netcoredbg
   eyedbg adapters install sharpdbg
   eyedbg adapters install debugpy
   eyedbg adapters install delve
   For C, C++ or Rust, install lldb-dap 18 or newer with my OS's package manager (ask me first). If
   it isn't on PATH as lldb-dap, set EYEDBG_LLDB_DAP to its full path.
   Then run eyedbg adapters doctor with the languages I picked (for example:
   eyedbg adapters doctor dotnet python) and fix what it reports.

8. VS Code extension: download eyedebugger_<version>_vscode.vsix from the same release, and check
   it exactly as in step 4: checksum against checksums.txt, then gh attestation verify if the gh
   CLI is installed. Then run:
   code --install-extension <the .vsix file>

9. The skill: eyedbg skill install puts it where Claude Code looks. For another agent, use
   eyedbg skill install --dir <your skills folder>. Tell me to restart you so you load it.

Only use the debug adapters eyedbg installs or finds itself. Don't download or set up any other
debugger. When you're done, tell me what you installed, where, and anything I still need to do.
```
<!-- install-prompt:end -->

Or install it by hand. Download an archive for your platform from
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

Claude Code users can also add the skill as a plugin (it still needs the `eyedbg` binary above):

```sh
/plugin marketplace add EyeDebugger/claude-plugin
/plugin install eyedbg@eyedebugger
```

From a clone: `task build` (or `go build -trimpath -o bin/ ./cmd/...`); `task snapshot` builds
release-shaped archives (and the VSIX's checksum) into `dist/`.

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

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). Third-party Go dependencies statically
linked into `eyedbg`/`eyedbgd` are listed in [THIRD-PARTY-NOTICES.txt](THIRD-PARTY-NOTICES.txt);
the .NET side helper's are in [helpers/dotnet/THIRD-PARTY-NOTICES.txt](helpers/dotnet/THIRD-PARTY-NOTICES.txt).
