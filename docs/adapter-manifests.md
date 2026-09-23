# Adapter manifests

An adapter manifest tells eyedbg how to find, install and run a debug adapter, and, for a language
that needs no Go code, how to launch programs with it. The decision and its trust model are in
[ADR 0011](adr/0011-declarative-adapter-manifests-and-their-trust-model.md); this page is the field
reference for manifest authors.

eyedbg ships two: [netcoredbg.json](../internal/adapters/manifests/netcoredbg.json) (dotnet, served by
the Go driver in `drivers/dotnet`) and [debugpy.json](../internal/adapters/manifests/debugpy.json)
(python, served entirely by the manifest). `eyedbg adapters ls` lists what is loaded.

## Where manifests come from

* **Bundled**: `internal/adapters/manifests/*.json`, built into eyedbg. A unit test checks each one.
* **Yours**: the `*.json` files directly in `$EYEDBG_CONFIG_DIR/adapters`, else
  `<config dir>/eyedbg/adapters`: `~/.config/eyedbg/adapters` on Linux (or
  `$XDG_CONFIG_HOME/eyedbg/adapters`), `~/Library/Application Support/eyedbg/adapters` on macOS,
  `%AppData%\eyedbg\adapters` on Windows. Subdirectories and other files are ignored; a manifest
  is at most 1 MiB.

A manifest of yours **replaces** the bundled one with the same `name` or the same language, whole
(no field merging); `adapters ls` shows what it replaces. Never from a project: a cloned
repository can't add manifests.

When a manifest of yours can't be used it is left out, the bundled one stays, and the reason is
listed as "ignored" by `eyedbg adapters ls` and as a problem by `eyedbg adapters doctor` (the
daemon logs it too):

* it is invalid (see below), or fails the permission check;
* two of yours share a name or a language (both are left out);
* it names a language a Go driver serves (dotnet) without `"builtin": true`, or sets `builtin` for a
  language no Go driver serves.

The daemon reads manifests when it starts: after changing one, run `eyedbg daemon stop` (the next
command starts a new daemon). `adapters` commands read them each time.

## Trust

A manifest names programs eyedbg runs and URLs it downloads, so a manifest of yours is trusted like
your shell configuration: whoever can write it can run code as you. eyedbg therefore:

