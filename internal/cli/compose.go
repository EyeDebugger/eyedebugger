// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// envGroup names the default group of the compose commands.
const envGroup = "EYEDBG_GROUP"

// composeStopUse is the name of 'compose stop'.
const composeStopUse = "stop"

// maxComposeMembers is the most containers one compose attach starts (the
// daemon's own bound).
const maxComposeMembers = 64

// composeDeps are what the compose commands reach outside the process; tests
// replace them.
type composeDeps struct {
	// ps lists the containers of a compose project in the caller's working
	// directory and environment: the one compose call eyedbg makes.
	ps func(ctx context.Context, host, dockerContext string, o container.ComposeOptions) ([]container.ComposeService, error)
	// now is the time stops are measured against.
	now func() time.Time
	// stack is what 'compose launch' and 'compose restore' reach besides.
	stack stackDeps
}

// defaultComposeDeps are the real docker and the real clock.
func defaultComposeDeps() composeDeps {
	return composeDeps{ps: composePS, now: time.Now, stack: defaultStackDeps()}
}

// composePS runs docker compose ps through the docker the engine names.
func composePS(ctx context.Context, host, dockerContext string, o container.ComposeOptions) ([]container.ComposeService, error) {
	e, err := container.NewEngine(host, dockerContext)
	if err != nil {
		return nil, err
	}

	return e.ComposePS(ctx, o)
}

const composeLong = `Debug the .NET services of a running docker compose stack as one group, without editing any compose file:
'eyedbg compose attach' attaches to every service at once (one session each, labeled with the compose project as
their group), 'compose bp' puts breakpoints, by host path, in any of them, 'compose wait' blocks until whichever stops,
you inspect and step that one with the ordinary commands and its -s ID, and 'compose stop' lets go of them all. The
stack keeps running: attaching changes nothing in it but copies netcoredbg into the containers.

  attach [SERVICE...]  attach to the running services (the .NET ones, or the named ones)
  wait                 block until any member stops; show where
  events               the members' event logs merged into one stream
  bp add|ls|rm         breakpoints in every member at once
  stop                 end every member's session (the containers keep running)

Needs docker with the compose plugin, and netcoredbg installed for the containers' platform ('eyedbg adapters
install netcoredbg --platform linux/arm64', or linux/amd64): see 'eyedbg help compose attach'. Which group the
other commands act on: -g/--group NAME, else $` + envGroup + `, else the only group that has sessions (an error
lists them when there are several). -s/--session doesn't apply: the commands act on a whole group; to act on one
member afterwards, use any ordinary command with its -s ID (status, vars, eval, next, continue, ...).

Stopping at a breakpoint stops the whole service, not one thread of it: HTTP callers time out, docker's healthcheck
turns the container unhealthy after about retries x interval + timeout, and a Kafka consumer leaves its group after
max.poll.interval.ms (5 minutes by default). Snapshots of a stopped service say how long it has been stopped, and
note when these thresholds pass: keep stops short.

Line breakpoints bind in Debug builds. In verified runs a Release build's line breakpoints did not bind when attached
(pause and stacks work): build the images with --build-arg BUILD_CONFIGURATION=Debug (the Visual Studio Dockerfile
template has the argument) for breakpoints.

Docker access is the trust boundary: eyedbg attaches wherever your docker user may, to the engine DOCKER_HOST (else
DOCKER_CONTEXT) names. eyedbg never reads a container's environment, command or arguments, nor 'docker compose
config' (it inlines env_file values): its one compose call is 'docker compose ps'. Linux containers on docker 24 to
26 are verified; Windows hosts, podman, rootless docker, remote engines and emulated architectures are not.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`

const composeExample = `  eyedbg compose attach                           # every running .NET service of the stack in this directory
  eyedbg compose attach producer consumer --bp Producer/Program.cs:42
  eyedbg compose wait                             # block until one of them stops
  eyedbg compose bp add Consumer/Program.cs:17
  eyedbg compose events --since 's-k3f9:12,s-m2n4:4'
  eyedbg compose stop`

// newComposeCommand returns 'eyedbg compose'.
func newComposeCommand(info version.Info, g *globals, deps composeDeps) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "compose",
		Short:   "Debug the .NET services of a docker compose stack as one group",
		Long:    composeLong,
		Example: composeExample,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(
		newComposeAttachCommand(info, g, deps),
		newComposeWaitCommand(info, g, deps),
		newComposeEventsCommand(info, g),
		newComposeBreakpointCommand(info, g),
		newComposeStopCommand(info, g),
		newComposeLaunchCommand(info, g, deps),
		newComposeRestoreCommand(info, g, deps),
	)

	return cmd
}

// groupHelp is appended to every compose command that acts on a group.
const groupHelp = `
Which group: -g/--group NAME, else $` + envGroup + `, else the only group that has sessions (a compose project name; an
error lists them if there are several). Who you are: --as agent[:NAME] or human[:NAME], else $EYEDBG_CLIENT, else agent.`

// addGroupFlag adds -g/--group to cmd.
func addGroupFlag(cmd *cobra.Command, group *string, persistent bool) {
	fl := cmd.Flags()
	if persistent {
		fl = cmd.PersistentFlags()
	}

	fl.StringVarP(group, "group", "g", "", "the group to act on: a compose project name (default: $"+envGroup+", else the only group)")
}

