# Maintaining EyeDebugger

## Required checks

`ci-ok` (aggregates all CI jobs), `conventional-commits`, `DCO`. Renaming a job means updating the
ruleset.

## Merge policy

Squash only. The default message is "Pull request title and commit details", which keeps the
`Signed-off-by` trailers. Auto-delete head branches.

## Pinned versions and where to bump them

Dependabot handles action SHAs, `go.mod`, the extension's npm dev dependencies
(`extensions/vscode`, weekly, grouped, 7-day cooldown) — except `@types/vscode` (must equal
`engines.vscode`) and semver-major bumps of `@types/node`/`typescript` (ADR 0015) — and the .NET
helper's NuGet packages (`nuget`, `/helpers/dotnet`, weekly, grouped, 7-day cooldown; ADR 0016).
Everything else is a manual bump.

- Go: the `go.mod` minimum, `ci.yml` `GO_VERSION`, and the test matrix `go` values.
- golangci-lint and govulncheck: `ci.yml` `env` + `Taskfile.yml` `vars` + CONTRIBUTING/AGENTS
  mentions.
- goreleaser: `ci.yml` `env`.
- `actions/setup-dotnet` and `actions/setup-python`: pinned by SHA in `ci.yml`; bump the version
  comment alongside. `DOTNET_VERSION` and `PYTHON_VERSION`: `ci.yml` `env`.
- `actionlint`: `ci.yml` `env` (`ACTIONLINT_VERSION`) **and** `Taskfile.yml` `vars` (its own copy,
  used by `task lint:workflows`) — bump both.
- `zizmor`: `ci.yml` `env` (`ZIZMOR_VERSION`) and the `zizmorcore/zizmor-action` SHA/version comment
  in the `workflows` job — bump both; not wired into any task, run it by hand for local iteration
  (see Workflow security rules below).
- Runner labels (the `test` job matrix's `os:` values, and the `full` array's `os:` values in the
  `e2e-matrix` job's script — `reduced` is derived from `full`'s first row): bump when GitHub
  renames or retires one (e.g. `-latest` moving to a new default).
- `NODE_VERSION`: `ci.yml`, `release.yml` and `publish-extension.yml` `env` — bump all three
  together.
- `actions/upload-artifact` and `actions/download-artifact`: pinned by SHA in `ci.yml` and
  `release.yml`; Dependabot bumps these (`github-actions` ecosystem).
