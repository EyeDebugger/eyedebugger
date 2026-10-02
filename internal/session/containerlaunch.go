// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// failureHintTimeout bounds [Launch.StartFailureHint].
const failureHintTimeout = 3 * time.Second

// startContainerLaunch starts a session that launches the app of
// p.ContainerLaunch's container inside it (docs/adr/0021): a launched session
// (mode "": stop kills the app, detach is refused) of a container, with the
// whole container claimed, so one app runs per container. Nothing is copied
// before the checks that need no container: the driver, the launch options and
// the path map; a container another session debugs is refused before the
// driver inspects it, and again, by its full id, before the adapter starts.
func (m *Manager) startContainerLaunch(ctx context.Context, c api.Client, drv Driver, p api.StartParams, policy api.LeasePolicy) (*Session, error) {
	cl, ok := drv.(ContainerLauncher)
	if !ok {
		return nil, api.NewError(api.CodeInvalidRequest, p.Lang+" can't launch inside a container", "")
	}

	if err := containerLaunchOnly(p.LaunchSpec); err != nil {
		return nil, err
	}

	spec := *p.ContainerLaunch

	if _, err := NewPathMap(spec.Map); err != nil {
		return nil, err
	}

	if holder := m.liveClaimOn(spec.Ref); holder != nil {
		return nil, api.NewError(api.CodeInvalidRequest, "container "+spec.Ref+" is already debugged by session "+holder.ID,
			"'eyedbg stop -s "+holder.ID+"' ends that session")
	}

	launch, err := cl.PrepareContainerLaunch(ctx, spec)
	if err != nil {
		return nil, err
	}

	launch, err = checkContainerLaunch(p.Lang, launch)
	if err != nil {
		return nil, err
	}

	for _, w := range launch.Warnings {
		m.logger.WarnContext(ctx, "container launch", slog.String("container", launch.Container.Name), slog.String("warning", w))
	}

	// Mode "": launched. The container has no host pid; the claim covers the
	// container, the one app it may run.
	s := m.create(ctx, c, p, policy, launch, "")

	if err := m.claimLaunch(s, launch.Container.ID); err != nil {
		s.closeRecording()

		return nil, err
	}

	started, err := m.run(ctx, s, launch, p.Breakpoints, nil)
	if err != nil {
		m.release(s)

		return nil, withFailureHint(ctx, err, launch.StartFailureHint)
	}

	return started, nil
}

// checkContainerLaunch checks what a driver's launch of a container's app
// must be, and returns it with the container marked as launched.
func checkContainerLaunch(lang string, launch Launch) (Launch, error) {
	switch {
	case launch.Container == nil || launch.Container.ID == "":
		return launch, api.NewError(api.CodeInternal, lang+"'s driver returned no container to launch in", "")
	case launch.Request != "" && launch.Request != RequestLaunch:
		return launch, api.NewError(api.CodeInternal, lang+"'s driver returned an attach for a container launch", "")
	}

	info := *launch.Container
	info.Launched = true
	launch.Container = &info
	launch.Request, launch.PID = RequestLaunch, 0

	return launch, nil
}

// containerLaunchOnly refuses host launch options for a launch in a
// container: the driver reads what runs from the container.
func containerLaunchOnly(spec api.LaunchSpec) error {
	err := refuseOptions(spec, nil, "launching in a container takes no host launch options: ",
		"what runs, its working directory and its environment are the container's own")
	if err != nil {
		return err
	}

	if len(spec.Options) > 0 {
		return api.NewError(api.CodeInvalidRequest, "launching in a container takes no --opt options", "")
	}

	return nil
}

// withFailureHint is err with what hint (nil: nothing) says added to its
// hint, after a failed start; hint runs under its own short deadline, even
// when the start failed because ctx ended.
func withFailureHint(ctx context.Context, err error, hint func(context.Context) string) error {
	if hint == nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failureHintTimeout)
	defer cancel()

	extra := hint(ctx)
	if extra == "" {
		return err
	}

	if e, ok := errors.AsType[*api.Error](err); ok {
		cp := *e
		if cp.Hint != "" {
			cp.Hint += "; "
		}

		cp.Hint += extra

		return &cp
	}

	return fmt.Errorf("%w (%s)", err, extra)
}