// groupCallError is the error of a group method as the command reports it:
// a daemon that doesn't know the method predates groups.
func groupCallError(err error) error {
	if api.CodeOf(err) == api.CodeUnknownMethod {
		return api.NewError(api.CodeVersionMismatch, "the running eyedbgd is too old for compose groups",
			"stop it with 'eyedbg daemon stop' (--force ends its sessions); the next command starts a current one")
	}

	return err
}

// boundedCall runs one call with its own timeout.
func boundedCall(ctx context.Context, cl *daemon.Client, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, daemonCallTimeout)
	defer cancel()

	return cl.Call(ctx, method, params, result)
}

// sessionList is the daemon's sessions.
func sessionList(ctx context.Context, cl *daemon.Client) ([]api.SessionInfo, error) {
	var list []api.SessionInfo
	if err := boundedCall(ctx, cl, api.MethodSessionList, nil, &list); err != nil {
		return nil, err
	}

	return list, nil
}

// explicitGroup is the group -g or $EYEDBG_GROUP names, checked; "" when
// neither does.
func explicitGroup(flag string) (string, error) {
	name := firstNonEmpty(flag, os.Getenv(envGroup))
	if name == "" {
		return "", nil
	}

	if err := api.CheckGroup(name); err != nil {
		return "", err
	}

	return name, nil
}

// onlyGroup is the one group the sessions belong to; none or several are
// errors that say what to do.
func onlyGroup(list []api.SessionInfo) (string, error) {
	var groups []string

	for i := range list {
		if g := list[i].Group; g != "" && list[i].State != api.StateLost && !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	}

	sort.Strings(groups)

	switch len(groups) {
	case 0:
		return "", api.NewError(api.CodeNoSession, "no compose group has a session", "attach to a stack with 'eyedbg compose attach'")
	case 1:
		return groups[0], nil
	default:
		return "", api.NewError(api.CodeInvalidRequest, "several groups exist: "+strings.Join(groups, ", "), "pick one with -g NAME or "+envGroup)
	}
}

// resolveGroup is the group a command acts on; the sessions are only listed
// when nothing names it.
func resolveGroup(ctx context.Context, cl *daemon.Client, flag string) (string, error) {
	name, err := explicitGroup(flag)
	if err != nil || name != "" {
		return name, err
	}

	list, err := sessionList(ctx, cl)
	if err != nil {
		return "", err
	}

	return onlyGroup(list)
}

// groupMembers resolves the group and returns its members, oldest first:
// every session of the group that isn't lost, live or exited.
func groupMembers(ctx context.Context, cl *daemon.Client, flag string) (string, []api.SessionInfo, error) {
	group, err := explicitGroup(flag)
	if err != nil {
		return "", nil, err
	}

	list, err := sessionList(ctx, cl)
	if err != nil {
		return "", nil, err
	}

	if group == "" {
		if group, err = onlyGroup(list); err != nil {
			return "", nil, err
		}
	}

	var members []api.SessionInfo

	for i := range list {
		if list[i].Group == group && list[i].State != api.StateLost {
			members = append(members, list[i])
		}
	}

	if len(members) == 0 {
		return "", nil, api.NewError(api.CodeNoSession, "no group "+group, "see 'eyedbg sessions'")
	}

	return group, members, nil
}

// liveMembers are the members whose program can still be debugged.
func liveMembers(members []api.SessionInfo) []api.SessionInfo {
	return slices.DeleteFunc(slices.Clone(members), func(s api.SessionInfo) bool { return s.State == api.StateExited })
}

// firstFailure is the first of errs that is set, nil when none is.
func firstFailure(errs ...*api.Error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}

	return nil
}

// ---------------------------------------------------------------- attach

// composeAttachFlags are the flags of 'eyedbg compose attach'.
type composeAttachFlags struct {
	files       []string
	project     string
	projectDir  string
	maps        []string
	bps         []string
	exceptions  string
	leasePolicy string
	noRecord    bool
}

const composeAttachLong = `Attach to the running services of a docker compose stack: one session per service, each a member of the group named
after the compose project, with breakpoints, exception stops and the lease policy as for 'eyedbg attach' (--bp,
--exceptions, --lease-policy, --no-record). It never waits for a stop: use 'eyedbg compose wait'.

Discovery is one 'docker compose ps' run in the current directory with your environment (name the project with -f
FILE (repeatable), -p NAME and --project-directory DIR, as for docker compose; COMPOSE_* variables work too), on the
engine DOCKER_HOST (else DOCKER_CONTEXT) names. Only running containers count; the stack must be one project.
Without SERVICE names, only services whose main process (pid 1) is dotnet are attached; the others are listed as
skipped, with why. Named services are always attempted. A service with 'init: true' has docker-init as pid 1, which
attaching can't use: it fails, and 'eyedbg attach dotnet --container NAME --pid N' attaches to its dotnet process.
A service named but not running is INVALID_REQUEST, listing the running ones.

Each attach copies eyedbg's own netcoredbg into the container (docker cp; a read-only root filesystem fails) and
runs it there with 'docker exec -i', as the container's user: install it once per platform with 'eyedbg adapters
install netcoredbg --platform linux/arm64' (or linux/amd64: the images' architecture; ADAPTER_NOT_INSTALLED says
which). The images must be glibc-based (not Alpine). The program is never touched: 'compose stop' leaves the
services running.

--map REMOTE=LOCAL (repeatable) maps a directory in the containers to a host directory, so breakpoints by host path
and the frames and source excerpts of a stop use host paths. Without it each service maps /src to the compose project
directory (the Visual Studio Dockerfile template builds in /src); --map replaces that default for every service. A
line breakpoint outside a service's map is skipped for it, never sent. --bp adds the breakpoint to every attached
service, in order; the output says what each did with it ("verified", "pending" when its module isn't loaded or has no
such line, or skipped).

Output: the group, one line per service (attached with its session id, skipped, or failed), the path map in use, each
--bp's result per service, a note on what stopping does to a service, and the next commands. Exit 0 when at least one
service attached; otherwise the first failure's code (INVALID_REQUEST, ATTACH_FAILED, ADAPTER_NOT_INSTALLED, ...), or
INVALID_REQUEST "no .NET services running" when every service was skipped. A session that already exists for a
container is refused for that service. Starts the daemon if needed.`

