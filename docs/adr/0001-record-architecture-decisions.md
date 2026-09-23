---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Record architecture decisions

## Context and Problem Statement

EyeDebugger will accumulate hard-to-reverse decisions: language, protocols, wire and persisted
formats, the security model, major dependencies, licensing and the contribution process. Without a
record, future contributors (human or AI agent) re-litigate settled questions or unknowingly
violate them.

## Decision Drivers

* Contributors, including AI agents reading `AGENTS.md`, need a durable, greppable record of why a
  decision was made, not just what it is.
* The record should be lightweight enough that writing one isn't a barrier to deciding.

## Considered Options

* No formal record (rely on commit messages and `docs/DESIGN.md`).
* A wiki or external doc tool.
* Architecture Decision Records (ADRs) in-repo, using MADR 4.0.

## Decision Outcome

Chosen option: "Architecture Decision Records in-repo, using MADR 4.0", because it version-controls
decisions alongside the code they constrain, survives repo migrations, and MADR's fixed sections
(Context, Options, Outcome) keep every record comparable.

### Consequences

* Good, because decisions and their rationale are searchable in the same place as the code.
* Good, because `docs/adr/template.md` gives a consistent starting point.
* Bad, because it adds a small amount of process for hard-to-reverse decisions.

## Pros and Cons of the Options

### No formal record

* Good, because it has zero overhead.
* Bad, because rationale gets lost or has to be reconstructed from commit history.

### A wiki or external doc tool

* Good, because it's easy to edit.
* Bad, because it drifts out of sync with the code and isn't reviewed in the same PR.

### ADRs in-repo (MADR 4.0)

* Good, because it's reviewed, versioned and colocated with the code.
* Good, because MADR 4.0 is MIT OR CC0-1.0, so the template can be copied verbatim.
* Bad, because writing one takes more effort than a commit message.

## More Information

* MADR 4.0: https://github.com/adr/madr
