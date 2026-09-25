# Code conventions

## Package boundaries

- `cmd/*` is wiring only: signal ctx, build info, `cli.Run`, `os.Exit`.
- `internal/cli` is the only importer of cobra and pflag.
- `internal/api` holds wire types and stable error codes.
- `internal/session` owns session state. `internal/dap` frames DAP in both directions (client
  toward adapters, server toward editors); `internal/session` talks to adapters and may expose
  adapter-level go-dap values (capabilities, passthrough requests and responses);
  `internal/facade` alone speaks DAP to editors and turns their requests into session calls.
- `drivers/*` implement the DESIGN §7 Driver interface and are registered explicitly (no `init()`).
- No package-level mutable state (`//nolint` only for link-time vars).
- Dependencies point inward: cli → daemon/session/api, never the reverse.

## Errors

- Wrap with context, lowercase and without "failed to": `fmt.Errorf("open socket %s: %w", p, err)`.
- Sentinels are `ErrX`; error types are `XError`. Use `errors.Is`/`errors.As`.
- Don't log and return the same error.
- User-facing errors carry stable `{code, message, hint}` from `internal/api` (DESIGN §5).
- Panic only on programmer error. Exit codes are decided in `internal/cli`; `os.Exit` only in `main`.

## Logging

- `log/slog` only.
- Inject `*slog.Logger` and never use the global logger.
- Use the `*Context` variants when a ctx is in scope.
- Static messages and snake_case keys; don't use the reserved keys (`time`, `level`, `msg`, `source`).
- Logs are for operators; results go to the output writers.
- Never log secrets or raw variable values without redaction (DESIGN §5/§11).

## Context

- `ctx` is the first param for anything that blocks or does I/O, and is never stored in a struct.
- The root ctx comes from `signal.NotifyContext` in `main`.
- Set timeouts at call sites and honor cancellation.
- Tests use `t.Context()`.

## Concurrency

- The starter of a goroutine owns stopping it (ctx + wait).
- Mutexes live with the owning type.
- DESIGN §3 rules: execution-changing requests are serialized per session.
- `-race` runs in CI on every OS.

## Output

- Results go to the command's stdout writer and diagnostics to stderr, never to `os.Stdout` or
  `fmt.Print*`.
- Text output is compact and tuned for LLMs.
- `--json` is stable and carries `"schema": N`; bump it only on incompatible changes.
- Every output shape has a golden test.

## Help text

- Help is the interface (DESIGN §4): there is no MCP server by default, so the CLI's own help is
  how an agent is expected to learn the tool.
- Every command **and subcommand** — including each binary's root command — has a `Short`, a `Long`
  (what it does, when to use it, whether it blocks and for how long, its side effects on the
  debuggee, its output shape, and its exit codes) and at least one `Example` with a real
  invocation. Flags document their unit and default in their usage string.
- Help must be reachable both ways, at every level of the tree: `eyedbg <path...> --help` and
  `eyedbg help <path...>`.
- `internal/cli`'s tests enforce this: one walks the whole command tree and fails on any non-hidden
  command missing `Short`, `Long` or `Example`; another executes both help forms for every command
  and asserts they print that command's own `Long` and `Example`. A new command without help fails
  CI, not review.
- `eyedbg help --all` prints the full tree's help in one read, and `--json` turns any help into
  structured data (`internal/cli/help.go`); both are built from the same `Short`/`Long`/`Example`
  and flags, so there is nothing extra to maintain.
- Help text is a tested, reviewed artifact: change it deliberately, the same as `--json` output.

## Testing

- Stdlib `testing`; testify is blocked. `github.com/google/go-cmp` may be added with justification
  when a test needs structural diffs.
- Table-driven, with `t.Run` + `t.Parallel()`.
- Use `t.TempDir`, `t.Setenv` (not with Parallel) and `t.Context`. No sleeps.
- Golden files live in the package's `testdata/`. Regenerate them with `task test:golden` and
  review the diff.
