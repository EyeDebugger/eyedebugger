// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// TestSharedConditions: through the real binaries and the fake adapter,
// the agent's and a human's CLI breakpoints at one line with different
// conditions each stop the program when their own holds; the CLI names
// whose breakpoint a stop is for (text and 'status --json'), and an editor
// joined over 'eyedbg dap' as another human gets the same ids as the
// stopped event's hitBreakpointIds.
func TestSharedConditions(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	fakeManifestFiles(t, h.configDir)

	program := filepath.Join(t.TempDir(), "prog.fake")
	if err := os.WriteFile(program, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var snap snapshotEnvelope

	h.run("start", "fakelang", "--program", program, "--opt", "lines=3", "--opt", "laps=5", "--stop-on-entry",
		"--timeout", startTimeout, "--json").wantCode(exitOK).decode(&snap)

	if snap.Session.State != api.StateStopped {
		t.Fatalf("start: %+v, want stopped at entry", snap.Session)
	}

	agents := addBreakpoint(t, h, "agent", program, "lap == 2")
	humans := addBreakpoint(t, h, humanE2E, program, "lap == 4")

	p := h.dap("human:editor", "-s", snap.Session.ID)
	joinAtEntry(t, p)

	for _, want := range []api.StopBreakpoint{{ID: agents, Owner: "agent"}, {ID: humans, Owner: humanE2E}} {
		h.run("continue", "--timeout", startTimeout).wantCode(exitOK).
			wantStdout("\n  stopped for breakpoint " + strconv.Itoa(want.ID) + " of " + want.Owner + "\n")

		h.run("--json", "status").wantCode(exitOK).decode(&snap)

		if st := snap.Session.Stop; st == nil || !slices.Equal(st.Breakpoints, []api.StopBreakpoint{want}) {
			t.Errorf("status --json stop = %+v, want breakpoints [%+v]", st, want)
		}

		stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent)
		if !ok || !slices.Equal(stop.Body.HitBreakpointIds, []int{want.ID}) {
			t.Errorf("editor's stopped = %+v, want hitBreakpointIds [%d]", stop, want.ID)
		}
	}

	p.ok(&godap.DisconnectRequest{Request: request("disconnect")})

	if code := p.wait(); code != exitOK {
		t.Errorf("eyedbg dap exited %d, want 0; stderr: %s", code, p.stderr)
	}

	h.run("stop").wantCode(exitOK)
}

// addBreakpoint adds client's breakpoint at line 2 of program with cond
// and returns its id.
func addBreakpoint(t *testing.T, h *harness, client, program, cond string) int {
	t.Helper()

	var bps struct {
		Breakpoints []api.Breakpoint `json:"breakpoints"`
	}

	h.run("--as", client, "--json", "bp", "add", loc(program, 2), "--if", cond).wantCode(exitOK).decode(&bps)

	if len(bps.Breakpoints) != 1 || bps.Breakpoints[0].Owner != client {
		t.Fatalf("bp add as %s = %+v", client, bps.Breakpoints)
	}

	return bps.Breakpoints[0].ID
}

// joinAtEntry completes an editor's handshake with no breakpoints of its
// own and waits for the replayed stop.
func joinAtEntry(t *testing.T, p *dapProc) {
	t.Helper()

	p.ok(initializeReq())
	p.ok(&godap.AttachRequest{Request: request("attach"), Arguments: []byte(`{}`)})
	p.waitEvent("initialized")
	p.ok(&godap.SetExceptionBreakpointsRequest{Request: request("setExceptionBreakpoints"), Arguments: godap.SetExceptionBreakpointsArguments{Filters: []string{}}})
	p.ok(&godap.ConfigurationDoneRequest{Request: request("configurationDone")})

	if stop, ok := p.waitEvent("stopped").(*godap.StoppedEvent); !ok || len(stop.Body.HitBreakpointIds) != 0 {
		t.Fatalf("replayed entry stop = %+v", stop)
	}
}
