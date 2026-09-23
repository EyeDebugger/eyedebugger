# EyeDebugger (`eyedbg`) — AI-native debugger (design)

Status: draft v0.1 · 2026-09-23

## 1. What and why

A CLI-first, cross-platform debugger built for AI coding agents (Claude Code, Codex, …) and, in phase 2, for humans in VS Code **sharing the same live session**. The CLI is stateless; a per-user daemon owns sessions; sessions are addressed by short IDs. Languages plug in via the Debug Adapter Protocol (DAP). .NET is the first-class target.

Gap in prior art (see §15): every existing agent debugger is an MCP server or IDE-bound, and none lets agent and human both *drive* one session — mcp-debugger's IDE view is read-only, delve's multiclient has no event fan-out.

### Goals
- Full debugging for agents: launch/attach, breakpoints (line, function, conditional, hit-count, logpoint), exception breakpoints, step, pause, threads, stack, scopes/variables, evaluate, set variable.
- Agent-shaped output: structured, budgeted, one call per useful step.
- Pluggable languages without touching the core.
- Session model that already supports multiple clients, ownership, and control handoff — so the VS Code extension is additive.
- Win/macOS/Linux, x64 + arm64, single binary.

### Non-goals (MVP)
- Our own debug engine (we drive existing DAP adapters).
- Hot reload / Edit & Continue (not available outside Microsoft tooling for .NET).
- Remote/cross-machine sessions.
- Time-travel debugging.
- MCP wrapper (§10): the CLI is the agent interface; a thin MCP wrapper is optional future work.

## 2. Architecture

```
eyedbg CLI (stateless) ──┐
                         ├── local IPC (JSON-RPC 2.0) ──► eyedbgd (daemon, per user)
VS Code extension (P2) ──┘     + per-session DAP facade (P2)       │
                                                                   ├── Session ── DAP ──► adapter process
                                                                   │   (netcoredbg | sharpdbg | debugpy | dlv | lldb-dap | js-debug)
                                                                   └── Side helpers (JSON-RPC over stdio)
                                                                       └── eyedbg-dotnet-helper (C#): ClrMD, EventPipe, dumps
```

- **Language: Go.** Cross-compiles every OS/arch from one host, ~5 ms process start (matters: agents invoke the CLI many times), `google/go-dap` provides DAP types + framing (used by delve). Measured and sourced in the research notes.
- **The daemon's session model is the source of truth.** The CLI and VS Code (and any future MCP
  wrapper, §10) are views/controllers over it. No client talks to an adapter directly.
- **Two client-facing protocols:**
  1. *Native API* (JSON-RPC 2.0) — sessions, leases, budgeted views, event log. Used by the CLI, and
     by the extension for non-DAP concerns.
  2. *DAP facade* (phase 2) — per-session DAP endpoint so VS Code's built-in debug UI attaches natively. Requests from it go through the same lease/ownership checks as native calls.

## 3. Core concepts

| Concept | Definition |
|---|---|
| **Session** | One debuggee + one adapter connection. ID: short, human-typable (`s-7f3k`). May have child sessions (js-debug `startDebugging`). |
| **Client** | A connected controller: `{id, kind: agent\|human, name}`. CLI calls identify via `--as` / `EYEDBG_CLIENT` (default `agent:<ppid-derived>`); persistent across calls, not per connection. |
| **Control lease** | Exactly one client holds *execution control* (continue/step/pause/restart/terminate, setVariable). Others can read, set their own breakpoints, and request the lease. Policies: `free` (anyone takes it, MVP default), `handoff` (holder must release/grant), `human-priority` (human request preempts agent). Lease changes are events. |
| **Breakpoint ownership** | Every breakpoint records `owner` and `createdAt`. The daemon merges all owners' breakpoints per file into the single `setBreakpoints` DAP call and maps adapter ids back. Clients can filter by owner; removing only touches your own unless `--force`. |
| **Event log** | Per-session append-only log with monotonic `seq`: DAP events (stopped, output, breakpoint, thread…) plus daemon events (lease, client joined, bp added by X). `eyedbg events --since N` / `--wait` makes stateless CLIs and late joiners consistent. Also written to disk as the session recording (§11). |
| **Stop snapshot** | On every `stopped`, the daemon eagerly fetches threads, top K frames of the stopped thread, and scopes/vars of frame 0 to depth D. Cached until the next resume. Most agent reads hit this cache — one call, no round-trips. Invalidated on resume; variable references are never exposed across resumes. |

### Concurrency rules
- The daemon serializes execution-changing requests per session; reads may run concurrently while stopped.
- DAP `seq` is owned by the daemon toward the adapter; clients' request ids are mapped per client.
- Reverse requests (`runInTerminal`, `startDebugging`) are handled by the daemon (MVP: spawn directly / create child session); phase 2 may route `runInTerminal` to the human's VS Code if the lease holder is human.

