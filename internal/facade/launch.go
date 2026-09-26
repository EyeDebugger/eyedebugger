// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Launcher makes [Serve] serve a launch connection (docs/adr/0019): one
// that joins no session, whose DAP launch starts one through Manager as
// the connection's client, with Defaults adding what 'eyedbg start' adds
// from the caller's environment.
type Launcher struct {
	Manager  *session.Manager
	Defaults api.FacadeLaunch
}

// stopTimeout bounds ending a launched session on the connection's behalf
// once the connection is gone (a launch stopped as it succeeded, or
// forgetting an exited session).
const stopTimeout = 10 * time.Second

// langDotnet is the dotnet driver's language: its launches take the
// caller's EYEDBG_DOTNET_ADAPTER when they name no adapter, as 'eyedbg
// start' does.
const langDotnet = "dotnet"

// launchState is a launch connection's launch.
//
// Goroutines: the reader starts the launch (started, cancel, then the
// launch goroutine); the launch goroutine runs session.Manager.Launch,
// binds the session from its Configure hook (connection.bind) and, once
// Launch returned, answers launch, stores launched and closes done;
// configurationDone is handled on the ordered worker, behind every earlier
// breakpoint and exception request, and releases the hook by closing
// configured.
type launchState struct {
	mgr      *session.Manager
	defaults api.FacadeLaunch

	// started is the reader's: a launch was started (at most one).
	started bool
	// cancel ends the launch; set by the reader before the launch
	// goroutine starts, so every goroutine the reader starts later sees it.
	cancel context.CancelFunc
	// done is closed when the launch goroutine finished: launch answered
	// and, on success, the join done.
	done chan struct{}
	// configured is closed once, by the editor's configurationDone.
	configured chan struct{}
	configOnce sync.Once
	// launched is the session the launch started, once Launch returned it
	// (and it wasn't stopped at once).
	launched atomic.Pointer[session.Session]
	// out is the launch's build output.
	out *buildOutput
	// unservable is the ordered worker's: the exception mode the last
	// setExceptionBreakpoints named that the adapter can't serve.
	unservable api.ExceptionMode
}

func newLaunchState(l *Launcher) *launchState {
	return &launchState{
		mgr: l.Manager, defaults: l.Defaults,
		done: make(chan struct{}), configured: make(chan struct{}),
	}
}

// finished reports whether the launch goroutine finished.
func (l *launchState) finished() bool {
	select {
	case <-l.done:
		return true
	default:
		return false
	}
}

// launchExceptionModes are the exception filters a launch connection's
// initialize offers: both, as the adapter isn't known yet (VS Code reads
// the filters from initialize only).
func launchExceptionModes() []api.ExceptionMode {
	return []api.ExceptionMode{api.ExceptionsAll, api.ExceptionsUncaught}
}

// startLaunch starts the launch: on the reader, it checks the arguments
// and hands Manager.Launch to the launch goroutine; the reader keeps
// reading. A refused launch leaves the connection as it was.
func (c *connection) startLaunch(ctx context.Context, r *godap.LaunchRequest) {
	l := c.launch

	if c.phase.load() != phaseInitialized || l.started {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "launch needs initialize first, once", ""), false)

		return
	}

	params, err := launchParams(r.Arguments, l.defaults)
	if err != nil {
		c.fail(ctx, r, err, false)

		return
	}

	lctx, cancel := context.WithCancel(ctx)
	l.started, l.cancel = true, cancel
	l.out = &buildOutput{c: c}
	c.phase.store(phaseLaunching)

	c.logger.InfoContext(ctx, "facade launch", slog.String("lang", params.Lang))

	c.wg.Go(func() {
		defer close(l.done)
		defer cancel()

		c.protect(ctx, func() { c.runLaunch(ctx, lctx, r, params) })
	})
}

// runLaunch is the launch goroutine: it starts the session, then answers
// launch and joins the session's event log as configurationDone does in an
// attach, or answers with the error. ctx is the connection's; lctx, the
// launch's, also ends with a terminate while it runs.
func (c *connection) runLaunch(ctx, lctx context.Context, r *godap.LaunchRequest, params api.StartParams) {
	l := c.launch

	s, err := l.mgr.Launch(lctx, c.client, params, session.LaunchHooks{Output: l.out, Configure: c.bind})
	l.out.finish()

	if err == nil && lctx.Err() != nil {
		// Stopped (terminate, disconnect) just as it succeeded: nothing
		// launched stays.
		c.stopAbandoned(ctx, s)
		err = lctx.Err()
	}

	c.gate.Lock()
	defer c.gate.Unlock()

	if err != nil {
		c.phase.store(phaseFailed)
		c.launchFailed(ctx, r, err, lctx.Err() != nil)

		return
	}

	l.launched.Store(s)

	info, seq := s.JoinPoint()
	c.phase.store(phaseConfigured)

	if c.view.leaving || !c.respond(ctx, r, &godap.LaunchResponse{}) {
		return
	}

	c.join(ctx, info, seq)
}

