// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Defaults of the stray guard (docs/adr/0021, D10): a stopped app may still be
// exiting, so a stray is looked for again, 20 checks 250 ms apart (about 5 s),
// before it refuses the launch. Counted in checks, not read off a clock, so a
// slow docker top doesn't make it give up early.
const (
	defaultStrayChecks = 20
	defaultStrayEvery  = 250 * time.Millisecond
)

var _ session.ContainerLauncher = (*Driver)(nil)

// PrepareContainerLaunch implements session.ContainerLauncher: the app of a
// fast-mode container is launched under netcoredbg, which runs in the
// container through `docker exec -i` (docs/adr/0021, D3). What runs comes from
// eyedbg's own labels on the container, never from the request: `dotnet
// <workdir>/<dll>`. The exec carries no -e, -u or -w and the launch request
// no env, so the app gets the container's own environment, user and working
// directory; eyedbg never reads or passes any of them. Everything that can be
// refused is refused before the adapter is copied.
func (d *Driver) PrepareContainerLaunch(ctx context.Context, spec api.ContainerLaunchSpec) (session.Launch, error) {
	t, err := d.launchTargetOf(ctx, spec)
	if err != nil {
		return session.Launch{}, err
	}

	pm, err := launchPathMap(spec, t.fi)
	if err != nil {
		return session.Launch{}, err
	}

	warning, err := d.strayGuard(ctx, t.engine, t.fi)
	if err != nil {
		return session.Launch{}, err
	}

	// Probe first: a relaunch skips the copy (the directory's name holds the
	// adapter's version).
	if t.engine.ProbeAdapter(ctx, t.fi.ID, t.adapter) != nil {
		if err := t.engine.InstallAdapter(ctx, t.fi.ID, t.adapter); err != nil {
			return session.Launch{}, err
		}
	}

	fi, engine := t.fi, t.engine

	launch := session.Launch{
		Adapter:     engine.Docker,
		AdapterArgs: engine.ExecArgs(fi.ID, t.adapter.ContainerPath(), t.m.Adapter.Args...),
		Request:     session.RequestLaunch,
		Arguments:   containerLaunchArguments(fi.Fast.WorkDir, fi.Fast.DLL, spec.StopOnEntry),
		Program:     containerAppProgram(fi),
		PathMap:     pm,
		Container: &api.ContainerInfo{
			ID: fi.ID, Name: fi.Name, Service: fi.Service, Project: fi.Project, Platform: t.platform,
			Map: pm.Mappings(), UnhealthyAfter: api.Duration(t.unhealthyAfter),
		},
		StartFailureHint: func(ctx context.Context) string { return strayHint(ctx, engine, fi.ID, fi.Name) },
	}

	if warning != "" {
		launch.Warnings = []string{warning}
	}

	d.applySettings(&launch, t.m, "linux")

	return launch, nil
}

// EnsureContainerAdapter makes netcoredbg runnable in the container fi
// describes, as the launch will need it, before anything is changed
// (docs/adr/0021, D19): `compose launch` calls it on a container it is about to
// recreate, so a musl image, a foreign architecture, a read-only root file
// system or a missing `--platform` install fail first. The adapter that is
// already there is probed and left alone; otherwise it is copied and probed
// (the same copy any attach makes). sharpdbg can't run in a container.
func (d *Driver) EnsureContainerAdapter(ctx context.Context, engine container.Engine, fi container.FastInfo) error {
	m, err := d.containerManifest()
	if err != nil {
		return err
	}

	arch, err := imageArch(ctx, engine, container.Info{Name: fi.Name, Image: fi.Image})
	if err != nil {
		return err
	}

	adapter, err := d.containerAdapter(m, "linux/"+arch)
	if err != nil {
		return err
	}

	if engine.ProbeAdapter(ctx, fi.ID, adapter) == nil {
		return nil
	}

	return engine.InstallAdapter(ctx, fi.ID, adapter)
}

// launchTarget is what is known of a fast-mode container before the stray
// guard runs and anything is copied into it.
type launchTarget struct {
	m              *adapters.Manifest
	engine         container.Engine
	fi             container.FastInfo
	adapter        container.Adapter
	platform       string // linux/<arch>
	unhealthyAfter time.Duration
}

// launchTargetOf reads the container (inspect, the image's platform, the
// healthcheck's timing) and refuses everything that can be refused without
// looking at its processes or copying.
func (d *Driver) launchTargetOf(ctx context.Context, spec api.ContainerLaunchSpec) (launchTarget, error) {
	m, err := d.containerManifest()
	if err != nil {
		return launchTarget{}, err
	}

	engine, err := d.env.engine(spec.Engine)
	if err != nil {
		return launchTarget{}, err
	}

	fi, err := engine.InspectFast(ctx, spec.Ref)
	if err != nil {
		return launchTarget{}, err
	}

	if err := checkLaunchTarget(fi); err != nil {
		return launchTarget{}, err
	}

	arch, err := imageArch(ctx, engine, container.Info{Name: fi.Name, Image: fi.Image})
	if err != nil {
		return launchTarget{}, err
	}

	adapter, err := d.containerAdapter(m, "linux/"+arch)
	if err != nil {
		return launchTarget{}, err
	}

	// The healthcheck's timing: how long a stop may last before the container
	// says unhealthy.
	info, err := engine.Inspect(ctx, fi.ID)
	if err != nil {
		return launchTarget{}, err
	}

	return launchTarget{m: m, engine: engine, fi: fi, adapter: adapter, platform: "linux/" + arch, unhealthyAfter: info.UnhealthyAfter()}, nil
}

