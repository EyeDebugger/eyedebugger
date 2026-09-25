// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// startTimeout bounds building plus the DAP start-up sequence.
const startTimeout = 5 * time.Minute

// Config configures a [Manager].
type Config struct {
	// Drivers are the languages sessions can debug.
	Drivers []Driver
	Logger  *slog.Logger
	// Stderr receives the adapters' stderr.
	Stderr io.Writer
	// OnLive is called with the number of live sessions whenever it may
	// have changed (for the daemon's idle timer); nil for none.
	OnLive func(int)
	// Store persists session metadata and recordings; nil for none.
	Store Store
	// ConnectTimeout bounds how long an adapter on the connect transport
	// ([Launch.SocketArgs]) may take to dial in; zero means 30 seconds.
	ConnectTimeout time.Duration
}

// Manager owns every session of the daemon, and knows the sessions an
// earlier daemon lost.
type Manager struct {
	// ctx outlives requests: adapters are killed when it ends.
	ctx     context.Context //nolint:containedctx // The daemon's lifetime context, not a request's.
	drivers map[string]Driver
	logger  *slog.Logger
	stderr  io.Writer
	onLive  func(int)
	store   Store
	// connectTimeout is Config.ConnectTimeout, defaulted.
	connectTimeout time.Duration

	mu       sync.Mutex
	sessions map[string]*Session
	// lost are the sessions of earlier daemons, read once at start; they
	// leave only when forgotten.
	lost map[string]api.SessionInfo
}