- The .NET helper (ADR 0016): the SDK floor in `helpers/dotnet/global.json` (10.0.x, feature
  roll-forward); `DOTNET_VERSION` in `ci.yml` **and** `release.yml` `env`; the helper job's extra
  `8.0.x` (its test floor, the helper's `net8.0`); package versions in
  `helpers/dotnet/Directory.Packages.props` (Dependabot). A Dependabot PR that fails the locked
  restore needs its lock files regenerated: `dotnet restore --force-evaluate` in `helpers/dotnet`,
  commit both `packages.lock.json`. When the set of shipped assemblies changes (a new or dropped
  transitive package), update `helpers/dotnet/THIRD-PARTY-NOTICES.txt` — CI's notices check fails
  until it names every shipped `.dll`.
  ClrMD (`Microsoft.Diagnostics.Runtime`, ADR 0016's P2-M7 addendum) is in the same Dependabot
  group; before merging a major bump, check a static root is still named
  (`EYEDBG_E2E=1 go test -run DotnetDump ./internal/e2e/` asserts `static Holder.Keep`) and that
  nothing constructs its symbol server (`grep -rn SymbolServer helpers/dotnet/src` finds only
  comments).
  On a TraceEvent bump (`Microsoft.Diagnostics.Tracing.TraceEvent`, ADR 0016's P2-M8 addendum),
  re-run the trace e2e (`EYEDBG_E2E=1 go test -run DotnetTrace ./internal/e2e/` asserts
  `Burn.Spin` among the hottest methods) and check the summary still resolves no symbols:
  `grep -rnE 'SymbolReader|ShouldResolveSymbols|AlwaysResolveSymbols|LookupSymbolsForModule|GetSourceLine' helpers/dotnet/src`
  finds nothing, and every `CreateFromEventPipeDataFile(` passes an explicit ETLX path.

## CI cost

The repo is public, so GitHub-hosted runners are free. `e2e` still runs on `ubuntu-latest` only on
push/pull_request (its full 6-platform matrix runs on `workflow_dispatch` and the weekly
`schedule`) to keep feedback fast.

## Releasing

1. Move `CHANGELOG.md`'s `[Unreleased]` entries under a new `## [X.Y.Z] - YYYY-MM-DD` heading.
2. Set `extensions/vscode/package.json` `version` to `X.Y.Z` (the release's `vsix` job fails
   otherwise — the extension's version follows eyedbg's).
3. Commit, then tag and push: `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`.
4. `.github/workflows/release.yml` builds the archives with GoReleaser, publishes the GitHub
   release with that CHANGELOG section as its notes, attests their provenance, and attaches the
   extension as `eyedebugger_X.Y.Z_vscode.vsix` (listed in `checksums.txt` and covered by the same
   attestation — `gh attestation verify eyedebugger_X.Y.Z_vscode.vsix -R EyeDebugger/eyedebugger`
   works for it too). Every archive carries the .NET side helper under `helpers/dotnet/`, built
   by release.yml's unprivileged `helper` job and packed by goreleaser, which fails if it's missing.
   Its last job then calls `publish-extension.yml` (below).
5. Pre-release tags (`vX.Y.Z-rc.N`, same version format in `package.json`): the VSIX is built and
   attached like any other, but never published to either registry.

## Publishing the VS Code extension

Releases attach the VSIX, but publishing it to the VS Code Marketplace and Open VSX only happens
once you configure the secrets below. Until then, `publish-extension.yml` runs on every release and
does nothing but print a notice.

One-time Marketplace setup: create publisher ID `eyedebugger` at
https://marketplace.visualstudio.com/manage (permanent once created; if it's taken, change
`publisher` in `extensions/vscode/package.json` first). Create an Azure DevOps personal access
token scoped to "All accessible organizations" with Marketplace → Manage, and a short expiry.
**Global PATs retire on 2026-12-01**: switch to Entra ID (`vsce publish --azure-credential` with
`azure/login` workload identity federation) before then.

One-time Open VSX setup: an eclipse.org account using your GitHub username, a signed Publisher
Agreement, an access token (Settings → Access Tokens), and the namespace created once with
`pnpm dlx ovsx@1.2.0 create-namespace eyedebugger -p <token>` (optionally claim namespace ownership
afterwards for the "verified" badge).

GitHub environments: create environment `vscode-marketplace` holding secret `VSCE_PAT`, and
`open-vsx` holding secret `OVSX_PAT`. **Environment secrets only, never repository secrets.** Set
each environment's deployment branches/tags to "Selected" with tag rule `v*`. Required reviewers
are optional; leave "prevent self-review" off while solo. Both environments may already exist,
auto-created empty by the M4 validation run — add the secrets and the tag rule to them.

Dry run before adding secrets: `gh workflow run publish-extension.yml --ref vX.Y.Z -f tag=vX.Y.Z -f dry-run=true`
downloads and verifies the VSIX without publishing.

To publish a past release: `gh workflow run publish-extension.yml --ref vX.Y.Z -f tag=vX.Y.Z`
(dispatch on the tag ref so the `v*` deployment rule matches). Re-runs are safe: both registries
skip a version they've already published.

Each job verifies before publishing: the VSIX's checksum against the release's `checksums.txt`, the
release's build-provenance attestation for that VSIX, and that the VSIX's own
`extension/package.json` version matches the tag.

## Workflow security rules

- Pin actions by full SHA with a version comment.
- Least-privilege `permissions`; `persist-credentials: false`.
- Never `pull_request_target` with checkout of PR code.
- Untrusted values go in via `env`, never `${{ }}` inside `run:`.
- No caches in release jobs.
- Run `actionlint` and zizmor on every workflow change. `task lint:workflows` runs actionlint only;
  CI's `workflows` job runs zizmor via the SHA-pinned `zizmorcore/zizmor-action` (offline audits,
  no Advanced Security upload). Run both locally by hand:
  `go run github.com/rhysd/actionlint/cmd/actionlint@$ACTIONLINT_VERSION` and, separately,
  `pipx run --spec zizmor==$ZIZMOR_VERSION zizmor --offline .`.
- npm/third-party build code never runs in a job holding `contents: write` or `id-token: write`
  (`release.yml`'s `vsix` job is `contents: read` only; the VSIX moves to the privileged `release`
  job as a run artifact).
- Publishing secrets (`VSCE_PAT`, `OVSX_PAT`) live in environments, never repository secrets, and
  appear only in the publishing step's `env` — every earlier step is gated behind a `Decide` step
  that checks `secrets.X != ''` as a boolean, so the secret itself is never read before that.
- `uses: ./…` (a same-repo reusable workflow call) carries a trailing
  `# zizmor: ignore[self-repository]` comment until actionlint can parse GitHub's `$/` shorthand
  (rhysd/actionlint#711).

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

## Release dry run and open items

- `task snapshot` (GoReleaser, local) is the dry run: it packages the extension first (needs Node
  and pnpm), then builds release-shaped archives and the VSIX's checksum into `dist/` without
  publishing.
- Open: ship third-party license notices in archives (pflag is BSD-3-Clause).
