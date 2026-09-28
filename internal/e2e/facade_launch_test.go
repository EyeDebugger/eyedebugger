// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/facade"
)

// TestFacadeLaunchFake: an editor's DAP launch through 'eyedbg dap
// --launch' starts the session (and the daemon) as the human, who holds its
// lease; the breakpoint set while the session is configured is in place;
// the human's terminate ends the session and forgets it (docs/adr/0019).
func TestFacadeLaunchFake(t *testing.T) {
	t.Parallel()

	lc := fakeCase(t)
	h := newHarness(t)
	fakeManifestFiles(t, h.configDir)

	args := launchArguments(lc)
	args["stopOnEntry"] = true

	p := h.dapLaunch(humanE2E)
	id := launchConfigured(t, p, args, lc.file, lc.target)

	stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent)
	if !ok || stop.Body.Reason != "entry" || stop.Body.ThreadId == 0 {
		t.Fatalf("stop after the launch = %+v, want the entry", stop)
	}

	thread := stop.Body.ThreadId
	if got := topLine(t, p, thread); got != 1 {
		t.Fatalf("stopped on entry at line %d, want 1", got)
	}

	p.ok(threadReq("next", thread))
	p.waitEvent("stopped")

	if got := topLine(t, p, thread); got != 2 {
		t.Fatalf("next stopped at line %d, want 2", got)
	}

	p.ok(threadReq("continue", thread))

	if stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent); !ok || stop.Body.Reason != "breakpoint" || topLine(t, p, thread) != lc.target {
		t.Fatalf("continue stopped with %+v, want the breakpoint at line %d", stop, lc.target)
	}

	info, listed := listedSession(t, h, id)
	if !listed || info.Lease == nil || info.Lease.Holder != humanE2E ||
		!slices.ContainsFunc(info.Clients, func(c api.ClientInfo) bool { return c.ID == humanE2E && c.Connected == 1 }) {
		t.Fatalf("sessions: listed %v, %+v; want the launch, its lease held by %s, connected", listed, info, humanE2E)
	}

	// The launcher's terminate ends and forgets the session before its
	// response.
	p.ok(&godap.TerminateRequest{Request: request("terminate")})

	if _, listed := listedSession(t, h, id); listed {
		t.Error("the session is still listed after the launcher's terminate")
	}

	p.waitEvent("terminated")
	launchLeave(t, p)
}

// TestFacadeLaunchRunsToEnd: a launched program stops at the breakpoint
// set while the session is configured, the agent sees who started it, and
// run to its end it is forgotten once its launcher leaves.
func TestFacadeLaunchRunsToEnd(t *testing.T) {
	t.Parallel()

	for _, lc := range []langCase{fakeCase(t), pythonCase(t)} {
		t.Run(lc.name, func(t *testing.T) {
			lc.require(t)
			t.Parallel()

			h := newHarness(t)
			if lc.lang == "fakelang" {
				fakeManifestFiles(t, h.configDir)
			}

			p := h.dapLaunch(humanE2E)
			id := launchConfigured(t, p, launchArguments(lc), lc.file, lc.anchor)

			stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent)
			if !ok || stop.Body.Reason != "breakpoint" || topLine(t, p, stop.Body.ThreadId) != lc.anchor {
				t.Fatalf("stop after the launch = %+v, want the breakpoint at line %d", stop, lc.anchor)
			}

			var events eventsEnvelope

			h.run("--json", "events", "-s", id, "--kind", "started", "--since", "0").wantCode(exitOK).decode(&events)

			if len(events.Events) != 1 || events.Events[0].Client != humanE2E {
				t.Errorf("started events = %+v, want one by %s", events.Events, humanE2E)
			}

			p.ok(setBreakpointsReq(lc.file))
			p.ok(threadReq("continue", stop.Body.ThreadId))
			p.waitEvent("exited")
			p.waitEvent("terminated")

			if info, listed := listedSession(t, h, id); !listed || info.State != api.StateExited {
				t.Errorf("sessions after the program ended: listed %v, %+v; want it listed as exited", listed, info)
			}

			launchLeave(t, p)

			if _, listed := listedSession(t, h, id); listed {
				t.Errorf("session %s is still listed after its launcher left", id)
			}
		})
	}
}

