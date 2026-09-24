---
status: proposed
date: 2026-09-25
decision-makers: Ijat (@ijat)
---

# The VS Code extension: toolchain, dependency policy and shared-breakpoint UX

## Context and Problem Statement

ADR 0012 lets an editor join a session through `eyedbg dap`; ADR 0014 gives it presence, lease
requests, the `eyedbg/*` messages and the other clients' breakpoints as DAP `breakpoint` events at
column 1. The user decided (2026-09-24) that VS Code shows those as real breakpoints: VS Code adopts
each announced breakpoint as a copy of its own (a red dot) and re-sends it, without its condition,
in later `setBreakpoints`; the facade recognises copies by column 1. What a human still lacks in
VS Code is a debug type to join or start a session, the lease (who has control, asking for it,
taking it), whose each red dot is and what it does, and the other clients' activity.

Two VS Code facts shape the design (read at tags 1.100.0 and 1.139.0, and observed in the
integration suite): an adopted copy is added without a model event, so it never appears in
`vscode.debug.breakpoints` (`debugSession.ts` `onDidBreakpoint`, `mainThreadDebugService.ts`); and a
decoration with a gutter icon on a line makes a gutter click there stop toggling breakpoints
(`breakpointEditorContribution.ts` `marginFreeFromNonDebugDecorations`).

The extension is also the first Node code in a Go repository: its supply chain (npm packages,
install scripts, a package manager) needs rules as strict as the Go side's.

## Decision Drivers

* No runtime dependencies: the VSIX holds the bundle, the manifest, the readme, the licence and
  the icon — nothing from npm.
* Dev dependencies few, exactly pinned, locked, and running no install scripts.
* The facade holds all policy (ADR 0012): the extension renders and asks; it never decides who may
  do what.
* Never corrupt a breakpoint: the extension adds nothing a copy could be confused with.
* No session text as markup: other clients' conditions, messages and names are untrusted.
* Windows first-class; no shell anywhere; the binary the extension runs can't be chosen by a
  cloned repository.
* Tests in real VS Code, at the supported floor and the latest release, never sleeping.

## Considered Options

* Toolchain: TypeScript 6 + esbuild + Biome + `node:test` · TypeScript 7 (native) · ESLint +
  typescript-eslint · webpack · `tsc` emit · mocha / `@vscode/test-cli`.
* Package manager: pnpm 11 with no dependency scripts · pnpm 12 · npm/yarn.
* pnpm in CI: corepack · `pnpm/action-setup`.
* Which `eyedbg`: machine-scoped `eyedbg.path`, else the extension's own PATH scan · a bundled
  binary · letting the process spawn search PATH.
* F5: `eyedbg start` then attach · a DAP `launch` through the facade.
* Other clients' breakpoints: end-of-line annotations tracked from DAP traffic · gutter icons ·
  `vscode.debug.breakpoints` + `getDebugProtocolBreakpoint` · annotating from `eyedbg/breakpoints`
  by canonical path · CodeLens.
* LEASE_HELD: a notice of the extension's own beside VS Code's · a facade flag hiding VS Code's ·
  an inline DAP proxy rewriting errors.
* CI: integration on three OSes on every run · on Linux only.

## Decision Outcome

Chosen: TypeScript 6 + esbuild + Biome with pnpm 11 and no dependency scripts, corepack in CI, a
machine-scoped path or the extension's own PATH scan, launch = `eyedbg start` + attach, annotations
from DAP traffic, the extension's own LEASE_HELD notice, and the integration suite on three OSes —
because together they ship zero npm code, run no install script, keep policy in the facade, touch
nothing VS Code uses to track breakpoints, and test what VS Code really does.

