// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// streamDriver is the fake driver with a streamed build: PrepareWith
// writes out to the options' Output, then fails with fail, waits for ctx
// to end (block) or prepares as the fake does.
type streamDriver struct {
	fakeDriver

	out   []string
	fail  error
	block bool
}

func (d streamDriver) PrepareWith(ctx context.Context, spec session.LaunchSpec, opts session.PrepareOptions) (session.Launch, error) {
	if opts.Output != nil {
		for _, s := range d.out {
			if _, err := io.WriteString(opts.Output, s); err != nil {
				return session.Launch{}, err
			}
		}
	}

	switch {
	case d.fail != nil:
		return session.Launch{}, d.fail
	case d.block:
		<-ctx.Done()

		return session.Launch{}, fmt.Errorf("build: %w", ctx.Err())
	default:
		return d.Prepare(ctx, spec)
	}
}

// launchEnv is a manager for drv and a directory holding a 10-line
// program, prog.txt.
type launchEnv struct {
	m    *session.Manager
	dir  string
	prog string
}

func newLaunchEnv(t *testing.T, drv session.Driver) *launchEnv {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := session.NewManager(ctx, session.Config{Drivers: []session.Driver{drv}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	prog := filepath.Join(dir, "prog.txt")
	if err := os.WriteFile(prog, []byte(strings.Repeat("line\n", 10)), 0o600); err != nil {
		t.Fatal(err)
	}

	return &launchEnv{m: m, dir: dir, prog: prog}
}

// open opens a launch connection for c, initialized.
func (e *launchEnv) open(t *testing.T, c api.Client) *testClient {
	t.Helper()

	tc := joinWith(t, Config{Launcher: &Launcher{Manager: e.m, Defaults: api.FacadeLaunch{ClientDir: e.dir}}, Client: c})
	tc.ok("initialize", `{"adapterID":"eyedbg","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path"}`)

	return tc
}

// launchArgs are launch arguments for the fake program, with extra JSON
// members.
func (e *launchEnv) launchArgs(extra string) string {
	args := `{"lang":"fake","program":` + jsonString(e.prog)
	if extra != "" {
		args += "," + extra
	}

	return args + "}"
}

// listed reports whether the manager lists session id.
func (e *launchEnv) listed(id string) (api.SessionInfo, bool) {
	i := slices.IndexFunc(e.m.List(), func(s api.SessionInfo) bool { return s.ID == id })
	if i < 0 {
		return api.SessionInfo{}, false
	}

	return e.m.List()[i], true
}

// eventIndex is the index of the first event named name, -1 if none.
func (tc *testClient) eventIndex(name string) int {
	return slices.IndexFunc(tc.events, func(ev godap.EventMessage) bool { return ev.GetEvent().Event == name })
}

// bound waits for the launch to bind its session and returns its id.
func (tc *testClient) bound() string {
	tc.t.Helper()

	ev, ok := tc.waitEvent(EventSession, nil).(*SessionEvent)
	if !ok || ev.Body.SessionID == "" {
		tc.t.Fatalf("eyedbg/session = %+v", ev)
	}

	tc.waitEvent("initialized", nil)

	return ev.Body.SessionID
}

// TestLaunch: the whole launch — initialize's fixed capabilities, the
// build's lines, eyedbg/session, capabilities and initialized in that
// order, the editor's breakpoint in place before the program runs, the
// launch answered, the stop replayed, the end; the session is forgotten
// once the launching connection leaves after the program exited. Nothing
// of the launch's arguments, environment or build reaches the log.
func TestLaunch(t *testing.T) {
	t.Parallel()

	const secret = "s3cret-value"

	e := newLaunchEnv(t, streamDriver{out: []string{"restore " + secret + "\n", "build\r\n", "done"}})
	logs := &logRecorder{}

	tc := joinWith(t, Config{Launcher: &Launcher{Manager: e.m, Defaults: api.FacadeLaunch{ClientDir: e.dir}}, Client: humanC, Logger: slog.New(logs)})

	caps, ok := tc.ok("initialize", `{"adapterID":"eyedbg","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path"}`).(*godap.InitializeResponse)
	if !ok || !caps.Body.SupportsConfigurationDoneRequest || !caps.Body.SupportsTerminateRequest || len(caps.Body.ExceptionBreakpointFilters) != 2 {
		t.Fatalf("initialize = %+v", caps)
	}

	launch := tc.send("launch", e.launchArgs(`"env":{"TOKEN":"`+secret+`"},"args":["`+secret+`"],"__sessionId":"x"`))
	id := tc.bound()

	tc.checkBound([]string{"console:restore " + secret + "\n", "console:build\n", "console:done\n"})

	bp := tc.setBPs(e.prog, "4")
	tc.ok("configurationDone", "")

	if r, _ := tc.response(launch); !r.GetResponse().Success {
		t.Fatalf("launch = %s", errorText(r))
	}

	tc.stoppedAt(bp[0].Line)

	tc.ok("continue", `{"threadId":1}`)
	tc.waitEvent("terminated", nil)

	if info, ok := e.listed(id); !ok || info.State != api.StateExited {
		t.Errorf("after the program exited: listed %v (%+v), want listed as exited", ok, info)
	}

	tc.ok("disconnect", "")

	// Forgotten before the disconnect response, not when the connection
	// closes: the client may list sessions as soon as it has the response.
	if _, ok := e.listed(id); ok {
		t.Error("the exited session is still listed after its launcher's disconnect response")
	}

	tc.waitClosed()

	if got := logs.containing(secret, e.prog); len(got) != 0 {
		t.Errorf("logged %q", got)
	}
}

// checkBound checks what came before initialized: the build's output
// (category:text), then eyedbg/session, then the adapter's capabilities.
func (tc *testClient) checkBound(build []string) {
	tc.t.Helper()

	var outputs []string

	for _, ev := range tc.events {
		if o, ok := ev.(*godap.OutputEvent); ok {
			outputs = append(outputs, o.Body.Category+":"+o.Body.Output)
		}
	}

	if !slices.Equal(outputs, build) {
		tc.t.Errorf("build output = %q, want %q", outputs, build)
	}

	order := []int{tc.eventIndex("output"), tc.eventIndex(EventSession), tc.eventIndex("capabilities"), tc.eventIndex("initialized")}
	if !slices.IsSorted(order) || order[0] < 0 {
		tc.t.Fatalf("events %s: want output, eyedbg/session, capabilities, initialized in that order", tc.eventNames())
	}

	caps, ok := tc.events[order[2]].(*godap.CapabilitiesEvent)
	if !ok || len(caps.Body.Capabilities.ExceptionBreakpointFilters) != 1 || !caps.Body.Capabilities.SupportsFunctionBreakpoints {
		tc.t.Errorf("capabilities event = %+v", caps)
	}
}

// containing returns the records whose message or attributes hold any of
// subs.
func (l *logRecorder) containing(subs ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var out []string

	for i := range l.records {
		r := &l.records[i]
		text := r.Message

		r.Attrs(func(a slog.Attr) bool {
			text += " " + a.String()

			return true
		})

		if slices.ContainsFunc(subs, func(sub string) bool { return strings.Contains(text, sub) }) {
			out = append(out, text)
		}
	}

	return out
}

// TestLaunchPipelinedConfiguration: VS Code sends its breakpoints and
// exception filters and configurationDone at once, without waiting for
// responses; configurationDone must not let the program run before the
// breakpoints are in place, so the program stops at the breakpoint every
// time.
func TestLaunchPipelinedConfiguration(t *testing.T) {
	t.Parallel()

	for i := range 4 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()

			e := newLaunchEnv(t, fakeDriver{})
			tc := e.open(t, humanC)
			launch := tc.send("launch", e.launchArgs(""))
			tc.bound()

			var batch strings.Builder

			seqs := make([]int, 0, 4)

			for _, r := range [][2]string{
				{"setBreakpoints", `{"source":{"path":` + jsonString(e.prog) + `},"breakpoints":[{"line":3}]}`},
				{"setFunctionBreakpoints", `{"breakpoints":[]}`},
				{"setExceptionBreakpoints", `{"filters":[]}`},
				{"configurationDone", ""},
			} {
				seq, msg := tc.request(r[0], r[1])
				seqs = append(seqs, seq)
				batch.WriteString(msg)
			}

			tc.rawBytes(batch.String())

			var bp *godap.SetBreakpointsResponse

			for _, seq := range append(seqs, launch) {
				r, _ := tc.response(seq)
				if !r.GetResponse().Success {
					t.Fatalf("request %d: %s", seq, errorText(r))
				}

				if seq == seqs[0] {
					bp, _ = r.(*godap.SetBreakpointsResponse)
				}
			}

			if bp == nil || len(bp.Body.Breakpoints) != 1 {
				t.Fatalf("setBreakpoints = %+v", bp)
			}

			tc.stoppedAt(3)
		})
	}
}

