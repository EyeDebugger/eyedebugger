---
status: proposed
date: 2026-10-02
decision-makers: Ijat (@ijat)
---

# Fast mode: host Debug builds launched under the debugger in compose services

## Context and Problem Statement

[ADR 0020](0020-debug-services-in-containers-and-compose-groups.md) lets eyedbg attach to the .NET process of a running container. That is not enough for the
Visual Studio experience: a typical Visual Studio-template Dockerfile publishes `-c Release` (some
hardcode it; the current template parameterises it as `ARG BUILD_CONFIGURATION=Release`), and
**attaching to such an image never binds a line breakpoint** (ADR 0020, Context; verified on macOS
and Linux, with `justMyCode` on and off). netcoredbg can only disable JIT optimisation at module
load, which an attach has already missed.

Visual Studio's and JetBrains Rider's "fast mode" solve this without touching the Dockerfile: they
build the Debug configuration on the host, run the service's container with the output mounted, and
launch the app **under the debugger inside the container** (Visual Studio: the container idles on
`tail -f /dev/null` and the debugger starts the app through an exec; Rider starts its own debugger
worker as pid 1; Visual Studio's behaviour is as its documentation describes it, Rider's was read
from a stack it created). The effect is IDE parity: real Debug code, breakpoints in startup code,
rebuilds that don't rebuild an image.

How does eyedbg offer that, from the CLI, without editing the user's Dockerfile or compose files,
without ever reading the container's environment, and with the stack otherwise left alone?

Facts verified for this decision (macOS Docker Desktop 24.0.7 / compose 2.23.3 on arm64; Debian 13
docker.io 26.1.5 / compose 2.26.1 on amd64; scratch compose projects):

* **Launch inside the container works.** netcoredbg, copied in and run as `docker exec -i
  <container> /.eyedbg-netcoredbg-<v>/netcoredbg --interpreter=vscode` (no `-e`, `-u`, `-w`, `-t`),
  accepts a DAP `launch` with `program: /app/X.dll` and no `env`: it runs `dotnet /app/X.dll`,
  finding `dotnet` on the container's `PATH` (a fallback form with the executable's absolute path
  also works, and is unneeded). A breakpoint set before `configurationDone` — on a startup line —
  is hit. `stopAtEntry: true` stops with reason `entry`. The `process` event carries the
  in-container pid. Program output arrives as DAP `output` events; `docker logs` shows none of it.
  `launch` answers `success` even when the app can't load (the failure shows only as output and
  exit events).
* **The exec's environment is the container's own.** An exec without `-e` carries pid 1's configured
  environment (image `ENV`, compose `environment:` and `env_file:`); netcoredbg passes its own
  environment on to the app unless the request has `env` (moby `daemon/exec.go`,
  `CreateDaemonEnvironment`; netcoredbg `Launch`, read in source). Measured: the launched app's
  sorted variable **names** equal pid 1's as built, for all three test services, with scratch
  canary values from both `environment:` and `env_file:` reaching the app.
* **User, working directory and ports are the container's.** The exec runs as the image's `USER`
  (uid 1654 for a non-root service), `cwd` is the working dir (the mount), published ports reach the
  launched app, and ASP.NET Core serves with a read-only content root at `/app`.
* **The idle container** — `entrypoint: ["tail", "-f", "/dev/null"]`, `command: []` (empties the
  CMD the image or a compose `command:` supplied), `init: true` — comes up in 0.6 s and `compose
  stop` takes 0.2 s with `init` against 10.2 s without (measured on scratch alpine containers: as pid 1,
`tail` has no default SIGTERM handler, so docker waits out the grace period). `docker-init`
  is present on both engines. `docker exec <as-built> tail --version` succeeds on `runtime:10.0` and
  `aspnet:10.0` (and root and non-root) and fails with `exec: "tail": executable file not found` on
  `runtime:10.0-noble-chiseled`.
* **Health.** An idle container's healthcheck, left as is, goes `unhealthy` about 15 s after the app
  is gone; a launched serving app is `healthy`. `up --wait` on an idle service fails (`container … is
  unhealthy`). While idle, a published port's proxy accepts and then closes or resets the
  connection (an empty reply on Docker Desktop, a reset on Linux), it doesn't refuse.
* **Terminate and loss.** `disconnect {terminateDebuggee: true}` leaves only `docker-init` and
  `tail`, within 3 s. A killed host `docker exec` leaves netcoredbg **and the app** frozen in the
  container, indefinitely. `compose stop` of a container under a session ends the host exec at
  once (exit 137) with no `terminated` event. `docker restart` of an idle fast container clears
  strays and keeps the copied adapter and the mount.
