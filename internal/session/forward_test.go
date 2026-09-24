// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"slices"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// dapRequest decodes a request the way the DAP facade does; args is its
// arguments' JSON ("" for none).
func dapRequest(t *testing.T, command, args string) godap.RequestMessage {
	t.Helper()

	raw := `{"seq":77,"type":"request","command":"` + command + `"`
	if args != "" {
		raw += `,"arguments":` + args
	}

	msg, err := godap.DecodeProtocolMessage([]byte(raw + "}"))
	if err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}

	req, ok := msg.(godap.RequestMessage)
	if !ok {
		t.Fatalf("%s decodes as %T", raw, msg)
	}

	return req
}

// allReadCaps are the fake's capabilities plus every one a forwarded read
// needs.
func allReadCaps() *godap.Capabilities {
	caps := daptest.DefaultCaps()
	caps.SupportsLoadedSourcesRequest, caps.SupportsModulesRequest, caps.SupportsBreakpointLocationsRequest = true, true, true
	caps.SupportsReadMemoryRequest, caps.SupportsDisassembleRequest, caps.SupportsCompletionsRequest = true, true, true

	return &caps
}

// TestForwardReads: reads reach the adapter (the fake's refusals prove
// it), those that need a stopped program are NOT_STOPPED while it runs,
// and refused commands never reach it.
func TestForwardReads(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, fakeDriver{opts: daptest.Options{Caps: allReadCaps()}})
	stopped := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	running := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"hang"}}})

	waitRunning(t, running)

	tests := []struct {
		command, args string
		stopped       api.Code // on the stopped session ("": success)
		running       api.Code // on the running one
	}{
		{command: "threads", running: ""},
		{command: "source", args: `{"sourceReference":1}`, stopped: api.CodeAdapterFailed, running: api.CodeAdapterFailed},
		{command: "loadedSources", stopped: api.CodeAdapterFailed, running: api.CodeAdapterFailed},
		{command: "modules", stopped: api.CodeAdapterFailed, running: api.CodeAdapterFailed},
		{command: "breakpointLocations", args: `{"source":{"path":"/p"},"line":1}`, stopped: api.CodeAdapterFailed, running: api.CodeAdapterFailed},
		{command: "readMemory", args: `{"memoryReference":"0x0","count":1}`, stopped: api.CodeAdapterFailed, running: api.CodeAdapterFailed},
		{command: "disassemble", args: `{"memoryReference":"0x0","instructionCount":1}`, stopped: api.CodeAdapterFailed, running: api.CodeAdapterFailed},
		{command: "stackTrace", args: `{"threadId":1}`, running: api.CodeNotStopped},
		{command: "scopes", args: `{"frameId":1}`, running: api.CodeNotStopped},
		{command: "variables", args: `{"variablesReference":1000}`, running: api.CodeNotStopped},
		{command: "exceptionInfo", args: `{"threadId":1}`, stopped: api.CodeAdapterFailed, running: api.CodeNotStopped},
		{command: "completions", args: `{"text":"x","column":1}`, stopped: api.CodeAdapterFailed, running: api.CodeNotStopped},
	}

	for _, refused := range []string{
		"restart", "stepBack", "reverseContinue", "restartFrame", "goto", "gotoTargets", "stepInTargets",
		"terminateThreads", "writeMemory", "dataBreakpointInfo", "setDataBreakpoints", "setInstructionBreakpoints",
		"cancel", "runInTerminal", "startDebugging", "initialize", "launch", "attach", "configurationDone",
		"disconnect", "terminate", "continue", "next", "stepIn", "stepOut", "pause", "setBreakpoints",
		"setFunctionBreakpoints", "setExceptionBreakpoints",
	} {
		tests = append(tests, struct {
			command, args string
			stopped       api.Code
			running       api.Code
		}{command: refused, stopped: api.CodeInvalidRequest, running: api.CodeInvalidRequest})
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			t.Parallel()

			for _, c := range []struct {
				s    *Session
				want api.Code
			}{{stopped, tt.stopped}, {running, tt.running}} {
				req := dapRequest(t, tt.command, tt.args)

				resp, err := c.s.Forward(t.Context(), humanC, req)
				if api.CodeOf(err) != c.want || (err == nil) != (c.want == "") {
					t.Errorf("%s on a %s session = %T, %v; want code %q", tt.command, c.s.Info().State, resp, err, c.want)
				}

				if req.GetRequest().Seq != 77 {
					t.Errorf("Forward changed the request's seq to %d", req.GetRequest().Seq)
				}
			}
		})
	}

	if n := len(eventsOf(stopped, api.EventExec)); n != 0 {
		t.Errorf("reads logged %d exec events", n)
	}
}

