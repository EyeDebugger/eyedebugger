---
status: proposed
date: 2026-09-26
decision-makers: Ijat (@ijat)
---

# Per-owner conditions at shared breakpoint lines

## Context and Problem Statement

Clients of one session — an agent through the CLI, a human through VS Code and the DAP facade —
often put breakpoints on the same line. ADR 0009 merges every owner's breakpoints per file into one
DAP source breakpoint per line (netcoredbg keeps one per line and orphans duplicates): any
unconditional breakpoint makes the line unconditional, equal conditions are kept, and different
ones make it unconditional with a note on every breakpoint there. So an agent's `--if 'i == 3'`
and a human's `i == 7` at one line stopped at every pass, for both of them, and neither could tell
whose stop it was. ADR 0014 left the same gap for a human's plain breakpoint on the agent's
conditional line (every stop looks like the agent's). ADR 0010's stop filter, which emulates hit
counts and logpoints, already evaluated each breakpoint's own condition where it differed from the
line's merged one — but only while some breakpoint had `--hit` or `--log`, so the same line behaved
differently depending on an unrelated breakpoint elsewhere (ADR 0010's last "bad" consequence).
The filter's truth rule also accepted only the text `true`, which misses a C comparison (`int` 1)
and any Python truthy value.

How should each owner's condition, hit count and log message apply on its own at a shared line,
and how does a client learn which breakpoint a stop is for?

## Decision Drivers

* Each owner's breakpoint means what it says: the program stops at a shared line iff some owner's
  breakpoint wants it (OR), and one owner's logpoint never keeps another's from stopping.
* A missed stop is silent; an extra one is visible. Prefer the extra one.
* Adapters see nothing new: one source breakpoint per line (ADR 0009's netcoredbg constraint), no
  per-language condition syntax in drivers, no manifest schema change.
* Every client — CLI text and `--json`, events, editors — learns whose breakpoint a stop is for.
* Sessions without such lines keep today's stop path, byte for byte.
* Additive wire changes only: no JSON `schema`, native protocol or `facadeVersion` bump.

## Considered Options

* **A. The daemon evaluates each owner's condition** (ADR 0010's stop filter): the adapter's
  breakpoint at the line is unconditional; at every pass the filter evaluates each owner's
  condition and continues unless one wants the stop.
* B. A driver-combined condition sent natively (`(a) || (b)`, Python `or`).
* C. B for speed, plus A for attribution.
* D. One adapter breakpoint per owner.
* E. Keep ADR 0009's degradation (stop unconditionally, with a note).

## Decision Outcome

Chosen option: "A. The daemon evaluates each owner's condition", because it is the only option
that is adapter- and language-independent and can't turn a condition that holds into a missed
stop; it reuses a mechanism already running for `--hit`/`--log` on every adapter.

**Engagement.** A stop with reason `breakpoint` goes through the stop filter while some line
breakpoint has `--hit` or `--log` (as before) or while some line, as the adapter placed it, holds
line breakpoints whose conditions aren't all equal — conflicting conditions, and an unconditional
breakpoint beside a conditional one. Otherwise the stop is applied at once, as before. Function
breakpoints never engage it.

**Planning per placed line.** Per file, the breakpoints at the stop's line — by the line the adapter
placed them on, so two requested lines it moved onto one are one group — are checked together: if
their conditions are all equal, the adapter checked that condition and none is evaluated;
otherwise every non-empty condition is evaluated in frame 0, each distinct text once per stop,
with no short-circuit (hit counts and attribution need every result). A breakpoint whose condition
held is a plain stop, or counts a hit (`--hit`) and then stops or logs (`--log`, never a stop). The
program stops iff some breakpoint wants it; otherwise it continues (a step or pause that ended
there is published with a description saying so, as before).

**Truth rule: fail open.** An `evaluate` result counts as false only when its trimmed text reads as
false in some language: `false`, `none`, `null`, `nil` (any case), `''`, `""`, `[]`, `{}`, `()`,
`set()`, or a number equal to 0 (Go `ParseInt` base 0, else `ParseFloat`). Anything else holds,
the empty text and NaN included. An evaluation error holds (the program stops, as ADR 0010's filter
did). This applies to every condition eyedbg evaluates, the emulated ones of ADR 0010 too.

**Error semantics per path.** Where eyedbg evaluates a condition (a line whose conditions differ,
whatever `--hit`/`--log` those breakpoints have) a failing expression stops. Elsewhere — including
a lone `--hit`/`--log` breakpoint's `--if`, or one sharing a line with equal conditions — the
adapter's own rule applies: debugpy ignores a failing condition (verified with debugpy 1.8.22).
`bp add --help` says both.

**Attribution.** `api.StopInfo.Breakpoints` (`[{id, owner}]`, `breakpoints` omitted when empty,
sorted by id) names the breakpoints a stop is for: each held and wants the stop (logpoints never;
a `--hit` one only when its count matches). It is set only when the filter decided to stop.
Absent means eyedbg didn't decide the stop: at a line breakpoint, every breakpoint there held (the
adapter checked their shared condition), unless eyedbg couldn't read where the program stopped,
in which case it keeps the stop. Surfaces: a snapshot's `  stopped for breakpoint 6 of human:ijat`
line (plural `breakpoints 5 of agent, 6 of human:ijat`) and `session.stop.breakpoints` in
`--json`; the events line `stopped: breakpoint, thread N; for breakpoint 6 of human:ijat`; and in
the DAP facade, `hitBreakpointIds` on `stopped` (live, at the join and on a resync), so VS Code
selects the hit breakpoint. Attribution is as of the stop: a breakpoint removed right after can
still be named.

**run-until.** `reached` is true when the program stopped at the target line and either nothing
is attributed or run-until's own breakpoint (its temporary one, or the caller's own it reused) is
among those named; a stop there only for another client's breakpoint is not reached. The missed
line names the condition: `run-until: did not reach Program.cs:9 with i == 3 (the program stopped
or exited first)`.