- CI runs `-race -shuffle=on`.
- Session-level tests run real DAP round trips against `internal/dap/daptest`, a fake adapter:
  the test binary re-executes itself as the adapter. A package whose tests start sessions needs a
  `TestMain` that calls `daptest.MaybeRun()` first and returns when it returns true, plus a small
  fake `session.Driver` launching `daptest.Command()` with `daptest.Arguments(...)`. Test runs use
  the fake runner too (`daptest.MaybeRunRunner()` in `TestMain`, `daptest.RunnerCommand`).
- e2e tests come in three tiers, all driving real, separately built processes rather than calling
  any eyedbg package directly:
  1. Driver-level e2e (`drivers/dotnet`, `drivers/generic`) drives one driver's DAP round trip
     against a real adapter.
  2. CLI-level e2e (`internal/e2e`) drives `eyedbg`/`eyedbgd` themselves as built binaries, through
     autostart, exit codes and `--json` output, the way an agent or a human at a shell would.
  3. Within `internal/e2e`, the fake-adapter case (`daptest`, this test binary re-executing itself)
     always runs in the unit suite; the python/dotnet cases behind `EYEDBG_E2E=1` are the same
     script against real adapters.
  All real-adapter tests (`EYEDBG_E2E=1`) drive the sample apps in `testdata/apps`, copied to a
  temporary directory first, and find lines by their `// marker: NAME` (`# marker: NAME`) comments,
  not by number. `drivers/dotnet` needs the .NET SDK and netcoredbg; `drivers/generic`
  (`TestPython*`) needs Python 3.10+ with debugpy (`EYEDBG_PYTHON=/path/to/venv/bin/python`) or
  `adapters install debugpy`, and `TestPythonManagedInstall` also `EYEDBG_E2E_NETWORK=1` (it
  downloads debugpy into a temporary data directory). `drivers/generic`'s `TestLldb*`/`TestCpp*`/
  `TestCAttach` (c, cpp, rust) need `cc`/`c++`/`rustc` on PATH and lldb-dap (`EYEDBG_LLDB_DAP` or
  PATH); unlike python and dotnet there is no bundled download, so a missing compiler or lldb-dap
  fails the test directly, the same "never skip silently" rule below. `drivers/generic`'s `TestGo*`
  (go, over schema 1's connect transport) need Go on PATH and Delve (`EYEDBG_DLV` or PATH, or
  `adapters install delve`); `TestGoManagedInstall` also needs `EYEDBG_E2E_NETWORK=1` (it downloads
  Delve into a temporary data directory). `requireE2E`/`require`
  closures are the only skip point: without `EYEDBG_E2E=1` the test skips; with it set and the
  adapter missing or broken, it fails, never skips silently. `EYEDBG_E2E_LANGS` (comma-separated
  language names) narrows which languages a real-adapter run exercises: unset runs every language;
  set, a language left out skips with a message naming the variable — everything named still
  follows the never-skip-silently rule. `task e2e` runs all real-adapter tests (`drivers/...` and
  `internal/e2e`) in one command; run it with `COUNT=5` before merging a change under `drivers/...`
  or `internal/e2e` to catch flakes a single pass would miss. Never add retries, sleeps or
  `-run`-scoped workarounds to make a flaky e2e test pass — fix the race or report it.
- Bundled adapter manifests are validated by a unit test (`internal/adapters`); a language that
  needs no Go logic gets a manifest, not a driver, and the generic driver's fake-adapter tests
  (`drivers/generic`) show how to debug a manifest-only language in tests.

## Dependencies

- Stdlib first.
- The PR justifies each new module: what, why not stdlib, license, maintenance. Significant ones
  get an ADR.
- No `replace` directives.
- `go mod tidy -diff` must be clean.
- Dependabot runs weekly with a 7-day cooldown.

## Formatting and linting

- `task fmt` (gofumpt + goimports).
- `task lint` must report 0 issues with v2.13.2.
- `//nolint:<linter> // <reason>` is the only suppression form.

## File headers and doc comments

- Two-line SPDX header on every file.
- A package doc comment (`doc.go` for multi-file packages).
- Exported identifiers are documented, as sentences ending with a period.

## Portability