// stoppedAt waits for a stop and checks it is at a breakpoint at line.
func (tc *testClient) stoppedAt(line int) {
	tc.t.Helper()

	stop, ok := tc.waitEvent("stopped", nil).(*godap.StoppedEvent)
	if !ok || stop.Body.Reason != "breakpoint" {
		tc.t.Fatalf("stopped = %+v, want at a breakpoint; events %s", stop, tc.eventNames())
	}

	st, ok := tc.ok("stackTrace", `{"threadId":1}`).(*godap.StackTraceResponse)
	if !ok || len(st.Body.StackFrames) == 0 || st.Body.StackFrames[0].Line != line {
		tc.t.Fatalf("stack = %+v, want at line %d", st, line)
	}
}

// TestLaunchHandshakeErrors: a launch connection refuses attach and a
// second launch; a join connection refuses launch; before the session is
// bound requests are refused without the "does not support" an editor
// extension reads as unsupported; the launch's arguments are checked
// before anything runs, and a refused launch leaves the connection usable.
func TestLaunchHandshakeErrors(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, streamDriver{out: []string{"building\n"}, block: true})
	tc := joinWith(t, Config{Launcher: &Launcher{Manager: e.m, Defaults: api.FacadeLaunch{ClientDir: e.dir}}, Client: humanC})

	tc.fails("launch", e.launchArgs(""), api.CodeInvalidRequest)
	tc.fails("threads", "", api.CodeInvalidRequest)
	tc.ok("initialize", `{"adapterID":"x","linesStartAt1":true,"columnsStartAt1":true}`)

	notStarted := tc.fails("threads", "", api.CodeInvalidRequest)
	if !strings.Contains(notStarted.Format, "send launch first") {
		t.Errorf("threads before launch = %+v", notStarted)
	}

	if a := tc.fails("attach", "{}", api.CodeInvalidRequest); !strings.Contains(a.Format, "eyedbg dap -s") {
		t.Errorf("attach on a launch connection = %+v", a)
	}

	tc.fails("launch", `{"lang":"fake","Program":"x"}`, api.CodeInvalidRequest)
	tc.fails("launch", `{"program":"x"}`, api.CodeInvalidRequest)

	launch := tc.send("launch", e.launchArgs(""))
	tc.waitEvent("output", nil)

	tc.fails("launch", e.launchArgs(""), api.CodeInvalidRequest)

	for _, req := range [][2]string{
		{"threads", ""},
		{"setBreakpoints", `{"source":{"path":` + jsonString(e.prog) + `},"breakpoints":[{"line":3}]}`},
		{"configurationDone", ""},
		{CommandLease, `{"action":"status"}`},
		{"continue", `{"threadId":1}`},
	} {
		msg := tc.fails(req[0], req[1], api.CodeInvalidRequest)
		if strings.Contains(msg.Format, "does not support") || (req[0] != "configurationDone" && !strings.Contains(msg.Format, "still starting")) {
			t.Errorf("%s while starting = %+v", req[0], msg)
		}
	}

	tc.ok("terminate", "")

	if r, _ := tc.response(launch); r.GetResponse().Success {
		t.Errorf("launch after terminate = %s, want an error", errorText(r))
	}

	joined := join(t, startSession(t, fakeDriver{}, startParams(stopOnEntry, "")), humanC)
	joined.ok("initialize", `{"adapterID":"x","linesStartAt1":true,"columnsStartAt1":true}`)

	if l := joined.fails("launch", "{}", api.CodeInvalidRequest); !strings.Contains(l.Format, "eyedbg dap --launch") {
		t.Errorf("launch on a join connection = %+v", l)
	}
}

