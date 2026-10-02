// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Environment variables that choose the docker engine of --container: the
// CLI sends them with the request, since eyedbgd's own environment is
// whoever's started it.
const (
	envDockerHost    = "DOCKER_HOST"
	envDockerContext = "DOCKER_CONTEXT"
)

// containerFlags are the flags of 'eyedbg attach' that attach to a container.
type containerFlags struct {
	ref   string
	maps  []string
	group string
}

// attachLong is the long help of attach.
const attachLong = `Attach the debugger to a running process, creating a new session, as 'eyedbg start' would for a
program it launches. Languages: dotnet (a .NET program whose runtime has started, via netcoredbg),
c, cpp and rust (a running native process, via lldb-dap), and go (via Delve), all subject to the
OS's own attach restrictions below. python can't attach yet (UNSUPPORTED_BY_ADAPTER, exit 4:
debugpy needs gdb or lldb for it): start the program with 'eyedbg start python' instead.
Only your own processes: another user's is refused (ATTACH_FAILED, exit 3). The session shows the
process as "pid N (name)", never its command line.

Breakpoints, exceptions, the lease policy and recording work as for 'eyedbg start' (--bp,
--exceptions, --lease-policy, --no-record); with --bp, attach waits (up to --timeout) for the
first stop. When the adapter can't attach it is ATTACH_FAILED with the likely causes: for .NET
the process must be yours, not already under a debugger, not started with
DOTNET_EnableDiagnostics=0, and see the same TMPDIR as eyedbgd; for a native (non-.NET) adapter,
also the OS's own attach restrictions (Linux ptrace_scope, macOS task_for_pid).

The program is never killed by eyedbg: 'eyedbg detach' (or 'eyedbg stop') ends the session and
leaves it running. Starts the daemon if needed.

--container NAME (an id or a name) attaches to a .NET process inside a running Linux docker
container, with the container's own netcoredbg: eyedbg copies its pinned Linux build into the
container (a read-only root filesystem fails) and runs it there with 'docker exec -i', as the
container's user. Install that build once with 'eyedbg adapters install netcoredbg --platform
linux/arm64' (or linux/amd64: the container image's architecture); the image must be glibc-based
(not Alpine or other musl images), amd64 or arm64. The process is --pid N, as the container
numbers it (default 1: the exec-form ENTRYPOINT ["dotnet", ...] of Visual Studio's Dockerfiles; with
'init: true' pid 1 is docker-init, which attaching can't use, so --pid is then required). The
engine is the one DOCKER_HOST (else DOCKER_CONTEXT) names, as for docker itself; eyedbgd runs
$EYEDBG_DOCKER or the docker on its PATH. eyedbg never reads the container's environment,
command or arguments.
  --map REMOTE=LOCAL (repeatable) maps a directory in the container to a host directory, so
  breakpoints by host path and the frames and source excerpts of the stop use host paths: the
  PDBs of an image built from Visual Studio's template name sources under /src, so the default
  is /src = the compose project's directory (the container's compose working_dir label, when it
  is a host directory that exists); --map replaces that default. A line breakpoint outside the
  map is refused, never sent, so without a map (and no such directory) add --map. LOCAL is
  relative to the current directory.
  --group NAME labels the session as a member of a group (a compose project).
Breakpoints hit while attached stop the whole service: its HTTP callers time out, its docker
healthcheck turns unhealthy after retries x interval + timeout (the session shows it), and a Kafka
consumer leaves its group after max.poll.interval.ms (5 minutes by default): keep stops short. A
lost host 'docker exec' (a killed eyedbgd, a crashed terminal) leaves netcoredbg and the frozen
program inside the container; 'docker restart NAME' clears it. In verified runs a Release build's
line breakpoints did not bind when attached (pause and stacks work; function breakpoints and
exception stops are unverified there): attach to a Debug build, e.g. an image built with
--build-arg BUILD_CONFIGURATION=Debug. 'eyedbg detach' leaves the container running.
Docker access is the trust boundary: eyedbg attaches wherever your docker user may. Linux
containers on docker 24 to 26 (Docker Desktop on macOS, docker.io on Linux) are verified; Windows
hosts, podman, rootless docker, remote engines and emulated architectures are not.

Output: the session's state, like 'eyedbg start'. Exits 1 for no such process or container.` + adapterHelp + dumpHelp

