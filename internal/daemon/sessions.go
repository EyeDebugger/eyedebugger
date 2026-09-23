// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Limits on how long one request may wait for the program.
const (
	defaultWait = 30 * time.Second
	maxWait     = 10 * time.Minute
)

type handler func(ctx context.Context, raw json.RawMessage) (any, error)

// sessionHandlers returns the handler of every session method.
func sessionHandlers(m *session.Manager) map[string]handler {
	all := make(map[string]handler)

	for _, group := range []map[string]handler{
		lifecycleHandlers(m), execHandlers(m), breakpointHandlers(m), inspectHandlers(m), leaseHandlers(m),
	} {
		maps.Copy(all, group)
	}

	return all
}

func lifecycleHandlers(m *session.Manager) map[string]handler {
	return map[string]handler{
		api.MethodSessionStart: withParams(func(ctx context.Context, p api.StartParams) (any, error) {
			c, err := api.ParseClient(p.Client)
			if err != nil {
				return nil, err
			}

			return startSession(ctx, m, c, p)
		}),
		api.MethodSessionList: func(context.Context, json.RawMessage) (any, error) { return m.List(), nil },
		api.MethodSessionStop: withParams(func(ctx context.Context, p api.SessionRef) (any, error) {
			c, err := api.ParseClient(p.Client)
			if err != nil {
				return nil, err
			}

			return m.Stop(ctx, c, p.SessionID)
		}),
		api.MethodSessionStatus: onSession(m, func(ctx context.Context, sess *session.Session, _ api.Client, p api.StatusParams) (any, error) {
			return sess.Snapshot(ctx, p.DumpSpec), nil
		}),
	}
}

func execHandlers(m *session.Manager) map[string]handler {
	return map[string]handler{
		api.MethodExec: onSession(m, func(ctx context.Context, sess *session.Session, c api.Client, p api.ExecParams) (any, error) {
			return sess.Resume(ctx, c, p.Kind, p.ThreadID, clampWait(p.Wait), p.DumpSpec)
		}),
		api.MethodRunUntil: onSession(m, func(ctx context.Context, sess *session.Session, c api.Client, p api.RunUntilParams) (any, error) {
			return sess.RunUntil(ctx, c, p.BreakpointSpec, p.ThreadID, clampWait(p.Wait), p.DumpSpec)
		}),
		api.MethodWait: onSession(m, func(ctx context.Context, sess *session.Session, _ api.Client, p api.WaitParams) (any, error) {
			after := p.AfterStops
			if after < 0 {
				after = sess.Stops()
			}

			return sess.Wait(ctx, after, clampWait(p.Wait), p.DumpSpec), nil
		}),
	}
}

func breakpointHandlers(m *session.Manager) map[string]handler {
	return map[string]handler{
		api.MethodBreakpointAdd: onSession(m, func(ctx context.Context, sess *session.Session, c api.Client, p api.BreakpointAddParams) (any, error) {
			return sess.AddBreakpoint(ctx, c, p.BreakpointSpec)
		}),
		api.MethodBreakpointLs: onSession(m, func(_ context.Context, sess *session.Session, c api.Client, p api.BreakpointListParams) (any, error) {
			owner := ""
			if p.Mine {
				owner = c.ID
			}

			return nonNil(sess.Breakpoints(owner)), nil
		}),
		api.MethodBreakpointRm: onSession(m, func(ctx context.Context, sess *session.Session, c api.Client, p api.BreakpointRemoveParams) (any, error) {
			removed, kept, err := sess.RemoveBreakpoint(ctx, c, p.ID, p.Force)

			return api.BreakpointRemoveResult{Removed: removed, Kept: kept}, err
		}),
	}
}

