// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/session"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// 'eyedbg compose launch' (docs/adr/0021, D18): put services of a running
// compose stack in fast mode, or rebuild the ones already there, and launch
// their apps under the debugger. It changes the user's running stack, so the
// order of its stages is the safety property: everything that can fail
// without touching docker or the daemon comes first (inspect, checks, the
// build into a staging directory), and nothing is stopped, replaced or
// recreated until the build succeeded.

const composeLaunchLong = `Debug the .NET services of a running docker compose stack in "fast mode", the way Visual Studio and Rider do, with no edit
to any Dockerfile or compose file: eyedbg builds each service's project in Debug on the host, makes the service's
container idle with that build mounted (read-only, at the container's own working directory), and launches the app inside
the container under the debugger. Line breakpoints bind (a Release image's never do), startup code can be stopped in
(--bp on a startup line, --stop-on-entry), and the app gets the container's own environment, user and working directory:
eyedbg never reads, copies or passes them.

What it does, in order: for each selected service (the named ones, else every running service of the stack that fits) it
inspects the running container, then builds the service's project once ('dotnet publish -c Debug' on this machine, in
your environment, into a staging directory under eyedbg's home: the project is found as the unique <assembly>.csproj
under the compose directory, or named with --dotnet-project SERVICE=PATH; each service's project is printed on stderr
before its build starts, and is "project" in --json), and only after the build succeeded it carries your
breakpoints and exception mode over from the service's live session, ends that session, replaces the service's files
with the build and launches the app again. A compile error therefore changes nothing: the running app keeps running.

ENTERING fast mode RECREATES the service's container ('docker compose up -d --no-deps --force-recreate --no-build' for
the named services only, with the compose files the container was created from plus an override eyedbg writes under its
home): anything the container wrote to its own file system outside volumes is lost, and the new container is created
with this shell's environment and --env-file files. Dependents are never touched. Re-running the command for a service
already in fast mode is the rebuild cycle: it publishes, replaces the files and relaunches the app, in the SAME container.
--no-build relaunches from the last build.

A service in fast mode runs only while its eyedbg session runs its app. 'eyedbg stop', 'compose stop' or the daemon
ending leave the container idle (the app is killed, with no graceful shutdown): the service is DOWN, its healthcheck
turns unhealthy, until the next 'compose launch' or 'compose restore'. The app's output is in 'eyedbg output -s ID' and
'eyedbg compose events --kind output', not in 'docker compose logs'. Its directory is read-only: an app that writes next
to itself fails. 'compose restore' puts services back as built.

Refused with the reason (skipped when you named no service): an entrypoint that isn't exec-form 'dotnet X.dll' (shell
wrappers, apphosts, an IDE's debugger worker: after an IDE's fast mode, 'docker compose up -d SERVICE' recreates the
container as built), a command: or image CMD (its arguments would be dropped, and eyedbg never reads them), a working
directory that can't hold the build, an image without 'tail' (chiseled, distroless), a service with several containers,
and netcoredbg not installed for the image's platform ('eyedbg adapters install netcoredbg --platform linux/arm64').
A service that depends on another with 'condition: service_healthy' fails when compose itself starts it while the
other idles: launch that one first.

Building a project runs its code on this machine, so a project a container could have planted is never built unasked:
a search skips every file under (or at) a read-write bind mount, or a local volume bound to a host directory
(driver_opts type none), of any container of the stack, resolved as real paths (a read-only mount can't be written
by its container and is searched); when the compose directory itself is under one, or one isn't a path on this
machine (a remote engine's, a Docker Desktop VM path on Windows), nothing is searched. Such a service is refused (skipped without names) with a hint to name the project yourself:
--dotnet-project SERVICE=PATH, which is built wherever it lies in the compose directory, as it is your choice. A
service's own fast-mode labels (dev.izzat.eyedbg.fast.*) are believed only when eyedbg's override file for the
project records the same ones: any container label is untrusted input unless eyedbg's own files corroborate it.

Discovery is one 'docker compose ps' with -f FILE (repeatable), -p NAME and --project-directory DIR, as for
'compose attach'; the Debug build maps /src to the compose directory, so breakpoints are by host path (there is no --map).
The compose directory is the one the container's labels name, believed only when a compose file (.yml/.yaml) its
labels list is in it (an image's own labels can claim any directory, so a container made by plain 'docker run' is
refused) and, when --project-directory or -f is given, when it is the directory they name (for -f, the first
file's). Not supported: compose files outside the project directory (--project-directory DIR with -f FILE outside
DIR); keep a compose file inside the project directory, or run compose without --project-directory.
--bp (repeatable) goes in before the app starts, for every service; breakpoints, conditions, logpoints and the exception
mode your sessions of these services had are carried over. One run per project at a time.

Output: per service the session id (the group is the compose project) and what happened (built in Ns, recreated, N
breakpoints carried), the path map, each --bp's result, the build log and override paths, and the next commands. Exit 0
when at least one service launched; otherwise the first failure's code (BUILD_FAILED, LEASE_HELD, ATTACH_FAILED,
INVALID_REQUEST, ADAPTER_NOT_INSTALLED, ...). Blocks for the build (up to 15 minutes per project) and the recreate. Starts
the daemon if needed. Needs docker with the compose plugin and the .NET SDK on this machine.` + groupHelp

