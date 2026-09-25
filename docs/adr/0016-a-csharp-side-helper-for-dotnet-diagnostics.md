---
status: proposed
date: 2026-09-25
decision-makers: Ijat (@ijat)
---

# A C# side helper for .NET diagnostics

## Context and Problem Statement

A debugger stops a program to show it; many .NET questions are about a program that keeps running:
is it allocating, collecting, throwing, starving its thread pool? The .NET runtime answers them
through its diagnostics endpoint (a Unix socket in `TMPDIR`, a named pipe on Windows) and EventPipe,
reachable from .NET code with Microsoft's `Microsoft.Diagnostics.NETCore.Client` and
`Microsoft.Diagnostics.Tracing.TraceEvent`; there is no Go client. DESIGN §7 planned "side helpers"
for this: out-of-process, JSON-RPC over stdio, any language. This ADR decides how the first one,
`eyedbg-dotnet-helper` (P2-M6: `eyedbg dotnet ps` and `eyedbg dotnet counters`), is run, shipped,
built and constrained, as the base for dumps, heap and thread views (M7) and traces (M8).

Verified while planning (.NET 10 targets; macOS arm64, Linux x64, Windows x64): TraceEvent parses
EventPipe without its Windows-only native files; starting an EventPipe session **blocks** while
netcoredbg holds the target at a breakpoint (the managed EventSource callback can't run), and the
async start cancels cleanly and the target recovers after it resumes; a target started with
`DOTNET_EnableDiagnostics=0` has no endpoint; on Windows, NETCore.Client waits for a missing pipe
until its timeout, lists every user's pipes, and a session whose events are never read can't be
stopped (the runtime blocks writing into the full pipe).

## Decision Drivers

* The CLI stays the interface and one static Go binary per platform; nothing new for agents to
  install beyond a .NET runtime they already have.
* Read-only: inspecting a process must not change it or anything it trusts.
* The same-user rule `attach` has (DESIGN §11) holds here too; no command lines or environment of
  other processes are shown or kept (they may hold secrets).
* Third-party build code (NuGet analyzers, source generators, MSBuild assets) never runs where
  release credentials are.
* Every archive of every release carries the helper, or the release fails.
* Windows first-class; bounded everything (lines, stderr, time).

## Considered Options

* **Who runs it:** the CLI, one helper process per command · the daemon, a long-lived helper.
* **What to run:** our own C# helper · Microsoft's `dotnet-counters`/`dotnet-trace`/`dotnet-dump`
  global tools.
* **Distribution:** one framework-dependent `net8.0` build, `RollForward=LatestMajor`, in every
  archive · `net10.0` · NativeAOT per platform · self-contained per platform · a NuGet `dotnet tool`.
* **Build placement:** unprivileged CI/release jobs, goreleaser packs the directory · a goreleaser
  `before` hook running `dotnet publish` in the release job.
* **Wire format:** internal/api's line-framed JSON-RPC · DAP-style `Content-Length` framing · gRPC
  or named pipes.
* **Counter source:** the `System.Runtime` EventCounters provider · the .NET 9+ `System.Runtime`
  meter (`System.Diagnostics.Metrics`).
* **Where aggregation lives:** Go · the helper.
* **C# test framework:** xUnit v3 without Microsoft.Testing.Platform (`xunit.v3.mtp-off`) · MSTest ·
  xUnit v3 default (`mtp-v2`).

## Decision Outcome

Chosen: **the CLI runs our own C# helper, one process per command**, a framework-dependent
`net8.0` build rolled forward to the newest installed runtime, **shipped in every release archive
under `helpers/dotnet/`** next to `eyedbg`, **built in unprivileged jobs**, speaking **internal/api's
framing**, reading **System.Runtime EventCounters** and leaving **aggregation to Go**; C# tests use
**xUnit v3 `mtp-off`**.

### As built

* **Process model.** `eyedbg dotnet <verb>` resolves its target, starts the helper
  (`internal/helper`: its own process group; on Windows `CREATE_NO_WINDOW`), says `hello`, makes
  one call, closes it. The daemon and sessions are untouched: no verb needs state across calls.
  Closing the helper's stdin cancels a running call (the helper stops its EventPipe session within
  3 s and exits without answering); it is killed 2 s after that if it hasn't exited. Ctrl-C reaches
  only eyedbg (the helper is in its own group), which then closes the helper's stdin.