// TestFacadeLaunchBuild: a .NET launch by project streams its build to
// the editor's console before the session is configured, then stops at the
// editor's breakpoint; under each .NET adapter.
func TestFacadeLaunchBuild(t *testing.T) {
	t.Parallel()

	for _, lc := range []langCase{dotnetCase(t), sharpdbgCase(t)} {
		t.Run(lc.name, func(t *testing.T) {
			lc.require(t)
			t.Parallel()

			h := newHarness(t)
			p := h.dapLaunch(humanE2E)
			id := launchConfigured(t, p, launchArguments(lc), lc.file, lc.anchor)

			// The project's name, not a (localizable) phrase of the build.
			project := strings.TrimSuffix(filepath.Base(projectFile(t, lc)), ".csproj")
			if !slices.ContainsFunc(eventsBefore(p.all(), "initialized"), func(ev godap.EventMessage) bool {
				o, ok := ev.(*godap.OutputEvent)

				return ok && o.Body.Category == "console" && strings.Contains(o.Body.Output, project)
			}) {
				t.Errorf("no console output naming %s before initialized", project)
			}

			stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent)
			if !ok || stop.Body.Reason != "breakpoint" || topLine(t, p, stop.Body.ThreadId) != lc.anchor {
				t.Fatalf("stop after the launch = %+v, want the breakpoint at line %d", stop, lc.anchor)
			}

			p.ok(&godap.TerminateRequest{Request: request("terminate")})

			if _, listed := listedSession(t, h, id); listed {
				t.Error("the session is still listed after the launcher's terminate")
			}

			p.waitEvent("terminated")
			launchLeave(t, p)
		})
	}
}

// launchArguments are the DAP launch arguments starting lc's program as
// its 'eyedbg start' flags do.
func launchArguments(lc langCase) map[string]any {
	switch lc.lang {
	case "fakelang":
		return map[string]any{"lang": lc.lang, "program": lc.file, "opts": map[string]string{"laps": "2"}}
	case "python":
		return map[string]any{"lang": lc.lang, "program": lc.file, "args": []string{"loop"}}
	default: // dotnet: by project, so it builds.
		args := map[string]any{"lang": lc.lang, "project": filepath.Dir(lc.file)}
		if lc.name == dotnet.SharpDbg {
			args["adapter"] = dotnet.SharpDbg
		}

		return args
	}
}

// projectFile is the .csproj in lc's project directory.
func projectFile(t *testing.T, lc langCase) string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(filepath.Dir(lc.file), "*.csproj"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("project file in %s: %v %v", filepath.Dir(lc.file), matches, err)
	}

	return matches[0]
}

// launchConfigured runs a launch connection's handshake as VS Code does:
// initialize, launch (answered only after configurationDone by some
// adapters, so awaited last), then, once eyedbg/session, capabilities and
// initialized came in that order, a breakpoint at each line of file and no
// exception filters, and configurationDone. It returns the session id.
func launchConfigured(t *testing.T, p *dapProc, args map[string]any, file string, lines ...int) string {
	t.Helper()

	return launchConfiguredWith(t, p, args, nil, file, lines...)
}