const composeAttachExample = `  eyedbg compose attach
  eyedbg compose attach producer consumer --bp Producer/Program.cs:42
  eyedbg compose attach -f compose.yml -f compose.dev.yml -p myapp --map /src=.
  eyedbg compose attach --exceptions all --json`

func newComposeAttachCommand(info version.Info, g *globals, deps composeDeps) *cobra.Command {
	var f composeAttachFlags

	cmd := &cobra.Command{
		Use:     "attach [SERVICE...]",
		Short:   "Attach to the running .NET services of a compose stack",
		Long:    composeAttachLong,
		Example: composeAttachExample,
		RunE: func(cmd *cobra.Command, args []string) error {
			return composeAttach(cmd, info, g, deps, &f, args)
		},
	}

	fl := cmd.Flags()
	fl.StringArrayVarP(&f.files, "file", "f", nil, "compose file (repeatable), as for docker compose -f")
	fl.StringVarP(&f.project, "project-name", "p", "", "compose project name, as for docker compose -p")
	fl.StringVar(&f.projectDir, "project-directory", "", "compose project directory, as for docker compose --project-directory")
	fl.StringArrayVar(&f.maps, "map", nil, "map REMOTE=LOCAL, a directory in the containers to a host directory (repeatable; default /src = the compose project directory)")
	fl.StringArrayVar(&f.bps, "bp", nil, `breakpoint FILE:LINE, FILE@"TEXT" or func:NAME added to every service (repeatable)`)
	fl.StringVar(&f.exceptions, "exceptions", "", "which exceptions stop a program for you: all, uncaught or none (see 'eyedbg bp exceptions')")
	fl.StringVar(&f.leasePolicy, "lease-policy", string(api.LeaseFree), "who may take a session's control lease: free, handoff or human-priority")
	fl.BoolVar(&f.noRecord, "no-record", false, "don't record the sessions' control events (also EYEDBG_NO_RECORD=1)")

	return cmd
}

// composeAttach is 'compose attach'.
func composeAttach(cmd *cobra.Command, info version.Info, g *globals, deps composeDeps, f *composeAttachFlags, names []string) error {
	req, bps, err := f.request(g, names)
	if err != nil {
		return err
	}

	host, dockerContext := os.Getenv(envDockerHost), os.Getenv(envDockerContext)

	psCtx, cancelPS := context.WithTimeout(cmd.Context(), container.ComposeTimeout+callSlack)
	defer cancelPS()

	rows, err := deps.ps(psCtx, host, dockerContext, req.opts)
	if err != nil {
		return err
	}

	picked, project, err := selectServices(rows, names)
	if err != nil {
		return err
	}

	params := req.params
	params.Group = project
	params.Members = make([]api.ContainerSpec, len(picked))

	for i, p := range picked {
		params.Members[i] = api.ContainerSpec{
			Engine: api.ContainerEngine{Host: host, Context: dockerContext}, Ref: p.ID, Map: req.maps,
			Service: p.Service, Project: p.Project, RequireDotnet: len(names) == 0,
		}
	}

	members, err := attachMembers(cmd, info, g, params, picked, bps)
	if err != nil {
		return err
	}

	if err := writeComposeAttach(cmd.OutOrStdout(), project, members, g.json, workDir()); err != nil {
		return err
	}

	return attachFailure(members)
}

// attachMembers runs container.attach for the members of params, then adds
// the breakpoints to every member that attached.
func attachMembers(cmd *cobra.Command, info version.Info, g *globals, params api.ContainerAttachParams, picked []container.ComposeService, bps []breakpointArg) ([]attachedMember, error) {
	p, err := daemon.DefaultPaths()
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), startBuildBudget+callSlack)
	defer cancel()

	cl, _, err := daemon.Connect(ctx, p, info, daemon.OptionsFromEnv())
	if err != nil {
		return nil, err
	}
	defer cl.Close()

	var res api.ContainerAttachResult
	if err := cl.Call(ctx, api.MethodContainerAttach, params, &res); err != nil {
		return nil, containerCallError(err)
	}

	members, err := attachedMembers(picked, res)
	if err != nil {
		return nil, err
	}

	addBreakpointsTo(ctx, cl, g, members, bps)

	return members, nil
}

// attachRequest is what the flags of 'compose attach' ask for.
type attachRequest struct {
	opts   container.ComposeOptions
	maps   []api.PathMapping
	params api.ContainerAttachParams
}

// breakpointArg is a --bp as given and parsed.
type breakpointArg struct {
	location string
	spec     api.BreakpointSpec
}