* **Lookup** (`drivers/dotnet/helper.go`): `$EYEDBG_DOTNET_HELPER` (absolute, an existing file; a
  `.dll` runs under the dotnet host, anything else directly; on Windows only `.dll` or `.exe`,
  never a script `cmd.exe` would run), else `helpers/dotnet/eyedbg-dotnet-helper.dll` next to
  `os.Executable()`, then next to the file a symlinked `eyedbg` points to. Never the working
  directory, a project, or a PATH search. The host is the driver's `FindHost`; no host, no helper
  or the host's "framework missing" exit (150 on Unix, `0x80008096` on Windows) is
  `HELPER_NOT_FOUND`.
* **Protocol 1.** One JSON document per `\n`-ended line under 1 MiB, both ways (a longer line is an
  error, never cut); JSON-RPC 2.0 with the daemon's error shape (`-32000`, the stable code in
  `data`). `hello {protocol}` first → `{protocol, version, runtime}`; another protocol is
  `HELPER_MISMATCH`. `processes` → `{processes: [{pid}]}` (published endpoints minus the helper
  itself). `counters {pid, intervalSec 1–60, durationMs}` → `counters.sample` notifications
  (`{elapsedMs, counters: [{provider, name, displayName, unit, kind: gauge|sum, value}]}`), then
  `{samples, endReason: duration|exited}`; `durationMs: 0` runs until stdin closes. stdout carries
  the protocol only (`Console.SetOut(Console.Error)` first); eyedbg keeps the last 4 KiB of stderr,
  shown sanitized only when the helper fails without an answer.
* **Counters.** An EventPipe session (no rundown, 4 MB buffer, `System.Runtime` at Informational,
  `EventCounterIntervalSec`), parsed by TraceEvent on a dedicated thread that **always drains** the
  stream. Values of one interval form one sample (a counter name seen again starts the next);
  non-finite values are dropped. Start is bounded at 5 s (`DIAGNOSTICS_TIMEOUT`: paused or hung);
  stop at 3 s, then the stream is closed under the reader. A stream that ends by itself is the
  target exiting (`endReason: exited`) — also when the parser fails because its input was cut off —
  while a parser failure on a stream that hadn't ended is `HELPER_FAILED`.
* **Targets and errors.** `--pid N` must be the caller's (`internal/proc`; another user's is
  `INVALID_REQUEST`); a session's program must be a running dotnet session: a stopped one is refused
  at once (`NOT_RUNNING`), since its runtime can't start a session. No endpoint is `NOT_DOTNET`, or
  `DIAGNOSTICS_DISABLED` when the process shows signs of .NET (named `dotnet`, or a coreclr module —
  readable on Linux and Windows, not macOS). On Windows the helper first checks the pipe is listed,
  as NETCore.Client lists them, so a non-.NET pid fails at once instead of after the connect
  timeout. `ps` drops other users' processes (Windows lists every user's pipes) and never reads
  command lines.
