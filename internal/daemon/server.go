// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// DefaultIdleTimeout is how long the daemon lives with no sessions.
const DefaultIdleTimeout = 30 * time.Minute

// ErrAlreadyRunning means another daemon holds this user's lock.
var ErrAlreadyRunning = errors.New("another eyedbgd is already running for this user")

// Config configures [Serve].
type Config struct {
	Paths Paths
	// IdleTimeout is how long the daemon keeps running with no sessions; 0
	// disables idle exit.
	IdleTimeout time.Duration
	Info        version.Info
	Logger      *slog.Logger
	// Drivers are the languages the daemon can debug.
	Drivers []session.Driver
}

type server struct {
	cfg       Config
	token     string
	startedAt time.Time
	shutdown  context.CancelFunc
	sessions  *session.Manager

	mu        sync.Mutex
	live      int // sessions that have not exited
	idleSince time.Time
	conns     map[net.Conn]struct{}
}

// Serve runs the daemon until ctx is canceled, a client stops it or it has
// been idle for cfg.IdleTimeout. It returns [ErrAlreadyRunning] if another
// daemon owns cfg.Paths.
//
// Holding the lock for the daemon's whole life is what makes the rest safe:
// concurrent auto-starts race on it and the losers exit, and the winner may
// remove any socket or token file it finds, since they can only be stale.
func Serve(ctx context.Context, cfg Config) error {
	if err := cfg.Paths.Prepare(); err != nil {
		return err
	}

	lock, err := lockFile(cfg.Paths.Lock)
	if err != nil {
		return err
	}
	defer lock.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s := &server{
		cfg:       cfg,
		token:     rand.Text(),
		startedAt: time.Now(),
		shutdown:  cancel,
		conns:     make(map[net.Conn]struct{}),
	}
	s.idleSince = s.startedAt
	// Adapters get their own context, canceled only after every session was
	// ended cleanly: killing an adapter outright would orphan its debuggee.
	adapterCtx, stopAdapters := context.WithCancel(context.WithoutCancel(ctx))
	defer stopAdapters()

	s.sessions = session.NewManager(adapterCtx, cfg.Drivers, cfg.Logger, os.Stderr, s.setLive)

	ln, err := s.listen(ctx)
	if err != nil {
		return err
	}

	defer func() {
		_ = os.Remove(cfg.Paths.Token)
		_ = os.Remove(cfg.Paths.Socket)
	}()

	cfg.Logger.InfoContext(ctx, "daemon started",
		slog.Int("pid", os.Getpid()), slog.String("dir", cfg.Paths.Dir),
		slog.String("version", cfg.Info.Version), slog.Duration("idle_timeout", cfg.IdleTimeout))

	var wg sync.WaitGroup

	wg.Go(func() { s.watchIdle(ctx) })
	wg.Go(func() { s.watchSocket(ctx) })
	wg.Go(func() {
		<-ctx.Done()
		_ = ln.Close()
		s.closeConns()
	})

	s.acceptLoop(ctx, ln, &wg)
	cancel()
	s.sessions.StopAll(adapterCtx)
	wg.Wait()

	cfg.Logger.InfoContext(ctx, "daemon stopped")

	return nil
}

// listen removes stale files, publishes a fresh token and opens the socket.
func (s *server) listen(ctx context.Context) (net.Listener, error) {
	p := s.cfg.Paths

	if err := os.Remove(p.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	if err := writeFileAtomic(p.Token, []byte(s.token), 0o600); err != nil {
		return nil, fmt.Errorf("write token: %w", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "unix", p.Socket)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", p.Socket, err)
	}

	// The directory already keeps others out; this is defense in depth.
	if err := os.Chmod(p.Socket, 0o600); err != nil {
		_ = ln.Close()

		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}

	return ln, nil
}

func (s *server) acceptLoop(ctx context.Context, ln net.Listener, wg *sync.WaitGroup) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				s.cfg.Logger.ErrorContext(ctx, "accept failed", slog.Any("error", err))
			}

			return
		}

		if !s.track(conn) {
			_ = conn.Close()

			return
		}

		wg.Go(func() {
			defer s.untrack(conn)
			s.serveConn(ctx, conn)
		})
	}
}

// serveConn answers requests on one connection until it closes. The first
// request must be an authenticated hello.
func (s *server) serveConn(ctx context.Context, conn net.Conn) {
	c := api.NewConn(conn, conn)
	authed := false

	for ctx.Err() == nil {
		var req api.Request
		if err := c.Read(&req); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				_ = c.Write(api.NewErrorResponse(0, api.NewError(api.CodeInvalidRequest, err.Error(), "")))
			}

			return
		}

		resp, stop := s.dispatch(ctx, req, &authed)
		if err := c.Write(resp); err != nil {
			return
		}

		if stop {
			s.cfg.Logger.InfoContext(ctx, "stop requested by client")
			s.shutdown()

			return
		}

		if !authed {
			return
		}
	}
}