## 4. CLI surface (MVP)

Global flags: `--session/-s <id>` (or `EYEDBG_SESSION`), `--json`, `--as <client>`, `--budget <tokens>`, `--timeout <dur>`.

```
eyedbg start  <lang> [--program P | --project P.csproj] [--args ...] [--cwd] [--env K=V] [--stop-on-entry] [--no-build]
eyedbg attach <lang> --pid N
eyedbg test   <lang> <filter>            # run tests under debugger (dotnet test + VSTEST_HOST_DEBUG attach)
eyedbg sessions | eyedbg status            # status = state + stop snapshot summary
eyedbg stop | eyedbg detach

eyedbg bp add <file:line | func:Name | anchor>  [--if EXPR] [--hit N] [--log "msg {expr}"]
eyedbg bp ls [--mine] | eyedbg bp rm <id|all>
eyedbg bp exceptions [all|uncaught|none]

eyedbg run-until <location|--if EXPR> [--dump locals,stack,args] [--timeout 30s]
eyedbg continue | next | step-in | step-out | pause     # all accept --dump
eyedbg wait [--until stopped|exited|output:/regex/] [--timeout 30s]

eyedbg stack [--thread T] [--frames N]
eyedbg threads
eyedbg vars [--frame F] [--depth D] [--expand path.to.field] [--changed]
eyedbg eval <expr> [--frame F] [--allow-side-effects]
eyedbg set <var> <value>
eyedbg source [--frame F] [--context 5]
eyedbg output [--since N] [--tail 50]
eyedbg events [--since N] [--wait]

eyedbg lease [take|release|grant <client>]          # prepared for P2, works in MVP
eyedbg daemon [status|stop|logs]
eyedbg adapters [ls|install <name>|doctor]
```

Design rules:
- **Every execution command returns the resulting state** (stop reason, location, source excerpt, top frames, locals diff) so the agent never needs a follow-up call just to see where it is.
- **Blocking is bounded.** Execution commands wait for `stopped|exited|timeout` (default 30 s) and report which happened; the program keeps running after a timeout and `wait` resumes waiting.
- **`--changed`** shows locals that differ from the previous stop snapshot (highest-signal view for step loops).
- **Anchors:** `bp add 'Foo.cs@"var total = items.Sum"'` resolves by content, re-resolved when the file changes; the reply always reports the *resolved* line and whether the adapter verified or moved it.
- **Help is the interface.** Every command and subcommand — including the root command of each
  binary — has a `Short`, a `Long` (what it does, when to use it, whether it blocks and for how
  long, its side effects on the debuggee, its output shape, and its exit codes) and at least one
  `Example` with real invocations; flags document their unit and default. This is how an agent is
  expected to learn the CLI: `eyedbg <command> --help` or `eyedbg help <command>`, reachable at
  every level of the tree, is authoritative and kept in sync by a unit test that walks the whole
  command tree (`internal/cli`). `eyedbg help --all`, printing the full tree's help in one read, and
  a `--json` help variant, are planned (§13). Help text is a tested, reviewed artifact — treat a
  change to it like a change to the output contract (§5).

## 5. Output contract

- Default: compact text (tuned for LLM reading). `--json`: stable schema, versioned (`"schema": 1`).
- Budgeting: `--budget` (default ~2k tokens for state dumps) enforced by the daemon via depth, max children per node (default 20), string truncation (default 200 chars), collection summaries (`List<Order> Count=1532 [0..19 shown]`). Truncation is always explicit (`…+1512 more, expand: eyedbg vars --expand orders`).
- Errors: `{code, message, hint}`; codes are stable (`NO_SESSION`, `NOT_STOPPED`, `LEASE_HELD`, `UNSUPPORTED_BY_ADAPTER`, `TIMEOUT`, …). Exit codes map to classes.
- Redaction: values of names matching configurable patterns (`password|secret|token|connectionstring`) masked by default.

## 6. Daemon

- **Discovery/IPC:** per-user dir (`$XDG_RUNTIME_DIR/eyedbg`, `~/Library/Caches/eyedbg` — TBD, `%LOCALAPPDATA%\eyedbg`), mode 0700. Unix domain socket everywhere (AF_UNIX works on Windows 10 1803+ and Go supports it); fallback named pipe via `go-winio` with owner-only DACL. Plus a 0600 random token file checked on connect.
- **Lifecycle:** CLI connects → on failure takes a lock file, spawns detached `eyedbgd`, polls ready. Handshake includes protocol version; mismatch → CLI asks the old daemon to drain & exit if it has no sessions, else errors with a hint. Idle exit after N minutes with zero sessions (default 30).
- **Crash resilience:** debuggee processes are children of adapters, adapters children of the daemon; if the daemon dies, sessions die (MVP). Session metadata persisted so `eyedbg sessions` can report "lost" instead of silently vanishing.
- **Logs:** `eyedbg daemon logs`; optional raw DAP trace per session.