// request validates the flags and the service names.
func (f *composeAttachFlags) request(g *globals, names []string) (attachRequest, []breakpointArg, error) {
	for _, n := range names {
		if err := container.ValidateService(n); err != nil {
			return attachRequest{}, nil, err
		}
	}

	req := attachRequest{opts: container.ComposeOptions{Files: f.files, ProjectName: f.project, ProjectDirectory: f.projectDir}}
	if err := req.opts.Validate(); err != nil {
		return attachRequest{}, nil, err
	}

	var err error
	if req.maps, err = parseMaps(f.maps); err != nil {
		return attachRequest{}, nil, err
	}

	policy, err := api.ParseLeasePolicy(f.leasePolicy)
	if err != nil {
		return attachRequest{}, nil, err
	}

	req.params = api.ContainerAttachParams{
		DumpSpec: api.DumpSpec{Budget: g.budget}, Client: g.clientID(), Lang: dotnet.Language, LeasePolicy: policy,
		NoRecord: f.noRecord || os.Getenv(envNoRecord) == "1",
	}

	if f.exceptions != "" {
		if req.params.Exceptions, err = api.ParseExceptionMode(f.exceptions); err != nil {
			return attachRequest{}, nil, err
		}
	}

	bps := make([]breakpointArg, len(f.bps))

	for i, b := range f.bps {
		spec, err := parseLocation(b)
		if err != nil {
			return attachRequest{}, nil, err
		}

		bps[i] = breakpointArg{location: b, spec: spec}
	}

	return req, bps, nil
}

// selectServices picks the containers to attach to from compose ps: the
// running ones of the named services, or all running ones without names.
// Every container must belong to one project, which is the group.
func selectServices(rows []container.ComposeService, names []string) ([]container.ComposeService, string, error) {
	var running []container.ComposeService

	for _, r := range rows {
		if r.State == "running" {
			running = append(running, r)
		}
	}

	if len(running) == 0 {
		return nil, "", api.NewError(api.CodeInvalidRequest, "no running containers in this compose project",
			"start the stack ('docker compose up -d'), and run this in its directory or name it with -f FILE, -p NAME or --project-directory DIR")
	}

	sort.SliceStable(running, func(i, j int) bool {
		if running[i].Service != running[j].Service {
			return running[i].Service < running[j].Service
		}

		return running[i].Name < running[j].Name
	})

	project, err := oneProject(running)
	if err != nil {
		return nil, "", err
	}

	picked := running

	if len(names) > 0 {
		picked = slices.DeleteFunc(slices.Clone(running), func(r container.ComposeService) bool { return !slices.Contains(names, r.Service) })

		for _, n := range names {
			if !slices.ContainsFunc(running, func(r container.ComposeService) bool { return r.Service == n }) {
				return nil, "", api.NewError(api.CodeInvalidRequest, "service "+n+" is not running", "running: "+serviceList(running))
			}
		}
	}

	if len(picked) > maxComposeMembers {
		return nil, "", api.NewError(api.CodeInvalidRequest, fmt.Sprintf("%d containers: at most %d at once", len(picked), maxComposeMembers),
			"name the services to attach to")
	}

	return picked, project, nil
}

// oneProject is the project every container belongs to, or INVALID_REQUEST.
func oneProject(rows []container.ComposeService) (string, error) {
	var projects []string

	for _, r := range rows {
		if !slices.Contains(projects, r.Project) {
			projects = append(projects, r.Project)
		}
	}

	if len(projects) > 1 {
		sort.Strings(projects)

		return "", api.NewError(api.CodeInvalidRequest, "containers of several compose projects: "+strings.Join(projects, ", "), "name one with -p NAME")
	}

	return projects[0], nil
}

// serviceList is the distinct services of rows, comma-separated.
func serviceList(rows []container.ComposeService) string {
	var names []string

	for _, r := range rows {
		if !slices.Contains(names, r.Service) {
			names = append(names, r.Service)
		}
	}

	return strings.Join(names, ", ")
}

// attachedMembers pairs the daemon's results with the containers they are
// for, in order.
func attachedMembers(picked []container.ComposeService, res api.ContainerAttachResult) ([]attachedMember, error) {
	if len(res.Members) != len(picked) {
		return nil, api.NewError(api.CodeInternal, fmt.Sprintf("eyedbgd answered %d members for %d containers", len(res.Members), len(picked)), "")
	}

	members := make([]attachedMember, len(picked))

	for i, r := range res.Members {
		members[i] = attachedMember{Service: picked[i].Service, Container: picked[i].Name, Skipped: r.Skipped, Error: r.Error}
		if r.Session != nil {
			sess := r.Session.Session
			members[i].Session = &sess
		}
	}

	return members, nil
}

// addBreakpointsTo adds each breakpoint to every attached member, recording
// what each did with it.
func addBreakpointsTo(ctx context.Context, cl *daemon.Client, g *globals, members []attachedMember, bps []breakpointArg) {
	for i := range members {
		if members[i].Session == nil {
			continue
		}

		ref := api.SessionRef{SessionID: members[i].Session.ID, Client: g.clientID()}
		for _, bp := range bps {
			members[i].Breakpoints = append(members[i].Breakpoints, addBreakpoint(ctx, cl, ref, bp))
		}
	}
}

