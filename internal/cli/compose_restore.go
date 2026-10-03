// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

const composeRestoreLong = `Put services that 'eyedbg compose launch' changed back as built: their containers are recreated from the compose
files they were created from, without eyedbg's override, so each runs its image's own entrypoint and Release build again,
with its own healthcheck, ports and restart policy, and 'docker compose logs' shows its output again.

For each service in fast mode (the named ones, else all of the project's): its eyedbg sessions are ended (the app is
terminated; an attached session is detached), then 'docker compose up -d --no-deps --force-recreate --no-build --wait
--wait-timeout 180' recreates the named services only, with this shell's environment and --env-file files: anything the
containers wrote to their own file systems outside volumes is lost, as when they entered fast mode. Dependents are never
touched. Then eyedbg's files for the services it restored (the builds, and the override once no service is in fast mode)
are removed, and nothing is launched; when the engine shows the project neither through a fast-mode container nor
through 'docker compose ps' (which -p NAME doesn't ask; the stack may run on another DOCKER_CONTEXT or host, whose
containers still mount those files) nothing is removed and the output names the directory. Like launch, restore recreates only from a compose directory its container's labels
corroborate (a compose file of its files label lies in it, and it is the directory --project-directory or -f names). A service that isn't in fast mode is skipped ("not in fast mode"). Containers in
any state are found, so a stopped fast-mode container is restored too; it is started as part of the recreate.

The project is the one 'docker compose ps' shows for -f FILE (repeatable), --project-directory DIR and the current
directory, or -p NAME (then docker compose isn't asked, so a stack that is down can be restored). Blocks until the
services are healthy or 180 seconds passed; a service that doesn't come up healthy fails with compose's words, and its
container is left as compose left it ('docker compose ps' shows it). One run per project at a time.

Output: one line per service (restored, skipped, or failed), the sessions ended, and what was removed. Exit 0 when at
least one service was restored; otherwise the first failure's code (ATTACH_FAILED, LEASE_HELD, INVALID_REQUEST, ...),
or INVALID_REQUEST "nothing is in fast mode" when every service was skipped. Starts no daemon.` + groupHelp

const composeRestoreExample = `  eyedbg compose restore                 # every service in fast mode, in the stack in this directory
  eyedbg compose restore producer
  eyedbg compose restore -p myapp --env-file prod.env`

// composeRestoreFlags are the flags of 'eyedbg compose restore'.
type composeRestoreFlags struct {
	files      []string
	project    string
	projectDir string
	envFiles   []string
}

// addComposeProjectFlags adds -f, -p and --project-directory, as for docker
// compose.
func addComposeProjectFlags(fl *pflag.FlagSet, files *[]string, project, projectDir *string) {
	fl.StringArrayVarP(files, "file", "f", nil, "compose file (repeatable), as for docker compose -f")
	fl.StringVarP(project, "project-name", "p", "", "compose project name, as for docker compose -p")
	fl.StringVar(projectDir, "project-directory", "", "compose project directory, as for docker compose --project-directory")
}

func newComposeRestoreCommand(info version.Info, g *globals, deps composeDeps) *cobra.Command {
	var f composeRestoreFlags

	cmd := &cobra.Command{
		Use:     "restore [SERVICE...]",
		Short:   "Put fast-mode services back as built",
		Long:    composeRestoreLong,
		Example: composeRestoreExample,
		RunE: func(cmd *cobra.Command, args []string) error {
			return composeRestore(cmd, info, g, deps, &f, args)
		},
	}

	fl := cmd.Flags()
	addComposeProjectFlags(fl, &f.files, &f.project, &f.projectDir)
	fl.StringArrayVar(&f.envFiles, "env-file", nil, "env file compose interpolates when the containers are recreated, as for docker compose --env-file (repeatable)")

	return cmd
}

// restoreSvc is one service of a restore run.
type restoreSvc struct {
	service string
	id      string
	fi      container.FastInfo
	res     restoredMember
}

// live reports whether the service is still in the run.
func (s *restoreSvc) live() bool { return s.res.Skipped == "" && s.res.Error == nil }

// restoreRun is one 'compose restore'.
type restoreRun struct {
	*stackRun

	names    []string
	envFiles []string
	opts     container.ComposeOptions
	svcs     []*restoreSvc
	// foundFast: the engine listed at least one fast-mode container of the
	// project. removed: eyedbg's files for the project are gone.
	foundFast, removed bool
	// psSeen: docker compose ps showed containers of the project on this
	// engine (the project was not named with -p).
	psSeen bool
}

