---
status: accepted
date: 2026-09-24
decision-makers: Ijat (@ijat)
---

# Hit counts and logpoints, exception stops, eval side effects, test runs and anchors

## Context and Problem Statement

Milestone 5 ("Breadth", docs/DESIGN.md §13) adds hit-count breakpoints and logpoints, exception
stops, `eval --allow-side-effects`, `set`, `attach`/`detach`, `test` and content anchors on top of
milestone 4's shared-session model (ADR 0009). netcoredbg, the only adapter, lacks several of the
DAP features these map to, and some of them change how clients share a session. Which of them
does the daemon build itself, how do per-client settings merge, how can an eval be kept from
changing the program, how does a test run get a debugger attached, and what happens to an anchor
when its file changes?

## Decision Drivers

* netcoredbg 3.2.0 declares neither `supportsHitConditionalBreakpoints` nor `supportsLogPoints`,
  and its `setBreakpoints` reads only `line` and `condition` (`src/protocols/vscodeprotocol.cpp`).
* netcoredbg's `evaluate` ignores `context` and always evaluates with function evaluation on: it
  can't evaluate without running code. Method calls are supported; assignment syntax is not.
* netcoredbg's exception filters are `all` (every first-chance throw) and `user-unhandled` (needs
  Just My Code); an unhandled exception always stops, whatever the filters.
* netcoredbg attaches at `configurationDone`, so an attach failure is that request's error, and it
  sends no `process` event for an attach.
* CoreCLR's debugger on Unix falls back to its pipe transport when it can't read the target's
  memory directly (dotnet/runtime `src/coreclr/debug/di/shimremotedatatarget.cpp`): managed attach
  needs no ptrace or `task_for_pid`.
* With `VSTEST_HOST_DEBUG=1`, vstest.console prints `Process Id: N, Name: dotnet` (not localized)
  and the test host polls `Debugger.IsAttached` every second; `VSTEST_DEBUG_NOBP=1` stops it from
  breaking into itself (microsoft/vstest `ProxyOperationManager.cs`, `DebuggerBreakpoint.cs`).
  With `--tl:off` the line reaches `dotnet test`'s stdout (checked on Linux, .NET 10 SDK).
* A running program was built from the source as it was; netcoredbg maps lines through that
  build's symbols.
* ADR 0009's rules hold: execution changes are serialized and leased; clients own their own
  settings; no lease is needed to change your own breakpoints.

## Considered Options

Hit counts and logpoints:

* Emulated by the daemon for every adapter: a plain breakpoint, and a filter at each stop there
  (chosen).
* Passed to the adapter when it declares them, emulated otherwise.
* `UNSUPPORTED_BY_ADAPTER` on netcoredbg.

Exception stops:

* Per client, merged by union, no lease (chosen).
* Session-wide, under the lease.
* Session-wide, last writer wins.

Eval side effects:

* A driver-supplied syntactic check refuses what visibly changes the program without
  `--allow-side-effects`; with it, the eval is a leased, logged execution request (chosen).
* Only the flag (DAP context `watch` vs `repl`), trusting the adapter.
* Refuse every eval without the flag.

Test runs:

* VSTest's host debugging: run `dotnet test` with `VSTEST_HOST_DEBUG`, read the host's pid from its
  output, attach (chosen).
* Microsoft.Testing.Platform's `TESTINGPLATFORM_WAIT_ATTACH_DEBUGGER`.
* Launch `dotnet test` itself under the adapter.
* Find the host among the runner's descendants.
* Launch the project's own self-hosting test app directly under the adapter, like `start`, instead
  of attaching to a separate host (chosen for Microsoft.Testing.Platform, xUnit v3 and TUnit
  projects; added 2026-09-26).

Anchors when the file changes:

* Resolve once, when added; note breakpoints in files changed since the session started (chosen).
* Re-resolve on every change of the file's modification time.
* Fuzzy matching at add time.

## Decision Outcome