const composeLaunchExample = `  eyedbg compose launch                                  # every service that fits, in the stack in this directory
  eyedbg compose launch producer consumer --bp Producer/Program.cs:6
  eyedbg compose launch producer                         # after an edit: rebuild, same container, breakpoints carried
  eyedbg compose launch --no-build --stop-on-entry producer
  eyedbg compose launch -f compose.yml -p myapp --dotnet-project producer=src/Producer/Producer.csproj`

// composeLaunchFlags are the flags of 'eyedbg compose launch'.
type composeLaunchFlags struct {
	files          []string
	project        string
	projectDir     string
	dotnetProjects []string
	envFiles       []string
	bps            []string
	exceptions     string
	leasePolicy    string
	noRecord       bool
	noBuild        bool
	stopOnEntry    bool
}

func newComposeLaunchCommand(info version.Info, g *globals, deps composeDeps) *cobra.Command {
	var f composeLaunchFlags

	cmd := &cobra.Command{
		Use:     "launch [SERVICE...]",
		Short:   "Build and launch services in fast mode under the debugger",
		Long:    composeLaunchLong,
		Example: composeLaunchExample,
		RunE: func(cmd *cobra.Command, args []string) error {
			return composeLaunch(cmd, info, g, deps, &f, args)
		},
	}

	fl := cmd.Flags()
	addComposeProjectFlags(fl, &f.files, &f.project, &f.projectDir)
	fl.StringArrayVar(&f.dotnetProjects, "dotnet-project", nil, "SERVICE=PATH: the project that builds a service (relative to the compose directory; repeatable)")
	fl.StringArrayVar(&f.envFiles, "env-file", nil, "env file compose interpolates when the containers are recreated, as for docker compose --env-file (repeatable)")
	fl.StringArrayVar(&f.bps, "bp", nil, `breakpoint FILE:LINE, FILE@"TEXT" or func:NAME, set before the apps start, for every service (repeatable)`)
	fl.StringVar(&f.exceptions, "exceptions", "", "which exceptions stop a program for you: all, uncaught or none (default: what the service's sessions had)")
	fl.StringVar(&f.leasePolicy, "lease-policy", string(api.LeaseFree), "who may take a session's control lease: free, handoff or human-priority")
	fl.BoolVar(&f.noRecord, "no-record", false, "don't record the sessions' control events (also EYEDBG_NO_RECORD=1)")
	fl.BoolVar(&f.noBuild, "no-build", false, "don't build: relaunch services already in fast mode from their last build")
	fl.BoolVar(&f.stopOnEntry, "stop-on-entry", false, "stop every app before its first line runs")

	return cmd
}

// launchRequest is what the flags of 'compose launch' ask for.
type launchRequest struct {
	opts     container.ComposeOptions
	projects map[string]string // service -> --dotnet-project
	envFiles []string
	bps      []breakpointArg
	params   api.ContainerLaunchParams
	noBuild  bool
	entry    bool
}