// launchFailed answers a launch that failed with its error (shown to the
// user), or one the client stopped (not shown); under the event gate, and
// not after the client disconnected.
func (c *connection) launchFailed(ctx context.Context, r *godap.LaunchRequest, err error, stopped bool) {
	if c.view.leaving || ctx.Err() != nil {
		c.logger.DebugContext(ctx, "facade launch abandoned: the connection is closing")

		return
	}

	if stopped {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "the launch was stopped", ""), false)

		return
	}

	if api.CodeOf(err) == "" {
		c.logger.WarnContext(ctx, "facade launch failed", slog.String("error_type", fmt.Sprintf("%T", err)))
		err = api.NewError(api.CodeInternal, "the launch failed: "+err.Error(), "see 'eyedbg daemon logs'")
	}

	c.fail(ctx, r, err, true)
}

// stopAbandoned ends and forgets a session launched for a client that
// stopped the launch meanwhile.
func (c *connection) stopAbandoned(ctx context.Context, s *session.Session) {
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	if err := c.stopLaunched(stopCtx, s); err != nil {
		c.logger.WarnContext(ctx, "facade launch: stop the session", slog.String("session", s.ID), slog.String("code", string(api.CodeOf(err))))
	}
}

// stopLaunched ends and forgets s (session.Manager.StopSession, under the
// lease); a session already gone is no error. It stops s itself, never by
// id: after the first stop forgot s, a later terminate, disconnect or
// close calls this again, and a newer session may have reused the id.
func (c *connection) stopLaunched(ctx context.Context, s *session.Session) error {
	if _, err := c.launch.mgr.StopSession(ctx, c.client, s); err != nil && api.CodeOf(err) != api.CodeNoSession {
		return err //nolint:wrapcheck // The session's error is the answer.
	}

	return nil
}

// bind is the launch's Configure hook, on the launch goroutine while the
// session is starting (its adapter initialized, no lock of it held): the
// connection takes the session, tells the editor its id and the adapter's
// capabilities, and sends initialized; then it waits, without the event
// gate, until the editor's configurationDone was handled (behind its
// earlier breakpoint and exception requests), or ctx ends.
func (c *connection) bind(ctx context.Context, s *session.Session) error {
	c.launch.out.finish()

	caps, _ := s.Capabilities()

	c.gate.Lock()

	if c.view.leaving {
		c.gate.Unlock()

		return errors.New("the editor disconnected")
	}

	c.sess.Store(s)
	c.presence.Store(s.Connect(c.client))
	c.send(ctx, &SessionEvent{Event: event(EventSession), Body: SessionBody{SessionID: s.ID}})
	c.send(ctx, &godap.CapabilitiesEvent{
		Event: event("capabilities"),
		Body:  godap.CapabilitiesEventBody{Capabilities: capabilities(caps, s.SupportedExceptionModes())},
	})
	// Before initialized: the editor's answer to it must be admitted.
	c.phase.store(phaseAttached)
	c.send(ctx, &godap.InitializedEvent{Event: event("initialized")})
	c.gate.Unlock()

	select {
	case <-c.launch.configured:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for configurationDone: %w", ctx.Err())
	}
}

// launchConfigurationDone queues configurationDone on the ordered worker:
// it runs after every breakpoint and exception request that came before
// it, so all of them are in place before the program runs.
func (c *connection) launchConfigurationDone(ctx context.Context, r *godap.ConfigurationDoneRequest, in inFlight) {
	if c.phase.load() != phaseAttached {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "configurationDone needs the initialized event first, once", ""), false)

		return
	}

	c.spawn(ctx, in, true, func() { c.launchConfigured(ctx, r) })
}

// launchConfigured answers the first configurationDone and releases the
// launch's Configure hook.
func (c *connection) launchConfigured(ctx context.Context, r *godap.ConfigurationDoneRequest) {
	first := false

	c.launch.configOnce.Do(func() { first = true })

	if !first {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "configurationDone was already sent", ""), false)

		return
	}

	c.respond(ctx, r, &godap.ConfigurationDoneResponse{})
	close(c.launch.configured)
}

