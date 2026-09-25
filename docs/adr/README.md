# Architecture Decision Records

An ADR records a hard-to-reverse decision: its context, the options considered, and the outcome.
This project uses [MADR 4.0](https://github.com/adr/madr) ([template.md](template.md)).

Write an ADR for hard-to-reverse choices: language, protocols, wire and persisted formats, the
security model, major dependencies, licensing and the contribution process.

## Index

| # | Title | Status |
|---|---|---|
| [0001](0001-record-architecture-decisions.md) | Record architecture decisions | accepted |
| [0002](0002-implement-in-go.md) | Implement in Go | accepted |
| [0003](0003-drive-debuggers-through-dap-adapters.md) | Drive debuggers through DAP adapters | accepted |
| [0004](0004-daemon-owned-sessions-and-control-lease.md) | Daemon-owned sessions and control lease | accepted |
| [0005](0005-never-use-vsdbg.md) | Never use vsdbg | accepted |
| [0006](0006-use-cobra-for-the-cli.md) | Use cobra for the CLI | accepted |
| [0007](0007-use-task-as-the-task-runner.md) | Use Task as the task runner | accepted |
| [0008](0008-apache-2-license-and-dco.md) | Apache-2.0 license and DCO | accepted |
| [0009](0009-client-identity-breakpoint-ownership-lease-and-event-log.md) | Client identity, breakpoint ownership, the control lease and the event log | accepted |
| [0010](0010-breakpoint-kinds-exceptions-attach-and-test-runs.md) | Hit counts and logpoints, exception stops, eval side effects, test runs and anchors | accepted |
| [0011](0011-declarative-adapter-manifests-and-their-trust-model.md) | Declarative adapter manifests and their trust model | accepted |
| [0012](0012-serve-dap-to-editors-through-eyedbg-dap.md) | Serve DAP to editors through `eyedbg dap` | proposed |
| [0013](0013-socket-transport-and-native-manifest-languages.md) | A socket transport and three more manifest-only native languages | accepted |
| [0014](0014-presence-lease-requests-and-shared-breakpoints.md) | Presence, lease requests and sharing breakpoints with editors | proposed |
| [0015](0015-the-vs-code-extension.md) | The VS Code extension: toolchain, dependency policy and shared-breakpoint UX | proposed |
| [0016](0016-a-csharp-side-helper-for-dotnet-diagnostics.md) | A C# side helper for .NET diagnostics | proposed |

## Process

Copy `template.md` to `NNNN-kebab-title.md` with the next number and `status: proposed`, open a
PR, and a maintainer marks it `accepted`.

Accepted decisions aren't edited; a new ADR supersedes them (`superseded by ADR-NNNN`).