// request validates the flags and the service names; nothing here touches
// docker or the daemon.
func (f *composeLaunchFlags) request(g *globals, names []string) (launchRequest, error) {
	for _, n := range names {
		if err := container.ValidateService(n); err != nil {
			return launchRequest{}, err
		}
	}

	req := launchRequest{
		opts:     container.ComposeOptions{Files: f.files, ProjectName: f.project, ProjectDirectory: f.projectDir},
		envFiles: f.envFiles, noBuild: f.noBuild, entry: f.stopOnEntry,
	}

	if err := req.opts.Validate(); err != nil {
		return launchRequest{}, err
	}

	if err := container.ValidateEnvFiles(f.envFiles); err != nil {
		return launchRequest{}, err
	}

	var err error
	if req.projects, err = parseDotnetProjects(f.dotnetProjects); err != nil {
		return launchRequest{}, err
	}

	policy, err := api.ParseLeasePolicy(f.leasePolicy)
	if err != nil {
		return launchRequest{}, err
	}

	req.params = api.ContainerLaunchParams{
		DumpSpec: api.DumpSpec{Budget: g.budget}, Client: g.clientID(), Lang: dotnet.Language, LeasePolicy: policy,
		NoRecord: f.noRecord || os.Getenv(envNoRecord) == "1",
	}

	if f.exceptions != "" {
		if req.params.Exceptions, err = api.ParseExceptionMode(f.exceptions); err != nil {
			return launchRequest{}, err
		}
	}

	for _, b := range f.bps {
		spec, err := parseLocation(b)
		if err != nil {
			return launchRequest{}, err
		}

		req.bps = append(req.bps, breakpointArg{location: b, spec: spec})
	}

	return req, nil
}

// parseDotnetProjects reads --dotnet-project SERVICE=PATH values.
func parseDotnetProjects(kvs []string) (map[string]string, error) {
	out := map[string]string{}

	for _, kv := range kvs {
		service, path, ok := strings.Cut(kv, "=")
		if !ok || path == "" {
			return nil, fmt.Errorf("invalid --dotnet-project %q: want SERVICE=PATH", kv)
		}

		if err := container.ValidateService(service); err != nil {
			return nil, err
		}

		if _, dup := out[service]; dup {
			return nil, fmt.Errorf("--dotnet-project names service %s twice", service)
		}

		out[service] = path
	}

	return out, nil
}

// composeLaunch is 'compose launch'.
func composeLaunch(cmd *cobra.Command, info version.Info, g *globals, deps composeDeps, f *composeLaunchFlags, names []string) error {
	req, err := f.request(g, names)
	if err != nil {
		return err
	}

	psCtx, cancelPS := context.WithTimeout(cmd.Context(), container.ComposeTimeout+callSlack)
	defer cancelPS()

	rows, err := deps.ps(psCtx, os.Getenv(envDockerHost), os.Getenv(envDockerContext), req.opts)
	if err != nil {
		return err
	}

	picked, project, err := selectServices(rows, names)
	if err != nil {
		return err
	}

	if err := checkProjectNames(req.projects, picked); err != nil {
		return err
	}

	base, err := newStackRun(cmd, info, g, deps, project)
	if err != nil {
		return err
	}

	run := &launchRun{stackRun: base, req: req, implicit: len(names) == 0}
	for _, p := range picked {
		run.svcs = append(run.svcs, &launchSvc{row: p})
	}

	if err := run.takeLock(); err != nil {
		return err
	}
	defer run.release()
	defer func() { run.prune(cmd.Context()) }()
	defer run.removeStage()

	if runErr := run.execute(cmd.Context()); runErr != nil {
		run.failLive(runErr)
	}

	members := run.members()

	if err := writeComposeLaunch(cmd.OutOrStdout(), project, members, run.view(), g.json, workDir()); err != nil {
		return err
	}

	return launchFailure(members)
}

// checkProjectNames refuses a --dotnet-project that names a service the run
// doesn't select.
func checkProjectNames(projects map[string]string, picked []container.ComposeService) error {
	for service := range projects {
		if !slices.ContainsFunc(picked, func(p container.ComposeService) bool { return p.Service == service }) {
			return api.NewError(api.CodeInvalidRequest, "--dotnet-project names service "+service+", which this run doesn't launch",
				"name it among the services to launch, and it must be running")
		}
	}

	return nil
}

// launchFailure is the error of a launch that launched nothing: the first
// failure's, else INVALID_REQUEST when every service was skipped. It has
// already been shown.
func launchFailure(members []launchedMember) error {
	for i := range members {
		if members[i].Session != nil {
			return nil
		}
	}

	for i := range members {
		if members[i].Error != nil {
			return &reportedError{err: members[i].Error}
		}
	}

	return &reportedError{err: api.NewError(api.CodeInvalidRequest, "no service to launch",
		"every running service was skipped (see why above); name services to try them anyway: 'eyedbg compose launch SERVICE...'")}
}