**Layout.** `extensions/vscode/` (`name: eyedbg`, `publisher: eyedebugger` — a placeholder until
publishing, `version: 0.0.0` until the lockstep release, `private: true`). `src/core/` is pure
(no `vscode` import; a unit test enforces it): protocol parsing, validation, identity, binary
resolution, CLI argv, the mirror model, lease decisions and every rendered string. `src/vscode/` is
the glue. `engines.vscode: ^1.100.0` with `@types/vscode` exactly 1.100.0, so every API used exists
at the floor. `extensionKind: ["workspace"]`; `untrustedWorkspaces: false` (in Restricted Mode the
extension doesn't activate, so an untrusted repository's launch.json can't make it run
`eyedbg start`, which builds and runs the repository's code); `virtualWorkspaces: false`.

**Dependencies** (all dev-only; none is shipped; licences from the registry):

| package | version | licence | why | install script |
|---|---|---|---|---|
| `typescript` | 6.0.3 | Apache-2.0 | type-check only (`tsc --noEmit`) | none |
| `esbuild` | 0.28.2 | MIT | bundles the extension and the tests | postinstall: **denied** (it finds its platform package at run time) |
| `@biomejs/biome` | 2.5.14 | MIT OR Apache-2.0 | lint + format | none |
| `@types/node` | 20.19.43 | MIT | Node 20 = VS Code 1.100's Electron 34 | none |
| `@types/vscode` | 1.100.0 | MIT | the API at the floor | none |
| `@vscode/vsce` | 4.0.0 | MIT | `vsce package`/`ls` | none |
| `@vscode/test-electron` | 3.1.0 | MIT | downloads and runs VS Code for the integration suite | none |

`pnpm-lock.yaml` locks 205 packages (about 150 install on one platform; the rest are other
platforms' binaries). Transitive licences (`pnpm licenses list`, macOS arm64): MIT 121, ISC 9,
Artistic-2.0 5 (`binaryextensions`, `editions`, `istextorbinary`, `textextensions`,
`version-range`, via vsce), BSD-2-Clause 3, BSD-3-Clause 2, Apache-2.0 2, BlueOak-1.0.0 2
(`minimatch`, `sax`), MIT OR Apache-2.0 2, MIT OR GPL-3.0-or-later 1 (`jszip`, via test-electron,
used under MIT), MIT AND Zlib 1 (`pako`), 0BSD 1 (`tslib`); the other platforms' `@esbuild/*` (MIT) and
`@biomejs/cli-*` (MIT OR Apache-2.0) packages share their parent's licence. The exception is
`@vscode/vsce-sign` 2.1.0 and its `@vscode/vsce-sign-*` binaries (2.0.6), which vsce requires: the
**Microsoft Software License Terms** (not OSI; use with Visual Studio products). They are dev and
CI tools only, never in the VSIX, and their postinstall (copying or downloading a signing binary
`vsce package` doesn't need) is **denied**.

**pnpm 11.27.1**, pinned with its sha512 in `packageManager` (corepack checks it; pnpm's own version switch
honours the version but not the hash). Settings live in `extensions/vscode/pnpm-workspace.yaml` (pnpm 11 reads none from
package.json): `allowBuilds: {esbuild: false, '@vscode/vsce-sign': false}` — no dependency runs an
install script, and `strictDepBuilds` (default true) fails any install that meets a new one until
someone decides; `minimumReleaseAge: 10080` (7 days, the repository's Dependabot cooldown);
`blockExoticSubdeps` and `trustLockfile: false` stay at their defaults. Every install is
`pnpm install --frozen-lockfile`. Not pnpm 12: its `packageManager` pin writes a two-document
lockfile that Dependabot (dependabot-core#15904) and `pnpm/action-setup` (#227) can't read. Not
`trustPolicy: no-downgrade`: it fails on `undici-types@6.21.0` (release-line publish order, a false
positive).

**CI.** Job `extension` on ubuntu, macOS and Windows, every run, in `ci-ok`: Node 24 LTS
(`actions/setup-node` v7.0.0, pinned by SHA, `package-manager-cache: false`), pnpm through
`corepack enable` (`COREPACK_ENABLE_DOWNLOAD_PROMPT=0`), then install, type-check, lint, unit
tests, `package` (the VSIX file list is checked) and the integration suite. No caches, no secrets.
If corepack fails on a runner, the fallback is `pnpm/action-setup` v6.1.0
(`ea17c68df8912ef543352723c149a84f56e3d413`, tag verified 2026-09-25) with `version: 11.27.1`.
Node 26 LTS (October 2026) no longer ships corepack: the setup changes then.

**Which `eyedbg`, and its arguments.** `eyedbg.path` has scope `machine`: only user (or remote
user) settings can set it, never a workspace. It must be absolute (a leading `~/` is the home
directory), an existing file, executable on Unix and an `.exe` on Windows (VS Code would run a
`.cmd`/`.bat` through cmd.exe). Without it the extension scans PATH itself — absolute entries
only, never the current directory, which Windows' process creation tries first (libuv
`search_path`) — for `eyedbg` (`eyedbg.exe` on Windows). The absolute result is what every
`execFile` (no shell) and the `DebugAdapterExecutable` get, checked once per path, size and mtime
with `eyedbg version --json` (`features` must hold `dap`, `presence`, `lease.request`,
`dap.collab`). No configuration property can name or influence the binary; the resolver deletes
`debugServer`, which would bypass the descriptor factory. Launch values are validated (session id
`^s-[a-z0-9]{1,32}$`, `lang` `^[a-z][a-z0-9_+-]{0,31}$`, option names, environment names without
`=`, no NUL, enums) and bound as `--flag=value`; program arguments follow `--`. The client is
`human:NAME` from the machine-scoped `eyedbg.clientName`, else the OS user name made valid.

**Debug type `eyedbg`.** `attach {session?}` joins a session (the only live one, or a pick; none is
an error). `launch {lang, program?, project?, args?, cwd?, env?, stopOnEntry?, noBuild?, opts?,
leasePolicy?, exceptions?}` runs `eyedbg start … --as=human:NAME --json` (cancellable) and becomes
an attach to the new session, remembered in memory under a random token put in the resolved
configuration (a launch.json can't forge it; the resolver deletes any given one). When a debug
session this window launched ends — unless VS Code is restarting it — the extension runs
`eyedbg stop`; joined sessions are only left. VS Code's Restart re-runs the same resolved
configuration, so it joins the same session again: the program isn't restarted. Dynamic
configurations list the running sessions ("Join s-7f3k — python app.py (agent has control)").

**Other clients' breakpoints.** VS Code already draws each mirror as a red dot. The extension adds,
at the end of the line, whose it is and what it does (`⬥ agent · if total > 3 · hit >= 5`), a mark
on the overview ruler, a plain-text hover, and two commands on the line-number context menu: *Copy
as My Breakpoint* (adds a breakpoint through the API at character 0 — no column, so the human's
own; the facade then retracts the copy and the line keeps the condition) and *Remove Breakpoint for
Everyone…* (after a modal confirmation, `eyedbg/breakpoints {action: remove, force}`). Deleting a
red dot stays VS Code's own gesture and only hides the mirror. The glyph margin is never touched: a
gutter icon would stop the human's clicks on that line from setting breakpoints. Because VS Code
hides its copies from the API, the extension follows the same DAP messages VS Code applies, through
a debug adapter tracker: `breakpoint new|changed` at column 1 sets a mirror, `removed` drops it,
each `setBreakpoints` response re-derives the mirrors of its path (answers at column 1 with a
positive id; a negative id is a retracted copy), a mirror's line is VS Code's (the adapter's once
verified, else the line VS Code had), and `terminated` or the `disconnect` response clears them.
Annotations match editors by `Uri.file(path)`, where VS Code keys the copy — not the canonical
path, which would put the annotation in a different tab than the red dot under a symlink.
`contributes.breakpoints` lists `csharp`, `python`, `c`, `cpp`, `rust` and `go`, so gutter clicks
work with no other extension installed.

**The lease.** A status bar item for the active eyedbg session (`$(hubot) agent · handoff · 1
request`); its menu offers what the session's rules allow now (take, take by force, request,
release, give to, policy). A request refused with `LEASE_HELD` (VS Code's step, continue, …) shows,
once per holder until the lease changes, "agent has control of s-7f3k (handoff)." with [Request
Control] [Take Over]; Take Over asks (modal) before forcing. Other clients' pending requests are
shown to the holder once each, with [Give Control] [Release]. After the human gets the lease from
an agent while the policy is `free`, `eyedbg.lease.afterTakeOver` (`ask`, default;
`humanPriority`; `keep`) offers or makes the switch to `human-priority` — under `free` the agent's
next run or step takes control back; under `human-priority` it has to ask (or force). Under
`handoff` and `human-priority` nothing is offered: the agent already can't take control back
without asking or forcing. The trigger is the lease moving from an agent to the human, whatever
moved it (an `eyedbg/lease` event doesn't say which action did); under `free` a grant and a take
are equally undone by the agent's next command.

**Untrusted text.** Everything a session sends is plain text. VS Code runs `command:` links in
notifications when clicked (`notifications.ts` `LINK_REGEX`), so every notification text passes
`notificationSafe()`, which leaves no `](`; hovers are `MarkdownString`s with `isTrusted`,
`supportHtml` and theme icons off, filled with `appendText`; annotations are decoration
`contentText`; control characters become spaces and lengths are capped; labels that render
`$(icon)` get it broken up. The "EyeDebugger" log channel records binaries, versions, commands
(with `--env` values redacted) and errors — never debuggee data; "EyeDebugger Activity" shows
other clients' actions as `eyedbg events` phrases them (expressions, never values).

**Exported API** (for tests; unstable, `apiVersion: 0`): read-only snapshots of the sessions,
annotations, notices and stops, and a change event. Nothing in it changes a session.

**Tests.** Unit: `node:test` on `src/core`, bundled by esbuild. Integration: a small in-host
harness on `@vscode/test-electron` runs I1–I9 (join and step, mirrors and the adopt-and-resend
round trip, disable/enable all, Restart, stale copies after a crash, a symlinked path, the lease,
F5, picking) in VS Code **1.100.0 and 1.139.0** against the real `eyedbg` and debugpy. Every run
gets a fresh temporary root; VS Code runs in portable mode inside it (its user data, extensions and
`argv.json` — which it otherwise reads and creates in `~/.vscode` — never touch a real profile),
and the runner refuses a directory inside one. Waits are on events with deadlines; a test that acts
on VS Code's breakpoints after seeing an adapter event first round-trips a no-op `eyedbg/clients`
request, because the workbench applies queued DAP messages one task apart
(`abstractDebugAdapter.ts`) and the tests see them earlier. To move the pinned VS Code versions,
edit `defaultVersions` in `test/integration/runner.ts` (`EYEDBG_TEST_VSCODE_VERSIONS` overrides it
for one run) and keep the floor equal to `engines.vscode` and `@types/vscode`.

### Consequences

* Good, because the VSIX ships no npm code and no dependency ran an install script to build it.
* Good, because policy stays in the facade: the extension's lease menu and prompts only render
  what the session answers, and every action goes through the same rules as the CLI.
* Good, because the agent's breakpoints look like breakpoints and say whose they are, without
  changing what a gutter click does.
* Bad, because a refused step shows two toasts: VS Code's own (the facade sets `showUser` on
  execution errors, and VS Code shows it) and the extension's with the actions.
* Bad, because Restart of a launched session re-joins it rather than restarting the program.
* Bad, because `task ci` now needs Node 24 and pnpm; `task ci:go` runs the Go checks alone.
* Bad, because `@vscode/vsce-sign` (Microsoft licence terms) is in the dev toolchain; every
  Marketplace packaging path requires vsce.
* Neutral, because corepack leaves Node with Node 26: CI's pnpm setup changes then.

### Confirmation

The unit suite (`pnpm test`), the integration suite (`pnpm run test:integration`, five
consecutive clean runs before merging an extension change), the VSIX file-list check in
`pnpm run package`, and the CI `extension` job on three operating systems.

## More Information

Supersedes nothing; builds on ADR 0012 and ADR 0014. Publishing (Marketplace and Open VSX,
secrets-gated) and the activity and clients views come later.
