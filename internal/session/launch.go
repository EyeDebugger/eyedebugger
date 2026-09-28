// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"io"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// terminalTimeout bounds a Terminal hook's run.
const terminalTimeout = 30 * time.Second

// TerminalHint is the hint of a start that can't run its program in a
// terminal.
const TerminalHint = `use "console": "internalConsole"`

// ErrTerminalTimeout is the cause of a Terminal hook's ctx that ended
// because the hook ran for 30 s ([LaunchHooks.Terminal]).
var ErrTerminalTimeout = errors.New("no answer within " + terminalTimeout.String())

// LaunchHooks let the caller of [Manager.Launch] take part in the start.
type LaunchHooks struct {
	// Output, when set, receives the build's output as it is produced, from
	// a driver that streams it ([OptionsPreparer]); nothing else. It is
	// not written to once the build is done, and never to the session's
	// event log.
	Output io.Writer
	// Configure, when set, is called once the adapter is initialized and
	// the starter's breakpoints and exception filters are sent, before
	// configurationDone: the session is then listed and starting, and no
	// lock of it is held, so Configure may set breakpoints and exception
	// modes as any client would. An error from it (or ctx ending while it
	// runs) fails the start: the session is ended and forgotten, and
	// Launch returns that error.
	Configure func(ctx context.Context, s *Session) error
	// Terminal, when set, makes the start run the program in the caller's
	// terminal: the driver prepares the launch for it
	// ([PrepareOptions.Terminal]), the adapter is told it may ask
	// (supportsRunInTerminalRequest), and the first runInTerminal request
	// the adapter sends while the start runs is handed to Terminal, on its
	// own goroutine, with a ctx that ends with the start or after 30 s
	// (its cause then [ErrTerminalTimeout]).
	// Terminal returns the process id it started (≥ 1), which is the
	// adapter's answer, or an error, which fails the start at once: Launch
	// returns it (an *api.Error unchanged, any other as ADAPTER_ERROR).
	// Every other runInTerminal request is refused, as for any start
	// without Terminal.
	Terminal func(ctx context.Context, args godap.RunInTerminalRequestArguments) (pid int, err error)
}

// Launch starts a session that launches a program for client c, who holds
// its lease, as [Manager.Start] does (the same checks, time limit and
// order), with h taking part: the build's output streams to h.Output,
// h.Configure runs during the configuration, and h.Terminal runs the
// program in a terminal. p may neither attach nor run tests.
func (m *Manager) Launch(ctx context.Context, c api.Client, p api.StartParams, h LaunchHooks) (*Session, error) {
	if p.Attach != nil || p.Test != nil {
		return nil, api.NewError(api.CodeInvalidRequest, "a launch neither attaches nor runs tests", "")
	}

	drv, policy, p, err := m.checkedStart(p)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	opts := PrepareOptions{Output: h.Output, Terminal: h.Terminal != nil}

	var launch Launch

	switch op, ok := drv.(OptionsPreparer); {
	case ok:
		launch, err = op.PrepareWith(ctx, p.LaunchSpec, opts)
	case opts.Terminal:
		err = api.NewError(api.CodeUnsupported, "the "+p.Lang+" debug adapter can't run the program in a terminal", TerminalHint)
	default:
		launch, err = drv.Prepare(ctx, p.LaunchSpec)
	}

	if err != nil {
		return nil, err
	}

	s := m.create(ctx, c, p, policy, launch, "")
	if h.Terminal == nil {
		return m.run(ctx, s, launch, p.Breakpoints, h.Configure)
	}

	return m.runWithTerminal(ctx, s, launch, p.Breakpoints, h)
}

// terminalRoute is a start's way to its caller's terminal (Session.term):
// the first runInTerminal request the adapter sends goes to reqs (taken
// then), every later one is refused. It is open from before the adapter
// starts until the start is over.
type terminalRoute struct {
	reqs  chan terminalRequest // capacity 1: onReverse never blocks
	taken bool
}

// terminalRequest is an adapter's runInTerminal and how to answer it.
type terminalRequest struct {
	args  godap.RunInTerminalRequestArguments
	reply func(godap.ResponseMessage)
}

// failedTerminalError is why a start's terminal failed: the cause of the
// start context it ended.
type failedTerminalError struct{ err error }

