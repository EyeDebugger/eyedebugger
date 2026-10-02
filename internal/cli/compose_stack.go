// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Shared by 'compose launch' and 'compose restore': both change the user's
// running stack, under one lock per compose project, and keep eyedbg's files
// for it (docs/adr/0021, D15 to D18).

// stackDocker is what 'compose launch' and 'compose restore' ask of docker;
// [container.Engine] is the real one, tests replace it.
type stackDocker interface {
	InspectFast(ctx context.Context, ref string) (container.FastInfo, error)
	ProbeIdle(ctx context.Context, ref string) error
	FastContainers(ctx context.Context, project string) ([]container.FastContainer, error)
	ComposeUp(ctx context.Context, ref container.ProjectRef, override string, services []string, wait bool) error
}

// stackDeps are the parts of 'compose launch' and 'compose restore' outside the
// process; tests replace them.
type stackDeps struct {
	// docker returns the docker CLI for an engine (DOCKER_HOST, DOCKER_CONTEXT).
	docker func(host, dockerContext string) (stackDocker, error)
	// ensureAdapter makes netcoredbg runnable in an as-built container before
	// it is recreated.
	ensureAdapter func(ctx context.Context, host, dockerContext string, fi container.FastInfo) error
	// publish builds a project for a container (dotnet publish).
	publish func(ctx context.Context, spec dotnet.PublishSpec) error
	// dotnetHost finds the dotnet executable that builds.
	dotnetHost func() (string, error)
	// root is eyedbg's compose directory, <home>/compose.
	root func() (string, error)
}

// defaultStackDeps are the real docker, dotnet and home directory.
func defaultStackDeps() stackDeps {
	return stackDeps{
		docker: func(host, dockerContext string) (stackDocker, error) {
			e, err := container.NewEngine(host, dockerContext)
			if err != nil {
				return nil, fmt.Errorf("find docker: %w", err)
			}

			return e, nil
		},
		ensureAdapter: func(ctx context.Context, host, dockerContext string, fi container.FastInfo) error {
			e, err := container.NewEngine(host, dockerContext)
			if err != nil {
				return fmt.Errorf("find docker: %w", err)
			}

			return dotnet.New().EnsureContainerAdapter(ctx, e, fi)
		},
		publish:    dotnet.PublishForContainer,
		dotnetHost: dotnet.FindHost,
		root:       func() (string, error) { return artifacts.Dir(artifacts.Compose) },
	}
}

// maxProjectSearch is how many entries the search for a service's project
// reads (docs/adr/0021, D12).
const maxProjectSearch = 50000

// stackRun is what one 'compose launch' or 'compose restore' run holds: the
// stack's project, its eyedbg directory under the project's lock, and
// whether the run has to leave eyedbg's files alone.
type stackRun struct {
	cmd  *cobra.Command
	info version.Info
	g    *globals
	deps composeDeps

	docker              stackDocker
	host, dockerContext string
	project             string

	root, projectDir, override string
	unlock                     func()
	// upFailed is set when a recreate failed: the state of that service is
	// uncertain, so nothing of eyedbg's is pruned (docs/adr/0021, D18).
	upFailed bool
}

// newStackRun opens the run: the docker of the caller's engine.
func newStackRun(cmd *cobra.Command, info version.Info, g *globals, deps composeDeps, project string) (*stackRun, error) {
	r := &stackRun{
		cmd: cmd, info: info, g: g, deps: deps, project: project,
		host: os.Getenv(envDockerHost), dockerContext: os.Getenv(envDockerContext),
	}

	var err error
	if r.docker, err = deps.stack.docker(r.host, r.dockerContext); err != nil {
		return nil, err
	}

	return r, nil
}

// engineSpec is the engine members of a daemon call name.
func (r *stackRun) engineSpec() api.ContainerEngine {
	return api.ContainerEngine{Host: r.host, Context: r.dockerContext}
}

// takeLock takes the project's lock (one run at a time per project) and
// clears the staging directories dead runs left; the caller defers [release].
func (r *stackRun) takeLock() error {
	var err error
	if r.root, err = r.deps.stack.root(); err != nil {
		return fmt.Errorf("find eyedbg's compose directory: %w", err)
	}

	if r.projectDir, err = artifacts.ProjectDir(r.root, r.project); err != nil {
		return fmt.Errorf("prepare eyedbg's directory for project %s: %w", r.project, err)
	}

	r.override = filepath.Join(r.projectDir, container.OverrideName)

	r.unlock, err = artifacts.Lock(r.projectDir)
	if errors.Is(err, artifacts.ErrLocked) {
		return api.NewError(api.CodeInvalidRequest, "another eyedbg compose run is working on project "+r.project, err.Error())
	}

	if err != nil {
		return fmt.Errorf("lock project %s: %w", r.project, err)
	}

	artifacts.PruneStages(r.projectDir, "")

	return nil
}

// release gives the lock back.
func (r *stackRun) release() {
	if r.unlock != nil {
		r.unlock()
	}
}

