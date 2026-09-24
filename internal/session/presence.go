// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// leaveTimeout bounds the cleanup after a client's last editor connection
// closed.
const leaveTimeout = 10 * time.Second

// presence is one client's open editor (DAP) connections to a session.
// An entry exists while the client has a connection or its leaving's
// cleanup is pending.
type presence struct {
	conns int
	// gen is the session-wide number of the client's newest connection: a
	// cleanup captured with an older one is superseded.
	gen uint64
	// excSet: an editor of the client last set an exception mode other
	// than none, which leaving resets.
	excSet bool
}

// Presence is one editor connection of a client to a session, from
// [Session.Connect] to [Presence.Leave].
type Presence struct {
	s *Session
	c api.Client
	// left is set, under s.mu, by the first Leave.
	left bool
}

// Connect records that client c opened an editor (DAP) connection to the
// session (docs/adr/0014): c is seen (as [Session.Touch]), its connection
// count goes up and, while the session is live, a client event with
// action connected is logged. The connection ends with [Presence.Leave].
func (s *Session) Connect(c api.Client) *Presence {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.touchLocked(c)

	if s.presence == nil {
		s.presence = make(map[string]*presence)
	}

	p := s.presence[c.ID]
	if p == nil {
		p = &presence{}
		s.presence[c.ID] = p
	}

	s.connGen++
	p.conns, p.gen = p.conns+1, s.connGen

	if s.state != api.StateExited {
		s.log.append(api.Event{Kind: api.EventClient, Client: c.ID, Action: "connected"})
	}

	return &Presence{s: s, c: c}
}

// ExceptionsSet records that the connection set its client's exception
// mode to mode: when the client leaves, a mode other than none is reset.
func (p *Presence) ExceptionsSet(mode api.ExceptionMode) {
	p.s.mu.Lock()
	defer p.s.mu.Unlock()

	if e := p.s.presence[p.c.ID]; e != nil && !p.left {
		e.excSet = mode != api.ExceptionsNone
	}
}

// Leave ends the connection; later calls do nothing. A client event with
// action disconnected is logged while the session is live. When it was the
// client's last connection, the session is live and ctx (the daemon's) is
// not done, the client leaves: its editor breakpoints are removed, the
// exception mode an editor of it set is reset, its lease request dropped
// and its lease released (reason disconnected), in that order — at once,
// or after grace unless one of its connections comes back meanwhile. Each
// step first checks, under mu, that no connection of the client opened
// since.
func (p *Presence) Leave(ctx context.Context, grace time.Duration) {
	gen, ok := p.decrement(ctx)
	if !ok {
		return
	}

	s := p.s

	if grace <= 0 {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaveTimeout)
		defer cancel()

		s.leave(cleanupCtx, p.c, gen)

		return
	}

	time.AfterFunc(grace, func() { //nolint:contextcheck // The cleanup runs after Leave returned, on the manager's context.
		if s.life.Err() != nil {
			return
		}

		cleanupCtx, cancel := context.WithTimeout(s.life, leaveTimeout)
		defer cancel()

		s.leave(cleanupCtx, p.c, gen)
	})
}

// decrement is Leave's first phase: it counts the connection out and
// reports whether the client left (its last connection closed, the session
// is live and ctx not done), with the generation its cleanup is valid for.
func (p *Presence) decrement(ctx context.Context) (gen uint64, cleanup bool) {
	s := p.s

	s.mu.Lock()
	defer s.mu.Unlock()

	if p.left {
		return 0, false
	}

	p.left = true

	e := s.presence[p.c.ID]
	if e == nil {
		return 0, false
	}

	e.conns--

	live := s.state != api.StateExited
	if live {
		s.log.append(api.Event{Kind: api.EventClient, Client: p.c.ID, Action: "disconnected"})
	}

	if e.conns > 0 {
		return 0, false
	}

	if !live || ctx.Err() != nil {
		delete(s.presence, p.c.ID)

		return 0, false
	}

	return e.gen, true
}

// leave is Leave's second phase, the cleanup of client c's leaving,
// captured at generation gen. Each step runs only while leftLocked holds.
func (s *Session) leave(ctx context.Context, c api.Client, gen uint64) {
	left := func() bool { return s.leftLocked(c.ID, gen) }

	n, _, err := s.removeBreakpoints(ctx, c.ID, func(b *breakpoint) bool {
		return b.Owner == c.ID && b.Editor && !b.Temporary && left()
	})
	if err != nil {
		s.logger.WarnContext(ctx, "remove a departed editor's breakpoints", slog.String("code", string(api.CodeOf(err))))
	} else if n > 0 {
		s.logger.DebugContext(ctx, "removed a departed editor's breakpoints", slog.Int("count", n))
	}

	s.mu.Lock()
	reset := left() && s.presence[c.ID].excSet && slices.ContainsFunc(s.excModes,
		func(m api.ClientExceptionMode) bool { return m.Client == c.ID })
	s.mu.Unlock()

	if reset {
		if _, err := s.setExceptionMode(ctx, c, api.ExceptionsNone, false, left); err != nil {
			s.logger.WarnContext(ctx, "reset a departed editor's exception mode", slog.String("code", string(api.CodeOf(err))))
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !left() {
		return
	}

	s.dropRequestLocked(c.ID)

	if s.lease.holder.ID == c.ID {
		s.setHolderLocked(api.Client{}, c.ID, "release", "disconnected")
	}

	delete(s.presence, c.ID)
}

// leftLocked reports whether client c's leaving captured at gen still
// holds: the session is live, and c has no connection and opened none
// since.
func (s *Session) leftLocked(clientID string, gen uint64) bool {
	e := s.presence[clientID]

	return s.state != api.StateExited && e != nil && e.conns == 0 && e.gen == gen
}

// clientsLocked returns the session's clients with their open editor
// connections counted.
func (s *Session) clientsLocked() []api.ClientInfo {
	clients := slices.Clone(s.clients)

	for i := range clients {
		if e := s.presence[clients[i].ID]; e != nil {
			clients[i].Connected = e.conns
		}
	}

	return clients
}