* **Host publish.** `dotnet publish <project> -c Debug -o <stage> --artifacts-path <dir>
  -p:UseAppHost=false -p:DebugType=portable -p:PathMap=<project root>/=/src/ -nologo -tl:off` on the
  host SDK 10.0.401 writes no `bin/` or `obj/` into the project tree, works with a root directory
  containing a space and `$`, takes 0.7–3.4 s on a small app (warm), and yields every runtime's
  natives under `runtimes/` (`linux-arm64` and `linux-x64` `librdkafka.so`), so one build serves
  amd64 and arm64 containers. Breakpoints at `/src/<Project>/…` bind in that build. Roslyn's `PathMap`
  applies the first matching key as an ordinal prefix and, with a replacement holding `/` and no
  `\`, rewrites every `\` in the result to `/` (so Windows-host PDBs name `/src/…` too; unverified on
  Windows). MSBuild's `-p:` splits a value on `,` and `;` and unescapes `%3B`, so such characters in
  the root or project path are not safe to pass.
* **Rebuild without recreate.** With the app running, publishing to a staging directory leaves it
  alone; terminate, mirror (rename-replace each file, delete files not in staging) and relaunch shows
  the new build in the same container in 0.1 s of mirroring; total about 2.2 s on macOS and 3.8 s
  on Linux (through an SDK container) for a small app. A mirror made under `umask 077` creates
  0700 directories that a non-root container user can't traverse: the app then dies at load with
  `realpath(/app/X.dll) failed: Permission denied`, visible only as output events. Normalising the
  mirrored **destination** (directories 0755, files `go+r`) fixes it.
* **Entering and leaving.** `up -d --no-deps --force-recreate --no-build` with a JSON override file
  (a `#` comment line followed by JSON, named `.yml`) recreates the service in 0.3–0.6 s;
  `command: []` replaces a base `command:`; `config --hash` of a service is unaffected by adding
  another service's fragment to the same override. With only the user's files and `--wait`, a
  recreate restores the image's entrypoint, CMD, no mounts, `init` unset, and the Release build
  (about 6 s with the healthcheck). A missing bind source with `create_host_path: false` makes `up`
  fail, leaving the **old container running** under its name.
* **`docker compose config` leaks secrets.** On compose 2.23.3 it inlines `env_file` values into
  `environment`, also with `--no-interpolate`.
* **The drift check can't use `config --hash`.** The container's `com.docker.compose.config-hash`
  label differs from `config --hash` for (a) every service with an `env_file`, even one created by a
  plain `up -d`, and (b) any service created with `--no-deps` that has `depends_on` (the label is
  computed without the dependency). Both reproduce on compose 2.23.3 and 2.26.1. It matches only for
  a plain `up` of a service without `env_file`.
* **Rider's override on a real stack** was read for its shape (keys and names only, never values):
  `command: []`, an 8-element entrypoint starting at the IDE's debugger worker, the project mounted at
  `/app` read-write, a few added variables, the healthcheck untouched. Such a container fails fast
  mode's entrypoint shape check and is refused until a plain `docker compose up -d` recreates it.

## Decision Drivers

* No Dockerfile or compose file edits; the stack keeps running as it did before eyedbg touched it.
* Real Debug code with startup-code breakpoints, as Visual Studio and Rider give.
* Secrets (environment, `env_file` values, arguments) never read by eyedbg, passed along or logged.
* Windows hosts first-class in the code, even though they can't be tested here.
* Reuse the attach path of ADR 0020 (adapter delivery, path map, sessions) rather than a second one.
* Anything eyedbg deletes or overwrites is eyedbg's own, by name; nothing reaches outside its home.
* Safe to re-run: a failed step leaves the previous state or says exactly what changed.

## Considered Options

* As-built attach only (ADR 0020), documenting a `BUILD_CONFIGURATION=Debug` rebuild.
* **FM-A** — run the Debug build as the container's main process (override entrypoint
  `dotnet /…/X.dll`) and attach after start.
* **FM-B** — the container idles with the Debug build mounted; eyedbg launches the app in it under
  the debugger.
* An IDE-style debugger worker as pid 1.
* Rewriting the Dockerfile through `dockerfile_inline`.
* A `BUILD_CONFIGURATION` build-arg override.

## Decision Outcome

Chosen option: **FM-B**, because it is how Visual Studio and Rider do it: breakpoints in startup
code, no recreate per rebuild, and the launched app inherits the container's own environment, user
and working directory. The user chose it over FM-A knowing its costs (below).

