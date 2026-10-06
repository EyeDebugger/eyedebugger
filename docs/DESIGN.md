# EyeDebugger (`eyedbg`) — AI-native debugger (design)

Status: v0.3 · 2026-10-03 · phase 1 (MVP) complete; phase 2: DAP facade (P2-M1), collaboration (P2-M2), VS Code extension (P2-M3, P2-M5), .NET side helper (P2-M6), .NET dumps, heap and threads (P2-M7), .NET traces (P2-M8), the extension's .NET views (P2-M9), per-owner conditions at shared lines (P2-S1), launching through the DAP facade (P2-S3a), terminals for launched programs (P2-S3b), services in docker containers and compose stacks as groups (ADR 0020), fast mode (ADR 0021)

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
- Remote/cross-machine sessions. (A .NET service in a container on a local docker engine is debugged, §8; a remote engine is unverified.)
- Time-travel debugging.
- MCP wrapper (§10): the CLI is the agent interface; a thin MCP wrapper is optional future work.

## 2. Architecture

```
eyedbg CLI (stateless) ──┐
                         ├── local IPC (JSON-RPC 2.0) ──────────────► eyedbgd (daemon, per user)
VS Code extension (P2) ──┘                                                 │
DAP client (VS Code, nvim-dap) ─stdio─► eyedbg dap ─ same socket: hello,   ├── DAP facade (per connection)
                                          facade.open, then DAP ──────────►│     └─► session calls
                                                                           └── Session ── DAP ──► adapter process
                                                                               (netcoredbg | sharpdbg | debugpy | dlv | lldb-dap | js-debug)
eyedbg dotnet … ─stdio, one process per command (JSON-RPC 2.0)─► side helper eyedbg-dotnet-helper (C#)
                                                                   └─ diagnostics IPC / EventPipe ─► the target .NET process
```

- **Language: Go.** Cross-compiles every OS/arch from one host, ~5 ms process start (matters: agents invoke the CLI many times), `google/go-dap` provides DAP types + framing (used by delve). Measured and sourced in the research notes.
- **The daemon's session model is the source of truth.** The CLI and VS Code (and any future MCP
  wrapper, §10) are views/controllers over it. No client talks to an adapter directly.
- **Two client-facing protocols:**
  1. *Native API* (JSON-RPC 2.0) — sessions, leases, budgeted views, event log. Used by the CLI, and
     by the extension for non-DAP concerns.
  2. *DAP facade* — an editor's DAP connection to one session, so VS Code's built-in debug UI (or
     nvim-dap, or any DAP client) attaches natively, or launches the session with a DAP `launch`
     (`eyedbg dap --launch`, ADR 0019). It is reached through `eyedbg dap`, which
     switches an authenticated connection on the same socket to DAP (§6, ADR 0012); requests from
     it go through the same lease, state and ownership checks as native calls, because the facade
     calls the same session methods (a launch, `Manager.Launch`, those of `eyedbg start`).