// NewManager returns a manager. With a Store it reads the sessions earlier
// daemons lost, once, now.
func NewManager(ctx context.Context, cfg Config) *Manager {
	m := &Manager{
		ctx: ctx, drivers: make(map[string]Driver), logger: cfg.Logger, stderr: cfg.Stderr, onLive: cfg.OnLive,
		store: cfg.Store, sessions: make(map[string]*Session), lost: make(map[string]api.SessionInfo),
		connectTimeout: cfg.ConnectTimeout,
	}

	if m.connectTimeout <= 0 {
		m.connectTimeout = defaultConnectTimeout
	}

	for _, d := range cfg.Drivers {
		m.drivers[d.Name()] = d
	}

	if m.store != nil {
		lost, err := m.store.Lost()
		if err != nil {
			m.logger.WarnContext(ctx, "read lost sessions", slog.Any("error", err))
		}

		for i := range lost {
			lost[i].State = api.StateLost
			m.lost[lost[i].ID] = lost[i]
		}
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

// Start prepares and launches a new session for client c, who holds its
// lease: it launches a program, attaches to one (p.Attach) or debugs a test
// run (p.Test). It returns once the program is running (or stopped at an
// entry or early breakpoint, if wait allows).
func (m *Manager) Start(ctx context.Context, c api.Client, p api.StartParams) (*Session, error) {
	drv, ok := m.drivers[p.Lang]
	if !ok {
		return nil, api.NewError(api.CodeInvalidRequest, "unknown language "+p.Lang,
			"supported: "+strings.Join(m.Languages(), ", "))
	}

	policy, err := api.ParseLeasePolicy(string(p.LeasePolicy))
	if err != nil {
		return nil, err
	}

	if p.Attach != nil && p.Test != nil {
		return nil, api.NewError(api.CodeInvalidRequest, "a session either attaches or runs tests, not both", "")
	}

	if p, err = checkStart(p); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	switch {
	case p.Attach != nil:
		return m.startAttach(ctx, c, drv, p, policy)
	case p.Test != nil:
		return m.startTest(ctx, c, drv, p, policy)
	}

	launch, err := drv.Prepare(ctx, p.LaunchSpec)
	if err != nil {
		return nil, err
	}

	return m.run(ctx, m.create(ctx, c, p, policy, launch, ""), launch, p.Breakpoints)
}

// create makes a session and starts its recording and event log.
func (m *Manager) create(ctx context.Context, c api.Client, p api.StartParams, policy api.LeasePolicy, launch Launch, mode string) *Session {
	s := newSession(m.ctx, m.newID(), p.Lang, mode, launch, m.logger, c, policy) //nolint:contextcheck // The session outlives this request.
	s.excModes = withMode(nil, c.ID, p.Exceptions, false)

	// Recording starts before anything can be logged, so it begins with
	// the started event.
	if !p.NoRecord {
		m.record(ctx, s)
	}

	s.logStarted()

	if p.Exceptions != api.ExceptionsNone {
		s.mu.Lock()
		s.log.append(api.Event{Kind: api.EventExceptions, Client: c.ID, Action: string(p.Exceptions)})
		s.mu.Unlock()
	}

	return s
}

// run starts s's adapter, shares s and configures the debuggee; a failure
// ends s and forgets it.
func (m *Manager) run(ctx context.Context, s *Session, launch Launch, bps []api.BreakpointSpec) (*Session, error) {
	// The adapter lives as long as the daemon, not this request.
	if err := s.startAdapter(m.ctx, launch, m.stderr, m.connectTimeout); err != nil { //nolint:contextcheck // Deliberately not the request's context.
		if s.mode == api.ModeTest { // shared already: end it properly
			m.fail(ctx, s, err)

			return nil, err
		}

		s.closeRecording()

		return nil, err
	}

	// A test run's session is shared before its adapter starts: a client
	// may have stopped it meanwhile, before there was an adapter to end.
	if s.Info().State == api.StateExited {
		m.fail(ctx, s, s.stoppedWhileStarting())

		return nil, s.stoppedWhileStarting()
	}

	m.add(s)
	m.saveIfLive(s)

	if err := s.configure(ctx, launch, bps); err != nil {
		m.fail(ctx, s, err)

		return nil, err
	}

	m.logger.InfoContext(ctx, "session started", slog.String("session", s.ID), slog.String("program", s.Program))

	go m.watch(s)

	return s, nil
}

// fail ends a session whose start failed, and forgets it.
func (m *Manager) fail(ctx context.Context, s *Session, err error) {
	m.logger.WarnContext(ctx, "session start failed", slog.String("session", s.ID), slog.Any("error", err))
	s.terminate(ctx, "", "its start failed")
	m.remove(s.ID)
	m.forget(ctx, s.ID)
}

// checkStart checks what every kind of start shares: the exception mode
// (empty: none) and the breakpoints, whose anchors it resolves (before a
// build, so a wrong one fails fast).
func checkStart(p api.StartParams) (api.StartParams, error) {
	if p.Exceptions == "" {
		p.Exceptions = api.ExceptionsNone
	}

	if _, err := api.ParseExceptionMode(string(p.Exceptions)); err != nil {
		return p, err
	}

	// ClientDir too: a driver may default the working directory to it, and
	// it must name that directory the same way as the program. On Windows
	// EvalSymlinks also expands 8.3 short names (C:\Users\RUNNER~1, the
	// GitHub runner's %TEMP%), and 'go build' run from the short form
	// refuses the long form's package as "outside main module".
	p.Project, p.Program, p.Cwd = realPath(p.Project), realPath(p.Program), realPath(p.Cwd)
	p.ClientDir = realPath(p.ClientDir)

	bps := make([]api.BreakpointSpec, len(p.Breakpoints))

	for i, spec := range p.Breakpoints {
		var err error
		if bps[i], err = resolveSpec(spec, true); err != nil {
			return p, err
		}
	}

	p.Breakpoints = bps

	return p, nil
}

// realPath resolves the symlinks in p when it exists: adapters report
// and match source paths with symlinks resolved (on macOS /tmp and /var
// are symlinks into /private), so the program, its directory and its
// breakpoints are all given that way.
func realPath(p string) string {
	if p == "" {
		return p
	}

	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}

	return p
}

// record attaches a recording to s; failing to is not fatal.
func (m *Manager) record(ctx context.Context, s *Session) {
	if m.store == nil {
		return
	}

	w, path, err := m.store.Record(s.ID)
	if err != nil {
		m.logger.WarnContext(ctx, "session not recorded", slog.String("session", s.ID), slog.Any("error", err))

		return
	}

	s.attachRecorder(w, path)
}

// watch saves s's final state and reports live-count changes when s ends
// on its own.
func (m *Manager) watch(s *Session) {
	s.waitUntil(m.ctx, func() bool { return s.state == api.StateExited })
	m.saveIfLive(s)
	m.notify()
}

// saveIfLive saves s's metadata if s is still in the session map. It holds
// m.mu while saving, and Stop and StopAll unmap a session under m.mu before
// removing its metadata, so a late save can't bring back the metadata of a
// stopped session.
func (m *Manager) saveIfLive(s *Session) {
	if m.store == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.sessions[s.ID] != s {
		return
	}

	if err := m.store.Save(s.Info()); err != nil {
		m.logger.WarnContext(m.ctx, "save session metadata", slog.String("session", s.ID), slog.Any("error", err))
	}
}

// forget removes a session's metadata (not its recording).
func (m *Manager) forget(ctx context.Context, id string) {
	if m.store == nil {
		return
	}

	if err := m.store.Remove(id); err != nil {
		m.logger.WarnContext(ctx, "remove session metadata", slog.String("session", id), slog.Any("error", err))
	}
}

// Get returns the session with id, or when id is empty the only session
// that hasn't exited (else the only session). A lost session is
// NO_SESSION, with a hint.
func (m *Manager) Get(id string) (*Session, error) {
	if id == "" {
		return m.only()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if s, ok := m.sessions[id]; ok {
		return s, nil
	}

	if info, ok := m.lost[id]; ok {
		hint := "'eyedbg stop -s " + id + "' forgets it"
		if info.Recording != "" {
			hint += "; its recording: " + info.Recording
		}

		return nil, api.NewError(api.CodeNoSession, "session "+id+" was lost: eyedbgd exited while it was live", hint)
	}

	return nil, api.NewError(api.CodeNoSession, "no session "+id, "see 'eyedbg sessions'")
}

// only is Get's default session: an exited session (kept for its output
// until stopped) is the default only while no other session exists.
func (m *Manager) only() (*Session, error) {
	m.mu.Lock()
	all := slices.Collect(maps.Values(m.sessions))
	m.mu.Unlock()

	if len(all) == 0 {
		return nil, api.NewError(api.CodeNoSession, "there are no debug sessions", "start one with 'eyedbg start'")
	}

	// Sessions lock themselves after the manager, never while holding it.
	live := slices.DeleteFunc(slices.Clone(all), func(s *Session) bool { return s.Info().State == api.StateExited })

	switch {
	case len(live) == 1:
		return live[0], nil
	case len(all) == 1:
		return all[0], nil
	case len(live) == 0:
		live = all
	}

	ids := make([]string, 0, len(live))
	for _, s := range live {
		ids = append(ids, s.ID)
	}

	slices.Sort(ids)

	return nil, api.NewError(api.CodeNoSession, "several sessions exist: "+strings.Join(ids, ", "),
		"pick one with -s <id> or EYEDBG_SESSION")
}

// List returns every live and lost session, oldest first.
func (m *Manager) List() []api.SessionInfo {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}

	out := make([]api.SessionInfo, 0, len(m.sessions)+len(m.lost))
	for id := range m.lost {
		out = append(out, m.lost[id])
	}
	m.mu.Unlock()

	for _, s := range all {
		out = append(out, s.Info())
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })

	return out
}

