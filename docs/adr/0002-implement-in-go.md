---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Implement in Go

## Context and Problem Statement

`eyedbg` is invoked by agents many times per debugging loop (docs/DESIGN.md §2), so process start
time matters. The project also ships one static binary across six OS/arch targets, cross-compiled
from a single host, and needs a Debug Adapter Protocol (DAP) client library. Which language should
the daemon and CLI be written in?

## Decision Drivers

* Many short-lived process invocations (agent-driven CLI calls): startup latency matters.
* Single static binary, cross-compiled to linux/darwin/windows × amd64/arm64 from one host.
* An existing, maintained DAP client/types library.
* Contributor familiarity and hiring pool for an OSS project.

## Considered Options

* Go
* Rust
* C#/.NET
* TypeScript/Node.js

## Decision Outcome

Chosen option: "Go", because it cross-compiles every target OS/arch from a single host without
extra toolchains, starts in roughly 5 ms (docs/DESIGN.md §2), and `google/go-dap` — used by delve —
already provides DAP types and message framing.

### Consequences

* Good, because `GOOS`/`GOARCH` cross-compilation needs no per-target toolchain install
  (docs/DESIGN.md §12, `task build:cross`).
* Good, because `google/go-dap` removes the need to hand-write DAP message types.
* Bad, because .NET-specific inspection (ClrMD, EventPipe) has no first-class Go bindings, so that
  logic lives in a separate C# side helper (`helpers/dotnet/`, docs/DESIGN.md §7-8) instead of the
  main binary.

## Pros and Cons of the Options

### Go

* Good, because of fast, dependency-free cross-compilation to a static binary.
* Good, because of low process-start overhead, which matters for repeated agent invocations.
* Good, because `google/go-dap` is maintained and already used by a real DAP client (delve).
* Bad, because deep .NET runtime introspection needs an out-of-process C# helper.

### Rust

* Good, because of comparable startup and binary-size characteristics.
* Bad, because there is no equivalently mature, widely used DAP client library.
* Bad, because a smaller pool of contributors familiar with the language for this kind of project.

### C#/.NET

* Good, because it gives direct, native access to ClrMD, EventPipe and .NET runtime APIs.
* Bad, because NativeAOT cross-compilation has real limits building for every target from one host
  (see docs/DESIGN.md §15 for the reference).
* Bad, because it commits the whole project to the .NET runtime model even for non-.NET debugging.

### TypeScript/Node.js

* Good, because of a large ecosystem and `vscode-debugadapter`-adjacent tooling.
* Bad, because it needs the Node runtime or a bundler/packager to ship a single binary, and startup
  is slower than a native binary.

## More Information

* google/go-dap: https://github.com/google/go-dap
* Delve DAP implementation: https://github.com/go-delve/delve/tree/master/Documentation/api/dap
* NativeAOT cross-compile limits: https://learn.microsoft.com/en-us/dotnet/core/deploying/native-aot/cross-compile