* reads yours only from your config directory, never from a project;
* on Unix, refuses a manifest unless the manifest directory, its parent and the file (for a
  symlink, its target) are owned by you and writable by neither your group nor others, whatever
  the group (OpenSSH's rule; `chmod go-w FILE` fixes the usual problem), checking the file it
  actually opened; on Windows it relies on
  the per-user `%AppData%` permissions;
* runs nothing when loading, and never runs a command through a shell;
* downloads over https only, stops at `size` bytes (1 GiB without it), checks the SHA-256 before
  extracting, extracts into a staging directory and renames it into place, refusing archive
  entries that escape it;
* probes a Python interpreter with `-c`, after removing the working directory from `sys.path`, and
  never imports the adapter's package to find it.

## Fields (schema 1)

Unknown fields are errors, so a manifest for a newer schema fails with a clear message instead of
being half-read. Top level:

| field | | |
|---|---|---|
| `schema` | required | `1` |
| `name` | required | `^[a-z][a-z0-9-]{0,31}$`; also the install directory `<data dir>/<name>/<version>` |
| `description` | required | at most 120 characters |
| `homepage` | | https URL |
| `license` | | text |
| `version` | required | `^[0-9A-Za-z._+-]{1,40}$`: the release `adapters install` fetches |
| `adapter` | required | how the adapter runs (below) |
| `python` | iff `adapter.runtime` is `python` | interpreter and package (below) |
| `install` | | downloads (below) |
| `language` | | the language it debugs (below) |
| `options` | | `start --opt` options (generic languages only) |
| `launch` | iff the language isn't built in | launch template |
| `attach` | | attach template; without it `attach` is `UNSUPPORTED_BY_ADAPTER` |
| `attachUnsupported` | | that error's hint |
| `exceptions` | | exception modes → filter ids |
| `evalGuard` | | the eval side-effect check |

`adapter`:

| field | | |
|---|---|---|
| `id` | required | the DAP `adapterID` sent in `initialize` |
| `transport` | | `stdio` (the default; nothing else yet) |
| `runtime` | | `""` (a native executable) or `python` |
| `entry` | required | native: an executable name (`.exe` is added on Windows) or an absolute path; python: a slash path relative to the package root, no `..` (debugpy: `debugpy/adapter`) |
| `args` | | literal arguments |
| `environment` | | `{NAME: value}` added to the adapter's environment |
| `env` | native | an environment variable naming the executable, e.g. `EYEDBG_NETCOREDBG` |
| `path` | native | also look `entry` up on PATH |
| `versionArgs` | native | arguments that make it print its version, for `adapters doctor` |
| `notRunningHint` | | doctor's fix when it doesn't run |

A native adapter is found by `env`, then an absolute `entry`, then the installed copy, then PATH
(if `path`).

`python` (for `runtime: python`; the adapter runs as `<interpreter> <root>/<entry>`, and the
interpreter's path is `${runtime}` in templates):

| field | | |
|---|---|---|
| `commands` | required | interpreters tried on PATH, in order, as `[name, args...]`, e.g. `[["python3"], ["python"], ["py", "-3"]]` |
| `env` | | an environment variable naming the interpreter (the daemon's environment) |
| `option` | | a declared string option naming the interpreter (`--opt python=PATH`) |
| `venvs` | | project venv directory names, e.g. `[".venv", "venv"]` |
| `minVersion` | | `X.Y`: the oldest Python the downloaded package runs on |
| `module` | required | the import name of the package holding `entry` |

The interpreter is the `option`'s value (a relative path is taken from the directory eyedbg ran
in), else `env`'s, else a venv (a `venvs` directory holding `pyvenv.cfg`, in the working
directory, then the program's directory and its parents; `bin/python`, or `Scripts\python.exe` on
Windows), else the first of `commands` that runs. Only that last step moves on when an interpreter
fails. On Unix only directories you own are searched for a venv (the walk up stops at the first
one you don't), and a venv found must pass the manifest permission check (its directory, its
`bin` or `Scripts` directory and `pyvenv.cfg` yours and not writable by group or others), else the
start fails: a venv someone else controls is never run. The package root is the installed copy when it
exists and the interpreter meets `minVersion`, else the interpreter's own copy of `module`.

`install.downloads`: keys `linux`, `darwin` or `windows` / `amd64` or `arm64` (e.g.
`linux/amd64`), or `"*"` for any platform (a specific key wins). Each value:

| field | | |
|---|---|---|
| `url` | required | https |
| `sha256` | required | 64 lowercase hex digits |
| `archive` | required | `zip` or `tar.gz` |
| `root` | required | the archive directory that becomes the install directory; `""` for the whole archive |
| `size` | | bytes; the download stops beyond it (1 GiB without it) |

`language`: `name` (required, like `name`), `builtin` (a Go driver serves it: then no `options`,
`launch`, `attach`, `exceptions` or `evalGuard`), `extensions` (`[".py"]`) and `markers` (file names
or globs like `pyproject.toml`, `*.csproj`). `extensions` and `markers` are shown by `adapters ls
--json`; detecting the language from them is future work.

`options`: `{NAME: {type, default, help}}`; `NAME` is letters and digits; `type` is `string` (the
default), `bool` or `int`; `default` is written as a string (`"true"`, `"2"`); `help` is required.
`start --opt NAME=VALUE` sets them; an unknown one, or a value of the wrong type, is
`INVALID_REQUEST` listing the options.

`launch`: `require` (inputs of which at least one must be set: `program` for `--program`, or
`opt.NAME`) and `arguments` (the launch request body, a template). `attach`: `arguments` only.

`exceptions`: `{"all": [...], "uncaught": [...]}`, the adapter's exception filter ids for each
mode of `eyedbg bp exceptions` (`none` sets no filters). A mode left out uses its own name as the
filter id.

`evalGuard` (without it, eval checks nothing): `quotes` (single characters that start strings),
`interpolatedPrefixes` (string prefixes that make a string run code, compared case-insensitively,
e.g. `f`), `nonCallWords` (words a `(` may follow without being a call, e.g. `if`, `not`),
`safeCalls` (functions whose bare calls are allowed, e.g. `len`), `assignOps` (assignment
operators, from `=<>!:+-*/%&|^~@`). The check scans the expression's text: a `(` after a name (not a
`nonCallWords` one, nor a bare `safeCalls` one) or after `)` or `]` is a call, an operator that is
or starts one of `assignOps` (but not followed by `=`, so `==` isn't `=`) is an assignment, and an
interpolated prefix before a quote is an interpolated string; a tripled quote runs to the next
triple, and a string that doesn't end is a finding (the rest can't be checked). It is
best-effort: getters,
operators and dunder methods still run code.

## Templates

`launch.arguments` and `attach.arguments` are JSON with references to these variables:

| variable | type | value |
|---|---|---|
| `${program}` | string | `--program` (launch) |
| `${args}` | list | the program's arguments (after `--`) |
| `${cwd}` | string | `--cwd`, else the directory eyedbg was run in, else the program's directory |
| `${env}` | map | `--env` |
| `${stopOnEntry}` | bool | `--stop-on-entry` (always set) |
| `${runtime}` | string | the interpreter (python runtime only) |
| `${pid}` | int | the process to attach to (attach only) |
| `${opt.NAME}` | the option's type | the option's value, else its default |

Launch templates may use all but `${pid}`; attach templates `${pid}`, `${runtime}` and options
(their defaults). Rules:

* A string that is exactly `"${x}"` becomes x's typed value. If x is unset (an empty string, list
  or map, or an option with no value and no default) the object member or array element is
  **omitted**.