**D1 Runtime model.** `eyedbg compose launch [SERVICE...]` puts each selected service in fast mode:
the container idles and eyedbg launches the app in it. A container-launch session is a **launched**
session (mode "", [ADR 0004](0004-daemon-owned-sessions-and-control-lease.md)'s rules: `stop` terminates the program, `detach` is refused) whose
`SessionInfo.container.launched` is true; `pid` stays 0 and the in-container pid is
`container.pid`. New daemon method `container.launch` (members in, per-member results out, like
`container.attach`) and optional driver interface `session.ContainerLauncher`. No `ProtocolVersion`
or JSON `schema` bump; an old daemon answers `UNKNOWN_METHOD`, mapped to `VERSION_MISMATCH`.
`version --json` lists the feature `compose-launch`. `eyedbg attach dotnet --container` stays
attach-only and refuses a fast-mode container ("its app runs only under `eyedbg compose launch`").

**D2 The idle container.** Per fast service, the generated override sets `entrypoint: ["tail", "-f",
"/dev/null"]`, `command: []` and `init: true` (docker-init forwards SIGTERM, so stop, restart and
recreate don't wait out the grace period, and reaps orphans). Before anything changes,
`docker exec <as-built container> tail --version` must exit 0, else the service is refused ("the
image has no `tail`: chiseled or distroless images can't idle"). Not chosen: an eyedbg-built idle DLL
run by the image's own `dotnet` (would serve chiseled images but needs a second host build per
target framework — a follow-up), and netcoredbg as pid 1 (it can only be copied in after start).

**D3 Launching in the container.** The adapter is `docker [engine flags] exec -i <full id>
/.eyedbg-netcoredbg-<v>/netcoredbg --interpreter=vscode` with **no** `-e/--env/--env-file`,
`-u/--user`, `-w/--workdir`, `-t` or `--privileged`. The DAP `launch` arguments are `{name: "eyedbg",
type: "coreclr", request: "launch", program: "<workdir>/<dll>", args: [], cwd: "<workdir>",
stopAtEntry, justMyCode: true}`: **no `env` key**. The app starts suspended; breakpoints (a
startup line included) are sent before `configurationDone`. `--stop-on-entry` sets `stopAtEntry`
(reason `entry`). The launch facts — `dll` and `workdir` — come from eyedbg's own labels on the
container (D12), read by the daemon, not from the request: a caller can't make eyedbg launch an
arbitrary program in an arbitrary container: the labels make a launch self-describing. A session treats adapter EOF as "the debug adapter exited" (a stopped
container ends the exec without a `terminated` event). A launch whose app dies at load shows only as
output; the CLI says where to look.

**D4 Environment.** eyedbg never reads, copies or passes the container's environment: no `env` in the
launch, no `-e` on the exec. The app gets pid 1's configured environment (image `ENV`, compose
`environment:` and `env_file:` values), measured name-for-name equal to the as-built app's. It
differs from pid 1 only by what a shell entrypoint would export (moot: only exec-form `["dotnet",
"<dll>"]` entrypoints are accepted) and `TERM` for `tty: true` services (unmeasured: the test
containers had no tty).

**D5 Program arguments.** A service whose as-built container has a non-empty `Config.Cmd` (a compose
`command:` or the image's `CMD`) is refused: fast mode launches `dotnet <dll>` without arguments and
eyedbg never reads them (they may hold secrets). The check is presence only (`{{if .Config.Cmd}}`).

**D6 The mount.** The Debug build is bind-mounted **read-only** at the as-built container's
`Config.WorkingDir` (the template's `/app`), so the path, the working directory and ASP.NET's
content root are what the container used, and the image's Release files are hidden. The working
directory is accepted only when absolute, `path.Clean`-equal, at most 255 bytes, 1–4 segments of
`[A-Za-z0-9._-]` (no `.` or `..`), the first not one of `bin boot dev etc lib lib32 lib64 libx32 proc
run sbin sys tmp usr var`; else the service is refused, naming it. The mount is long-syntax with
`create_host_path: false`, so a missing source fails instead of being created as an empty directory.
Read-only means an app that writes next to itself fails (documented); read-write was rejected
because on Linux the files a root container writes land root-owned in eyedbg's directory and break
the mirror's deletions.

**D7 Healthchecks untouched.** The override has no `healthcheck`. An idle container, or an app held
at a breakpoint, is `unhealthy`; a launched, serving app is `healthy`: health keeps meaning "the app
serves". Entering fast mode runs `up -d` **without `--wait`** (it would fail on an idle container);
`restore` keeps `--wait`. Dependents are never touched (`--no-deps`); one that compose itself
(re)starts with `condition: service_healthy` on an idle service fails — documented ("launch it
first"). Not chosen: `healthcheck: {disable: true}` (breaks `--wait` and `service_healthy` dependents
— docker/compose#13522 — and hides whether the app serves) and rewriting the check (needs its text,
which may hold credentials).

**D8 Output.** The app's stdout and stderr reach eyedbg as DAP output: `eyedbg output -s ID` (the
last 2000 chunks, in memory, never recorded) and `eyedbg compose events --kind output` (merged,
service-prefixed). `docker compose logs SERVICE` shows nothing from the app while the service is in
fast mode — said in help and on every launch. Not chosen for now: mirroring the output into the
container's own log (a second exec per session, coreutils, back-pressure) — a follow-up.

**D9 Lifecycle.** The app lives exactly as long as its session: `eyedbg stop`, `compose stop` or the
daemon's shutdown send `disconnect {terminateDebuggee: true}`, which kills it without graceful
shutdown; the container stays up, idle and `unhealthy` — **the service is down** until the next
`compose launch` or `compose restore`. `docker compose stop|restart|down`, or a plain `docker
compose up` that recreates the service, kills the app with its container and ends the session. An
app crash ends the session with the exit code (unknown on Linux: `ExitCodeUnknown`); the restart
policy doesn't apply because pid 1 idles.

**D10 One app per container; stray guard.** The session manager keeps one **claim** per full
container id, taken under its lock before the adapter starts and released when the start fails or
the session ends: a launch that finds any claim, or an attach that finds a launch claim, is refused,
naming the session; a container holds either one launch or any number of attaches with distinct pids.
The full id is only known once the driver has inspected the container (a reference may be a name
or a short id), so the claim is two steps: an early check of the live claims by the reference
(container name, or an id prefix of at least 12 characters) before the driver runs, which spares
the inspect, the `docker top` and the copy, and the authoritative claim by full id after the driver
and before the adapter starts, which catches every other spelling. Then `docker top <id> -o pid,stat,comm` (names only, never command lines): a non-zombie process
named exactly `dotnet` is an earlier app — for instance one whose netcoredbg never saw EOF — and
the launch is refused after up to 5 s of re-checking (a stopped app may still be exiting), with the
hint `docker restart NAME`, which keeps fast mode and kills only that container's processes. A failing
`docker top` lets the launch proceed with a logged warning. eyedbg never restarts a container or
kills a process it didn't launch. (The `Z` filter is moot: `docker top` doesn't list zombies on
either engine; it is kept as harmless.)

**D11 Rebuild without recreate.** Each fast service mounts a stable directory
`<home>/compose/<project>/services/<service>/`. Re-running `compose launch` for a service already in
fast mode: publish into a fresh `stage-<8 hex>/` while the old app runs (a build failure changes
nothing) → capture the caller's breakpoints from the service's live sessions → stop the old session
(the app dies) → mirror the stage into the service directory → launch → prune. The mirror writes each
file to a temp file in its destination directory and renames it over (mode 0644 or 0755 by exec
bit), makes directories 0755, removes destination entries that aren't in the stage — files and
symlinks with `os.Remove` (links are never followed), real directories with `os.RemoveAll` of that
exact joined path — and walks the source first, so anything but regular files and directories fails
**before** any change; the destination must already be a real directory. A container is recreated
only on entry: one whose fast-mode labels this project's override file doesn't record (another
override path, a moved home, an image's own `LABEL`s) is judged as built (D12). `--no-build`
relaunches from the service directory as it is. Not chosen: publishing straight into the mounted
directory (overwriting a running app's mapped assemblies can crash it, and a failed build leaves a
half-copied app).

**D12 Discovery from the running container.** Never from compose configuration. Per selected
service, one `docker inspect --type container --format <template>` (a test pins the template) that
emits: id, name, state flags, `.Platform` (only `"linux"`; the architecture comes from `docker image
inspect`, as in ADR 0020), the entrypoint's second element **only** when the entrypoint
is exactly two elements and the first is `dotnet` (`{{if and (eq (len .Config.Entrypoint) 2) (eq
(index .Config.Entrypoint 0) "dotnet")}}…{{else}}null{{end}}`; checked on 0-, 2-, 3- and 8-element
entrypoints, on both engines), whether `.Config.Cmd` is non-empty, `.Config.WorkingDir`, and labels
through `index`: compose's `project`, `service`, `project.working_dir`, `project.config_files`; and
eyedbg's `dev.izzat.eyedbg.fast.{version,override,dll,workdir,project}`. Never `.Config.Env`, `.Args`,
`Cmd` or entrypoint values outside those gates, or a healthcheck's `Test`. The DLL must match
`^(\./)?[A-Za-z0-9_.-]+\.dll$`. An entrypoint of any other shape — shell wrappers, apphosts, an IDE's
debugger worker — is refused. The .NET project is the unique `<name>.csproj|.fsproj|.vbproj`
(name = the DLL without `.dll`) under the compose working directory (realpath; the walk skips
dot-directories, `bin`, `obj` and `node_modules`, follows no symlinks and stops at 50 000 entries)
**that no container of the stack can write**, else `--dotnet-project SERVICE=PATH`, which must resolve
inside the compose directory.

*Writable mounts.* Building a project runs its code on the host, so a project file a container
could have planted is never found. Per run (once, and only when a search is needed) eyedbg asks docker
for the host `Source` of every read-write **bind** mount of every container of the project in any
state (`docker ps -a --filter label=com.docker.compose.project=P`, then one `docker inspect` whose
template names only `.Mounts[].Source` of `RW` bind mounts; a pinned template, no environment or
arguments). A match at or under a source is not counted among the matches; when it is the only match,
the service is refused (`INVALID_REQUEST`, skipped without names) naming `--dotnet-project`; when the
compose directory itself is at or under a source, no search is made. A path is at or under a source
when it, or an ancestor up to the compose directory, is the same file as the source (`os.SameFile`),
so symlinks and letter case on a case-insensitive file system don't matter; a source that doesn't
exist on this host is compared by its path. A **read-only** mount can't be written by its container
and stays searchable (named volumes aren't bind mounts here). A project named with
`--dotnet-project` is built even under a writable mount: the user chose that file. A project a
corroborated label remembers (below) was accepted by an earlier run and isn't searched again. Not
analysed: a project outside the mounts whose `ProjectReference`s, imports or globs reach into one.

*Untrusted labels.* **Any container label is untrusted input unless eyedbg-owned state
corroborates it.** The compose directory is corroborated by the compose files in it (D14);
eyedbg's own `dev.izzat.eyedbg.fast.*` labels only when the override file in eyedbg's 0700 project
directory (`<home>/compose/<project>/override.yml`, a regular file with eyedbg's header, read back
bounded) records the same `version` and `override` for that service, at that file's own path, and,
for the label format this code reads, the same `dll`, `workdir` and `project`. A container whose
labels fail that is judged as built (its labels decide nothing, so the project comes from the search
or the flag, and an idle container is refused saying its labels were ignored); the staying services
an override is rewritten with come from corroborated containers only. Values are also validated by
the grammars above (control characters refused, paths absolute or confined), exactly as if a caller
had typed them.

Each service's project (relative to the compose directory) is printed on stderr before its build
starts, and is `project` of the member in `--json` (additive, also for a failed build).

**D13 Host build.** The CLI runs `dotnet publish <project> -c Debug -o <stage>/<service>
--artifacts-path <home>/compose/<project>/artifacts -p:UseAppHost=false -p:DebugType=portable
-p:PathMap=<realpath(working dir)><separator>=/src/ -nologo -tl:off`, in the caller's environment
(its NuGet credentials, SDK and `global.json`; the host `dotnet` as the launch path finds it), with
stdout and stderr to a `build.log` (0600, a file, not a pipe a lingering build server could hold
open), bounded by the context and 15 minutes. The PathMap root and the project path are **refused**
when they hold `,` `;` `%` `=` `"` or a control character — never escaped (MSBuild and Roslyn would
split or unescape them). `--artifacts-path` keeps the user's `bin/` and `obj/` untouched (a PathMap'd
build would otherwise overwrite their IDE outputs). After the build, `<stage>/<service>/<dll>` and
its `.pdb` must exist. The portable, framework-dependent output has no RID, as the Visual Studio
template's own `publish /p:UseAppHost=false` shape. The host SDK must build the project's target
framework and the container's runtime is the image's own, so an image older than the project (a
runtime that can't run the build) shows up as an app that dies at load, in the output events, hinted
as "rebuild the image (`docker compose build SERVICE`)"; a multi-targeted project fails `publish` without `-f`
(`BUILD_FAILED`; `--framework` is a follow-up).

**D14 PDB paths.** The build's `PathMap` to `/src/` gives the same document paths as a
Visual Studio-template image (`WORKDIR /src`, `COPY . .`), so a fast-mode session uses ADR 0020's
default map `[{/src, realpath(working dir)}]`. `--map` with `launch` is refused (`INVALID_REQUEST`).
`launch` and `restore` (and the daemon's `container.launch`) use the working directory and the files
a container's labels name only when they corroborate each other (ADR 0020, D5: a regular
`.yml`/`.yaml` file of `config_files` lies in the directory), and, when `--project-directory` or
the first `-f` names the project's directory, when it is that directory (compose's own rule); a
container made by plain `docker run` from an image that sets those labels is refused
(`INVALID_REQUEST`). Without either flag compose found the project from the current directory,
which eyedbg doesn't second-guess. **Not supported:** compose files outside the project directory
(`--project-directory DIR` with `-f FILE` outside DIR): eyedbg can't tell that layout from a foreign
container's labels, so it is refused with "can't confirm the project directory", the files it looked
at, and the way out (keep a compose file inside the project directory, or run compose without
`--project-directory`); `attach` uses `--map` there.
Not chosen: host paths in the PDBs plus an identity map (netcoredbg on Linux would compare `C:\…`
paths; the path map requires POSIX remotes).

**D15 One override per project.** `<home>/compose/<project>/override.yml` (0600, temp file and
rename) holds a fragment for every service of that project in fast mode (entering now plus those
already fast per their labels), sorted by name — adding a fragment doesn't change another service's
configuration hash. Line 1 is exactly `# Generated by eyedbg compose launch; 'eyedbg compose restore'
undoes it. Do not edit.`, then JSON indented by `encoding/json` with `SetEscapeHTML(false)` and `$`
doubled in every string (compose interpolation). Per service only: D2's `entrypoint`/`command`/
`init`; D6's `volumes: [{type: bind, source: <home>/compose/<project>/services/<service>, target:
<workdir>, read_only: true, bind: {create_host_path: false}}]`; `labels` `dev.izzat.eyedbg.fast.
{version: "1", override: <this file's absolute path>, dll, workdir, project}` (the project file's
path relative to the compose working directory, `/`-separated). **Never** `environment`,
`env_file`, `secrets`, `configs`, `healthcheck`, `ports`, `user`, `image`, `build` or `working_dir`.
There is no `--out`: an idle-container override is useless outside eyedbg; `--json` reports its path.
Compose files are never edited; the override is a separate file in eyedbg's own directory.