**Hit counts and logpoints are emulated.** The adapter gets a plain breakpoint (line and
condition, merged per line as in ADR 0009). While any breakpoint has `--hit` or `--log`, a stop
with reason `breakpoint` is not applied at once: a filter, on its own goroutine under the
session's execution lock, fetches frame 0, finds the breakpoints at that location, evaluates each
one's own condition where it differs from the line's merged one, counts hits, renders log messages
(output category `logpoint`) and either publishes the stop or continues. Until it publishes, the
session still looks running; a stop or resume that arrives meanwhile, or the session ending,
drops it. Any adapter error publishes the stop. A step that ends at such a line is published (the
adapter's step is over). `--hit` is `N` (only the Nth), `>=N` or `%N`; hits count stops where the
breakpoint's own condition held; re-adding resets the count.

**Exception stops are per client.** Each client's mode is `none`, `uncaught` or `all`; the adapter
gets the union of their filters (the driver maps modes to filter ids; an adapter without one is
`UNSUPPORTED_BY_ADAPTER`, naming its filters); no lease; `--force` sets one mode for every client.
Changes are `exceptions` events. At an exception stop the snapshot carries the adapter's
`exceptionInfo`, cut to bounds.

**Eval is guarded by the driver, and a side-effecting eval is an execution request.** Without
`--allow-side-effects` a driver check refuses expressions that visibly change the program — for
C#: a call, `new`, an assignment, `++`/`--`, an interpolated string — with `SIDE_EFFECTS`. With the
flag, the eval runs in context `repl`, takes the lease like `continue` and is an `exec` event
(action `eval`). `set` is the same kind of request (action `set`, the variable's name only).
Getters, indexers and operators are invisible to the check; the help says so.

**`test` uses VSTest's host debugging.** The daemon runs `dotnet test -c Debug --tl:off` with
`VSTEST_HOST_DEBUG=1 VSTEST_DEBUG_NOBP=1` in its own process group, pumps its output into the
session, attaches to the first `Process Id: N` it prints, and ends the session with `dotnet
test`'s exit code; `stop` kills the group. The session exists (state starting) while it builds.
Microsoft.Testing.Platform projects are refused with `NO_TEST_HOST` and a hint to debug the test
app with `start`. (Amended 2026-09-24: xUnit v3 projects are refused the same way. Their VSTest
adapter runs the tests in a child process of the test host, so a run passed without stopping.)
(Amended 2026-09-26: superseded for both, and extended to TUnit. `test` now detects a
Microsoft.Testing.Platform, xUnit v3 or TUnit project the same way before the build and instead
builds it and launches its self-hosting test app directly under the adapter — no runner, no
attach, like `start` — with FILTER mapped to the app's own filter option and `--` args passed to
its argv verbatim; a VSTest project's host-attach flow above is unchanged. Where the driver marks
an adapter's exit codes untrusted (netcoredbg on macOS reports every one as 0;
`session.Launch.ExitCodeUnknown`, D8a), the launched session ends without one instead of naming a
wrong one, for `start`/`attach` sessions too.)

**Anchors resolve once.** `FILE@"TEXT"` names the one line holding TEXT (whitespace runs as one
space, case-sensitive); several lines are `ANCHOR_AMBIGUOUS`, none `ANCHOR_NOT_FOUND` with up to
three closest lines (token longest-common-subsequence score of at least 0.5). A breakpoint in a
file modified after the session started gets a note saying the line may not match the running
code; adding it again re-resolves it.

Attach checks the same-user rule itself first (`internal/proc`), shows the process as `pid N
(name)`, and maps adapter failures to `ATTACH_FAILED` with the driver's hint (the real .NET failure
modes, not ptrace). `stop` and `detach` of an attached session detach; nothing eyedbg attached to
is killed.

### Consequences

* Good, because netcoredbg, the first-class adapter, gets hit counts and logpoints, with one
  meaning of `--hit` for every adapter.
* Good, because a client's exception choice can't be undone by another client without `--force`,
  and phase 2's exception checkboxes need no lease.
* Good, because an agent can't change the program through `eval` by accident, and when it does on
  purpose the other clients see it in the lease and the event log.
* Good, because the test flow needs no process-tree scanning and was validated end to end on
  Linux (the host proceeds after the attach).
* Good, because an anchor never silently moves to a line of code the program isn't running.
* Bad, because each emulated hit pauses the program and costs a few round trips; a hot loop with a
  logpoint is slow.
* Bad, because the side-effect check is a best-effort text scan: `x.ToString()` needs the flag, a
  getter with side effects doesn't, and `(f)(1)` passes as a cast.
* Bad, because only the first test host of a multi-targeting project is debugged (a warning says
  so; `--framework` picks one). (Amended 2026-09-26: Microsoft.Testing.Platform, xUnit v3 and
  TUnit projects no longer need `start` separately — see the amendment above; multi-targeting
  still needs `--framework` on the launch path too.)
* Bad, because the filter attributes a stop by location only: an emulated breakpoint on a
  function's first line also decides that function's `func:` breakpoint stops.
* Bad, because while any breakpoint is emulated, a line two clients share with different
  conditions stops only when one of the conditions holds, unlike ADR 0009's "stops
  unconditionally" note.

### Confirmation

Fake-adapter tests in `internal/session` (the filter's decisions, hit counts, logpoints, steps,
run-until, stale stops; exception merging; eval and set leases; attach, detach and test runs with
a fake runner) and the real netcoredbg end-to-end tests in `drivers/dotnet` (`TestBreadthLoop`,
`TestBreadthHitsAndLogpoints`, `TestBreadthExceptions`, `TestAttach`, `TestDebugTests`).

## Pros and Cons of the Options

### Passing hit counts and logpoints to adapters that declare them

* Good, because it costs nothing at run time.
* Bad, because it can't be tested against netcoredbg, and `--hit` would mean what each adapter
  makes of `hitCondition`.

### Session-wide exception mode

* Good, because it is what DAP models.
* Bad, under the lease: a human's exception setting in phase 2 would be refused while an agent
  holds the lease. Bad, last writer wins: clients silently undo each other.

### Trusting the DAP context for side effects

* Bad, because netcoredbg ignores it: the flag would do nothing.

### Microsoft.Testing.Platform's wait-for-debugger

* Bad, because whether `dotnet test` forwards the host's pid line in MTP mode is unverified; a
  missed line is a silent hang.

### Launching `dotnet test` under the adapter

* Bad, because it debugs MSBuild and vstest.console, not the tests.

### Finding the host among the runner's descendants

* Neutral: kept as the fallback if a future SDK drops the pid line; it needs per-OS process-tree
  code and a trigger to look.

### Re-resolving anchors when the file changes

* Bad, because the running program still has the old code: a moved anchor lands on a line whose
  code isn't what runs there.

## More Information

Planned in `m5-breadth` (plan decisions D1, D4, D5, D8, D9). Amended in `p2-mtp-tests` (2026-09-26,
plan decisions D1–D14 with D8 superseded by D8a, plus D15/D16): Microsoft.Testing.Platform, xUnit
v3 and TUnit test runs launched directly (superseding the "MTP test runs" follow-up below), and
driver-marked untrusted exit codes for netcoredbg on macOS. Follow-ups: `restart` re-resolving
every anchor against the rebuilt program; `--hit`/`--log` on function breakpoints; native
pass-through where adapters support it; per-framework test hosts; macOS and Windows validation of
attach and test.