// TestLaunchStopped: a terminate or disconnect while the launch runs (a
// build that waits, or the editor's configuration) stops it: terminate is
// answered once the launch returned, the launch gets an error the user
// isn't shown, and no session stays.
func TestLaunchStopped(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name  string
		bound bool
		how   string
	}{
		{name: "terminate during the build", how: "terminate"},
		{name: "disconnect during the build", how: "disconnect"},
		{name: "close during the build", how: "close"},
		{name: "terminate while configuring", bound: true, how: "terminate"},
		{name: "disconnect while configuring", bound: true, how: "disconnect"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newLaunchEnv(t, streamDriver{out: []string{"building\n"}, block: !tt.bound})
			tc := e.open(t, humanC)
			launch := tc.send("launch", e.launchArgs(""))

			if tt.bound {
				tc.bound()
			} else {
				tc.waitEvent("output", nil)
			}

			switch tt.how {
			case "terminate":
				tc.terminateLaunch(launch)
			case "disconnect":
				tc.ok("disconnect", "")
			case "close":
				_ = tc.conn.Close()
			}

			tc.waitClosed()

			if list := e.m.List(); len(list) != 0 {
				t.Errorf("sessions after the launch was stopped = %+v", list)
			}
		})
	}
}

// terminateLaunch terminates a running launch (request seq launch) and
// checks that the launch was answered first, with an error not shown to
// the user, and that the connection then refuses requests but terminate
// and disconnect.
func (tc *testClient) terminateLaunch(launch int) {
	tc.t.Helper()

	term, _ := tc.response(tc.send("terminate", ""))
	if !term.GetResponse().Success {
		tc.t.Fatalf("terminate = %s", errorText(term))
	}

	_, first := tc.responses[launch]

	r, _ := tc.response(launch)
	if e, ok := r.(*godap.ErrorResponse); !first || !ok || e.Body.Error == nil || e.Body.Error.ShowUser {
		tc.t.Errorf("launch = %s (before terminate's response: %v), want an error not shown", errorText(r), first)
	}

	tc.fails("threads", "", api.CodeInvalidRequest)
	tc.ok("terminate", "")
	tc.ok("disconnect", "")
}

