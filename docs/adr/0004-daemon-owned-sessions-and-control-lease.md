---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Daemon-owned sessions and control lease

## Context and Problem Statement

EyeDebugger's CLI is stateless, and phase 2 adds a VS Code extension that must join the same live
session as an agent (docs/DESIGN.md §1, §9). Something must own the debug session as the source of
truth, and something must arbitrate who is allowed to change execution state when multiple clients
are attached. Where does that ownership live?

## Decision Drivers

* The CLI is stateless by design (docs/DESIGN.md §2); state has to live somewhere else.
* Phase 2 requires a human (VS Code) and an agent to share one live session (docs/DESIGN.md §9).
* Execution-changing operations (continue/step/pause/restart/terminate, setVariable) must be
  serialized per session (docs/DESIGN.md §3).

## Considered Options

* The CLI owns the session state per invocation.
* The MCP server owns sessions.
* A daemon as the source of truth, with a control lease over execution.

## Decision Outcome

Chosen option: "A daemon as the source of truth, with a control lease over execution"
(docs/DESIGN.md §2, §3, §6): a per-user `eyedbgd` owns sessions, clients, breakpoint ownership, the
event log and stop snapshots; exactly one client holds the control lease at a time, under a
pluggable policy (`free`, `handoff`, `human-priority`).

### Consequences

* Good, because a stateless CLI stays simple: every invocation is just an IPC call.
* Good, because the same session model supports the CLI, the phase 2 VS Code extension, and any
  future MCP wrapper (docs/DESIGN.md §10) without redesign — client identity, breakpoint ownership,
  leases and the event log are built in the MVP even though only the CLI uses them at first
  (docs/DESIGN.md §9).
* Bad, because IPC access control (socket permissions, token file, §6/§11) is required work in the
  MVP, not deferrable to phase 2.
* Bad, because if the daemon dies, sessions die with it (docs/DESIGN.md §6); this is accepted for
  the MVP and session metadata is persisted so `eyedbg sessions` can report "lost" honestly.

## Pros and Cons of the Options

### The CLI owns the session per invocation

* Good, because it needs no daemon process at all.
* Bad, because it cannot support long-lived sessions across many short-lived CLI invocations
  without externalizing state somewhere else anyway.
* Bad, because it gives no path to a shared human/agent session (docs/DESIGN.md §9).

### The MCP server owns sessions

* Good, because it matches the shape of prior art (other MCP-based debuggers, docs/DESIGN.md §1,
  §15) and gives a single controller.
* Bad, because it ties session ownership to one specific client protocol, blocking a stateless CLI
  and a native VS Code integration from sharing the same session on equal footing.

### Daemon as source of truth, with a control lease

* Good, because the CLI, the phase 2 VS Code extension and any future MCP wrapper are all thin
  views/controllers over one model (docs/DESIGN.md §2).
* Good, because the lease policy is explicit and can evolve (`free` now; `handoff` /
  `human-priority` are open questions for phase 2, docs/DESIGN.md §14).
* Bad, because it requires solving IPC hardening and daemon lifecycle in the MVP.

## More Information

* Core concepts and control lease: docs/DESIGN.md §3
* Daemon lifecycle and IPC: docs/DESIGN.md §6
* Phase 2 VS Code co-debugging: docs/DESIGN.md §9
* Open question: lease policy default for phase 2 — docs/DESIGN.md §14
