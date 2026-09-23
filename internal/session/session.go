// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"sync"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/present"
)

// Timeouts for talking to an adapter.
const (
	requestTimeout    = 30 * time.Second
	shutdownTimeout   = 3 * time.Second
	maxOutputLines    = 2000
	maxSourceFileSize = 4 << 20
	// snapshotOutput is how many output chunks a snapshot carries.
	snapshotOutput = 20
	// dumpFrames is how many frames a stack dump carries.
	dumpFrames = 10
	// Adapters may send a program's last output after the stopped event: a
	// stop is reported once output has been quiet for outputQuiet, waiting
	// at most outputSettleMax.
	outputQuiet     = 50 * time.Millisecond
	outputSettleMax = 250 * time.Millisecond
)

// Session is one debuggee driven through one adapter process.
type Session struct {
	ID        string
	Lang      string
	Program   string
	CreatedAt time.Time

	cmd    *exec.Cmd
	client *dap.Client
	logger *slog.Logger
	// life is the manager's context; shutdown work started by adapter
	// events (not by a request) runs under it.
	life context.Context //nolint:containedctx // Outlives requests by design, like the adapter it tears down.

	caps        godap.Capabilities
	initialized chan struct{}
	initOnce    sync.Once

	mu        sync.Mutex
	state     api.SessionState
	stops     int // number of stopped events so far
	stop      api.StopInfo
	pid       int
	exitCode  *int
	endReason string
	bps       map[string][]*breakpoint // by absolute file path
	nextBP    int
	output    []api.OutputLine
	outSeq    int
	resumedAt int           // outSeq when the program last resumed
	changed   chan struct{} // closed and replaced on every state change
	ended     bool          // adapter torn down

	// captureMu serializes fetching stop captures; cur is the capture of
	// the current (or last) stop, prev the one before it.
	captureMu sync.Mutex
	cur, prev *stopCapture
}

// stopCapture is frame 0's locals at one stop, kept to diff the next stop
// against.
type stopCapture struct {
	stops  int
	frame  api.Frame
	locals []api.Var
	more   int
}

type breakpoint struct {
	api.Breakpoint

	adapterID int
}

func newSession(life context.Context, id, lang string, launch Launch, logger *slog.Logger) *Session {
	return &Session{
		life:        life,
		ID:          id,
		Lang:        lang,
		Program:     launch.Program,
		CreatedAt:   time.Now(),
		logger:      logger.With(slog.String("session", id)),
		initialized: make(chan struct{}),
		state:       api.StateStarting,
		bps:         make(map[string][]*breakpoint),
		changed:     make(chan struct{}),
	}
}

// startAdapter launches the adapter process and connects a DAP client to it.
func (s *Session) startAdapter(ctx context.Context, launch Launch, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, launch.Adapter, launch.AdapterArgs...) //nolint:gosec // The adapter path comes from the driver (installed netcoredbg or EYEDBG_NETCOREDBG).
	cmd.Env = append(cmd.Environ(), launch.AdapterEnv...)
	cmd.Stderr = stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("adapter stdin: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("adapter stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return api.NewError(api.CodeAdapterFailed, "start "+launch.Adapter+": "+err.Error(), "check 'eyedbg adapters doctor'")
	}

	s.cmd = cmd
	s.client = dap.NewClient(stdout, stdin, dap.Handlers{Event: s.onEvent})

	go s.watchAdapter()

	return nil
}

// configure runs the DAP start-up sequence: initialize, launch, breakpoints,
// configurationDone.
func (s *Session) configure(ctx context.Context, launch Launch, bps []api.BreakpointSpec) error {
	if err := s.initialize(ctx, launch.AdapterID); err != nil {
		return err
	}

	args, err := json.Marshal(launch.Arguments)
	if err != nil {
		return fmt.Errorf("encode launch arguments: %w", err)
	}

	// Some adapters answer launch only after configurationDone, so it is
	// sent without waiting.
	launchDone := make(chan error, 1)

	go func() {
		_, err := s.client.Do(ctx, &godap.LaunchRequest{Request: godap.Request{Command: "launch"}, Arguments: args})
		launchDone <- err
	}()

	if err := s.waitInitialized(ctx, launchDone); err != nil {
		return err
	}

	if err := s.configurationPhase(ctx, bps); err != nil {
		return err
	}

	select {
	case err := <-launchDone:
		if err != nil {
			return adapterErr(err)
		}
	case <-ctx.Done():
		return fmt.Errorf("wait for launch: %w", ctx.Err())
	}

	s.mu.Lock()
	if s.state == api.StateStarting {
		s.setStateLocked(api.StateRunning)
	}
	s.mu.Unlock()

	return nil
}