// TestLaunchFails: a launch the session refuses (an unknown language) or
// whose build fails is answered with the error, shown to the user, after
// the build's output; the connection stays open.
func TestLaunchFails(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		drv  session.Driver
		args string
		code api.Code
	}{
		{name: "unknown language", drv: fakeDriver{}, args: `{"lang":"cobol"}`, code: api.CodeInvalidRequest},
		{
			name: "build failed",
			drv:  streamDriver{out: []string{"error CS0029\n"}, fail: api.NewError(api.CodeBuildFailed, "dotnet build failed (its output is above)", "")},
			code: api.CodeBuildFailed,
		},
		{name: "uncoded", drv: streamDriver{fail: errors.New("boom")}, code: api.CodeInternal},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newLaunchEnv(t, tt.drv)
			tc := e.open(t, humanC)

			args := tt.args
			if args == "" {
				args = e.launchArgs("")
			}

			r, ok := tc.do("launch", args).(*godap.ErrorResponse)
			if !ok || r.Message != string(tt.code) || r.Body.Error == nil || !r.Body.Error.ShowUser {
				t.Fatalf("launch = %s, want %s shown", errorText(r), tt.code)
			}

			if tt.code == api.CodeBuildFailed && tc.eventIndex("output") < 0 {
				t.Errorf("no build output before the error: %s", tc.eventNames())
			}

			tc.fails("threads", "", api.CodeInvalidRequest)
			tc.fails("launch", args, api.CodeInvalidRequest)
			tc.ok("terminate", "")
			tc.ok("disconnect", "")
			tc.waitClosed()

			if list := e.m.List(); len(list) != 0 {
				t.Errorf("sessions = %+v", list)
			}
		})
	}
}

