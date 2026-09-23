---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Drive debuggers through DAP adapters

## Context and Problem Statement

EyeDebugger must control real debuggers (start/stop, breakpoints, stepping, variable inspection)
across multiple languages. Building and maintaining a debug engine per language is explicitly a
non-goal (docs/DESIGN.md §1). How should the daemon talk to the underlying debugger for each
language?

## Decision Drivers

* No new debug engines: reuse what already exists and is maintained upstream.
* A single, language-agnostic integration point in the daemon.
* Ability to add a new language without changing core code when possible.

## Considered Options

* Build our own debug engine per language.
* Bind to each debugger's native API directly (ICorDebug/ClrDebug, lldb, gdb/MI, ...).
* Drive existing debuggers through the Debug Adapter Protocol (DAP).

## Decision Outcome

Chosen option: "Drive existing debuggers through the Debug Adapter Protocol (DAP)", using
`google/go-dap` (docs/DESIGN.md §2, §7). A language plugs in with a declarative adapter manifest
plus an optional Go `Driver` for languages that need extra logic (docs/DESIGN.md §7).

### Consequences

* Good, because the daemon speaks one protocol to every adapter, regardless of language.
* Good, because simple languages need only a manifest (`drivers/generic/`), no Go code.
* Bad, because adapters vary in which DAP capabilities they implement; the daemon must degrade
  gracefully with `UNSUPPORTED_BY_ADAPTER` and record quirks in manifests (docs/DESIGN.md §7).
* Bad, because adapter-specific gaps (e.g. netcoredbg's missing `readMemory`/`disassemble`,
  docs/DESIGN.md §8) must be surfaced honestly rather than papered over.

## Pros and Cons of the Options

### Our own debug engine

* Good, because behavior would be fully under our control.
* Bad, because it duplicates years of existing debugger engineering per language.
* Bad, because it is explicitly out of scope (docs/DESIGN.md §1, non-goals).

### Native per-debugger APIs (ICorDebug/ClrDebug, lldb, gdb/MI)

* Good, because it can expose capabilities a DAP adapter doesn't.
* Bad, because it needs a separate integration per language/platform, with no shared abstraction.
* Bad, because it multiplies the maintenance surface as languages are added.

### DAP adapters

* Good, because DAP is a widely adopted protocol with existing, maintained adapters per language
  (netcoredbg, debugpy, dlv, lldb-dap, js-debug).
* Good, because `google/go-dap` provides framing and types already proven by delve.
* Bad, because the daemon is limited to what an adapter's `initialize` capabilities report.

## More Information

* go-dap: https://github.com/google/go-dap
* DAP spec: https://microsoft.github.io/debug-adapter-protocol/
* Plugin model: docs/DESIGN.md §7
