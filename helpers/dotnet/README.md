# helpers/dotnet

`eyedbg-dotnet-helper`: the C# side helper behind `eyedbg dotnet ps` and `eyedbg dotnet counters`
(docs/adr/0016-a-csharp-side-helper-for-dotnet-diagnostics.md). eyedbg starts it once per command
and talks JSON-RPC 2.0 to it over stdio; it reaches the target's .NET diagnostics endpoint with
`Microsoft.Diagnostics.NETCore.Client` and parses EventPipe with `TraceEvent`. It only lists
endpoints and starts EventPipe sessions: read-only.

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
- One call per process. While it runs, the helper reads stdin only to see it close: that cancels
  the call (the EventPipe session is stopped within 3 s) and the helper exits 0 without answering.
- stdout carries the protocol only; anything else goes to stderr, which eyedbg shows only when the
  helper fails without answering.

## Layout

- `src/EyeDbg.DotnetHelper/`: `Program.cs`; `Rpc/` (framing, messages, the server); `Methods/`
  (`processes`, `counters`); `Counters/` (the EventPipe session, payload mapping, batching);
  `Diagnostics/` (finding the target, telling `NOT_DOTNET` from `DIAGNOSTICS_DISABLED`).
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

End-to-end tests against real processes are in `internal/e2e` (`EYEDBG_E2E=1`, name `DotnetHelper`).
