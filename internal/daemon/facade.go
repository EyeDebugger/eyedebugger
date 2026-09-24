// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/facade"
)

// handover takes over a connection once the response that switched it is
// written: r holds what the client sent past that request.
type handover func(ctx context.Context, r *bufio.Reader, conn net.Conn)

// facadeOpen answers api.MethodFacadeOpen: it checks the client and the
// session (errors leave the connection speaking JSON-RPC) and returns the
// hand-over that serves DAP on the connection from then on.
func (s *server) facadeOpen(raw json.RawMessage) (api.FacadeOpenResult, handover, *api.Error) {
	var p api.FacadeOpenParams
	if err := decodeParams(raw, &p); err != nil {
		return api.FacadeOpenResult{}, nil, err
	}

	c, err := api.ParseClient(p.Client)
	if err != nil {
		return api.FacadeOpenResult{}, nil, toAPIError(err)
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
		facade.Serve(ctx, facade.Config{Session: sess, Client: c, Logger: s.cfg.Logger}, r, conn)
	}

	return api.FacadeOpenResult{SessionID: sess.ID, FacadeVersion: api.FacadeVersion}, serve, nil
}