// launchConfiguredWith is launchConfigured, calling beforeBind (unless
// nil) once the launch is sent, before waiting for eyedbg/session: a
// terminal launch's eyedbg/runInTerminal comes first.
func launchConfiguredWith(t *testing.T, p *dapProc, args map[string]any, beforeBind func(), file string, lines ...int) string {
	t.Helper()

	caps, ok := p.ok(initializeReq()).(*godap.InitializeResponse)
	if !ok || !caps.Body.SupportsConfigurationDoneRequest || !caps.Body.SupportsTerminateRequest || len(caps.Body.ExceptionBreakpointFilters) != 2 {
		t.Fatalf("initialize = %+v, want configurationDone, terminate and both exception filters", caps)
	}

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	launched := p.send(&godap.LaunchRequest{Request: request("launch"), Arguments: raw})

	if beforeBind != nil {
		beforeBind()
	}

	sess, ok := p.waitEvent(facade.EventSession).(*facade.SessionEvent)
	if !ok || sess.Body.SessionID == "" {
		t.Fatalf("eyedbg/session = %+v", sess)
	}

	p.waitEvent("capabilities")
	p.waitEvent("initialized")

	bps, ok := p.ok(setBreakpointsReq(file, lines...)).(*godap.SetBreakpointsResponse)
	if !ok || len(bps.Body.Breakpoints) != len(lines) {
		t.Fatalf("setBreakpoints = %+v", bps)
	}

	p.ok(&godap.SetExceptionBreakpointsRequest{Request: request("setExceptionBreakpoints"), Arguments: godap.SetExceptionBreakpointsArguments{Filters: []string{}}})
	p.ok(&godap.ConfigurationDoneRequest{Request: request("configurationDone")})

	if resp := launched(); !resp.GetResponse().Success {
		t.Fatalf("launch failed: %s; eyedbg dap stderr: %s", errorCode(resp), p.stderr)
	}

	return sess.Body.SessionID
}

// eventsBefore returns the events before the first one named name.
func eventsBefore(events []godap.EventMessage, name string) []godap.EventMessage {
	i := slices.IndexFunc(events, func(ev godap.EventMessage) bool { return ev.GetEvent().Event == name })
	if i < 0 {
		return events
	}

	return events[:i]
}

// launchLeave disconnects; 'eyedbg dap' exits 0.
func launchLeave(t *testing.T, p *dapProc) {
	t.Helper()

	p.ok(&godap.DisconnectRequest{Request: request("disconnect")})

	if code := p.wait(); code != exitOK {
		t.Fatalf("eyedbg dap exited %d, want 0; stderr: %s", code, p.stderr)
	}
}

// listedSession returns session id as 'eyedbg sessions --json' lists it.
func listedSession(t *testing.T, h *harness, id string) (api.SessionInfo, bool) {
	t.Helper()

	var sessions struct {
		Sessions []api.SessionInfo `json:"sessions"`
	}

	h.run("--json", "sessions").wantCode(exitOK).decode(&sessions)

	i := slices.IndexFunc(sessions.Sessions, func(s api.SessionInfo) bool { return s.ID == id })
	if i < 0 {
		return api.SessionInfo{}, false
	}

	return sessions.Sessions[i], true
}

// answerTerminal answers the eyedbg/runInTerminal event with processId pid
// and returns the event.
func answerTerminal(t *testing.T, p *dapProc, pid func(ev *facade.TerminalEvent) int) *facade.TerminalEvent {
	t.Helper()

	ev, ok := p.waitEvent(facade.CommandRunInTerminal).(*facade.TerminalEvent)
	if !ok || ev.Body.ID < 1 {
		t.Fatalf("eyedbg/runInTerminal = %+v", ev)
	}

	n := int64(pid(ev))
	p.ok(&facade.TerminalRequest{Request: request(facade.CommandRunInTerminal), Arguments: facade.TerminalArguments{ID: ev.Body.ID, ProcessID: &n}})

	return ev
}

// countEvents is how many of events are named name.
func countEvents(events []godap.EventMessage, name string) int {
	n := 0

	for _, ev := range events {
		if ev.GetEvent().Event == name {
			n++
		}
	}

	return n
}

// outputHas reports whether an output event among events holds text.
func outputHas(events []godap.EventMessage, text string) bool {
	return slices.ContainsFunc(events, func(ev godap.EventMessage) bool {
		o, ok := ev.(*godap.OutputEvent)

		return ok && strings.Contains(o.Body.Output, text)
	})
}

