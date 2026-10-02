// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// The stages of 'compose launch', in the order they run (compose_launch.go).

// defaultMapRemote is where the build's PathMap and the session's path map put
// the compose directory (docs/adr/0021, D14).
const defaultMapRemote = "/src"

// classify inspects every selected container and decides what each is:
// refused, in fast mode already with this project's override (rebuild, no
// recreate), or entering fast mode (its container is recreated). An entering
// container is also checked as far as can be without changing it: the
// project's compose files, an idle command, netcoredbg for its platform.
func (r *launchRun) classify(ctx context.Context) {
	count := map[string]int{}
	for _, s := range r.svcs {
		count[s.row.Service]++
	}

	for _, s := range r.svcs {
		if count[s.row.Service] > 1 {
			// 'compose up SERVICE' recreates every replica: the ones not
			// launched in would idle for good.
			r.refuse(s, api.NewError(api.CodeInvalidRequest, "service "+s.row.Service+" has more than one container",
				"fast mode runs one container per service"))

			continue
		}

		r.classifyOne(ctx, s)
	}

	var names []string
	for _, s := range r.live() {
		names = append(names, s.row.Service)
	}

	if err := artifacts.CheckServiceNames(names); err != nil {
		r.failLive(api.NewError(api.CodeInvalidRequest, err.Error(), "launch services whose names differ by more than case"))
	}
}

func (r *launchRun) classifyOne(ctx context.Context, s *launchSvc) {
	fi, err := r.docker.InspectFast(ctx, s.row.ID)
	if err != nil {
		r.refuse(s, err)

		return
	}

	s.fi = fi

	if err := r.checkKind(s); err != nil {
		r.refuse(s, err)

		return
	}

	if !s.entry {
		return
	}

	ref := r.refOf(s)
	if err := ref.Validate(); err != nil {
		r.refuse(s, err)

		return
	}

	if err := r.docker.ProbeIdle(ctx, fi.ID); err != nil {
		r.refuse(s, err)

		return
	}

	if err := r.deps.stack.ensureAdapter(ctx, r.host, r.dockerContext, fi); err != nil {
		r.refuse(s, err)
	}
}

// checkKind decides whether the container can be launched in and how: it sets
// entry, dll and appDir, or says why not.
func (r *launchRun) checkKind(s *launchSvc) error {
	fi := &s.fi

	switch {
	case fi.Project != r.project:
		return api.NewError(api.CodeInvalidRequest, "its compose project label is "+fi.Project+", not "+r.project, "")
	case fi.Paused, fi.Restarting, !fi.Running:
		return api.NewError(api.CodeAttachFailed, "container "+fi.Name+" is not running", "start it first")
	case fi.ComposeDir == "" || !filepath.IsAbs(fi.ComposeDir):
		return api.NewError(api.CodeInvalidRequest, "container "+fi.Name+" doesn't record its compose project directory",
			"fast mode needs a container created by 'docker compose up' from compose files")
	case fi.Fast != nil && fi.Fast.Version != container.FastVersion:
		return api.NewError(api.CodeInvalidRequest, "container "+fi.Name+" is in fast mode of another eyedbg (label version "+fi.Fast.Version+")",
			"'eyedbg compose restore "+fi.Service+"' with the eyedbg that made it, or 'docker compose up -d --force-recreate "+fi.Service+"'")
	case fi.Fast != nil:
		// In fast mode: its labels say what runs. Another override path means
		// its container is recreated with this project's.
		s.dll, s.appDir, s.entry = fi.Fast.DLL, fi.Fast.WorkDir, fi.Fast.Override != r.override

		return nil
	}

	return r.checkAsBuilt(s)
}

// checkAsBuilt checks a container nobody changed: its entrypoint must be
// exactly 'dotnet X.dll', with no arguments, in a working directory the build
// can be mounted at.
func (r *launchRun) checkAsBuilt(s *launchSvc) error {
	fi := &s.fi

	switch {
	case fi.DLL == "":
		return api.NewError(api.CodeInvalidRequest, "its entrypoint isn't exec-form 'dotnet X.dll' (a shell wrapper, an apphost or an IDE's debugger worker)",
			"fast mode can't run it; after an IDE's fast mode, 'docker compose up -d "+fi.Service+"' recreates the container as built")
	case fi.HasCmd:
		return api.NewError(api.CodeInvalidRequest, "it passes arguments to its app (a command: or the image's CMD); fast mode launches 'dotnet "+fi.DLL+"' without them",
			"eyedbg never reads a container's arguments: they may hold secrets")
	case fi.WorkDirErr != nil:
		return api.NewError(api.CodeInvalidRequest, fi.WorkDirErr.Error(), "")
	case len(fi.ConfigFiles) == 0:
		return api.NewError(api.CodeInvalidRequest, "container "+fi.Name+" doesn't record its compose files",
			"fast mode recreates a container from the files it was created from")
	}

	s.entry, s.dll, s.appDir = true, fi.DLL, fi.WorkDir

	return nil
}