func (f *failedTerminalError) Error() string { return f.err.Error() }

func (f *failedTerminalError) Unwrap() error { return f.err }

// onReverse takes the adapter's reverse request req if it is a
// runInTerminal the open route of s's start hasn't served yet (on the DAP
// read goroutine: it only hands req over); anything else is left to the
// client, which refuses it.
func (s *Session) onReverse(req godap.RequestMessage, reply func(godap.ResponseMessage)) bool {
	r, ok := req.(*godap.RunInTerminalRequest)
	if !ok {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.term == nil || s.term.taken {
		return false
	}

	s.term.taken = true
	s.term.reqs <- terminalRequest{args: r.Arguments, reply: reply}

	return true
}

// runWithTerminal is [Manager.run] for a start with h.Terminal: it opens
// s's terminal route before the adapter starts, serves it while the start
// runs, and closes it, the server done, before it returns. A terminal
// that fails during the start fails the start with the terminal's error.
func (m *Manager) runWithTerminal(ctx context.Context, s *Session, launch Launch, bps []api.BreakpointSpec, h LaunchHooks) (*Session, error) {
	startCtx, fail := context.WithCancelCause(ctx)
	defer fail(nil)

	reqs := make(chan terminalRequest, 1)

	s.mu.Lock()
	s.term = &terminalRoute{reqs: reqs}
	s.mu.Unlock()

	served := make(chan struct{})

	go func() {
		defer close(served)

		serveTerminal(startCtx, reqs, h.Terminal, fail)
	}()

	sess, err := m.run(startCtx, s, launch, bps, h.Configure)

	// Close the route: no request is taken from here on. Then end the
	// server (a hook still running sees its ctx end) and wait for it: it
	// has answered what it took. A request it didn't take (it ended with
	// ctx first) is refused.
	s.mu.Lock()
	s.term = nil
	s.mu.Unlock()
	fail(nil)
	<-served

	select {
	case tr := <-reqs:
		tr.reply(terminalRefusal(errors.New("the start is over")))
	default:
	}

	failure, failed := errors.AsType[*failedTerminalError](context.Cause(startCtx))

	switch {
	case !failed:
		return sess, err
	case err == nil:
		// The start was done as the terminal failed: end it all the same.
		m.fail(ctx, s, failure.err)
	}

	return nil, failure.err
}

// serveTerminal serves the one request a terminal route takes, unless
// ctx ends first: it runs it through run, answers the adapter, and on
// failure ends the start (fail, with a *failedTerminalError).
func serveTerminal(ctx context.Context, reqs <-chan terminalRequest,
	run func(context.Context, godap.RunInTerminalRequestArguments) (int, error), fail context.CancelCauseFunc,
) {
	var tr terminalRequest

	select {
	case tr = <-reqs:
	case <-ctx.Done():
		return
	}

	hctx, cancel := context.WithTimeoutCause(ctx, terminalTimeout, ErrTerminalTimeout)
	pid, err := run(hctx, tr.args)
	timedOut := errors.Is(context.Cause(hctx), ErrTerminalTimeout)

	cancel()

	if err == nil && pid < 1 {
		err = api.NewError(api.CodeAdapterFailed, "the terminal reported no process id", TerminalHint)
	}

	if err == nil {
		tr.reply(&godap.RunInTerminalResponse{
			Response: godap.Response{Success: true},
			Body:     godap.RunInTerminalResponseBody{ProcessId: pid},
		})

		return
	}

	apiErr, ok := errors.AsType[*api.Error](err)

	switch {
	case ok:
	case timedOut:
		apiErr = api.NewError(api.CodeAdapterFailed, "the terminal didn't start the program within "+terminalTimeout.String(), TerminalHint)
	default:
		apiErr = api.NewError(api.CodeAdapterFailed, err.Error(), TerminalHint)
	}

	tr.reply(terminalRefusal(apiErr))
	fail(&failedTerminalError{err: apiErr})
}

// terminalRefusal is the failed answer to a runInTerminal request.
func terminalRefusal(err error) godap.ResponseMessage {
	msg := "eyedbg: " + err.Error()

	return &godap.ErrorResponse{
		Response: godap.Response{Message: msg},
		Body:     godap.ErrorResponseBody{Error: &godap.ErrorMessage{Format: msg}},
	}
}
