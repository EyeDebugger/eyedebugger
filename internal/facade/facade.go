// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime/debug"
	"sync"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Limits of one connection.
const (
	// maxInFlight bounds the requests handled at once; the reader waits
	// beyond it.
	maxInFlight = 32
	// maxRequest bounds one request's content.
	maxRequest = api.MaxMessageSize
	// readyTimeout bounds initialize's wait for a session still starting.
	readyTimeout = 30 * time.Second
	// maxRequestBreakpoints bounds the entries of one setBreakpoints or
	// setFunctionBreakpoints (INVALID_REQUEST beyond): no editor sends that
	// many, and each one costs every connection a mirror to reconcile.
	maxRequestBreakpoints = 1000
)

// DefaultRestartGrace is how long a client that left with disconnect
// {restart: true} (an editor's Restart) keeps its lease and editor
// breakpoints, waiting for it to connect again.
const DefaultRestartGrace = 10 * time.Second

// Config configures [Serve].
type Config struct {
	// Session is the session the connection joined.
	Session *session.Session
	// Client is who every request of the connection acts as.
	Client api.Client
	Logger *slog.Logger
	// RestartGrace is how long the client's leaving waits after a
	// disconnect asking for a restart (see [DefaultRestartGrace]); 0 leaves
	// at once.
	RestartGrace time.Duration
}

// phase is how far the connection's handshake got.
type phase int

const (
	phaseNew         phase = iota // nothing yet
	phaseInitialized              // initialize answered
	phaseAttached                 // attach answered, initialized sent
	phaseConfigured               // configurationDone answered: following the log
)

// connection is one editor's DAP connection to a session.
type connection struct {
	sess     *session.Session
	client   api.Client
	presence *session.Presence
	logger   *slog.Logger
	srv      *dap.Server
	conn     net.Conn

	// phase and invalidated are the reader's: set before the follower
	// starts, never changed after. restart is the reader's too: the
	// disconnect asked for a restart.
	phase       phase
	invalidated bool
	restart     bool

	// gate orders responses before the events they cause: a request that
	// changes something holds it from its session call until its response
	// is written (and its breakpoints are recorded), and the follower holds
	// it while it translates and writes each event. It also guards view.
	// Lock order: gate, then mu or the session's locks; neither is held
	// while taking it.
	gate sync.Mutex
	view *view

	// wg tracks the handlers and the follower.
	wg sync.WaitGroup

	mu     sync.Mutex
	closed bool
}

// bpsKey keys view.lists: one source's breakpoints (path, "" for a source
// without one), or the function breakpoints (function).
type bpsKey struct {
	function bool
	path     string
}

// funcBreakpoints is the key of the function breakpoints.
var funcBreakpoints = bpsKey{function: true} //nolint:gochecknoglobals // a constant; Go has no struct constants

// Serve speaks DAP on conn (r is conn's buffered reader, holding what was
// read past the switch) for cfg.Client on cfg.Session until the client
// disconnects, the connection breaks or ctx ends. The connection counts as
// the client's presence in the session (session.Session.Connect) until
// Serve waited for every request in flight; when it was the client's last,
// the session removes its editor breakpoints and releases its lease (after
// cfg.RestartGrace for a restart). It does not close conn.
func Serve(ctx context.Context, cfg Config, r *bufio.Reader, conn net.Conn) {
	logger := cfg.Logger.With(slog.String("session", cfg.Session.ID), slog.String("client", cfg.Client.ID))

	codec := godap.NewCodec()
	if err := RegisterMessages(codec); err != nil {
		logger.ErrorContext(ctx, "facade codec", slog.Any("error", err))

		return
	}

	c := &connection{
		sess:     cfg.Session,
		client:   cfg.Client,
		presence: cfg.Session.Connect(cfg.Client),
		logger:   logger,
		srv:      dap.NewServer(r, conn, codec, maxRequest),
		conn:     conn,
		view:     newView(),
	}

	c.logger.InfoContext(ctx, "facade opened")

	connCtx, cancel := context.WithCancel(ctx)

	c.protect(ctx, func() { c.readLoop(connCtx) })
	cancel()
	c.wg.Wait()

	grace := time.Duration(0)
	if c.restart {
		grace = cfg.RestartGrace
	}

	c.protect(ctx, func() { c.presence.Leave(ctx, grace) })
	c.logger.InfoContext(ctx, "facade closed")
}

// inFlight is where the reader hands the requests it doesn't handle
// itself: each holds a slot of sem until it is answered.
type inFlight struct {
	sem chan struct{}
	// ordered feeds the connection's one worker, which handles the
	// requests that change something in arrival order (a setBreakpoints
	// replaces the one before it). It never blocks: it holds at most one
	// request per slot.
	ordered chan func()
}

// readLoop reads and dispatches requests until the stream ends, breaks or
// the client disconnects. Lifecycle requests are handled here, in order;
// requests that change something by one worker, in order; reads on their
// own goroutines. At most maxInFlight are handled or waiting at a time.
func (c *connection) readLoop(ctx context.Context) {
	in := inFlight{sem: make(chan struct{}, maxInFlight), ordered: make(chan func(), maxInFlight)}

	c.wg.Go(func() {
		for fn := range in.ordered {
			c.protect(ctx, fn)
			<-in.sem
		}
	})

	defer close(in.ordered)

	for {
		req, err := c.srv.ReadRequest()
		if de, ok := errors.AsType[*dap.DecodeError](err); ok {
			c.failDecode(ctx, de)

			continue
		}

		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				c.logger.DebugContext(ctx, "facade connection ended", slog.String("reason", readEndReason(err)))
			}

			return
		}

		c.sess.Touch(c.client)

		if done := c.dispatch(ctx, req, in); done {
			return
		}
	}
}

// readEndReason names why reading stopped, without the peer's bytes.
func readEndReason(err error) string {
	if errors.Is(err, dap.ErrFraming) {
		return "framing"
	}

	return "io"
}

// failDecode answers a request that couldn't be decoded.
func (c *connection) failDecode(ctx context.Context, de *dap.DecodeError) {
	msg := "invalid arguments for " + de.Command
	if _, unknown := errors.AsType[*godap.DecodeProtocolMessageFieldError](de.Err); unknown {
		msg = "eyedbg's DAP facade does not support " + de.Command
	}

	req := &godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: de.Seq}, Command: de.Command}
	c.fail(ctx, req, api.NewError(api.CodeInvalidRequest, msg, ""), false)
}

// protect runs fn, turning a panic into a logged error and a closed
// connection: one connection's bug must not end the daemon's sessions.
func (c *connection) protect(ctx context.Context, fn func()) {
	defer func() {
		if v := recover(); v != nil {
			c.logger.ErrorContext(ctx, "facade panic", slog.String("value_type", fmt.Sprintf("%T", v)),
				slog.String("stack", string(debug.Stack())))
			c.close()
		}
	}()

	fn()
}

// spawn hands fn, holding a slot of in.sem, to the worker if ordered, else
// to its own goroutine.
func (c *connection) spawn(ctx context.Context, in inFlight, ordered bool, fn func()) {
	select {
	case in.sem <- struct{}{}:
	case <-ctx.Done():
		return
	}

	if ordered {
		in.ordered <- fn

		return
	}

	c.wg.Go(func() {
		defer func() { <-in.sem }()

		c.protect(ctx, fn)
	})
}

// close closes the connection (once), which ends the reader.
func (c *connection) close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.closed {
		c.closed = true
		_ = c.conn.Close()
	}
}