// refOf is the compose project of a container the way it was created: its own
// directory and files, without any override of eyedbg's, plus --env-file.
func (r *launchRun) refOf(s *launchSvc) container.ProjectRef {
	override := ""
	if s.fi.Fast != nil {
		override = s.fi.Fast.Override
	}

	files := filesWithout(filesWithout(s.fi.ConfigFiles, override), r.override)

	return container.ProjectRef{Name: r.project, WorkDir: s.fi.ComposeDir, Files: files, EnvFiles: r.req.envFiles}
}

// settle fixes the compose directory every service shares and refuses what
// disagrees with it, then checks the breakpoints against the path map: a bad
// --bp fails the whole run now, not after the first app was stopped.
func (r *launchRun) settle() error {
	for _, s := range r.live() {
		dir, err := filepath.EvalSymlinks(s.fi.ComposeDir)
		if err != nil {
			r.refuse(s, api.NewError(api.CodeInvalidRequest, "its compose project directory "+s.fi.ComposeDir+" can't be read here", err.Error()))

			continue
		}

		if r.workDir == "" {
			r.workDir = dir
		}

		if dir != r.workDir {
			r.refuse(s, api.NewError(api.CodeInvalidRequest, "its compose project directory is "+dir+", not "+r.workDir+" like the other services'", ""))
		}
	}

	if len(r.live()) == 0 {
		return nil
	}

	pm, err := session.NewPathMap([]api.PathMapping{{Remote: defaultMapRemote, Local: r.workDir}})
	if err != nil {
		return fmt.Errorf("path map for %s: %w", r.workDir, err)
	}

	r.pm = pm

	for _, b := range r.req.bps {
		if err := r.checkBreakpoint(b); err != nil {
			return err
		}
	}

	return nil
}

// checkBreakpoint refuses a --bp the sessions would refuse: a file that isn't
// there or isn't under the compose directory.
func (r *launchRun) checkBreakpoint(b breakpointArg) error {
	if b.spec.Function != "" {
		return nil
	}

	file := resolvedPath(b.spec.File)

	if info, err := os.Stat(file); err != nil || info.IsDir() {
		return api.NewError(api.CodeInvalidRequest, "--bp "+b.location+": no source file "+file,
			"FILE is a path from the current directory, e.g. src/App/Program.cs:20")
	}

	if _, ok := r.pm.ToRemote(file); !ok {
		return api.NewError(api.CodeInvalidRequest, "--bp "+b.location+": "+file+api.MessageOutsidePathMap,
			"the apps are built with /src mapped to "+r.workDir+": a breakpoint's file must be under it")
	}

	return nil
}

// resolveProjects finds each service's project: --dotnet-project, else what
// its container remembers, else the unique <assembly>.csproj under the compose
// directory. Not needed without a build.
func (r *launchRun) resolveProjects() {
	if r.req.noBuild {
		return
	}

	for _, s := range r.live() {
		abs, rel, err := r.projectOf(s)
		if err != nil {
			r.refuse(s, err)

			continue
		}

		s.project, s.projectRel = abs, rel
	}
}

func (r *launchRun) projectOf(s *launchSvc) (abs, rel string, err error) {
	if p, ok := r.req.projects[s.row.Service]; ok {
		abs, rel, err = container.ResolveProject(r.workDir, p)

		return abs, rel, projectError("--dotnet-project "+p, err)
	}

	if !s.entry && s.fi.Fast.ProjectPath != "" {
		return s.fi.Fast.ProjectPath, s.fi.Fast.Project, nil
	}

	found, err := dotnet.FindContainerProject(r.workDir, s.dll, maxProjectSearch)
	if err != nil {
		return "", "", fmt.Errorf("find the project of %s: %w", s.dll, err)
	}

	abs, rel, err = container.ResolveProject(r.workDir, found)

	return abs, rel, projectError("the project found for "+s.dll, err)
}

// projectError is the INVALID_REQUEST for a project that can't be used.
func projectError(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return api.NewError(api.CodeInvalidRequest, what+": the file doesn't exist", "name a project file under the compose directory")
	case errors.Is(err, container.ErrOutsideRoot):
		return api.NewError(api.CodeInvalidRequest, what+" isn't a project file inside the compose directory", err.Error())
	default:
		return fmt.Errorf("%s: %w", what, err)
	}
}