func (s *Session) initialize(ctx context.Context, adapterID string) error {
	resp, err := dap.Call[*godap.InitializeResponse](ctx, s.client, &godap.InitializeRequest{
		Request: godap.Request{Command: "initialize"},
		Arguments: godap.InitializeRequestArguments{
			ClientID: "eyedbg", ClientName: "EyeDebugger", AdapterID: adapterID,
			LinesStartAt1: true, ColumnsStartAt1: true, PathFormat: "path",
			SupportsVariableType: true,
		},
	})
	if err != nil {
		return adapterErr(err)
	}

	s.caps = resp.Body

	return nil
}

// waitInitialized waits for the adapter's initialized event. A launch that
// fails first is reported instead.
func (s *Session) waitInitialized(ctx context.Context, launchDone chan error) error {
	select {
	case <-s.initialized:
		return nil
	case err := <-launchDone:
		if err != nil {
			return adapterErr(err)
		}

		launchDone <- nil // keep the result for configure

		select {
		case <-s.initialized:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("wait for the adapter to initialize: %w", ctx.Err())
		}
	case <-s.client.Done():
		return api.NewError(api.CodeAdapterFailed, "the debug adapter exited during start-up", "see 'eyedbg daemon logs'")
	case <-ctx.Done():
		return fmt.Errorf("wait for the adapter to initialize: %w", ctx.Err())
	}
}

// configurationPhase sends breakpoints and exception filters, then
// configurationDone.
func (s *Session) configurationPhase(ctx context.Context, bps []api.BreakpointSpec) error {
	s.mu.Lock()
	for _, b := range bps {
		s.addBreakpointLocked(b)
	}

	files := make([]string, 0, len(s.bps))
	for file := range s.bps {
		files = append(files, file)
	}
	s.mu.Unlock()

	for _, file := range files {
		if err := s.syncBreakpoints(ctx, file); err != nil {
			return err
		}
	}

	if _, err := s.client.Do(ctx, &godap.SetExceptionBreakpointsRequest{
		Request:   godap.Request{Command: "setExceptionBreakpoints"},
		Arguments: godap.SetExceptionBreakpointsArguments{Filters: []string{}},
	}); err != nil {
		return adapterErr(err)
	}

	if s.caps.SupportsConfigurationDoneRequest {
		if _, err := s.client.Do(ctx, &godap.ConfigurationDoneRequest{Request: godap.Request{Command: "configurationDone"}}); err != nil {
			return adapterErr(err)
		}
	}

	return nil
}

// onEvent runs on the DAP client's read goroutine.
func (s *Session) onEvent(ev godap.EventMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch e := ev.(type) {
	case *godap.InitializedEvent:
		s.initOnce.Do(func() { close(s.initialized) })
	case *godap.StoppedEvent:
		s.stops++
		s.stop = api.StopInfo{Reason: e.Body.Reason, ThreadID: e.Body.ThreadId, Description: e.Body.Description, Text: e.Body.Text}
		s.setStateLocked(api.StateStopped)
	case *godap.ContinuedEvent:
		if s.state == api.StateStopped {
			s.setStateLocked(api.StateRunning)
		}
	case *godap.ExitedEvent:
		code := e.Body.ExitCode
		s.exitCode = &code
		s.bump()
	case *godap.TerminatedEvent:
		s.endLocked("the program terminated")
	case *godap.ProcessEvent:
		s.pid = e.Body.SystemProcessId
	case *godap.OutputEvent:
		s.appendOutputLocked(e.Body.Category, e.Body.Output)
	case *godap.BreakpointEvent:
		s.updateBreakpointLocked(e.Body.Breakpoint)
	default:
		// module, thread, capabilities, ... carry nothing a client needs yet.
	}
}

// watchAdapter marks the session exited when the adapter goes away.
func (s *Session) watchAdapter() {
	<-s.client.Done()

	s.mu.Lock()
	s.endLocked("the debug adapter exited")
	s.mu.Unlock()

	_ = s.cmd.Wait()
}

// endLocked moves the session to exited (once) and tears down the adapter.
func (s *Session) endLocked(reason string) {
	if s.state == api.StateExited {
		return
	}

	s.endReason = reason
	s.setStateLocked(api.StateExited)

	go s.shutdownAdapter(s.life, false)
}