## 7. Plugin model (languages)

Two layers, so simple languages need no Go code:

1. **Adapter manifest** (declarative, TOML/JSON, bundled or in `~/.config/eyedbg/adapters/`): how to obtain the adapter (download URL per os/arch, checksum, or "on PATH"), how to spawn it (stdio vs TCP), launch/attach config templates, file extensions, and quirks (e.g. `readMemory: false`).
2. **Driver** (Go interface, compiled in) for languages that need logic:

```go
type Driver interface {
    Name() string                                     // "dotnet"
    Detect(dir string) (Confidence, error)            // *.csproj / *.sln present
    Prepare(ctx, LaunchSpec) (Prepared, error)        // e.g. dotnet build, resolve dll, test host
    AdapterFor(Prepared) (AdapterSpec, error)         // netcoredbg vs sharpdbg
    LaunchArgs(Prepared) (map[string]any, error)      // DAP launch/attach body
    Presenter() Presenter                             // language-aware value formatting / collection summaries
    Extensions() []Extension                          // extra verbs (e.g. dotnet heap), backed by side helpers
}
```

- **Capability degradation:** the daemon caches the adapter's `initialize` capabilities; commands needing unsupported features fail with `UNSUPPORTED_BY_ADAPTER` + a hint (e.g. `memory read` on netcoredbg → "use `eyedbg dotnet heap`").
- **Side helpers:** out-of-process, JSON-RPC over stdio, any language. Lets .NET-specific inspection live in C# while the core stays Go.

## 8. .NET specifics

