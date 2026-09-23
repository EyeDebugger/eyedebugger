// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

func (s *session) try(req godap.RequestMessage) (godap.Message, error) {
	ctx, cancel := context.WithTimeout(s.t.Context(), 10*time.Second)
	defer cancel()

	return s.c.Do(ctx, req)
}

func (s *session) cont() {
	s.t.Helper()
	s.do(&godap.ContinueRequest{Request: godap.Request{Command: "continue"}})
}

func (s *session) evalCtx(expr, evalContext string) string {
	s.t.Helper()

	resp, ok := s.do(&godap.EvaluateRequest{
		Request:   godap.Request{Command: "evaluate"},
		Arguments: godap.EvaluateArguments{Expression: expr, Context: evalContext},
	}).(*godap.EvaluateResponse)
	if !ok {
		s.t.Fatal("evaluate: unexpected response")
	}

	return resp.Body.Result
}

func (s *session) filters(names ...string) error {
	_, err := s.try(&godap.SetExceptionBreakpointsRequest{
		Request:   godap.Request{Command: "setExceptionBreakpoints"},
		Arguments: godap.SetExceptionBreakpointsArguments{Filters: names},
	})

	return err
}

func (s *session) functions(names ...string) ([]godap.Breakpoint, error) {
	fbs := make([]godap.FunctionBreakpoint, len(names))
	for i, n := range names {
		fbs[i] = godap.FunctionBreakpoint{Name: n}
	}

	msg, err := s.try(&godap.SetFunctionBreakpointsRequest{
		Request:   godap.Request{Command: "setFunctionBreakpoints"},
		Arguments: godap.SetFunctionBreakpointsArguments{Breakpoints: fbs},
	})
	if err != nil {
		return nil, err
	}

	resp, ok := msg.(*godap.SetFunctionBreakpointsResponse)
	if !ok {
		s.t.Fatal("setFunctionBreakpoints: unexpected response")
	}

	return resp.Body.Breakpoints, nil
}

func TestFakeAdapterBreadth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts daptest.Options
		run  func(s *session)
	}{
		{name: "laps", run: fakeLaps},
		{name: "function breakpoints", run: fakeFunctionBreakpoints},
		{name: "exception filters", run: fakeExceptionFilters},
		{name: "set and context", run: fakeSetAndContext},
		{name: "step hits breakpoints", opts: daptest.Options{StepHitsBreakpoints: true}, run: fakeStepHitsBreakpoints},
		{name: "attach", run: fakeAttach},
		{name: "attach fails", run: fakeAttachFails},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.run(newSessionWith(t, tt.opts))
		})
	}
}

func fakeLaps(s *session) {
	s.t.Helper()

	s.launchWith(daptest.ProgramArgs{Program: prog, Lines: 3, Laps: 3}, nil, godap.SourceBreakpoint{Line: 2, Condition: "lap == 2"})
	s.expectStop("breakpoint", 2)

	if got := s.eval("lap"); got != "2" {
		s.t.Fatalf("lap = %s, want 2", got)
	}

	if got := s.eval("lap == 2"); got != "true" {
		s.t.Errorf("lap == 2 evaluates to %s", got)
	}

	if got := s.eval("line == 3"); got != "false" {
		s.t.Errorf("line == 3 evaluates to %s", got)
	}
}

func fakeFunctionBreakpoints(s *session) {
	s.t.Helper()

	s.launchWith(daptest.ProgramArgs{Program: prog, Lines: 5}, func() {
		got, err := s.functions("f4", "nope")
		if err != nil || len(got) != 2 || !got[0].Verified || got[1].Verified || got[1].Message != "no function nope" {
			s.t.Fatalf("setFunctionBreakpoints = %+v, %v", got, err)
		}

		var reqErr *dap.RequestError
		if _, err := s.functions("f2", "f2"); !errors.As(err, &reqErr) {
			s.t.Errorf("duplicate names: err = %v, want a rejected request", err)
		}

		if _, err := s.functions("f4"); err != nil {
			s.t.Fatal(err)
		}
	})
	s.expectStop("breakpoint", 4)

	if got := s.eval("$fbps"); got != "f4" {
		s.t.Errorf("$fbps = %q", got)
	}
}

