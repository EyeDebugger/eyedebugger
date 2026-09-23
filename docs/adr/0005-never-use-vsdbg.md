---
status: accepted
date: 2026-09-23
decision-makers: Ijat (@ijat)
---

# Never use vsdbg

## Context and Problem Statement

`vsdbg` is the debugger Microsoft ships with Visual Studio and the Microsoft-distributed build of
VS Code / C# tooling. Its documented license restricts it to Microsoft's own IDEs: "vsdbg is not an
open source product but rather is a proprietary part of Visual Studio. It is licensed to work only
with IDEs from Microsoft -- Visual Studio Code, Visual Studio, or Visual Studio for Mac", and
separately, "the debugger is only licensed to work with the Microsoft-distributed version of Visual
Studio Code" (dotnet/vscode-csharp licensing docs). Should EyeDebugger ever download, detect, or
drive vsdbg as a .NET adapter, e.g. as a fallback when netcoredbg has gaps?

## Decision Drivers

* EyeDebugger is not a Microsoft-distributed IDE and must stay license-compliant.
* Contributors and future maintainers need an unambiguous, permanent rule, not a case-by-case
  judgment call.
* .NET debugging still needs a usable default adapter (docs/DESIGN.md §8).

## Considered Options

* Never use vsdbg, under any circumstance.
* Use vsdbg only as an opt-in fallback, with a warning.
* Auto-detect and use vsdbg if already present on the machine.

## Decision Outcome

Chosen option: "Never use vsdbg, under any circumstance." EyeDebugger must never download, detect,
launch or recommend vsdbg. **netcoredbg** (Samsung, MIT) is the default .NET adapter, and
**SharpDbg** (MIT) is the alternative (docs/DESIGN.md §8).

### Consequences

* Good, because the project stays unambiguously compliant with vsdbg's license terms.
* Good, because contributors have a bright-line rule instead of a licensing judgment call
  (`AGENTS.md` rule 1; enforced by review).
* Bad, because netcoredbg's known gaps (no lambda/LINQ-lambda evaluation, no
  `readMemory`/`disassemble`, no `DebuggerDisplay`, docs/DESIGN.md §8) must be surfaced honestly to
  users instead of silently falling back to a more capable but forbidden debugger.

## Pros and Cons of the Options

### Never use vsdbg

* Good, because it removes any license risk entirely.
* Bad, because .NET debugging inherits netcoredbg's and SharpDbg's real limitations.

### Opt-in fallback with a warning

* Good, because it could paper over netcoredbg gaps for users willing to accept the risk.
* Bad, because it still requires shipping code that detects and launches vsdbg outside a licensed
  IDE, which the license text does not carve out an exception for.

### Auto-detect and use if present

* Bad, because it silently violates the license terms and is rejected outright.

## More Information

* vsdbg licensing: https://github.com/dotnet/vscode-csharp/blob/main/docs/debugger/Microsoft-.NET-Core-Debugger-licensing-and-Microsoft-Visual-Studio-Code.md
* .NET adapter choices: docs/DESIGN.md §8
* `drivers/dotnet/doc.go` references this ADR directly.
