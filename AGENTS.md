# Agent instructions

EyeDebugger (`eyedbg`) is an AI-native, CLI-first debugger; agents and humans share one live debug
session through a per-user daemon. There is no MCP server by default: the CLI is the agent
interface. `docs/DESIGN.md` is the source of truth for design; decisions are recorded in
`docs/adr/`.

## Repo map

- `cmd/eyedbg/`, `cmd/eyedbgd/` — wiring-only entry points for the two binaries.
- `internal/cli/` — command trees for all binaries; the only importer of cobra/pflag besides `cmd/`
  (enforced by depguard).
- `internal/version/` — build metadata (ldflags / `debug.ReadBuildInfo`).
- `internal/api/` — native JSON-RPC schema shared by the CLI and the extension.
- `internal/daemon/` — daemon lifecycle, IPC, token auth.
- `internal/session/` — session, lease, breakpoint ownership, event log, stop snapshot.
- `internal/dap/` — DAP client: framing, seq mapping, reverse requests.
- `internal/facade/` — per-session DAP facade (phase 2).
- `internal/present/` — budgeting, truncation, text/JSON renderers.
- `internal/adapters/` — adapter manifest loader and installer.
- `drivers/dotnet/`, `drivers/generic/` — language drivers implementing DESIGN §7's `Driver`.
- `helpers/dotnet/` — C# side helper (phase 2).
- `skill/` — `SKILL.md`, the agent-facing usage guide (milestone 6).
- `testdata/apps/` — sample debuggees for e2e tests.
- `docs/adr/` — architecture decision records (MADR 4.0).

## Commands

Run `task ci` before finishing. Individual tasks and their raw equivalents are in
`CONTRIBUTING.md`. golangci-lint must be **v2.13.2**; older versions report different issues.

## Rules

1. Never download, detect, launch or offer vsdbg (see `docs/adr/0005-never-use-vsdbg.md`).
2. No new dependency without justification in the PR; significant ones need an ADR.
3. CLI output is an API. Text is for LLM readers; `--json` carries `"schema"`. Change output
   deliberately: regenerate goldens with `task test:golden` and explain the diff. An incompatible
   JSON change bumps `schema`.
4. Follow `docs/CONVENTIONS.md`: wrap errors with `%w`; use `log/slog` with injected loggers; `ctx`
   first, never stored in structs; no `fmt.Print*`; no globals or `init()`.
5. Every `.go` file has the two-line SPDX header.
6. Never weaken `.golangci.yml`. Use `//nolint:<linter> // <reason>` only when justified.
7. Windows is first-class: no bash-only scripts, `filepath` for OS paths.
8. Tests: stdlib `testing`, table-driven, `t.Parallel()`, no sleeps.
9. Debuggee data may hold secrets: never copy it into logs, issues or commits.

## Commits

- Conventional Commits.
- **Never add `Signed-off-by`**: the human submitter signs off.
- Add `Assisted-by: <tool> (<model>)`, or keep the tool's own `Co-authored-by:` attribution trailer.
- Never `--no-verify`.

## Before finishing

Run `task ci` (or its individual commands), report the real results, and say what couldn't be run.