// prune removes what no container of the project refers to any more: the
// service directories of services not in fast mode, and the whole project
// directory (override, builds, log) when no service is; it reports whether it
// removed the project directory. It acts only on what docker says about the
// project's labels, and not at all when the list can't be read or a recreate
// failed (docs/adr/0021, D17).
func (r *stackRun) prune(ctx context.Context) bool {
	if r.upFailed || r.projectDir == "" {
		return false
	}

	fast, err := r.docker.FastContainers(ctx, r.project)
	if err != nil {
		return false
	}

	keep := map[string]bool{}
	for _, c := range fast {
		keep[c.Service] = true
	}

	if len(keep) == 0 {
		return artifacts.RemoveProjectDir(r.root, r.project) == nil
	}

	artifacts.PruneServices(r.projectDir, keep)

	return false
}

// serviceDir is the directory of a service's build, which must exist as a
// real directory for the override to mount it ("" when it doesn't).
func (r *stackRun) serviceDir(service string) string {
	dir := filepath.Join(r.projectDir, "services", service)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return ""
	}

	return dir
}

// fragmentOf is the override's fragment for a service already in fast mode,
// rebuilt from its container's labels; it has none when its labels, its build
// directory or its project file can't be found ('eyedbg compose launch
// SERVICE' makes them again).
func (r *stackRun) fragmentOf(ctx context.Context, c container.FastContainer) (container.FastService, bool) {
	fi, err := r.docker.InspectFast(ctx, c.ID)
	if err != nil || fi.Fast == nil || fi.Fast.Version != container.FastVersion || fi.Fast.Project == "" {
		return container.FastService{}, false
	}

	dir := r.serviceDir(c.Service)
	if dir == "" {
		return container.FastService{}, false
	}

	return container.FastService{Name: c.Service, DLL: fi.Fast.DLL, WorkDir: fi.Fast.WorkDir, Project: fi.Fast.Project, Source: dir}, true
}

// stayingFragments are the fragments of the project's fast-mode services that
// the run doesn't change (every one not in skip), rebuilt from their
// containers' labels; one whose labels or directory can't give a fragment
// has none.
func (r *stackRun) stayingFragments(ctx context.Context, fast []container.FastContainer, skip func(service string) bool) []container.FastService {
	var frags []container.FastService

	seen := map[string]bool{}

	for _, c := range fast {
		if skip(c.Service) || seen[c.Service] {
			continue
		}

		seen[c.Service] = true

		if frag, ok := r.fragmentOf(ctx, c); ok {
			frags = append(frags, frag)
		}
	}

	return frags
}

// dial connects to the daemon, starting it if needed.
func (r *stackRun) dial(ctx context.Context) (*daemon.Client, error) {
	p, err := daemon.DefaultPaths()
	if err != nil {
		return nil, fmt.Errorf("locate the daemon: %w", err)
	}

	cl, _, err := daemon.Connect(ctx, p, r.info, daemon.OptionsFromEnv())
	if err != nil {
		return nil, fmt.Errorf("connect to eyedbgd: %w", err)
	}

	return cl, nil
}

// liveSession is a session a run is about to end.
type liveSession struct {
	id string
	// launched: its app runs only under it, so ending it stops the app (an
	// attached session's program keeps running).
	launched bool
}

// liveSessionsOf are the live sessions (a program that can still be debugged)
// of the project's services in names, as the daemon lists them: attached,
// launched, in whatever group.
func liveSessionsOf(list []api.SessionInfo, project string, names []string) []liveSession {
	var out []liveSession

	for i := range list {
		s := &list[i]
		if s.Container == nil || s.Container.Project != project || !slices.Contains(names, s.Container.Service) {
			continue
		}

		if s.State == api.StateExited || s.State == api.StateLost {
			continue
		}

		out = append(out, liveSession{id: s.ID, launched: s.Container.Launched})
	}

	return out
}

// stopSession ends one session as 'eyedbg stop' does: an attached one is
// detached from, a launched one's app is terminated. A session that is
// already gone is fine; any other refusal (the lease is held) is returned.
func (r *stackRun) stopSession(ctx context.Context, cl *daemon.Client, id string) error {
	var res api.SessionInfo

	ref := api.SessionRef{SessionID: id, Client: r.g.clientID()}

	err := boundedCall(ctx, cl, api.MethodSessionStop, ref, &res)
	if err != nil && api.CodeOf(err) == api.CodeNoSession {
		return nil
	}

	return err
}

// refuseText is err as one line for a skipped service.
func refuseText(err error) string {
	e := toErrorDoc(err)

	s := strings.Join(strings.Fields(e.Message), " ")
	if e.Hint != "" {
		s += " (" + strings.Join(strings.Fields(e.Hint), " ") + ")"
	}

	return s
}

// groupKey names a list of compose files and the directory they are used
// from: services with the same key are recreated by one 'compose up'.
func groupKey(dir string, files []string) string {
	return dir + "\x00" + strings.Join(files, "\x00")
}

// filesWithout is a container's compose files without the override it was
// created with: the files compose actually used, which a CLI's own -f can't
// reproduce (docs/adr/0021, D16). The match is exact; no file is read.
func filesWithout(files []string, override string) []string {
	if override == "" {
		return slices.Clone(files)
	}

	return slices.DeleteFunc(slices.Clone(files), func(f string) bool { return f == override })
}

// upFailure is the error of a failed 'compose up': compose's own words, and
// the way back.
func upFailure(err error, services []string) error {
	e, ok := errors.AsType[*api.Error](err)
	if !ok {
		return err
	}

	failed := *e
	failed.Hint = "see 'docker compose ps'; 'eyedbg compose restore " + strings.Join(services, " ") + "' goes back to the image's own entrypoint"

	return &failed
}