// addBreakpoint adds one breakpoint to one session. A line outside the
// session's path map is a skip, not a failure: it was never meant for this
// service.
func addBreakpoint(ctx context.Context, cl *daemon.Client, ref api.SessionRef, bp breakpointArg) bpOutcome {
	var added api.Breakpoint

	err := boundedCall(ctx, cl, api.MethodBreakpointAdd, api.BreakpointAddParams{SessionRef: ref, BreakpointSpec: bp.spec}, &added)

	switch {
	case err == nil:
		return bpOutcome{Location: bp.location, Breakpoint: &added}
	case outsideMap(err):
		return bpOutcome{Location: bp.location, Skipped: "its file is outside this service's path map"}
	default:
		return bpOutcome{Location: bp.location, Error: jsonErr(err)}
	}
}

// outsideMap reports whether err is a line breakpoint refused for being
// outside a session's path map.
func outsideMap(err error) bool {
	apiErr, ok := errors.AsType[*api.Error](err)

	return ok && apiErr.Code == api.CodeInvalidRequest && strings.HasSuffix(apiErr.Message, api.MessageOutsidePathMap)
}

// attachFailure is the error of an attach that attached nothing: the first
// failure's, else "no .NET services running" when everything was skipped. It
// has already been shown: its details are in the output.
func attachFailure(members []attachedMember) error {
	if attachedCount(members) > 0 {
		return nil
	}

	for i := range members {
		if members[i].Error != nil {
			return &reportedError{err: members[i].Error}
		}
	}

	return &reportedError{err: api.NewError(api.CodeInvalidRequest, "no .NET services running",
		"every running service was skipped (see why above); name services to try them anyway: 'eyedbg compose attach SERVICE...'")}
}

// ------------------------------------------------------------------ wait

const composeWaitLong = `Wait until any member of a group stops (a breakpoint, an exception, a pause, a step) or ends, at most --timeout, and
show where it stopped: the group, the member's session id and service, its snapshot as 'eyedbg status' prints it
(location, source around it, what --dump asks for, what the program printed), how long it has been stopped, and
the other members that are stopped too. If a member is already stopped it answers at once, the one stopped longest;
--new ignores members that are already stopped and waits for a stop after this call. When every member has already
ended, says so at once.

Act on the member that answered with the ordinary commands and its -s ID: 'eyedbg vars -s s-k3f9', 'eyedbg next
-s s-k3f9', 'eyedbg continue -s s-k3f9'. A stop stops that whole service: its callers time out, its healthcheck
turns unhealthy and a Kafka consumer in it leaves its group after max.poll.interval.ms; the output notes this once
the stop is long enough. A timeout is not an error: nothing stopped, the members keep running ("timedOut": true in
--json).` + dumpHelp + groupHelp + `

Never resumes or pauses a program. Blocks at most --timeout (default 30s). --json: {"schema", "group", "session":
the answering member's snapshot, "service", "stopped": [{"sessionId", "service", "reason", "threadId",
"stoppedAt"}] (the other stopped members), "timedOut", "ended"}. Exits 2 when the group has no session
(NO_SESSION).`

const composeWaitExample = `  eyedbg compose wait
  eyedbg compose wait --new --timeout 5m
  eyedbg compose wait -g myapp --dump locals --json`

func newComposeWaitCommand(info version.Info, g *globals, deps composeDeps) *cobra.Command {
	var (
		group   string
		fresh   bool
		timeout time.Duration
		dump    *dumpFlag
	)

	cmd := &cobra.Command{
		Use:     "wait",
		Short:   "Wait for any service of the group to stop",
		Long:    composeWaitLong,
		Example: composeWaitExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds, err := dump.spec(g)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), timeout+callSlack)
			defer cancel()

			cl, err := dialRunning(ctx, info)
			if err != nil {
				return err
			}
			defer cl.Close()

			name, err := resolveGroup(ctx, cl, group)
			if err != nil {
				return err
			}

			var res api.GroupWaitResult

			params := api.GroupWaitParams{DumpSpec: ds, Client: g.clientID(), Group: name, New: fresh, Wait: api.Duration(timeout)}
			if err := cl.Call(ctx, api.MethodGroupWait, params, &res); err != nil {
				return groupCallError(err)
			}

			return writeGroupWait(cmd.OutOrStdout(), res, groupWaitView{timeout: timeout}, g.json, workDir(), deps.now())
		},
	}

	addGroupFlag(cmd, &group, false)
	cmd.Flags().BoolVar(&fresh, "new", false, "ignore members that are already stopped: wait for a stop after this call")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultExecTimeout, "longest time to wait")
	dump = addDumpFlag(cmd)

	return cmd
}

// ---------------------------------------------------------------- events

const composeEventsLong = `Show what happened in the members of a group as one stream: each member's event log (clients joining, lease changes,
execution commands, stops, program output, breakpoints, exit and end; 'eyedbg help events' lists the kinds),
merged by time, one line per event, prefixed with the service (its session id when it has no service, and
service/session when two members of a service are in the result) and the event's sequence number in its member.

The last line, "next:", gives the --since CURSOR to read on from. A cursor is every member's last-seen sequence
number, "s-k3f9:12,s-m2n4:4": pass it back unchanged. Without --since the events come oldest first from each
member's beginning, at most --limit (default 100, 0 for the maximum, 1000); the cursor never skips a matching event
the limit cut, so reading on pages through all of them. --newest shows the newest --limit events instead; its
cursor never skips an event but may repeat some (a member with older events left out keeps its place, so reading
on from the cursor shows those and repeats the ones shown): use it to look, not to page.
--kind keeps some kinds only (comma-separated: started, client, lease, exec, continued, stopped, output, breakpoint,
thread, exited, ended).

--wait blocks until a matching event exists after --since (without --since, from each member's beginning: it
returns at once when the logs already hold a matching one), at most --timeout (default 30s): a timeout is not an
error, it prints "(no new events within ...)". Each log keeps its last 10000 events (8 MiB); a line says how many
older ones were dropped. Output is cut to --budget tokens. A group has at most 64 members here.

Read-only; never affects a program.` + groupHelp + `

--json: {"schema", "events": [{"sessionId", "service", "event": {"seq", "time", "kind", ...}}], "cursor", "more",
"dropped", "timedOut"}.`

