---
status: accepted
date: 2026-09-24
decision-makers: Ijat (@ijat)
---

# Declarative adapter manifests and their trust model

## Context and Problem Statement

Milestone 7 (docs/DESIGN.md §13) adds Python through debugpy, and must do it with a manifest alone
to prove the plugin boundary of DESIGN §7: a language that needs no Go code of its own. Until now
the one adapter, netcoredbg, was Go code: its pinned release, digests, lookup order and launch
arguments lived in `internal/adapters/netcoredbg.go` and `drivers/dotnet`. What format do adapter
manifests have, where do they come from and how much are the user's own trusted, how does a
Python-hosted adapter find its interpreter and package, how are launch arguments built from a
manifest, and what stays in Go?

## Decision Drivers

* Once users write manifests, the schema is a contract (fields can't change meaning).
* A manifest names commands eyedbg runs and URLs `adapters install` downloads: whoever can write a
  manifest eyedbg reads can run code as the user.
* debugpy 1.8.22 is a pure-Python package: its universal wheel
  (`debugpy-1.8.22-py2.py3-none-any.whl`, SHA-256 `a9e9d355…3947`) unzipped and run as
  `python <root>/debugpy/adapter` works on a Python without debugpy (checked on Debian 13,
  Python 3.13.5). Debian's system Python refuses `pip install` (PEP 668), and `venv` needs a
  separate `python3-venv` package there.
* debugpy's `python` launch argument makes the adapter start the program on that interpreter, which
  imports debugpy from the adapter's copy: one interpreter can run both.
* Without the client's `supportsStartDebuggingRequest`, debugpy announces child processes with a
  custom `debugpyAttach` event, and the child waits for a debugger until it is killed after 30 s;
  `subProcess: false` lets it run.
* debugpy attaches to a pid by injecting code with gdb or lldb (or Python 3.14's `sys.remote_exec`);
  on the test VM it failed after 40 s.
* debugpy's scopes are always Locals (presentationHint `locals`) and Globals, both cheap.
* JSON is already eyedbg's wire and output format; the standard library parses it strictly.

## Considered Options

Format:

* JSON, strict (unknown fields are errors), with a `schema` number (chosen).
* TOML.
* YAML.

Sources:

* Bundled manifests (embedded) plus the user's config directory; a user manifest replaces the
  bundled one of the same name or language (chosen).
* Also project-local manifests (`.eyedbg/adapters/` in the repository).
* Merged overlays of user fields onto bundled ones.

Trust:

* User manifests are user code, like shell rc files: read only from the user's config directory,
  permission-checked on Unix (chosen).
* Signed manifests.
* A command allowlist.

debugpy acquisition:

* The pinned universal wheel, size- and digest-checked, unzipped by `adapters install`; else the
  interpreter's own debugpy (chosen).
* Instructions only (`pip install debugpy`).
* A venv with pip.
* Per-platform binary wheels.

Launch arguments:

* JSON templates with `${var}` references and fixed rules (chosen).
* Go `text/template`.
* JSON patches over a default.

## Decision Outcome

**Manifests are strict JSON, schema 1.** One manifest per adapter: `name`, `version` (the release
`adapters install` fetches), `adapter` (DAP `id`, `transport` stdio, `runtime` native or python,
`entry`, `args`, `environment`; for a native adapter an `env` override variable, PATH lookup and a
`versionArgs` probe), `python` (interpreter commands, env variable, option, venv names,
`minVersion`, `module`), `install.downloads` (per `os/arch` or `"*"`: https `url`, `sha256`,
`archive` zip or tar.gz, `root`, `size`), and an optional `language`. A language is either
`builtin` (a Go driver serves it; the manifest carries adapter metadata only: netcoredbg for
dotnet) or served by the generic driver, which then needs `launch` (and may have `options`,
`attach`, `attachUnsupported`, `exceptions` and `evalGuard`). Unknown fields are errors; a newer
schema is a clear error, not a silent partial read. docs/adapter-manifests.md is the reference.

**Two sources, replacement not merging.** Bundled manifests are embedded
(`internal/adapters/manifests/`). User manifests are the `*.json` files directly in
`$EYEDBG_CONFIG_DIR/adapters`, else `<home>/adapters` (`~/.eyedbg` by default, `$EYEDBG_HOME`
overrides it — the same on every OS). A user manifest replaces,
whole, the bundled one with its name or language. Loading never fails: an invalid or untrusted user
manifest is left out and reported (`adapters ls`, `adapters doctor`, the daemon's log), and the
bundled one stays; two user manifests with one name or language are both left out; only a
`builtin` manifest may name a Go driver's language, and a `builtin` one needs such a driver. The
daemon reads manifests when it starts; `adapters` commands read them each time.

**User manifests are trusted like shell configuration.** They are never read from a project. On
Unix the manifest directory, its parent and each file (symlinks at their target) must be owned by
the user and writable by neither group nor others, whatever the group (OpenSSH's `StrictModes`
rule, `st_mode & 022`: a primary group need not be private, e.g. macOS's shared `staff`); the file
is checked through the handle that is then read, so a symlink target can't be swapped between
check and read. On Windows the per-user profile directory's ACL is relied on. Nothing
in a manifest runs when it is loaded; argv never goes through a shell; downloads are https only,
capped at `size` (else 1 GiB) and verified by SHA-256 before extraction, into a staging directory
renamed into place.

**Two adapter runtimes, one download mechanism.** A native adapter is found by its env variable,
then an absolute `entry`, then the installed copy (`<data dir>/<name>/<version>/`, `~/.eyedbg/tools`
by default, `$EYEDBG_DATA_DIR` overrides it), then PATH
(netcoredbg's order and messages, unchanged). A Python adapter runs as `<interpreter>
<root>/<entry>` on one interpreter chosen by `--opt python`, then the manifest's env variable
(`EYEDBG_PYTHON`; a relative `--opt python` path is taken from the caller's directory), then the
caller's `$VIRTUAL_ENV` (the CLI reads its own `$VIRTUAL_ENV` and sends it in the start request,
the same way it sends the client's directory, since the daemon's environment is fixed at daemon
start and would otherwise miss whichever venv the caller had active), then a project venv (`.venv`
or `venv` with `pyvenv.cfg`, in the working directory, then the program's directory and its
parents, searching only directories the user owns and running a venv only if it passes the same
permission check as manifests — `$VIRTUAL_ENV` included), then the commands on PATH (`python3`,
`python`, `py -3`); only PATH candidates fall through a failed probe. The probe runs the
interpreter with `-c` and the package name as an argument, drops the working directory from
`sys.path` first and never imports the package. The root is the installed copy (when the
interpreter meets `minVersion`), else the interpreter's own package. debugpy's manifest pins the
universal wheel.

**Templates.** Launch and attach arguments are JSON with `${program}`, `${args}`, `${cwd}`,
`${env}`, `${stopOnEntry}`, `${runtime}`, `${pid}` (attach only) and `${opt.NAME}`. A string that is
exactly `${x}` becomes x's typed value and is omitted when x is unset; a list inside an array is
spliced; other text interpolates strings and integers. No escapes, conditionals or functions.
References are checked when the manifest loads.

**No capability overrides in schema 1.** Honoring them needs a session hook; neither adapter needs
one (debugpy's capabilities are accurate for what eyedbg uses). Unknown fields being errors, adding
`capabilities` later is explicit.

What stays in Go, and why: the schema, loader, trust check, template renderer and downloader
(the boundary itself); the Python runtime (interpreter discovery is ecosystem knowledge, reusable by
any Python-hosted adapter, but names no adapter); the generic driver and its eval-guard tokenizer
(the rules are manifest data); `--opt`, the client's directory and its `$VIRTUAL_ENV` in the start
request; and one
session change, in DAP's terms rather than debugpy's: stop snapshots read only the scopes the
adapter marks `presentationHint: "locals"` when it marks any.

### Consequences

* Good, because Python needed no language-specific Go: every debugpy fact (package, entry, filters,
  launch keys, wheel URL and digest) is in `debugpy.json`, and a test adds and debugs a language
  that exists only as a manifest written at test time.
* Good, because netcoredbg's pins moved into JSON without changing its lookup, install or doctor
  behavior, and a user can repin or replace an adapter without rebuilding eyedbg.
* Good, because `eyedbg adapters install python` works on a machine whose system Python refuses
  pip, with nothing but the interpreter.
* Bad, because a user manifest runs commands by design: anyone who can write the user's config
  directory can run code as the user (as with their shell configuration).
* Bad, because JSON has no comments (`description` is the only prose).
* Bad, because the downloaded debugpy has no compiled speedups (tracing may be slower than a
  pip-installed one; not measured).
* Bad, because Python child processes aren't debugged and `attach` for Python is refused
  (`UNSUPPORTED_BY_ADAPTER`) for now.
* Bad, because a manifest directory or venv made with umask 002 (group-writable) is refused until
  `chmod go-w`.

### Confirmation

Unit tests in `internal/adapters` (schema validation, templates, registry precedence and
conflicts, the permission check with injected and real files, find order, install from a local TLS
server, interpreter discovery with fakes, the real probe ignoring files planted in the working
directory, and a table pinning netcoredbg's manifest to the constants it replaced); fake-adapter
tests in `drivers/generic` debugging a manifest-only language through `session.Manager` and in
`internal/cli` through the real commands and an in-process daemon; and the real debugpy end-to-end
tests in `drivers/generic` (`TestPython*`, including `TestPythonManagedInstall`).

## Pros and Cons of the Options

### TOML or YAML

* Good, because both allow comments.
* Bad, because both are new dependencies (AGENTS.md rule 2), and YAML types values implicitly.

### Project-local manifests

* Bad, because cloning a repository would let it choose the commands eyedbg runs.

### Merged overlays

* Bad, because a user overriding one field would keep a bundled download pinned to another version.

### Signed manifests or a command allowlist

* Bad, signing: there is no key infrastructure, and a same-user attacker can change the environment
  (PATH, `EYEDBG_*`) anyway. Bad, an allowlist: it defeats the plugin model.

### Instructions only, or pip into a venv

* Bad, because every first Python session would start with a pip dance, which PEP 668 blocks for
  system interpreters and `venv` needs an extra package for on Debian and Ubuntu.

### Per-platform binary wheels

* Bad, because they form an OS × architecture × Python-version matrix that a manifest's download
  keys can't express.

### Go `text/template` or JSON patches

* Bad, `text/template`: conditionals and functions make templates a language. Bad, patches:
  removing an unset value can't be expressed.

## More Information

Planned in `m7-debugpy` (plan decisions D1–D6, D9, D16). docs/adapter-manifests.md is the field
reference. Follow-ups: language detection from `extensions` and `markers`; capability overrides;
Python attach (gdb/lldb, `sys.remote_exec`) and child sessions for subprocesses; reloading
manifests without restarting the daemon; moving dotnet's side-effect check onto the manifest
guard; macOS and Windows Python end-to-end runs.