**D16 Recreate, and no drift check.** Entering runs `docker [engine flags] compose -p <project>
--project-directory <dir> -f <files>… -f <override> [--env-file F]… up -d --no-deps
--force-recreate --no-build SERVICE…` (no `--wait`: D7). `<files>` are the container's own
`com.docker.compose.project.config_files` label **minus** the value of its `fast.override` label
(exact string match; files are never read): any `-f` disables compose's default discovery of
`compose.override.yml` and of `COMPOSE_FILE`, so the label is what compose actually used and the
CLI's own `-f` can't reproduce it. The CLI's environment and `--env-file` flags are what compose
interpolates. Because `config --hash` can't be compared with the container's label (Context), there
is **no drift check**: the first launch of a service prints "recreated with this shell's compose
environment" so a changed variable or file is visible. eyedbg never runs `compose down|rm|build|pull`
or `compose config`. Recreating **discards what the container wrote in its own filesystem outside
volumes**; it happens only on an explicit `compose launch`/`restore`, and is said in the help. A
failed recreate (a missing mount source, say) leaves the old container running.

**D17 Files and retention.** `<home>/compose/<project>/{lock, override.yml, artifacts/, stage-…/,
services/<service>/}`. A service directory lives while some container of the project for that
service, in any state, carries `dev.izzat.eyedbg.fast.override` (`docker ps -a` with a label filter);
the project directory goes when none does. A `stage-…` directory lives for one run. `lock` (O_EXCL,
stale after one hour) serialises runs per project. Deletion is confined to those eyedbg-named
directories by `Lstat` and exact-name grammars: a symlinked or foreign name is left alone, and
never followed. `restore` decides by what the project directory holds, not by what its engine
showed: it removes eyedbg's files for the project when it listed at least one fast-mode container of
it, or when the directory holds nothing a container can mount (no `override.yml`, nothing under
`services/`: a failed first launch's `build.log`, the directory this run's lock made). With neither
(the same-named project on another engine, whose containers still mount the directory: a `restore -p
NAME` or a `restore` whose `compose ps` showed that other project) it removes nothing and says which
directory it kept. A run that finds nothing and holds nothing leaves no directory and no note.

