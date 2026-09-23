// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
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

// sessionHandler returns the handler for a session method.
func (s *server) sessionHandler(method string) (handler, bool) {
	h, ok := sessionHandlers(s.sessions)[method]

	return h, ok
}

func sessionHandlers(m *session.Manager) map[string]handler {
	return map[string]handler{
		api.MethodSessionStart: withParams(func(ctx context.Context, p api.StartParams) (any, error) {
			return startSession(ctx, m, p)
		}),
		api.MethodSessionList: func(context.Context, json.RawMessage) (any, error) { return m.List(), nil },
		api.MethodSessionStop: withParams(func(ctx context.Context, p api.SessionRef) (any, error) {
			return m.Stop(ctx, p.SessionID)
		}),
		api.MethodSessionStatus: onSession(m, func(ctx context.Context, sess *session.Session, _ api.SessionRef) (any, error) {
			return sess.Snapshot(ctx), nil
		}),
		api.MethodExec: onSession(m, func(ctx context.Context, sess *session.Session, p api.ExecParams) (any, error) {
			return sess.Resume(ctx, p.Kind, p.ThreadID, clampWait(p.Wait))
		}),
		api.MethodWait: onSession(m, func(ctx context.Context, sess *session.Session, p api.WaitParams) (any, error) {
			after := p.AfterStops
			if after < 0 {
				after = sess.Stops()
			}

			return sess.Wait(ctx, after, clampWait(p.Wait)), nil
		}),
		api.MethodBreakpointAdd: onSession(m, func(ctx context.Context, sess *session.Session, p api.BreakpointAddParams) (any, error) {
			return sess.AddBreakpoint(ctx, p.BreakpointSpec)
		}),
		api.MethodBreakpointLs: onSession(m, func(_ context.Context, sess *session.Session, _ api.SessionRef) (any, error) {
			return nonNil(sess.Breakpoints()), nil
		}),
		api.MethodBreakpointRm: onSession(m, func(ctx context.Context, sess *session.Session, p api.BreakpointRemoveParams) (any, error) {
			n, err := sess.RemoveBreakpoint(ctx, p.ID)

			return api.BreakpointRemoveResult{Removed: n}, err
		}),
		api.MethodStack: onSession(m, func(ctx context.Context, sess *session.Session, p api.StackParams) (any, error) {
			return sess.Stack(ctx, p.ThreadID, p.Levels)
		}),
		api.MethodVars: onSession(m, func(ctx context.Context, sess *session.Session, p api.VarsParams) (any, error) {
			return sess.Vars(ctx, p.Frame, max(p.Depth, 1))
		}),
		api.MethodEval: onSession(m, func(ctx context.Context, sess *session.Session, p api.EvalParams) (any, error) {
			return sess.Eval(ctx, p.Expression, p.Frame)
		}),
		api.MethodOutput: onSession(m, func(_ context.Context, sess *session.Session, p api.OutputParams) (any, error) {
			return nonNil(sess.Output(p.Since, p.Tail)), nil
		}),
	}
}

// startSession starts a session and, when a stop is expected soon (stop on
// entry, or breakpoints), waits for it so the reply says where it stopped.
func startSession(ctx context.Context, m *session.Manager, p api.StartParams) (any, error) {
	sess, err := m.Start(ctx, p)
	if err != nil {
		return nil, err
	}

	if p.Wait > 0 && (p.StopOnEntry || len(p.Breakpoints) > 0) {
		return sess.Wait(ctx, 0, clampWait(p.Wait)), nil
	}

	return sess.Snapshot(ctx), nil
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

// onSession decodes params of type P and resolves the session they name.
func onSession[P interface{ Ref() api.SessionRef }](
	m *session.Manager, fn func(context.Context, *session.Session, P) (any, error),
) handler {
	return withParams(func(ctx context.Context, p P) (any, error) {
		sess, err := m.Get(p.Ref().SessionID)
		if err != nil {
			return nil, err
		}

		return fn(ctx, sess, p)
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