**Notes.** Line breakpoints lose ADR 0009's "stops there unconditionally" note (no degradation
left to announce); `bp ls` still shows co-located breakpoints and their owners. Function
breakpoints keep the merge — different conditions at one function stop unconditionally — with the
note reworded: "shares its function with another client's breakpoint that has a different
condition: it stops there unconditionally".

**Unchanged.** Adapters get the same merged `setBreakpoints` as before. Recordings keep a stop's
reason and thread only (no attribution). JSON `schema` stays 1, the native protocol 2,
`facadeVersion` 2; no `version --json` feature (as ADR 0014's addendum did for
`preserveFocusHint`). The VS Code extension is unchanged.

This amends ADR 0009 (the per-line merge's note and its degradation, for line breakpoints), ADR
0010 (the filter's engagement, planning and truth rule) and ADR 0014 (a human's plain breakpoint on
a conditional line; the facade's `stopped` event). Those ADRs are not edited.

### Consequences

* Good, because each owner's condition, hit count and log message apply on their own at a shared
  line: the program stops iff one of them wants it, and another client's logpoint never suppresses
  a stop.
* Good, because every stop eyedbg decides names whose breakpoint it is for, in the CLI, events,
  `--json` and editors (VS Code highlights the hit breakpoint); an agent whose breakpoint isn't
  named knows its condition didn't hold.
* Good, because a human's plain breakpoint on the agent's conditional line (ADR 0014) no longer
  looks like the agent's stop.
* Good, because the truth rule no longer misses stops for a C comparison or a Python truthy value,
  including for `--hit`/`--log` breakpoints.
* Good, because the line behaves the same whether or not an unrelated breakpoint has `--hit` or
  `--log`.
* Bad, because a line whose owners' conditions differ pauses at every pass (a `stackTrace`, one
  `evaluate` per distinct condition, a `continue`), as `--hit`/`--log` lines do; SharpDbg compiles
  each evaluation. A hot loop there is slow. A stuck `evaluate` holds the execution lock up to 30 s
  (ADR 0010's filter, unchanged).
* Bad, because a failing condition stops where eyedbg evaluates it but not where debugpy does.
* Bad, because function breakpoints keep the unconditional merge and have no attribution:
  netcoredbg reports their stops as reason `breakpoint` without `hitBreakpointIds`, and frame names
  differ per adapter.
* Bad, because ADR 0010's first-line limitation widens: the filter attributes stops by location,
  so a line on a function's first line whose clients' conditions differ also decides that
  function's `func:` breakpoint stops (`bp add --help` and DESIGN §8 say so).
* Bad, because the fail-open rule stops on a value an adapter prints in a false-meaning form it
  doesn't list (an extra, visible stop rather than a missed one).

### Confirmation

`internal/session` tests: `TestTruthy`, `TestPlanLocked`, `TestNeedsFilterLocked`,
`TestAttributionSortsAndCopies`, `TestSharedConditions` (fake adapter: different and equal
conditions, conditional beside unconditional, a third client's logpoint), `TestRunUntilSharedLine`,
`TestApplySlotsNotes`, and `TestRecordingIsMinimal` (attribution not recorded). Surfaces:
`internal/facade` `TestStoppedNamesHitBreakpoints` and the `hitBreakpointIds` row of
`TestTranslate`; `internal/cli` `TestSharedConditionsCLI` and the goldens `snapshot_attributed`, `snapshot_attributed_json`, `snapshot_run_until_condition`,
`breakpoints`, `events_newest`; `internal/e2e` `TestSharedConditions` (real binaries, CLI text,
`status --json` and an editor joined through `eyedbg dap`). Real adapters, one scenario each
(the second owner adds a different condition at the line, each continue stops once for its owner,
a third client's logpoint suppresses neither): `TestBreadthSharedConditions` (netcoredbg and
SharpDbg), `TestPythonSharedConditions` (debugpy), `TestGoSharedConditions` (Delve),
`TestLldbSharedConditions` (lldb-dap: C, C++, Rust), in CI's e2e matrix.

## Pros and Cons of the Options

### A. The daemon evaluates each owner's condition

* Good, because it is adapter- and language-independent, and already runs for `--hit`/`--log`.
* Good, because every owner's condition is evaluated as that owner's alone would be, so attribution
  and hit counts fall out of the same decision.
* Bad, because every pass at such a line pauses the program.

### B. A driver-combined condition sent natively

* Good, because the adapter checks it: no pause per pass.
* Bad, because it needs per-language condition syntax in drivers and a manifest schema change.
* Bad, because textual combination breaks on comments and unbalanced text, and adapters' error
  rules differ: a combined condition that is false or fails while one owner's holds is a silent
  missed stop.
* Bad, because it can't say whose breakpoint a stop is for.

### C. B for speed, plus A for attribution

* Good, because the adapter filters most passes.
* Bad, because it inherits B's silent missed stops and schema change.

### D. One adapter breakpoint per owner

* Bad, because netcoredbg keeps one breakpoint per line and orphans duplicates (ADR 0009).

### E. Keep the degradation

* Good, because it costs nothing.
* Bad, because every owner stops at every pass and can't tell whose stop it is.

## More Information

Follow-ups: a native combined condition (B) if the per-pass pause bites; attributing function
breakpoint stops; the VS Code extension naming whose breakpoint a stop is for in its Activity view.