// attachExample is the examples of attach.
const attachExample = `  eyedbg attach dotnet --pid 4242
  eyedbg attach dotnet --pid 4242 --bp 'Worker.cs@"ProcessNext()"'
  eyedbg attach dotnet --pid 4242 --exceptions all --json
  eyedbg attach dotnet --pid 4242 --adapter sharpdbg
  eyedbg attach dotnet --container myapp-web-1 --bp Controllers/OrderController.cs:42
  eyedbg attach dotnet --container myapp-web-1 --map /src=. --group myapp
  eyedbg attach dotnet --container 3f2a9c1b7d4e --pid 7`

func newAttachCommand(info version.Info, g *globals) *cobra.Command {
	var (
		sf  sessionFlags
		cf  containerFlags
		pid int
	)

	cmd := &cobra.Command{
		Use:     "attach <lang> (--pid N | --container NAME)",
		Short:   "Debug a program that is already running",
		Long:    attachLong,
		Example: attachExample,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cf.ref != "" {
				if cmd.Flags().Changed("pid") && pid < 1 {
					return errors.New("--pid is a process id from 1, as the container numbers it")
				}

				return attachContainer(cmd, info, g, &sf, args[0], cf, pid)
			}

			if len(cf.maps) > 0 || cf.group != "" {
				return errors.New("--map and --group go with --container")
			}

			if pid < 1 {
				return errors.New("attach needs --pid N (a process id from 1), or --container NAME")
			}

			params := api.StartParams{Lang: args[0], Attach: &api.AttachSpec{PID: pid}}
			if err := sf.params(g, &params); err != nil {
				return err
			}

			return startSession(cmd, info, g, params, sf.timeout)
		},
	}

	f := cmd.Flags()
	f.IntVar(&pid, "pid", 0, "id of the process to attach to (with --container: as numbered inside the container, default 1)")
	f.StringVar(&cf.ref, "container", "", "attach to a process in this docker container (an id or a name) instead of a host process")
	f.StringArrayVar(&cf.maps, "map", nil, "with --container: map REMOTE=LOCAL, a directory in the container to a host directory (repeatable; default /src = the compose project directory)")
	f.StringVar(&cf.group, "group", "", "with --container: label the session as a member of this group (a compose project name)")
	sf.register(cmd)

	return cmd
}

// attachContainer attaches to one container with the container.attach
// method (never session.start: an older daemon would drop the container and
// attach to a host process).
func attachContainer(cmd *cobra.Command, info version.Info, g *globals, sf *sessionFlags, lang string, cf containerFlags, pid int) error {
	sp := api.StartParams{Lang: lang}
	if err := sf.params(g, &sp); err != nil {
		return err
	}

	maps, err := parseMaps(cf.maps)
	if err != nil {
		return err
	}

	params := api.ContainerAttachParams{
		DumpSpec: sp.DumpSpec, Client: sp.Client, Lang: lang, Group: cf.group,
		Members: []api.ContainerSpec{{
			Engine: api.ContainerEngine{Host: os.Getenv(envDockerHost), Context: os.Getenv(envDockerContext)},
			Ref:    cf.ref, PID: pid, Map: maps,
		}},
		Breakpoints: sp.Breakpoints, Exceptions: sp.Exceptions, LeasePolicy: sp.LeasePolicy, NoRecord: sp.NoRecord,
		Adapter: sp.Adapter, Wait: sp.Wait,
	}

	p, err := daemon.DefaultPaths()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), startBuildBudget+sf.timeout+callSlack)
	defer cancel()

	cl, _, err := daemon.Connect(ctx, p, info, daemon.OptionsFromEnv())
	if err != nil {
		return err
	}
	defer cl.Close()

	var res api.ContainerAttachResult
	if err := cl.Call(ctx, api.MethodContainerAttach, params, &res); err != nil {
		return containerCallError(err)
	}

	snap, err := soleMember(res)
	if err != nil {
		return err
	}

	return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
}