const composeEventsExample = `  eyedbg compose events --newest --limit 20
  eyedbg compose events --since 's-k3f9:12,s-m2n4:4' --wait --kind stopped,output
  eyedbg compose events -g myapp --kind stopped --json`

// composeEventsFlags are the flags of 'eyedbg compose events'.
type composeEventsFlags struct {
	group, since string
	kinds        []string
	limit        int
	newest, wait bool
	timeout      time.Duration
}

func newComposeEventsCommand(info version.Info, g *globals) *cobra.Command {
	var f composeEventsFlags

	cmd := &cobra.Command{
		Use:     "events",
		Short:   "Show the members' event logs merged, or wait for them",
		Long:    composeEventsLong,
		Example: composeEventsExample,
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return composeEvents(cmd, info, g, &f) },
	}

	addGroupFlag(cmd, &f.group, false)
	fl := cmd.Flags()
	fl.StringVar(&f.since, "since", "", "show events after this cursor (the \"next:\" line gives it), oldest first (default: from each member's beginning)")
	fl.IntVar(&f.limit, "limit", 100, "most events to show; 0 for the maximum (1000)")
	fl.StringSliceVar(&f.kinds, "kind", nil, "only these kinds (comma-separated)")
	fl.BoolVar(&f.newest, "newest", false, "show the newest events instead of the oldest (the cursor may repeat some of them)")
	fl.BoolVar(&f.wait, "wait", false, "wait until a matching event exists, at most --timeout")
	fl.DurationVar(&f.timeout, "timeout", defaultExecTimeout, "longest time --wait waits")

	return cmd
}

// params validates the flags and builds the request (the group is the
// caller's to fill in).
func (f *composeEventsFlags) params(g *globals) (api.GroupEventsParams, error) {
	if f.limit < 0 {
		return api.GroupEventsParams{}, errors.New("--limit takes a number from 0")
	}

	p := api.GroupEventsParams{Client: g.clientID(), Since: f.since, Newest: f.newest, Limit: f.limit, Budget: g.budget}
	if p.Limit == 0 {
		p.Limit = api.MaxEventsLimit
	}

	for _, k := range f.kinds {
		if !slices.Contains(api.EventKinds(), api.EventKind(k)) {
			return api.GroupEventsParams{}, fmt.Errorf("invalid --kind %q: want %s", k, api.EventKindNames())
		}

		p.Kinds = append(p.Kinds, api.EventKind(k))
	}

	if f.wait {
		p.Wait = api.Duration(f.timeout)
	}

	return p, nil
}

// composeEvents is 'compose events'.
func composeEvents(cmd *cobra.Command, info version.Info, g *globals, f *composeEventsFlags) error {
	params, err := f.params(g)
	if err != nil {
		return err
	}

	callTimeout := daemonCallTimeout
	if f.wait {
		callTimeout = f.timeout + callSlack
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), callTimeout+daemonCallTimeout)
	defer cancel()

	cl, err := dialRunning(ctx, info)
	if err != nil {
		return err
	}
	defer cl.Close()

	if params.Group, err = resolveGroup(ctx, cl, f.group); err != nil {
		return err
	}

	var res api.GroupEventsResult
	if err := cl.Call(ctx, api.MethodGroupEvents, params, &res); err != nil {
		return groupCallError(err)
	}

	view := groupEventsView{group: params.Group, newest: f.newest, wait: f.wait, timeout: f.timeout}

	return writeGroupEvents(cmd.OutOrStdout(), res, view, g.json, workDir())
}

// -------------------------------------------------------------------- bp

const composeBpLong = `Manage breakpoints in every live member of a group at once: 'compose bp add' puts one in all of them, 'compose bp ls'
lists each member's, 'compose bp rm all' removes yours from all. They act on each member's own breakpoints ('eyedbg bp'
with its -s ID): for one member only, use 'eyedbg bp add FILE:LINE -s s-k3f9'. A line breakpoint whose file is outside
a member's path map is skipped for that member. Members that have ended are left out.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`

