// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"sync"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Limits of one container.attach request.
const (
	// maxContainerMembers is how many containers one request may name.
	maxContainerMembers = 64
	// maxConcurrentAttaches is how many members start at once: each copies
	// an adapter into its container.
	maxConcurrentAttaches = 4
)

func containerHandlers(m *session.Manager) map[string]handler {
	return map[string]handler{
		api.MethodContainerAttach: withParams(func(ctx context.Context, p api.ContainerAttachParams) (any, error) {
			c, err := api.ParseClient(p.Client)
			if err != nil {
				return nil, err
			}

			return attachContainers(ctx, m, c, p)
		}),
	}
}

// attachContainers starts one session per member and answers per member, in
// the order of the request. A member that fails, or is skipped, doesn't stop
// the others; a request that is wrong as a whole starts nothing.
func attachContainers(ctx context.Context, m *session.Manager, c api.Client, p api.ContainerAttachParams) (api.ContainerAttachResult, error) {
	if err := checkContainerAttach(p); err != nil {
		return api.ContainerAttachResult{}, err
	}

	var (
		results = make([]api.ContainerMemberResult, len(p.Members))
		slots   = make(chan struct{}, maxConcurrentAttaches)
		wg      sync.WaitGroup
	)

	// wait is honored for a single member only, as attach does.
	wait := api.Duration(0)
	if len(p.Members) == 1 {
		wait = p.Wait
	}

	for i := range p.Members {
		wg.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				results[i] = api.ContainerMemberResult{
					Service: p.Members[i].Service, Container: p.Members[i].Ref,
					Error: toAPIError(ctx.Err()),
				}

				return
			}

			results[i] = attachMember(ctx, m, c, p, p.Members[i], wait)
		})
	}

	wg.Wait()

	return api.ContainerAttachResult{Members: results}, nil
}

// checkContainerAttach checks the whole request before anything starts.
func checkContainerAttach(p api.ContainerAttachParams) error {
	if len(p.Members) == 0 || len(p.Members) > maxContainerMembers {
		return api.NewError(api.CodeInvalidRequest, "container.attach takes between 1 and 64 members", "")
	}

	if p.Group != "" {
		if err := api.CheckGroup(p.Group); err != nil {
			return err
		}
	}

	for i := range p.Members {
		if err := container.ValidateSpec(&p.Members[i]); err != nil {
			return err
		}
	}

	return nil
}

// attachMember starts the session of one member.
func attachMember(
	ctx context.Context, m *session.Manager, c api.Client, p api.ContainerAttachParams, member api.ContainerSpec, wait api.Duration,
) api.ContainerMemberResult {
	res := api.ContainerMemberResult{Service: member.Service, Container: member.Ref}

	sess, err := m.Start(ctx, c, api.StartParams{
		DumpSpec: p.DumpSpec, Lang: p.Lang, Breakpoints: p.Breakpoints, Client: p.Client, LeasePolicy: p.LeasePolicy,
		NoRecord: p.NoRecord, Exceptions: p.Exceptions, Adapter: p.Adapter, Group: p.Group,
		Attach: &api.AttachSpec{Container: &member},
	})

	switch {
	case err == nil:
		snap := sess.Snapshot(ctx, p.DumpSpec)
		if wait > 0 && len(p.Breakpoints) > 0 {
			snap = sess.Wait(ctx, 0, clampWait(wait), p.DumpSpec)
		}

		res.Session = &snap
	case member.RequireDotnet && errors.Is(err, session.ErrNotCandidate):
		res.Skipped = toAPIError(err).Message
	default:
		res.Error = toAPIError(err)
	}

	return res
}