func fakeExceptionFilters(s *session) {
	s.t.Helper()

	s.launchWith(daptest.ProgramArgs{Program: prog, Lines: 5, Throws: []int{2, 4}}, func() {
		var reqErr *dap.RequestError
		if err := s.filters("bogus"); !errors.As(err, &reqErr) {
			s.t.Errorf("unknown filter: err = %v, want a rejected request", err)
		}

		if err := s.filters("all"); err != nil {
			s.t.Fatal(err)
		}
	})

	e, ok := s.next("continued", "process", "thread", "initialized").(*godap.StoppedEvent)
	if !ok || e.Body.Reason != "exception" || !strings.Contains(e.Body.Text, "Fake.Error") {
		s.t.Fatalf("event = %+v, want an exception stop", e)
	}

	resp, ok := s.do(&godap.ExceptionInfoRequest{Request: godap.Request{Command: "exceptionInfo"}}).(*godap.ExceptionInfoResponse)
	if !ok || resp.Body.ExceptionId != "Fake.Error" || resp.Body.Details == nil || resp.Body.Details.Message != "fake failure at line 2" {
		s.t.Fatalf("exceptionInfo = %+v", resp)
	}

	s.cont()
	s.expectStop("exception", 4)

	if got := s.eval("$filters"); got != "all" {
		s.t.Errorf("$filters = %q", got)
	}
}

func fakeSetAndContext(s *session) {
	s.t.Helper()

	s.launch(3, true, false)
	s.expectStop("entry", 1)

	s.do(&godap.SetExpressionRequest{Request: godap.Request{Command: "setExpression"}, Arguments: godap.SetExpressionArguments{Expression: "x", Value: "5"}})

	if got := s.eval("x"); got != "5" {
		s.t.Fatalf("x = %s after setExpression, want 5", got)
	}

	s.do(&godap.SetVariableRequest{Request: godap.Request{Command: "setVariable"}, Arguments: godap.SetVariableArguments{VariablesReference: 1000, Name: "x", Value: "7"}})

	if got := s.eval("x"); got != "7" {
		s.t.Fatalf("x = %s after setVariable, want 7", got)
	}

	if got := s.evalCtx("$context", "repl"); got != "repl" {
		s.t.Errorf("$context = %q, want repl", got)
	}
}

func fakeStepHitsBreakpoints(s *session) {
	s.t.Helper()

	s.launch(4, true, false, godap.SourceBreakpoint{Line: 2})
	s.expectStop("entry", 1)
	s.do(&godap.NextRequest{Request: godap.Request{Command: "next"}})
	s.expectStop("breakpoint", 2)
}

func fakeAttach(s *session) {
	s.t.Helper()

	attach(s, false)
	s.do(&godap.ConfigurationDoneRequest{Request: godap.Request{Command: "configurationDone"}})

	if e, ok := s.next("continued", "thread", "initialized").(*godap.StoppedEvent); !ok || e.Body.Reason != "breakpoint" {
		s.t.Fatalf("event = %+v, want a breakpoint stop (and no process event)", e)
	}

	s.do(&godap.DisconnectRequest{Request: godap.Request{Command: "disconnect"}, Arguments: &godap.DisconnectArguments{TerminateDebuggee: true}})
	expectOutput(s, "fake: disconnect terminateDebuggee=true\n")
}

func fakeAttachFails(s *session) {
	s.t.Helper()

	attach(s, true)

	_, err := s.try(&godap.ConfigurationDoneRequest{Request: godap.Request{Command: "configurationDone"}})
	if err == nil || !strings.Contains(err.Error(), "0x80070057") {
		s.t.Fatalf("configurationDone err = %v, want netcoredbg's attach failure", err)
	}
}

