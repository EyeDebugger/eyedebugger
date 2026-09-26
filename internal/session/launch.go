// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"io"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// LaunchHooks let the caller of [Manager.Launch] take part in the start.
type LaunchHooks struct {
	// Output, when set, receives the build's output as it is produced, from
	// a driver that streams it ([OptionsPreparer]); nothing else. It is
	// not written to once the build is done, and never to the session's
	// event log.
	Output io.Writer
	// Configure, when set, is called once the adapter is initialized and
	// the starter's breakpoints and exception filters are sent, before
	// configurationDone: the session is then listed and starting, and no
	// lock of it is held, so Configure may set breakpoints and exception
	// modes as any client would. An error from it (or ctx ending while it
	// runs) fails the start: the session is ended and forgotten, and
	// Launch returns that error.
	Configure func(ctx context.Context, s *Session) error
}

// Launch starts a session that launches a program for client c, who holds
// its lease, as [Manager.Start] does (the same checks, time limit and
// order), with h taking part: the build's output streams to h.Output, and
// h.Configure runs during the configuration. p may neither attach nor run
// tests.
func (m *Manager) Launch(ctx context.Context, c api.Client, p api.StartParams, h LaunchHooks) (*Session, error) {
	if p.Attach != nil || p.Test != nil {
		return nil, api.NewError(api.CodeInvalidRequest, "a launch neither attaches nor runs tests", "")
	}

	drv, policy, p, err := m.checkedStart(p)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	var launch Launch

	if op, ok := drv.(OptionsPreparer); ok {
		launch, err = op.PrepareWith(ctx, p.LaunchSpec, PrepareOptions{Output: h.Output})
	} else {
		launch, err = drv.Prepare(ctx, p.LaunchSpec)
	}

	if err != nil {
		return nil, err
	}

	return m.run(ctx, m.create(ctx, c, p, policy, launch, ""), launch, p.Breakpoints, h.Configure)
}