**D18 CLI.** `eyedbg compose launch [SERVICE...] [-f FILE]... [-p NAME] [--project-directory DIR]
[--dotnet-project SERVICE=PATH]... [--env-file FILE]... [--bp LOC]... [--exceptions M]
[--lease-policy P] [--no-record] [--no-build] [--stop-on-entry]`; `eyedbg compose restore
[SERVICE...]`; `eyedbg compose stop` (ADR 0020). `launch` narrows the set stage by stage — discover
(running containers only), classify (fast and current → no recreate; else entry), checks, lock,
project, publish (once per distinct project, sequentially), carry the caller's own non-temporary,
non-editor breakpoints and the exception mode from the live sessions of the selected services, stop
those sessions, mirror, write the override and recreate (entry services only), `container.launch`
with the carried breakpoints plus `--bp`, prune. Nothing in docker or in the daemon's sessions
changes before the old sessions are stopped, apart from the adapter copy into an as-built container
(as any attach makes); any failure before that removes only the stage directory. The mirror never
runs before the old app was stopped. Exit 0 when at least one service launched, else the first
failure's class. `restore` stops the members' sessions, runs `up -d --no-deps --force-recreate
--no-build --wait --wait-timeout 180` with only the container's own files, rewrites the override
and prunes; it never launches. A service that isn't in fast mode is skipped ("not in fast mode").