- **A container is a target, not a second session model** (ADR 0020, 0021): for a .NET process in a docker container, `eyedbgd` runs `docker exec -i <id> /.eyedbg-netcoredbg-<v>/netcoredbg --interpreter=vscode` as the session's adapter (stdio, so the transport is unchanged), and a path map at the DAP client's single choke point translates every source path (§7). A compose project is a *group* of such sessions (§3). eyedbg only runs the docker CLI as an argv (§6); it never talks to the engine's API itself.
- **Side helpers are run by the CLI, not the daemon** (ADR 0016): `eyedbg dotnet …` starts the
  helper for one call and closes it; no session state, lease or event is involved. A session only
  names the target (its program's pid).

## 3. Core concepts

| Concept | Definition |
|---|---|
| **Session** | One debuggee + one adapter connection. ID: short, human-typable (`s-7f3k`). May have child sessions (js-debug `startDebugging`). |
| **Group** | A label on ordinary sessions naming a compose project (`StartParams.Group`, `SessionInfo.group`), not an object: no parent session, no facade multiplexer, nothing changes for a session without one (ADR 0020 D1). The daemon methods that take one are `group.wait` (wait-any: the member stopped longest answers, the others stopped are listed; `--new` ignores members already stopped) and `group.events` (the members' event logs merged by time; the cursor holds every member's last seen `seq`, a member's own order never reordered, at most 64 members); `bp` and `stop` fan out CLI-side, each member under its own lease rules. |
| **Container session** | A session whose debuggee is in a docker container: `SessionInfo.container {id, name, service, project, pid, platform, map, unhealthyAfter, launched}` and `stoppedAt` (set while stopped). `pid` is omitted (the container pid is `container.pid`), so every consumer of a host pid (`eyedbg dotnet …`, the detach text) skips it; `eyedbg dotnet` on one is refused. Attached (ADR 0020) it ends with `detach`, the container's program keeps running; launched (`container.launched`, ADR 0021: the app eyedbg started in an idle fast-mode container) it is an ordinary launched session — `stop` terminates the app, `detach` is refused. The manager keeps one **claim** per container (key: full id): a launch conflicts with any live claim on the container, an attach with a launch claim or with the same pid's; an early check by the reference runs before the driver, the authoritative claim by full id after the driver and before the adapter starts (ADR 0021 D10). |
| **Client** | A controller: `{id, kind: agent\|human, name}`, id `KIND[:NAME]` (`agent`, `human:ijat`). Every request names it: CLI calls via `--as` / `EYEDBG_CLIENT`, default `agent` (never a human; not derived from the parent pid, since agent harnesses run each command in a fresh shell). Two agents sharing a session set distinct names. Persistent across calls, not per connection; the session lists each client with when it was first and last seen (ADR 0009), and how many editor (`eyedbg dap`) connections it has open — its presence (ADR 0014; CLI clients are never connected). |
| **Control lease** | Exactly one client holds *execution control* (continue/step/pause/run-until, terminate or detach while the program is live, `set`, and `eval --allow-side-effects`). Others can read, set their own breakpoints, and take or be granted the lease. The starter holds it first. Policies (set at start or by the holder): `free` (anyone takes it, executing takes it automatically; the default), `handoff` (only the holder releases or grants it), `human-priority` (handoff, except that a human may take it from an agent; agents never take it from one another). `--force` overrides the policy; a refusal is `LEASE_HELD`. Lease changes are events. `lease request` asks the holder for it without moving it: a pending request (one per client, with a message) shows in every stop until the lease changes hands. A client's lease is released when its last editor connection closes (a Restart waits 10 s; ADR 0014). |
| **Breakpoint ownership** | Every breakpoint records `owner` and `createdAt`. The daemon merges all owners' breakpoints per file into the single `setBreakpoints` DAP call — one source breakpoint per line (adapters keep one per line): equal conditions are kept; otherwise (different conditions, or a conditional beside an unconditional one) the line is unconditional at the adapter and the stop filter (§4) checks each owner's own condition, so the program stops there iff some owner's breakpoint wants it and the stop names whose (ADR 0018) — and maps the adapter's answer back to every breakpoint of the line. Function breakpoints merge per name the same way, except that different conditions make the function stop unconditionally, with a note. Clients can filter by owner (`bp ls --mine`); removing only touches your own unless `--force` (`NOT_OWNER` otherwise). Breakpoints set through an editor are *editor* breakpoints: an editor's list replaces only those (never its client's CLI ones), and they're removed when the client's last editor connection closes (ADR 0014). |
| **Exception stops** | Per client, like breakpoints: each client picks `none`, `uncaught` or `all` (`bp exceptions`), the adapter gets the union of their filters, and no lease is needed; `--force` sets one mode for every client. Changes are `exceptions` events (ADR 0010). |
| **Event log** | Per-session in-memory log with a monotonic `seq` from 1, bounded (10000 events, 8 MiB; the oldest are dropped and readers are told): DAP events (stopped, continued, output, breakpoint, thread, exited) plus daemon events (started, client joined, editor connected/disconnected, lease — including requests —, exec by X — including `set` and side-effecting `eval` —, bp added/removed by X, exceptions set by X, ended). `output` and a snapshot's output are views of it (their `seq` is the event's). `eyedbg events --since N` / `--wait` (long-poll) makes stateless CLIs and late joiners consistent. Its control events are also written to disk as the session recording (§11). |
| **Stop snapshot** | Built on demand, not eagerly: a read after a stop fetches the stopped thread's top frames from the adapter, and frame 0's locals once per stop, cached until the next resume (a `set` or side-effecting `eval` marks them stale; `--changed` diffs against the previous stop's). Every execution command returns one, so the agent needs no follow-up call. Variable references are never exposed across resumes by the CLI; an editor gets the adapter's, and drops them on `continued`. |

### Concurrency rules
- The daemon serializes execution-changing requests per session; reads may run concurrently while stopped.
- DAP `seq` is owned by the daemon toward the adapter; an editor's request ids are mapped per
  connection (the facade answers each with its own `seq`, and never relays the editor's bytes: it
  re-encodes the request the policy decided on).
- Reverse requests from adapters (`runInTerminal`, `startDebugging`) are refused ("not supported"), with one exception (ADR 0019 D8): a start made by an editor's DAP `launch` with `console: integratedTerminal` takes the adapter's first `runInTerminal` while that start runs, and hands it to the launching connection alone (§9). The lease is not consulted — the command comes from that editor's own launch and goes back to it — and only such a start tells the adapter `supportsRunInTerminalRequest`. The DAP read goroutine only hands the request over; it is answered from another goroutine, once.

## 4. CLI surface (MVP)

Global flags: `--session/-s <id>` (or `EYEDBG_SESSION`), `--json`, `--as <client>`, `--budget <tokens>`, `--timeout <dur>`.

```
eyedbg start  <lang> [--program P | --project P.csproj] [--args ...] [--cwd] [--env K=V] [--stop-on-entry] [--no-build]
              [--opt NAME=VALUE]... [--bp LOC]... [--exceptions all|uncaught|none]
              [--lease-policy free|handoff|human-priority] [--no-record]
                                          # --opt: the language's options from its manifest (python: module, python, justMyCode)
eyedbg attach <lang> --pid N [--bp LOC]... [--exceptions MODE] [--lease-policy P] [--no-record]
eyedbg attach dotnet --container REF [--pid N] [--map REMOTE=LOCAL]... [--group NAME]   # a .NET process in a docker container (ADR 0020)
eyedbg test   <lang> [FILTER] [--project P] [--framework TFM] [--no-build] [--env K=V] [--bp LOC]... [--exceptions MODE]
                                          # dotnet test (VSTest) + VSTEST_HOST_DEBUG, attached to the test host
eyedbg sessions | eyedbg status            # status = state + stop snapshot summary (+ the exception at an exception stop)
eyedbg stop | eyedbg detach                # stop detaches from an attached program; detach only for attached sessions

eyedbg bp add <file:line | file@"text" | func:Name>  [--if EXPR] [--hit N|>=N|%N] [--log "msg {expr}"]
eyedbg bp ls [--mine] | eyedbg bp rm <id|all> [--force]
eyedbg bp exceptions [all|uncaught|none] [--force]

eyedbg run-until <location|--if EXPR> [--dump locals,stack,args] [--timeout 30s]
eyedbg continue | next | step-in | step-out | pause     # all accept --dump
eyedbg wait [--until stopped|exited|output:/regex/] [--timeout 30s]

eyedbg stack [--thread T] [--frames N]
eyedbg threads
eyedbg vars [--frame F] [--depth D] [--expand path.to.field] [--changed]
eyedbg eval <expr> [--frame F] [--depth D] [--allow-side-effects]
eyedbg set <var> <value> [--frame F]
eyedbg source [--frame F] [--context 5]
eyedbg output [--since N] [--tail 50]
eyedbg events [--since N] [--limit N] [--kind k,...] [--wait] [--timeout 30s]

eyedbg lease [status|take [--force]|release|grant <client> [--force]|policy <p> [--force]|request [--message TEXT]]
eyedbg dap [-s ID] [--as CLIENT]           # DAP on stdio for an editor, joined to a running session (ADR 0012)
eyedbg dap --launch [--as CLIENT]          # DAP on stdio; the client's DAP launch starts the session (ADR 0019)
eyedbg daemon [status|stop|logs]
eyedbg adapters ls | install <adapter|language> [--platform OS/ARCH] | doctor [adapter|language...]
eyedbg compose attach [SERVICE...] [--map R=L]... [--bp LOC]... [-f FILE]... [-p NAME] [--project-directory DIR]   # every .NET service of a stack, one session each, group = project
eyedbg compose launch [SERVICE...] [--bp LOC]... [--dotnet-project SERVICE=PATH]... [--env-file F]... [--no-build] [--stop-on-entry] ...   # fast mode (ADR 0021): host Debug build, app launched in an idle container
eyedbg compose restore [SERVICE...]        # fast-mode services back as built
eyedbg compose wait [-g G] [--new] | events [--since CURSOR] [--wait] [--kind k,...] | bp add|ls|rm | stop   # the group's sessions
eyedbg skill [print|install [--dir ROOT] [--force]]
```

Design rules:
- **Every execution command returns the resulting state** (stop reason, location, source excerpt, top frames, locals diff) so the agent never needs a follow-up call just to see where it is.
- **Blocking is bounded.** Execution commands wait for `stopped|exited|timeout` (default 30 s) and report which happened; the program keeps running after a timeout and `wait` resumes waiting.
- **`--changed`** shows locals that differ from the previous stop snapshot (highest-signal view for step loops).
- **Anchors:** `bp add 'Foo.cs@"var total = items.Sum"'` resolves by content — the one line holding the text, whitespace runs matching one space, case-sensitive; several lines are `ANCHOR_AMBIGUOUS`, none `ANCHOR_NOT_FOUND` with the closest lines — once, when added; the reply reports the *resolved* line and whether the adapter verified or moved it. Anchors are not moved when the file changes mid-session (the program still runs the code it was built from, and the adapter maps lines through that build's symbols); breakpoints in a changed file are noted instead, and adding one again re-resolves it (ADR 0010).
- **Hit counts and logpoints** (`--hit`, `--log`) are emulated by the daemon for every adapter: the adapter gets a plain breakpoint, and a stop there is counted, logged (output category `logpoint`) and continued unless it should stop. Each hit costs a pause and a few round trips; a step that reaches such a line ends there.
- **Shared lines** (ADR 0018): the same stop filter runs at a line whose clients' breakpoints have different conditions (the adapter's breakpoint there is unconditional). It evaluates each distinct condition once per pass and stops iff some breakpoint wants the stop — its condition held and it is neither a logpoint nor short of its hit count — so each owner's `--if`, `--hit` and `--log` apply on their own, and one's logpoint never suppresses another's stop. Where eyedbg evaluates a condition (here — whatever `--hit`/`--log` those breakpoints have), a value counts as false only when it reads as false (`false`, `0`, `None`, `null`, `nil`, an empty string or collection) and an expression that fails to evaluate stops; elsewhere — including the `--if` of a lone breakpoint (even one with `--hit`/`--log`) or of an equal-condition group — the adapter's rule applies (debugpy ignores a failing one). A stop the filter decided names the breakpoints it is for and their owners (`stopped for breakpoint 6 of human:ijat`; `stop.breakpoints` in `--json`; the events line); none named at a line breakpoint means every breakpoint there held, eyedbg couldn't read where the program stopped and kept it, or a step or pause ended there with no breakpoint wanting the stop (its description says so; the reason is still `breakpoint`). `run-until` at a shared line is not `reached` when the stop was only for another client's breakpoint.
- **Groups** (ADR 0020, 0021): the group for `compose wait|events|bp|stop` is `-g/--group`, else `$EYEDBG_GROUP`, else the only group among the live sessions. `compose attach` makes one session per running service of the stack whose pid 1 is `dotnet` (named services are always attempted; the others are listed as skipped), with `--bp` applied after attaching so a line outside one service's map is skipped for it only. `compose stop` applies `stop`'s rule to each member (attached: detached, the container keeps running; launched: the app is terminated, the container idles). Stops of container sessions are noted: a stopped service stops answering, so snapshots say how long it has been stopped and, once `stoppedAt` passes the container's `unhealthyAfter` (`retries × interval + timeout`, docker's 3 / 30 s / 30 s when unset, none without a healthcheck) or 4 minutes (a Kafka consumer's default `max.poll.interval.ms` is 5), say what that does. The CLI renders these from a clock it is given; the daemon has no timers for them.
- **Help is the interface.** Every command and subcommand — including the root command of each
  binary — has a `Short`, a `Long` (what it does, when to use it, whether it blocks and for how
  long, its side effects on the debuggee, its output shape, and its exit codes) and at least one
  `Example` with real invocations; flags document their unit and default. This is how an agent is
  expected to learn the CLI: `eyedbg <command> --help` or `eyedbg help <command>`, reachable at
  every level of the tree, is authoritative and kept in sync by a unit test that walks the whole
  command tree (`internal/cli`). `eyedbg help --all` prints the full tree's help in one read, and
  `--json` gives any help as structured data. Help text is a tested, reviewed artifact — treat a
  change to it like a change to the output contract (§5).

## 5. Output contract

- Default: compact text (tuned for LLM reading). `--json`: stable schema, versioned (`"schema": 1`).
- Budgeting: `--budget` (default ~2k tokens for state dumps) enforced by the daemon via depth, max children per node (default 20), string truncation (default 200 chars), collection summaries (`List<Order> Count=1532 [0..19 shown]`). Truncation is always explicit (`…+1512 more, expand: eyedbg vars --expand orders`).
- Errors: `{code, message, hint}`, plus `output` (an optional, additive field) when a session start
  failed after its debug adapter logged stderr/important output (e.g. a compiler's error), bounded
  to the last 20 lines / 4 KiB: the CLI prints it after the hint, and the DAP facade appends it to
  the error's `format`. It is never written to the daemon log (rule 9). Codes are stable (`NO_SESSION`, `NOT_STOPPED`, `LEASE_HELD`, `UNSUPPORTED_BY_ADAPTER`, `SIDE_EFFECTS`, `ATTACH_FAILED`, `NO_TEST_HOST`, `ANCHOR_NOT_FOUND`, `ANCHOR_AMBIGUOUS`, `NOT_DOTNET`, `DIAGNOSTICS_DISABLED`, `DIAGNOSTICS_TIMEOUT`, `HELPER_NOT_FOUND`, `HELPER_MISMATCH`, `HELPER_FAILED`, `DUMP_UNSUPPORTED`, `DUMP_RUNTIME_MISSING`, `DUMP_FAILED`, `TRACE_UNSUPPORTED`, …). Exit codes map to classes: 1 usage (incl. `SIDE_EFFECTS`, `ANCHOR_*`), 2 state (incl. a target, dump or trace that can't be inspected: `NOT_DOTNET`, `DIAGNOSTICS_*`, `DUMP_UNSUPPORTED`, `TRACE_UNSUPPORTED`), 3 setup (incl. `ATTACH_FAILED`, `NO_TEST_HOST`, `HELPER_NOT_FOUND`, `HELPER_MISMATCH`, `DUMP_RUNTIME_MISSING`), 4 adapter or side helper (incl. `UNSUPPORTED_BY_ADAPTER`, `HELPER_FAILED`, `DUMP_FAILED`).
- With no daemon running and autostart disabled (`EYEDBG_NO_AUTOSTART=1`): a command that needs an
  *existing* session (`status`, `bp`, `continue`, `stop`, …) reports `NO_SESSION` (exit 2) — there
  cannot be a session without a daemon, so `internal/cli/session.go`'s `call()` deliberately folds
  "no daemon" into "no session". A command that *creates* one (`start`, `attach`, `test`) instead
  reports `DAEMON_NOT_RUNNING` (exit 3), since there it is the daemon itself, not a session, that
  is missing. `eyedbg daemon status`/`stop` print "eyedbgd is not running" and exit 0 (querying
  daemon state never fails); `daemon logs` exits 1 (no log file to open).
- Redaction: values of names matching configurable patterns (`password|secret|token|connectionstring`) masked by default.

## 6. Daemon

- **Discovery/IPC:** per-user runtime dir: `$EYEDBG_RUNTIME_DIR` if set, else `$XDG_RUNTIME_DIR/eyedbg` on Unix when set, else the user cache dir (`~/Library/Caches/eyedbg`, `~/.cache/eyedbg`, `%LocalAppData%\eyedbg`). Mode 0700 and owned by the user (checked on Unix; on Windows the `%LocalAppData%` ACL applies). Unix domain socket everywhere (AF_UNIX works on Windows 10 1803+ and Go supports it); a named-pipe fallback is not needed so far. A fresh random token (0600) is written at each daemon start and checked (constant-time) on every connection's first request.
- **Lifecycle:** One daemon per user serves all sessions; each session's adapter is its own child process, in its own process group (Unix) or process tree (Windows), so an adapter crash only ends that session. When a launch start fails once its adapter runs, the daemon kills that group or tree before disconnecting — with it the program the adapter launched, which an adapter may leave running after a failed start (netcoredbg after a failed `configurationDone`); an attach start's target is never killed. Nobody starts or stops the daemon by hand, and it is never installed as a system service:
  - *Start:* CLI connects → on failure spawns detached `eyedbgd` (`setsid` on Unix, detached process on Windows; stdout/stderr appended to `eyedbgd.log`, rotated at 5 MiB), polls ready (5 s). The daemon holds an exclusive OS lock (`flock` / no-share open) on `eyedbgd.lock` for its whole life, so when concurrent agents spawn several, exactly one wins and the rest exit at once; the winner may delete any leftover socket or token, since they can only be stale. `EYEDBG_NO_AUTOSTART=1` makes the CLI fail instead of spawning (CI, or users who manage the daemon themselves).
  - *Handshake:* protocol version + token on every connection; version mismatch → CLI asks the old daemon to drain & exit if it has no sessions, else errors with a hint (never kills live sessions).
  - *Connection switch:* after `hello`, `facade.open {sessionId, client}` turns the connection into a DAP connection for that session and client, for good (ADR 0012): the result is its last JSON-RPC line, and whatever the client sent past the request is handed to the facade with it. `eyedbg dap` bridges it to stdio; an older daemon answers `UNKNOWN_METHOD`, reported as `VERSION_MISMATCH`. `facade.open {client, launch: {clientDir, virtualEnv?, noRecord?, dotnetAdapter?}}` (no session id; `facadeVersion` 3) opens a launch connection instead: no session yet, its DAP `launch` starts one (ADR 0019); `eyedbg dap --launch` sends it, starting the daemon if needed, and reports a daemon answering with a session id or an older facade version as `VERSION_MISMATCH`.
  - *Idle exit:* the idle timer starts when the last session ends (no immediate exit, so the next command starts fast); exit after N minutes with zero sessions (default 30, configurable).
  - *Manual:* `eyedbg daemon status` (pid, uptime, version, sessions); `eyedbg daemon stop` refuses while sessions exist unless `--force`, which ends them (killing their debuggees); `eyedbgd` run directly stays in the foreground with logs on stderr, for debugging the daemon itself.
- **Crash resilience:** debuggee processes are children of adapters, adapters children of the daemon; if the daemon dies, sessions die (MVP). Session metadata is persisted in the runtime dir as `sessions/<id>.json` (0600; written at start and when the session ends, removed when it is stopped or the daemon exits cleanly), so a daemon reads what is left at its start as the sessions an earlier one lost: `eyedbg sessions` lists them as `lost` (read from the files directly when no daemon runs) and `eyedbg stop -s ID` forgets one (ADR 0009).
- **Auto-start races:** a daemon started by a client (`EYEDBG_AUTOSTARTED=1`) that finds the lock taken exits 0 silently, so the shared log stays clean.
- **Docker** (ADR 0020 D6, D12; ADR 0021): eyedbg runs the docker CLI only as an argv, never through a shell: `$EYEDBG_DOCKER` (an absolute path) else `docker` from the `PATH` of the process that runs it, each value a single `--flag=value` token placed before the subcommand, and every container reference, context, host, group and service name checked against a grammar first (a leading `-`, whitespace, control characters or NUL are refused). The daemon runs it for sessions: `inspect`, `image inspect`, `cp -` and `exec` (netcoredbg's copy and probe, then the adapter itself) and `top` (the stray guard, §11). The CLI runs it for `compose attach`'s one `compose ps --format json` and, for `compose launch` and `restore`, for the discovery (`inspect`, `ps --all` with a label filter, one `inspect` of the stack's read-write bind mounts when a project is searched for, an `exec … tail --version` probe, the adapter's copy and probe into the as-built container before it is recreated), the host build (`dotnet publish`, §8) and `compose up` for the named services. The engine is the one the CLI's `DOCKER_HOST` (else `DOCKER_CONTEXT`) names: the CLI sends only those two with each request and passes them as flags; neither process hands its own copy to a docker child as environment. Docker's stderr is bounded (4 KiB) and stripped of control characters before it reaches an error. The new daemon methods are `container.attach`, `container.launch`, `group.wait` and `group.events`; the daemon decodes parameters with a plain `json.Unmarshal`, so an *older* daemon would drop a new `container` field of `session.start` and attach to host pid 1 — hence new methods, whose `UNKNOWN_METHOD` the CLI reports as `VERSION_MISMATCH` with the `eyedbg daemon stop` hint (as for `facade.open`). No `ProtocolVersion` or JSON `schema` bump (new fields are additive, new outputs are new shapes); `version --json` lists the features `container` and `compose-launch`.
- **Logs:** `eyedbg daemon logs`; optional raw DAP trace per session.

## 7. Plugin model (languages)

Two layers, so simple languages need no Go code (ADR 0011):

1. **Adapter manifest** (declarative JSON, schema 1; field reference in docs/adapter-manifests.md): bundled (`internal/adapters/manifests/`, embedded) or the user's own (`~/.eyedbg/adapters/`; `EYEDBG_CONFIG_DIR` overrides the `~/.eyedbg` part, `EYEDBG_HOME` overrides `~/.eyedbg` itself), one per adapter. It says how to obtain the adapter (pinned download per os/arch or `"*"`, SHA-256, size, archive layout), how to find and spawn it (a native executable found by env variable, installed copy or PATH, a `python` runtime: a package run on the user's interpreter, or a `dotnet` runtime: a framework-dependent `.dll` run on the user's `dotnet` host, with a minimum `Microsoft.NETCore.App` — ADR 0017), how to talk DAP to it once spawned (`transport`: `stdio`, the default, or `connect` — ADR 0013 — a Unix socket in a fresh private directory that the adapter dials in to, its path `${socket}` in `adapter.args`; for an adapter that can only listen, like Delve's `dlv dap`), and, for a language served by the generic driver, its `--opt` options, launch/attach templates (`${program}`, `${args}`, `${cwd}`, `${env}`, `${envList}`, `${stopOnEntry}`, `${runtime}`, `${pid}`, `${opt.NAME}`), exception-mode filter ids and eval-guard rules. A user manifest replaces the bundled one of the same name or language, whole; invalid or untrusted ones are left out and reported (`adapters ls`, `doctor`, the daemon log). `eyedbg adapters install NAME --platform OS/ARCH` installs another platform's download under `<data>/_platform/<os>-<arch>/<name>/<version>` (a leading `_` can't start a manifest name, so it can't collide); that copy is never used on this machine, only streamed into containers (§8).
2. **Driver** (Go interface, compiled in) for languages that need logic (dotnet: project detection and builds, VSTest test runs, the C# side-effect check); its manifest is `"builtin": true` and carries adapter metadata only. Every other language a manifest names is served by the generic driver (`drivers/generic`), registered by `generic.Drivers(registry)` in `internal/cli`: nothing in `internal/session` or `internal/daemon` knows a language.

The interface as designed:

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

As built (`internal/session/driver.go`): `Name()` and `Prepare(ctx, LaunchSpec) (Launch, error)`, which returns the adapter command and the DAP request body, plus per-driver behaviour the session applies: `Request` (`launch`/`attach`), `PID`, `ExceptionFilters` (mode → adapter filter ids), `SideEffects` (the eval check), `AttachHint`, `AdapterName` (shown as each session's adapter), `PauseUnsupported` (refuse pause, with this hint) and `SetByEval` (`set` evaluates `VAR = VALUE` when the adapter has neither setExpression nor setVariable). `AdapterFor` is built as the optional `AdapterSelector` (`WithAdapter(name) (Driver, error)`): `start`/`attach`/`test --adapter NAME` (`StartParams.Adapter`) binds the driver before anything is built; a driver without it refuses `--adapter` (ADR 0017). `LaunchSpec` carries the language's `Options` (`--opt`) and the caller's working directory (`ClientDir`). Optional interfaces: `Attacher` (`PrepareAttach(ctx, AttachSpec)`) and `Tester` (`TestCommand(ctx, TestSpec)`: the command, how to find the test host's pid in its output, and how to explain an exit without one; a Tester is also an Attacher). The generic driver is always an Attacher: without an `attach` template it answers `UNSUPPORTED_BY_ADAPTER` with the manifest's hint. Optional `ContainerAttacher` (`PrepareContainerAttach(ctx, AttachSpec)`, ADR 0020) and `ContainerLauncher` (`PrepareContainerLaunch(ctx, ContainerLaunchSpec)`, ADR 0021): implemented by the .NET driver over the language-neutral `internal/container` (inspect, tar, `docker cp`, `docker exec`, `docker top`, the compose calls), and returning an ordinary `Launch` whose adapter is the `docker` binary (`exec -i <full id> /.eyedbg-netcoredbg-<v>/netcoredbg --interpreter=vscode`), so the session stays docker-agnostic. A `Launch` gains `PathMap` (host↔container source paths, §8; nil = a host session, byte-for-byte today's behaviour), `Container` (`api.ContainerInfo`: the session reports it, has no host pid and takes a claim, §3), `StartFailureHint` (called, bounded by 3 s, after a failed start: a launch in a container can't kill its process group, the adapter's kill reaching only the host docker CLI) and `Warnings` (logged, never fail the start); `ErrNotCandidate` in an error's chain marks a container an implicit pick should skip. The generic driver implements neither. Language detection from a manifest's `extensions` and `markers` is not wired yet (`start` names the language).

- **Trust:** a user manifest names commands eyedbg runs, so it is trusted like the user's shell configuration: never read from a project; on Unix its directory, the parent and the file (checked through the opened handle) must be the user's and writable by neither group nor others (OpenSSH's `st_mode & 022` rule); nothing runs at load; argv never goes through a shell; downloads are https, size-capped and SHA-256-checked before extraction (§11). A `connect`-transport adapter's socket lives in a directory only the same user can enter and is gone, with the listener, on every return path — no TCP fallback: a same-user guard on the adapter's own side can't be relied on cross-platform (ADR 0013).
- **Capability degradation:** the daemon caches the adapter's `initialize` capabilities; commands needing unsupported features fail with `UNSUPPORTED_BY_ADAPTER` + a hint (e.g. `memory read` on netcoredbg → "use `eyedbg dotnet heap`"). Checked today: `--if` (conditional breakpoints), `func:` (function breakpoints), exception filters (the error names the adapter's), `set` (setExpression, else setVariable, else evaluating the assignment when the driver allows it), pause (when the driver says the adapter can't: SharpDbg); exception details (exceptionInfo) are left out silently when missing. Manifests can't override capabilities yet (schema 1 has no field for it; it would need a session hook).
- **Stop captures** read the scopes the adapter marks `presentationHint: "locals"` when it marks any, else every cheap scope; `vars` shows every scope.
- **Side helpers:** out-of-process, JSON-RPC over stdio, any language. Lets .NET-specific inspection live in C# while the core stays Go. As built (ADR 0016): the CLI runs one per command — `internal/helper` spawns it in its own process group, says `hello` (protocol version), makes one call whose result may follow notifications, and closes stdin to cancel or end it (killed 2 s later if it hasn't exited); internal/api's line framing (≤ 1 MiB) both ways; its stderr (last 4 KiB, sanitized) is shown only when it fails without answering. The designed `Extensions()` is, for .NET, `drivers/dotnet/helper.go`: the helper's lookup (`$EYEDBG_DOTNET_HELPER`, else `helpers/dotnet/` next to `eyedbg` or next to a symlinked `eyedbg`'s target), protocol, methods and wire types; the verbs live in `internal/cli/dotnet.go`.

## 8. Language specifics

### .NET

| Piece | Choice | Notes |
|---|---|---|
| Default adapter | **netcoredbg** (Samsung, MIT) 3.2.0 | Binaries: linux-x64/arm64, osx-arm64 ("community supported"), win-x64. No win-arm64 / osx-x64 builds: on osx-x64 an installed SharpDbg is the default (below), on win-arm64 it is withheld (`--adapter sharpdbg` only); building netcoredbg ourselves is future work. Pinned in `internal/adapters/manifests/netcoredbg.json`. |
| Alt adapter | **SharpDbg** 0.1.17 (MattParkerDev, MIT, C#) — built (P2-M10, ADR 0017) | The `SharpDbg.Cli` nupkg from nuget.org, pinned (`manifests/sharpdbg.json`, runtime `dotnet`), run as `dotnet SharpDbg.Cli.dll` on the user's host; needs the .NET 10+ runtime. **Opt-in**: never shipped with eyedbg, fetched only by `eyedbg adapters install sharpdbg` (or `EYEDBG_SHARPDBG` names a `.dll`). It bundles `Microsoft.VisualStudio.Shared.VSCodeDebugProtocol` under the Microsoft Software License Terms (not OSS: use to develop and test your apps; duties on redistributors; no reverse engineering; a data-collection clause) — disclosed in the manifest, `adapters install` help and ADR 0017. Chosen with `--adapter sharpdbg` (or `EYEDBG_DOTNET_ADAPTER`); the default without it where netcoredbg has no build and isn't found (darwin/amd64; windows/arm64 is withheld) once installed, unless `SharpDbgWithheld` names the platform. Gains: lambdas/LINQ in eval, `[DebuggerDisplay]`/type proxies. Gaps: pause refused (it stops the program without a `stopped` event), `set` by evaluating the assignment (no setVariable/setExpression), native logpoints broken (emulated anyway), main thread "<No Name>", breakpoints bind by file name, no inner exceptions, stepIn enters `[DebuggerStepThrough]`, exit codes untrusted on every OS (0.1.17 sends `exitCode: 0` whenever it misses the real one — a race in its exit callback — and always on attach, so eyedbg reports none, as for netcoredbg on macOS: `ExitCodeUnknown`; re-check on a SharpDbg bump). Parity table: ADR 0017. |
| Forbidden | **vsdbg** | License restricts it to Microsoft IDEs. Never download, detect, or drive it. |
| Non-pausing inspection | `eyedbg-dotnet-helper` (C#, `net8.0`, rolled forward): DiagnosticsClient/EventPipe; ClrMD 4.1 for dumps | Built (P2-M6, ADR 0016): `eyedbg dotnet ps` (your processes with a diagnostics endpoint, and the session debugging each) and `eyedbg dotnet counters` (System.Runtime EventCounters: summary or `--watch`; `--pid N` or the session's running program — a program stopped at a breakpoint can't start a diagnostics session, so that is refused). Built (P2-M7, ADR 0016 addendum): `eyedbg dotnet dump` (a heap, mini, triage or full dump into a private directory, or `--out`), `eyedbg dotnet heap` (generations, top types, `--gcroot` root paths incl. static fields) and `eyedbg dotnet threads` (stacks with source lines, contended locks and owners), on a dump file or a process dumped first — stopped sessions allowed (the runtime writes dumps from a native thread). Live-heap reads without suspension are inconsistent → dump-then-analyze. Built (P2-M8, ADR 0016 addendum): `eyedbg dotnet trace` (a short EventPipe trace into a private directory, or `--out`: `--profile cpu` — hottest methods exclusive/inclusive and where the other threads wait, GC counts and pauses — or `--profile gc` — collections per generation, pauses, top allocating types; also any `.nettrace` FILE) — running programs only, like counters. |

Known netcoredbg gaps to surface honestly: no lambda/LINQ-lambda evaluation, no `readMemory`/`disassemble`, no `DebuggerDisplay`; no native hit counts or logpoints (emulated by the daemon, §4); `evaluate` ignores its context and always runs code, so eval can't be side-effect free — the driver refuses expressions that visibly change the program (a method call, `new`, assignment, `++`/`--`, interpolated strings) without `--allow-side-effects`, but getters, indexers and operators still run; an unhandled exception always stops the program, whatever the exception mode. On macOS (verified: 3.2.0-1092, osx-arm64), it reports every launched or attached program's exit code as 0 regardless of the real one, so eyedbg reports none there instead (`session.Launch.ExitCodeUnknown`); the end reason says why and, for a launched test run, to read the test output. A VSTest test run's exit code comes from `dotnet test`'s own OS-level exit, not from netcoredbg's `exited` event, so it keeps it regardless. A function breakpoint also stops with reason `breakpoint`, so the stop filter can't tell its stops from those of a line breakpoint on the function's first line: a `--hit`/`--log` breakpoint there, or clients' breakpoints there with different conditions (ADR 0018), decide the function breakpoint's stops too (documented in `bp add --help`). Driver `Prepare` builds with `dotnet build` unless `--no-build`; it takes no `--opt`. `attach` sends netcoredbg only the pid (Just My Code stays on); an attach failure surfaces at `configurationDone` and becomes `ATTACH_FAILED`. `test` runs `dotnet test -c Debug --tl:off` with `VSTEST_HOST_DEBUG=1 VSTEST_DEBUG_NOBP=1`, reads the `Process Id: N, Name: …` line vstest.console prints (the host runs under `dotnet`), attaches, and ends with `dotnet test`'s exit code — validated on Linux (.NET 10 SDK). A Microsoft.Testing.Platform project (a `global.json`/`DOTNET_TEST_RUNNER` selecting it, `EnableMSTestRunner`/`EnableNUnitRunner`/`TestingPlatformDotnetTestSupport`/`IsTestingPlatformApplication`/`UseMicrosoftTestingPlatformRunner` true, or `MSTest.Sdk` without `UseVSTest`), an xUnit v3 project (a `PackageReference` to `xunit.v3`, `xunit.v3.core` or their `mtp-vN` flavors, in the project or a `Directory.Build.props`/`.targets` above it) or a TUnit project (`TUnit`/`TUnit.Engine`) is detected before the build instead (ADR 0010's amendment): their own VSTest adapter, where they have one, runs the tests in a child of the test host the attached host never sees, so eyedbg builds the project and launches its self-hosting test app directly under the adapter instead, like `start` (no runner, no attach). FILTER becomes the app's own filter option (`--filter` for the Microsoft.Testing.Platform frameworks and MTP-mode xUnit v3, xUnit v3 native's `-filterVSTest` on 4.0+, TUnit's `--treenode-filter`) and everything after `--` reaches its argv; the session ends with the app's own exit code (0/2/8 for the Microsoft.Testing.Platform frameworks, 0/1 for xUnit v3 native), except where netcoredbg's exit codes aren't trusted (above), when it ends without one. SharpDbg (ADR 0017) takes the same launch, attach and
test flow and the same exception filter ids; its evaluate runs code too (it compiles expressions and
loads them into the debuggee), so the same side-effect check applies.

### .NET in docker containers and compose stacks

Built (ADR 0020, 0021): `eyedbg attach dotnet --container`, `eyedbg compose …`. Verified on macOS (Docker Desktop 24.0.7, compose 2.23.3, arm64) and Debian 13 (docker.io 26.1.5, compose 2.26.1, amd64), Linux containers only. **Unverified, so not claimed:** Windows hosts, podman, rootless docker, SELinux relabelling, remote engines (a `DOCKER_HOST` naming one), emulated architectures, Windows containers, compose profiles and scaled services, and no CI covers a Windows host (GitHub's Windows runners can't run Linux containers).

- **Attach** (`attach dotnet --container REF [--pid N]`, `compose attach`): the pinned netcoredbg for the container's platform (`adapters install netcoredbg --platform linux/ARCH`; ARCH is the *image's*, from `docker image inspect`; amd64 or arm64, glibc only: Alpine and other musl images, chiseled and distroless images can't run it) is streamed on **every** attach into the running container as a tar eyedbg builds (`docker cp - <id>:/`: `/.eyedbg-netcoredbg-<v>/`, uid/gid 0, directories and the executable 0755, the rest 0644, regular files only, ≤ 64 MiB), probed with `netcoredbg --version`, then run with `docker exec -i` as the container's own user. DAP `attach` needs no `SYS_PTRACE`. The target is pid 1, or `--pid N` as the container numbers it (`docker top` shows host pids). With `init: true` pid 1 is docker-init, and attaching to it returns success and does nothing, so eyedbg reads `.HostConfig.Init` and refuses without `--pid` (the .NET child's pid was accepted in a test but not driven to a breakpoint: unverified). A read-only root filesystem fails the copy; the copy disappears when the container is recreated and is the only thing an attach leaves. SharpDbg is refused. `compose attach` without names attaches only services whose pid 1 (`.Path`) is `dotnet`.
- **Path map** (`--map REMOTE=LOCAL`): netcoredbg has no source-map option, a breakpoint's absolute path must equal a PDB document path exactly, and it reports PDB paths back, so eyedbg maps. A `Translator` hook in `dap.Client` rewrites every `Source.path` of every request, response and event (host→container out, container→host in), a test enumerates go-dap's `Source`-bearing types so a library bump can't add an unmapped one, and the session reads source excerpts only from the mapped host roots (`EvalSymlinks` of the path must lie inside one; `..`, a symlink out of a root and an unmapped path are never read) and only files whose resolved name ends in a source extension (`.cs .csx .vb .fs .fsi .fsx .razor .cshtml .xaml`, any case; generated `X.g.cs` is `.cs`), so a hostile adapter naming `id_rsa` or `.env` under a mapped directory gets nothing. A map is 0–16 REMOTE (absolute POSIX) = LOCAL (absolute host directory) pairs, longest prefix on a segment boundary wins, duplicates are refused, never first-wins; a line breakpoint outside the map is refused (`INVALID_REQUEST`, the map in the hint), never sent. The default, when `--map` is absent, is `/src` = the compose `working_dir` label's directory (a Visual Studio-template image's PDBs name `/src/…`) **only when the labels corroborate it**: the directory exists here and holds a regular compose file (`.yml`/`.yaml`, symlinks resolved) that the container's `config_files` label lists, since an image's own `LABEL`s are copied onto containers made from it and a `docker run` container can carry any compose labels; uncorroborated, the map is empty (line breakpoints refused with the `--map` hint). An explicit `--map` is trusted as typed; a CI build with deterministic source paths (`/_/…`) leaves breakpoints pending: use `--map`.
- **Release images:** on attach to an image's own `-c Release` build, `pause` and stacks work but **line breakpoints never bind** (verified on both machines, `justMyCode` on and off; function breakpoints and exception stops on such an attach are unverified), because netcoredbg can only disable JIT optimisation at module load and an attach has missed it. A *launch* of the same Release build binds them. Rebuilding the image with `--build-arg BUILD_CONFIGURATION=Debug` (when the Dockerfile has the argument, as the current Visual Studio template does) or fast mode is the way.
- **Stops are visible from outside:** a stopped service doesn't answer (HTTP callers time out), its healthcheck turns `unhealthy` after about `retries × interval + timeout` (about 19 s for 5 s / 3 retries; docker doesn't restart it, and it is `healthy` within one interval after `continue`), a Kafka consumer paused past `max.poll.interval.ms` loses its partitions and rejoins after the resume. The CLI says so (§4).
- **A lost client leaves a stray:** if the host `docker exec` dies without a clean `disconnect` (a killed daemon, a crashed terminal) netcoredbg and the frozen program stay in the container (it never sees EOF), nothing reaps them from outside, and a re-attach returns success but never stops; `docker restart NAME` clears it. Stopping the container instead ends the exec (exit 137) with no DAP `terminated` event, which the session reads as "the debug adapter exited".
- **Fast mode** (`compose launch`, `compose restore`; ADR 0021): the Visual Studio-style Debug workflow without editing the Dockerfile or the compose files. Per selected service, from its *running* container only (never compose configuration): the entrypoint must be exactly exec-form `["dotnet", "X.dll"]`, there must be no `command:`/image CMD (presence is read, never the value), a usable working directory, `tail` in the image (chiseled and distroless images have none), one container, and netcoredbg installed for its platform. eyedbg then builds the service's project once with `dotnet publish -c Debug` **on the host** (`-p:PathMap=<compose dir>/=/src/`, `--artifacts-path` so the user's `bin/` and `obj/` are untouched, into a staging directory under `<EYEDBG_HOME>/compose/<project>/`; a compile error changes nothing), and enters fast mode by recreating the service once (`docker compose up -d --no-deps --force-recreate --no-build` for the named services only, from the compose files the container was created from plus an override eyedbg writes — never edits a compose file): the container idles (`entrypoint: ["tail","-f","/dev/null"]`, `command: []`, `init: true`) with the Debug build mounted **read-only** at its own working directory and labels `dev.izzat.eyedbg.fast.*`; the override carries nothing else (no `environment`, `env_file`, `secrets`, `healthcheck`, `ports`, `user`, `image`, `build`). The daemon's `container.launch` then runs netcoredbg through `docker exec -i` (no `-e`, `-u`, `-w`, `-t`) and DAP-`launch`es `dotnet <workdir>/<dll>` with **no `env`**: the app gets the container's own environment (what an exec carries: image `ENV`, compose `environment:` and `env_file:` values; measured name-for-name equal to pid 1's as built), user, working directory and ports, and breakpoints (a startup line included, `--bp`, `--stop-on-entry`) are in before it starts. Re-running `compose launch` for a service already in fast mode is the rebuild cycle **in the same container**: publish to a fresh staging directory while the old app runs → carry the live sessions' breakpoints, conditions, logpoints and exception mode → stop them (the app dies) → mirror the stage into the mounted directory → launch. `compose restore` recreates the services from the user's files alone and removes eyedbg's files for them.
  - **Consequences, stated in the help and ADR 0021:** entering and leaving fast mode **recreates** the container (writes outside volumes are lost; the new container gets this shell's compose environment and `--env-file` files; dependents are never touched); a fast-mode service **runs only while its eyedbg session runs its app** (`stop`, `compose stop` or the daemon ending kill the app with no graceful shutdown; the container idles and its healthcheck turns `unhealthy`; a crash isn't restarted); the app's output is in `eyedbg output -s ID` and `compose events --kind output`, not `docker compose logs`; the app directory is read-only; there is **no drift check** (docker's `config-hash` label can't be compared with `compose config --hash`: it differs for every `env_file` service and for `--no-deps` services with `depends_on`), so the first launch says "recreated with this shell's compose environment"; a service that compose itself starts with `condition: service_healthy` on an idle service fails (launch the dependency first); an IDE's own fast-mode container is refused until a plain `docker compose up -d` recreates it. One app per container (§3 claims), one run per project at a time (a lock file).

### Python

Served by the manifest alone (`internal/adapters/manifests/debugpy.json`, debugpy 1.8.22, MIT): `eyedbg start python --program app.py` or `--opt module=pytest` (like `python -m`). One interpreter runs both debugpy (`<interpreter> <root>/debugpy/adapter`) and the program (debugpy's `python` launch argument): `--opt python=PATH`, else `EYEDBG_PYTHON` (from the daemon's environment), else the caller's `$VIRTUAL_ENV` (read by the CLI and sent in the start request, like the client's directory), else a `.venv`/`venv` holding `pyvenv.cfg` in the working directory, then the program's directory and its parents (only directories the user owns; the venv itself must pass the manifest permission check), else `python3`, `python`, `py -3` on PATH; Python 3.10+. debugpy comes from `eyedbg adapters install python` (the pinned pure-Python universal wheel, SHA-256- and size-checked, no pip or venv; it has no compiled speedups) when installed, else from the interpreter's own. The working directory defaults to where `eyedbg` ran (like `python app.py`). `--project` and `--no-build` are ignored.

- Child processes run but aren't debugged (`subProcess: false`; debugpy's `debugpyAttach` event is dropped and its `startDebugging` reverse request refused). `attach` is refused (`UNSUPPORTED_BY_ADAPTER`: debugpy needs gdb or lldb to inject into a pid).
- Exception modes: `all` → filters `raised` + `uncaught` (debugpy stops at each frame the exception passes through, then once more if nothing catches it); `uncaught` → `uncaught` + `userUnhandled` (one stop where it leaves the user's code; frame 0 is the raise site, and execution waits in the frame debugpy marks "(Current frame)"); `none` lets an uncaught exception end the program with its traceback in the output.
- Scopes are Locals and Globals; special (dunder) and function variables are hidden, class variables grouped (`variablePresentation`), so snapshots and `--changed` read Locals only (§7). `eval` refuses, without `--allow-side-effects`, calls other than read-only builtins (`len`, `str`, `repr`, `type`, `isinstance`, …), `=`, `:=` and f-/t-strings (the manifest's `evalGuard`; best-effort, as for .NET). Watch-context evaluation of an unknown name is `ADAPTER_ERROR` ("NameError: …"); a string's value is its repr.
- Function breakpoints match the bare function name (`func:price`), stop with reason `function breakpoint`, and debugpy verifies any name.
- Stop on entry stops at the module's first executable line.

### C, C++ and Rust

Served by three manifests over one executable, LLVM's **lldb-dap** (Apache-2.0 WITH LLVM-exception,
stdio by default; ADR 0013): `lldb-dap-c`, `lldb-dap-cpp` and `lldb-dap-rust`, one language each,
`entry: lldb-dap` found by `EYEDBG_LLDB_DAP` or PATH (rarely on PATH by default: macOS's Command
Line Tools install it under `/Library/Developer/CommandLineTools/usr/bin/`, Debian/Ubuntu package it
as `lldb-dap-NN`), `args: ["--repl-mode", "variable"]` (the repl only ever evaluates expressions,
never LLDB commands). No managed download (LLVM's own archives are 0.25–1.8 GB `.tar.xz`/`.tar.zst`,
over the installer's cap and needing a new decompression dependency). `--env` reaches the program
through `${envList}` (a sorted `"NAME=VALUE"` list), the shape lldb-dap 18/19 need (20+ also accepts
an object); C++ alone maps `--exceptions all` to `cpp_throw` (`uncaught` has no lldb-dap filter and
is refused with the adapter's own list). Rust's `String`/`Vec`/`HashMap` show raw layouts unless its
LLDB formatters are imported (`~/.lldbinit`, from `rustc --print sysroot`; a documented manual step,
not automated — follow-up F7); Rust panics have no exception filter. `eval`'s side-effect guard
covers `sizeof`/`alignof` (C++ also `decltype`/`typeid`) and the usual assignment operators;
`strlen()` and similar bare calls still run code, best-effort as for every language's guard.
Two lldb-dap quirks the session absorbs for every adapter: lldb-dap 18–20 answer `setVariable` with
`"result"` instead of `"value"`, so an answer without a value makes `set` re-read the variable from
its parent; and on Linux lldb-dap pauses with SIGSTOP and reports the stop as reason `exception`
(`signal SIGSTOP`), so a SIGSTOP signal stop while a pause is outstanding is reported as `pause`
(its description kept). Not absorbed: lldb-dap 18 answers a pause sent before its event thread has
seen the program resume after `configurationDone` (a sub-millisecond window right after attach) with
success and no stop; the pause times out and a second one stops the program.

### Go

Served by the manifest alone (`internal/adapters/manifests/delve.json`, Delve 1.27.2, MIT), the
first language on the **connect transport** (ADR 0013): `dlv dap` never speaks stdio, only a Unix
socket it dials in to (`args: ["dap", "--client-addr=unix:${socket}"]`). `eyedbg start go --program
DIR` (a package directory or `.go` file) debugs with `mode: debug` (the default `--opt`); `--opt
mode=exec` runs an already-built binary (`go build -gcflags=all=-N -l` first, so breakpoints and
locals are reliable); `--opt mode=test` runs the package's tests, in `--cwd`, not the package
directory `go test` itself would use. `--opt buildFlags` passes extra `go build` flags (e.g.
`-tags=integration`). Delve builds a temporary `__debug_bin*` in the working directory for debug
mode, removed after the session. Exceptions: both `all` and `uncaught` map to `unrecovered-panic`
and `runtime-fatal-throw` (Delve has no separate filter for a recovered panic); an unrecovered
panic's own physical breakpoint sits inside the runtime, not necessarily the frame the client shows
first. `func:main.price` (package-qualified, unlike Python's bare names) sets a function breakpoint.
`eval`'s guard allow-lists Go's built-ins (`len`, `cap`, `min`, `max`, `real`, `imag`, `complex`) and
conversion type names (`int`, `string`, …) as safe bare calls; Delve's own `call ` evaluate prefix
(actually invoking a function) is invisible to the guard, which flags `name(` regardless of what
precedes it. Delve 1.27.2 accepts Go 1.25–1.27; a distro Delve older than 1.24 can't dial a Unix
socket at all (the installed pinned copy always wins over an older PATH one for this reason).
Downloads: `linux/{amd64,arm64}`, `darwin/{amd64,arm64}`, `windows/amd64` (Delve publishes no
others for 1.27.2; a future bump may have to skip a release with none at all).

## 9. Phase 2: editors co-debugging

Built (P2-M1, ADR 0012):

- **`eyedbg dap [-s ID] [--as CLIENT]`** is an editor's debug adapter command: it authenticates on
  the daemon's socket like every command, switches the connection to DAP (`facade.open`, §6) and
  copies bytes between stdio and the daemon. VS Code (through the extension), nvim-dap
  (`{type = 'executable', command = 'eyedbg', args = {'dap', '--as', 'human:NAME'}}`) or a script
  then attach with standard DAP. The token never reaches the editor; there is no other listener.
- **The facade holds no policy.** `initialize` answers once the session finished starting, with
  the adapter's capabilities adjusted (eyedbg's hit counts and logpoints on; restart, step back,
  goto, memory writes, data breakpoints, terminate-debuggee and the like off); `attach` joins
  (`launch` is refused there: a launch connection takes it, P2-S3a below); `configurationDone` is the join point — the current stop is replayed and
  the session's event log followed from there. Execution requests (`continue`, steps, `pause`,
  `terminate`, `setVariable`/`setExpression`, `evaluate` in the debug console) go through the lease
  exactly like the CLI's; reads are forwarded (stack, scopes, variables, …), `evaluate` in watch or
  hover (with a frame) behind the side-effect check, reads the adapter didn't declare refused as
  unsupported; everything else is refused. One table in
  `internal/session/forward.go` is the whole policy.
- **Breakpoints:** `setBreakpoints(file)` replaces the connection client's editor breakpoints for
  that file (ids are session breakpoint ids, stable across re-sends), except the ones the editor
  set under another path of the same file (a symlink); others' are untouched and keep merging per
  line. At most 1000 entries per request. `setExceptionBreakpoints` sets the client's own exception mode.
- **Events** come from the event log, not the adapter: stops, other clients' resumes (`continued`)
  and state changes (`invalidated`), output (logpoints as console), threads, the end, and changes
  to the breakpoints the editor knows. A response always precedes the events its request causes.
  A `stopped` eyedbg decided (§4, shared lines, hit counts, logpoints) carries `hitBreakpointIds`
  (P2-S1, ADR 0018), live, at the join and on a resync.
- Errors are DAP error responses whose `message` is the stable code; `body.error.variables.code`
  carries it for clients, with the holder for `LEASE_HELD`.

Built (P2-M2, ADR 0014):

- **Presence:** each `eyedbg dap` connection counts as its client's presence (`sessions` shows who
  is connected; `client connected`/`disconnected` events). When a client's last connection closes,
  its editor breakpoints are removed, the exception mode its editor set is reset and its lease is
  released — after 10 s for a Restart (`disconnect {restart: true}`), and not if it came back.
- **`eyedbg lease request [--message TEXT]`** (and DAP `eyedbg/lease {action: request}`) asks the
  holder: a pending request shown in every stop's sharing line until the lease changes hands.
- **Custom DAP messages** for the editor extension: requests `eyedbg/lease` (the lease commands),
  `eyedbg/clients`, `eyedbg/breakpoints` (list, remove as `bp rm`); events `eyedbg/lease`,
  `eyedbg/clients`, `eyedbg/breakpoints`, `eyedbg/activity` (other clients' actions).
- **Shared breakpoints:** other clients' line breakpoints (and the client's own CLI ones) are
  announced as DAP `breakpoint` events at column 1 — the mark by which the facade recognises
  VS Code's adopted copies when the editor re-sends them, so a copy is answered with the owner's
  breakpoint (its condition kept) and never becomes the human's. A copy of a breakpoint that's gone
  is retracted; removing a copy only hides it for that editor; editing one makes a breakpoint of
  the human's own. Copies are retracted when the editor leaves or the session ends.
- **Output replay:** the output from before the join (the newest 200 chunks, 64 KiB) is replayed
  once, before the program's state.

Built (P2-M3, ADR 0015):

- **The VS Code extension** (`extensions/vscode/`; TypeScript bundled by esbuild, no runtime
  dependencies, VS Code ≥ 1.100): debug type `eyedbg` — `attach` joins a running session (picked
  when not named; Run and Debug lists one configuration per session), `launch` starts one as the
  human — since P2-S3a a DAP launch through `eyedbg dap --launch` (below), before it `eyedbg start`
  and an attach.
- **Which binary:** the machine-scoped `eyedbg.path`, else the extension's own PATH scan (absolute
  entries, never the current directory; `eyedbg.exe` on Windows), checked with `version --json`;
  every call is `execFile` without a shell, values bound as `--flag=value`.
- **The lease:** a status bar item (who has control, the policy, pending requests) with the actions
  the session allows; on `LEASE_HELD` a notice with Request Control / Take Over; other clients'
  requests shown to the holder; after taking control from an agent under `free`, an offer to
  switch to `human-priority` (`eyedbg.lease.afterTakeOver`).
- **Other clients' breakpoints:** VS Code draws each mirror as a red dot; the extension adds whose
  it is and what it does at the end of the line, a plain-text hover, and *Copy as My Breakpoint* /
  *Remove Breakpoint for Everyone…* on the line-number menu — never a gutter icon (it would stop
  gutter clicks on that line). It tracks the mirrors from the DAP traffic, as VS Code applies it,
  since VS Code hides its copies from extensions.
- **Activity:** an "EyeDebugger Activity" output channel with other clients' actions, phrased as
  `eyedbg events` phrases them. Every string from a session is rendered as plain text; nothing in a
  notification can become a link.

Releases carry the VSIX (checksummed, attested); a workflow publishes it to the Marketplace and
Open VSX once the maintainer adds the secrets (ADR 0015 addendum).

Built (P2-M5, ADR 0015 and 0014 addenda):

- **Views in Run and Debug:** *EyeDebugger Activity* (other clients' actions; a step shows and
  opens the line it stopped at, read from VS Code's own `stackTrace` after the stop) and
  *EyeDebugger Clients* (who is in the session, who has control, last seen; lease actions per row).
- **Follow the agent:** the facade marks a stop caused by another client's execution request with
  `preserveFocusHint`, so VS Code no longer focuses the editor or raises its window for it; the
  extension shows the line without taking focus (`eyedbg.followAgent`).
- **Auto-join prompt:** in a trusted window with focus and nothing joined, `eyedbg sessions` every
  5 s; an agent's session of a program in the workspace is offered once (`eyedbg.autoJoin`).
- **launch.json snippets** and a *Get started* walkthrough.

Built (P2-M9, ADR 0015 addendum): the human side of `eyedbg dotnet …`, in an *EyeDebugger .NET*
activity-bar container shown in .NET workspaces — **Counters** (a live `counters --watch`, values
and changes; Pause ends the watch; a joined session stopped at a breakpoint restarts it when it
runs), **Memory** (a heap dump's top types; a type's GC root paths on expand), **Threads** (stacks
grouped as the CLI's text, locks; a frame's source opened only for a local absolute path of a
regular file, the helper's rule) and **CPU Trace** (hottest methods, or GC and allocations). One
target — the active joined session, a session or a process from `dotnet ps` — every run
cancellable (SIGINT off Windows; on Windows, `EYEDBG_CANCEL_ON_STDIN_EOF=1` plus closing stdin,
since Windows has no graceful Ctrl-C for a child process — either way eyedbg discards a partial
file), dumps and traces left in eyedbg's private directory. No new CLI surface.

Built (P2-S1, ADR 0018): **per-owner conditions at shared lines** — breakpoints of several clients
at one line each keep their own condition, hit count and log message (§3, §4); a stop names the
breakpoints it is for (CLI, events, `--json`, DAP `hitBreakpointIds`, which VS Code uses to select
the hit breakpoint). The extension is unchanged.

Built (P2-S3a, ADR 0019): **launching through the facade** — `eyedbg dap --launch [--as CLIENT]`
opens a launch connection (`facade.open {launch}`, §6), and the client's DAP `launch` starts the
session through `session.Manager.Launch` as that client (the checks, time limit and order of
`eyedbg start`; the launcher holds the lease). Launch arguments are known keys only, typed (`lang`,
`program`, `project`, `cwd`, `args`, `env`, `opts`, `stopOnEntry`, `noBuild`, `leasePolicy`,
`exceptions`, `adapter`); other keys are ignored, a case variant of a known key is refused, and
relative paths resolve against the `eyedbg dap` process's directory. `initialize` answers at once
with the forced-on capabilities and both exception filters (a filter the adapter can't serve is
turned off for that client, with one console line); the build streams to that connection only as
console `output` (the .NET driver builds, then queries the program's path; bounded: 4096 bytes a
line, 10,000 lines, 1 MiB). Once the adapter is initialized the connection binds the session and
sends `eyedbg/session {sessionId}`, a `capabilities` event (the adapter's) and `initialized`; the
editor's `configurationDone` runs behind its breakpoint and exception requests, so they are in
place before the program runs; `launch` is answered once the session started, and the connection
then joins as an attach does. The launcher's `terminate` ends and forgets the session (`eyedbg
stop`); when the launcher leaves, an exited session is forgotten and a live one keeps running. So a
Restart restarts the program. `version --json` lists `dap.launch`. The VS Code extension's F5 runs
`eyedbg dap --launch --as human:NAME` as its debug adapter (in the launch's cwd, else the workspace
folder, else an absolute program's directory) and sends the validated configuration as the launch; it needs `dap.launch` (no fallback to
`eyedbg start`), learns the id from `eyedbg/session`, and no longer stops sessions itself: Stop is
the facade's terminate, and Restart a new launch.

Built (P2-S3b, ADR 0019): **terminals** — the launch argument `console` is
`internalConsole` (default) or `integratedTerminal` (Python only: a manifest's `launch.terminal`,
debugpy's, is merged over its launch arguments; the .NET driver and a manifest without it refuse:
`UNSUPPORTED_BY_ADAPTER`). With `integratedTerminal`, the adapter's `runInTerminal` during that
launch (§3) is checked against the validation table (§11) and sent to the launching connection as
the custom event `eyedbg/runInTerminal {id, title, cwd, args, env}` built from the checked values;
the editor starts `args` itself, without a shell, and answers with the request
`eyedbg/runInTerminal {id, processId}` or `{id, error}` within 30 s, which the adapter gets as its
answer. A refused request, the editor's error or silence fails the launch at once
(`ADAPTER_ERROR`). The standard `runInTerminal` is never sent to an editor (VS Code's types a
command line into a shell). `version --json` lists `dap.terminal`. The VS Code extension
(`console` in launch configurations) accepts the event only for its own launch that asked for a
terminal, while that launch is pending, once; re-checks it against the same table; and runs it with
`createTerminal({shellPath: args[0], shellArgs: args[1..]})` — the program is the terminal's own
process, no shell, no `sendText` — answering with the terminal's process id. Other DAP clients get
no terminal.

## 10. Agent integration

- `SKILL.md` shipped with the binary (`eyedbg skill print`/`install`): when to reach for the debugger (after a failed hypothesis or two, per debug-gym), the standard loop (`start → bp add → run-until --dump → vars --changed → eval`), budgets, and "always `eyedbg stop` when done". `skill print` writes it to stdout byte for byte; `skill install [--dir ROOT] [--force]` writes it to `ROOT/eyedbg/SKILL.md`, `ROOT` defaulting to Claude Code's personal skills directory (`~/.claude/skills`, `%USERPROFILE%\.claude\skills` on Windows); idempotent, and a differing existing file is left alone unless `--force`. `skill/eyedbg/SKILL.md` is embedded in the binary, so it is always in sync with the `eyedbg` that prints it; `TestSkillMatchesCommandTree` (`internal/cli`) checks its examples and exit codes against the real command tree.
- No MCP server by default — agents use the CLI directly; its help is the documentation. A thin MCP wrapper may be added later only if a concrete agent needs it (non-goal for MVP).

## 11. Safety

- `eval` may run code (method calls, property getters). Default: read-only intent (DAP `context: "watch"`) plus the driver's syntactic check (`SIDE_EFFECTS`; for manifest languages, the manifest's `evalGuard`), since netcoredbg enforces nothing and debugpy only refuses statements in `watch`; `--allow-side-effects` (context `repl`) is an execution request: it needs the lease and is logged. `vars --expand` doesn't evaluate a path the check flags. Breakpoint conditions and logpoint expressions are not checked: the user wrote them for the program to run. The conditions eyedbg evaluates itself — a shared line's `--if` (§4) and every `--log` message's `{EXPR}` — run in the program as the adapter's would, without the lease; the daemon's log holds neither their text nor their values, and recordings keep a stop's reason and thread only (no attribution).
- Socket access restricted to the user; token file; no TCP listener by default. Editors reach sessions through the same socket and token (`eyedbg dap`); their DAP frames are bounded (one `Content-Length` header, ≤ 1 MiB) and a malformed one closes the connection. A launch connection (ADR 0019) logs its launch's language only, never its arguments; the build's output goes to that connection alone, bounded, never to the event log or the daemon's log.
- Terminals (ADR 0019 D7–D11): a command reaches an editor's terminal only from that editor's own launch (§3), refused whole — never cut or rewritten — unless it passes one table: the integrated kind, no shell interpretation, 1–1000 arguments, no control characters (C0, DEL, C1, U+2028/2029) anywhere, ≤ 256 KiB, the program and working directory local absolute paths (no UNC or `\\?\`; a drive root on Windows), on Windows an `.exe` and no single-space or quote-enclosed spaced argument (node-pty would pass it unquoted), plain environment names with string or null values, a title of ≤ 200 characters. The editor gets only the checked values, re-encoded, and runs them without a shell; the extension checks them again. Nothing of the command is logged.
- Redaction (§5), not implemented yet. Session recordings (`sessions/<id>.jsonl` in the private runtime dir, 0600, never overwritten, capped at 4 MiB, pruned 7 days after the session ended) hold control events only: started, client, lease, exec, continued, stopped (reason and thread only), breakpoints (conditions included), threads, exited, ended — no program output, stop text, launch arguments, environment or variable values, until redaction exists. On by default; `start --no-record` or `EYEDBG_NO_RECORD=1` turns it off.
- `attach` only to processes owned by the same user (checked first: Linux `/proc`, other Unix `ps`, Windows the process token's SID), shown as `pid N (name)`, never by command line. For .NET the ptrace-scope/`task_for_pid` hints don't apply: CoreCLR's debugger falls back to its pipe transport when it can't read memory directly (dotnet/runtime `shimremotedatatarget.cpp`), so the real failure modes — not a started .NET runtime, `DOTNET_EnableDiagnostics=0`, another debugger attached, a different `TMPDIR`, another user — are what `ATTACH_FAILED`'s hint lists. A native (non-.NET) manifest-driven adapter's `ATTACH_FAILED` keeps the generic ptrace-scope (Linux)/`task_for_pid` (macOS) hint instead, since such an adapter typically does attach through the OS's own mechanism. `stop` detaches from an attached program, never kills it.
- Containers (ADR 0020): **docker access is the trust boundary.** The same-user pid check above has no meaning for a pid in a container's namespace and eyedbg invents no replacement: it attaches wherever the user's docker may (a session's lease and ownership rules are unchanged). The adapter's DAP stream is untrusted input (it comes from a process in a container): the existing bounded reader, every path in it validated by the path map before it can name a host file (§8), reverse requests as for any adapter. Secrets: eyedbg selects inspect fields through `--format` and so never receives a container's environment, `Cmd` (except whether it is non-empty) or entrypoint arguments (except the assembly name of an exact `dotnet X.dll` entrypoint), runs no `docker compose config` (it inlines `env_file` values, also with `--no-interpolate`, on compose 2.23), and the launched app's environment is never read, copied or passed (§8). Argv only, grammar-checked names (§6). What an attach changes in a container: it writes `/.eyedbg-netcoredbg-<v>/` and runs netcoredbg as the container's own user; it restarts nothing and signals no process it didn't start (netcoredbg detaches on `disconnect`). The only process eyedbg ever kills in a container is the app it launched.
- Fast mode (ADR 0021): the host build has the trust of building the project yourself in the caller's shell (`dotnet publish` runs the project's MSBuild logic with the caller's credentials; the project's own code runs only as the app an explicit `compose launch` starts, in the container). Recreating a service changes the user's running stack, only on an explicit `compose launch`/`restore`, only for the named or implied services, `--no-deps --no-build`. Any container label is untrusted input unless eyedbg-owned state corroborates it: eyedbg's own labels are re-validated at every use and believed only when its override file for the project records them (a launch re-derives nothing from the request, so a caller can't make eyedbg launch an arbitrary program in a container). A project file a container could have written is never built unasked: the search skips everything at or under a read-write bind mount of any container of the stack (read-only mounts stay searchable) and refuses with the `--dotnet-project SERVICE=PATH` hint, a project the user names is built wherever it lies in the compose directory, and each project is printed before its build. MSBuild's and Roslyn's separators (`,` `;` `%` `=` `"`, control characters) in the compose or project path are refused, never escaped; `$` in the override is doubled; compose file lists come from the container's own label, never from parsing YAML. The labels that name the compose directory and files are believed only when corroborated (a regular `.yml`/`.yaml` compose file of the files label lies in the directory, symlinks resolved, and the directory is the one `--project-directory` or the first `-f` names when given): an image's own labels can claim any directory, so a container made by plain `docker run` is refused by `launch`/`restore` and gets no default path map from `attach`; compose files outside the project directory (`--project-directory DIR` with `-f FILE` outside DIR) are therefore not supported. `restore` removes eyedbg's files for a project when it found a fast-mode container of it or when they hold nothing a container can mount (no override, no build), and otherwise keeps them (the real containers may be on another engine). Deletion and overwrite are confined to eyedbg's per-project directory under its home by `Lstat` and exact-name grammars (a symlinked or foreign name is left alone and never followed; the override is the only file replaced, by rename); the build log is written 0600. The stray guard reads process names (`docker top -o pid,stat,comm`) and never command lines. No vsdbg, ever (ADR 0005): eyedbg never mounts, starts or reuses a debugger worker it finds in a container.
- `test` passes the filter, framework and environment to `dotnet test` as argv, never through a shell, and runs it in its own process group, killed as a whole by `stop`.
- The .NET side helper (ADR 0016) inspects only the user's own processes (the `attach` check; `dotnet ps` leaves others' out, which Windows would list), shows them as `pid N (name)` (names from other processes, and counter names and units, made fit for a terminal) and never reads a command line or memory, nor an environment beyond the TMPDIR NETCore.Client looks up on Linux to find the socket; it only lists endpoints and starts EventPipe sessions (no startup hooks, profilers, environment writes or resumes). `$EYEDBG_DOTNET_HELPER` is trusted like shell configuration (absolute paths only; on Windows a `.dll` or `.exe`, never a script); the helper is otherwise found only next to `eyedbg`. A session stopped at a breakpoint is refused before the helper starts; a paused `--pid` ends after 5 s (`DIAGNOSTICS_TIMEOUT`). The same-user check covers the pid, not its endpoint: NETCore.Client finds the endpoint by name (a socket in a temp directory on Unix, a pipe on Windows), so on a shared machine another local user can pose as it — deny, feed false data, and on Windows act with the caller's identity. A private TMPDIR closes it on Unix (macOS's default is private); on Windows only a `dotnet-diagnostic-dsrouter-PID` decoy is refused (ADR 0016).
- Dumps (P2-M7, ADR 0016 addendum) hold a program's memory: eyedbg writes them only under fresh random names in `<home>/dumps` (0700, not a symlink, owned by the user on Unix; files 0600; Windows: the profile's ACL), never prints their values (types, counts, sizes, addresses, root kinds, static field names, frames — no field values, strings, thread names or exception messages), prunes its own after 7 days and beyond the newest 10, and never overwrites at `--out` (hard link, else an `O_EXCL` copy). The analysis library (DAC, native code) is loaded only from the installed runtime of the dotnet running the helper, or — for eyedbg's own dumps — from the dump's recorded runtime directory when no other user can write there, after a version or (Linux) build-id match; ClrMD never downloads symbols or reads a shared symbol cache. Tests use a temporary `EYEDBG_HOME`.
- Traces (P2-M8, ADR 0016 addendum) hold method, module and type names, GC data and what the runtime records about the process — its command line and module paths: they go to `<home>/traces` under the dumps' rules (fresh names, created `O_EXCL` 0600, pruned after 7 days and beyond the newest 10, `--out` never overwrites), TraceLog's temporary file stays in that directory and is removed after each summary, and only names and counts are ever printed (no event payloads; exception events aren't collected). A `.nettrace` is parsed as untrusted input: no symbol lookup, no file a trace names is opened, parser failures are `TRACE_UNSUPPORTED` without echoing the parser's message. Tests use a temporary `EYEDBG_HOME`.
- Adapter manifests (§7, ADR 0011): the user's are trusted like shell configuration and permission-checked on Unix, never read from a project; downloads are pinned (https, SHA-256, size) and extracted through a staging directory that refuses escaping entries. The Python interpreter probe runs with `-c` after dropping the working directory from `sys.path`, so a cloned repository's `debugpy.py` or `sitecustomize.py` is never used; the adapter is started by path, never with `-m`. A discovered venv is run only if the user owns it and no one else can write it, and only directories the user owns are searched, so another user's `/tmp/.venv` is never run (on Windows, which has no mode bits, only directories inside the user's profile are searched).

## 12. Repo layout (Go)

```
cmd/eyedbg/            CLI
cmd/eyedbgd/           daemon (also `eyedbg daemon run`)
internal/api/        native JSON-RPC schema (shared by the CLI, the extension, and any future MCP wrapper)
internal/daemon/     lifecycle, IPC, auth
internal/session/    session, lease, breakpoint store, event log, stop snapshot
internal/dap/        DAP over go-dap, both ways: bounded framing, the client toward adapters (seq mapping, reverse requests), the server side of editor connections
internal/facade/     DAP facade: an editor's connection served as session calls (eyedbg dap, ADR 0012; launch, ADR 0019)
internal/present/    budgeting, truncation, text/JSON renderers
internal/adapters/   manifest schema, loader and trust check, templates, installer (download + checksum), Python runtime
internal/adapters/manifests/   bundled adapter manifests (netcoredbg, debugpy, lldb-dap-{c,cpp,rust}, delve)
internal/proc/       process owner lookup (attach, dotnet), process groups (test runs, side helpers)
internal/helper/     side-helper runner: spawn, hello, one call with notifications, cancel/kill (helpertest: the fake helper)
internal/artifacts/  private artefact directories: dumps and traces (0700/0600, EYEDBG_HOME), fresh names, pruning, --out placement; compose's fast-mode builds (`<EYEDBG_HOME>/compose/<project>`: stage dirs, the mirror, a lock)
internal/container/  docker for containers (ADR 0020, 0021): engine argv, inspect templates, the adapter tar and copy, compose ps/up, the fast-mode override, docker top (containertest: the fake docker)
internal/cli/        command trees for all binaries (only package importing the CLI framework)
internal/version/    build metadata (ldflags / debug.ReadBuildInfo)
drivers/dotnet/      Driver impl
drivers/generic/     manifest-only driver
helpers/dotnet/      C# side helper (ADR 0016): src/EyeDbg.DotnetHelper (Rpc/, Methods/, Counters/, Diagnostics/, Dumps/, Analysis/, Tracing/),
                     tests/ (xUnit v3), global.json, nuget.config, Directory.*.props, lock files, THIRD-PARTY-NOTICES.txt
skill/eyedbg/SKILL.md   agent-facing usage guide, embedded in the binary (`eyedbg skill print|install`)
internal/e2e/        CLI end-to-end tests driving the real eyedbg/eyedbgd binaries against the sample apps
testdata/apps/       sample debuggees per language (dotnet/, python/, c/, cpp/, rust/, go/); dotnet/compose is the docker compose fixture
extensions/vscode/   VS Code extension (TypeScript, pnpm, esbuild; ADR 0015)
```

## 13. MVP milestones

1. **Skeleton** (done): daemon auto-start, IPC + token, version handshake, `daemon start/status/stop/logs`.
2. **DAP core** (done): go-dap client, netcoredbg install/doctor, `start` a console app, `bp add` (line), `continue`, `status`, `stack`, `vars`, `stop`.
3. **Agent ergonomics** (done): stop snapshot, `--dump`, `run-until`, `wait`, `--changed`, budgets, JSON
   schema, errors, `eyedbg help --all` (the full command tree's help in one read) and a `--json`
   help variant.
4. **Model for P2** (done): client identity, bp ownership merge, lease, event log + `events --since`,
   lost sessions and recordings (ADR 0009).
5. **Breadth** (done): hit counts, logpoints, function and exception breakpoints, eval with side
   effects, `set`, `attach`/`detach`, `test`, anchors, capability degradation, sample apps (ADR 0010).
6. **Ship** (done): SKILL.md (`eyedbg skill print|install`), CI matrix (6 os/arch) with real .NET
   and Python e2e, CLI end-to-end tests driving the real binaries against the sample apps.
7. **Second language** (done) via manifest only (debugpy) to prove the plugin boundary: manifest schema,
   loader and trust model, generic driver, `adapters ls`, `--opt` (ADR 0011).

All MVP milestones are done (phase 1 complete); phase 2 is in progress.

Phase 2: DAP facade + VS Code extension; .NET side helper; SharpDbg adapter; more languages.
- **P2-M1 facade core** (done): `eyedbg dap`, the connection switch, the DAP facade with the CLI's
  rules, per-connection breakpoints and cleanup, event-log-driven events (ADR 0012).
- **P2-M2 facade collaboration** (done): presence and releasing the lease on the last editor
  disconnect, `lease request`, `eyedbg/*` custom messages, other clients' breakpoints in editors
  (column-1 mark, echoes, retracts, hiding), output replay (ADR 0014).
- **P2-M6 .NET side helper core** (done): `helpers/dotnet` (C#, protocol 1), `internal/helper`,
  `eyedbg dotnet ps|counters`, the helper in every release archive (ADR 0016).
- **P2-M7 .NET dumps, heap, threads** (done): `eyedbg dotnet dump|heap|threads`, ClrMD 4.1 in the
  helper, `internal/artifacts` (private dumps directory, pruning, `--out`), the DAC provenance rule
  (ADR 0016 addendum).
- **P2-M8 .NET traces** (done): `eyedbg dotnet trace` (cpu and gc profiles, FILE summaries), the
  helper's `trace` and `traceSummary` (TraceEvent's TraceLog), a private traces directory
  (`internal/artifacts` generalized), `TRACE_UNSUPPORTED` (ADR 0016 addendum).
- **P2-M10 SharpDbg** (done): the `dotnet` manifest runtime, `sharpdbg.json` (opt-in, nuget.org),
  `--adapter`/`EYEDBG_DOTNET_ADAPTER` (`AdapterSelector`), sessions' adapter, pause refusal and set by
  evaluate, SharpDbg as the default where netcoredbg has no build, both adapters' e2e (ADR 0017).
- **P2-S1 shared conditions** (done): the stop filter checks each owner's condition at a line
  whose clients' conditions differ, a fail-open truth rule, stop attribution (`stop.breakpoints`,
  DAP `hitBreakpointIds`), run-until's `reached` at a shared line; real-adapter e2e (ADR 0018).
- **P2-S3a launch through the facade** (done): `session.Manager.Launch` with build-output and
  configure hooks, the .NET driver's streamed build, launch connections (`eyedbg dap --launch`,
  `facade.open {launch}`, `dap.launch`), real-binary e2e (ADR 0019), and the VS Code extension's F5
  through it (a Restart restarts the program).
- **P2-S3b terminals** (done): asynchronous reverse replies in the DAP client, the session's
  terminal route (D8), `launch.terminal` (debugpy), the facade's `console`, validation table and
  `eyedbg/runInTerminal` exchange, `dap.terminal`, real-binary e2e, and the VS Code extension's
  shell-free runner (`console` in launch configurations).

- **Containers and compose** (built, unreleased; ADR 0020, 0021): `attach dotnet --container`, the path map and translator, groups (`group.wait`, `group.events`), `eyedbg compose attach|wait|events|bp|stop`, fast mode (`compose launch`, `compose restore`, container-launch sessions), `adapters install --platform`, the fixture `testdata/apps/dotnet/compose`, and a docker e2e (`internal/e2e` `TestComposeAttach` and `TestComposeLaunch`, behind `EYEDBG_E2E_DOCKER=1` on top of `EYEDBG_E2E=1`) that CI runs on its two ubuntu e2e entries.

**More languages** (ADR 0013, run independently of phase 2's own sequencing): C, C++, Rust
(lldb-dap, manifest-only, no schema change) and Go (Delve, manifest-only through a new
`adapter.transport: "connect"`) are done. Ruby, Java, Kotlin, JS/TS and PHP were scoped and
descoped in the same task, each needing strictly more than schema 1 offers today — see ADR 0013's
follow-ups F1–F5.

## 14. Risks & open questions

- netcoredbg eval limits and macOS arm64 stability → SharpDbg as the alternative (ADR 0017); e2e
  runs in CI on `ubuntu-latest` on every push, and on all 6 platforms (milestone 6) on
  `workflow_dispatch` and the weekly schedule: .NET with both adapters on Linux (x64/arm64), macOS
  (arm64) and Windows (x64), and with SharpDbg alone on Intel Macs, where netcoredbg has no build;
  Python on all 6. Windows on Arm has no netcoredbg build either, and SharpDbg is withheld there
  (its e2e failed intermittently: `start` missed the first breakpoint), so .NET debugging there
  is `--adapter sharpdbg`, unverified.
- SharpDbg is 0.1.x with one maintainer (ADR 0017): pause and native logpoints broken, its eval
  compiles and loads code into the debuggee (the C# side-effect guard stays), and it bundles a
  Microsoft-licensed (non-OSS) DAP library with a data-collection clause (no network check made).
  Pinned; opt-in; a failing platform is withheld from the default (`SharpDbgWithheld`).
- `eyedbg pause` before the program's first stop: netcoredbg lists its threads while running
  (found in P2-M10), so a pause with no thread given asks for them first and pauses one it lists,
  falling back to today's choice (and today's `ADAPTER_ERROR` if that's refused) when the answer is
  empty or the request fails.
- netcoredbg release cadence (~2/yr, single corporate maintainer) → pin versions, keep our own builds.
- Test debugging flow (`VSTEST_HOST_DEBUG`) and `attach`: validated on Linux, macOS (arm64) and
  Windows (x64) by CI e2e's weekly/dispatch full matrix (milestone 6); on Intel Macs and Windows on
  Arm by the same e2e with SharpDbg (P2-M10).
- The Microsoft.Testing.Platform/xUnit v3/TUnit launch path and netcoredbg's untrusted exit codes on
  macOS (ADR 0010's amendment): validated by unit tests and local macOS e2e (both adapters) as of
  this commit; CI's full-matrix e2e (all 6 platforms) not run yet — update this line once it is.
- Lease policy default for P2: decided (ADR 0014) — stays `free`; the editor extension offers
  `human-priority` after a human takes over from an agent under `free`.
- Anchor re-resolution after edits: decided (ADR 0010) — anchors resolve once, exactly; a changed file gets a note, and a future `restart` can re-resolve every anchor against the rebuilt program.
- Windows: decided (§6) — AF_UNIX everywhere; no named-pipe fallback needed so far.
- Python: the downloaded pure-Python debugpy has no compiled speedups (tracing speed unmeasured); attach to a running Python process (gdb/lldb injection, Python 3.14's `sys.remote_exec`) and debugging child processes are future work; interpreter discovery (Windows `py`/Store aliases, conda/poetry venvs outside the project) is checked by unit tests only; the end-to-end tests now run on all 6 CI platforms (milestone 6).
- Delve dialing out over `AF_UNIX` on Windows (ADR 0013): Go has supported it since Windows 10
  1803, and Delve's own dial call is unconditional, but it was unverified at runtime when the
  connect transport was added — CI's `windows-latest` matrix entry now exercises it on every run;
  if it fails, the fix is not a TCP fallback (that needs its own authentication design, ADR 0013's
  "Considered Options"), but confirming Windows's Unix-socket support against the specific runner
  image, or reporting the gap upstream to Delve.
- lldb-dap discovery (ADR 0013 follow-up F6): it is rarely on PATH by default (macOS Command Line
  Tools, Debian/Ubuntu's `lldb-dap-NN` packages), so `EYEDBG_LLDB_DAP` is the only way past a bare
  `path` lookup finding nothing on a stock machine; `adapters doctor` reports it missing until set.
- C++ `--exceptions uncaught` and Rust panics have no lldb-dap exception filter (documented, not
  fixed); Rust's own value formatters need a manual `~/.lldbinit` step (follow-up F7) or its
  `String`/`Vec`/`HashMap` show raw layouts.
- Containers (ADR 0020, 0021): verified on two machines only (macOS Docker Desktop 24.0.7 / compose 2.23.3 arm64, Debian 13 docker.io 26.1.5 / compose 2.26.1 amd64), Linux containers. Unverified and unsupported: Windows hosts, podman, rootless docker, SELinux relabelling, remote engines, emulated architectures, Windows containers, compose profiles and scaled services, `tty: true` services' `TERM` under fast mode, and attaching to the .NET child of an `init: true` container. Line breakpoints don't bind on an attach to a Release image (fast mode, or a Debug image, is the way); a lost client leaves a stray frozen program (`docker restart NAME`); an attach streams netcoredbg (~9 MB) every time; images that aren't glibc linux/amd64 or linux/arm64 (Alpine, chiseled, distroless) can't be attached to, and chiseled/distroless ones can't idle either; PDBs naming sources under another root leave breakpoints pending until `--map`. Fast mode: no drift check (compose's hash can't be compared), `command:` arguments, non-exec-form entrypoints and `service_healthy` dependents are unsupported, the service is down without its session, output is not in `docker logs` (mirroring it is a follow-up), the app directory is read-only, and a multi-targeted project fails `publish` (`--framework` is a follow-up).
- lldb-dap breakpoints for c/cpp/rust never bind when the build/working directory is reached
  through a symlink (e.g. macOS's `/tmp`, `/var`): lldb-dap matches the compiler's own unresolved
  path, while eyedbg resolves symlinks before sending a breakpoint. debugpy, netcoredbg and Delve
  are not affected (documented, not fixed).

## 15. References

- netcoredbg: https://github.com/Samsung/netcoredbg (releases; `src/protocols/vscodeprotocol.cpp`)
- debugpy: https://github.com/microsoft/debugpy (`src/debugpy/adapter/clients.py`, `launcher/handlers.py`) · https://pypi.org/project/debugpy/
- vsdbg licensing: https://github.com/dotnet/vscode-csharp/blob/main/docs/debugger/Microsoft-.NET-Core-Debugger-licensing-and-Microsoft-Visual-Studio-Code.md
- SharpDbg: https://github.com/MattParkerDev/sharpdbg · ClrDebug: https://github.com/lordmilko/ClrDebug
- ClrMD / diagnostics: https://github.com/microsoft/clrmd · https://learn.microsoft.com/en-us/dotnet/core/diagnostics/
- go-dap: https://github.com/google/go-dap · DAP spec: https://microsoft.github.io/debug-adapter-protocol/
- NativeAOT cross-compile limits: https://learn.microsoft.com/en-us/dotnet/core/deploying/native-aot/cross-compile
- Delve multiclient: https://github.com/go-delve/delve/tree/master/Documentation/api/dap · issue #2322
- js-debug child sessions: https://github.com/microsoft/vscode-js-debug/blob/main/src/dapDebugServer.ts
- Prior art: https://github.com/debugmcp/mcp-debugger · https://github.com/microsoft/DebugMCP · https://www.jetbrains.com/help/idea/mcp-server.html
- Evidence: debug-gym https://arxiv.org/abs/2503.21557 · ChatDBG https://arxiv.org/abs/2403.16354 · CLI vs MCP https://mariozechner.at/posts/2025-08-15-mcp-vs-cli/