// build publishes each distinct project once, into the run's staging
// directory, sequentially; a failed build fails its services and changes
// nothing else. With --no-build it only checks there is a build to relaunch.
func (r *launchRun) build(ctx context.Context) {
	if r.req.noBuild {
		r.checkNoBuild()

		return
	}

	host, err := r.deps.stack.dotnetHost()
	if err != nil {
		r.failLive(err)

		return
	}

	if r.stage, err = artifacts.NewStage(r.projectDir); err != nil {
		r.failLive(fmt.Errorf("prepare a staging directory: %w", err))

		return
	}

	r.buildLog = filepath.Join(r.projectDir, "build.log")

	if r.log, err = os.OpenFile(r.buildLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
		r.failLive(fmt.Errorf("open the build log: %w", err))

		return
	}
	defer r.log.Close()

	for _, group := range r.buildGroups() {
		first := group[0]
		out := filepath.Join(r.stage, first.row.Service)
		started := time.Now()

		err := r.deps.stack.publish(ctx, dotnet.PublishSpec{
			Host: host, Project: first.project, Root: r.workDir, Out: out, Artifacts: filepath.Join(r.projectDir, "artifacts"), DLL: first.dll, Log: r.log,
		})

		elapsed := time.Since(started)

		for _, s := range group {
			if err != nil {
				r.fail(s, err)

				continue
			}

			s.out, s.built = out, elapsed
		}
	}
}

// buildGroups are the live services grouped by what they build (the same
// project and assembly), in order of first appearance: each group is built
// once.
func (r *launchRun) buildGroups() [][]*launchSvc {
	var (
		groups [][]*launchSvc
		index  = map[string]int{}
	)

	for _, s := range r.live() {
		key := s.project + "\x00" + s.dll

		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, nil)
		}

		groups[i] = append(groups[i], s)
	}

	return groups
}

// checkNoBuild refuses --no-build for a service that has no build to relaunch:
// one entering fast mode (nothing is mounted yet), or one whose service
// directory is missing or empty.
func (r *launchRun) checkNoBuild() {
	for _, s := range r.live() {
		if s.entry {
			r.refuse(s, api.NewError(api.CodeInvalidRequest, "it isn't in fast mode yet, so there is no build to relaunch", "run 'eyedbg compose launch "+s.row.Service+"' without --no-build"))

			continue
		}

		entries, err := os.ReadDir(filepath.Join(r.projectDir, "services", s.row.Service))
		if err != nil || len(entries) == 0 {
			r.refuse(s, api.NewError(api.CodeInvalidRequest, "its build directory is missing or empty", "run 'eyedbg compose launch "+s.row.Service+"' without --no-build"))
		}
	}
}

// ---------------------------------------------------------------- the change

// carryAndStop captures the breakpoints and exception mode of each service's
// live sessions (attached or launched) and ends those sessions: the first
// change of the run. A session that refuses to end (the lease is held)
// fails its service and stays.
func (r *launchRun) carryAndStop(ctx context.Context, cl *daemon.Client, list []api.SessionInfo) {
	for _, s := range r.live() {
		for _, ls := range liveSessionsOf(list, r.project, []string{s.row.Service}) {
			r.capture(ctx, cl, s, ls.id)

			if err := r.stopSession(ctx, cl, ls.id); err != nil {
				r.fail(s, err)

				break
			}

			s.downed = s.downed || ls.launched
		}
	}
}

// mirror replaces each service's files with its staged build. A service whose
// files could not be replaced is failed, and never launched: its directory may
// be half updated.
func (r *launchRun) mirror() {
	if r.req.noBuild {
		return
	}

	for _, s := range r.live() {
		dst, err := artifacts.ServiceDir(r.projectDir, s.row.Service)
		if err == nil {
			err = artifacts.Mirror(s.out, dst)
		}

		if err != nil {
			r.fail(s, api.NewError(api.CodeBuildFailed, "replace the files of "+s.row.Service+" with its build: "+err.Error(),
				"its app is stopped and its directory may be half updated: 'eyedbg compose launch "+s.row.Service+"' builds and tries again"))
		}
	}
}

// recreate puts the entering services in fast mode: it writes the override
// (their fragments, and those of the services already in fast mode) and runs
// one 'compose up' for the entering services only, per distinct list of
// compose files.
func (r *launchRun) recreate(ctx context.Context) {
	var entering []*launchSvc

	for _, s := range r.live() {
		if s.entry {
			entering = append(entering, s)
		}
	}

	if len(entering) == 0 {
		return
	}

	fast, err := r.docker.FastContainers(ctx, r.project)
	if err != nil {
		r.failEntering(entering, err)

		return
	}

	frags, err := r.overrideFragments(ctx, fast, entering)
	if err != nil {
		r.failEntering(entering, err)

		return
	}

	if err := container.WriteOverride(r.override, frags); err != nil {
		r.failEntering(entering, err)

		return
	}

	r.up(ctx, entering)
	r.refresh(ctx, entering)
}