// shutdownAdapter disconnects from the adapter (asking it to kill the
// debuggee if terminate is true) and kills the adapter if it lingers.
func (s *Session) shutdownAdapter(ctx context.Context, terminate bool) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()

		return
	}

	s.ended = true
	s.mu.Unlock()

	// Finish even if the caller's request is canceled: a half-torn-down
	// adapter would leak the debuggee.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	_, _ = s.client.Do(ctx, &godap.DisconnectRequest{
		Request:   godap.Request{Command: "disconnect"},
		Arguments: &godap.DisconnectArguments{TerminateDebuggee: terminate},
	})

	select {
	case <-s.client.Done():
	case <-ctx.Done():
	}

	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// Terminate ends the session: the debuggee is killed and the adapter exits.
func (s *Session) Terminate(ctx context.Context) {
	s.mu.Lock()
	running := s.state != api.StateExited
	s.mu.Unlock()

	s.shutdownAdapter(ctx, running)

	s.mu.Lock()
	s.endLocked("stopped by a client")
	s.mu.Unlock()
}

func (s *Session) setStateLocked(st api.SessionState) {
	s.state = st
	s.bump()
}

// bump wakes every waiter; call with mu held.
func (s *Session) bump() {
	close(s.changed)
	s.changed = make(chan struct{})
}

func (s *Session) appendOutputLocked(category, text string) {
	if category == "telemetry" || text == "" {
		return
	}

	if category == "" {
		category = "console"
	}

	s.outSeq++
	s.output = append(s.output, api.OutputLine{Seq: s.outSeq, Category: category, Text: text})

	if len(s.output) > maxOutputLines {
		s.output = s.output[len(s.output)-maxOutputLines:]
	}
}

// Info returns the session's summary.
func (s *Session) Info() api.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := api.SessionInfo{
		ID:        s.ID,
		Lang:      s.Lang,
		Program:   s.Program,
		State:     s.state,
		PID:       s.pid,
		CreatedAt: s.CreatedAt,
		ExitCode:  s.exitCode,
	}

	if s.state == api.StateStopped {
		stop := s.stop
		info.Stop = &stop
	}

	if s.state == api.StateExited {
		info.EndReason = s.endReason
	}

	return info
}

// Snapshot returns the session state and the output since the program last
// resumed; when stopped, also the stop location and what dump asks for.
// Parts the adapter fails to deliver are left out.
func (s *Session) Snapshot(ctx context.Context, dump api.DumpSpec) api.Snapshot {
	snap := api.Snapshot{Session: s.Info()}
	snap.Output, snap.OutputOmitted = s.outputSinceResume()

	if snap.Session.State != api.StateStopped || snap.Session.Stop == nil {
		return snap
	}

	levels := 1
	if dump.Wants(api.DumpStack) {
		levels = dumpFrames
	}

	frames, err := s.stack(ctx, snap.Session.Stop.ThreadID, levels)
	if err == nil && len(frames) > 0 {
		f := frames[0].Frame
		snap.Frame = &f
		snap.Source = readSource(f.File, f.Line, 2)

		if dump.Wants(api.DumpStack) {
			for _, fr := range frames {
				snap.Stack = append(snap.Stack, fr.Frame)
			}
		}
	}

	if !dump.Wants(api.DumpLocals) && !dump.Wants(api.DumpChanged) {
		return snap
	}

	cur, prev, err := s.capture(ctx)
	if err != nil {
		return snap
	}

	fitter := present.NewFitter(dump.Budget)

	if dump.Wants(api.DumpChanged) {
		ch := changes(cur, prev)
		ch.Vars, ch.More, ch.Truncated = fitter.Fit(ch.Vars)
		snap.Changes = &ch
	}

	if dump.Wants(api.DumpLocals) {
		sc := api.Scope{Name: "Locals"}
		sc.Vars, sc.More, sc.Truncated = fitter.Fit(cur.locals)
		sc.More += cur.more
		snap.Locals = &sc
	}

	return snap
}

// changes diffs cur's locals against prev's when both are in the same
// function; otherwise every local is new.
func changes(cur, prev *stopCapture) api.Changes {
	if prev == nil || prev.frame.Name != cur.frame.Name || prev.frame.File != cur.frame.File {
		return api.Changes{NewFrame: true, Vars: present.Diff(nil, cur.locals)}
	}

	return api.Changes{Vars: present.Diff(prev.locals, cur.locals)}
}

