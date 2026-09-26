// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/facade"
)

// handover takes over a connection once the response that switched it is
// written: r holds what the client sent past that request.
type handover func(ctx context.Context, r *bufio.Reader, conn net.Conn)

// facadeOpen answers api.MethodFacadeOpen: it checks the client and the
// session, or for a launch connection what it launches with (errors leave
// the connection speaking JSON-RPC), and returns the hand-over that serves
// DAP on the connection from then on.
func (s *server) facadeOpen(raw json.RawMessage) (api.FacadeOpenResult, handover, *api.Error) {
	var p api.FacadeOpenParams
	if err := decodeParams(raw, &p); err != nil {
		return api.FacadeOpenResult{}, nil, err
	}

	c, err := api.ParseClient(p.Client)
	if err != nil {
		return api.FacadeOpenResult{}, nil, toAPIError(err)
	}

	if p.Launch != nil {
		return s.facadeLaunch(c, p)
	}

	sess, err := s.sessions.Get(p.SessionID)
	if err != nil {
		return api.FacadeOpenResult{}, nil, toAPIError(err)
	}

	if sess.Info().State == api.StateExited {
		return api.FacadeOpenResult{}, nil, api.NewError(api.CodeSessionExited, "session "+sess.ID+" has exited",
			"see 'eyedbg status -s "+sess.ID+"'; start a new one with 'eyedbg start'")
	}

	sess.Touch(c)

	serve := func(ctx context.Context, r *bufio.Reader, conn net.Conn) {
		facade.Serve(ctx, facade.Config{Session: sess, Client: c, Logger: s.cfg.Logger, RestartGrace: facade.DefaultRestartGrace}, r, conn)
	}

	return api.FacadeOpenResult{SessionID: sess.ID, FacadeVersion: api.FacadeVersion}, serve, nil
}

// facadeLaunch opens a launch connection: no session yet, its DAP launch
// starts one (docs/adr/0019).
func (s *server) facadeLaunch(c api.Client, p api.FacadeOpenParams) (api.FacadeOpenResult, handover, *api.Error) {
	l := *p.Launch

	switch {
	case p.SessionID != "":
		return api.FacadeOpenResult{}, nil, api.NewError(api.CodeInvalidRequest, "a launch connection joins no session: drop the session id", "")
	case !filepath.IsAbs(l.ClientDir):
		return api.FacadeOpenResult{}, nil, api.NewError(api.CodeInvalidRequest, "a launch connection needs the caller's directory as an absolute path", "")
	case l.VirtualEnv != "" && !filepath.IsAbs(l.VirtualEnv):
		return api.FacadeOpenResult{}, nil, api.NewError(api.CodeInvalidRequest, "a launch connection's virtual environment must be an absolute path", "")
	case strings.ContainsRune(l.ClientDir+l.VirtualEnv+l.DotnetAdapter, 0):
		return api.FacadeOpenResult{}, nil, api.NewError(api.CodeInvalidRequest, "a launch connection's settings hold a NUL character", "")
	}

	launcher := &facade.Launcher{Manager: s.sessions, Defaults: l}

	serve := func(ctx context.Context, r *bufio.Reader, conn net.Conn) {
		facade.Serve(ctx, facade.Config{Launcher: launcher, Client: c, Logger: s.cfg.Logger, RestartGrace: facade.DefaultRestartGrace}, r, conn)
	}

	return api.FacadeOpenResult{FacadeVersion: api.FacadeVersion}, serve, nil
}