// checkLaunchTarget refuses a container that is not in fast mode, is not
// running, or whose launch labels are not what this eyedbg understands.
func checkLaunchTarget(fi container.FastInfo) error {
	switch {
	case fi.Fast == nil || fi.Fast.Version != container.FastVersion:
		service := fi.Service
		if service == "" {
			service = "SERVICE"
		}

		return api.NewError(api.CodeInvalidRequest, "container "+fi.Name+" isn't in eyedbg fast mode",
			"put it there with 'eyedbg compose launch "+service+"'")
	case fi.Paused:
		return api.NewError(api.CodeAttachFailed, "container "+fi.Name+" is paused", "unpause it: docker unpause "+fi.Name)
	case fi.Restarting:
		return api.NewError(api.CodeAttachFailed, "container "+fi.Name+" is restarting", "wait until it runs")
	case !fi.Running:
		return api.NewError(api.CodeAttachFailed, "container "+fi.Name+" is not running", "start it first: docker start "+fi.Name)
	case fi.Fast.DLL == "" || fi.Fast.WorkDir == "":
		// InspectFast refuses a version-1 container without them; this is the
		// last line before they name a program.
		return api.NewError(api.CodeInvalidRequest, "container "+fi.Name+" has no launch labels",
			"re-enter fast mode with 'eyedbg compose launch'")
	}

	return nil
}

// containerLaunchArguments are the DAP launch request's body (docs/adr/0021,
// D3): the app is `dotnet <workdir>/<dll>` (netcoredbg runs a .dll through the
// container's own dotnet) in workdir, no arguments, and no "env" key: the app
// inherits the container's environment.
func containerLaunchArguments(workDir, dll string, stopOnEntry bool) map[string]any {
	args := dapArguments("launch")
	args["program"] = path.Join(workDir, dll)
	args["args"] = []string{}
	args["cwd"] = workDir
	args["stopAtEntry"] = stopOnEntry

	return args
}

// launchPathMap is the session's path map: the one asked for, else the default,
// /src for the compose project's directory (an empty map when there is none,
// which refuses line breakpoints with a hint to pass --map).
func launchPathMap(spec api.ContainerLaunchSpec, fi container.FastInfo) (*session.PathMap, error) {
	return containerPathMap(&api.ContainerSpec{Map: spec.Map}, container.Info{WorkingDir: fi.ComposeDir})
}

// containerAppProgram describes the launched app for status: never its
// environment or arguments.
func containerAppProgram(fi container.FastInfo) string {
	app := "dotnet " + path.Join(fi.Fast.WorkDir, fi.Fast.DLL)

	switch {
	case fi.Service == "":
		return "container " + fi.Name + ": " + app
	case fi.Project == "":
		return fi.Service + " (container " + fi.Name + "): " + app
	default:
		return fi.Service + " (compose " + fi.Project + ", container " + fi.Name + "): " + app
	}
}

// strayGuard refuses a launch into a container that already runs a .NET
// process (docs/adr/0021, D10): an earlier app, for instance one whose
// netcoredbg never saw EOF when its client was lost. eyedbg never kills a
// process it didn't launch, so the way out is a hint. A stopped app may still
// be exiting: a stray is looked for again for a few seconds. A docker top that
// fails lets the launch proceed, with a warning for the log.
func (d *Driver) strayGuard(ctx context.Context, engine container.Engine, fi container.FastInfo) (warning string, err error) {
	checks, every := d.env.strayChecks, d.env.strayEvery
	if checks <= 0 {
		checks = defaultStrayChecks
	}

	if every <= 0 {
		every = defaultStrayEvery
	}

	for n := 1; ; n++ {
		procs, err := engine.Top(ctx, fi.ID)
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("look for an earlier app in container %s: %w", fi.Name, ctx.Err())
			}

			return "docker top failed, so an earlier app in the container was not ruled out: " + err.Error(), nil
		}

		if len(container.Strays(procs)) == 0 {
			return "", nil
		}

		if n >= checks {
			return "", strayError(fi.Name)
		}

		timer := time.NewTimer(every)

		select {
		case <-ctx.Done():
			timer.Stop()

			return "", fmt.Errorf("look for an earlier app in container %s: %w", fi.Name, ctx.Err())
		case <-timer.C:
		}
	}
}

// strayError is the refusal of a launch into a container that runs a .NET
// process.
func strayError(name string) error {
	return api.NewError(api.CodeAttachFailed, "container "+name+" already runs a .NET process: an earlier app, whose debugger was probably lost",
		"'docker restart "+name+"' ends it and keeps fast mode (it restarts only this container); eyedbg never kills a process it didn't launch")
}

// strayHint is what to add to the error of a failed launch when a .NET process
// runs in the container: the host process that was killed reaches only the
// docker client, not what it started in the container.
func strayHint(ctx context.Context, engine container.Engine, id, name string) string {
	procs, err := engine.Top(ctx, id)
	if err != nil || len(container.Strays(procs)) == 0 {
		return ""
	}

	return "a .NET process may still run in container " + name + ": 'docker restart " + name + "' ends it and keeps fast mode"
}
