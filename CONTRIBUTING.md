# Contributing

## Before you start

- Open an issue first for non-trivial work.
- Architectural changes need an ADR (see [docs/adr/README.md](docs/adr/README.md)).
- Read [docs/DESIGN.md](docs/DESIGN.md) and [docs/CONVENTIONS.md](docs/CONVENTIONS.md).

## Development setup

- Go ≥ 1.26 (CI uses the latest 1.27.x).
- Task v3: `winget install Task.Task`, `scoop install task`, `choco install go-task`, or
  `go install github.com/go-task/task/v3/cmd/task@latest`; other platforms at
  https://taskfile.dev/installation/.
- **golangci-lint v2.13.2**, installed as a binary
  (https://golangci-lint.run/docs/welcome/install/local/). Older versions report different issues.
- Optional: goreleaser v2 for `task snapshot`.
- `task test:race` needs cgo and a C compiler (on Windows, MinGW-w64 gcc).
- For `task e2e`: the .NET 10 SDK and `eyedbg adapters install netcoredbg`; Python 3.10+ and
  `eyedbg adapters install debugpy`.
- Without Task, use the raw commands below.

## Everyday commands

| Task | Raw equivalent |
|---|---|
| `task build` | `go build -o bin/ ./cmd/...` |
| `task test` | `go test -shuffle=on ./...` |
| `task test:race` | `go test -race -shuffle=on ./...` |
| `task test:golden` | `EYEDBG_UPDATE_GOLDEN=1 go test ./...` |
| `task e2e` | `EYEDBG_E2E=1 go test -count=1 -p 1 -timeout 40m ./drivers/... ./internal/e2e/...` |
| `task lint` | `golangci-lint run` |
| `task lint:workflows` | `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12` |
| `task fmt` | `golangci-lint fmt` |
| `task vuln` | `go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...` |
| `task tidy:check` | `go mod tidy -diff` |
| `task build:cross` | cross-compile all 6 os/arch targets, `CGO_ENABLED=0` |
| `task ci` | runs the checks CI runs |

A change to `drivers/...` or `internal/e2e` needs a clean `task e2e COUNT=5` run before it merges —
these tests spawn real processes and daemons, so run them repeatedly to catch flakes a single pass
would miss.

## Commits and PR titles

- [Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/). Types: build,
  chore, ci, docs, feat, fix, perf, refactor, revert, style, test.
- Scope = area (`cli`, `daemon`, `session`, `dap`, `dotnet`, `deps`, `adr`).
- Breaking changes use `!` or a `BREAKING CHANGE:` footer.
- CI checks the PR title, and it becomes the squash-commit subject.
- Rename GitHub's `Revert "…"` titles to `revert: …`.

## Developer Certificate of Origin

- Every commit needs `Signed-off-by: Name <email>` matching the commit author's name and email:
  `git commit -s`. See [DCO 1.1](https://developercertificate.org/).
- To fix a missing sign-off, run `git rebase --signoff <base-branch>` and force-push the PR branch,
  or follow the DCO check's remediation text.
- Web-UI commits are signed off automatically by a repo setting.

## AI-assisted contributions

- Welcome. The submitting human is the author: review every line, make sure it can be licensed
  under Apache-2.0, and run the checks.
- **Only humans sign off. AI agents must not add `Signed-off-by`** (same rule as
  https://docs.kernel.org/process/coding-assistants.html).
- Disclose meaningful assistance with an `Assisted-by: <tool> (<model>)` trailer. A
  `Co-authored-by:` attribution trailer added by the tool also counts.
- Agent rules live in [AGENTS.md](AGENTS.md).

## Pull requests

- Keep them small, and say what, why and how it was tested.
- Required checks: `ci-ok`, `conventional-commits`, `DCO`. Review by CODEOWNERS; squash-merge.
- Explain golden-file diffs and justify new dependencies.

## Bugs and vulnerabilities

Use the issue forms. Security reports go through [SECURITY.md](SECURITY.md), never a public issue.

## License

Apache-2.0, inbound = outbound (§5 of the license), certified by the DCO; no CLA.