// launchTerminate handles terminate on a launch connection until its
// launch succeeded, and reports whether it did: with nothing launched (no
// launch yet, or it failed) there is nothing to end; while the launch runs
// it is stopped. After a successful launch terminate takes the usual path
// (the ordered worker), which ends and forgets the session.
func (c *connection) launchTerminate(ctx context.Context, r *godap.TerminateRequest, in inFlight) bool {
	l := c.launch

	switch {
	case !l.started, l.finished() && l.launched.Load() == nil:
		c.respond(ctx, r, &godap.TerminateResponse{})

		return true
	case !l.finished():
		c.spawn(ctx, in, false, func() { c.terminateLaunch(ctx, r) })

		return true
	default:
		return false
	}
}

// terminateLaunch stops the running launch and answers once it returned;
// a launch that succeeded meanwhile has its session ended and forgotten
// (under the event gate, so the response comes before terminated).
func (c *connection) terminateLaunch(ctx context.Context, r *godap.TerminateRequest) {
	l := c.launch
	l.cancel()

	select {
	case <-l.done:
	case <-ctx.Done():
		return
	}

	c.gate.Lock()
	defer c.gate.Unlock()

	if c.view.leaving {
		return
	}

	if s := l.launched.Load(); s != nil {
		if err := c.stopLaunched(ctx, s); err != nil {
			c.fail(ctx, r, err, true)

			return
		}
	}

	c.respond(ctx, r, &godap.TerminateResponse{})
}

// afterExceptions says, after its response, that setExceptionBreakpoints
// named a mode the adapter can't serve (see setExceptionBreakpoints).
func (c *connection) afterExceptions() {
	if c.launch == nil || c.launch.unservable == "" {
		return
	}

	mode := c.launch.unservable
	c.launch.unservable = ""

	c.relay(func() []godap.EventMessage {
		return []godap.EventMessage{consoleLine(fmt.Sprintf(
			"the debug adapter can't stop on %s exceptions: exception stops are off for you", mode))}
	})
}

// forgetExited forgets, when the launch connection ends (on its disconnect
// request, and again when it closes), the session it launched if that has
// exited (docs/adr/0019); a live one keeps running. Stop skips the lease
// check for an exited session; one already forgotten is no error.
func (c *connection) forgetExited(ctx context.Context) {
	s := c.launch.launched.Load()
	if s == nil || s.Info().State != api.StateExited {
		return
	}

	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
	defer cancel()

	if err := c.stopLaunched(stopCtx, s); err != nil {
		c.logger.WarnContext(ctx, "facade launch: forget the exited session", slog.String("session", s.ID), slog.String("code", string(api.CodeOf(err))))
	}
}

// Launch argument names (docs/adr/0019); keys are case-sensitive.
const (
	argLang        = "lang"
	argProgram     = "program"
	argProject     = "project"
	argCwd         = "cwd"
	argArgs        = "args"
	argEnv         = "env"
	argOpts        = "opts"
	argStopOnEntry = "stopOnEntry"
	argNoBuild     = "noBuild"
	argLeasePolicy = "leasePolicy"
	argExceptions  = "exceptions"
	argAdapter     = "adapter"
)

// launchKey returns the launch argument key names case-insensitively.
func launchKey(k string) (string, bool) {
	for _, name := range []string{
		argLang, argProgram, argProject, argCwd, argArgs, argEnv, argOpts, argStopOnEntry, argNoBuild,
		argLeasePolicy, argExceptions, argAdapter,
	} {
		if strings.EqualFold(k, name) {
			return name, true
		}
	}

	return "", false
}

// launchArgs are a launch request's arguments as eyedbg reads them.
type launchArgs struct {
	lang, program, project, cwd, leasePolicy, exceptions, adapter string
	args                                                          []string
	env, opts                                                     map[string]string
	stopOnEntry, noBuild                                          bool
}