func newComposeBreakpointCommand(info version.Info, g *globals) *cobra.Command {
	var group string

	cmd := &cobra.Command{
		Use:   "bp",
		Short: "Add, list and remove breakpoints in every service of the group",
		Long:  composeBpLong,
		Example: `  eyedbg compose bp add Consumer/Program.cs:17
  eyedbg compose bp ls
  eyedbg compose bp rm all`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	addGroupFlag(cmd, &group, true)
	cmd.AddCommand(newComposeBpAddCommand(info, g, &group), newComposeBpListCommand(info, g, &group), newComposeBpRemoveCommand(info, g, &group))

	return cmd
}

const composeBpAddLong = `Add a breakpoint to every live member of a group. LOCATION is FILE:LINE, FILE@"TEXT" or func:NAME, as for 'eyedbg bp add'
(see its help for the forms, conditions and logpoints); FILE is relative to the current directory. The breakpoint
is the caller's in each member (--as) and replaces one of the same line with its --if, --hit and --log.

Each member answers on its own line: "bp 3 Producer/Program.cs:42 verified", "pending" (its module isn't loaded in
that service, or the line has no code there; it may bind later), "skipped: its file is outside this service's path
map" (a line breakpoint is never sent to a service whose map doesn't hold the file), or "failed: ...". Exits 0 when
at least one member took the breakpoint; otherwise the first failure's code, or INVALID_REQUEST when every member
skipped it. Doesn't resume anything and needs no lease; returns at once.` + groupHelp

func newComposeBpAddCommand(info version.Info, g *globals, group *string) *cobra.Command {
	var cond, hit, logMsg string

	cmd := &cobra.Command{
		Use:   "add <location>",
		Short: "Add a breakpoint to every service of the group",
		Long:  composeBpAddLong,
		Example: `  eyedbg compose bp add Consumer/Program.cs:17
  eyedbg compose bp add Orders.cs:88 --if 'order.Total > 100'
  eyedbg compose bp add Program.cs:20 --log 'i={i}' --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := parseLocation(args[0])
			if err != nil {
				return err
			}

			spec.Condition, spec.HitCondition, spec.LogMessage = cond, hit, logMsg

			return composeBpAdd(cmd, info, g, *group, breakpointArg{location: args[0], spec: spec})
		},
	}

	cmd.Flags().StringVar(&cond, "if", "", "stop only when this expression is true")
	cmd.Flags().StringVar(&hit, "hit", "", "stop only at some hits: N, >=N or %N")
	cmd.Flags().StringVar(&logMsg, "log", "", "print this message (with {EXPR} values) instead of stopping")

	return cmd
}

// composeBpAdd is 'compose bp add'.
func composeBpAdd(cmd *cobra.Command, info version.Info, g *globals, groupFlag string, bp breakpointArg) error {
	cl, group, members, err := openGroup(cmd, info, groupFlag)
	if err != nil {
		return err
	}
	defer cl.Close()

	live := liveMembers(members)
	if len(live) == 0 {
		return noLiveMembers(group)
	}

	out := make([]bpMember, len(live))
	outcomes := make([]bpOutcome, len(live))

	for i := range live {
		outcomes[i] = addBreakpoint(cmd.Context(), cl, api.SessionRef{SessionID: live[i].ID, Client: g.clientID()}, bp)
		out[i] = bpMember{Service: serviceOf(&live[i]), SessionID: live[i].ID, bpOutcome: outcomes[i]}
	}

	if err := writeComposeBpAdd(cmd.OutOrStdout(), group, out, g.json, workDir()); err != nil {
		return err
	}

	return bpAddFailure(outcomes)
}

// bpAddFailure is the error of an add that no member took: the first
// failure's, else INVALID_REQUEST when every member skipped it.
func bpAddFailure(outcomes []bpOutcome) error {
	if slices.ContainsFunc(outcomes, func(o bpOutcome) bool { return o.Breakpoint != nil }) {
		return nil
	}

	for _, o := range outcomes {
		if o.Error != nil {
			return &reportedError{err: o.Error}
		}
	}

	return &reportedError{err: api.NewError(api.CodeInvalidRequest, "the breakpoint is outside every service's path map",
		"a breakpoint's file must be under a host directory of the map (--map REMOTE=LOCAL on 'compose attach' sets it)")}
}

// openGroup connects to the running daemon and resolves the group and its
// members; the caller closes the client.
func openGroup(cmd *cobra.Command, info version.Info, groupFlag string) (*daemon.Client, string, []api.SessionInfo, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*daemonCallTimeout)
	defer cancel()

	cl, err := dialRunning(ctx, info)
	if err != nil {
		return nil, "", nil, err
	}

	group, members, err := groupMembers(ctx, cl, groupFlag)
	if err != nil {
		_ = cl.Close()

		return nil, "", nil, err
	}

	return cl, group, members, nil
}

// noLiveMembers is the error of a group whose members have all ended.
func noLiveMembers(group string) error {
	return api.NewError(api.CodeNoSession, "every session of group "+group+" has ended",
		"'eyedbg compose stop -g "+group+"' clears them; 'eyedbg compose attach' starts new ones")
}

// serviceOf is a member's compose service.
func serviceOf(s *api.SessionInfo) string {
	if s.Container != nil {
		return s.Container.Service
	}

	return ""
}

const composeBpListLong = `List the breakpoints of every live member of a group, each member's own, every client's (--mine: only yours): the
columns of 'eyedbg bp ls' (id, owner, where, verified or pending, hits, notes) under a line naming the member and its
session id. Breakpoint ids are per member. Read-only; returns at once. A member that can't answer says why under its
name; exits 0 when at least one answered.` + groupHelp

func newComposeBpListCommand(info version.Info, g *globals, group *string) *cobra.Command {
	var mine bool

	cmd := &cobra.Command{
		Use:     "ls",
		Short:   "List the breakpoints of every service of the group",
		Long:    composeBpListLong,
		Example: "  eyedbg compose bp ls\n  eyedbg compose bp ls --mine --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, name, members, err := openGroup(cmd, info, *group)
			if err != nil {
				return err
			}
			defer cl.Close()

			live := liveMembers(members)
			if len(live) == 0 {
				return noLiveMembers(name)
			}

			out := make([]bpListMember, len(live))

			for i := range live {
				out[i] = bpListMember{Service: serviceOf(&live[i]), SessionID: live[i].ID}

				params := api.BreakpointListParams{SessionRef: api.SessionRef{SessionID: live[i].ID, Client: g.clientID()}, Mine: mine}
				if err := boundedCall(cmd.Context(), cl, api.MethodBreakpointLs, params, &out[i].Breakpoints); err != nil {
					out[i].Error = jsonErr(err)
				}
			}

			if err := writeComposeBpList(cmd.OutOrStdout(), name, out, g.json, workDir()); err != nil {
				return err
			}

			return everyMemberFailed(len(out), func(i int) *api.Error { return out[i].Error })
		},
	}

	cmd.Flags().BoolVar(&mine, "mine", false, "list only your breakpoints (see --as)")

	return cmd
}

// everyMemberFailed is the first member's error when every one of n failed
// (shown already: it is marked reported), else nil.
func everyMemberFailed(n int, errAt func(int) *api.Error) error {
	var first *api.Error

	for i := range n {
		e := errAt(i)
		if e == nil {
			return nil
		}

		if first == nil {
			first = e
		}
	}

	if first == nil {
		return nil
	}

	return &reportedError{err: first}
}

const composeBpRemoveLong = `Remove all of your breakpoints from every live member of a group ('all' is the only form: breakpoint ids are
per member, so remove one with 'eyedbg bp rm ID -s SESSION'). Another client's breakpoint is kept and counted;
--force removes everyone's. Removing never fails for finding none: each member's line says how many were removed and
how many of other clients were kept. Doesn't resume anything and needs no lease; returns at once. Exits 0 when at
least one member answered.` + groupHelp

func newComposeBpRemoveCommand(info version.Info, g *globals, group *string) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "rm all",
		Short:   "Remove your breakpoints from every service of the group",
		Long:    composeBpRemoveLong,
		Example: "  eyedbg compose bp rm all\n  eyedbg compose bp rm all --force",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "all" {
				return fmt.Errorf("invalid argument %q: 'compose bp rm' takes 'all' (breakpoint ids are per session: 'eyedbg bp rm ID -s SESSION')", args[0])
			}

			cl, name, members, err := openGroup(cmd, info, *group)
			if err != nil {
				return err
			}
			defer cl.Close()

			live := liveMembers(members)
			if len(live) == 0 {
				return noLiveMembers(name)
			}

			out := make([]bpRemoveMember, len(live))

			for i := range live {
				out[i] = bpRemoveMember{Service: serviceOf(&live[i]), SessionID: live[i].ID}

				var res api.BreakpointRemoveResult

				params := api.BreakpointRemoveParams{SessionRef: api.SessionRef{SessionID: live[i].ID, Client: g.clientID()}, Force: force}
				if err := boundedCall(cmd.Context(), cl, api.MethodBreakpointRm, params, &res); err != nil {
					out[i].Error = jsonErr(err)

					continue
				}

				out[i].Removed, out[i].Kept = res.Removed, res.Kept
			}

			if err := writeComposeBpRemove(cmd.OutOrStdout(), name, out, g.json); err != nil {
				return err
			}

			return everyMemberFailed(len(out), func(i int) *api.Error { return out[i].Error })
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "remove other clients' breakpoints too")

	return cmd
}

// ------------------------------------------------------------------ stop

const composeStopLong = `End the session of every member of a group, as 'eyedbg stop' does for one: an attached session is detached from (the
containers' programs keep running, unstopped and unhampered), anything else is ended. Members that have already ended
are cleared from 'eyedbg sessions'. Each member is stopped on its own, with its own lease rules: while its program is
live this needs its control lease (LEASE_HELD, exit 2, if the policy keeps you from taking it), and a member that
fails doesn't keep the others from being stopped.

Output: one line per member, "detached; container NAME keeps running" (an attached service), "app terminated;
container NAME idles" (a program eyedbg launched in a container) or "ended", or why it failed. Exits 0 when every
member was stopped; otherwise the first failure's code (the other members were stopped anyway). Returns within a few
seconds per member. The netcoredbg copied into a container stays there until it is recreated.` + groupHelp

func newComposeStopCommand(info version.Info, g *globals) *cobra.Command {
	var group string

	cmd := &cobra.Command{
		Use:     composeStopUse,
		Short:   "End every session of the group (the containers keep running)",
		Long:    composeStopLong,
		Example: "  eyedbg compose stop\n  eyedbg compose stop -g myapp --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, name, members, err := openGroup(cmd, info, group)
			if err != nil {
				return err
			}
			defer cl.Close()

			out := make([]stopMember, len(members))
			errs := make([]*api.Error, len(members))

			for i := range members {
				out[i] = stopMember{Service: serviceOf(&members[i]), SessionID: members[i].ID}

				var res api.SessionInfo

				ref := api.SessionRef{SessionID: members[i].ID, Client: g.clientID()}
				if err := boundedCall(cmd.Context(), cl, api.MethodSessionStop, ref, &res); err != nil {
					errs[i] = jsonErr(err)
					out[i].Error = errs[i]

					continue
				}

				out[i].Session = &res
			}

			if err := writeComposeStop(cmd.OutOrStdout(), name, out, g.json); err != nil {
				return err
			}

			if first := firstFailure(errs...); first != nil {
				return &reportedError{err: first}
			}

			return nil
		},
	}

	addGroupFlag(cmd, &group, false)

	return cmd
}