| Piece | Choice | Notes |
|---|---|---|
| Default adapter | **netcoredbg** (Samsung, MIT) 3.2.0 | Binaries: linux-x64/arm64, osx-arm64 ("community supported"), win-x64. We build win-arm64 / osx-x64 in our CI. |
| Alt adapter | **SharpDbg** (MIT, C#, `dotnet tool`) | Better eval & `DebuggerDisplay`/`DebuggerTypeProxy`. Young, single maintainer. Selectable: `--adapter sharpdbg`. |
| Forbidden | **vsdbg** | License restricts it to Microsoft IDEs. Never download, detect, or drive it. |
| Non-pausing inspection | `eyedbg-dotnet-helper` (C#): ClrMD, DiagnosticsClient/EventPipe | `eyedbg dotnet counters`, `trace`, `dump`, `heap` (stats, top types, gcroot on a dump), `threads` for a hung process. Live-heap reads without suspension are inconsistent → default to dump-then-analyze. |

Known netcoredbg gaps to surface honestly: no lambda/LINQ-lambda evaluation, no `readMemory`/`disassemble`, no `DebuggerDisplay`. Driver `Prepare` builds with `dotnet build` unless `--no-build`; `test` uses `VSTEST_HOST_DEBUG=1` and attaches to the reported testhost PID (to be validated).

## 9. Phase 2: VS Code co-debugging (design now, build later)

- Extension registers a debug type `eyedbg` whose `DebugAdapterDescriptorFactory` returns a connection to the daemon's **per-session DAP facade** (socket/pipe). VS Code's native UI (breakpoints gutter, call stack, variables, debug console) then just works.
- Facade translates: VS Code `setBreakpoints(file)` → replace *the human client's* breakpoints for that file only; other owners' breakpoints appear to VS Code as adapter-originated `breakpoint` events (so they show in the gutter).
- Execution requests from VS Code go through the lease. If denied, the facade returns an error and the extension shows "Agent has control — Request / Take over".
- Extension also uses the native API for: session picker (`eyedbg sessions`), presence (who's connected), lease UI, agent activity feed (from event log), "start debugging the agent's session" command.
- Late join = initialize from cached capabilities + replay current stop snapshot + breakpoint set.
- **Implication for MVP:** client identity, breakpoint ownership, leases, event log, and cached capabilities must exist in MVP even though only the CLI uses them.

## 10. Agent integration

- `SKILL.md` shipped with the binary (`eyedbg skill print`/`install`): when to reach for the debugger (after a failed hypothesis or two, per debug-gym), the standard loop (`start → bp add → run-until --dump → vars --changed → eval`), budgets, and "always `eyedbg stop` when done".
- No MCP server by default — agents use the CLI directly; its help is the documentation. A thin MCP wrapper may be added later only if a concrete agent needs it (non-goal for MVP).

## 11. Safety

- `eval` may run code (method calls, property getters). Default: read-only intent (DAP `context: "watch"`); `--allow-side-effects` for calls. How strictly each adapter enforces this is unverified → document per adapter.
- Socket access restricted to the user; token file; no TCP listener by default.
- Redaction (§5). Session recordings (DAP transcript + daemon events, JSONL) stored locally, opt-out, with redaction applied.
- `attach` only to processes owned by the same user; explain macOS `task_for_pid`/Linux ptrace-scope errors with actionable hints.

## 12. Repo layout (Go)

```
cmd/eyedbg/            CLI
cmd/eyedbgd/           daemon (also `eyedbg daemon run`)
internal/api/        native JSON-RPC schema (shared by the CLI, the extension, and any future MCP wrapper)
internal/daemon/     lifecycle, IPC, auth
internal/session/    session, lease, breakpoint store, event log, stop snapshot
internal/dap/        DAP client over go-dap: framing, seq mapping, reverse requests
internal/facade/     DAP facade (P2)
internal/present/    budgeting, truncation, text/JSON renderers
internal/adapters/   manifest loader, installer (download + checksum)
internal/cli/        command trees for all binaries (only package importing the CLI framework)
internal/version/    build metadata (ldflags / debug.ReadBuildInfo)
drivers/dotnet/      Driver impl
drivers/generic/     manifest-only driver
helpers/dotnet/      C# side helper (ClrMD, EventPipe)
skill/SKILL.md
testdata/apps/       sample debuggees per language
```

## 13. MVP milestones

1. **Skeleton:** daemon auto-start, IPC + token, version handshake, `daemon status/stop`.
2. **DAP core:** go-dap client, netcoredbg install/doctor, `start` a console app, `bp add` (line), `continue`, `status`, `stack`, `vars`, `stop`.
3. **Agent ergonomics:** stop snapshot, `--dump`, `run-until`, `wait`, `--changed`, budgets, JSON
   schema, errors, `eyedbg help --all` (the full command tree's help in one read) and a `--json`
   help variant.
4. **Model for P2:** client identity, bp ownership merge, lease, event log + `events --since`.
5. **Breadth:** conditional/log/function/exception bps, eval, set, attach, `test`, anchors.
6. **Ship:** SKILL.md, CI matrix (6 os/arch), e2e tests driving sample apps.
7. **Second language** via manifest only (debugpy) to prove the plugin boundary.

Phase 2: DAP facade + VS Code extension; .NET side helper; SharpDbg adapter; more languages.

## 14. Risks & open questions

- netcoredbg eval limits and macOS arm64 stability → SharpDbg as fallback; e2e tests on every platform in CI.
- netcoredbg release cadence (~2/yr, single corporate maintainer) → pin versions, keep our own builds.
- Test debugging flow (`VSTEST_HOST_DEBUG`) must be validated per platform.
- Lease policy default for P2 (`handoff` vs `human-priority`) — decide with real usage.
- Anchor re-resolution after large edits: fuzzy match strategy TBD.
- Windows: AF_UNIX vs named pipe as primary — prototype both in milestone 1.

## 15. References

- netcoredbg: https://github.com/Samsung/netcoredbg (releases; `src/protocols/vscodeprotocol.cpp`)
- vsdbg licensing: https://github.com/dotnet/vscode-csharp/blob/main/docs/debugger/Microsoft-.NET-Core-Debugger-licensing-and-Microsoft-Visual-Studio-Code.md
- SharpDbg: https://github.com/MattParkerDev/sharpdbg · ClrDebug: https://github.com/lordmilko/ClrDebug
- ClrMD / diagnostics: https://github.com/microsoft/clrmd · https://learn.microsoft.com/en-us/dotnet/core/diagnostics/
- go-dap: https://github.com/google/go-dap · DAP spec: https://microsoft.github.io/debug-adapter-protocol/
- NativeAOT cross-compile limits: https://learn.microsoft.com/en-us/dotnet/core/deploying/native-aot/cross-compile
- Delve multiclient: https://github.com/go-delve/delve/tree/master/Documentation/api/dap · issue #2322
- js-debug child sessions: https://github.com/microsoft/vscode-js-debug/blob/main/src/dapDebugServer.ts
- Prior art: https://github.com/debugmcp/mcp-debugger · https://github.com/microsoft/DebugMCP · https://www.jetbrains.com/help/idea/mcp-server.html
- Evidence: debug-gym https://arxiv.org/abs/2503.21557 · ChatDBG https://arxiv.org/abs/2403.16354 · CLI vs MCP https://mariozechner.at/posts/2025-08-15-mcp-vs-cli/