- Linux, macOS and Windows × amd64/arm64 are all first-class.
- Use `path/filepath` for OS paths.
- Platform code goes in `_windows.go` / `_unix.go` files; the lint job runs on all 3 OSes.
- Release builds use `CGO_ENABLED=0`. No build logic in shell scripts.

## TypeScript (extensions/vscode)

The VS Code extension (ADR 0015) follows the same rules where they apply; these are its own.

- `src/core/` is pure — no `vscode` import (a unit test enforces it) — and holds everything that
  can be unit-tested: parsing, validation, argv building, the mirror model, lease decisions and
  every string shown to the user. `src/vscode/` is the glue. `src/vscode/exec.ts` is the only
  place that starts a process.
- Processes: `child_process.execFile` of the resolved absolute `eyedbg`, never a shell; flag
  values bound as `--flag=value`, program arguments after `--`; every value from a configuration
  validated first. Nothing a workspace can set names the binary (`eyedbg.path` is
  machine-scoped).
- Text from a session (other clients' names, conditions, log and lease-request messages, the
  adapter's messages, program paths) is untrusted: build it in `src/core/render.ts`; control
  characters become spaces and lengths are capped (`plainText`); notifications go through
  `notificationSafe` (VS Code runs `command:` links in them); hovers are `MarkdownString`s with
  `isTrusted` and `supportHtml` off, filled with `appendText`; labels that render `$(icon)` go
  through `noIcons`.
- Logs never hold debuggee data (values, output, evaluation results); `--env` values are redacted
  (`redact`).
- Tests: `node:test` for `src/core`; the integration suite waits on events (DAP messages, the
  extension's API change event, `eyedbg events --wait`) with deadlines — never sleeps or retries —
  and always runs VS Code with a throwaway profile (the runner refuses a real one). Act on VS
  Code's breakpoints after seeing an adapter event only once `synced()` returned. Product code
  has one test hook, `EYEDBG_TEST_ASSUME_FOCUSED=1` (window focus is unreliable under xvfb); add
  no other.
- Biome formats and lints (`task ext:fmt`, `task ext:lint`); `// biome-ignore <rule>: <reason>` is
  the only suppression form.
- Every `.ts` and `.mjs` file starts with the two-line SPDX header.

## C# (helpers/dotnet)

The .NET side helper (ADR 0016) follows the same rules where they apply; these are its own.

- Every `dotnet` command runs in `helpers/dotnet` (its `global.json` pins the 10.0.x SDK; the
  repository root has none, so users' projects built under a daemon started in this repository
  keep their own SDK).
- Every `.cs` file starts with the two-line SPDX header, enforced by IDE0073
  (`helpers/dotnet/.editorconfig`'s `file_header_template`), at build and by `dotnet format`.
- Analyzers at `AnalysisLevel` `10.0-recommended` (pinned, never `latest`), code style enforced at
  build, warnings as errors; a suppression is an `.editorconfig` entry or `[SuppressMessage]` with
  its reason. `task helper:fmt` formats; `task helper:lint` checks.
- stdout is the protocol only: `Program` calls `Console.SetOut(Console.Error)` first and writes
  messages through `Console.OpenStandardOutput()`, one line each, under 1 MiB.
- Nothing read from a target process (values, memory, environment, command lines) goes into
  stderr, exception messages or results beyond what a method's contract names (counter values);
  messages may name a pid, a process name and an exception type.
- `DiagnosticsClient`: only `GetPublishedProcesses` and `StartEventPipeSession[Async]` (read-only
  diagnostics); a new call needs its own review.
- An EventPipe stream is always drained on its own thread until the session ends; every wait on
  the target (session start, stop) is bounded.
- Packages: exact versions in `Directory.Packages.props`, lock files committed, restores locked
  (`--locked-mode`, or `RestoreLockedMode` in CI), nuget.org only (`nuget.config` source mapping).
  A package whose assembly ships needs a `THIRD-PARTY-NOTICES.txt` entry (CI checks).
- Tests: xUnit v3 (`xunit.v3.mtp-off`), run as a program (`task helper:test`); table-driven
  (`[Theory]`), no sleeps — gate on tasks and cancellation, like the Go side.
