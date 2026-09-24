---
status: accepted
date: 2026-09-24
decision-makers: Ijat (@ijat)
---

# A socket transport and three more manifest-only native languages

## Context and Problem Statement

ADR 0011 gave eyedbg declarative adapter manifests for a language served entirely by data, but
schema 1's transport is stdio only and its templates carry no launch-time value but the program's
own inputs. Nine more languages were candidates for this task (C, C++, Rust, Go, Ruby, Java,
Kotlin, JS/TS, PHP). Verified against each adapter's own source and release assets (not memory):
lldb-dap (LLVM, C/C++/Rust) speaks DAP on stdio by default, so it fits schema 1 as written, once
its `env` argument's shape is fixed for LLVM 18/19 (an array of `"NAME=VALUE"` strings; object or
array only from LLVM 20). Delve's `dlv dap` (Go) never speaks stdio: it only starts a TCP or Unix
socket server and waits for a client to dial in (Unix sockets only since Delve 1.22.1, and only
since 1.24.0 as the *dialing* side, `--client-addr`). Ruby's `rdbg`, Java's java-debug (an LSP
client driving jdt.ls, not a DAP adapter by itself), the unmaintained kotlin-debug-adapter,
vscode-js-debug (a `node` process whose real targets arrive as `startDebugging` child sessions) and
vscode-php-debug (`node`, plus its own literal `${port}` placeholder) each need strictly more than
a transport: a socket direction schema 1 doesn't have, an "adapter is the debuggee" launch model, an
LSP client, a `node` runtime, or multi-connection sessions. What, if anything, should this task add
to the schema and to `internal/session` so C, C++, Rust and Go can be manifest-only languages, and
what should the remaining five become instead?

## Decision Drivers

* A manifest names commands eyedbg runs and, now, whether eyedbg opens a listening socket: the
  trust model (ADR 0011 — same-user manifests, no shell, https-only downloads) must not weaken.
* Schema 1's promise is that an addition an older eyedbg doesn't understand fails clearly (unknown
  transport, unknown `${var}`), not silently: bundled manifests ship with their own eyedbg, so this
  only matters for a user manifest written against a newer eyedbg.
* `adapter.args` were, until now, literal and never rendered: adding one templated reference to
  them is a narrow, auditable change, not a general templating pass over argv.
* Go is the most valuable of the nine (verified: Delve is actively maintained, ubiquitous, and the
  language itself is heavily used for exactly the kind of service eyedbg's own agent-facing niche
  targets), and Q1 confirmed keeping it in this task rather than descoping it alongside the other
  five.
* `task e2e` must still fail, not skip, when a language it is asked to exercise has no toolchain;
  narrowing which languages a given machine or CI runner exercises must be explicit (Q2).

## Considered Options

Socket direction:

* The adapter dials in to a listener eyedbg creates first, so there is never a connect-retry loop
  on eyedbg's side (chosen: `adapter.transport: "connect"`).
* eyedbg dials an adapter that listens on a path or port it announces; fits Ruby's `rdbg` and
  vscode-js-debug later, but neither needs only a transport (rdbg is "adapter is the debuggee";
  js-debug needs child sessions), so this is deferred to whichever follow-up takes one of them on.
* Plain TCP on localhost.

C/C++/Rust as one manifest, or three:

* Three manifests over one executable (`lldb-dap-c`, `lldb-dap-cpp`, `lldb-dap-rust`), one language
  each (chosen).
* A new `language.aliases` field letting one manifest answer to several language names.
* A neutral shared language name (`lldb`) that all three extensions map to.

lldb-dap's `--env` shape:

* A new template variable, `${envList}` (a sorted `"NAME=VALUE"` list), used only where a manifest's
  launch `env` needs it (chosen).
* Require lldb-dap 20+, which reads `env` as an object like every other adapter.
* A managed download of lldb-dap.

End-to-end language selection:

* `EYEDBG_E2E_LANGS` (comma-separated; unset runs every language) (chosen, Q2).
* Per-language opt-in variables (`EYEDBG_E2E_GO=1`, …), leaving a new language untested by default.

## Decision Outcome