* A list reference that is an array element is spliced into the array: `["--", "${args}"]`.
* `${x}` inside other text is replaced by the value of a string or integer variable (unset: empty):
  `"run ${program}"`. Lists, maps and bools can only stand alone.
* No escapes, conditionals or functions. Object keys are literal. Every reference is checked when
  the manifest loads: an unknown variable, `${pid}` in `launch`, `${runtime}` without a Python
  runtime, or a malformed `${` makes the manifest invalid.

Literal values (numbers, booleans, objects) pass through unchanged.

## Adding a language

1. Write `<name>.json` in your manifest directory with `adapter`, `language` (not built in),
   `options` if the language needs any, and a `launch` template (start from debugpy.json); add
   `install.downloads` if eyedbg should download the adapter.
2. Run `eyedbg adapters ls` (it must be listed, not ignored), then `eyedbg adapters doctor <lang>`.
3. `eyedbg daemon stop`, then `eyedbg start <lang> --program FILE --bp FILE:LINE`.

A manifest for eyedbg itself goes in `internal/adapters/manifests/`, with an end-to-end test like
`drivers/generic/e2e_test.go`; nothing else in Go changes (registration is `generic.Drivers` in
`internal/cli`).

## Bumping a pinned adapter

Change `version` and each download's `url`, `sha256` (from the release page or `sha256sum` of the
file) and `size`, run the unit tests (the netcoredbg one pins its values on purpose: update it
too) and the adapter's end-to-end tests. The new version installs into its own directory; the old
one stays until removed by hand.

## Examples

debugpy's manifest, the reference for a generic language:

```json
{
  "schema": 1,
  "name": "debugpy",
  "description": "Python debugger by Microsoft (MIT); needs Python 3.10+",
  "homepage": "https://github.com/microsoft/debugpy",
  "license": "MIT",
  "version": "1.8.22",
  "adapter": {"id": "debugpy", "transport": "stdio", "runtime": "python", "entry": "debugpy/adapter"},
  "python": {
    "env": "EYEDBG_PYTHON", "option": "python",
    "commands": [["python3"], ["python"], ["py", "-3"]],
    "venvs": [".venv", "venv"], "minVersion": "3.10", "module": "debugpy"
  },
  "install": {"downloads": {"*": {
    "url": "https://files.pythonhosted.org/packages/56/3d/c7dc9f35bc9e22cdb53679f844bee40fd5cff871862f60595869a433400d/debugpy-1.8.22-py2.py3-none-any.whl",
    "sha256": "a9e9d3550e15ca479c59333e90845029190531f0cacfedab3b815a57bd913947",
    "size": 5374857, "archive": "zip", "root": ""
  }}},
  "language": {"name": "python", "extensions": [".py", ".pyw"],
               "markers": ["pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile"]},
  "options": {
    "module": {"help": "run this module like 'python -m MODULE' instead of --program (e.g. pytest)"},
    "python": {"help": "the Python 3.10+ interpreter that runs the program and debugpy"},
    "justMyCode": {"type": "bool", "default": "true", "help": "stop and step only in your own code; false also in libraries"}
  },
  "launch": {
    "require": ["program", "opt.module"],
    "arguments": {
      "name": "eyedbg", "type": "debugpy", "request": "launch",
      "program": "${program}", "module": "${opt.module}", "args": "${args}", "cwd": "${cwd}",
      "env": "${env}", "python": "${runtime}", "stopOnEntry": "${stopOnEntry}",
      "justMyCode": "${opt.justMyCode}", "console": "internalConsole", "redirectOutput": true,
      "subProcess": false, "showReturnValue": true,
      "variablePresentation": {"special": "hide", "function": "hide", "class": "group", "protected": "inline"}
    }
  },
  "attachUnsupported": "attaching to a running Python process needs gdb or lldb and isn't supported yet; start it under the debugger with 'eyedbg start python --program FILE'",
  "exceptions": {"all": ["raised", "uncaught"], "uncaught": ["uncaught", "userUnhandled"]},
  "evalGuard": {
    "quotes": ["'", "\""],
    "interpolatedPrefixes": ["f", "rf", "fr", "t", "rt", "tr"],
    "nonCallWords": ["and", "or", "not", "in", "is", "if", "else", "for"],
    "safeCalls": ["len", "str", "repr", "type", "isinstance", "issubclass", "id", "abs", "bool", "int",
                  "float", "hash", "hasattr", "getattr", "callable", "ord", "chr", "hex", "oct", "bin",
                  "round", "format", "vars", "dir"],
    "assignOps": ["=", ":="]
  }
}
```

netcoredbg's manifest (a built-in language: adapter metadata only) has `adapter` `{"id": "coreclr",
"entry": "netcoredbg", "args": ["--interpreter=vscode"], "env": "EYEDBG_NETCOREDBG", "path": true,
"versionArgs": ["--version"], ...}`, one download per platform with `"root": "netcoredbg"`, and
`"language": {"name": "dotnet", "builtin": true, ...}`.
