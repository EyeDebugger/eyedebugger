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
	WritableSources(ctx context.Context, project string) ([]container.WritableSource, error)
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
	// keepProject keeps the project directory although no service is in fast
	// mode: a build failed, and its log is in it.
	keepProject bool

	// recorded is what eyedbg's override file for the project records (read
	// once, again after a write); recordedErr why it could not be read.
	recorded    *container.Recorded
	recordedErr error
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

// corroborateProject checks the compose project directory a container's
// labels name before anything is built in it, recreated from it or mapped to
// it, and sets fi.ComposeDir to it with its symlinks resolved. An image's own
// LABELs are copied onto every container made from it, so a container started
// by plain 'docker run' can carry any compose labels: the directory must hold
// a compose file the container's files label names (docs/adr/0021, D14), and,
// when the command line says where the project is (--project-directory, else
// the directory of the first -f file; compose's own rule), it must be that
// directory. Without either flag compose found the project from the current
// directory, which eyedbg doesn't second-guess.
func corroborateProject(fi *container.FastInfo, opts container.ComposeOptions) error {
	dir, err := container.CorroborateComposeDir(fi.ComposeDir, fi.ConfigFiles)
	if err != nil {
		return api.NewError(api.CodeInvalidRequest, "container "+fi.Name+": "+err.Error(),
			"fast mode needs a compose file inside the project directory: keep one there, or run compose without --project-directory. "+
				"A container made by plain 'docker run' from an image that sets compose labels is refused the same way: an image's own labels can say anything")
	}

	if want := flagProjectDir(opts); want != "" && !sameDir(want, dir) {
		return api.NewError(api.CodeInvalidRequest, "container "+fi.Name+"'s compose project directory is "+dir+", not the "+want+" the command line names",
			"name the project as it was created ('docker compose ps' with the same -f and --project-directory finds the container), or recreate the container with 'docker compose up -d --force-recreate'")
	}

	fi.ComposeDir = dir

	return nil
}

// flagProjectDir is the project directory the compose flags name: --project-
// directory, else the directory of the first -f file (stdin, "-", names none);
// "" when they name none.
func flagProjectDir(opts container.ComposeOptions) string {
	switch {
	case opts.ProjectDirectory != "":
		return opts.ProjectDirectory
	case len(opts.Files) > 0 && opts.Files[0] != "-":
		return filepath.Dir(opts.Files[0])
	default:
		return ""
	}
}

// sameDir reports whether a and b are the same directory (the same file, so a
// symlink or a letter case on Windows doesn't matter).
func sameDir(a, b string) bool {
	ia, err := os.Stat(a)
	if err != nil {
		return false
	}

	ib, err := os.Stat(b)
	if err != nil {
		return false
	}

	return os.SameFile(ia, ib)
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

	// Compose records a container's files as one comma-separated label: a path
	// with a comma can't be told apart from two (and fails closed when read).
	if strings.ContainsRune(r.root, ',') {
		return api.NewError(api.CodeInvalidRequest, "eyedbg's home directory has a comma in its path: "+r.root,
			"compose records container files comma-separated; set EYEDBG_HOME to a path without one")
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

	artifacts.PruneServices(r.projectDir, keep)

	if len(keep) == 0 && !r.keepProject {
		return artifacts.RemoveProjectDir(r.root, r.project) == nil
	}

	return false
}

// holdsMounted reports whether eyedbg's directory for the project holds
// anything a container can mount: the override file or a build under
// services/. A directory with neither (a build log, the lock) has nothing a
// container on any engine depends on; what can't be read counts as held.
func (r *stackRun) holdsMounted() bool {
	if _, err := os.Lstat(r.override); !errors.Is(err, fs.ErrNotExist) {
		return true
	}

	entries, err := os.ReadDir(filepath.Join(r.projectDir, "services"))
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}

	return err != nil || len(entries) > 0
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

// recordedFast reports whether the fast-mode labels fl of service's container
// are the ones eyedbg's own override file for this project records. Any
// container label is untrusted input unless eyedbg-owned state corroborates
// it: an image's LABELs copy onto every container made from it, so labels
// alone may not say what a service runs or which project file builds it. A
// file that can't be read records nothing.
func (r *stackRun) recordedFast(service string, fl *container.FastLabels) bool {
	if r.recorded == nil {
		rec, err := container.ReadRecorded(r.override)
		r.recorded, r.recordedErr = &rec, err
	}

	return r.recorded.Corroborates(service, fl)
}

// writeOverride writes the project's override file; what the next
// [stackRun.recordedFast] reads is the new one.
func (r *stackRun) writeOverride(frags []container.FastService) error {
	r.recorded = nil

	return container.WriteOverride(r.override, frags)
}

// fragmentOf is the override's fragment for a service already in fast mode,
// rebuilt from its container's labels, as eyedbg's own override records them
// (the labels alone are untrusted). Its project is the one recorded, even when
// that file has moved since (the next 'eyedbg compose launch SERVICE' searches
// again). It has none when its labels aren't recorded or its build directory
// is gone (the override can't mount it): the container then keeps labels no
// override records, and launch refuses it until 'eyedbg compose restore
// SERVICE' puts it back as built.
func (r *stackRun) fragmentOf(ctx context.Context, c container.FastContainer) (container.FastService, bool) {
	fi, err := r.docker.InspectFast(ctx, c.ID)
	if err != nil || fi.Fast == nil || fi.Fast.Version != container.FastVersion || fi.Fast.ProjectLabel == "" || !r.recordedFast(c.Service, fi.Fast) {
		return container.FastService{}, false
	}

	dir := r.serviceDir(c.Service)
	if dir == "" {
		return container.FastService{}, false
	}

	return container.FastService{Name: c.Service, DLL: fi.Fast.DLL, WorkDir: fi.Fast.WorkDir, Project: fi.Fast.ProjectLabel, Source: dir}, true
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