**`adapter.transport` gains `"connect"`.** eyedbg creates a fresh, private directory
(`os.MkdirTemp`, entered by no one but the user — Unix mode bits, and on Windows the same ACL trust
already relied on for the daemon's own socket, docs/DESIGN.md §14 "AF_UNIX everywhere"), listens on
a Unix socket inside it, starts the adapter with `adapter.args` rendered against one new reference,
`${socket}` (its path), accepts exactly one connection, then closes the listener and removes the
directory — on every return path, so a failed or hung adapter leaves nothing running or reachable.
`connect` is refused with any `runtime` but native, and `adapter.args` must contain `${socket}` at
least once and no other reference (`checkString` reused with a `{socket: string}` kind map);
`${socket}` in a `stdio` adapter's args is refused too, naming the fix. Delve's manifest:
`"args": ["dap", "--client-addr=unix:${socket}"]`. `internal/session` gained the mechanics
(`Launch.SocketArgs func(socket string) []string`, non-nil selects the transport so
`internal/session` itself carries no template syntax or language knowledge) and the generic driver
wires a `connect`-transport manifest into it instead of `Launch.AdapterArgs`.

**`${envList}` is a new launch-only template variable**: `--env` as a list of sorted
`"NAME=VALUE"` strings, for an adapter (lldb-dap ≤19) whose launch `env` argument is an array, not
an object. Additive: existing manifests are unaffected, and it is checked like every other
reference (unknown-variable and "can only stand alone" rules apply) so a manifest that misuses it
is refused, not silently wrong.

**C, C++ and Rust are one executable, three manifests, three languages**: `lldb-dap-c`,
`lldb-dap-cpp`, `lldb-dap-rust`, each `entry: lldb-dap`, `env: EYEDBG_LLDB_DAP`, `path: true`,
`args: ["--repl-mode", "variable"]` (the repl evaluates expressions, never LLDB commands),
`versionArgs: ["--help"]` (`--version` only exists from LLVM 21). C++ alone sets
`exceptions.all: ["cpp_throw"]` (lldb-dap has no uncaught-only filter, so `uncaught` is left unset
and refused with the adapter's own filter list); each has its own `evalGuard` word lists
(`decltype`/`typeid` for C++; no `++`/`--` for Rust). No `install`: LLVM's own release archives are
0.25–1.8 GB `.tar.xz`/`.tar.zst`, over the unconfigured 1 GiB cap and needing a decompression
dependency this task doesn't justify (AGENTS.md rule 2); `EYEDBG_LLDB_DAP` or PATH is how users
point eyedbg at whichever copy their OS or toolchain ships (macOS Command Line Tools, distro
`lldb-dap-NN` packages).

**Go is `delve.json`, pinned to Delve 1.27.2**, through the unchanged installer (downloads,
SHA-256, staging, size cap): `linux/{amd64,arm64}`, `darwin/{amd64,arm64}`, `windows/amd64` (Delve
publishes no others for this release). `launch.arguments` render `mode` (`debug`/`exec`/`test`) and
`buildFlags` from declared `--opt` options alongside the schema's own `${program}`/`${args}`/
`${cwd}`/`${env}`/`${stopOnEntry}`; exceptions map both modes to `unrecovered-panic` and
`runtime-fatal-throw` (Delve has no separate "recovered panic" filter); `evalGuard.safeCalls`
allow-lists Go's built-in and conversion functions (`len`, `cap`, `int`, `string`, …), since a bare
call otherwise looks identical to one with real side effects to the guard's tokenizer.

**`EYEDBG_E2E_LANGS`** (comma-separated; unset runs every language) narrows a real-adapter test run
without ever turning a listed language's failure into a skip: a language left *out* skips with a
message naming the variable; a language left *in* whose toolchain is missing fails, exactly as
before this task. This lets Windows CI (no lldb-dap story yet) and a developer machine missing one
toolchain narrow a run explicitly, while `task e2e` unnarrowed still means "every language, no
excuses."

**Ruby, Java, Kotlin, JS/TS and PHP are descoped**, each to its own follow-up (below): none is
manifest-only under schema 1 even with this task's additions, and each needs strictly more new
mechanism (a listen-direction transport and an "adapter is the debuggee" launch model for Ruby; an
LSP client and a dedicated driver for Java; Kotlin waits on deciding between an unmaintained
adapter and Java's stack; a `node` runtime and multi-connection sessions for JS/TS; `node` and a
template escape for PHP's own `${port}` syntax).

### Consequences

* Good, because Go — the most requested and, once Delve's transport was understood, most tractable
  of the nine — is manifest-only: no new Go driver, no dependency, the same trust model.
* Good, because `${socket}`'s validation is narrow and reused (`checkString`, the existing
  reference-checking machinery) rather than a new argv-templating surface.
* Good, because the connect transport is reusable: any future adapter that only listens (in this
  direction) needs no more session code, only a manifest and, for a `node`-hosted one, a runtime
  this task didn't need to add.
* Bad, because a private Unix socket per session is a new local IPC surface, however narrow (see
  ADR-level security review of `internal/session/adapter_socket.go`, `validate.go`, `template.go`).
* Bad, because lldb-dap being rarely on PATH by default (macOS CLT, distro `lldb-dap-NN`) means
  `adapters doctor` reports it missing on a stock machine until `EYEDBG_LLDB_DAP` is set.
* Bad, because C++ `--exceptions uncaught` and Rust panics have no adapter-side filter: documented
  as a known limitation, not fixed here.
* Bad, because lldb-dap breakpoints for c/cpp/rust never bind when the build/working directory is
  reached through a symlink (e.g. macOS's `/tmp`, `/var`): lldb-dap matches the compiler's own
  unresolved path, while eyedbg resolves symlinks before sending a breakpoint. debugpy, netcoredbg
  and Delve are not affected. Known limitation, not fixed here.
* Bad, because Delve publishes binaries only for some releases (not 1.27.0 or 1.27.1): a future
  version bump may have to skip a release entirely.
* Bad, because Delve dialing out over `AF_UNIX` on Windows (Go has supported it since Windows 10
  1803, and Delve's own dial call is unconditional) is unverified at the time of this decision;
  CI's Windows matrix entry now exercises it for the first time instead of a controlled test.

### Confirmation

Unit tests in `internal/adapters` (schema validation table rows for every new `connect`/`${socket}`
rule, `${envList}` rendering, the three lldb-dap manifests and delve.json pinned against known
values, a check that no two bundled manifests share a name or language); `internal/session`
(`-race -count=5`, a full round trip over the socket, an adapter that exits or hangs before
connecting, a socket path too long for `sun_path`); `drivers/generic` (the socket-transport wiring
into `Launch.SocketArgs`, `${envList}` rendering); `internal/cli`
(`TestManifestLanguageConnectCLI`, a full breakpoint/vars/eval/continue round trip over the connect
transport against the fake adapter, driven through the real commands). Real-adapter end-to-end
tests in `drivers/generic` (`TestLldb*`/`TestCpp*`/`TestCAttach` for C/C++/Rust; `TestGo*` for Go,
run here against a locally built Delve 1.27.2 including the managed-install path — a genuine, not
merely structural, first exercise of the connect transport against its intended adapter) and
`internal/e2e` (`TestCLI/c`, `TestCLI/go`).

## Pros and Cons of the Options

### eyedbg dials an adapter that listens

* Good, because it is the direction Ruby's `rdbg` and vscode-js-debug's `dapDebugServer.js` need.
* Bad, because it needs a connect-retry loop (the adapter's listener isn't guaranteed ready when
  eyedbg starts trying), and neither of its two real users is otherwise servable with a transport
  alone — deferred to whichever follow-up (F1 or F4) takes one of them on, as `"transport": "listen"`.

### Plain TCP on localhost

* Bad, because any local user could race to connect and drive the debugger as the session's owner;
  Delve's own same-user guard (`--only-same-user`) is Linux-only in its current source
  (`//go:build !linux` defaults it to always-allow), so it cannot be relied on. A private Unix
  socket, reachable by construction only by the same user, keeps ADR 0011's trust model instead of
  reopening it.

### A new `language.aliases` field, or a neutral `lldb` language name

* Bad, aliases: touches registry precedence and conflict-checking code and `adapters ls` output for
  a cosmetic saving, and still can't give C++ its own exception filter or eval-guard words without
  extra structure.
* Bad, a neutral name: users and agents type `rust`, `c` and `cpp`, not `lldb`; `adapters ls` would
  need to show the source extension anyway to tell them apart.

### Require lldb-dap 20+

* Bad, because the lldb-dap versions distro packages actually ship (18 on Ubuntu 24.04, 19 on
  Debian 13) would silently drop `--env`, which is worse than a template variable scoped to the
  manifests that need it.

### A managed lldb-dap download

* Bad, because LLVM's release archives are `.tar.xz`/`.tar.zst`, 0.25–1.8 GB: over the manifest
  installer's unconfigured 1 GiB cap and needing a new decompression dependency this task has no
  independent justification for (AGENTS.md rule 2).

### Per-language e2e opt-in variables

* Bad, because a newly added language would be untested by anyone not already opted in, the
  opposite of `EYEDBG_E2E_LANGS`'s "everything unless narrowed."

## More Information

Planned in `lang-manifests` (this task). docs/adapter-manifests.md documents the transport and
`${socket}`/`${envList}` for manifest authors. Follow-ups, each its own task:

* **F1 Ruby** (`rdbg`): a listen-direction transport (`"transport": "listen"`) plus an
  "adapter is the debuggee" launch model (program, args, cwd and env go on *its* command line, its
  stdout is the program's output); `rdbg`'s own output forwarding is unverified.
* **F2 Java** (java-debug): a Go driver hosting an LSP client that starts jdt.ls with the
  java-debug bundle and gets a DAP session over TCP from it; its own ADR (a driver, not a manifest).
* **F3 Kotlin**: decide between the unmaintained kotlin-debug-adapter (needs a relative native entry
  inside its own install directory, which schema 1 doesn't support, plus a Windows `.bat` schema 1
  can't name) and riding on F2's stack instead.
* **F4 JS/TS** (vscode-js-debug): a `node` runtime, a listen-direction transport (shared with F1),
  and multi-connection sessions in `internal/session` for `startDebugging` child sessions — its own
  ADR, the largest of the five.
* **F5 PHP** (vscode-php-debug): a `node` runtime (shared with F4), a template escape so
  php-debug's own literal `${port}` placeholder survives eyedbg's `${...}` syntax, and Xdebug as a
  user-side prerequisite.
* **F6 lldb-dap discovery**: an `adapter.candidates` fallback list (`lldb-dap-19`, `lldb-dap-18`,
  the macOS Command Line Tools path) so `EYEDBG_LLDB_DAP` isn't the only way past a bare `path`
  lookup finding nothing.
* **F7 Rust formatters as an option**: once templates can omit an unset interpolated string, offer
  `~/.lldbinit`'s two `command script import`/`type synthetic add` lines (from `rustc --print
  sysroot`) as a manifest-driven default instead of a documented manual step.
