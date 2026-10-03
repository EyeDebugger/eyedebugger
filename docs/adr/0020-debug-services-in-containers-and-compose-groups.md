---
status: proposed
date: 2026-10-02
decision-makers: Ijat (@ijat)
---

# Debug services in containers, and compose stacks as groups

## Context and Problem Statement

A common .NET setup is a docker compose stack: several microservices, each built from a Visual
Studio-template Dockerfile, next to Kafka and a database. Visual Studio's container tools let a
developer debug every service of such a stack at once, with breakpoints in any of them, while the
stack keeps running. eyedbg can't: `attach` takes a **host** pid (DESIGN §3, §11 — the same-user
check reads `/proc`, `ps` or a process token on the host), the adapter runs on the host, and every
source path an adapter reports is a path inside the container, not a file the CLI can open.

What does it take to attach to a .NET process in a running container and to treat a whole compose
project as one thing an agent can wait on — without editing the user's compose files, without a
second session model, and without letting a container's adapter or a client reach anything it
shouldn't? [ADR 0021](0021-fast-mode-host-debug-builds-in-compose-services.md) covers the other half of the Visual Studio experience (debugging a Debug build
instead of the image's Release build, with the app launched under the debugger); this ADR is what it
builds on.

What was verified before deciding (an orchestrated spike and a verification pass, on macOS with
Docker Desktop 24.0.7 / compose 2.23.3 (arm64) and on Debian 13 with docker.io 26.1.5 / compose
2.26.1 (amd64); scratch compose projects only):

* netcoredbg's Linux release (3.2.0-1092; `linux-amd64` and `linux-arm64` tarballs, seven flat files,
  about 9 MB, glibc-dynamic, no musl build) streamed into a **running** container with `docker cp -`
  and a tar eyedbg builds runs there as root and as a non-root `USER`, and speaks DAP over
  `docker exec -i … netcoredbg --interpreter=vscode`: stdio fits eyedbg's adapter transport
  unchanged. DAP `attach {processId: 1}` needs no `SYS_PTRACE`; `disconnect {terminateDebuggee:
  false}` leaves the container running and no netcoredbg behind. Two services at once work.
* netcoredbg has no source-map option. A line breakpoint's absolute path must equal a PDB document
  path exactly, and it reports PDB paths back. Images built from the Visual Studio template
  (`WORKDIR /src`, `COPY . .`) have PDB paths under `/src/…`; the host's files are somewhere else.
  Mapping has to happen on eyedbg's side of the DAP connection.
* **Attach to an image's Release build binds no line breakpoints.** With the image's own
  `-c Release` publish (PDBs present, same `/src` paths), `attach` succeeds, modules and threads
  appear, `pause` and stacks work, but every line breakpoint stays unverified and never hits, with
  `justMyCode` on and off. The same image built `-c Debug` binds and hits, and a *launch* of the
  Release build binds (locals partly optimized away). This is why ADR 0021 exists.
* `init: true` makes `docker-init` pid 1 and the .NET runtime a child (pid 7 in the test container).
  `docker inspect`'s `.Path` still says `dotnet`; `attach {processId: 1}` returns success and
  **nothing happens** (no module or thread events, no error).
* A lost host `docker exec` client (`kill -9`) leaves netcoredbg **and the frozen debuggee** inside
  the container: netcoredbg never sees EOF on its stdin. Nothing cleans them up; `docker restart`
  does. Stopping the container instead ends the host `docker exec` (exit 137) with no DAP
  `terminated` event, just EOF.
* While a service is stopped at a breakpoint its curl-style healthcheck fails after about
  `retries × interval + timeout` (about 19 s for 5 s / 3 retries) and the container shows
  `unhealthy`; plain docker doesn't restart it and it turns `healthy` within one interval after
  `continue`. A Kafka consumer paused past `max.poll.interval.ms` is still alive after the resume,
  logs `Local_MaxPollExceeded`, loses and regains its partitions and goes on.
* `docker inspect --format` templates behave as needed on both engines, with traps: `.Platform` is
  only `"linux"` (the architecture is in `docker image inspect --format
  '{{.Os}}/{{.Architecture}}/{{.Variant}}'`); `.Config.Healthcheck.Test` on a container **without** a
  healthcheck is a template *error* (`index .Config "Healthcheck"` inside `{{with}}` works);
  `docker compose ps --format json` is NDJSON (one object per line) on both compose versions, with
  about twenty fields of which a handful are needed.