// attach initializes, attaches to a fake program with a breakpoint at 2.
func attach(s *session, fail bool) {
	s.t.Helper()

	s.do(&godap.InitializeRequest{Request: godap.Request{Command: "initialize"}})

	args, err := json.Marshal(daptest.AttachArguments(prog, 3, 1, fail))
	if err != nil {
		s.t.Fatal(err)
	}

	s.do(&godap.AttachRequest{Request: godap.Request{Command: "attach"}, Arguments: args})

	if err := s.setBreakpoints(godap.SourceBreakpoint{Line: 2}); err != nil {
		s.t.Fatal(err)
	}
}

// expectOutput waits for an output event with text.
func expectOutput(s *session, text string) {
	s.t.Helper()

	for {
		select {
		case e := <-s.events:
			if o, ok := e.(*godap.OutputEvent); ok && o.Body.Output == text {
				return
			}
		case <-time.After(10 * time.Second):
			s.t.Fatalf("no output %q", text)
		}
	}
}

func TestFakeRunner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode     string
		code     int
		wantPID  bool
		wantLine string
	}{
		{mode: daptest.RunnerNoPID, code: 1, wantLine: "error CS1002"},
		{mode: daptest.RunnerHang, wantPID: true},
		{mode: daptest.RunnerExit, code: 3, wantPID: true, wantLine: "Passed!"},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			t.Parallel()

			path, args, env, err := daptest.RunnerCommand(tt.mode, tt.code)
			if err != nil {
				t.Fatal(err)
			}

			cmd := exec.CommandContext(t.Context(), path, args...)
			cmd.Env = append(os.Environ(), env...)

			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}

			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			sawPID, sawLine := readRunner(t, cmd, out, tt.mode, tt.wantLine)

			err = cmd.Wait()
			if sawPID != tt.wantPID || tt.wantLine != "" && !sawLine {
				t.Fatalf("pid line %v (want %v), %q seen %v", sawPID, tt.wantPID, tt.wantLine, sawLine)
			}

			if tt.mode != daptest.RunnerHang && cmd.ProcessState.ExitCode() != tt.code {
				t.Fatalf("exit = %v, want code %d", err, tt.code)
			}
		})
	}
}

// attachTo attaches a fake adapter to the fake runner pid and configures
// it, which connects it to the runner's gate.
func attachTo(s *session, pid int) {
	s.t.Helper()

	s.do(&godap.InitializeRequest{Request: godap.Request{Command: "initialize"}})

	args, err := json.Marshal(daptest.AttachArguments(prog, 1, pid, false))
	if err != nil {
		s.t.Fatal(err)
	}

	s.do(&godap.AttachRequest{Request: godap.Request{Command: "attach"}, Arguments: args})
	s.do(&godap.ConfigurationDoneRequest{Request: godap.Request{Command: "configurationDone"}})
	s.do(&godap.DisconnectRequest{Request: godap.Request{Command: "disconnect"}})
}

// readRunner reads a fake runner's output, killing a hanging one and
// attaching to a waiting one once it printed its pid line.
func readRunner(t *testing.T, cmd *exec.Cmd, out io.Reader, mode, wantLine string) (sawPID, sawLine bool) {
	t.Helper()

	sc := bufio.NewScanner(out)
	pidLine := "Process Id: " + strconv.Itoa(cmd.Process.Pid) + ", Name: testhost"

	for sc.Scan() {
		if sc.Text() == pidLine {
			sawPID = true

			switch mode {
			case daptest.RunnerHang:
				_ = cmd.Process.Kill()
			case daptest.RunnerExit:
				// Stand in for the session: attach, then go.
				attachTo(newSession(t), cmd.Process.Pid)
			}
		}

		sawLine = sawLine || wantLine != "" && strings.Contains(sc.Text(), wantLine)
	}

	return sawPID, sawLine
}
