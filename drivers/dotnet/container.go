// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// containerAttachHint lists why attaching to a .NET process in a container
// fails (docs/adr/0020).
const containerAttachHint = "pid 1 must be the .NET runtime (init: true or a shell wrapper moves it: pass --pid with the " +
	".NET process's pid inside the container); the program must not run with DOTNET_EnableDiagnostics=0, " +
	"nor already be under a debugger (a stray netcoredbg from a lost session: 'docker restart' the container)"

// defaultMapRemote is where Visual Studio's container templates put the
// sources ('WORKDIR /src', 'COPY . .'): the compose project directory maps
// there unless --map says otherwise.
const defaultMapRemote = "/src"

// dotnetExecutable is the pid 1 executable of a .NET service built from the
// Visual Studio template (ENTRYPOINT ["dotnet", ...]).
const dotnetExecutable = "dotnet"

var _ session.ContainerAttacher = (*Driver)(nil)

// PrepareContainerAttach implements session.ContainerAttacher: netcoredbg for
// the container's own platform is copied into the container (docker cp) and
// the adapter is `docker exec -i` running it there. Everything that can be
// refused is refused before the copy, and the container is read only through
// internal/container's fixed inspect template.
func (d *Driver) PrepareContainerAttach(ctx context.Context, spec api.AttachSpec) (session.Launch, error) {
	cs := spec.Container
	if cs == nil {
		return session.Launch{}, api.NewError(api.CodeInternal, "container attach without a container", "")
	}

	t, err := d.containerTargetOf(ctx, cs)
	if err != nil {
		return session.Launch{}, err
	}

	adapter, err := d.containerAdapter(t.m, t.platform)
	if err != nil {
		return session.Launch{}, err
	}

	if err := t.engine.InstallAdapter(ctx, t.info.ID, adapter); err != nil {
		return session.Launch{}, err
	}

	pm, err := containerPathMap(cs, t.info)
	if err != nil {
		return session.Launch{}, err
	}

	launch := session.Launch{
		Adapter:     t.engine.Docker,
		AdapterArgs: t.engine.ExecArgs(t.info.ID, adapter.ContainerPath(), t.m.Adapter.Args...),
		Request:     session.RequestAttach,
		PID:         t.pid,
		Arguments:   attachArguments(t.pid),
		AttachHint:  containerAttachHint,
		Program:     containerProgram(t.info, t.pid),
		PathMap:     pm,
		Container: &api.ContainerInfo{
			ID: t.info.ID, Name: t.info.Name, Service: t.info.Service, Project: t.info.Project, PID: t.pid,
			Platform: t.platform, Map: pm.Mappings(), UnhealthyAfter: api.Duration(t.info.UnhealthyAfter()),
		},
	}
	d.applySettings(&launch, t.m, "linux")

	return launch, nil
}

// containerTarget is what is known of a container to attach to before
// anything is copied into it.
type containerTarget struct {
	m        *adapters.Manifest
	engine   container.Engine
	info     container.Info
	pid      int    // the process to attach to, as the container numbers it
	platform string // linux/<arch>
}

// containerTargetOf reads the container (two docker calls) and refuses
// everything that can be refused without copying.
func (d *Driver) containerTargetOf(ctx context.Context, cs *api.ContainerSpec) (containerTarget, error) {
	m, err := d.containerManifest()
	if err != nil {
		return containerTarget{}, err
	}

	engine, err := d.env.engine(cs.Engine)
	if err != nil {
		return containerTarget{}, err
	}

	info, err := engine.Inspect(ctx, cs.Ref)
	if err != nil {
		return containerTarget{}, err
	}

	pid, err := checkContainerTarget(info, cs)
	if err != nil {
		return containerTarget{}, err
	}

	arch, err := imageArch(ctx, engine, info)
	if err != nil {
		return containerTarget{}, err
	}

	return containerTarget{m: m, engine: engine, info: info, pid: pid, platform: "linux/" + arch}, nil
}

// containerManifest is the manifest of the adapter a container gets:
// netcoredbg's, never SharpDbg's.
func (d *Driver) containerManifest() (*adapters.Manifest, error) {
	if d.pick == SharpDbg {
		return nil, api.NewError(api.CodeInvalidRequest, "sharpdbg can't debug inside a container: only netcoredbg is copied there",
			"drop --adapter sharpdbg")
	}

	m := d.manifest(netcoredbgName)
	if m == nil || m.Adapter.Runtime != adapters.RuntimeNative {
		return nil, api.NewError(api.CodeAdapterMissing, "no netcoredbg manifest: it is the adapter that is copied into containers",
			"see 'eyedbg adapters ls': a user manifest may have replaced it or failed to load")
	}

	return m, nil
}