// launchParams turns a launch request's arguments into start params with
// d (docs/adr/0019): only known keys are read, each with its type, and a
// key naming one of them in another case is refused (Go would match it,
// the editor didn't mean it); other keys are ignored (editors add their
// own). No value may hold NUL. Relative paths resolve against the caller's
// directory; without a program or project, the project is that directory.
func launchParams(raw json.RawMessage, d api.FacadeLaunch) (api.StartParams, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return api.StartParams{}, invalidLaunch("launch needs its arguments as an object, with lang at least")
	}

	var a launchArgs

	for _, k := range slices.Sorted(maps.Keys(fields)) {
		name, known := launchKey(k)
		if !known {
			continue
		}

		if name != k {
			return api.StartParams{}, invalidLaunch(fmt.Sprintf("launch argument %q: did you mean %q? (names are case-sensitive)", k, name))
		}

		if err := a.set(name, fields[k]); err != nil {
			return api.StartParams{}, err
		}
	}

	if err := a.check(); err != nil {
		return api.StartParams{}, err
	}

	return a.params(d)
}

// set decodes the value of argument name.
func (a *launchArgs) set(name string, raw json.RawMessage) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return invalidLaunch("launch argument " + name + " is not valid JSON")
	}

	var err error

	switch name {
	case argLang:
		a.lang, err = stringArg(name, v)
	case argProgram:
		a.program, err = stringArg(name, v)
	case argProject:
		a.project, err = stringArg(name, v)
	case argCwd:
		a.cwd, err = stringArg(name, v)
	case argLeasePolicy:
		a.leasePolicy, err = stringArg(name, v)
	case argExceptions:
		a.exceptions, err = stringArg(name, v)
	case argAdapter:
		a.adapter, err = stringArg(name, v)
	case argArgs:
		a.args, err = stringsArg(name, v)
	case argEnv:
		a.env, err = mapArg(name, v)
	case argOpts:
		a.opts, err = mapArg(name, v)
	case argStopOnEntry:
		a.stopOnEntry, err = boolArg(name, v)
	case argNoBuild:
		a.noBuild, err = boolArg(name, v)
	}

	return err
}

// check checks the language and the adapter's names.
func (a *launchArgs) check() error {
	switch {
	case a.lang == "":
		return invalidLaunch("launch needs lang (dotnet, python, ...: see 'eyedbg adapters ls')")
	case !isName(a.lang, "_+-"):
		return invalidLaunch("launch argument lang must be a language name: lowercase letters, digits and _ + -, up to 32")
	case a.adapter != "" && !isName(a.adapter, "-"):
		return invalidLaunch("launch argument adapter must be an adapter name: lowercase letters, digits and -, up to 32")
	default:
		return nil
	}
}

// params is a as start params with d.
func (a *launchArgs) params(d api.FacadeLaunch) (api.StartParams, error) {
	var paths [3]string

	for i, p := range []string{a.program, a.project, a.cwd} {
		resolved, err := resolvePath(d.ClientDir, p)
		if err != nil {
			return api.StartParams{}, err
		}

		paths[i] = resolved
	}

	program, project, cwd := paths[0], paths[1], paths[2]
	if program == "" && project == "" {
		project = d.ClientDir
	}

	adapter := a.adapter
	if adapter == "" && a.lang == langDotnet {
		adapter = d.DotnetAdapter
	}

	return api.StartParams{
		Lang: a.lang,
		LaunchSpec: api.LaunchSpec{
			Project: project, Program: program, Cwd: cwd, Args: a.args, Env: a.env, Options: a.opts,
			NoBuild: a.noBuild, StopOnEntry: a.stopOnEntry, ClientDir: d.ClientDir, VirtualEnv: d.VirtualEnv,
		},
		NoRecord:    d.NoRecord,
		LeasePolicy: api.LeasePolicy(a.leasePolicy),
		Exceptions:  api.ExceptionMode(a.exceptions),
		Adapter:     adapter,
	}, nil
}

// isName reports whether s is 1 to 32 characters: a lowercase letter, then
// lowercase letters, digits and the characters of extra.
func isName(s, extra string) bool {
	if s == "" || len(s) > 32 || s[0] < 'a' || s[0] > 'z' {
		return false
	}

	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune(extra, r) {
			return false
		}
	}

	return true
}