* `docker cp - <id>:/` on a `read_only: true` container fails with `container rootfs is marked
  read-only`; nothing is copied.

## Decision Drivers

* No edits to the user's compose files, Dockerfiles or images: the attach path works on a stack as
  it runs today.
* CLI first, for agents (DESIGN §4): every capability has complete built-in help; no MCP server.
* Reuse the session model ([ADR 0004](0004-daemon-owned-sessions-and-control-lease.md), 0009): one session = one adapter, one stop, one frame space;
  lease, breakpoint ownership, event log and the editor facade ([ADR 0012](0012-serve-dap-to-editors-through-eyedbg-dap.md)) work unchanged.
* The trust model stays explicit (ADR 0004, 0011): the same-user pid check protects host processes;
  a container needs its own boundary. Secrets in the container's environment, arguments and compose
  configuration must never be read, logged or returned.
* An adapter running in a container, and a path it reports, are untrusted input.
* Windows is a first-class host (AGENTS.md rule 7) even where a container test can't run there.
* A multi-service stack has to be usable by an agent: wait for whichever service stops, set a
  breakpoint in any service by its host path, stop everything, with one command each.
* No change to the native `ProtocolVersion` or to the CLI's JSON `schema` (additive fields only).

## Considered Options

D1 — how a compose project is represented:

* One session per service, tagged with a group (the compose project).
* A parent session with child sessions.
* A multiplexer in the DAP facade.

D2 — the CLI surface:

* `eyedbg attach dotnet --container REF` for one container and an `eyedbg compose` subtree for
  groups.
* A `--group` flag on `wait`, `events`, `detach`, `bp` and `sessions`.
* An `eyedbg compose up` that runs docker compose.

D3 — where the docker transport lives:

* An optional driver interface (`session.ContainerAttacher`) implemented by the .NET driver, over a
  language-neutral `internal/container` package.
* A manifest `adapter.transport: "docker"`.
* A session-level wrapper that copies and execs for any driver.

D4 — path mapping:

* A translator hook at the DAP client's single choke point plus read confinement in the session.
* Per-site mapping in the session and the facade's forwarder.
* Adapter-side maps.

D6 — the daemon API:

* New methods (`container.attach`, `group.wait`, `group.events`).
* A `container` field on `session.start`.

D10 — getting the adapter into the container:

* `docker cp -` of a tar eyedbg builds, on every attach.
* A bind mount added through a compose override.
* Requiring an image that already contains netcoredbg.

## Decision Outcome

Chosen options: one session per service tagged with a group; `eyedbg attach dotnet --container` plus
an `eyedbg compose` subtree; a `ContainerAttacher` driver interface over `internal/container`; a
translator at the DAP client; new daemon methods; `docker cp -`.

**D1 Group model.** `StartParams.Group` and `SessionInfo.Group` name the compose project. A group is
a label on ordinary sessions, not an object: no parent session, no facade multiplexer, nothing
changes for a session that has no group. Group operations that need the daemon are two new methods
(D6); `bp` and `stop` fan out CLI-side over the group's sessions, each under its own lease rules.

**D2 CLI surface.** `eyedbg attach dotnet --container REF [--map REMOTE=LOCAL]... [--pid N] [--group
NAME]` attaches to one container. `eyedbg compose attach [SERVICE...]`, `compose wait`, `compose
events`, `compose bp add|ls|rm` and `compose stop` work on a project's sessions (ADR 0021 adds
`compose launch` and `compose restore`). `compose stop` applies `eyedbg stop`'s rule to each member:
an attached member is detached and its container keeps running; a launched one (ADR 0021) has its
app terminated. The group for the follow-up commands is `-g/--group`, else `$EYEDBG_GROUP`, else the
only group among the live sessions. `eyedbg compose attach` and the commands that follow it run
no `docker compose up|down|rm|build` and never `compose config`: their only compose call is `compose
ps --format json`. (ADR 0021's `launch` and `restore` also run `compose up` for the services they
name, nothing else.) There is no general `compose up` command.

