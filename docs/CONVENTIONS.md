# Code conventions

## Package boundaries

- `cmd/*` is wiring only: signal ctx, build info, `cli.Run`, `os.Exit`.
- `internal/cli` is the only importer of cobra and pflag.
- `internal/api` holds wire types and stable error codes.
- `internal/session` owns session state; `internal/dap` alone speaks DAP.
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
- e2e tests against real adapters (`EYEDBG_E2E=1`) drive the sample apps in `testdata/apps`,
  copied to a temporary directory first, and find lines by their `// marker: NAME` (`# marker:
  NAME`) comments, not by number. `drivers/dotnet` needs the .NET SDK and netcoredbg;
  `drivers/generic` (`TestPython*`) needs Python 3.10+ with debugpy (`EYEDBG_PYTHON=/path/to/venv/
  bin/python`) or `adapters install debugpy`, and `TestPythonManagedInstall` also
  `EYEDBG_E2E_NETWORK=1` (it downloads debugpy into a temporary data directory).
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