// launched launches the fake program stopped on entry and returns its
// session's id once the launch was answered and the stop came.
func (e *launchEnv) launched(tc *testClient) string {
	tc.t.Helper()

	launch := tc.send("launch", e.launchArgs(`"stopOnEntry":true`))
	id := tc.bound()
	tc.ok("configurationDone", "")

	if r, _ := tc.response(launch); !r.GetResponse().Success {
		tc.t.Fatalf("launch = %s", errorText(r))
	}

	tc.waitEvent("stopped", nil)

	return id
}

// TestLaunchTerminate: the launcher's terminate ends and forgets the
// session (answered before terminated).
func TestLaunchTerminate(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, fakeDriver{})
	tc := e.open(t, humanC)
	id := e.launched(tc)

	tc.checkBefore("terminate", "", "terminated")

	if _, ok := e.listed(id); ok {
		t.Error("the session is still listed after its launcher's terminate")
	}

	tc.ok("terminate", "")
}

// TestLaunchJoinedTerminate: a joined connection's terminate leaves the
// launched session listed as exited (D32) until its launcher's connection
// ends.
func TestLaunchJoinedTerminate(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, fakeDriver{})
	tc := e.open(t, humanC)
	id := e.launched(tc)

	s, err := e.m.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	other := joinReady(t, s, humanC, "")
	other.ok("terminate", "")
	tc.waitEvent("terminated", nil)

	if info, ok := e.listed(id); !ok || info.State != api.StateExited {
		t.Errorf("after a joined terminate: listed %v (%+v), want exited", ok, info)
	}

	_ = tc.conn.Close()
	tc.waitClosed()

	if _, ok := e.listed(id); ok {
		t.Error("the exited session is still listed after its launcher's connection closed")
	}
}

// TestLaunchLiveSessionStays: a launcher leaving a live session leaves it
// running, with its presence gone.
func TestLaunchLiveSessionStays(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, fakeDriver{})
	tc := e.open(t, humanC)
	id := e.launched(tc)
	tc.ok("disconnect", "")
	tc.waitClosed()

	info, ok := e.listed(id)
	if !ok || info.State == api.StateExited {
		t.Fatalf("after the launcher left: listed %v (%+v), want live", ok, info)
	}

	for _, c := range info.Clients {
		if c.Connected != 0 {
			t.Errorf("client %s still connected %d", c.ID, c.Connected)
		}
	}
}

// TestLaunchUnservableException: an exception filter the adapter can't
// serve (both are offered before it is known) is accepted as none, with
// one console line.
func TestLaunchUnservableException(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, fakeDriver{})
	tc := e.open(t, humanC)
	launch := tc.send("launch", e.launchArgs(`"stopOnEntry":true`))
	id := tc.bound()

	tc.ok("setExceptionBreakpoints", `{"filters":["uncaught"]}`)
	tc.waitEvent("output", func(ev godap.EventMessage) bool {
		o, ok := ev.(*godap.OutputEvent)

		return ok && strings.Contains(o.Body.Output, "can't stop on uncaught exceptions")
	})
	tc.ok("configurationDone", "")
	tc.response(launch)

	s, err := e.m.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	if res, _ := s.Exceptions(t.Context(), humanC, api.ExceptionsParams{}); len(res.Modes) != 0 {
		t.Errorf("exception modes = %+v, want none", res.Modes)
	}

	tc.ok("setExceptionBreakpoints", `{"filters":["all"]}`)

	if res, _ := s.Exceptions(t.Context(), humanC, api.ExceptionsParams{}); len(res.Modes) != 1 || res.Modes[0].Mode != api.ExceptionsAll {
		t.Errorf("exception modes = %+v, want all", res.Modes)
	}
}