**D3 Docker transport.** `session.ContainerAttacher` is implemented by `drivers/dotnet`; the
language-neutral docker code lives in `internal/container` (inspect, tar, `docker cp`, `docker
exec`), so a second language can use it later. The session stays docker-agnostic: it sees a `Launch`
whose adapter is the `docker` binary and whose arguments are `exec -i <id> /.eyedbg-netcoredbg-<v>/
netcoredbg --interpreter=vscode`. The adapter choice (platform, version, the SharpDbg refusal,
attach arguments) is already the driver's job, which is why it wins over a session-level wrapper.

**D4 Path mapping.** A `Translator` hook in `dap.Client` rewrites every `Source.path` of every
request, response and event, host-to-container outgoing and container-to-host incoming; nothing above
the DAP client sees a container path unless it couldn't be mapped. The session confines source reads
to the mapped host roots.

**D5 Map semantics.** A map is a list of REMOTE (absolute POSIX path) = LOCAL (absolute host
directory, symlinks resolved) pairs, 0 to 16. Longest prefix on a path-segment boundary wins;
duplicate remotes or locals are refused, never first-wins. An outgoing line-breakpoint path that
doesn't map is refused (`INVALID_REQUEST`, with the map in the hint); a forwarded editor request's
unmappable path passes unchanged; an incoming unmappable path stays a container path and is **never
read** (so a frame in the container's runtime or NuGet cache gets no source excerpt, even if a host
file exists at that very path). `..` segments, `\`, NUL and control characters, and on a Windows host
`:` and volume escapes are unmappable. The default map, when `--map` is absent and the container
carries compose's `working_dir` label naming an existing directory, is `/src` = that directory (the
Visual Studio template's `COPY . .`); `--map` replaces it. A test enumerates go-dap's message types
that hold a `Source` so a library bump can't add an unmapped one.

**D6 Native API.** `container.attach {members: [...]}` starts one or more member sessions (at most 4
at once) and answers per member in input order; `group.wait` waits for any member to stop;
`group.events` returns the members' event logs merged by time with a resumable cursor. None goes
through `session.start`: the daemon decodes request parameters with a plain `json.Unmarshal`, so an
**older** daemon would silently drop a new `container` field on `session.start` and attach to host
pid 1. An unknown method answers `UNKNOWN_METHOD`, which the CLI maps to `VERSION_MISMATCH` with the
`eyedbg daemon stop` hint (the `openError` precedent). No `ProtocolVersion` bump; the CLI's JSON
`schema` stays 1 (new fields are additive, new outputs are new shapes). `version --json` lists the
feature `container`.

**D7 Container session info.** `SessionInfo` gains `group`, `container {id, name, service, project,
pid, platform, map, unhealthyAfter}` and `stoppedAt` (set while any session is stopped). `pid` is
omitted for container sessions, so every consumer of a host pid (`eyedbg dotnet ps`, `dotnet`
target resolution, the detach text) skips them; `eyedbg dotnet …` on a container session is
refused ("inspects host processes only").

**D8 The adapter in the container.** The pinned netcoredbg for the **container's** platform:
`eyedbg adapters install netcoredbg --platform linux/ARCH` downloads and SHA-256-checks it as any
install, into `<data>/_platform/<os>-<arch>/<name>/<version>` (a leading `_` can't start a manifest
name, so it can't collide). The container's platform is `linux` plus the **image's** architecture
(`docker image inspect`), amd64 or arm64 only. Never an adapter found on the container's or the
host's `PATH`; SharpDbg is refused in containers (`INVALID_REQUEST`).

**D9 Debug builds.** Decided in ADR 0021.

**D10 Delivery.** On every attach eyedbg streams a tar it builds from its pinned install — top
directory `.eyedbg-netcoredbg-<version>/` mode 0755, the executable 0755, the rest 0644, uid/gid 0,
regular files and directories only, at most 64 MiB — to `docker cp - <id>:/`, then runs
`/.eyedbg-netcoredbg-<v>/netcoredbg --version` through `docker exec`, which must exit 0 (else
`ATTACH_FAILED`: glibc-based linux/amd64 or arm64 only, not Alpine or other musl images; read-only
root filesystems unsupported). Streaming a tar to `/` sidesteps `docker cp`'s directory-target
semantics and the host's file modes (a Windows host has none). The copy is gone when the container
is recreated; it is the only thing eyedbg leaves in an attached container. ADR 0021 skips the copy
when a copy under that name already passes the probe.

**D11 Target process.** The default pid is 1 (the exec-form `ENTRYPOINT ["dotnet", …]` of the
template); `--pid N` is a pid in the container's namespace. `compose attach` without service names
attaches only services whose pid-1 executable (`.Path`) is `dotnet`, listing the rest as skipped;
named services are always attempted. A container started with `init: true` has `docker-init` as pid
1, and attaching to it appears to succeed and does nothing; eyedbg therefore reads the one boolean
`.HostConfig.Init` and, when it is true and no `--pid` was given, refuses with the hint to pass the
.NET process's in-container pid with `--pid` (`docker top` shows host-side pids, not container-side
ones). `attach {processId: <child>}` returned success in the spike but wasn't driven to a
breakpoint, so that path is unverified.

**D12 Engine selection.** The CLI sends `DOCKER_HOST` and `DOCKER_CONTEXT` (only these) with each
request; the daemon runs `$EYEDBG_DOCKER` (an absolute path) else `docker` from its `PATH`. Argv
only, never a shell; each value is a single `--flag=value` token placed before the subcommand, and
every container reference, context, host, group and service name is checked against a grammar
before anything runs (a leading `-`, whitespace, control characters or NUL are refused).

**D13 CI.** The docker e2e runs behind `EYEDBG_E2E_DOCKER=1` on the two ubuntu entries of the e2e
matrix only (`ubuntu-latest`, `ubuntu-24.04-arm`; Windows runners can't run Linux containers and
macOS runners have no docker). Unset, the tests skip naming the variable; set with docker missing,
they fail.

**D14 e2e scope.** Automated runs start only the .NET services of the fixture (`up -d --build --wait
--no-deps producer consumer`, no Kafka); the Kafka-dependent paths (a consumer past
`max.poll.interval.ms`) are exercised by hand on the test VM.

**D15 Long-stop notes.** A container session that stays stopped is a production hazard the CLI says
so about, rendered from `stoppedAt` and the container's own healthcheck timing:
`unhealthyAfter = retries × interval + timeout` (Docker's 3 / 30 s / 30 s when unset; none when the
container has no healthcheck or `NONE`), plus a fixed 4-minute Kafka note (the default
`max.poll.interval.ms` is 5 minutes). No daemon timers; the CLI renders from a clock it is given.
Documented consequences of a pause: HTTP callers time out, the healthcheck fails after about
`unhealthyAfter`, Kafka consumers leave their group after `max.poll.interval.ms`.

**What `container.attach` reads.** One `docker inspect --type container --format <template>` whose
template emits only (13 values): id, name, image id, `.Path`, the running / paused / restarting
flags, `.HostConfig.Init`, the healthcheck's `Interval`, `Timeout`, `Retries` and whether `Test` is
`NONE` (never its text), the compose labels `project`, `service` and `project.working_dir`, and
eyedbg's own fast-mode label (ADR 0021: an attach refuses a container that carries it). Docker's
template engine fails on a missing key, and `.HostConfig.Init` is absent on a container without
`init`, as is a container's `Healthcheck`, so every optional key is read with `index` (`index
.HostConfig "Init"`, `index .Config "Healthcheck"`): a container without them isn't an error.
`.Platform` is **not** read: it is only `"linux"`; the architecture comes from `docker image
inspect --format '{{.Os}}/{{.Architecture}}/{{.Variant}}'` of the image id the inspect returned.
Never `.Config.Env`, `.Args`, `.Config.Cmd`, entrypoint arguments or healthcheck commands, and never
`docker compose config` (which inlines `env_file` values into `environment`, with or without
`--no-interpolate`, on compose 2.23). The one compose call is `compose ps --format json`, parsed
for `ID`, `Name`, `Service`, `Project` and `State` only (on compose 2.23 the project comes from its
`Labels` field, matched on the exact `com.docker.compose.project` key; no other label is kept); its
other fields (`Command`, `Ports`, …) are never stored or shown.

### Security

* **Docker access is the trust boundary.** Whoever can run `docker exec` as the daemon's user can
  run code in any container; eyedbg adds nothing to that, but the host's same-user pid check
  (ADR 0004, DESIGN §11) doesn't apply to a container pid, and eyedbg doesn't invent a replacement:
  it attaches wherever docker lets the user attach. A session's lease and ownership rules ([ADR 0009](0009-client-identity-breakpoint-ownership-lease-and-event-log.md))
  are unchanged.
* **The adapter's DAP stream is untrusted input** (it comes from a process in a container, possibly
  a compromised one). It is read by the existing bounded reader; every path in it is validated by
  the path map before it can name a host file; `runInTerminal` and other reverse requests are
  handled as for any other adapter, unchanged.
* **Path confinement.** A source is read from the host only when `EvalSymlinks` of the mapped path
  lies inside a mapped host root; `..`, a symlink out of a root and an unmapped container path are
  never read. Breakpoints outside the map are refused rather than sent.
* **Secrets.** eyedbg selects inspect fields through `--format` and so never receives the
  environment, `Cmd` or entrypoint arguments, and it runs no `compose config`. Docker's stderr is bounded (4 KiB)
  and stripped of control characters before it reaches an error; stdout of inspect/probe calls is
  bounded (1 MiB).
* **Argv.** Every docker invocation is an argv; the grammars of D12 stop an attacker-influenced
  name from becoming a flag (`--privileged`, `-v=/:/h`); container names that look like flags are
  refused before they are used.
* **What eyedbg changes in a container** on attach: it writes `/.eyedbg-netcoredbg-<v>/` (D10) and
  runs netcoredbg as the container's own user. It does not restart anything and does not signal any
  process it did not start (netcoredbg detaches on `disconnect`).

### Consequences

* Good, because a compose stack can be debugged as a unit from the shell: attach to every .NET
  service, a breakpoint by host path in any of them, one wait for whichever stops, one stop.
* Good, because nothing above the DAP client changed: lease, ownership, event log, snapshot, the
  editor facade (`eyedbg dap -s ID` joins one member; frames arrive as host paths) all work on a
  container session.
* Good, because the user's compose files are never read for configuration or edited.
* Bad: **line breakpoints don't bind when attaching to an image's Release build** (see Context).
  Attach to a service whose image publishes `-c Release` supports `pause` and stacks, but not line
  breakpoints; function breakpoints and exception stops weren't verified on a Release attach.
  ADR 0021 is the way to a Debug build without editing the
  Dockerfile; a Dockerfile that exposes `ARG BUILD_CONFIGURATION` (the current Visual Studio
  template does) can instead be rebuilt `--build-arg BUILD_CONFIGURATION=Debug`.
* Bad: **a lost client leaves a stray.** If the host `docker exec` dies without a clean
  `disconnect` (a killed daemon, a crashed CLI), netcoredbg stays in the container and the debuggee
  stays frozen at its stop. eyedbg can't reap it from outside, and a re-attach to that container
  returns success but no breakpoint ever stops (verified). `docker restart NAME` clears it.
* Bad: a service stopped for long is observable from outside (D15). Pausing is not free in a
  running stack.
* Bad: the netcoredbg copy (about 9 MB) is streamed on every attach and disappears when the
  container is recreated.
* Bad: images that are not glibc-based linux/amd64 or linux/arm64 (Alpine, chiseled, distroless)
  can't be attached to; self-contained apps on a `runtime-deps` image are unverified.
* Bad: a service with `init: true` needs `--pid` (D11).
* Unverified, so unsupported and not claimed: Windows hosts (Docker Desktop with Linux containers),
  podman, rootless docker, SELinux relabelling, remote engines, emulated amd64-on-arm64 containers,
  Windows containers. GitHub's Windows runners can't run Linux containers, so no CI covers a Windows
  host; the Windows-tagged path-map tests do run.
* Bad: PDBs naming sources under another root (a CI build with deterministic source paths, `/_/…`)
  leave breakpoints pending; the hint names `--map`. A learned map is a follow-up.

### Confirmation

* `internal/session` and `internal/dap` unit tests against the fake adapter (no docker): the
  translator's completeness test over go-dap's `Source`-bearing types; map tables (`..`, `//`, `.`,
  a prefix without a boundary, nested roots, symlinks out of a root, Windows volumes and `\`);
  breakpoint, stack, source-excerpt and facade round trips through a remote-path fake.
* `internal/container` and `drivers/dotnet` tests with a fake engine: grammar tables with injection
  attempts, the tar's modes and refusals, the pinned inspect template (no `Env`, `Args`, `Cmd`),
  every failure path (not running, paused, wrong platform, `read_only`, probe failure), the exact
  adapter argv.
* `internal/daemon` and `internal/cli` tests with an in-process daemon: `container.attach` with a
  failing member, `group.wait` and `group.events` ordering, cursor and no-skip rules, goldens for
  every new text and JSON shape, and the help-tree tests.
* The docker e2e (`internal/e2e`, `TestComposeAttach`, behind `EYEDBG_E2E_DOCKER=1`, fixture
  `testdata/apps/dotnet/compose`, Debug images so line breakpoints bind): `compose attach` to both
  services (the path map is `/src` = the copy, the producer's `unhealthyAfter` is 18 s, the
  consumer, a non-root service, has none), `compose bp add`, a stop in each service with the frame
  in the host file and a source excerpt, `compose events` listing both stops with their session ids
  and a cursor naming both, `compose stop` leaving both containers running and no session behind,
  then `attach dotnet --container` with a stop and a `detach` that leaves the app running; five
  consecutive clean runs on macOS Docker Desktop and on the Linux test VM, and the two ubuntu CI
  entries.

## Pros and Cons of the Options

### One session per service tagged with a group

* Good, because the session model (one adapter, one stop, one frame space) is unchanged; groups are a
  label plus two daemon methods.
* Good, because editors keep joining a single member (`eyedbg dap -s ID`).
* Bad, because "stop everything" and "set a breakpoint everywhere" are CLI-side fan-outs, each
  member answering on its own.

### A parent session with children

* Bad, because a `Session` owns one adapter, one thread space and one stop; every per-session method
  would need parent semantics.

### A facade multiplexer

* Bad, because DAP has no multi-process thread space, and it would help editors only, not the CLI
  agents use.

### `attach --container` plus `eyedbg compose …`

* Good, because group semantics are contained in one subtree's help and goldens, and the
  single-container command stays the same shape as `attach`.

### A `--group` flag on every command; a `compose up`

* Bad, because group semantics spread over six commands' help and flags (`-s` / `-g` conflicts);
  running `docker compose up` for the user is a different, riskier feature (ADR 0021 runs it only for
  named services, on request).

### An optional driver interface over `internal/container`

* Good, because the driver already owns the adapter's platform, version and arguments.
* Good, because the session stays docker-agnostic and a second language can reuse the docker code.

### A manifest `transport: "docker"`

* Bad, because a container is a per-session target, not a property of an adapter; dotnet's manifest
  is builtin and never reads `transport`; it would widen argv templating beyond [ADR 0013](0013-socket-transport-and-native-manifest-languages.md)'s
  `${socket}`.

### A session-level wrapper for any driver

* Bad, because docker code lands in `internal/session`, and it still needs a per-driver hook for the
  foreign-platform adapter.

### A translator at the DAP client

* Good, because one hook each way sees every message; nil is today's behaviour byte for byte.
* Bad, because it runs on hot paths (no locks, no I/O) and a completeness test must guard new go-dap
  types.

### Per-site mapping; adapter-side maps

* Bad, because six or more sites (breakpoints, replace, function breakpoints, stack, forwarded
  editor requests) would each need it, and a missed one fails silently; netcoredbg has no map.

### New daemon methods

* Good, because an old daemon says `UNKNOWN_METHOD` instead of attaching to host pid 1.

### A `container` field on `session.start`

* Bad, because an older daemon drops unknown fields silently.

### `docker cp -` of a tar, every attach

* Good, because it needs no restart and no edit: a running container gets the adapter in under a
  second; works the same on Docker Desktop and Linux.
* Bad, because it is ephemeral and fails on read-only root filesystems.

### A bind mount through a compose override

* Good, because it was verified in the spike and is persistent.
* Bad, because every attach would first recreate the services, and the pinned install directory is
  0750, so a non-root container user can't traverse a bind mount of it on Linux.

### Requiring netcoredbg in the image

* Bad, because it means editing the user's images.

## More Information

* ADR 0021 builds on this one: the Debug-build "fast mode" that launches the app under the debugger
  inside the container.
* Step-1 checks behind the Context: the verification pass's per-machine table is kept with the task
  notes; the facts used here are stated above.
* Follow-ups, not decided here: VS Code joining a whole group; more languages through
  `ContainerAttacher`; learned or relative path maps; read-only root filesystems through mounts;
  resolving the .NET child of `init: true` containers; Windows hosts.
