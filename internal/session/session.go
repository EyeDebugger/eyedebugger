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
	"net"
	"os/exec"
	"sort"
	"sync"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/present"
)

// Timeouts for talking to an adapter, and size limits.
const (
	requestTimeout    = 30 * time.Second
	shutdownTimeout   = 3 * time.Second
	maxSourceFileSize = 4 << 20
	// maxOutputChunk cuts one output event's text.
	maxOutputChunk = 64 << 10
	// snapshotOutput is how many output chunks a snapshot carries, in at
	// most snapshotOutputBytes.
	snapshotOutput      = 20
	snapshotOutputBytes = 16 << 10
	// dumpFrames is how many frames a stack dump carries.
	dumpFrames = 10
	// Adapters may send a program's last output after the stopped event: a
	// stop is reported once output has been quiet for outputQuiet, waiting
	// at most outputSettleMax.
	outputQuiet     = 50 * time.Millisecond
	outputSettleMax = 250 * time.Millisecond
)

// Session is one debuggee driven through one adapter process, shared by
// any number of clients.
//
// Lock order: execMu, syncMu, captureMu, mu, then the event log's and the
// recorder's own locks. The DAP read goroutine (onEvent) takes only mu and
// below, never execMu or syncMu, and nothing holds mu while it waits on the
// adapter.
type Session struct {
	ID        string
	Lang      string
	Program   string
	CreatedAt time.Time
	// mode is api.ModeAttach, api.ModeTest or "" (launched); immutable.
	mode string
	// run is a test session's runner (nil otherwise); the pointer is
	// immutable.
	run *testRun

	cmd    *exec.Cmd
	client *dap.Client
	// conn is the adapter's connection on the connect transport (nil on
	// stdio), and exited is closed once its process has been waited for
	// (nil on stdio: watchAdapter waits for it). All three are set with
	// client, under mu, and never change after.
	conn    net.Conn
	exited  <-chan struct{}
	logger  *slog.Logger
	starter api.Client
	// life is the manager's context; shutdown work started by adapter
	// events (not by a request) runs under it.
	life context.Context //nolint:containedctx // Outlives requests by design, like the adapter it tears down.

	initialized chan struct{}
	initOnce    sync.Once

	// execMu serializes execution-changing requests (docs/DESIGN.md §3):
	// each checks the state, then the lease, then sends, all under it.
	execMu sync.Mutex
	// syncMu serializes setBreakpoints round trips (snapshot, request,
	// apply), so the adapter always ends up with the newest list.
	syncMu sync.Mutex

	log *eventLog
	// rec records the session's control events; nil when not recording.
	// Both are set before the session is shared.
	rec     *recorder
	recPath string

	mu        sync.Mutex
	state     api.SessionState
	stops     int // number of stopped events so far
	stop      api.StopInfo
	pid       int
	exitCode  *int
	endReason string
	bps       map[string][]*breakpoint // by absolute file path
	nextBP    int
	lease     lease
	clients   []api.ClientInfo // by first seen
	resumedAt int              // log seq when the program last resumed
	// presence counts each client's open editor connections (presence.go);
	// connGen numbers connections, session-wide.
	presence map[string]*presence
	connGen  uint64
	// lastOutput is the seq of the newest output event.
	lastOutput int
	// execInFlight is set while an execution request is sent: the
	// adapter's continued event for it is not logged.
	execInFlight bool
	// stopping is the client that ended the session, and stopReason why;
	// the ended event says so rather than what the adapter reported.
	stopping, stopReason string
	changed              chan struct{} // closed and replaced on every state change
	ended                bool          // adapter torn down
	// caps are the adapter's capabilities, known once it answered
	// initialize.
	caps      godap.Capabilities
	capsKnown bool
	// excFilters maps exception modes to the adapter's filters (the
	// driver's).
	excFilters map[api.ExceptionMode][]string
	// excModes are the clients' exception modes other than none, in the
	// order they were first set.
	excModes []api.ClientExceptionMode
	// excInfo describes the exception of stop number excInfoStops.
	excInfo      *api.ExceptionInfo
	excInfoStops int
	// sideEffects is the driver's check of eval expressions (nil: none).
	sideEffects func(expr string) (string, bool)
	// adapter names the adapter; pauseUnsupported is the hint of a refused
	// pause ("" allows pause); setByEval lets set assign by evaluate;
	// exitCodeUnknown is why the adapter's exit codes can't be trusted ("":
	// they can) (all the driver's Launch settings; a test run's are set
	// once its host is known).
	adapter          string
	pauseUnsupported string
	setByEval        bool
	exitCodeUnknown  string
	// programExited is set once the program has exited under an adapter
	// whose exit codes aren't trusted (exitCodeUnknown != ""): exitCode
	// stays nil, but endReasonLocked still says the program is done.
	programExited bool
	// curStale means cur's values may have changed (set, eval with side
	// effects): the next capture refetches them.
	curStale bool
	// stopGen counts the adapter's stopped and continued events: a stop
	// the stop filter holds is still pending while it is unchanged.
	stopGen int
	// stepping is set while a step is the last execution request sent and
	// the program has not stopped since.
	stepping bool
	// pauseRequested is set from sending a pause until the next applied
	// stop: a pause that reached the adapter while the stop filter held a
	// stop was a no-op there (netcoredbg: "already stopped"), so the filter
	// publishes that stop instead of continuing.
	pauseRequested bool

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
	// source is the path the adapter gave for the verified breakpoint (to
	// attribute stops); empty until then.
	source string
	// hit and log are the parsed HitCondition and LogMessage (emulate.go).
	hit *hitCondition
	log []logPart
}