// TestBuildOutputBounds: long lines are cut on a rune boundary, and the
// launch's output stops at its bounds with one line saying so.
func TestBuildOutputBounds(t *testing.T) {
	t.Parallel()

	// 3 bytes each: byte maxBuildLine (4096 % 3 == 1) is inside a rune, so
	// the cut steps back to a rune start.
	long := strings.Repeat("€", maxBuildLine)
	manyLines := strings.Repeat("x\n", maxBuildLines+5)
	bigLines := strings.Repeat(strings.Repeat("y", maxBuildLine-1)+"\n", maxBuildBytes/maxBuildLine+5)

	for _, tt := range []struct {
		name  string
		out   []string
		check func(t *testing.T, got []string)
	}{
		// Written split inside a rune; the last line has no newline.
		{name: "long line", out: []string{long[:1001], long[1001:] + "\nnext"}, check: checkLongLine},
		{name: "lines", out: []string{manyLines}, check: checkManyLines},
		{name: "bytes", out: []string{bigLines}, check: checkBigOutput},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newLaunchEnv(t, streamDriver{out: tt.out, fail: api.NewError(api.CodeBuildFailed, "failed", "")})
			tc := e.open(t, humanC)
			tc.do("launch", e.launchArgs(""))

			var got []string

			for _, ev := range tc.events {
				if o, ok := ev.(*godap.OutputEvent); ok {
					got = append(got, o.Body.Output)
				}
			}

			tt.check(t, got)
		})
	}
}

func checkLongLine(t *testing.T, got []string) {
	t.Helper()

	if len(got) != 2 || got[1] != "next\n" || !utf8.ValidString(got[0]) || !strings.HasSuffix(got[0], "…\n") ||
		len(got[0]) > maxBuildLine+len("…\n") || len(got[0]) < maxBuildLine-2 {
		t.Errorf("got %d lines, first %d bytes, valid %v", len(got), len(got[0]), utf8.ValidString(got[0]))
	}
}

func checkManyLines(t *testing.T, got []string) {
	t.Helper()

	if len(got) != maxBuildLines+1 || !strings.Contains(got[maxBuildLines], "more build output isn't shown") {
		t.Errorf("got %d lines, last %q", len(got), got[len(got)-1])
	}
}

func checkBigOutput(t *testing.T, got []string) {
	t.Helper()

	total := 0
	for _, l := range got[:len(got)-1] {
		total += len(l) - 1
	}

	if total > maxBuildBytes || total < maxBuildBytes-maxBuildLine || !strings.Contains(got[len(got)-1], "more build output isn't shown") {
		t.Errorf("got %d lines, %d bytes", len(got), total)
	}
}

// TestLaunchParams: the launch arguments as start params.
func TestLaunchParams(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "work")
	d := api.FacadeLaunch{ClientDir: dir, VirtualEnv: "/venv", NoRecord: true, DotnetAdapter: "sharpdbg"}
	abs := filepath.Join(dir, "abs", "p.py")
	spec := func(ls api.LaunchSpec) api.LaunchSpec {
		ls.ClientDir, ls.VirtualEnv = dir, "/venv"

		return ls
	}

	tests := []struct {
		name string
		args string
		want api.StartParams
	}{
		{name: "defaults", args: `{"lang":"python"}`, want: api.StartParams{Lang: "python", LaunchSpec: spec(api.LaunchSpec{Project: dir}), NoRecord: true}},
		{
			name: "everything",
			args: `{"lang":"python","program":` + jsonString(abs) + `,"cwd":"sub","args":["a b",""],"env":{"K":"v"},"opts":{"module":"pytest"},` +
				`"stopOnEntry":true,"noBuild":true,"leasePolicy":"handoff","exceptions":"all","adapter":"debugpy","extra":{"x":1},"__restart":true}`,
			want: api.StartParams{
				Lang: "python", NoRecord: true, LeasePolicy: api.LeaseHandoff, Exceptions: api.ExceptionsAll, Adapter: "debugpy",
				LaunchSpec: spec(api.LaunchSpec{
					Program: abs, Cwd: filepath.Join(dir, "sub"), Args: []string{"a b", ""}, Env: map[string]string{"K": "v"},
					Options: map[string]string{"module": "pytest"}, StopOnEntry: true, NoBuild: true,
				}),
			},
		},
		{
			name: "relative project", args: `{"lang":"dotnet","project":"src/app"}`,
			want: api.StartParams{Lang: "dotnet", LaunchSpec: spec(api.LaunchSpec{Project: filepath.Join(dir, "src", "app")}), NoRecord: true, Adapter: "sharpdbg"},
		},
		{
			name: "dotnet adapter named", args: `{"lang":"dotnet","adapter":"netcoredbg"}`,
			want: api.StartParams{Lang: "dotnet", LaunchSpec: spec(api.LaunchSpec{Project: dir}), NoRecord: true, Adapter: "netcoredbg"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if p, err := launchParams(json.RawMessage(tt.args), d); err != nil || !reflect.DeepEqual(p, tt.want) {
				t.Errorf("launchParams = %+v, %v\nwant %+v", p, err, tt.want)
			}
		})
	}
}