// composeRestore is 'compose restore'.
func composeRestore(cmd *cobra.Command, info version.Info, g *globals, deps composeDeps, f *composeRestoreFlags, names []string) error {
	for _, n := range names {
		if err := container.ValidateService(n); err != nil {
			return err
		}
	}

	opts := container.ComposeOptions{Files: f.files, ProjectName: f.project, ProjectDirectory: f.projectDir}
	if err := opts.Validate(); err != nil {
		return err
	}

	if err := container.ValidateEnvFiles(f.envFiles); err != nil {
		return err
	}

	project, seen, err := restoreProject(cmd.Context(), deps, opts)
	if err != nil {
		return err
	}

	base, err := newStackRun(cmd, info, g, deps, project)
	if err != nil {
		return err
	}

	run := &restoreRun{stackRun: base, names: names, envFiles: f.envFiles, opts: opts, psSeen: seen}

	if err := run.takeLock(); err != nil {
		return err
	}
	defer run.release()

	runErr := run.execute(cmd.Context())

	// Only a run that saw the project on its engine tidies eyedbg's files for
	// it: through its fast-mode containers, or through docker compose ps (a
	// failed first launch leaves a build log and no container in fast mode).
	// One that saw neither (restore -p asks no ps; the stack may be on another
	// engine, whose containers still mount them) leaves them (docs/adr/0021,
	// D17).
	if run.foundFast || run.psSeen {
		run.removed = run.prune(cmd.Context())
	}

	if runErr != nil {
		return runErr
	}

	members := run.members()

	outcome := restoreOutcome{Removed: run.removed}
	if !run.foundFast && !run.psSeen {
		outcome.Kept, outcome.Engine = run.projectDir, engineWords(run.host, run.dockerContext)
	}

	if err := writeComposeRestore(cmd.OutOrStdout(), project, members, outcome, g.json); err != nil {
		return err
	}

	return restoreFailure(members)
}

// restoreProject is the compose project to restore: -p's, else the one docker
// compose ps shows (seen: this engine's ps showed it).
func restoreProject(ctx context.Context, deps composeDeps, opts container.ComposeOptions) (project string, seen bool, err error) {
	if opts.ProjectName != "" {
		return opts.ProjectName, false, nil
	}

	psCtx, cancel := context.WithTimeout(ctx, container.ComposeTimeout+callSlack)
	defer cancel()

	rows, err := deps.ps(psCtx, os.Getenv(envDockerHost), os.Getenv(envDockerContext), opts)
	if err != nil {
		return "", false, err
	}

	if len(rows) == 0 {
		return "", false, api.NewError(api.CodeInvalidRequest, "no containers in this compose project",
			"name the project with -p NAME (a stack that is down shows no containers to docker compose ps)")
	}

	project, err = oneProject(rows)

	return project, err == nil, err
}

// engineWords names the docker engine a command talks to, for a message: its
// DOCKER_HOST and DOCKER_CONTEXT, else docker's own default.
func engineWords(host, dockerContext string) string {
	var parts []string

	if host != "" {
		parts = append(parts, "DOCKER_HOST "+host)
	}

	if dockerContext != "" {
		parts = append(parts, "docker context "+dockerContext)
	}

	if len(parts) == 0 {
		return "this shell's default docker engine"
	}

	return strings.Join(parts, ", ")
}

// restoreFailure is the error of a restore that restored nothing: the first
// failure's, else INVALID_REQUEST when everything was skipped. It has been
// shown already.
func restoreFailure(members []restoredMember) error {
	for i := range members {
		if members[i].Restored {
			return nil
		}
	}

	for i := range members {
		if members[i].Error != nil {
			return &reportedError{err: members[i].Error}
		}
	}

	return &reportedError{err: api.NewError(api.CodeInvalidRequest, "nothing is in fast mode",
		"'eyedbg compose launch' puts services there; 'docker ps -a --filter label=dev.izzat.eyedbg.fast.override' lists the containers that are")}
}

// execute restores the services, under the project's lock: end their
// sessions, recreate them as built, then tidy eyedbg's files.
func (r *restoreRun) execute(ctx context.Context) error {
	fast, err := r.docker.FastContainers(ctx, r.project)
	if err != nil {
		return err
	}

	r.foundFast = len(fast) > 0

	r.pick(fast)
	r.inspect(ctx)

	if err := r.stopSessions(ctx); err != nil {
		return err
	}

	r.up(ctx)

	if !r.upFailed && slices.ContainsFunc(r.svcs, func(s *restoreSvc) bool { return s.res.Restored }) {
		r.rewriteOverride(ctx)
	}

	return nil
}

// pick lists the services to restore: the named ones, else every service in
// fast mode; a named one that isn't in fast mode is skipped.
func (r *restoreRun) pick(fast []container.FastContainer) {
	byService := map[string]string{}

	for _, c := range fast {
		if _, ok := byService[c.Service]; !ok {
			byService[c.Service] = c.ID
		}
	}

	services := r.names
	if len(services) == 0 {
		services = sortedMapKeys(byService)
	}

	for _, name := range slices.Compact(slices.Sorted(slices.Values(services))) {
		s := &restoreSvc{service: name, id: byService[name]}
		s.res.Service = name

		if s.id == "" {
			s.res.Skipped = "not in fast mode"
		}

		r.svcs = append(r.svcs, s)
	}
}