// TestFacadeLaunchTerminalFake: a fakelang launch with console
// integratedTerminal sends the adapter's runInTerminal to the launching
// editor, once, and the editor's process id to the adapter; a joined
// connection never gets it, and a runInTerminal after the start (asked
// through the joined connection's watch) is refused.
func TestFacadeLaunchTerminalFake(t *testing.T) {
	t.Parallel()

	lc := fakeCase(t)
	h := newHarness(t)
	fakeManifestFiles(t, h.configDir)

	args := launchArguments(lc)
	args["stopOnEntry"] = true
	args["console"] = "integratedTerminal"

	p := h.dapLaunch(humanE2E)

	var ev *facade.TerminalEvent

	id := launchConfiguredWith(t, p, args, func() {
		ev = answerTerminal(t, p, func(*facade.TerminalEvent) int { return 4242 })
	}, lc.file, lc.target)

	// The session resolves symlinks in the program's path (macOS /var).
	file, err := filepath.EvalSymlinks(lc.file)
	if err != nil {
		t.Fatal(err)
	}

	if b := ev.Body; len(b.Args) != 2 || b.Args[1] != file || b.Cwd != filepath.Dir(file) || b.Title != "Fake Debug Console" {
		t.Errorf("eyedbg/runInTerminal body = %+v, want the fake's request for %s", b, file)
	}

	if _, ok := p.waitEvent("stopped").(*godap.StoppedEvent); !ok {
		t.Fatal("no stop on entry")
	}

	if !outputHas(p.all(), "fake: runInTerminal processId=4242") {
		t.Error("the adapter didn't get the process id")
	}

	joined := h.dap("agent", "-s", id)
	joined.ok(initializeReq())
	joined.ok(&godap.AttachRequest{Request: request("attach"), Arguments: json.RawMessage(`{}`)})
	joined.waitEvent("initialized")
	joined.ok(&godap.ConfigurationDoneRequest{Request: request("configurationDone")})

	stop, ok := joined.waitEvent("stopped").(*godap.StoppedEvent)
	if !ok {
		t.Fatal("the joined connection got no stop")
	}

	frame := topFrame(t, joined, stop.Body.ThreadId).Id

	if r, ok := joined.ok(evaluateReq("$runInTerminal", "watch", frame)).(*godap.EvaluateResponse); !ok || r.Body.Result != "failed" {
		t.Errorf("runInTerminal after the start = %+v, want refused", r)
	}

	if n := countEvents(joined.all(), facade.CommandRunInTerminal); n != 0 {
		t.Errorf("the joined connection got %d eyedbg/runInTerminal", n)
	}

	if n := countEvents(p.all(), facade.CommandRunInTerminal); n != 1 {
		t.Errorf("the launcher got %d eyedbg/runInTerminal, want 1", n)
	}

	launchLeave(t, joined)
	p.ok(&godap.TerminateRequest{Request: request("terminate")})
	p.waitEvent("terminated")
	launchLeave(t, p)
}