// dispatch handles one request. stop reports that the daemon must exit once
// the response is written.
func (s *server) dispatch(ctx context.Context, req api.Request, authed *bool) (resp api.Response, stop bool) {
	if !*authed {
		if req.Method != api.MethodHello {
			return api.NewErrorResponse(req.ID, api.NewError(api.CodeUnauthorized,
				"the first request must be "+api.MethodHello, "")), false
		}

		result, apiErr := s.hello(req.Params)
		if apiErr != nil {
			s.cfg.Logger.WarnContext(ctx, "rejected connection", slog.String("code", string(apiErr.Code)))

			return api.NewErrorResponse(req.ID, apiErr), false
		}

		*authed = true

		return s.result(req.ID, result), false
	}

	switch req.Method {
	case api.MethodDaemonStatus:
		return s.result(req.ID, s.status()), false
	case api.MethodDaemonStop:
		var params api.StopParams
		if err := decodeParams(req.Params, &params); err != nil {
			return api.NewErrorResponse(req.ID, err), false
		}

		result, apiErr := s.stop(ctx, params)
		if apiErr != nil {
			return api.NewErrorResponse(req.ID, apiErr), false
		}

		return s.result(req.ID, result), true
	default:
		if h, ok := s.sessionHandler(req.Method); ok {
			result, err := h(ctx, req.Params)
			if err != nil {
				return api.NewErrorResponse(req.ID, toAPIError(err)), false
			}

			return s.result(req.ID, result), false
		}

		return api.NewErrorResponse(req.ID, api.NewError(api.CodeUnknownMethod,
			"unknown method "+req.Method, "the daemon may be older than this client; see 'eyedbg daemon status'")), false
	}
}

func (s *server) hello(raw json.RawMessage) (api.HelloResult, *api.Error) {
	var params api.HelloParams
	if err := decodeParams(raw, &params); err != nil {
		return api.HelloResult{}, err
	}

	if subtle.ConstantTimeCompare([]byte(params.Token), []byte(s.token)) != 1 {
		return api.HelloResult{}, api.NewError(api.CodeUnauthorized, "invalid token", "")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	return api.HelloResult{
		ProtocolVersion: api.ProtocolVersion,
		Version:         s.cfg.Info.Version,
		Commit:          s.cfg.Info.Commit,
		PID:             os.Getpid(),
		Sessions:        s.live,
	}, nil
}

func (s *server) status() api.StatusResult {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := api.StatusResult{
		PID:         os.Getpid(),
		Version:     s.cfg.Info.Version,
		Commit:      s.cfg.Info.Commit,
		StartedAt:   s.startedAt,
		Sessions:    s.live,
		Dir:         s.cfg.Paths.Dir,
		Socket:      s.cfg.Paths.Socket,
		IdleTimeout: api.Duration(s.cfg.IdleTimeout),
	}

	if s.live == 0 && s.cfg.IdleTimeout > 0 {
		at := s.idleSince.Add(s.cfg.IdleTimeout)
		res.IdleExitAt = &at
	}

	return res
}

func (s *server) stop(ctx context.Context, params api.StopParams) (api.StopResult, *api.Error) {
	s.mu.Lock()
	live := s.live
	s.mu.Unlock()

	if live > 0 && !params.Force {
		return api.StopResult{}, api.NewError(api.CodeSessionsActive,
			fmt.Sprintf("%d debug session(s) are active", live),
			"end them first with 'eyedbg stop', or run 'eyedbg daemon stop --force' to end them (their debuggees are killed)")
	}

	return api.StopResult{EndedSessions: s.sessions.StopAll(ctx)}, nil
}

// setLive records the live session count and restarts the idle clock when
// it drops to zero.
func (s *server) setLive(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if n == 0 && s.live > 0 {
		s.idleSince = time.Now()
	}

	s.live = n
}

// watchIdle stops the daemon once it has had no sessions for IdleTimeout.
func (s *server) watchIdle(ctx context.Context) {
	timeout := s.cfg.IdleTimeout
	if timeout <= 0 {
		return
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		s.mu.Lock()
		wait := timeout
		if s.live == 0 {
			wait = time.Until(s.idleSince.Add(timeout))
		}
		s.mu.Unlock()

		if wait <= 0 {
			s.cfg.Logger.InfoContext(ctx, "idle timeout reached", slog.Duration("idle_timeout", timeout))
			s.shutdown()

			return
		}

		timer.Reset(wait)
	}
}

// socketCheckInterval is how often the daemon checks that its socket still
// exists.
const socketCheckInterval = 5 * time.Second

// watchSocket stops the daemon once its socket file is gone. On Linux,
// systemd removes $XDG_RUNTIME_DIR when the user's last login session ends;
// a daemon left behind would be unreachable, and the next client would start
// a second one.
func (s *server) watchSocket(ctx context.Context) {
	ticker := time.NewTicker(socketCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if _, err := os.Stat(s.cfg.Paths.Socket); errors.Is(err, os.ErrNotExist) {
			s.cfg.Logger.WarnContext(ctx, "socket removed, shutting down", slog.String("socket", s.cfg.Paths.Socket))
			s.shutdown()

			return
		}
	}
}

func (s *server) result(id int64, v any) api.Response {
	resp, err := api.NewResult(id, v)
	if err != nil {
		return api.NewErrorResponse(id, api.NewError(api.CodeInternal, err.Error(), ""))
	}

	return resp
}

// track registers conn for closing at shutdown; false means shutdown began.
func (s *server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conns == nil {
		return false
	}

	s.conns[conn] = struct{}{}

	return true
}

func (s *server) untrack(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.conns != nil {
		delete(s.conns, conn)
	}

	_ = conn.Close()
}

func (s *server) closeConns() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for conn := range s.conns {
		_ = conn.Close()
	}

	s.conns = nil
}

func decodeParams(raw json.RawMessage, v any) *api.Error {
	if len(raw) == 0 {
		return nil
	}

	if err := json.Unmarshal(raw, v); err != nil {
		return api.NewError(api.CodeInvalidRequest, "invalid params: "+err.Error(), "")
	}

	return nil
}

// writeFileAtomic writes data to a temporary file next to path and renames it
// into place, so readers never see a partial file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}

	name := tmp.Name()

	_, werr := tmp.Write(data)
	cerr := tmp.Close()

	if err := errors.Join(werr, cerr, os.Chmod(name, perm)); err != nil {
		_ = os.Remove(name)

		return err
	}

	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)

		return err
	}

	return nil
}
