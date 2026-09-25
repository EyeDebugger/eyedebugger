# helpers/dotnet

`eyedbg-dotnet-helper`: the C# side helper behind `eyedbg dotnet ps|counters|dump|heap|threads|trace`
(docs/adr/0016-a-csharp-side-helper-for-dotnet-diagnostics.md and its P2-M7 and P2-M8 addenda). eyedbg
starts it once per step and talks JSON-RPC 2.0 to it over stdio; it reaches the target's .NET
diagnostics endpoint with `Microsoft.Diagnostics.NETCore.Client`, parses EventPipe with
`TraceEvent` and reads dumps with ClrMD (`Microsoft.Diagnostics.Runtime`). It lists endpoints,
starts EventPipe sessions (counters, traces) and asks a runtime to write a dump; nothing else
changes a target.

## How eyedbg finds it

`$EYEDBG_DOTNET_HELPER` (an absolute path: a `.dll`, run with the dotnet host, or an executable),
else `helpers/dotnet/eyedbg-dotnet-helper.dll` next to `eyedbg` — where release archives put it and
`task build` builds it — or next to the file a symlinked `eyedbg` points to. It runs on any
installed .NET 8 or later (`net8.0`, `RollForward=LatestMajor`).

## Protocol 1

One JSON document per `\n`-ended line, under 1 MiB, both ways (internal/api's framing). Errors
are `{"code": -32000, "message", "data": {"code", "message", "hint"}}` with eyedbg's stable codes.

- `hello {protocol}` must come first → `{protocol: 1, version, runtime}`.
- `processes {}` → `{processes: [{pid}]}`: processes publishing a diagnostics endpoint, without the
  helper itself (eyedbg adds names and drops other users' processes).
- `counters {pid, intervalSec (1–60), durationMs (0: until stdin closes)}` → `counters.sample`
  notifications `{elapsedMs, counters: [{provider, name, displayName, unit, kind: "gauge"|"sum",
  value}]}`, one per interval — sent when the next interval's values begin (TraceEvent holds a
  burst's events until the next arrives), so about one interval late; a canceled call doesn't send
  its last one — then `{samples, endReason: "duration"|"exited"}`. Errors:
  `INVALID_REQUEST` (bad params, no such process, a `dotnet-diagnostic-dsrouter-PID` pipe on
  Windows — ADR 0016), `NOT_DOTNET`, `DIAGNOSTICS_DISABLED`,
  `DIAGNOSTICS_TIMEOUT` (no session within 5 s, or no sample at all), `HELPER_FAILED`.
- `dump {pid, type: "heap"|"mini"|"triage"|"full", path, timeoutMs (1000–3600000)}` →
  `{bytes, elapsedMs}`: the target's runtime writes a dump at `path` (absolute, nothing there yet —
  eyedbg passes a fresh name in its private directory; the runtime truncates and follows symlinks),
  suspending the process meanwhile; never with logging or crash-report flags. Errors:
  `INVALID_REQUEST`, `NOT_DOTNET`, `DIAGNOSTICS_DISABLED`, `DIAGNOSTICS_TIMEOUT` (not written within
  `timeoutMs`), `DUMP_FAILED` (createdump failed, or no file arrived).
- `heap {path, trustRecordedRuntime, top (1–1000), typeFilter?, gcroot?: {type? | address?},
  paths (1–20), timeoutMs}` → `{runtime: {version, gc, heaps, platform, arch}, objects, bytes,
  generations: [{name: gen0|gen1|gen2|loh|poh|frozen|unknown, objects, bytes}], typeCount, types:
  [{name, count, bytes}], typesOmitted, gcroot?: {target: {type?, address?, instances?}, paths:
  [{root: {kind, name?, thread?, frame?}, chain: [{address, type}], chainOmitted?}], pathsFound,
  complete}}`. Addresses are hex strings (`"0x1a2b"`). Errors: `INVALID_REQUEST` (bad params, an
  unknown or ambiguous `--gcroot` type, no object at an address), `DUMP_UNSUPPORTED` (not a dump
  ClrMD reads, another OS or architecture, no .NET runtime, no heap: a mini or triage dump),
  `DUMP_RUNTIME_MISSING`, `DIAGNOSTICS_TIMEOUT` (the heap walk ran out of time; a root-path search
  cut by it returns `complete: false` instead).
- `threads {path, trustRecordedRuntime, frames (1–64), timeoutMs}` → `{runtime, threads: [{managedId,
  osId, alive, background, threadpool, finalizer, gc, exception? (a type name), frames: [{kind:
  "managed"|"runtime", method, module?, ilOffset?, file?, line?}], framesOmitted}], threadsOmitted,
  locks: [{object, type, owner? (managedId), waiting}], locksAvailable}`. Same errors as `heap`.
  `ilOffset` (and so `file`/`line`) is absent where the dump has no IL map for the method — every
  user method in a Windows heap or mini dump; a full dump has them.
- `trace {pid, profile: "cpu"|"gc", durationMs (1000–300000), path}` → `{bytes, elapsedMs, endReason:
  "duration"|"exited"|"size"}`: records an EventPipe trace into `path` (absolute, nothing there yet;
  created `O_EXCL`, 0600 off Windows). cpu: `Microsoft-DotNETCore-SampleProfiler` and the runtime
  provider's GC, Loader and Jit keywords, with the rundown at stop; gc: the runtime provider Verbose
  with the GC keyword (collections and allocation ticks). Buffer 256 MB; the stream is drained on
  its own thread; the trace ends early past 512 MiB (`size`), when the process exits (`exited`), or
  on stdin's end (stopped within 3 s, no answer). Stopping waits up to 30 s for the rundown. Errors:
  `INVALID_REQUEST`, `NOT_DOTNET`, `DIAGNOSTICS_DISABLED`, `DIAGNOSTICS_TIMEOUT` (no session within
  5 s, or the target stopped answering mid-trace: the file is incomplete), `HELPER_FAILED` (writing
  the file failed).
- `traceSummary {path, scratch, top (1–1000), timeoutMs (1000–3600000)}` → `{durationMs, eventsLost,
  cpu?: {samples, managed, other, threads, unresolvedFrames, exclusive: [{method, module?, samples}],
  exclusiveOmitted, inclusive, inclusiveOmitted, waiting, waitingOmitted}, gc?: {collections, gen0,
  gen1, gen2, background, induced, pauseTotalMs, pauseMaxMs}, allocations?: {ticks, bytes,
  typeCount, types: [{name, ticks, bytes}], typesOmitted}}`: summarizes a `.nettrace`. `scratch`
  (absolute; nothing there nor at `scratch.new`) is where TraceLog's ETLX file goes to resolve the
  samples' stacks, deleted afterwards. Samples in managed code count for their top method
  (exclusive) and once for each method in the stack (inclusive); the others for the managed method
  they left from (waiting). `cpu` is there with samples, `gc` with a collection, `allocations` with
  an allocation tick; `top` bounds each list. No symbol lookup, and no file a trace names is opened.
  A readable trace with none of them answers `{durationMs, eventsLost}` only. Errors:
  `INVALID_REQUEST`, `TRACE_UNSUPPORTED` (empty, unreadable by the user, not a `.nettrace` the parser
  reads, or cut off — the parser's message is never passed on), `DIAGNOSTICS_TIMEOUT`,
  `HELPER_FAILED` (another parser failure, by type only).
- Dumps are opened with a file locator that finds nothing (no symbol servers, no
  `$TMPDIR/symbols`). The DAC comes from this dotnet's installed runtime of the dump's version, or,
  with `trustRecordedRuntime` (eyedbg's own dumps only), from the dump's recorded runtime directory
  when neither it nor the file is writable by its group or others; it must match the dump's runtime (file
  version on Windows and macOS, `libcoreclr.so`'s GNU build id on Linux) — else
  `DUMP_RUNTIME_MISSING`. Source lines come from the module's PDB only at a local absolute path
  (never UNC or device paths) and from non-empty files. Results name types, methods, static fields
  and source lines, never values;
  names are cut at 400 characters and a result kept under 900 KiB.
- One call per process. While it runs, the helper reads stdin only to see it close: that cancels
  the call (the EventPipe session is stopped within 3 s) and the helper exits 0 without answering.
- stdout carries the protocol only; anything else goes to stderr, which eyedbg shows only when the
  helper fails without answering.

## Layout

- `src/EyeDbg.DotnetHelper/`: `Program.cs`; `Rpc/` (framing, messages, the server); `Methods/`
  (`processes`, `counters`, `dump`, `heap`, `threads`, `trace`, `traceSummary`); `Counters/` (the
  counters session, payload mapping, batching); `Diagnostics/` (finding the target, its endpoint,
  telling `NOT_DOTNET` from `DIAGNOSTICS_DISABLED`, the bounded EventPipe start); `Tracing/` (trace
  profiles, collection into a file, the summary: stack and GC aggregation, method names); `Dumps/`
  (WriteDump); `Analysis/` (loading dumps and choosing the DAC, the ELF build id, heap statistics,
  root paths, threads and locks, source lines, result bounds).
- `tests/EyeDbg.DotnetHelper.Tests/`: xUnit v3 (`xunit.v3.mtp-off`), run as a program.
- `global.json` (SDK 10.0.x), `nuget.config` (nuget.org only), `Directory.Build.props` (analyzers,
  warnings as errors, lock files, NuGet audit), `Directory.Packages.props` (versions),
  `.editorconfig` (C# style, the SPDX header), `EyeDbg.DotnetHelper.slnx`.
- `THIRD-PARTY-NOTICES.txt`: the licences of the assemblies shipped with the helper; published next
  to it. Update it when that set changes (CI fails otherwise).

## Build and test

In this directory (the tasks do that for you):

| Task | Command |
|---|---|
| `task helper:build` | `dotnet publish src/EyeDbg.DotnetHelper/EyeDbg.DotnetHelper.csproj -c Release -p:RestoreLockedMode=true -o ../../bin/helpers/dotnet` |
| `task helper:test` | `dotnet run --project tests/EyeDbg.DotnetHelper.Tests -c Release` |
| `task helper:lint` | `dotnet format --verify-no-changes`, `dotnet build -c Release --no-restore` |
| `task helper:vuln` | `dotnet restore --locked-mode --force -p:EyeDbgNuGetAudit=true` |
| `task helper:dist` | publish into `artifacts/dist`, which goreleaser packs into every archive |

End-to-end tests against real processes are in `internal/e2e` (`EYEDBG_E2E=1`, names `DotnetHelper`,
`DotnetDump` and `DotnetTrace`).