func inspectHandlers(m *session.Manager) map[string]handler {
	return map[string]handler{
		api.MethodStack: onSession(m, func(ctx context.Context, sess *session.Session, _ api.Client, p api.StackParams) (any, error) {
			return sess.Stack(ctx, p.ThreadID, p.Levels)
		}),
		api.MethodVars: onSession(m, func(ctx context.Context, sess *session.Session, _ api.Client, p api.VarsParams) (any, error) {
			switch {
			case p.Changed && p.Frame != 0:
				return nil, api.NewError(api.CodeInvalidRequest, "--changed works on frame 0 only", "drop --frame")
			case p.Changed:
				return sess.Changes(ctx, p.Budget)
			case p.Expand != "":
				return sess.Expand(ctx, p.Frame, p.Expand, max(p.Depth, 1), p.Budget)
			default:
				return sess.Vars(ctx, p.Frame, max(p.Depth, 1), p.Budget)
			}
		}),
		api.MethodEval: onSession(m, func(ctx context.Context, sess *session.Session, _ api.Client, p api.EvalParams) (any, error) {
			return sess.Eval(ctx, p.Expression, p.Frame)
		}),
		api.MethodOutput: onSession(m, func(_ context.Context, sess *session.Session, _ api.Client, p api.OutputParams) (any, error) {
			return sess.Output(p.Since, p.Tail), nil
		}),
		api.MethodEvents: onSession(m, func(ctx context.Context, sess *session.Session, _ api.Client, p api.EventsParams) (any, error) {
			var wait time.Duration
			if p.Wait > 0 {
				wait = min(time.Duration(p.Wait), maxWait)
			}

			return sess.Events(ctx, p, wait)
		}),
	}
}

func leaseHandlers(m *session.Manager) map[string]handler {
	result := func(sess *session.Session, info api.LeaseInfo, err error) (any, error) {
		if err != nil {
			return nil, err
		}

		return api.LeaseResult{SessionID: sess.ID, Lease: info}, nil
	}

	return map[string]handler{
		api.MethodLeaseStatus: onSession(m, func(_ context.Context, sess *session.Session, _ api.Client, _ api.LeaseParams) (any, error) {
			return result(sess, sess.Lease(), nil)
		}),
		api.MethodLeaseTake: onSession(m, func(_ context.Context, sess *session.Session, c api.Client, p api.LeaseParams) (any, error) {
			info, err := sess.TakeLease(c, p.Force)

			return result(sess, info, err)
		}),
		api.MethodLeaseRelease: onSession(m, func(_ context.Context, sess *session.Session, c api.Client, _ api.LeaseParams) (any, error) {
			return result(sess, sess.ReleaseLease(c), nil)
		}),
		api.MethodLeaseGrant: onSession(m, func(_ context.Context, sess *session.Session, c api.Client, p api.LeaseGrantParams) (any, error) {
			info, err := sess.GrantLease(c, p.To, p.Force)

			return result(sess, info, err)
		}),
		api.MethodLeasePolicy: onSession(m, func(_ context.Context, sess *session.Session, c api.Client, p api.LeasePolicyParams) (any, error) {
			info, err := sess.SetLeasePolicy(c, p.Policy, p.Force)

			return result(sess, info, err)
		}),
	}
}

// startSession starts a session and, when a stop is expected soon (stop on
// entry, or breakpoints), waits for it so the reply says where it stopped.
func startSession(ctx context.Context, m *session.Manager, c api.Client, p api.StartParams) (any, error) {
	sess, err := m.Start(ctx, c, p)
	if err != nil {
		return nil, err
	}

	if p.Wait > 0 && (p.StopOnEntry || len(p.Breakpoints) > 0) {
		return sess.Wait(ctx, 0, clampWait(p.Wait), p.DumpSpec), nil
	}

	return sess.Snapshot(ctx, p.DumpSpec), nil
}

// withParams decodes params of type P before calling fn.
func withParams[P any](fn func(context.Context, P) (any, error)) handler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p P
		if err := decodeParams(raw, &p); err != nil {
			return nil, err
		}

		return fn(ctx, p)
	}
}

// onSession decodes params of type P, identifies the client and resolves
// the session they name, recording that the client used it.
func onSession[P interface{ Ref() api.SessionRef }](
	m *session.Manager, fn func(context.Context, *session.Session, api.Client, P) (any, error),
) handler {
	return withParams(func(ctx context.Context, p P) (any, error) {
		ref := p.Ref()

		c, err := api.ParseClient(ref.Client)
		if err != nil {
			return nil, err
		}

		sess, err := m.Get(ref.SessionID)
		if err != nil {
			return nil, err
		}

		sess.Touch(c)

		return fn(ctx, sess, c, p)
	})
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}

	return list
}

func clampWait(d api.Duration) time.Duration {
	w := time.Duration(d)
	if w <= 0 {
		return defaultWait
	}

	return min(w, maxWait)
}

// toAPIError keeps *api.Error as is and wraps anything else as INTERNAL.
func toAPIError(err error) *api.Error {
	if e, ok := errors.AsType[*api.Error](err); ok {
		return e
	}

	return api.NewError(api.CodeInternal, err.Error(), "see 'eyedbg daemon logs'")
}
