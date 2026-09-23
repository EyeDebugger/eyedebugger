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

## Testing

- Stdlib `testing`; testify is blocked. `github.com/google/go-cmp` may be added with justification
  when a test needs structural diffs.
- Table-driven, with `t.Run` + `t.Parallel()`.
- Use `t.TempDir`, `t.Setenv` (not with Parallel) and `t.Context`. No sleeps.
- Golden files live in the package's `testdata/`. Regenerate them with `task test:golden` and
  review the diff.
- CI runs `-race -shuffle=on`.
- e2e tests against real adapters arrive with milestone 6.

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