// TestFacadeLaunchTerminalRefused: an adapter's runInTerminal the table
// refuses (an argument with a newline) fails the launch with ADAPTER_ERROR
// and never reaches the editor.
func TestFacadeLaunchTerminalRefused(t *testing.T) {
	t.Parallel()

	lc := fakeCase(t)
	h := newHarness(t)
	fakeManifestFiles(t, h.configDir)

	// The fake's request, from its options in its manifest's environment.
	opts, err := json.Marshal(daptest.Options{Terminal: &godap.RunInTerminalRequestArguments{
		Cwd: filepath.Dir(lc.file), Args: []string{lc.file, "a\nrm -rf x"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	m := daptest.UserManifest(exe, "")
	env, _ := m["adapter"].(map[string]any)["environment"].(map[string]any)
	env[daptest.EnvFakeOptions] = string(opts)

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(h.configDir, "adapters", "fakelang.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	args := launchArguments(lc)
	args["console"] = "integratedTerminal"

	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	p := h.dapLaunch("human:ana")
	p.ok(initializeReq())

	resp := p.do(&godap.LaunchRequest{Request: request("launch"), Arguments: body})
	if code := errorCode(resp); !strings.HasPrefix(code, string(api.CodeAdapterFailed)+": ") || !strings.Contains(code, "rule 4") {
		t.Fatalf("launch = %s, want ADAPTER_ERROR for rule 4", code)
	}

	if n := countEvents(p.all(), facade.CommandRunInTerminal); n != 0 {
		t.Errorf("the editor got %d eyedbg/runInTerminal", n)
	}

	launchLeave(t, p)
}

// TestFacadeLaunchTerminalPython: a Python launch with console
// integratedTerminal runs the program as the editor starts it — here
// exec'd without a shell, stdin a pipe — with its arguments exactly as
// given, reading what the "terminal" types; it is forgotten once its
// launcher leaves.
func TestFacadeLaunchTerminalPython(t *testing.T) {
	t.Parallel()

	lc := pythonCase(t)
	lc.require(t)

	h := newHarness(t)
	progArgs := []string{"a b", "$HOME;echo x", `"q"`}
	args := map[string]any{
		"lang": "python", "program": lc.file, "args": append([]string{"input"}, progArgs...),
		"stopOnEntry": true, "console": "integratedTerminal",
	}

	p := h.dapLaunch(humanE2E)

	var stdin io.WriteCloser

	id := launchConfiguredWith(t, p, args, func() {
		answerTerminal(t, p, func(ev *facade.TerminalEvent) int {
			cmd, in := startTerminal(t, ev.Body)
			stdin = in

			return cmd.Process.Pid
		})
	}, lc.file)

	stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent)
	if !ok || stop.Body.Reason != "entry" {
		t.Fatalf("stop = %+v, want the entry", stop)
	}

	p.ok(threadReq("continue", stop.Body.ThreadId))

	if _, err := io.WriteString(stdin, "hello\n"); err != nil {
		t.Fatal(err)
	}

	p.waitEvent("exited")
	p.waitEvent("terminated")

	events := p.all()
	if want := `args ['a b', '$HOME;echo x', '"q"']`; !outputHas(events, "got hello") || !outputHas(events, want) {
		t.Errorf("output holds no %q and %q: %s", "got hello", want, outputs(events))
	}

	launchLeave(t, p)

	if _, listed := listedSession(t, h, id); listed {
		t.Errorf("session %s is still listed after its launcher left", id)
	}
}

// startTerminal runs body as the editor's terminal would, without a
// shell: args[0] with the rest as its arguments, in cwd, with env added
// (null unsets), its stdin a pipe the test writes. It is waited for (and
// killed if still running) at cleanup.
func startTerminal(t *testing.T, body facade.TerminalEventBody) (*exec.Cmd, io.WriteCloser) {
	t.Helper()

	env := map[string]string{}

	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}

	for k, v := range body.Env {
		if v == nil {
			delete(env, k)
		} else {
			env[k] = *v
		}
	}

	cmd := exec.CommandContext(context.Background(), body.Args[0], body.Args[1:]...) //nolint:gosec // The facade's checked request, as the editor runs it.
	cmd.Dir = body.Cwd

	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the terminal's program: %v", err)
	}

	exited := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(exited)
	}()

	t.Cleanup(func() {
		_ = stdin.Close()

		select {
		case <-exited:
		case <-time.After(dapTimeout):
			_ = cmd.Process.Kill()
			<-exited

			t.Error("the terminal's program was still running")
		}
	})

	return cmd, stdin
}

// outputs are the output events' texts, for a failure message.
func outputs(events []godap.EventMessage) string {
	var b strings.Builder

	for _, ev := range events {
		if o, ok := ev.(*godap.OutputEvent); ok {
			b.WriteString(o.Body.Output)
		}
	}

	return b.String()
}
