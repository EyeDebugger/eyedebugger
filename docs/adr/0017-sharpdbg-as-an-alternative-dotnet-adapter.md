---
status: proposed
date: 2026-09-25
decision-makers: Ijat (@ijat)
---

# SharpDbg as an alternative .NET adapter

## Context and Problem Statement

netcoredbg (Samsung, MIT) is eyedbg's .NET adapter. It has no build for Intel Macs (osx-x64) or
Windows on Arm (win-arm64), so .NET debugging was unavailable there (DESIGN §8, §14), and it can't
evaluate lambdas or show `[DebuggerDisplay]` values. SharpDbg (MattParkerDev, MIT, C#) is a younger
.NET debug adapter speaking DAP, built on Roslyn's expression compiler and ICorDebug, shipped as the
`SharpDbg.Cli` NuGet package (a framework-dependent `dotnet tool` for .NET 10, with dbgshim for every
platform). DESIGN §8 named it as the alternative adapter, selectable with `--adapter sharpdbg`. This
ADR decides how eyedbg gets it, runs it, chooses it, and what it does about its gaps. It extends
ADR 0011's runtimes (that ADR is accepted and not edited).

## Decision Drivers

* Rule 1 (ADR 0005): never vsdbg. SharpDbg contains none (no `vsdbg` string in any bundled DLL, no
  dependency on it in its nuspec or deps.json).
* Same trust model as every other adapter (ADR 0011): a pinned download, SHA-256 and size checked
  before extraction, nothing run from a project.
* Nothing eyedbg doesn't already need: SharpDbg runs on the user's `dotnet` host, which the .NET
  driver already requires to build.
* The session core stays language-agnostic; SharpDbg's quirks are Go-driver knowledge.
* A licence we can defend. SharpDbg is MIT, but it bundles `Microsoft.VisualStudio.Shared.
  VSCodeDebugProtocol` 18.0.10427.1 under the **Microsoft Software License Terms** (not open source;
  below).

## Considered Options

Acquisition:

* **The pinned `SharpDbg.Cli` nupkg from nuget.org**, extracted by the existing installer (it is a
  zip), run as `dotnet SharpDbg.Cli.dll`.
* `dotnet tool install -g SharpDbg.Cli`: needs the SDK's tool machinery, writes outside eyedbg's
  data directory, and floats unless pinned by hand.
* Building SharpDbg from source: a .NET build of someone else's project at install time.

Selection:

* **An optional `session.AdapterSelector` interface** (`WithAdapter(name) (Driver, error)`) on the
  driver, chosen by a new `StartParams.Adapter` before anything is built: DESIGN §7's `AdapterFor`,
  as built.
* An `Adapter` field on `LaunchSpec` and `AttachSpec`, threaded through three driver entry points
  (and copied again for a test run's attach).
* A manifest-level selector (a manifest listing the languages it can serve): schema growth for one
  user, and SharpDbg's quirks are Go-driver knowledge anyway.

## Decision Outcome

Chosen: the pinned nupkg and the selector interface.

* **Manifest.** `internal/adapters/manifests/sharpdbg.json`: SharpDbg.Cli 0.1.17 from
  `https://api.nuget.org/v3-flatcontainer/sharpdbg.cli/0.1.17/sharpdbg.cli.0.1.17.nupkg`
  (7,951,923 bytes, SHA-256 `549fe48f…961309`), root `tools/net10.0/any`, one `"*"` download. Schema
  1 gains, additively, `adapter.runtime: "dotnet"`: `adapter.entry` is a slash path to the `.dll`
  relative to the install directory, `adapter.env` may name a variable holding a `.dll`
  (`EYEDBG_SHARPDBG`), `path` and `versionArgs` are refused, and a required
  `"dotnet": {"minRuntime": "X.Y"}` block names the oldest `Microsoft.NETCore.App` it runs on. Only
  the .NET driver runs such an adapter, so validation refuses runtime `dotnet` with a language other
  than a built-in one (Q4; a generic-driver dotnet runtime is a follow-up).
* **Running it.** `adapters.ResolveDotnet`: the `.dll` (the environment variable, else the installed
  copy, else not installed), the dotnet host (`FindDotnetHost`, the lookup that used to be
  `drivers/dotnet.FindHost`: `DOTNET_HOST_PATH`, `PATH`, `DOTNET_ROOT`, `~/.dotnet`), and
  `dotnet --list-runtimes` (fixed argv, 10 s, in the adapter's own directory; no project code runs)
  listing a `Microsoft.NETCore.App` at least `minRuntime` (a pre-release counts: the host rolls
  forward to one when no release fits). The session runs `dotnet SharpDbg.Cli.dll
  --interpreter=vscode`.
* **Choosing it.** `start`, `attach` and `test` take `--adapter NAME`; the CLI fills it from
  `EYEDBG_DOTNET_ADAPTER` for dotnet when the flag is absent (read per request: the daemon's own
  environment goes stale). `Manager.Start` binds the driver through `WithAdapter` before `Prepare`,
  `PrepareAttach` or `TestCommand`; a driver without a selector refuses `--adapter`
  (`INVALID_REQUEST`). The .NET driver knows the manifest serving dotnet (netcoredbg) and the one
  named `sharpdbg`. `SessionInfo.adapter` names each session's adapter (`sessions`' ADAPTER column,
  `status`' `(dotnet, sharpdbg)`), for every language.
* **Default.** netcoredbg. Where netcoredbg's manifest has no download for the platform (today
  darwin/amd64 and windows/arm64), it isn't otherwise found (`EYEDBG_NETCOREDBG`, installed, PATH)
  and SharpDbg is installed, sessions use SharpDbg without `--adapter` — unless CI's SharpDbg e2e
  fails there (`dotnet.SharpDbgWithheld`: windows/arm64, see Addendum). Nothing is ever downloaded
  unasked: SharpDbg is used only after the user's own `adapters install sharpdbg` or
  `EYEDBG_SHARPDBG`.
* **Its gaps, handled in the session through driver settings** (`Launch.PauseUnsupported`,
  `Launch.SetByEval`), so the session stays language-agnostic:
  * **pause is refused** (`UNSUPPORTED_BY_ADAPTER`, before the lease or an exec event, with a hint:
    set a breakpoint, or `eyedbg dotnet threads`). SharpDbg 0.1.17 answers pause and does stop the
    program, but sends no `stopped` event (its `Pause()` calls `ICorDebugProcess.Stop` without
    reporting it), so the session would think it still runs. Synthesizing a stop after the
    response was rejected: timing logic and an adapter-specific inference in the session.
  * **set evaluates the assignment.** SharpDbg has neither `setVariable` nor `setExpression`, but
    its evaluator runs assignments; with `SetByEval`, `set VAR VALUE` evaluates `VAR = VALUE` in the
    `repl` context, under the same lease, exec event and stale-locals marking. VAR must be a plain
    member path (identifiers, `[N]` and `["key"]` indexes), so the path itself can't call anything;
    VALUE is an expression, as with `setExpression`. A property's setter runs; a `readonly` field is
    the adapter's compile error.
  * Logpoints stop in SharpDbg instead of logging, and hit conditions are native; the session already
    emulates both for every adapter, so neither reaches it.
* **Licence (decided by the user: opt-in only).** eyedbg never redistributes SharpDbg: it is not in
  release archives or the VSIX, and is fetched from nuget.org only by `eyedbg adapters install
  sharpdbg`. The bundled Microsoft library's terms (read in full, `go.microsoft.com/fwlink/
  ?linkid=832965`): one user may use it to develop and test their applications — which is what
  debugging is; it is "Distributable Code" whose conditions (end users agreeing to protective
  terms, indemnifying Microsoft) bind the *distributor*, SharpDbg's publisher, not eyedbg or its
  user; reverse engineering and hosting it for others are forbidden; and a DATA clause says the
  software may collect information and send it to Microsoft. Unlike vsdbg's (ADR 0005), they don't
  restrict use to Microsoft's IDEs. A string scan of that DLL finds no telemetry endpoint, only DAP
  field names; no network-level check was made. It is disclosed in the manifest (`license`,
  description), `eyedbg help adapters install`, DESIGN §8 and here. It is the first non-OSS component
  eyedbg fetches.

### Consequences

* Good, because .NET debugging works on Intel Macs (and on Windows on Arm with `--adapter
  sharpdbg`, unverified: see Addendum), and
  everywhere a user wants lambdas, LINQ and `[DebuggerDisplay]` in eval.
* Good, because the adapter choice is one optional interface: generic-driver languages are untouched,
  and a future second adapter for another language uses the same `--adapter`.
* Bad, because SharpDbg is 0.1.x with one maintainer: pause and native logpoints are broken, its
  main thread is "<No Name>", breakpoints bind by file *name* (upstream #57), `innerException` is
  always empty (#50), and `stepIn` enters `[DebuggerStepThrough]`/`[DebuggerHidden]` code (#52).
* Bad, because it needs the .NET 10 runtime on the user's `dotnet` (netcoredbg needs none), and
  `EYEDBG_E2E=1` now needs SharpDbg installed (narrow it with `EYEDBG_E2E_DOTNET_ADAPTERS`).
* Neutral, because the parity below is measured by the same e2e tests for both adapters, not
  asserted.

### Parity (P2-M10, SharpDbg 0.1.17 vs netcoredbg 3.2.0)

Measured by `drivers/dotnet`'s e2e (every test runs per adapter), `internal/e2e`'s CLI and facade
scripts (`dotnet` = the platform's default adapter, `sharpdbg` with `--adapter`), and a real-binary
smoke; macOS arm64, Linux x64 and Windows x64 before merging, Linux arm64, Intel macOS and Windows
on Arm by CI.

| Feature | netcoredbg | SharpDbg | eyedbg |
|---|---|---|---|
| launch (`dotnet app.dll`), env, cwd, stop-on-entry | ok | ok | same launch body |
| line and anchor breakpoints | ok | ok (verified by a later `breakpoint` event) | — |
| `--if` conditions | native | native | — |
| `--hit`, `--log` | emulated | emulated (native logpoints stop instead) | the session emulates both |
| `func:` breakpoints | ok | ok | — |
| exceptions `all` / `uncaught`, exception info | ok | ok (`innerException` empty) | same filters |
| next / step-in / step-out, run-until | ok | ok | — |
| pause | ok after a first stop (see below) | stops without a `stopped` event | refused: `UNSUPPORTED_BY_ADAPTER` |
| eval: locals, operators, method calls | ok | ok (a string-literal receiver fails) | side-effect guard for both |
| eval: lambdas / LINQ | "Parameter not implemented" | ok, with System.Linq loaded | needs `--allow-side-effects` |
| `[DebuggerDisplay]` | `{Point}` | `Point(3, 4)`, `Count = 3` | — |
| `set` | setExpression | assignment by evaluate | `SetByEval` |
| attach, detach | ok | ok | same attach body |
| `test` (VSTest host attach) | ok | ok | same test command |
| two clients, lease, event log, DAP facade | ok | ok | adapter-independent |
| program exit code (start, attach, launched test run) | ok on Linux/Windows; always 0 on macOS | 0 when it misses the real one (a race), always 0 on attach | none reported under SharpDbg, or netcoredbg on macOS (`ExitCodeUnknown`) |

Found while measuring, not SharpDbg's: under netcoredbg, `eyedbg pause` before the program's first
stop fails (`ADAPTER_ERROR`, 0x80070057): the session sends pause's thread id 0 (none known yet),
netcoredbg requires a real one, and it lists no threads before a first stop. A follow-up.

### Confirmation

`internal/adapters` tests (schema, `ResolveDotnet`, the pinned manifest, a nupkg-shaped install),
`internal/session` tests (selector, pause refusal, set by evaluate against the fake adapter),
`drivers/dotnet` choice tests, and the real-adapter e2e above on every CI platform: on macos-15-intel
and windows-11-arm the .NET debugger e2e runs with SharpDbg alone (the default there). A platform
where it fails gets a `SharpDbgWithheld` entry with the failure, and its SharpDbg e2e skips naming it.

## Pros and Cons of the Options

### The pinned nupkg

* Good, because the existing installer handles it (https, size cap, SHA-256, zip extraction, one
  root kept), and a single `"*"` download serves every platform.
* Bad, because a SharpDbg update is a manifest bump like any adapter's.

### `dotnet tool install`

* Good, because it is how SharpDbg documents installation.
* Bad, because it floats, writes to the user's global tool directory, and can't be size- or
  digest-checked by eyedbg.

### Building from source

* Bad, because it runs a third-party build at install time.

### A selector interface

* Good, because the choice happens once, before any build, and everything downstream is unchanged.

### A spec field

* Bad, because it threads through three signatures and a test run's attach must copy it again.

## More Information

Follow-ups: report SharpDbg's pause (no `stopped` event) and native logpoints upstream (for the user
to file); lift the pause refusal in a version bump with a test once fixed; a dotnet runtime for
generic-driver languages (F#-script-like manifests); `eyedbg pause` under netcoredbg before a first
stop (above). Related: ADR 0005 (vsdbg), ADR 0010 (exception filters, emulated hits and logpoints),
ADR 0011 (manifests and their trust model), ADR 0016 (the .NET side helper, whose `eyedbg dotnet
threads` is the pause hint).

## Addendum (2026-09-26): SharpDbg withheld on windows/arm64

CI's full-matrix e2e on windows-11-arm failed with SharpDbg in two of three runs after M10
(runs 36161519531, first attempt, and 36210502382): `start` missed the first breakpoint —
`TestCLI/dotnet` and `TestCLI/sharpdbg` saw the program run to exit, `TestPause/sharpdbg` saw it
still running instead of stopped at the entry. macos-15-intel passed every run. `SharpDbgWithheld` now names windows/arm64: sessions there
don't default to an installed SharpDbg (the missing-netcoredbg error says to install SharpDbg and
start with `--adapter sharpdbg`, unverified), and its SharpDbg e2e skips naming the reason. Lift the
entry once a SharpDbg release passes there repeatedly.

## Addendum (2026-09-26): SharpDbg's exit codes untrusted

CI's full-matrix e2e saw launched test apps that exited 1 or 2 reported as exiting 0 under SharpDbg,
intermittently (run 36248189223: macos-15-intel and ubuntu-24.04-arm; run 36237638520's rerun:
macos-15-intel). SharpDbg 0.1.17 (commit 293d77f) sends `exited` from ICorDebug's ExitProcess
callback with `Process.HasExited ? ExitCode : null`, and `null` as `exitCode: 0`: when the callback
runs before .NET has seen the process exit, the real code is lost; attach is always `null`.
`ExitCodeUnknown` now names SharpDbg on every platform, so its sessions end without an exit code
(the end reason says why) instead of a possibly wrong 0; a VSTest run keeps `dotnet test`'s. Lift
it once a SharpDbg release waits for the real exit code (re-check on every manifest bump).
