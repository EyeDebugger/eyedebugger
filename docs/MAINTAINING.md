# Maintaining EyeDebugger

## Required checks

`ci-ok` (aggregates all CI jobs), `conventional-commits`, `DCO`. Renaming a job means updating the
ruleset.

## Merge policy

Squash only. The default message is "Pull request title and commit details", which keeps the
`Signed-off-by` trailers. Auto-delete head branches.

## Pinned versions and where to bump them

Dependabot handles only action SHAs and `go.mod`; everything else is a manual bump.

- Go: the `go.mod` minimum, `ci.yml` `GO_VERSION`, and the test matrix `go` values.
- golangci-lint and govulncheck: `ci.yml` `env` + `Taskfile.yml` `vars` + CONTRIBUTING/AGENTS
  mentions.
- goreleaser: `ci.yml` `env`.
- `actions/setup-dotnet` and `actions/setup-python`: pinned by SHA in `ci.yml`; bump the version
  comment alongside. `DOTNET_VERSION` and `PYTHON_VERSION`: `ci.yml` `env`.
- `actionlint`: `ci.yml` `env` (`ACTIONLINT_VERSION`) **and** `Taskfile.yml` `vars` (its own copy,
  used by `task lint:workflows`) — bump both.
- `zizmor`: `ci.yml` `env` (`ZIZMOR_VERSION`) only; it isn't wired into any task, run it by hand
  (see Workflow security rules below).
- Runner labels (the `test` job matrix's `os:` values, and the `full`/`reduced` `os:` values in the
  `e2e-matrix` job's script): bump when GitHub renames or retires one (e.g. `-latest` moving to a
  new default).

## CI cost while private

Private-repo runners are billed per minute past the included quota: Linux x64 $0.006, Linux
arm64 $0.005, Windows x64/arm64 $0.010, macOS $0.062 — **macOS costs about 10× Linux x64**. The
`test` job's full 6-platform matrix (two of the six rows are macOS, two are Windows) runs on every
push to `main`, but `e2e` runs on `ubuntu-latest` only on push/pull_request; its full 6-platform
matrix runs only on `workflow_dispatch` and the weekly `schedule`. Watch usage under Settings →
Billing while the repo is private; making it public removes the cost (hosted runners are free for
public repos).

## Workflow security rules

- Pin actions by full SHA with a version comment.
- Least-privilege `permissions`; `persist-credentials: false`.
- Never `pull_request_target` with checkout of PR code.
- Untrusted values go in via `env`, never `${{ }}` inside `run:`.
- No caches in release jobs.
- Run `actionlint` and `zizmor --offline .` on every workflow change. `task lint:workflows` runs
  actionlint only; run zizmor by hand: `go run github.com/rhysd/actionlint/cmd/actionlint@$ACTIONLINT_VERSION`
  and, separately, `pipx run --spec zizmor==$ZIZMOR_VERSION zizmor --offline .`.

## One-time setup after publishing

1. Create the public repo `EyeDebugger/eyedebugger` with no auto-generated files, and push `main`.
2. Org → Actions → General: "Require actions to be pinned to a full-length commit SHA".
3. Org: require 2FA for members. It is currently off.
4. Repo → Advanced Security:
   - Private vulnerability reporting.
   - Dependabot alerts and security updates.
   - Secret scanning and push protection, if offered.
   - Code scanning → CodeQL default setup.
5. Install the DCO App (https://github.com/apps/dco) on this repository only.
6. Repo → General:
   - "Require contributors to sign off on web-based commits".
   - Squash merging only, with "Pull request title and commit details"; auto-delete head branches.
   - Enable Discussions, or remove that contact link from `.github/ISSUE_TEMPLATE/config.yml`.
7. Ruleset on `main`:
   - Require a PR with **0** approvals while there is one maintainer, rising to 1 with the second.
   - Required checks `ci-ok`, `conventional-commits`, `DCO`.
   - Block force-push and deletion; require linear history.

## Releasing (not enabled)

- `task snapshot` (GoReleaser, local) is the dry run: it builds release-shaped archives into
  `dist/` without publishing, so packaging can be checked before any of this is wired up.
- Remove `release.disable`.
- Add a tag-triggered release workflow with `contents: write` only in that job and no caches.
- Move `[Unreleased]` to the version.
- Ship third-party license notices in archives (pflag is BSD-3-Clause).
- Consider signing and provenance.
