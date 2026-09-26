// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
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
			waitUnlisted(t, h, id)
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

	caps, ok := p.ok(initializeReq()).(*godap.InitializeResponse)
	if !ok || !caps.Body.SupportsConfigurationDoneRequest || !caps.Body.SupportsTerminateRequest || len(caps.Body.ExceptionBreakpointFilters) != 2 {
		t.Fatalf("initialize = %+v, want configurationDone, terminate and both exception filters", caps)
	}

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	launched := p.send(&godap.LaunchRequest{Request: request("launch"), Arguments: raw})

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

// waitUnlisted waits, up to dapTimeout, until 'eyedbg sessions' no longer
// lists id: the daemon forgets a launcher's exited session once the
// launcher's connection has ended, which is after 'eyedbg dap' saw it end
// (internal/facade Serve), so the first listing may still show it.
func waitUnlisted(t *testing.T, h *harness, id string) {
	t.Helper()

	deadline := time.Now().Add(dapTimeout)

	for {
		if _, listed := listedSession(t, h, id); !listed {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("session %s is still listed %s after its launcher left", id, dapTimeout)
		}
	}
}