// resolvePath resolves launch path p ("" stays) against dir: an absolute
// path is kept, a relative one joined to dir. On Windows a path relative to
// a drive (C:x) or to the current drive's root (\x) is refused: dir can't
// settle which one the editor meant.
func resolvePath(dir, p string) (string, error) {
	if p == "" || filepath.IsAbs(p) {
		return p, nil
	}

	if runtime.GOOS == "windows" && (filepath.VolumeName(p) != "" || strings.HasPrefix(p, `\`) || strings.HasPrefix(p, "/")) {
		return "", invalidLaunch("launch path " + p + " is relative to a drive or its root: give it in full")
	}

	return filepath.Join(dir, p), nil
}

func invalidLaunch(msg string) error {
	return api.NewError(api.CodeInvalidRequest, msg, "")
}

func stringArg(name string, v any) (string, error) {
	s, ok := v.(string)
	if !ok {
		return "", invalidLaunch("launch argument " + name + " must be a string")
	}

	return s, noNUL(name, s)
}

func stringsArg(name string, v any) ([]string, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, invalidLaunch("launch argument " + name + " must be a list of strings")
	}

	out := make([]string, 0, len(list))

	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, invalidLaunch("launch argument " + name + " must be a list of strings")
		}

		if err := noNUL(name, s); err != nil {
			return nil, err
		}

		out = append(out, s)
	}

	return out, nil
}

func mapArg(name string, v any) (map[string]string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, invalidLaunch("launch argument " + name + " must be an object of strings")
	}

	out := make(map[string]string, len(m))

	for k, e := range m {
		s, ok := e.(string)
		if !ok {
			return nil, invalidLaunch("launch argument " + name + " must be an object of strings")
		}

		if err := noNUL(name, k+s); err != nil {
			return nil, err
		}

		// As 'eyedbg start' parses --env/--opt KEY=VALUE: a name is
		// non-empty and holds no "=" (an env name with one would set
		// another variable). The name isn't echoed.
		if k == "" || strings.Contains(k, "=") {
			return nil, invalidLaunch("launch argument " + name + ` has an empty name or one holding "="`)
		}

		out[k] = s
	}

	return out, nil
}

func boolArg(name string, v any) (bool, error) {
	b, ok := v.(bool)
	if !ok {
		return false, invalidLaunch("launch argument " + name + " must be true or false")
	}

	return b, nil
}

func noNUL(name, s string) error {
	if strings.ContainsRune(s, 0) {
		return invalidLaunch("launch argument " + name + " holds a NUL character")
	}

	return nil
}

// Bounds of a launch's build output on the editor's console.
const (
	maxBuildLine  = 4096    // bytes of one line; longer ones are cut
	maxBuildLines = 10000   // lines of one launch
	maxBuildBytes = 1 << 20 // bytes of one launch
)

// buildOutput is a launch's Output hook: each line the build writes
// becomes a console output event on the launching connection only (never
// logged, never in the session's event log), bounded. Write is safe for
// concurrent use and never fails, so the editor can't fail the build; it
// drops everything once finished (the build is done) or once the bounds
// were reached. A pointer, so that os/exec, given it as both stdout and
// stderr, copies both on one goroutine.
type buildOutput struct {
	c *connection

	mu sync.Mutex
	// line is the line being written, at most maxBuildLine+utf8.UTFMax
	// bytes; cut: more of it was dropped.
	line  []byte
	cut   bool
	lines int
	bytes int
	// done: finished, or the bounds were reached.
	done bool
}

// Write takes build output.
func (o *buildOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	n := len(p)

	for len(p) > 0 && !o.done {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			o.add(p)

			break
		}

		o.add(p[:i])
		o.emit()
		p = p[i+1:]
	}

	return n, nil
}

// add appends b to the line, up to its bound.
func (o *buildOutput) add(b []byte) {
	room := maxBuildLine + utf8.UTFMax - len(o.line)
	if len(b) > room {
		b, o.cut = b[:room], true
	}

	o.line = append(o.line, b...)
}

// emit sends the line (without a trailing \r), cut to maxBuildLine bytes on
// a rune boundary with "…", or, past the bounds, one line saying so.
func (o *buildOutput) emit() {
	line := bytes.TrimSuffix(o.line, []byte("\r"))
	cut := o.cut || len(line) > maxBuildLine
	o.line, o.cut = o.line[:0], false

	if cut {
		end := min(len(line), maxBuildLine)
		for end > 0 && end < len(line) && !utf8.RuneStart(line[end]) {
			end--
		}

		line = append(line[:end:end], "…"...)
	}

	if o.lines+1 > maxBuildLines || o.bytes+len(line) > maxBuildBytes {
		o.done = true
		o.c.relay(func() []godap.EventMessage { return []godap.EventMessage{consoleLine("more build output isn't shown")} })

		return
	}

	o.lines++
	o.bytes += len(line)
	text := string(line) + "\n"

	o.c.relay(func() []godap.EventMessage { return []godap.EventMessage{outputEvent("console", text)} })
}

// finish sends a last line without a newline, and drops all later output.
func (o *buildOutput) finish() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.done && (len(o.line) > 0 || o.cut) {
		o.emit()
	}

	o.done = true
}