* **What the same-user check covers.** It decides which pid may be targeted; it doesn't bind the
  connection to that process. NETCore.Client finds the endpoint by name: on Unix the newest
  `dotnet-diagnostic-PID-*-socket` (or a newer `dotnet-diagnostic-dsrouter-PID-*-socket`) in a
  temp directory — on Linux the target's TMPDIR, read from `/proc/PID/environ` (its only use; nothing
  of it is shown), else the helper's own; on Windows `\\.\pipe\dotnet-diagnostic-PID`, or
  `dotnet-diagnostic-dsrouter-PID` whenever that exists. Where another local user can create those
  names — a shared `/tmp` (mode 1777), Windows' shared pipe namespace — they can pose as the
  endpoint: deny, feed false counters and names, and on Windows, since the client connects allowing
  impersonation, act with the caller's identity. The helper refuses a target with a
  `dotnet-diagnostic-dsrouter-PID` pipe on Windows (`INVALID_REQUEST`: eyedbg doesn't inspect
  through dotnet-dsrouter, and any user can create that pipe); nothing else is checked, since the
  socket's or pipe's owner isn't visible through NETCore.Client and a check before its connect
  would race it. The text renderers make every name from the endpoint fit for a terminal. Help and
  DESIGN §11 tell users on shared machines to give the program and eyedbg a TMPDIR only they can
  write (macOS's per-user default already is). This is the same exposure Microsoft's
  `dotnet-counters` and `dotnet-trace` have.
* **Read-only diagnostics.** The helper calls only `GetPublishedProcesses` and
  `StartEventPipeSession[Async]`; never `ApplyStartupHook`, `AttachProfiler`, `SetStartupProfiler`,
  `SetEnvironmentVariable`, `EnablePerfMap`/`DisablePerfMap` or `ResumeRuntime`. `WriteDump` (M7)
  comes with its own review — added by the P2-M7 addendum below. It reads no target memory, environment or command line (a
  trace file, P2-M8, holds the command line the runtime records, unread: see that addendum); error messages
  carry pids, process names and exception types, never target data.
* **Aggregation in Go** (`internal/cli`): a gauge's last, min and max; a sum's total and total per
  second over the intervals it was seen in; missed intervals (a pause) counted against the duration;
  `--watch` prints each sample as the helper sends it. One C# code path, Go-tested and
  golden-locked.
* **Watch latency.** The helper sends a sample when the next one's first value arrives (a name
  repeats) or when the collection ends, so a `--watch` line appears about one interval after its
  sample is taken, and interrupting drops the newest sample. That is TraceEvent's doing, not the
  batching: its EventPipe parser (`EventCache`) holds all but the first of a burst's events until a
  later event arrives, so the helper doesn't have a sample's values any sooner; a quiet-period
  flush was tried and only split samples. Summary mode is unaffected (the end of the collection
  flushes everything).
* **Build and release.** `helpers/dotnet` (SDK pinned to 10.0.x by its own `global.json` — not at the
  repository root, where it would govern `dotnet build` of users' projects under a daemon started
  there): central package versions, lock files with content hashes, locked restores everywhere,
  nuget.org as the only source (package source mapping, audit source), NuGet audit at every
  severity that fails CI (`EyeDbgNuGetAudit=true`), analyzers at `10.0-recommended` and
  code style enforced at build with warnings as errors, the SPDX header enforced by IDE0073.
  TraceEvent is referenced with `ExcludeAssets="build;buildTransitive"` (its Windows-only natives).
  CI's `helper` job builds and tests on Linux, macOS and Windows, and again on the .NET 8 runtime;
  `helper-dist` (CI) and release.yml's `helper` job publish it with `contents: read` only and hand
  the directory to goreleaser as an artifact; `.goreleaser.yaml` names the directory without glob
  characters, so a missing helper fails the release instead of shipping archives without it.
  `THIRD-PARTY-NOTICES.txt` ships with it; CI checks every shipped assembly is named there.

### Dependencies

Shipped (all MIT, © Microsoft Corporation; build assets none unless noted):

| Package | Version | Why |
|---|---|---|
| Microsoft.Diagnostics.NETCore.Client | 0.2.745401 | the diagnostics IPC: endpoint discovery, EventPipe sessions |
| Microsoft.Diagnostics.Tracing.TraceEvent | 3.2.6 | the EventPipe (nettrace) parser; `build`/`buildTransitive` excluded (Windows ETW/DIA natives, ~10 MB); ships `Dia2Lib.dll`, `TraceReloggerLib.dll` as lib assets |
| Microsoft.Extensions.Logging.Abstractions | 8.0.3 | NETCore.Client dependency (analyzer runs at build) |
| Microsoft.Extensions.DependencyInjection.Abstractions | 8.0.2 | transitive |
| System.Collections.Immutable, System.Reflection.Metadata, System.IO.Pipelines, System.Text.Encodings.Web | 9.0.8 | TraceEvent/System.Text.Json dependencies |
| System.Text.Json | 9.0.8 | TraceEvent dependency (source generator runs at build) |

P2-M7 adds ClrMD and its closure, and moves some of the versions above; see the addendum's
dependency table.