**D19 The adapter copy, probed first.** At entry the CLI copies netcoredbg into the **as-built**
container and probes it, so musl, a foreign architecture and a read-only root filesystem fail before
anything is recreated; on a launch eyedbg first probes an existing `/.eyedbg-netcoredbg-<v>/netcoredbg
--version` and copies only when that fails (the directory name pins the version; whoever can replace
the copy can already run code in that container). The copy survives a restart but not a recreate.

### Security

* **The host build has the trust of building the project yourself, in the caller's shell:**
  `dotnet publish` runs the project's MSBuild logic (targets, analyzers, restore sources), with the
  caller's credentials. The project's own code runs only as the app that an explicit `compose launch`
  starts in the container.
* **Recreating a service changes the user's running stack**: containers lose their own-filesystem
  writes, and a service is down whenever no eyedbg session runs its app (D9). Only an explicit
  `compose launch`/`restore` does it, only for the named or implied services, `--no-deps`,
  `--no-build`; `restore` is the documented way back.
* **Environment and arguments are never read or passed** (D4, D5, D12): the launch has no `env`, the
  exec no `-e`, the inspect template names its fields. The override holds only paths, the idle
  entrypoint and eyedbg's labels, never `environment`, `env_file`, `secrets`, `configs`,
  `healthcheck` or `ports`.