// Stop ends a live session for client c (LEASE_HELD while another client
// holds its lease and the program is live) and forgets it; a lost session
// is just forgotten.
func (m *Manager) Stop(ctx context.Context, c api.Client, id string) (api.SessionInfo, error) {
	m.mu.Lock()
	info, lost := m.lost[id]

	if lost {
		delete(m.lost, id)
	}
	m.mu.Unlock()

	if lost {
		m.forget(ctx, id)
		m.logger.InfoContext(ctx, "lost session forgotten", slog.String("session", id))

		return info, nil
	}

	s, err := m.Get(id)
	if err != nil {
		return api.SessionInfo{}, err
	}

	if err := s.Terminate(ctx, c); err != nil {
		return api.SessionInfo{}, err
	}

	m.remove(s.ID)
	m.forget(ctx, s.ID)
	m.logger.InfoContext(ctx, "session stopped", slog.String("session", s.ID))

	return s.Info(), nil
}

// Detach ends attached session id for client c, leaving its program
// running (see Session.Detach), and forgets it.
func (m *Manager) Detach(ctx context.Context, c api.Client, id string) (api.SessionInfo, error) {
	s, err := m.Get(id)
	if err != nil {
		return api.SessionInfo{}, err
	}

	if err := s.Detach(ctx, c); err != nil {
		return api.SessionInfo{}, err
	}

	m.remove(s.ID)
	m.forget(ctx, s.ID)
	m.logger.InfoContext(ctx, "session detached", slog.String("session", s.ID))

	return s.Info(), nil
}

// StopAll ends every session, whoever holds its lease: launched programs
// and test runs are killed, attached ones detached from.
func (m *Manager) StopAll(ctx context.Context) int {
	m.mu.Lock()
	all := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}

	clear(m.sessions)
	m.mu.Unlock()
	m.notify()

	for _, s := range all {
		s.terminate(ctx, "", s.endedBy(""))
		m.forget(ctx, s.ID)
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

// newID returns an id like "s-k3f9" that no live or lost session has.
func (m *Manager) newID() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	for {
		id := "s-" + strings.ToLower(rand.Text()[:4])

		_, live := m.sessions[id]
		_, lost := m.lost[id]

		if !live && !lost {
			return id
		}
	}
}