Test only: `xunit.v3.mtp-off` 4.0.1 (Apache-2.0) and its closure (xunit.v3.*, xunit.analyzers
2.1.0, Microsoft.Bcl.AsyncInterfaces 6.0.0; buildTransitive props/targets of
`xunit.v3.core.mtp-off`).

### Consequences

* Good, because `eyedbg dotnet counters` works on all six targets from one 5 MB, IL-only build, on
  any installed .NET 8 or later, with nothing to install beyond what .NET developers have.
* Good, because the helper can't outlive a command or hold state; a crash is one command's
  `HELPER_FAILED` with its stderr.
* Good, because NuGet build code runs only in unprivileged jobs and on developer machines; the
  release job packs a directory.
* Bad, because every command pays the helper's start (about 0.1 s on Linux and macOS, up to 1 s on
  Windows).
* Bad, because a C# toolchain joins the repository: contributors touching the helper need the .NET
  10 SDK, and `task ci` needs it too.
* Bad, because the helper targets `net8.0` six weeks before .NET 8's end of support (2026-11-10):
  the widest reach today (its own runtime doesn't matter to the target); move to `net10.0` later.
* Neutral: whatever can pose as the endpoint — a same-user process, or on a shared machine
  another local user (see "What the same-user check covers") — can feed the helper a crafted
  EventPipe stream; at worst it crashes or misleads the helper, which runs with the user's
  privilege. eyedbg bounds everything it reads from the helper and shows its strings made fit for a
  terminal.
* Bad, because on a shared machine another local user can pose as a target's diagnostics endpoint
  (unless TMPDIR is private; on Windows, except for the refused dsrouter pipe); see "What the
  same-user check covers".
* Neutral: a macOS app started through its own launcher with diagnostics off reports `NOT_DOTNET`
  (its coreclr module isn't visible there); the hint covers both causes.

### Confirmation

`internal/helper` tests run the runner against a fake helper (the test binary) in every failure
mode; the C# tests cover framing, the protocol, payload mapping, batching and the classification;
`internal/e2e` runs the real helper, published next to the built `eyedbg`, against real .NET
processes (and a session stopped at a breakpoint) on all six CI platforms; CI's `goreleaser` job
checks every archive carries the helper and runs `eyedbg dotnet ps` from the extracted Linux
archive.

## Pros and Cons of the Options

### Who runs it: the CLI, per command

* Good, because no helper lifecycle in the daemon, no cross-client state, and nothing new in the
  native API.
* Bad, because a future verb needing state across calls (a live trace session) would have to hold
  a process open for its duration — fine for one command, as `--watch` does.

The daemon-owned helper (DESIGN §2's first sketch) would share one warm process, at the cost of
supervising it, routing several clients' calls through it, and a daemon exposed to its failures.

### What to run: our own helper, not Microsoft's global tools

`dotnet-counters`/`-trace`/`-dump` print for humans, are separate per-platform downloads with
their own versions, and `dotnet-dump analyze` is a REPL; parsing their output would be a contract
we don't control.

### Distribution

`net10.0` excludes machines with only .NET 8 or 9. NativeAOT can't cross-compile between OSes (six
runner builds) and TraceEvent's AOT support is undocumented. Self-contained is ~70 MB per platform.
A `dotnet tool` needs a NuGet publishing key and installs global state.

### Build placement

A goreleaser `before` hook would run `dotnet publish` — package analyzers, source generators and
MSBuild assets — inside the job holding `contents: write` and `id-token: write`; hooks also can't
write into `dist/` (goreleaser empties it after them).

### Wire format

internal/api's framing is already bounded and tested; a second framing (`Content-Length`) or a
new transport (gRPC, named pipes) adds code on both sides for nothing stdio lacks.

### Counter source

The meter-based `System.Runtime` exists only from .NET 9, with lowercase, version-dependent payload
fields; EventCounters exist since .NET Core 3.0 with one stable payload. The wire's per-counter
`provider` leaves room for other providers later.

### Aggregation

In the helper it would be a second path for summaries beside watch, tested only by C# tests; in Go
it is one path and golden-locked.

### C# tests

MSTest 4.4.1's adapter pulls `Microsoft.Testing.Extensions.Telemetry` → Application Insights, and
the `MSTest` metapackage a code-coverage extension under non-OSI Microsoft terms; xUnit v3's default
flavour pulls the telemetry extension too. `mtp-off` has neither and runs as a plain executable.

## Addendum (P2-M7, 2026-09-25): dumps, heap and threads

`eyedbg dotnet dump` has a process's runtime write a dump; `eyedbg dotnet heap` and `eyedbg dotnet
threads` analyze one — taken first when given a process — with ClrMD in the helper. Three helper
methods join protocol 1 (additive: an older helper answers `UNKNOWN_METHOD`, which the CLI reports
as `HELPER_MISMATCH`): `dump`, `heap`, `threads` (wire in `helpers/dotnet/README.md`), and three
codes: `DUMP_UNSUPPORTED` (exit 2), `DUMP_RUNTIME_MISSING` (3), `DUMP_FAILED` (4).

* **ClrMD 4.1.745802, not 3.1.512801.** Measured on .NET 10 (macOS arm64, Linux x64): both read
  heap statistics and stacks, but 3.1 can't read .NET 9+ statics — a static field's address reads
  0 and no `StaticVar` roots are reported (static-variable support came after 3.1) — so `--gcroot`
  couldn't name a static root, and 3.1 lacks 2026's parser hardening for untrusted dumps
  (`DataTargetLimits`, bounds checks). 3.1 adds one package (+0.7 MB); 4.1 adds 18 (+3.7 MB):
  Azure.Identity, Azure.Core, System.ClientModel, MSAL, six Microsoft.Extensions.* and others, all
  MIT, and moves six versions up. The Azure/MSAL closure is reached only through ClrMD's symbol
  server, which the helper never constructs; `Azure.Core` types appear in `DataTarget`'s own
  signatures, so it must ship. Trimming it with `ExcludeAssets` was tried and rejected: it drops
  one assembly, the rest of the closure still ships, and excluding files one by one would break on
  a Dependabot bump along a path no test covers. Re-run the statics check (a static root named in
  `eyedbg dotnet heap --gcroot`) before a major bump.
* **`WriteDump` joins the permitted `DiagnosticsClient` calls** (with `GetPublishedProcesses` and
  `StartEventPipeSession[Async]`). The runtime suspends the process while it writes; the flags are
  always `None` — never logging or crash-report flags, which write into the target's own output.
  It works while a debugger holds the program at a breakpoint (R4; verified on macOS and Linux:
  the runtime writes the dump from a native thread), so dump, heap and threads accept a stopped
  session, unlike counters.
* **Two helper processes, no snapshots, no temp cores.** For a live target the CLI runs the helper
  twice: `dump` into eyedbg's directory, then `heap` or `threads` on that file — keeping one call
  per helper process, and ClrMD (a large parser that loads native code) out of the process that
  talks to the target. ClrMD's `CreateSnapshotAndAttach`/`AttachToProcess` are never used: every
  core file is one eyedbg asked the runtime for.
* **The private directory** (`internal/artifacts`): `<home>/dumps`, `<home>` = `$EYEDBG_HOME` or
  `~/.eyedbg` (the adapters' own resolution), created 0700; refused when it is a symlink or, on
  Unix, owned by another user; group and other access removed. Files are 0600 (createdump's mode,
  re-applied). Windows relies on the user profile's ACL, as the runtime directory does; an
  `EYEDBG_HOME` outside the profile is the user's choice. createdump truncates an existing file in
  place, follows a symlink and resolves a relative path against the target's working directory
  (measured), so the runtime only ever gets an absolute, fresh, random name in that directory
  (`<pid>-<UTC time>-<type>-<8 hex>.dmp`); the helper refuses a relative or existing path too.
  After it answers, the CLI checks a regular file is there (else `DUMP_FAILED`: a target in another
  filesystem namespace) and removes a failed dump's leftovers.
* **`--out`** never overwrites: checked early (nothing there, dangling symlinks included; the parent
  an existing directory — none is created), then placed by a hard link (fails on any existing name
  without following it) or, across filesystems, a copy into a file created with `O_EXCL`, 0600; the
  private copy is removed only once the placement succeeded, and a lost race or failed copy leaves
  it in place with its path in the hint.
* **Pruning.** Each dump, heap or threads command removes eyedbg's own dumps (its name pattern,
  regular files only — never symlinks, directories or other files) older than 7 days, then the
  oldest beyond the newest 10 — after a new dump is written, so it counts and at most 10 remain;
  the dump just taken or about to be analyzed is never removed. The cap of 10 goes
  beyond the roadmap's 7 days: a heap dump of a small program is about 350 MB on macOS.
* **Loading a dump safely.** The helper opens dumps with its own file locator that finds nothing: no
  symbol-server download, no shared `$TMPDIR/symbols` cache (on Linux a DAC planted there by another
  user would be loaded), no `_NT_SYMBOL_PATH`. It refuses a dump of another OS or architecture, one
  without a .NET runtime, or one ClrMD can't parse (`DUMP_UNSUPPORTED`). The DAC — native code the
  analysis loads — comes from (a) this machine's installed runtime of the dotnet running the helper,
  in the version directory named like the dump's runtime directory (a version name, so no path
  traversal), or (b) the dump's own recorded runtime directory, only for eyedbg's own dumps (in the
  private directory, taken from a process of the user's) and only when neither the file nor its
  directory is writable by its group or others (on macOS every local user shares the `staff`
  group). Either must be a regular file. On Windows and macOS ClrMD checks
  the DAC's file version against the dump's runtime before loading it (Windows also its signature,
  left on); on Linux, where ClrMD reports version 0.0, the helper compares the GNU build id of the
  candidate directory's `libcoreclr.so` with the dump's (a bounded ELF note reader). ClrMD loads the
  exact path given (read in its 4.1 source). No candidate: `DUMP_RUNTIME_MISSING`, naming the
  directories searched. A self-contained app's dump moved out of the private directory therefore
  needs its runtime installed to be analyzed. Rejected: trusting any recorded directory that isn't
  world-writable (another user's 0755 directory passes and would run code as you), ClrMD's default
  locator, a `--dac` flag.
* **What the analyses show — never values.** Types, counts, sizes, generations, addresses (hex
  strings: a uint64 exceeds JSON numbers in JavaScript), root kinds and static field names, thread
  ids and flags, the type of a thread's current exception, frame method signatures, module file
  names, IL offsets and source file:line (from the module's portable PDB, embedded or next to it
  with a matching id; only local absolute paths — no UNC or device paths, which on Windows would
  reach a server named by the dump — and non-empty `.dll`/`.exe`/`.pdb` files of at most 64 MiB,
  an open that blocks, as on a FIFO, given up after 2 s). Never field values,
  string contents, thread names or exception messages. Every string is cut at 400 characters and a
  result kept under 900 KiB (the protocol's 1 MiB line limit stays the backstop); the CLI's text
  makes every helper string fit for a terminal.
* **Tests never touch real data.** The CLI tests and the e2e harness set `EYEDBG_HOME` to a
  temporary directory (a guard test checks it); tests assert types and counts, never print dump
  contents.

Known limits: only locks with a sync block are listed (a contended `lock`/`Monitor`); an uncontended
one and .NET 9's `System.Threading.Lock` aren't, and which lock a waiting thread waits on isn't
recorded. A Windows heap or mini dump lacks the JIT's IL-to-native maps of user methods (measured:
ClrMD returns none), so frames there have no IL offset or source line; a full dump has them
(`threads --dump-type full`). The helper never guesses an offset without a map. .NET 8 and 9 targets are claimed by ClrMD 4.1 but only .NET 10 targets were run.

Dependencies added (shipped; all MIT, © Microsoft Corporation):

| Package | Version | Why |
|---|---|---|
| Microsoft.Diagnostics.Runtime (ClrMD) | 4.1.745802 | reads dumps: heap, roots, threads, stacks, sync blocks |
| Azure.Core, Azure.Identity, System.ClientModel | 1.53.0, 1.21.0, 1.10.0 | ClrMD dependencies (its symbol server's credentials; never used) |
| Microsoft.Identity.Client, Microsoft.Identity.Client.Extensions.Msal, Microsoft.IdentityModel.Abstractions | 4.83.1, 4.83.1, 8.14.0 | Azure.Identity dependencies |
| Microsoft.Extensions.{Configuration,Diagnostics,FileProviders,Hosting}.Abstractions, Microsoft.Extensions.Options, Microsoft.Extensions.Primitives | 10.0.3 | transitive |
| Microsoft.Bcl.AsyncInterfaces, System.Diagnostics.DiagnosticSource, System.Memory.Data | 10.0.3 | transitive |
| System.Security.Cryptography.ProtectedData | 4.5.0 | MSAL dependency (also `runtimes/win/lib/netstandard2.0/`) |

Moved up: Microsoft.Extensions.Logging.Abstractions and DependencyInjection.Abstractions 10.0.3,
System.Collections.Immutable 10.0.7, System.IO.Pipelines, System.Text.Encodings.Web and
System.Text.Json 10.0.3. The helper publishes 35 files, 8.9 MB.

## Addendum (P2-M8, 2026-09-25): CPU and GC traces

`eyedbg dotnet trace` records a short EventPipe trace of a running process into a `.nettrace` file
and summarizes it; `eyedbg dotnet trace FILE` summarizes an existing one (eyedbg's or
dotnet-trace's). Two helper methods join protocol 1 (additive; an older helper's `UNKNOWN_METHOD`
is `HELPER_MISMATCH`): `trace` and `traceSummary` (wire in `helpers/dotnet/README.md`), and one
code: `TRACE_UNSUPPORTED` (exit 2). No new package: TraceEvent 3.2.6 (M6) has `TraceLog`.

* **Two helper processes.** The CLI runs `trace` (collect into a file), then a second helper
  process runs `traceSummary` on that file: one call per process, the trace parser (fed by a
  same-user and possibly spoofed endpoint) out of the process holding the diagnostics connection,
  and FILE summaries for free. Rejected: one combined method; TraceLog's real-time session (no file
  for PerfView, and a second rundown session at start).
* **Profiles, minimal keywords.** `cpu`: `Microsoft-DotNETCore-SampleProfiler` (Informational) and
  `Microsoft-Windows-DotNETRuntime` Informational with GC | Loader | Jit (0x19), rundown requested;
  `gc`: the runtime provider Verbose with GC (0x1: collections and allocation ticks), no rundown.
  Both with a 256 MB EventPipe buffer (dotnet-trace's default, allocated as needed). Not
  dotnet-trace's default keywords: no Exception keyword (exception messages are program values),
  no contention, threading or interop payloads. A trace then holds method, module and type names,
  GC data and what the runtime records about the process itself — its command line and module
  paths — which is why it is kept private.
* **Collection.** The helper creates the file before starting the session, with `FileMode.CreateNew`
  (O_EXCL: an existing name or symlink fails; nothing followed or truncated), no sharing, and 0600
  off Windows (`UnixCreateMode`'s setter throws on Windows: measured). The session starts within
  5 s (`Diagnostics/EventPipeStart`, shared with counters). The stream is copied to the file on its
  own thread and always drained (M6's Windows rule); past 512 MiB, or once writing failed, it keeps
  reading and the session is stopped (`endReason: "size"`; a write failure is `HELPER_FAILED`).
  Stopping waits up to 30 s for the stream's end, which carries the rundown (the method names): a
  target that stopped answering mid-trace (a breakpoint, a suspend) gets its stream closed, the call
  fails with `DIAGNOSTICS_TIMEOUT`, and the CLI removes the file, which couldn't be read. A target
  that exits during the trace ends the stream itself, readable (`endReason: "exited"`); one killed
  leaves a cut file (`TRACE_UNSUPPORTED` at the summary; appending an end tag would parse it but
  with every frame unresolved, so it isn't done). stdin's end (Ctrl-C) stops the session within 3 s
  and the CLI discards the trace. The CLI checks a regular non-empty file arrived (re-applying
  0600), prunes after it, and moves it to `--out` only after the summary (the helper reads the
  private file, never the user's location). A stopped session is refused before any helper runs
  (`NOT_RUNNING`, as counters: EventPipe can't start there).
* **Summary.** Pass 1 reads the file with `EventPipeEventSource`: GC collections (per generation,
  background, induced, pause total and max, from TraceEvent's GC analysis), allocation ticks by
  type, the number of CPU samples. Pass 2, only with samples, converts it with
  `TraceLog.CreateFromEventPipeDataFile(path, scratch)` — always an explicit scratch path in
  eyedbg's traces directory (`<pid>-<UTC>-<profile|file>-<8 hex>.etlx`; the default,
  `<file>.etlx`, deletes and replaces whatever is next to the input; its `.new` sibling is covered
  too) — made 0600, deleted in the helper's `finally` and again by the CLI; a scratch file left by
  a killed summary is pruned after an hour (a summary's `--timeout` is at most that). Each sample's
  stack counts for its top method (exclusive) and once for every method in it (inclusive) when the
  thread was in managed code; any other sample (a wait, I/O, native or runtime code) counts for the
  managed method it left from (waiting). EventPipe samples every managed thread about a thousand
  times a second in total (measured: ~1000/s over 3 threads), not CPU time: ranking all samples
  together would put `Thread.Sleep` first, hence the separate lists. Method names are TraceEvent's
  without the IL keywords `class `/`value class `, parameter types as TraceEvent spells them
  (`int32`); frames without a name count as "(unresolved)" in their module.
* **Untrusted input.** Any `.nettrace` (a FILE, or a spoofed endpoint's stream) is parsed without
  symbol lookup: never `SymbolReader`, `TraceLogOptions.ShouldResolveSymbols`/`AlwaysResolveSymbols`,
  `LookupSymbolsForModule` or `GetSourceLine`, so no symbol server is contacted and no file a trace
  names is opened (read in TraceEvent 3.2.6's source; measured: the traced app's `.dll` and
  `.deps.json` replaced by FIFOs, the summary completes). A zero-length file (also what a FIFO or
  device reports) is refused before it is opened. A file the user can't read
  (`UnauthorizedAccessException`) is `TRACE_UNSUPPORTED` "can't read PATH"; `FormatException`,
  `EndOfStreamException`, `InvalidDataException` and `IOException` while reading are
  `TRACE_UNSUPPORTED` without the parser's message (it can quote the file); any other parser
  exception is `HELPER_FAILED` with its type only. `TRACE_UNSUPPORTED` means only a file that can't
  be read: a readable trace with no samples, collections or allocation ticks (a quiet process traced
  for gc) is an empty summary — the CLI says so in one line and exits 0 (M8-Q13, amending the plan's
  refusal). The deadline is checked every 4096 events; the TraceLog conversion itself can't be
  canceled, so a runaway one ends at the CLI's deadline (the helper is killed). Memory inside the
  helper isn't bounded (the serializer reads lengths from the file, so a crafted one can ask for
  more than its size): the short-lived helper can die (`HELPER_FAILED`) or be killed at the
  deadline; and the ETLX file (2.7–4.4× the trace) takes disk in the private directory until the
  summary ends — residuals for the phase-end security review.
* **Output.** Names and counts only: methods, modules, types, samples, GC counts and pauses,
  allocation estimates; never event payloads (the process information with the command line is
  never read). Names cut at 400 characters, a result under 900 KiB; the CLI makes every string fit
  for a terminal.

Measured while planning and building (tiny apps; macOS arm64 .NET 10.0.10, Linux x64 10.0.12,
Windows x64 10.0.12): start 22–110 ms, stop with rundown 25–99 ms; 3 s ≈ 0.5–5.7 MB for cpu, up to
25 MB for gc at an extreme allocation rate (its event rate follows the allocation rate); the ETLX
file is 2.7–4.4× the trace, converted in 0.1–0.4 s; a spinning method ranked first with 0 unresolved
frames (System.Private.CoreLib included) on all three OSes, without the native libraries excluded
from the package (the Windows conversion was checked before building). Unmeasured: the rundown's
time on a large app (100k+ methods; the 30 s stop bound), sampling overhead on the target, .NET 8
and 9 targets.