// newSession returns a session started by starter, who holds its lease;
// mode is how it gets its program (see api.SessionInfo.Mode).
func newSession(life context.Context, id, lang, mode string, launch Launch, logger *slog.Logger, starter api.Client, policy api.LeasePolicy) *Session {
	now := time.Now()

	return &Session{
		life:        life,
		ID:          id,
		Lang:        lang,
		mode:        mode,
		pid:         launch.PID,
		Program:     launch.Program,
		CreatedAt:   now,
		logger:      logger.With(slog.String("session", id)),
		starter:     starter,
		initialized: make(chan struct{}),
		log:         newEventLog(maxEvents, maxEventBytes, time.Now),
		state:       api.StateStarting,
		bps:         make(map[string][]*breakpoint),
		lease:       lease{policy: policy, holder: starter, since: now},
		clients:     []api.ClientInfo{{Client: starter, FirstSeen: now, LastSeen: now}},
		changed:     make(chan struct{}),
		excFilters:  launch.ExceptionFilters,
		sideEffects: launch.SideEffects,

		adapter:          launch.AdapterName,
		pauseUnsupported: launch.PauseUnsupported,
		setByEval:        launch.SetByEval,
		exitCodeUnknown:  launch.ExitCodeUnknown,
	}
}

// attachRecorder records the session's control events to w from now on.
// Call it before the session is shared.
func (s *Session) attachRecorder(w io.WriteCloser, path string) {
	s.rec = newRecorder(w, maxRecording, s.logger)
	s.recPath = path
	s.log.setSink(s.rec.record)
}

// logStarted appends the started event, the log's first; its action is the
// session's mode (empty for a launch).
func (s *Session) logStarted() {
	s.mu.Lock()
	defer s.mu.Unlock()

	info := s.lease.info()
	s.log.append(api.Event{Kind: api.EventStarted, Client: s.starter.ID, Program: s.Program, Lease: &info, Action: s.mode})
}

// startAdapter launches the adapter process and connects a DAP client to it,
// on its stdio or, with launch.SocketArgs, on the socket it dials in to
// within connectTimeout.
func (s *Session) startAdapter(ctx context.Context, launch Launch, stderr io.Writer, connectTimeout time.Duration) error {
	if launch.SocketArgs != nil {
		return s.startSocketAdapter(ctx, launch, stderr, connectTimeout)
	}

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

	// Under mu: a test run's session is shared before its adapter starts.
	s.mu.Lock()
	s.cmd = cmd
	s.client = dap.NewClient(stdout, stdin, dap.Handlers{Event: s.onEvent})
	s.mu.Unlock()

	go s.watchAdapter()

	return nil
}