// checkContainerTarget checks what can be known from one inspect, before
// anything is copied: that the container can be attached to and which
// process to attach to (pid, as the container numbers it).
func checkContainerTarget(info container.Info, cs *api.ContainerSpec) (int, error) {
	switch {
	case info.FastMode:
		err := api.NewError(api.CodeInvalidRequest, "container "+info.Name+" runs in eyedbg fast mode: its app runs only under 'eyedbg compose launch'",
			"debug it with 'eyedbg compose launch', or undo fast mode with 'eyedbg compose restore'")

		return 0, skipIf(cs, err)
	case info.Paused:
		return 0, api.NewError(api.CodeAttachFailed, "container "+info.Name+" is paused", "unpause it: docker unpause "+info.Name)
	case info.Restarting:
		return 0, api.NewError(api.CodeAttachFailed, "container "+info.Name+" is restarting", "wait until it runs")
	case !info.Running:
		return 0, api.NewError(api.CodeAttachFailed, "container "+info.Name+" is not running", "start it first")
	case cs.RequireDotnet && info.ProcessName() != dotnetExecutable:
		err := api.NewError(api.CodeNotDotnet, "pid 1 runs "+info.ProcessName(),
			"name the service to attach to it anyway, or pass --pid with the .NET process's pid")

		return 0, skipIf(cs, err)
	}

	if info.Init && cs.PID == 0 {
		return 0, api.NewError(api.CodeInvalidRequest,
			"container "+info.Name+" runs with init: true: pid 1 is docker-init, not the .NET process, and attaching to it does nothing",
			"pass --pid N with the .NET process's pid inside the container ('docker top' shows host pids; look in /proc/*/comm or use pidof)")
	}

	return max(cs.PID, 1), nil
}

// skipIf marks err as "not a candidate" when the member was picked
// implicitly.
func skipIf(cs *api.ContainerSpec, err *api.Error) error {
	if cs.RequireDotnet {
		return errors.Join(err, session.ErrNotCandidate)
	}

	return err
}

// imageArch is the architecture of the container's image, which must be a
// Linux one netcoredbg is built for.
func imageArch(ctx context.Context, engine container.Engine, info container.Info) (string, error) {
	goos, arch, variant, err := engine.ImagePlatform(ctx, info.Image)
	if err != nil {
		return "", err
	}

	platform := goos + "/" + arch
	if variant != "" {
		platform += "/" + variant
	}

	switch {
	case goos != "linux":
		return "", api.NewError(api.CodeAttachFailed, "container "+info.Name+" is a "+platform+" container: eyedbg debugs Linux containers",
			"Windows containers are not supported")
	case arch != "amd64" && arch != "arm64":
		return "", api.NewError(api.CodeAttachFailed, "container "+info.Name+" runs a "+platform+" image: netcoredbg is built for linux/amd64 and linux/arm64 only", "")
	}

	return arch, nil
}

// containerAdapter is netcoredbg's installed directory for platform, as
// internal/container puts it in a container.
func (d *Driver) containerAdapter(m *adapters.Manifest, platform string) (container.Adapter, error) {
	dir, _, err := d.env.installedFor(m, platform)
	if errors.Is(err, adapters.ErrNotInstalled) {
		return container.Adapter{}, api.NewError(api.CodeAdapterMissing, m.Name+" for "+platform+" is not installed",
			"run 'eyedbg adapters install "+m.Name+" --platform "+platform+"'")
	}

	if err != nil {
		return container.Adapter{}, fmt.Errorf("find %s for %s: %w", m.Name, platform, err)
	}

	probe := m.Adapter.VersionArgs
	if len(probe) == 0 {
		probe = []string{"--version"}
	}

	return container.Adapter{Name: m.Name, Version: m.Version, Dir: dir, Entry: m.Adapter.Entry, ProbeArgs: probe}, nil
}

// containerPathMap is the session's path map: the one asked for, else the
// default, /src for the compose project's directory (an empty map when there
// is none, which refuses line breakpoints with a hint to pass --map).
func containerPathMap(cs *api.ContainerSpec, info container.Info) (*session.PathMap, error) {
	if len(cs.Map) > 0 {
		return session.NewPathMap(cs.Map)
	}

	if dir := info.WorkingDir; dir != "" && filepath.IsAbs(dir) && filepath.Dir(dir) != dir {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			if pm, err := session.NewPathMap([]api.PathMapping{{Remote: defaultMapRemote, Local: dir}}); err == nil {
				return pm, nil
			}
		}
	}

	return session.NewPathMap(nil)
}

// containerProgram describes the debuggee for status: never its command line.
func containerProgram(info container.Info, pid int) string {
	where := "container " + info.Name + ", pid " + strconv.Itoa(pid)

	if info.Service == "" {
		return "container " + info.Name + " (pid " + strconv.Itoa(pid) + ")"
	}

	if info.Project == "" {
		return info.Service + " (" + where + ")"
	}

	return info.Service + " (compose " + info.Project + ", " + where + ")"
}