// TestForwardCompletionsColumn: completions needs a 1-based column (DAP
// columns start at 1, and the facade's caps declare columnsStartAt1).
// Omitted (go-dap's zero value), 0 and negative are refused before the
// adapter sees them: debugpy's completer treats column < 1 as license to
// eval the raw text, not just the dotted name before it. A column of at
// least 1 still reaches the adapter — the fake's refusal (ADAPTER_ERROR,
// not INVALID_REQUEST) proves it arrived.
func TestForwardCompletionsColumn(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManagerWith(t, nil, fakeDriver{opts: daptest.Options{Caps: allReadCaps()}}), agentC,
		api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	tests := []struct{ name, args string }{
		{name: "omitted", args: `{"text":"x"}`},
		{name: "zero", args: `{"text":"x","column":0}`},
		{name: "negative", args: `{"text":"x","column":-1}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := s.Forward(t.Context(), agentC, dapRequest(t, "completions", tt.args))
			expectCode(t, err, api.CodeInvalidRequest)
		})
	}

	t.Run("column 1 reaches the adapter", func(t *testing.T) {
		t.Parallel()

		_, err := s.Forward(t.Context(), agentC, dapRequest(t, "completions", `{"text":"x","column":1}`))
		expectCode(t, err, api.CodeAdapterFailed)
	})
}

// TestForwardUndeclared: a read whose capability the adapter didn't
// declare is UNSUPPORTED_BY_ADAPTER and never sent (the fake would refuse
// it: ADAPTER_ERROR); declared, it is sent.
func TestForwardUndeclared(t *testing.T) {
	t.Parallel()

	none := newTestManagerWith(t, nil, fakeDriver{opts: daptest.Options{Caps: &godap.Capabilities{SupportsConfigurationDoneRequest: true}}})
	undeclared := start(t, none, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})
	declared := start(t, newTestManagerWith(t, nil, fakeDriver{opts: daptest.Options{Caps: allReadCaps()}}), agentC,
		api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	tests := []struct{ command, args string }{
		{command: "loadedSources"},
		{command: "modules"},
		{command: "breakpointLocations", args: `{"source":{"path":"/p"},"line":1}`},
		{command: "readMemory", args: `{"memoryReference":"0x0","count":1}`},
		{command: "disassemble", args: `{"memoryReference":"0x0","instructionCount":1}`},
		{command: "exceptionInfo", args: `{"threadId":1}`},
		{command: "completions", args: `{"text":"x","column":1}`},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			t.Parallel()

			_, err := undeclared.Forward(t.Context(), humanC, dapRequest(t, tt.command, tt.args))
			expectCode(t, err, api.CodeUnsupported)

			_, err = declared.Forward(t.Context(), humanC, dapRequest(t, tt.command, tt.args))
			expectCode(t, err, api.CodeAdapterFailed)
		})
	}
}

// waitRunning waits until s runs (a hung fake program).
func waitRunning(t *testing.T, s *Session) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), testWait)
	defer cancel()

	if !s.waitUntil(ctx, func() bool { return s.state == api.StateRunning }) {
		t.Fatalf("session %s is %s, want running", s.ID, s.Info().State)
	}
}

// TestForwardEvaluate: read contexts are guarded reads sent with their
// context; repl or none is an execution request sent as repl; anything
// else is refused. Duplicate keys decide the same way for the policy and
// the adapter.
func TestForwardEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, args string
		client     api.Client
		want       string   // the adapter's answer
		wantCode   api.Code // instead
		exec       bool     // an exec eval event is logged
	}{
		{name: "watch", args: `{"expression":"$context","context":"watch","frameId":1}`, client: humanC, want: "watch"},
		{name: "hover", args: `{"expression":"$context","context":"hover","frameId":1}`, client: humanC, want: "hover"},
		{name: "variables", args: `{"expression":"$context","context":"variables","frameId":1}`, client: humanC, want: "variables"},
		{name: "clipboard, sent as watch", args: `{"expression":"$context","context":"clipboard","frameId":1}`, client: humanC, want: "watch"},
		{name: "repl", args: `{"expression":"$context","context":"repl"}`, client: agentC, want: "repl", exec: true},
		{name: "no context", args: `{"expression":"$context"}`, client: agentC, want: "repl", exec: true},
		{name: "repl of a non-holder", args: `{"expression":"$context","context":"repl"}`, client: humanC, wantCode: api.CodeLeaseHeld},
		{name: "unknown context", args: `{"expression":"$context","context":"foo"}`, client: agentC, wantCode: api.CodeInvalidRequest},
		{name: "side effects in watch", args: `{"expression":"f(1)","context":"watch","frameId":1}`, client: agentC, wantCode: api.CodeSideEffects},
		{name: "duplicate context, repl last", args: `{"expression":"$context","context":"watch","context":"repl"}`, client: agentC, want: "repl", exec: true},
		{name: "duplicate context, watch last", args: `{"expression":"$context","context":"repl","context":"watch","frameId":1}`, client: humanC, want: "watch"},
		{name: "watch without a frame", args: `{"expression":"$context","context":"watch"}`, client: humanC, wantCode: api.CodeInvalidRequest},
		{name: "hover in frame 0", args: `{"expression":"$context","context":"hover","frameId":0}`, client: humanC, wantCode: api.CodeInvalidRequest},
		{name: "clipboard without a frame", args: `{"expression":"$context","context":"clipboard"}`, client: agentC, wantCode: api.CodeInvalidRequest},
		{name: "duplicate context, watch last, no frame", args: `{"expression":"$context","context":"repl","context":"watch"}`, client: humanC, wantCode: api.CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := start(t, newTestManagerWith(t, nil, fakeDriver{sideEffects: callsCheck}), agentC, api.StartParams{
				LeasePolicy: api.LeaseHandoff, LaunchSpec: api.LaunchSpec{StopOnEntry: true},
			})

			resp, err := s.Forward(t.Context(), tt.client, dapRequest(t, "evaluate", tt.args))
			if tt.wantCode != "" {
				expectCode(t, err, tt.wantCode)
			} else if r, ok := resp.(*godap.EvaluateResponse); err != nil || !ok || r.Body.Result != tt.want {
				t.Errorf("evaluate = %+v, %v; want %q", resp, err, tt.want)
			}

			execs := eventsOf(s, api.EventExec)
			if tt.exec != (len(execs) == 1) || len(execs) > 1 {
				t.Fatalf("exec events = %+v, want one: %v", execs, tt.exec)
			}

			if tt.exec && (execs[0].Action != ExecEval || execs[0].Text != "$context" || execs[0].Client != tt.client.ID) {
				t.Errorf("exec event = %+v, want %s's eval of $context", execs[0], tt.client.ID)
			}
		})
	}
}

// TestForwardSet: setVariable and setExpression are execution requests
// whose exec event names the variable, never the value.
func TestForwardSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		command, args, text string
	}{
		{command: "setVariable", args: `{"variablesReference":1000,"name":"x","value":"41"}`, text: "x"},
		{command: "setExpression", args: `{"expression":"x","value":"41"}`, text: "x"},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			t.Parallel()

			s := start(t, newTestManager(t, nil), agentC, api.StartParams{
				LeasePolicy: api.LeaseHandoff, LaunchSpec: api.LaunchSpec{StopOnEntry: true},
			})

			if _, err := s.Changes(t.Context(), 0); err != nil {
				t.Fatal(err)
			}

			_, err := s.Forward(t.Context(), humanC, dapRequest(t, tt.command, tt.args))
			expectCode(t, err, api.CodeLeaseHeld)

			if n := len(eventsOf(s, api.EventExec)); n != 0 {
				t.Fatalf("a refused %s logged %d exec events", tt.command, n)
			}

			if _, err := s.Forward(t.Context(), agentC, dapRequest(t, tt.command, tt.args)); err != nil {
				t.Fatalf("%s: %v", tt.command, err)
			}

			execs := eventsOf(s, api.EventExec)
			if len(execs) != 1 || execs[0].Action != ExecSet || execs[0].Text != tt.text || execs[0].Client != agentC.ID {
				t.Errorf("exec events = %+v, want the agent's set of x without its value", execs)
			}

			scopes, err := s.Changes(t.Context(), 0)
			if err != nil || len(scopes) == 0 || !slices.ContainsFunc(scopes[0].Vars, func(v api.Var) bool { return v.Name == "x" && v.Value == "41" }) {
				t.Errorf("changes after %s = %+v, %v; want x = 41", tt.command, scopes, err)
			}
		})
	}
}