// configure runs the DAP start-up sequence: initialize, launch (or
// attach), breakpoints (owned by the starter), exception filters,
// configurationDone.
func (s *Session) configure(ctx context.Context, launch Launch, bps []api.BreakpointSpec) error {
	if err := s.initialize(ctx, launch.AdapterID); err != nil {
		return err
	}

	args, err := json.Marshal(launch.Arguments)
	if err != nil {
		return fmt.Errorf("encode launch arguments: %w", err)
	}

	var req godap.RequestMessage = &godap.LaunchRequest{Request: godap.Request{Command: RequestLaunch}, Arguments: args}
	if launch.Request == RequestAttach {
		req = &godap.AttachRequest{Request: godap.Request{Command: RequestAttach}, Arguments: args}
	}

	// Some adapters answer launch (and attach) only after
	// configurationDone, so it is sent without waiting.
	launchDone := make(chan error, 1)

	go func() {
		_, err := s.client.Do(ctx, req)
		launchDone <- err
	}()

	if err := s.waitInitialized(ctx, launchDone); err != nil {
		return startErr(launch, err)
	}

	if err := s.configurationPhase(ctx, bps); err != nil {
		return err
	}

	if err := s.configurationDone(ctx); err != nil {
		return startErr(launch, err)
	}

	select {
	case err := <-launchDone:
		if err != nil {
			return startErr(launch, adapterErr(err))
		}
	case <-ctx.Done():
		return fmt.Errorf("wait for %s: %w", req.GetRequest().Command, ctx.Err())
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

	s.setCaps(resp.Body)

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

	added := make([]*breakpoint, 0, len(bps))

	for _, spec := range bps {
		if err := s.checkBreakpointCapsLocked(spec); err != nil {
			s.mu.Unlock()

			return err
		}

		if b, isNew, _ := s.addBreakpointLocked(s.starter.ID, spec); isNew {
			added = append(added, b)
		}
	}

	keys := make([]string, 0, len(s.bps))
	for key := range s.bps {
		keys = append(keys, key)
	}

	s.mu.Unlock()

	for _, key := range keys {
		if err := s.syncKey(ctx, key); err != nil {
			return err
		}
	}

	s.mu.Lock()
	for _, b := range added {
		s.logBreakpointLocked("added", s.starter.ID, b)
	}
	s.mu.Unlock()

	return s.configureExceptions(ctx)
}

// configurationDone ends the configuration, if the adapter wants it. An
// attach fails here with netcoredbg, which attaches only now.
func (s *Session) configurationDone(ctx context.Context) error {
	s.mu.Lock()
	caps, _ := s.capsLocked()
	s.mu.Unlock()

	if !caps.SupportsConfigurationDoneRequest {
		return nil
	}

	if _, err := s.client.Do(ctx, &godap.ConfigurationDoneRequest{Request: godap.Request{Command: "configurationDone"}}); err != nil {
		return adapterErr(err)
	}

	return nil
}

// startErr turns an adapter's refusal of an attach into ATTACH_FAILED with
// the driver's hint.
func startErr(launch Launch, err error) error {
	if launch.Request != RequestAttach || api.CodeOf(err) != api.CodeAdapterFailed {
		return err
	}

	return api.NewError(api.CodeAttachFailed, fmt.Sprintf("attach to pid %d failed: %s", launch.PID, err.Error()), launch.AttachHint)
}

// onEvent runs on the DAP client's read goroutine.
func (s *Session) onEvent(ev godap.EventMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch e := ev.(type) {
	case *godap.InitializedEvent:
		s.initOnce.Do(func() { close(s.initialized) })
	case *godap.StoppedEvent:
		s.onStoppedLocked(e)
	case *godap.ContinuedEvent:
		s.onContinuedLocked(e)
	case *godap.ExitedEvent:
		s.onExitedLocked(e)
	case *godap.ThreadEvent:
		s.log.append(api.Event{Kind: api.EventThread, Reason: e.Body.Reason, ThreadID: e.Body.ThreadId})
	case *godap.TerminatedEvent:
		// A test host ending is not the session's end: its runner's is.
		if s.run != nil {
			go s.shutdownAdapter(s.life, true)
		} else {
			s.endLocked(s.endReasonLocked("the program terminated"))
		}
	case *godap.ProcessEvent:
		s.pid = e.Body.SystemProcessId
	case *godap.OutputEvent:
		s.appendOutputLocked(e.Body.Category, e.Body.Output)
	case *godap.BreakpointEvent:
		s.updateBreakpointLocked(e.Body.Breakpoint)
	default:
		// module, capabilities, ... carry nothing a client needs yet.
	}
}

// onStoppedLocked applies a stop, or hands a breakpoint stop that may be
// one to count or log and pass to the stop filter (off this goroutine: it
// waits for the adapter's answers).
func (s *Session) onStoppedLocked(e *godap.StoppedEvent) {
	stop := api.StopInfo{
		Reason: e.Body.Reason, ThreadID: e.Body.ThreadId, Description: e.Body.Description, Text: e.Body.Text,
		AllThreadsStopped: e.Body.AllThreadsStopped,
	}
	s.stopGen++

	// lldb-dap pauses with SIGSTOP on Linux and reports the stop as a
	// signal (reason exception): with a pause outstanding, it is the pause.
	if s.pauseRequested && pauseSignal(stop) {
		stop.Reason = reasonPause
	}

	if stop.Reason == reasonBreakpoint && s.emulatingLocked() {
		go s.filterStop(s.stopGen, stop)

		return
	}

	s.applyStopLocked(stop)
}

func (s *Session) onContinuedLocked(e *godap.ContinuedEvent) {
	s.stopGen++

	if s.state == api.StateStopped {
		s.setStateLocked(api.StateRunning)
	}

	// Adapters (netcoredbg) also send it for our own requests, before
	// answering them: only an adapter-initiated resume is news.
	if !s.execInFlight {
		s.log.append(api.Event{Kind: api.EventContinued, ThreadID: e.Body.ThreadId})
	}
}

// onExitedLocked records the program's exit code; a test host's is only
// noted (the session's is its runner's). Under an adapter marked
// exitCodeUnknown, no exit code is trusted: it is dropped in both cases,
// and endReasonLocked explains why once the session ends.
func (s *Session) onExitedLocked(e *godap.ExitedEvent) {
	if s.run != nil {
		if s.exitCodeUnknown != "" {
			s.appendOutputLocked("console", "eyedbg: the test host exited\n")
		} else {
			s.hostExitedLocked(e.Body.ExitCode)
		}

		return
	}

	if s.exitCodeUnknown != "" {
		s.programExited = true
		s.bump()
		s.log.append(api.Event{Kind: api.EventExited})

		return
	}

	code, logged := e.Body.ExitCode, e.Body.ExitCode
	s.exitCode = &code
	s.bump()
	s.log.append(api.Event{Kind: api.EventExited, ExitCode: &logged})
}

// watchAdapter marks the session exited when the adapter goes away, and
// ends the recording.
func (s *Session) watchAdapter() {
	<-s.client.Done()

	// A test session with a runner ends with it (watchRunner).
	if s.run == nil {
		s.mu.Lock()
		s.endLocked(s.endReasonLocked("the debug adapter exited"))
		s.mu.Unlock()
	}

	// On stdio, Wait only now: it closes the pipes the DAP stream was read
	// from. On a socket, another goroutine owns Wait.
	if s.exited != nil {
		<-s.exited
	} else {
		_ = s.cmd.Wait()
	}

	if s.run == nil {
		s.closeRecording()
	}
}

// endReasonLocked is def, unless s is a launched test run (mode test, no
// runner) that reported an exit code: then it names it, as a runner's test
// run does in finishRun. When the program exited under an adapter whose
// exit code isn't trusted (exitCodeUnknown), it says so instead, with
// finishRun's "read the test output" hint for a launched test run.
func (s *Session) endReasonLocked(def string) string {
	if s.mode == api.ModeTest && s.run == nil {
		if s.exitCode != nil {
			return fmt.Sprintf("the test run finished: the test app exited with code %d", *s.exitCode)
		}

		if s.programExited {
			return fmt.Sprintf("the test run finished; the test app's exit code is unknown (%s): read the test output for the result",
				s.exitCodeUnknown)
		}
	}

	if s.programExited {
		return def + "; its exit code is unknown (" + s.exitCodeUnknown + ")"
	}

	return def
}

// closeRecording ends the recording, if any.
func (s *Session) closeRecording() {
	if s.rec != nil {
		s.rec.close()
	}
}

// endLocked moves the session to exited (once), logs the ended event and
// tears down the adapter. A client's stop overrides the adapter's reason.
func (s *Session) endLocked(reason string) {
	if s.state == api.StateExited {
		return
	}

	if s.stopReason != "" {
		reason = s.stopReason
	}

	s.endReason = reason
	s.setStateLocked(api.StateExited)
	s.log.append(api.Event{Kind: api.EventEnded, Reason: reason, Client: s.stopping})

	// A test host goes with its run.
	if s.client != nil {
		go s.shutdownAdapter(s.life, s.mode == api.ModeTest)
	}
}

// shutdownAdapter disconnects from the adapter (asking it to kill the
// debuggee if terminate is true, else to leave an attached one running)
// and kills the adapter if it lingers. Without an adapter (a test run
// still waiting for its host) there is nothing to do.
func (s *Session) shutdownAdapter(ctx context.Context, terminate bool) {
	s.mu.Lock()
	if s.ended || s.client == nil {
		s.mu.Unlock()

		return
	}

	s.ended = true
	s.mu.Unlock()

	// Finish even if the caller's request is canceled: a half-torn-down
	// adapter would leak the debuggee.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()

	_, _ = s.client.Do(ctx, disconnect(terminate, s.mode != ""))

	select {
	case <-s.client.Done():
	case <-ctx.Done():
	}

	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}

	// On a socket, the stream may outlive the adapter (a child that
	// inherited it): closing it ends the DAP client.
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// disconnectRequest is godap's DisconnectRequest, able to send
// terminateDebuggee false: for an attached program that means detach, and
// leaving it out lets the adapter choose (netcoredbg detaches then too, but
// kills a launched one).
type disconnectRequest struct {
	godap.Request

	Arguments disconnectArguments `json:"arguments"`
}

type disconnectArguments struct {
	TerminateDebuggee *bool `json:"terminateDebuggee,omitempty"`
}

// disconnect builds the request; explicit sends terminateDebuggee even when
// false.
func disconnect(terminate, explicit bool) *disconnectRequest {
	r := &disconnectRequest{Request: godap.Request{Command: "disconnect"}}
	if terminate || explicit {
		r.Arguments.TerminateDebuggee = &terminate
	}

	return r
}

// Terminate ends the session for client c: the debuggee is killed (an
// attached one is detached from and keeps running) and the adapter exits.
// While the program is live this is an execution request: it needs the
// lease (LEASE_HELD otherwise).
func (s *Session) Terminate(ctx context.Context, c api.Client) error {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	s.mu.Lock()
	if s.state != api.StateExited {
		if err := s.acquireLocked(c); err != nil {
			s.mu.Unlock()

			return err
		}
	}
	s.mu.Unlock()

	s.terminate(ctx, c.ID, s.endedBy(c.ID))

	return nil
}

// endedBy is the ended event's reason when client by (empty: the daemon)
// ends the session.
func (s *Session) endedBy(by string) string {
	who := by
	if who == "" {
		who = "the daemon"
	}

	if s.mode == api.ModeAttach {
		return "detached by " + who + " (the program keeps running)"
	}

	return "stopped by " + who
}

// terminate ends the session without checking the lease: by names the
// client (empty: the daemon), reason goes to the ended event. An attached
// program is detached from, never killed.
func (s *Session) terminate(ctx context.Context, by, reason string) {
	s.mu.Lock()
	running := s.state != api.StateExited

	if running && s.stopReason == "" {
		s.stopping, s.stopReason = by, reason
	}
	s.mu.Unlock()

	s.killRunner(ctx)
	s.shutdownAdapter(ctx, running && s.mode != api.ModeAttach)

	s.mu.Lock()
	s.endLocked(reason)
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

// appendOutputLocked logs a chunk of output, cut to maxOutputChunk.
func (s *Session) appendOutputLocked(category, text string) {
	if category == "telemetry" || text == "" {
		return
	}

	if category == "" {
		category = "console"
	}

	cut := present.CutText(text, maxOutputChunk, false)
	e := s.log.append(api.Event{Kind: api.EventOutput, Category: category, Text: cut, Truncated: len(cut) < len(text)})
	s.lastOutput = e.Seq
}

// Info returns the session's summary.
func (s *Session) Info() api.SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.infoLocked()
}

func (s *Session) infoLocked() api.SessionInfo {
	leaseInfo := s.lease.info()

	info := api.SessionInfo{
		ID:        s.ID,
		Lang:      s.Lang,
		Program:   s.Program,
		State:     s.state,
		PID:       s.pid,
		CreatedAt: s.CreatedAt,
		ExitCode:  s.exitCode,
		Lease:     &leaseInfo,
		Clients:   s.clientsLocked(),
		Recording: s.recPath,
		Mode:      s.mode,
		Adapter:   s.adapter,
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

	if snap.Session.Stop.Reason == reasonException {
		snap.Exception = s.exceptionInfo(ctx, s.Stops(), snap.Session.Stop.ThreadID)
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

	if s.cur != nil && s.cur.stops == stops && !s.curStale {
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

	// A refetch of the same stop (after a change) keeps the previous stop.
	if s.cur == nil || s.cur.stops != stops {
		s.prev = s.cur
	}

	s.cur, s.curStale = c, false

	return s.cur, s.prev, nil
}

// outputSinceResume returns the last output chunks printed since the
// program last resumed (at most snapshotOutput, in snapshotOutputBytes) and
// how many older ones were left out.
func (s *Session) outputSinceResume() (lines []api.OutputLine, omitted int) {
	s.mu.Lock()
	since := s.resumedAt
	s.mu.Unlock()

	lines = s.log.outputSince(since)

	if len(lines) > snapshotOutput {
		omitted = len(lines) - snapshotOutput
		lines = lines[omitted:]
	}

	lines, cut := present.CapOutput(lines, snapshotOutputBytes, true)

	return lines, omitted + cut
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

// Resume runs the debuggee (continue or a step), or pauses it, for client
// c, then waits up to wait for it to stop or exit. A timeout is not an
// error: the result says the program is still running. Temporary
// breakpoints left by a run-until that timed out are removed first.
//
// Only the sending is serialized with other clients' execution requests:
// while this one waits, another client may pause the program.
func (s *Session) Resume(ctx context.Context, c api.Client, kind string, threadID int, wait time.Duration, dump api.DumpSpec) (api.Snapshot, error) {
	before, err := s.Exec(ctx, c, kind, threadID)
	if err != nil {
		return api.Snapshot{}, err
	}

	return s.Wait(ctx, before, wait, dump), nil
}

// Exec sends an execution request (continue, a step, or pause) for client
// c and returns once the adapter answered it, without waiting for the
// program to stop: the stop arrives as an event. It returns how many times
// the program had stopped when the request was accepted (for [Session.Wait]).
func (s *Session) Exec(ctx context.Context, c api.Client, kind string, threadID int) (int, error) {
	switch kind {
	case ExecContinue, ExecNext, ExecStepIn, ExecStepOut, ExecPause:
	default:
		return 0, api.NewError(api.CodeInvalidRequest, "unknown execution kind "+kind,
			"use continue, next, stepIn, stepOut or pause")
	}

	x, err := s.execute(ctx, execRequest{client: c, kind: kind, thread: threadID})
	if err != nil {
		return 0, err
	}

	return x.before, nil
}

// ExecRunUntil is run-until's kind in exec events: a continue to a target.
const ExecRunUntil = "runUntil"

// execRequest is one execution-changing request.
type execRequest struct {
	client api.Client
	kind   string // an Exec* kind, ExecRunUntil, ExecEval or ExecSet
	thread int
	target *api.BreakpointSpec // run-until's location
	text   string              // eval's expression, set's variable
}

// execution is what an accepted request leaves for its caller to wait on.
type execution struct {
	before int                // stops seen when it was accepted
	target api.BreakpointSpec // where run-until's breakpoint landed
	temp   *breakpoint        // run-until's temporary breakpoint, if it placed one
}

// execute runs an execution request under execMu, in the order: state
// check, lease, then side effects (the exec event, removing run-until's
// leftovers, placing its target, capturing the stop) and the send. A
// request refused by the state or the lease has no side effect.
func (s *Session) execute(ctx context.Context, r execRequest) (execution, error) {
	s.execMu.Lock()
	defer s.execMu.Unlock()

	x, thread, err := s.admit(r)
	if err != nil {
		return execution{}, err
	}

	if r.kind != ExecPause {
		if err := s.removeTemporary(ctx, r.client.ID); err != nil {
			return execution{}, err
		}
	}

	if r.target != nil {
		if x.target, x.temp, err = s.placeTarget(ctx, r.client, *r.target); err != nil {
			return execution{}, err
		}
	}

	if r.kind != ExecPause {
		// Record where it was, to diff the next stop against.
		_, _, _ = s.capture(ctx)
	}

	if err := s.send(ctx, r.kind, thread, x.before); err != nil {
		if x.temp != nil {
			_, _, _ = s.removeBreakpoints(ctx, r.client.ID, stillTemporary(x.temp))
		}

		return execution{}, err
	}

	return x, nil
}

// admit checks that the adapter can do it, the state and the lease (taking
// it when the policy allows) and logs the exec event. It returns the thread
// to act on.
func (s *Session) admit(r execRequest) (execution, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.kind == ExecPause && s.pauseUnsupported != "" {
		return execution{}, 0, api.NewError(api.CodeUnsupported,
			"the "+s.Lang+" debug adapter ("+s.adapter+") can't pause a running program: it stops it without reporting where",
			s.pauseUnsupported)
	}

	switch {
	case r.kind == ExecPause && s.state != api.StateRunning:
		return execution{}, 0, stateError(s.ID, s.state, "pause needs a running program")
	case r.kind == ExecRunUntil && s.state != api.StateStopped:
		return execution{}, 0, stateError(s.ID, s.state, "run-until needs a stopped program")
	case r.kind != ExecPause && s.state != api.StateStopped:
		return execution{}, 0, stateError(s.ID, s.state, r.kind+" needs a stopped program")
	}

	if err := s.acquireLocked(r.client); err != nil {
		return execution{}, 0, err
	}

	thread := r.thread
	if thread == 0 {
		thread = s.stop.ThreadID
	}

	s.log.append(api.Event{Kind: api.EventExec, Client: r.client.ID, Action: r.kind, ThreadID: r.thread, Text: r.text})

	return execution{before: s.stops}, thread, nil
}

// send sends the request to the adapter; a resume marks the program
// running unless it already stopped again.
func (s *Session) send(ctx context.Context, kind string, thread, before int) error {
	s.mu.Lock()
	if kind != ExecPause {
		s.resumedAt = s.log.latest()
		s.stepping = kind == ExecNext || kind == ExecStepIn || kind == ExecStepOut
		// A pause that reached an already stopped program got no stop to
		// clear it; it must not outlive this resume.
		s.pauseRequested = false
	} else {
		s.pauseRequested = true
	}

	s.execInFlight = true
	s.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	dapKind := kind
	if kind == ExecRunUntil {
		dapKind = ExecContinue
	}

	err := s.sendExec(reqCtx, dapKind, thread)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.execInFlight = false

	switch {
	case err == nil:
	case kind == ExecPause:
		s.pauseRequested = false
	default:
		s.stepping = false
	}

	if err == nil && kind != ExecPause && s.state == api.StateStopped && s.stops == before {
		s.setStateLocked(api.StateRunning)
	}

	return err
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
		seq := s.lastOutput
		s.mu.Unlock()

		t := time.NewTimer(outputQuiet)
		select {
		case <-ctx.Done():
			t.Stop()

			return
		case <-t.C:
		}

		s.mu.Lock()
		quiet := s.lastOutput == seq
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

// Breakpoints returns the breakpoints of owner (a client id; "" for every
// client's), ordered by id, with a note on those in files changed since the
// session started.
func (s *Session) Breakpoints(owner string) []api.Breakpoint {
	s.mu.Lock()

	var out []api.Breakpoint
	for _, list := range s.bps {
		for _, b := range list {
			if owner == "" || b.Owner == owner {
				out = append(out, b.Breakpoint)
			}
		}
	}
	s.mu.Unlock()

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })

	return s.annotate(out)
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
