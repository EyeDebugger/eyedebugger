// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// remoteProgram is where the debuggee of a mappedDriver session names its
// source: the program's directory is mapped to /src.
const remoteProgram = "/src/prog.txt"

// mappedDriver runs the fake program as if in a container: the fake
// adapter knows it as remoteProgram, and the session maps /src to the
// program's host directory.
type mappedDriver struct{}

func (mappedDriver) Name() string { return "fake" }

func (mappedDriver) Prepare(_ context.Context, spec session.LaunchSpec) (session.Launch, error) {
	path, args, env, err := daptest.CommandWith(daptest.Options{})
	if err != nil {
		return session.Launch{}, err
	}

	pm, err := session.NewPathMap([]api.PathMapping{{Remote: "/src", Local: filepath.Dir(spec.Program)}})
	if err != nil {
		return session.Launch{}, err
	}

	pa := daptest.ProgramArgs{Program: remoteProgram, Lines: 10, StopAtEntry: spec.StopOnEntry, Hang: true}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: pa.Map(), PathMap: pm,
	}, nil
}

// startMapped starts a mappedDriver session for the agent on a host
// program file of 10 lines, stopped at entry.
func startMapped(t *testing.T) *session.Session {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := session.NewManager(ctx, session.Config{Drivers: []session.Driver{mappedDriver{}}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	program := filepath.Join(dir, "prog.txt")
	if err := os.WriteFile(program, []byte(strings.Repeat("line\n", 10)), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", LaunchSpec: api.LaunchSpec{Program: program, StopOnEntry: true}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if snap := s.Wait(t.Context(), 0, testWait, api.DumpSpec{}); snap.Session.State != api.StateStopped {
		t.Fatalf("session = %+v, want stopped at entry", snap.Session)
	}

	return s
}

// Through the facade, an editor sees and sets host paths only: stackTrace
// frames come back mapped, a host-path setBreakpoints reaches the adapter
// at the container path, and a file outside the map is refused per entry.
func TestPathMapThroughFacade(t *testing.T) {
	t.Parallel()

	s := startMapped(t)
	tc := joinReady(t, s, humanC, "")

	st, ok := tc.ok("stackTrace", `{"threadId":1}`).(*godap.StackTraceResponse)
	if !ok || len(st.Body.StackFrames) == 0 || st.Body.StackFrames[0].Source == nil || st.Body.StackFrames[0].Source.Path != s.Program {
		t.Fatalf("stackTrace = %+v, want frame 0 at %s", st, s.Program)
	}

	if got := tc.setBPs(s.Program, "3"); !got[0].Verified {
		t.Errorf("setBreakpoints = %+v, want verified", got)
	}

	// $bps lists the breakpoints the fake adapter holds at its program's
	// path, remoteProgram: the breakpoint got there.
	if got := evalOK(t, s, "$bps"); got != "3" {
		t.Errorf("the adapter holds %q at %s, want 3", got, remoteProgram)
	}

	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("line\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := tc.setBPs(outside, "1"); got[0].Verified || !strings.Contains(got[0].Message, "outside the container's path map") {
		t.Errorf("setBreakpoints outside the map = %+v, want refused", got)
	}

	if got := evalOK(t, s, "$bps"); got != "3" {
		t.Errorf("after the refusal the adapter holds %q, want 3", got)
	}
}