// sortedMapKeys is the keys of m, sorted.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// live is the services still in the run.
func (r *restoreRun) live() []*restoreSvc {
	var out []*restoreSvc

	for _, s := range r.svcs {
		if s.live() {
			out = append(out, s)
		}
	}

	return out
}

// inspect reads each container: its compose directory and the files it was
// created from, which the recreate needs.
func (r *restoreRun) inspect(ctx context.Context) {
	for _, s := range r.live() {
		fi, err := r.docker.InspectFast(ctx, s.id)

		switch {
		case err != nil:
			s.res.Error = jsonErr(err)
		case fi.ComposeDir == "" || len(fi.ConfigFiles) == 0 || fi.Project != r.project:
			s.res.Error = jsonErr(api.NewError(api.CodeInvalidRequest, "container "+fi.Name+" doesn't record the compose files it was created from",
				"'docker compose up -d --force-recreate "+s.service+"' recreates it from the files you name"))
		default:
			// The recreate runs in the directory its labels name: only when
			// compose's files are really there.
			if err := corroborateProject(&fi, r.opts); err != nil {
				s.res.Error = jsonErr(err)

				continue
			}

			s.fi, s.res.Container = fi, fi.Name
		}
	}
}

// stopSessions ends the sessions of the services to restore (the app of a
// launched one is terminated). No daemon means no sessions; a session that
// refuses to end (the lease is held) fails its service and stays.
func (r *restoreRun) stopSessions(ctx context.Context) error {
	if len(r.live()) == 0 {
		return nil
	}

	dialCtx, cancel := context.WithTimeout(ctx, 2*daemonCallTimeout)
	defer cancel()

	cl, err := dialRunning(dialCtx, r.info)
	if errors.Is(err, errNoDaemon) {
		return nil
	}

	if err != nil {
		return err
	}
	defer cl.Close()

	list, err := sessionList(dialCtx, cl)
	if err != nil {
		return err
	}

	for _, s := range r.live() {
		for _, ls := range liveSessionsOf(list, r.project, []string{s.service}) {
			if err := r.stopSession(ctx, cl, ls.id); err != nil {
				s.res.Error = jsonErr(err)

				break
			}

			s.res.Sessions = append(s.res.Sessions, ls.id)
		}
	}

	return nil
}

// up recreates the services as built, once per distinct list of compose files:
// the container's own, without eyedbg's override. It waits until they are
// healthy.
func (r *restoreRun) up(ctx context.Context) {
	groups := map[string][]*restoreSvc{}

	var order []string

	for _, s := range r.live() {
		key := groupKey(s.fi.ComposeDir, r.filesOf(s))
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}

		groups[key] = append(groups[key], s)
	}

	for _, key := range order {
		group := groups[key]
		names := make([]string, len(group))

		for i, s := range group {
			names[i] = s.service
		}

		ref := container.ProjectRef{Name: r.project, WorkDir: group[0].fi.ComposeDir, Files: r.filesOf(group[0]), EnvFiles: r.envFiles}

		err := ref.Validate()
		if err == nil {
			err = r.docker.ComposeUp(ctx, ref, "", names, true)
		}

		for _, s := range group {
			if err != nil {
				s.res.Error = jsonErr(err)
			} else {
				s.res.Restored = true
			}
		}

		if err != nil {
			r.upFailed = true
		}
	}
}

// filesOf are the compose files to recreate s from: its container's own,
// without eyedbg's override.
func (r *restoreRun) filesOf(s *restoreSvc) []string {
	override := r.override
	if s.fi.Fast != nil {
		override = s.fi.Fast.Override
	}

	return filesWithout(filesWithout(s.fi.ConfigFiles, override), r.override)
}

// rewriteOverride drops the restored services' fragments from the override
// when other services are still in fast mode; with none left, the prune
// removes the whole project directory, override included.
func (r *restoreRun) rewriteOverride(ctx context.Context) {
	fast, err := r.docker.FastContainers(ctx, r.project)
	if err != nil || len(fast) == 0 {
		return
	}

	frags := r.stayingFragments(ctx, fast, func(string) bool { return false })
	if len(frags) == 0 {
		return
	}

	_ = container.WriteOverride(r.override, frags)
}

// members is the result of every service, in order.
func (r *restoreRun) members() []restoredMember {
	out := make([]restoredMember, len(r.svcs))

	for i, s := range r.svcs {
		out[i] = s.res
	}

	return out
}