// launchSvc is one service of a launch run.
type launchSvc struct {
	row container.ComposeService
	fi  container.FastInfo
	// entry: the service enters fast mode (its container is recreated);
	// otherwise it is in fast mode with this project's override already.
	entry bool
	// dll and appDir are what the app is: its assembly and the directory the
	// build is mounted at.
	dll, appDir string
	// project is the project file (absolute) and projectRel its path below
	// the compose directory.
	project, projectRel string
	// out is the staged build to mirror; buildFailed: its build failed.
	out         string
	built       time.Duration
	buildFailed bool
	carry       carry
	// downed: the service's app was stopped by this run; recreated: its
	// container was.
	downed, recreated bool
	res               launchedMember
}

// live reports whether the service is still in the run.
func (s *launchSvc) live() bool { return s.res.Skipped == "" && s.res.Error == nil }

// launchRun is one 'compose launch'.
type launchRun struct {
	*stackRun

	req      launchRequest
	implicit bool
	svcs     []*launchSvc

	// workDir is the compose directory, resolved: what the path map and the
	// build map /src to.
	workDir  string
	pm       *session.PathMap
	stage    string
	buildLog string
	log      *os.File

	// rw are the host files and directories the project's containers can
	// write (read at the first search for a project: rwRead, rwErr).
	rw     []container.WritableSource
	rwRead bool
	rwErr  error
}

// live is the services still in the run, in order.
func (r *launchRun) live() []*launchSvc {
	var out []*launchSvc

	for _, s := range r.svcs {
		if s.live() {
			out = append(out, s)
		}
	}

	return out
}

// refuse takes a service out of the run because it can't be launched: skipped
// when no service was named and the refusal is about the service (not about
// docker or the adapter), else failed.
func (r *launchRun) refuse(s *launchSvc, err error) {
	if r.implicit && api.CodeOf(err) == api.CodeInvalidRequest {
		s.res.Skipped = refuseText(err)

		return
	}

	r.fail(s, err)
}

// fail takes a service out of the run with an error.
func (r *launchRun) fail(s *launchSvc, err error) {
	s.res.Error = jsonErr(err)
}

// failLive fails every service still in the run that didn't launch.
func (r *launchRun) failLive(err error) {
	for _, s := range r.live() {
		if s.res.Session == nil {
			r.fail(s, err)
		}
	}
}

// execute runs the stages, under the project's lock. The order is the
// contract: nothing is stopped, replaced or recreated before the build
// succeeded, the old app is stopped before its files are replaced, and a
// service whose files could not be replaced is never launched. An error is
// one that ends the whole run (the daemon can't be reached, a breakpoint
// can't be set); a service's own failure is in its result.
func (r *launchRun) execute(ctx context.Context) error {
	r.classify(ctx)

	if err := r.settle(); err != nil || len(r.live()) == 0 {
		return err
	}

	r.resolveProjects(ctx)
	r.build(ctx)

	if len(r.live()) == 0 {
		return nil
	}

	cl, list, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer cl.Close()

	// From here on the stack changes. An interrupt would leave a service
	// stopped or half replaced, so the stages run to their own bounds.
	ctx = context.WithoutCancel(ctx)

	r.carryAndStop(ctx, cl, list)
	r.mirror()
	r.recreate(ctx)
	r.launch(ctx, cl)

	return nil
}

// removeStage removes the run's staging directory, whatever happened.
func (r *launchRun) removeStage() {
	if r.stage != "" {
		_ = artifacts.RemoveStage(r.projectDir, r.stage)
	}
}

// connect starts or finds the daemon (before anything changes, so a daemon
// that can't be used fails the run early) and lists its sessions.
func (r *launchRun) connect(ctx context.Context) (*daemon.Client, []api.SessionInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, startBuildBudget+callSlack)
	defer cancel()

	cl, err := r.dial(ctx)
	if err != nil {
		return nil, nil, err
	}

	list, err := sessionList(ctx, cl)
	if err != nil {
		_ = cl.Close()

		return nil, nil, err
	}

	return cl, list, nil
}

// resolvedPath is p with its symlinks resolved, or p when it can't be.
func resolvedPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}

	return p
}
