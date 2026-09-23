// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

func setExceptions(t *testing.T, s *Session, c api.Client, mode api.ExceptionMode, force bool) api.ExceptionsResult {
	t.Helper()

	res, err := s.Exceptions(t.Context(), c, api.ExceptionsParams{Mode: mode, Force: force})
	if err != nil {
		t.Fatalf("%s: exceptions %s: %v", c.ID, mode, err)
	}

	return res
}

// TestExceptionModesMerge: each client has a mode, the adapter gets the
// union, none removes, --force replaces everyone's.
func TestExceptionModesMerge(t *testing.T) {
	t.Parallel()

	drv := fakeDriver{excFilters: map[api.ExceptionMode][]string{api.ExceptionsUncaught: {"user-unhandled"}}}
	s := start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{
		LaunchSpec: api.LaunchSpec{StopOnEntry: true}, Exceptions: api.ExceptionsUncaught,
	})

	if got := evalValue(t, s, "$filters"); got != "user-unhandled" {
		t.Fatalf("filters after --exceptions uncaught = %q", got)
	}

	res := setExceptions(t, s, humanC, api.ExceptionsAll, false)
	want := []api.ClientExceptionMode{{Client: agentC.ID, Mode: api.ExceptionsUncaught}, {Client: humanC.ID, Mode: api.ExceptionsAll}}

	if !reflect.DeepEqual(res.Modes, want) || !slices.Equal(res.Filters, []string{"user-unhandled", "all"}) {
		t.Fatalf("result = %+v, want both modes and both filters", res)
	}

	if got := evalValue(t, s, "$filters"); got != "user-unhandled,all" {
		t.Errorf("adapter filters = %q", got)
	}

	checkExceptionsCleared(t, s)
}

// checkExceptionsCleared: agent none removes the agent's mode, none
// --force everyone's.
func checkExceptionsCleared(t *testing.T, s *Session) {
	t.Helper()

	res := setExceptions(t, s, agentC, api.ExceptionsNone, false)
	if len(res.Modes) != 1 || res.Modes[0].Client != humanC.ID || evalValue(t, s, "$filters") != "all" {
		t.Errorf("after agent none: %+v, adapter %q", res, evalValue(t, s, "$filters"))
	}

	res = setExceptions(t, s, agentC, api.ExceptionsNone, true)
	if len(res.Modes) != 0 || len(res.Filters) != 0 || evalValue(t, s, "$filters") != "" {
		t.Errorf("after none --force: %+v, adapter %q", res, evalValue(t, s, "$filters"))
	}

	report, err := s.Exceptions(t.Context(), humanC, api.ExceptionsParams{})
	if err != nil || len(report.Modes) != 0 || report.Filters == nil {
		t.Errorf("report = %+v, %v; want empty lists", report, err)
	}

	events := eventsOf(s, api.EventExceptions)
	want := []string{"exceptions:uncaught", "exceptions:all", "exceptions:none", "exceptions:none"}
	if got := kindsAndActions(events); !slices.Equal(got, want) || events[0].Client != agentC.ID || events[3].Reason != "force" {
		t.Errorf("exceptions events = %v (%+v)", got, events)
	}
}

// TestExceptionModeWithoutFilter: a mode the driver maps to no filter the
// adapter has is refused, naming the adapter's filters.
func TestExceptionModeWithoutFilter(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	_, err := s.Exceptions(t.Context(), agentC, api.ExceptionsParams{Mode: api.ExceptionsUncaught})
	expectCode(t, err, api.CodeUnsupported)

	if e, ok := err.(*api.Error); !ok || !strings.Contains(e.Message, "all, user-unhandled") { //nolint:errorlint // Returned unwrapped.
		t.Errorf("error %v doesn't list the adapter's filters", err)
	}

	_, err = s.Exceptions(t.Context(), agentC, api.ExceptionsParams{Mode: "sometimes"})
	expectCode(t, err, api.CodeInvalidRequest)

	if len(eventsOf(s, api.EventExceptions)) != 0 {
		t.Error("a refused mode logged an event")
	}
}

// TestExceptionStop: with all, a throw stops the program and the snapshot
// describes the exception; without exceptionInfo there is none.
func TestExceptionStop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		opts     daptest.Options
		wantInfo bool
	}{
		{name: "with exceptionInfo", wantInfo: true},
		{name: "without", opts: daptest.Options{Caps: &godap.Capabilities{
			SupportsConfigurationDoneRequest: true,
			ExceptionBreakpointFilters:       []godap.ExceptionBreakpointsFilter{{Filter: "all"}},
		}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := start(t, newTestManagerWith(t, nil, fakeDriver{opts: tt.opts}), agentC, api.StartParams{
				LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"throws=3"}},
			})
			setExceptions(t, s, humanC, api.ExceptionsAll, false)

			snap := resume(t, s, agentC, ExecContinue)
			expectStopped(t, snap, reasonException, 3)

			if !tt.wantInfo {
				if snap.Exception != nil {
					t.Fatalf("exception = %+v, want none without exceptionInfo", snap.Exception)
				}

				return
			}

			e := snap.Exception
			if e == nil || e.ID != "Fake.Error" || e.Type != "Fake.Error" || e.Message != "fake failure at line 3" ||
				len(e.Inner) != 1 || e.Inner[0].Message != "inner cause" || e.StackTrace == "" {
				t.Fatalf("exception = %+v", e)
			}

			// Continuing completes the line: the exception was caught.
			if snap := resume(t, s, agentC, ExecContinue); snap.Session.State != api.StateExited {
				t.Errorf("after the caught exception: %+v, want exited", snap.Session)
			}
		})
	}
}