// TestLaunchParamsRefused: launch arguments refused as INVALID_REQUEST.
func TestLaunchParamsRefused(t *testing.T) {
	t.Parallel()

	for name, args := range map[string]string{
		"null arguments":     `null`,
		"not an object":      `[1]`,
		"no lang":            `{"program":"x"}`,
		"bad lang":           `{"lang":"Py"}`,
		"long lang":          `{"lang":"` + strings.Repeat("a", 33) + `"}`,
		"bad adapter":        `{"lang":"python","adapter":"a_b"}`,
		"case variant":       `{"lang":"python","Program":"x"}`,
		"case variant lang":  `{"LANG":"python"}`,
		"number program":     `{"lang":"python","program":1}`,
		"null program":       `{"lang":"python","program":null}`,
		"string args":        `{"lang":"python","args":"a b"}`,
		"number in args":     `{"lang":"python","args":["a",1]}`,
		"number env":         `{"lang":"python","env":{"K":1}}`,
		"string stopOnEntry": `{"lang":"python","stopOnEntry":"true"}`,
		"NUL program":        `{"lang":"python","program":"a\u0000b"}`,
		"NUL arg":            `{"lang":"python","args":["\u0000"]}`,
		"NUL env key":        `{"lang":"python","env":{"K\u0000":"v"}}`,
		"NUL opt value":      `{"lang":"python","opts":{"k":"\u0000"}}`,
		"empty env key":      `{"lang":"python","env":{"":"v"}}`,
		"= in env key":       `{"lang":"python","env":{"PATH=/x:":"y"}}`,
		"= env key":          `{"lang":"python","env":{"=":"y"}}`,
		"empty opt key":      `{"lang":"python","opts":{"":"v"}}`,
		"= in opt key":       `{"lang":"python","opts":{"a=b":"v"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if p, err := launchParams(json.RawMessage(args), api.FacadeLaunch{ClientDir: t.TempDir()}); api.CodeOf(err) != api.CodeInvalidRequest {
				t.Errorf("launchParams = %+v, %v; want INVALID_REQUEST", p, err)
			}
		})
	}
}

// TestResolvePath: launch paths against the caller's directory; on
// Windows a drive- or root-relative one is refused.
func TestResolvePath(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "work")

	type pathCase struct {
		in, want string
		bad      bool
	}

	tests := []pathCase{
		{in: "", want: ""},
		{in: "a", want: filepath.Join(dir, "a")},
		{in: filepath.Join("..", "b"), want: filepath.Join(filepath.Dir(dir), "b")},
		{in: dir, want: dir},
	}

	if runtime.GOOS == "windows" {
		tests = append(tests,
			pathCase{in: `C:x`, bad: true},
			pathCase{in: `\x`, bad: true},
			pathCase{in: `/x`, bad: true},
			pathCase{in: `\\server\share\x`, want: `\\server\share\x`},
		)
	} else {
		tests = append(tests, pathCase{in: `\x`, want: dir + "/" + `\x`})
	}

	for _, tt := range tests {
		got, err := resolvePath(dir, tt.in)
		if tt.bad != (err != nil) || (!tt.bad && got != tt.want) {
			t.Errorf("resolvePath(%q) = %q, %v; want %q (refused: %v)", tt.in, got, err, tt.want, tt.bad)
		}
	}
}