// containerCallError is the error of a container.attach call as the command
// reports it: a daemon that doesn't know the method predates containers.
func containerCallError(err error) error {
	if api.CodeOf(err) == api.CodeUnknownMethod {
		return api.NewError(api.CodeVersionMismatch, "the running eyedbgd is too old to attach to containers",
			"stop it with 'eyedbg daemon stop' (--force ends its sessions); the next command starts a current one")
	}

	return err
}

// soleMember is the snapshot of a container.attach of one member, or its
// error.
func soleMember(res api.ContainerAttachResult) (api.Snapshot, error) {
	if len(res.Members) != 1 {
		return api.Snapshot{}, api.NewError(api.CodeInternal, fmt.Sprintf("eyedbgd answered %d members for one container", len(res.Members)), "")
	}

	m := res.Members[0]

	switch {
	case m.Error != nil:
		return api.Snapshot{}, m.Error
	case m.Session == nil:
		return api.Snapshot{}, api.NewError(api.CodeInvalidRequest, "container "+m.Container+" is not a candidate: "+m.Skipped, "")
	default:
		return *m.Session, nil
	}
}

// parseMaps parses --map REMOTE=LOCAL flags: REMOTE is up to the first '=';
// LOCAL is made absolute against the current directory.
func parseMaps(specs []string) ([]api.PathMapping, error) {
	var out []api.PathMapping

	for _, spec := range specs {
		remote, local, ok := strings.Cut(spec, "=")
		if !ok || remote == "" || local == "" {
			return nil, fmt.Errorf("invalid --map %q: want REMOTE=LOCAL (for example /src=./app)", spec)
		}

		out = append(out, api.PathMapping{Remote: remote, Local: absPath(local)})
	}

	return out, nil
}

func newDetachCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "detach",
		Short: "End an attached session, leaving its program running",
		Long: `Detach from a session made by 'eyedbg attach': the debugger lets go (breakpoints stop applying,
a stopped program resumes), the session ends, and the program keeps running. Sessions made by
'eyedbg start' or 'eyedbg test' can't be detached from (INVALID_REQUEST, exit 1): 'eyedbg stop'
ends them. While the program is live this needs the control lease (LEASE_HELD, exit 2, if the
policy keeps you from taking it).

Output: "session ID detached; pid N keeps running". Returns within a few seconds.` + sessionHelp,
		Example: `  eyedbg detach
  eyedbg detach -s s-k3f9 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res api.SessionInfo
			if err := call(cmd, info, daemonCallTimeout, api.MethodSessionDetach, g.ref(), &res); err != nil {
				return err
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Schema  int             `json:"schema"`
					Session api.SessionInfo `json:"session"`
				}{jsonSchemaVersion, res})
			}

			return writeEnded(cmd.OutOrStdout(), res)
		},
	}
}

// writeEnded says a session ended, or that it was detached from and its
// program keeps running (not when the program had exited by itself).
func writeEnded(w io.Writer, info api.SessionInfo) error {
	return writeText(w, "session "+info.ID+" "+endedPhrase(info)+"\n")
}

// endedPhrase is what became of a session that was stopped, without its id:
// "ended"; "detached; pid N keeps running" or "detached; container NAME keeps
// running" for an attached one, which the program or container outlives; and
// for an app launched in a container (fast mode), which dies with its session,
// "app terminated; container NAME idles".
func endedPhrase(info api.SessionInfo) string {
	detached := info.Mode == api.ModeAttach && strings.HasPrefix(info.EndReason, "detached by")

	switch {
	case detached && info.Container != nil:
		return "detached; container " + info.Container.Name + " keeps running"
	case detached:
		return "detached; pid " + strconv.Itoa(info.PID) + " keeps running"
	case info.Container != nil && info.Container.Launched:
		return "app terminated; container " + info.Container.Name + " idles"
	default:
		return "ended"
	}
}
