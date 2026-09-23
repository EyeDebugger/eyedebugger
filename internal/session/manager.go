// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// startTimeout bounds building plus the DAP start-up sequence.
const startTimeout = 5 * time.Minute

// Manager owns every session of the daemon.
type Manager struct {
	// ctx outlives requests: adapters are killed when it ends.
	ctx     context.Context //nolint:containedctx // The daemon's lifetime context, not a request's.
	drivers map[string]Driver
	logger  *slog.Logger
	stderr  io.Writer
	// onLive is called with the number of live sessions whenever it may
	// have changed (for the daemon's idle timer).
	onLive func(int)

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager returns a manager. Adapters' stderr goes to stderr.
func NewManager(ctx context.Context, drivers []Driver, logger *slog.Logger, stderr io.Writer, onLive func(int)) *Manager {
	m := &Manager{
		ctx: ctx, drivers: make(map[string]Driver), logger: logger, stderr: stderr, onLive: onLive,
		sessions: make(map[string]*Session),
	}

	for _, d := range drivers {
		m.drivers[d.Name()] = d
	}

	return m
}

// Languages returns the registered driver names.
func (m *Manager) Languages() []string {
	names := make([]string, 0, len(m.drivers))
	for n := range m.drivers {
		names = append(names, n)
	}

	sort.Strings(names)

	return names
}

// Start prepares and launches a new session. It returns once the program is
// running (or stopped at an entry or early breakpoint, if wait allows).
func (m *Manager) Start(ctx context.Context, p api.StartParams) (*Session, error) {
	drv, ok := m.drivers[p.Lang]
	if !ok {
		return nil, api.NewError(api.CodeInvalidRequest, "unknown language "+p.Lang,
			"supported: "+strings.Join(m.Languages(), ", "))
	}

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	launch, err := drv.Prepare(ctx, p.LaunchSpec)
	if err != nil {
		return nil, err
	}

	s := newSession(m.ctx, m.newID(), p.Lang, launch, m.logger) //nolint:contextcheck // The session outlives this request.

	// The adapter lives as long as the daemon, not this request.
	if err := s.startAdapter(m.ctx, launch, m.stderr); err != nil { //nolint:contextcheck // Deliberately not the request's context.
		return nil, err
	}

	m.add(s)

	if err := s.configure(ctx, launch, p.Breakpoints); err != nil {
		m.logger.WarnContext(ctx, "session start failed", slog.String("session", s.ID), slog.Any("error", err))
		s.Terminate(ctx)
		m.remove(s.ID)

		return nil, err
	}

	m.logger.InfoContext(ctx, "session started", slog.String("session", s.ID), slog.String("program", s.Program))

	go m.watch(s)

	return s, nil
}

// watch reports live-count changes when s ends on its own.
func (m *Manager) watch(s *Session) {
	s.waitUntil(m.ctx, func() bool { return s.state == api.StateExited })
	m.notify()
}

// Get returns the session with id, or the only session when id is empty.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if id != "" {
		if s, ok := m.sessions[id]; ok {
			return s, nil
		}

		return nil, api.NewError(api.CodeNoSession, "no session "+id, "see 'eyedbg sessions'")
	}

	switch len(m.sessions) {
	case 0:
		return nil, api.NewError(api.CodeNoSession, "there are no debug sessions", "start one with 'eyedbg start'")
	case 1:
		for _, s := range m.sessions {
			return s, nil
		}
	}

	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	return nil, api.NewError(api.CodeNoSession, "several sessions exist: "+strings.Join(ids, ", "),
		"pick one with -s <id> or EYEDBG_SESSION")
}

// List returns every session, oldest first.
func (m *Manager) List() []api.SessionInfo {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()

	out := make([]api.SessionInfo, 0, len(all))
	for _, s := range all {
		out = append(out, s.Info())
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })

	return out
}

// Stop terminates a session and forgets it.
func (m *Manager) Stop(ctx context.Context, id string) (api.SessionInfo, error) {
	s, err := m.Get(id)
	if err != nil {
		return api.SessionInfo{}, err
	}

	s.Terminate(ctx)
	m.remove(s.ID)
	m.logger.InfoContext(ctx, "session stopped", slog.String("session", s.ID))

	return s.Info(), nil
}

// StopAll terminates every session.
func (m *Manager) StopAll(ctx context.Context) int {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()

	for _, s := range all {
		s.Terminate(ctx)
		m.remove(s.ID)
	}

	return len(all)
}

// Live returns how many sessions have not exited.
func (m *Manager) Live() int {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()

	n := 0

	for _, s := range all {
		if s.Info().State != api.StateExited {
			n++
		}
	}

	return n
}

func (m *Manager) add(s *Session) {
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) notify() {
	if m.onLive != nil {
		m.onLive(m.Live())
	}
}

// newID returns an unused short id like "s-k3f9".
func (m *Manager) newID() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	for {
		id := "s-" + strings.ToLower(rand.Text()[:4])
		if _, taken := m.sessions[id]; !taken {
			return id
		}
	}
}
