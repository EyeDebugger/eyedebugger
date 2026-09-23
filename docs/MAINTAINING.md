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

## Workflow security rules

- Pin actions by full SHA with a version comment.
- Least-privilege `permissions`; `persist-credentials: false`.
- Never `pull_request_target` with checkout of PR code.
- Untrusted values go in via `env`, never `${{ }}` inside `run:`.
- No caches in release jobs.
- Run `actionlint` and `zizmor --offline .` on every workflow change.

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

- Remove `release.disable`.
- Add a tag-triggered release workflow with `contents: write` only in that job and no caches.
- Move `[Unreleased]` to the version.
- Ship third-party license notices in archives (pflag is BSD-3-Clause).
- Consider signing and provenance.