* **Labels, as read back, are untrusted** (anyone who can edit a container's labels can edit
  eyedbg's input; an image's own `LABEL`s are copied onto its containers): *any container label is
  untrusted input unless eyedbg-owned state corroborates it.* `dll`, `workdir` and `project` are
  re-validated at every use and believed only when eyedbg's own override file records them (D12), a
  launch re-derives nothing from the request (D3), and the compose directory and files the labels
  name are used only when they corroborate each other and the command line (D14).
* **A container can't choose what the host builds.** The host build runs the project's code, so a
  project file written by a container (the stack's read-write bind mounts, D12) is never found and
  built unasked; a project the user names with `--dotnet-project` is the user's own choice, built
  wherever it lies in the compose directory. Each service's project is printed before its build.
  Residual: MSBuild reaches files beyond the project file (a `ProjectReference`, an `Import`, a
  glob into a writable directory, a package source); eyedbg checks the project file it picks, not its
  graph, and a named volume bound to a host path is not seen as a bind mount.
* **Escaping fails closed.** MSBuild's and Roslyn's separators in a path are refused, not escaped;
  `$` in override strings is doubled; compose file lists come from labels, never from parsing YAML.
* **Deletion and overwrite are confined** to eyedbg's per-project directory under its home by `Lstat`
  and grammar; the mirror's deletions never follow a symlink; the override is the only file replaced,
  by rename.
* **No vsdbg, ever** ([ADR 0005](0005-never-use-vsdbg.md)): eyedbg never mounts, starts, reuses or recommends `vsdbg` or an IDE's
  debugger worker it finds in a container; an IDE-created container is refused until recreated.
* **A process in a foreign namespace.** The launched app runs as the container's own user, with its
  own environment; the adapter's DAP stream is untrusted input, as in ADR 0020. The stray guard
  reads process names only, never command lines.

### Consequences

* Good, because Debug code, startup breakpoints and `--stop-on-entry` work in a stack whose images
  publish Release, with no Dockerfile edit and no image rebuild; a rebuild is a re-run of
  `compose launch`, about 2 to 4 s for a small app (the real project's build dominates).
* Good, because the app sees the container's own environment, user, working directory and ports.
* Bad: **a fast-mode service runs only while its eyedbg session runs its app** (D9). Without a
  session the container idles and the service is down. This is the largest behaviour change and is
  in the help text and in every launch's output; `compose restore` undoes it.
* Bad: the app's output is in eyedbg, not `docker compose logs` (D8); stopping kills it without a
  graceful shutdown; a crash is not restarted (D9).
* Bad: an idle fast service is `unhealthy`; compose-started `service_healthy` dependents of it fail
  (D7).
* Bad: the app directory is read-only (D6); an app that writes beside itself fails.
* Bad: services that pass `command:` arguments, whose entrypoint isn't exec-form `["dotnet", "X.dll"]`,
  or whose image has no `tail` (chiseled, distroless) are unsupported and refused with the reason.
  A container last created by an IDE's fast mode is refused until a plain `docker compose up -d`
  recreates it.
* Bad: there is no drift check (D16): a launch after the compose file or environment changed
  recreates the service from whatever this shell's compose resolves; the printed note is the only
  guard.
* Bad: a lost client leaves a frozen app and netcoredbg (ADR 0020, "a lost client leaves a stray");
  the stray guard refuses a second launch with `docker restart NAME` as the way out.
* Unverified, not claimed: Windows hosts (publish, `PathMap`, long-syntax `C:\…` mounts, file locking),
  podman, rootless docker, SELinux relabelling, remote engines (the bind mount fails there with a
  missing-source error), compose profiles and scaled services, `tty: true` services' `TERM`, and the
  child pid of `init: true` containers under attach.

### Confirmation

* `internal/container`, `internal/artifacts` and `drivers/dotnet` unit tests: the pinned inspect
  template (no `Env`, `Args`, `Cmd` or entrypoint values outside the gates), the override golden
  (`$`, `$$`, `${X}`, re-parse after line 1, sorting), the `up` argv, the `docker top` parser
  (`dotnet-counters` isn't `dotnet`), the mirror (a symlink in the destination pointing outside is
  removed and its target survives; a symlink or FIFO in the source fails with the destination
  untouched; stale files go; modes normalised on the destination; the destination a symlink →
  error), prune confinement, the project search (0, 1, 2 matches, limits, symlink loops), the
  publish argv with a fake `dotnet` and the character refusals.
* `internal/session`, `internal/daemon` and `drivers/dotnet` tests (`-race -count=5`): no `env` key
  and empty `args` in the launch request, a startup breakpoint hits before `configurationDone`, stop
  sends `terminateDebuggee: true`, `detach` refused, two concurrent `container.launch` on one
  container give exactly one session, claims freed after a failed start and after exit, attach and
  launch claims conflict, a stray is retried then refused, `docker top` failing proceeds.
* `internal/cli` tests with fakes for the engine and `dotnet`: one publish for two services of a
  project, the stop → mirror → launch order on a rebuild, a build failure leaving sessions untouched,
  `LEASE_HELD` excluding a service, `--no-build` cases, a `compose up` failure pruning nothing,
  `restore` of a mixed set; goldens and help tests.
* The docker e2e (`internal/e2e`, `TestComposeLaunch`, behind `EYEDBG_E2E_DOCKER=1`, fixture
  `testdata/apps/dotnet/compose`; it needs the .NET SDK on the host for the Debug publish): the
  as-built Release container's log says `build=release` and holds the inherited `environment:` and
  `env_file:` values (a Release frame can't evaluate `Build`), `compose attach` and `compose stop`
  leave it running; `compose launch --bp <startup>` recreates both containers and stops the
  producer at the startup line with the copy's host file and an excerpt; after one `next`,
  `startupEnv` is `compose/file` (the inherited values) and `Build` is `debug`; output appears in
  `compose events --kind output` and not in the container's `docker logs`; an edited `Tag` shows
  after a re-run in the **same container id**, not recreated, with the carried breakpoint listed;
  `eyedbg stop` leaves the container running with no `dotnet` in `docker top`; `compose launch
  --no-build` relaunches the last build; `compose restore` removes every fast label,
  `<home>/compose/<project>` and eyedbg's override, and the services run as built again; no `bin`
  or `obj` appears in the project copy; five consecutive clean runs on macOS Docker Desktop and the
  Linux test VM, and the two ubuntu CI entries.

## Pros and Cons of the Options

### As-built attach only

* Good, because it needs no feature beyond ADR 0020.
* Bad, because line breakpoints never bind on a Release image; the user must edit the Dockerfile
  or rebuild with a build argument the Dockerfile may not declare.

### FM-A — the Debug build as the main process, attach after start

* Good, because the service behaves as before: pid 1, `docker compose logs`, healthcheck, restart
  policy and `depends_on` work with or without eyedbg, and the attach path is reused unchanged.
* Bad, because code that runs before the attach (startup) can't be stopped in, and every rebuild
  recreates the container. The user rejected it for that reason.

### FM-B — an idle container, the debugger launches the app

* Good, because startup breakpoints, `--stop-on-entry` and rebuilds without recreating work, and it
  is how the IDEs do it.
* Bad, because the service is down without a session, its output goes to eyedbg, it isn't pid 1, and
  an idle entrypoint needs a program the image has.

### An IDE-style debugger worker as pid 1

* Good, because the app is pid 1 under the debugger from the start.
* Bad, because netcoredbg can only be copied in after start, and a listening worker needs published
  TCP ports.

### `dockerfile_inline` rewrite

* Bad, because rewriting Dockerfile text is fragile and fails silently.

### A `BUILD_CONFIGURATION` build-arg override

* Good, because it keeps image-based behaviour (attach works unchanged on a Debug image).
* Bad, because it needs the Dockerfile to declare the argument (the current Visual Studio template
  does; many projects' don't) and a rebuild of the image per change. It stays documented for users
  who prefer it.

## More Information

* Follow-ups, not decided here: mirroring the app's output into the container's log; passing
  `command:` arguments; an idle DLL for chiseled images; `--framework` for multi-targeted projects;
  a drift check that doesn't depend on compose's hash; a `--no-wait` for `restore`; VS Code joining
  a whole group; Windows hosts.
* The measurements behind the Context are recorded in the task notes of the implementation, per
  machine, with the compose and docker versions above.