// capture returns the capture of the current stop, fetching it if needed,
// and the one before it.
func (s *Session) capture(ctx context.Context) (cur, prev *stopCapture, err error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()

	s.mu.Lock()
	state, stops, thread := s.state, s.stops, s.stop.ThreadID

	if s.cur != nil && s.cur.stops == stops {
		cur, prev = s.cur, s.prev
		s.mu.Unlock()

		return cur, prev, nil
	}
	s.mu.Unlock()

	if state != api.StateStopped {
		return nil, nil, stateError(s.ID, state, "reading locals needs a stopped program")
	}

	frames, err := s.stack(ctx, thread, 1)
	if err != nil {
		return nil, nil, err
	}

	c := &stopCapture{stops: stops}

	if len(frames) > 0 {
		c.frame = frames[0].Frame

		if c.locals, c.more, err = s.frameLocals(ctx, frames[0].id); err != nil {
			return nil, nil, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stops != stops { // resumed meanwhile: don't record stale values
		return c, s.cur, nil
	}

	s.prev, s.cur = s.cur, c

	return s.cur, s.prev, nil
}

// outputSinceResume returns the last output chunks printed since the
// program last resumed and how many older ones were left out.
func (s *Session) outputSinceResume() (lines []api.OutputLine, omitted int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, l := range s.output {
		if l.Seq > s.resumedAt {
			lines = append(lines, l)
		}
	}

	if len(lines) > snapshotOutput {
		return lines[len(lines)-snapshotOutput:], len(lines) - snapshotOutput
	}

	return lines, 0
}

// waitChange waits until pred holds, ctx ends or the session changes state
// in a way pred accepts. It returns false on timeout.
func (s *Session) waitUntil(ctx context.Context, pred func() bool) bool {
	for {
		s.mu.Lock()
		ok := pred()
		ch := s.changed
		s.mu.Unlock()

		if ok {
			return true
		}

		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// Execution kinds for [Session.Resume].
const (
	ExecContinue = "continue"
	ExecNext     = "next"
	ExecStepIn   = "stepIn"
	ExecStepOut  = "stepOut"
	ExecPause    = "pause"
)

// Resume runs the debuggee (continue or a step), or pauses it, then waits up
// to wait for it to stop or exit. A timeout is not an error: the result says
// the program is still running. Temporary breakpoints left by a run-until
// that timed out are removed first.
func (s *Session) Resume(ctx context.Context, kind string, threadID int, wait time.Duration, dump api.DumpSpec) (api.Snapshot, error) {
	if kind != ExecPause {
		if err := s.removeTemporary(ctx); err != nil {
			return api.Snapshot{}, err
		}
	}

	return s.resume(ctx, kind, threadID, wait, dump)
}

func (s *Session) resume(ctx context.Context, kind string, threadID int, wait time.Duration, dump api.DumpSpec) (api.Snapshot, error) {
	if kind != ExecPause {
		// Record where it was, to diff the next stop against.
		_, _, _ = s.capture(ctx)
	}

	s.mu.Lock()
	state, before := s.state, s.stops

	if threadID == 0 {
		threadID = s.stop.ThreadID
	}
	s.mu.Unlock()

	if kind == ExecPause {
		if state != api.StateRunning {
			return api.Snapshot{}, stateError(s.ID, state, "pause needs a running program")
		}
	} else if state != api.StateStopped {
		return api.Snapshot{}, stateError(s.ID, state, kind+" needs a stopped program")
	}

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	if kind != ExecPause {
		s.mu.Lock()
		s.resumedAt = s.outSeq
		s.mu.Unlock()
	}

	if err := s.sendExec(reqCtx, kind, threadID); err != nil {
		return api.Snapshot{}, err
	}

	s.mu.Lock()
	if kind != ExecPause && s.state == api.StateStopped && s.stops == before {
		s.setStateLocked(api.StateRunning)
	}
	s.mu.Unlock()

	return s.Wait(ctx, before, wait, dump), nil
}

// Wait waits up to wait until the program stops for the (stopsSeen+1)th
// time or exits, and returns a snapshot. TimedOut is set if neither happened.
func (s *Session) Wait(ctx context.Context, stopsSeen int, wait time.Duration, dump api.DumpSpec) api.Snapshot {
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	ok := s.waitUntil(waitCtx, func() bool { return s.stops > stopsSeen || s.state == api.StateExited })
	if ok {
		s.settleOutput(ctx)
	}

	snap := s.Snapshot(ctx, dump)
	snap.TimedOut = !ok

	return snap
}

// settleOutput waits until no output has arrived for outputQuiet, at most
// outputSettleMax.
func (s *Session) settleOutput(ctx context.Context) {
	deadline := time.Now().Add(outputSettleMax)

	for {
		s.mu.Lock()
		seq := s.outSeq
		s.mu.Unlock()

		t := time.NewTimer(outputQuiet)
		select {
		case <-ctx.Done():
			t.Stop()

			return
		case <-t.C:
		}

		s.mu.Lock()
		quiet := s.outSeq == seq
		s.mu.Unlock()

		if quiet || time.Now().After(deadline) {
			return
		}
	}
}

// Stops returns how many times the program has stopped so far.
func (s *Session) Stops() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.stops
}

func (s *Session) sendExec(ctx context.Context, kind string, threadID int) error {
	var req godap.RequestMessage

	switch kind {
	case ExecContinue:
		req = &godap.ContinueRequest{Request: godap.Request{Command: "continue"}, Arguments: godap.ContinueArguments{ThreadId: threadID}}
	case ExecNext:
		req = &godap.NextRequest{Request: godap.Request{Command: "next"}, Arguments: godap.NextArguments{ThreadId: threadID}}
	case ExecStepIn:
		req = &godap.StepInRequest{Request: godap.Request{Command: "stepIn"}, Arguments: godap.StepInArguments{ThreadId: threadID}}
	case ExecStepOut:
		req = &godap.StepOutRequest{Request: godap.Request{Command: "stepOut"}, Arguments: godap.StepOutArguments{ThreadId: threadID}}
	case ExecPause:
		req = &godap.PauseRequest{Request: godap.Request{Command: "pause"}, Arguments: godap.PauseArguments{ThreadId: threadID}}
	default:
		return api.NewError(api.CodeInvalidRequest, "unknown execution kind "+kind, "")
	}

	if _, err := s.client.Do(ctx, req); err != nil {
		return adapterErr(err)
	}

	return nil
}

func (s *Session) requireStopped() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.state != api.StateStopped {
		return 0, stateError(s.ID, s.state, "inspecting needs a stopped program")
	}

	return s.stop.ThreadID, nil
}

// Stack returns up to levels frames of threadID (0: the stopped thread).
func (s *Session) Stack(ctx context.Context, threadID, levels int) ([]api.Frame, error) {
	stopped, err := s.requireStopped()
	if err != nil {
		return nil, err
	}

	if threadID == 0 {
		threadID = stopped
	}

	frames, err := s.stack(ctx, threadID, levels)
	if err != nil {
		return nil, err
	}

	out := make([]api.Frame, len(frames))
	for i, f := range frames {
		out[i] = f.Frame
	}

	return out, nil
}

// frame is an api.Frame plus the adapter's id for it.
type frame struct {
	api.Frame

	id int
}

func (s *Session) stack(ctx context.Context, threadID, levels int) ([]frame, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	resp, err := dap.Call[*godap.StackTraceResponse](ctx, s.client, &godap.StackTraceRequest{
		Request:   godap.Request{Command: "stackTrace"},
		Arguments: godap.StackTraceArguments{ThreadId: threadID, Levels: levels},
	})
	if err != nil {
		return nil, adapterErr(err)
	}

	frames := make([]frame, 0, len(resp.Body.StackFrames))
	for i, f := range resp.Body.StackFrames {
		fr := frame{Frame: api.Frame{Index: i, Name: f.Name, Line: f.Line, Column: f.Column}, id: f.Id}
		if f.Source != nil {
			fr.File = f.Source.Path
		}

		frames = append(frames, fr)
	}

	return frames, nil
}

// frameID maps a frame index of the stopped thread to the adapter's id.
func (s *Session) frameID(ctx context.Context, index int) (int, error) {
	stopped, err := s.requireStopped()
	if err != nil {
		return 0, err
	}

	frames, err := s.stack(ctx, stopped, index+1)
	if err != nil {
		return 0, err
	}

	if index < 0 || index >= len(frames) {
		return 0, api.NewError(api.CodeInvalidRequest,
			fmt.Sprintf("frame %d does not exist (the stack has %d)", index, len(frames)), "see 'eyedbg stack'")
	}

	return frames[index].id, nil
}

// Breakpoints returns every breakpoint, ordered by id.
func (s *Session) Breakpoints() []api.Breakpoint {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []api.Breakpoint
	for _, list := range s.bps {
		for _, b := range list {
			out = append(out, b.Breakpoint)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return out
}

func stateError(id string, state api.SessionState, what string) error {
	code := api.CodeNotStopped
	if state == api.StateStopped {
		code = api.CodeNotRunning
	}

	if state == api.StateExited {
		code = api.CodeSessionExited
	}

	return api.NewError(code, fmt.Sprintf("%s: session %s is %s", what, id, state),
		"see 'eyedbg status' for where it is")
}

func adapterErr(err error) error {
	if req, ok := errors.AsType[*dap.RequestError](err); ok {
		return api.NewError(api.CodeAdapterFailed, req.Error(), "")
	}

	if errors.Is(err, dap.ErrClosed) {
		return api.NewError(api.CodeSessionExited, "the debug adapter has exited", "see 'eyedbg status' and 'eyedbg daemon logs'")
	}

	return err
}