// failEntering fails the entering services with err: they were stopped and
// not recreated.
func (r *launchRun) failEntering(entering []*launchSvc, err error) {
	for _, s := range entering {
		r.fail(s, err)
	}
}

// overrideFragments are the override's fragments: the entering services', and
// every other service already in fast mode, rebuilt from its labels.
func (r *launchRun) overrideFragments(ctx context.Context, fast []container.FastContainer, entering []*launchSvc) ([]container.FastService, error) {
	var frags []container.FastService

	for _, s := range entering {
		dir := r.serviceDir(s.row.Service)
		if dir == "" {
			return nil, fmt.Errorf("the build directory of %s is missing", s.row.Service)
		}

		frags = append(frags, container.FastService{Name: s.row.Service, DLL: s.dll, WorkDir: s.appDir, Project: s.projectRel, Source: dir})
	}

	staying := r.stayingFragments(ctx, fast, func(service string) bool {
		return slices.ContainsFunc(entering, func(s *launchSvc) bool { return s.row.Service == service })
	})

	return append(frags, staying...), nil
}

// up runs 'compose up' for the entering services, once per distinct list of
// compose files; a failure fails that list's services and stops eyedbg from
// pruning anything.
func (r *launchRun) up(ctx context.Context, entering []*launchSvc) {
	groups := map[string][]*launchSvc{}

	var order []string

	for _, s := range entering {
		ref := r.refOf(s)
		key := groupKey(ref.WorkDir, ref.Files)

		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}

		groups[key] = append(groups[key], s)
	}

	for _, key := range order {
		group := groups[key]
		names := make([]string, len(group))

		for i, s := range group {
			names[i] = s.row.Service
		}

		if err := r.docker.ComposeUp(ctx, r.refOf(group[0]), r.override, names, false); err != nil {
			r.upFailed = true

			r.failEntering(group, upFailure(err, names))

			continue
		}

		for _, s := range group {
			s.recreated = true
		}
	}
}

// refresh reads the recreated containers again (their ids changed): each
// service must have exactly one, running, in fast mode with this project's
// override.
func (r *launchRun) refresh(ctx context.Context, entering []*launchSvc) {
	fast, err := r.docker.FastContainers(ctx, r.project)

	for _, s := range entering {
		if !s.live() || !s.recreated {
			continue
		}

		if err != nil {
			r.fail(s, err)

			continue
		}

		r.refreshOne(ctx, s, fast)
	}
}

// refreshOne checks the container a recreate made for s.
func (r *launchRun) refreshOne(ctx context.Context, s *launchSvc, fast []container.FastContainer) {
	var ids []string

	for _, c := range fast {
		if c.Service == s.row.Service {
			ids = append(ids, c.ID)
		}
	}

	if len(ids) != 1 {
		r.fail(s, api.NewError(api.CodeAttachFailed, fmt.Sprintf("%d containers of %s are in fast mode after the recreate, want one", len(ids), s.row.Service),
			"see 'docker compose ps'"))

		return
	}

	fi, err := r.docker.InspectFast(ctx, ids[0])

	switch {
	case err != nil:
		r.fail(s, err)
	case !fi.Running || fi.Fast == nil || fi.Fast.Version != container.FastVersion || fi.Fast.Override != r.override:
		r.fail(s, api.NewError(api.CodeAttachFailed, "container "+fi.Name+" isn't running in fast mode after the recreate",
			"see 'docker compose ps' and 'docker logs "+fi.Name+"'"))
	default:
		s.fi = fi
	}
}

// members is the result of every service, in order, as the output shows it.
func (r *launchRun) members() []launchedMember {
	out := make([]launchedMember, len(r.svcs))

	for i, s := range r.svcs {
		m := s.res
		m.Service, m.Container = s.row.Service, s.row.Name

		if m.Session != nil {
			m.Fast = &launchFast{
				Project: s.projectRel, Override: r.override, Recreated: s.recreated, DurationMs: s.built.Milliseconds(),
				Carried: len(s.carry.bps) - s.carry.dropped, Dropped: s.carry.dropped,
			}
			if s.out != "" {
				m.Fast.BuildLog = r.buildLog
			}

			if s.projectRel == "" && s.fi.Fast != nil {
				m.Fast.Project = s.fi.Fast.Project
			}
		}

		out[i] = m
	}

	return out
}

// view is what the output says besides the members.
func (r *launchRun) view() launchView {
	v := launchView{Override: r.override, BuildLog: r.buildLog}

	for _, s := range r.svcs {
		switch {
		case s.res.Error != nil && (s.downed || s.recreated):
			v.Down = append(v.Down, s.row.Service)
		case s.res.Session != nil:
			v.Launched = append(v.Launched, s.row.Service)
		}

		if s.recreated && s.res.Session != nil {
			v.Recreated = true
		}
	}

	if r.stage == "" {
		v.BuildLog = ""
	}

	return v
}
