// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestCapabilities(t *testing.T) {
	t.Parallel()

	adapter := godap.Capabilities{
		SupportsConditionalBreakpoints: true, SupportsFunctionBreakpoints: true, SupportsEvaluateForHovers: true,
		SupportsSetVariable: true, SupportsCompletionsRequest: true, CompletionTriggerCharacters: []string{"."},
		SupportsRestartRequest: true, SupportsStepBack: true, SupportTerminateDebuggee: true, SupportSuspendDebuggee: true,
		SupportsDataBreakpoints: true, SupportsCancelRequest: true, SupportsSteppingGranularity: true,
		SupportsExceptionOptions: true, SupportsExceptionInfoRequest: true, SupportsClipboardContext: true,
		ExceptionBreakpointFilters: []godap.ExceptionBreakpointsFilter{{Filter: "raised"}},
	}

	tests := []struct {
		name    string
		modes   []api.ExceptionMode
		filters []string
	}{
		{name: "no modes", modes: nil, filters: nil},
		{name: "all", modes: []api.ExceptionMode{api.ExceptionsAll}, filters: []string{"all"}},
		{name: "both", modes: []api.ExceptionMode{api.ExceptionsAll, api.ExceptionsUncaught}, filters: []string{"all", "uncaught"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := capabilities(adapter, tt.modes)

			want := godap.Capabilities{
				SupportsConfigurationDoneRequest: true, SupportsHitConditionalBreakpoints: true, SupportsLogPoints: true,
				SupportsTerminateRequest: true,
				// Passed through.
				SupportsConditionalBreakpoints: true, SupportsFunctionBreakpoints: true, SupportsEvaluateForHovers: true,
				SupportsSetVariable: true, SupportsCompletionsRequest: true, CompletionTriggerCharacters: []string{"."},
				SupportsExceptionInfoRequest: true,
			}

			for _, f := range tt.filters {
				label := map[string]string{"all": "All exceptions", "uncaught": "Uncaught exceptions"}[f]
				want.ExceptionBreakpointFilters = append(want.ExceptionBreakpointFilters, godap.ExceptionBreakpointsFilter{Filter: f, Label: label})
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("capabilities =\n%+v\nwant\n%+v", got, want)
			}
		})
	}

	if adapter.ExceptionBreakpointFilters[0].Filter != "raised" {
		t.Error("capabilities changed the adapter's filters")
	}
}

func TestExceptionMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		filters []string
		want    api.ExceptionMode
	}{
		{filters: nil, want: api.ExceptionsNone},
		{filters: []string{"bogus"}, want: api.ExceptionsNone},
		{filters: []string{"uncaught"}, want: api.ExceptionsUncaught},
		{filters: []string{"uncaught", "all"}, want: api.ExceptionsAll},
		{filters: []string{"all", "uncaught"}, want: api.ExceptionsAll},
	}

	for _, tt := range tests {
		if got := exceptionMode(tt.filters); got != tt.want {
			t.Errorf("exceptionMode(%v) = %s, want %s", tt.filters, got, tt.want)
		}
	}
}

// encode renders DAP events as JSON for comparing.
func encode(t *testing.T, events []godap.EventMessage) string {
	t.Helper()

	parts := make([]string, 0, len(events))

	for _, ev := range events {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}

		parts = append(parts, string(raw))
	}

	return strings.Join(parts, "\n")
}

func TestTranslate(t *testing.T) {
	t.Parallel()

	code := 3
	own := &api.Breakpoint{ID: 4, Owner: "human:t", Verified: true, Line: 7, Message: "moved", Note: "shared"}
	agents := &api.Breakpoint{ID: 6, Owner: "agent", Line: 9}

	tests := []struct {
		name        string
		ev          api.Event
		invalidated bool
		want        string
	}{
		{
			name: "stopped",
			ev:   api.Event{Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "breakpoint", ThreadID: 2, Description: "d", Text: "x", AllThreadsStopped: true}},
			want: `{"seq":0,"type":"","event":"stopped","body":{"reason":"breakpoint","description":"d","threadId":2,"text":"x","allThreadsStopped":true}}`,
		},
		{name: "own exec", ev: api.Event{Kind: api.EventExec, Client: "human:t", Action: "continue"}, want: ""},
		{
			name: "other's continue", ev: api.Event{Kind: api.EventExec, Client: "agent", Action: "continue"},
			want: `{"seq":0,"type":"","event":"continued","body":{"threadId":1,"allThreadsContinued":true}}`,
		},
		{
			name: "other's step on a thread", ev: api.Event{Kind: api.EventExec, Client: "agent", Action: "next", ThreadID: 9},
			want: `{"seq":0,"type":"","event":"continued","body":{"threadId":9,"allThreadsContinued":true}}`,
		},
		{
			name: "other's run-until", ev: api.Event{Kind: api.EventExec, Client: "agent", Action: "runUntil"},
			want: `{"seq":0,"type":"","event":"continued","body":{"threadId":1,"allThreadsContinued":true}}`,
		},
		{name: "other's pause", ev: api.Event{Kind: api.EventExec, Client: "agent", Action: "pause"}, want: ""},
		{name: "other's set, no invalidated", ev: api.Event{Kind: api.EventExec, Client: "agent", Action: "set"}, want: ""},
		{
			name: "other's eval, invalidated", ev: api.Event{Kind: api.EventExec, Client: "agent", Action: "eval"}, invalidated: true,
			want: `{"seq":0,"type":"","event":"invalidated","body":{"areas":["variables"]}}`,
		},
		{
			name: "adapter continued", ev: api.Event{Kind: api.EventContinued, ThreadID: 3},
			want: `{"seq":0,"type":"","event":"continued","body":{"threadId":3,"allThreadsContinued":true}}`,
		},
		{
			name: "logpoint output", ev: api.Event{Kind: api.EventOutput, Category: "logpoint", Text: "hit {x}\n"},
			want: `{"seq":0,"type":"","event":"output","body":{"category":"console","output":"hit {x}\n"}}`,
		},
		{
			name: "stdout", ev: api.Event{Kind: api.EventOutput, Category: "stdout", Text: "a"},
			want: `{"seq":0,"type":"","event":"output","body":{"category":"stdout","output":"a"}}`,
		},
		{
			name: "thread", ev: api.Event{Kind: api.EventThread, Reason: "started", ThreadID: 4},
			want: `{"seq":0,"type":"","event":"thread","body":{"reason":"started","threadId":4}}`,
		},
		{name: "exited", ev: api.Event{Kind: api.EventExited, ExitCode: &code}, want: `{"seq":0,"type":"","event":"exited","body":{"exitCode":3}}`},
		{name: "ended", ev: api.Event{Kind: api.EventEnded, Reason: "stopped by agent"}, want: `{"seq":0,"type":"","event":"terminated","body":{}}`},
		// Breakpoint events reconcile the connection's view instead
		// (mirrors_test.go).
		{name: "adapter changed own", ev: api.Event{Kind: api.EventBreakpoint, Action: "changed", Breakpoint: own}, want: ""},
		{name: "forced removal of own", ev: api.Event{Kind: api.EventBreakpoint, Action: "removed", Client: "agent", Breakpoint: own}, want: ""},
		{name: "added", ev: api.Event{Kind: api.EventBreakpoint, Action: "added", Client: "agent", Breakpoint: agents}, want: ""},
		{name: "started", ev: api.Event{Kind: api.EventStarted}, want: ""},
		{name: "client", ev: api.Event{Kind: api.EventClient, Client: "agent"}, want: ""},
		{name: "lease", ev: api.Event{Kind: api.EventLease, Action: "grant"}, want: ""},
		{name: "exceptions", ev: api.Event{Kind: api.EventExceptions, Action: "all"}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st := &followState{self: "human:t", invalidated: tt.invalidated, lastThread: 1}
			if got := encode(t, translate(&tt.ev, st)); got != tt.want {
				t.Errorf("translate =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}

	covered := map[api.EventKind]bool{}
	for i := range tests {
		covered[tests[i].ev.Kind] = true
	}

	for _, k := range api.EventKinds() {
		if !covered[k] {
			t.Errorf("no translate case for event kind %s", k)
		}
	}
}

// TestStopFocusHint: a stop carries preserveFocusHint when the latest
// execution request before it came from another client.
func TestStopFocusHint(t *testing.T) {
	t.Parallel()

	exec := func(client, action string) api.Event {
		return api.Event{Kind: api.EventExec, Client: client, Action: action}
	}
	stop := api.Event{Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "step", ThreadID: 1}}

	tests := []struct {
		name   string
		events []api.Event
		want   []bool // the hint of each stop, in order
	}{
		{name: "other's next", events: []api.Event{exec("agent", "next"), stop}, want: []bool{true}},
		{name: "own next", events: []api.Event{exec("human:t", "next"), stop}, want: []bool{false}},
		{name: "other's pause", events: []api.Event{exec("agent", "pause"), stop}, want: []bool{true}},
		{name: "other's run-until", events: []api.Event{exec("agent", "runUntil"), stop}, want: []bool{true}},
		{name: "other's eval", events: []api.Event{exec("agent", "eval"), stop}, want: []bool{false}},
		{name: "other's set after own step", events: []api.Event{exec("human:t", "stepIn"), exec("agent", "set"), stop}, want: []bool{false}},
		{name: "latest wins", events: []api.Event{exec("agent", "next"), exec("human:t", "continue"), stop}, want: []bool{false}},
		{name: "other's pause after own continue", events: []api.Event{exec("human:t", "continue"), exec("agent", "pause"), stop}, want: []bool{true}},
		{name: "no exec", events: []api.Event{stop}, want: []bool{false}},
		{name: "second stop", events: []api.Event{exec("agent", "stepOut"), stop, stop}, want: []bool{true, false}},
		{name: "each stop its own", events: []api.Event{exec("agent", "continue"), stop, exec("human:t", "next"), stop, exec("agent", "next"), stop}, want: []bool{true, false, true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := stopHints(tt.events); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("hints = %v, want %v", got, tt.want)
			}
		})
	}
}

// stopHints translates events for the client human:t and returns the
// preserveFocusHint of each stop sent.
func stopHints(events []api.Event) []bool {
	st := &followState{self: "human:t", lastThread: 1}

	var hints []bool

	for i := range events {
		for _, ev := range translate(&events[i], st) {
			if s, ok := ev.(*godap.StoppedEvent); ok {
				hints = append(hints, s.Body.PreserveFocusHint)
			}
		}
	}

	return hints
}

// TestStopFocusHintWire: the hint's JSON, and a resync (the cause of its
// stop is unknown) sends none and forgets the last execution request.
func TestStopFocusHintWire(t *testing.T) {
	t.Parallel()

	next := api.Event{Kind: api.EventExec, Client: "agent", Action: "next"}
	stop := api.Event{Kind: api.EventStopped, Stop: &api.StopInfo{Reason: "step", ThreadID: 1}}

	st := &followState{self: "human:t", lastThread: 1}
	translate(&next, st)

	want := `{"seq":0,"type":"","event":"stopped","body":{"reason":"step","threadId":1,"preserveFocusHint":true}}`
	if got := encode(t, translate(&stop, st)); got != want {
		t.Errorf("stopped =\n%s\nwant\n%s", got, want)
	}

	translate(&next, st)

	want = `{"seq":0,"type":"","event":"output","body":{"category":"console","output":"eyedbg: 1 session events were missed\n"}}` + "\n" +
		`{"seq":0,"type":"","event":"stopped","body":{"reason":"step","threadId":1}}`
	if got := encode(t, resync(1, api.SessionInfo{State: api.StateStopped, Stop: stop.Stop}, st)); got != want {
		t.Errorf("resync =\n%s\nwant\n%s", got, want)
	}

	if got := encode(t, translate(&stop, st)); got != `{"seq":0,"type":"","event":"stopped","body":{"reason":"step","threadId":1}}` {
		t.Errorf("stop after the resync = %s, want no preserveFocusHint", got)
	}
}

func TestResync(t *testing.T) {
	t.Parallel()

	notice := `{"seq":0,"type":"","event":"output","body":{"category":"console","output":"eyedbg: 7 session events were missed\n"}}`

	tests := []struct {
		name string
		info api.SessionInfo
		want string
	}{
		{
			name: "stopped", info: api.SessionInfo{State: api.StateStopped, Stop: &api.StopInfo{Reason: "step", ThreadID: 2}},
			want: notice + "\n" + `{"seq":0,"type":"","event":"stopped","body":{"reason":"step","threadId":2}}`,
		},
		{
			name: "running", info: api.SessionInfo{State: api.StateRunning},
			want: notice + "\n" + `{"seq":0,"type":"","event":"continued","body":{"threadId":1,"allThreadsContinued":true}}`,
		},
		{name: "exited", info: api.SessionInfo{State: api.StateExited}, want: notice},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			st := &followState{self: "human:t", lastThread: 1}
			if got := encode(t, resync(7, tt.info, st)); got != tt.want {
				t.Errorf("resync =\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}

func TestErrorResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		err      error
		holder   string
		showUser bool
		message  string
		want     godap.ErrorMessage
	}{
		{
			name: "lease held", err: api.NewError(api.CodeLeaseHeld, "held by agent", "ask agent"), holder: "agent", showUser: true,
			message: "LEASE_HELD",
			want:    godap.ErrorMessage{Id: 7006, Format: "held by agent — ask agent", ShowUser: true, Variables: map[string]string{"code": "LEASE_HELD", "holder": "agent"}},
		},
		{
			name: "no hint", err: api.NewError(api.CodeNotStopped, "running", ""), holder: "agent",
			message: "NOT_STOPPED",
			want:    godap.ErrorMessage{Id: 7003, Format: "running", Variables: map[string]string{"code": "NOT_STOPPED"}},
		},
		{
			name: "placeholder kept", err: api.NewError(api.CodeInvalidRequest, "invalid log message {x", ""),
			message: "INVALID_REQUEST",
			want:    godap.ErrorMessage{Id: 7001, Format: "invalid log message {x", Variables: map[string]string{"code": "INVALID_REQUEST"}},
		},
		{
			name: "wrapped", err: errors.Join(errors.New("ctx"), api.NewError(api.CodeAdapterFailed, "rejected", "")),
			message: "ADAPTER_ERROR",
			want:    godap.ErrorMessage{Id: 7010, Format: "rejected", Variables: map[string]string{"code": "ADAPTER_ERROR"}},
		},
		{
			name: "other code", err: api.NewError(api.CodeAnchorNotFound, "no anchor", ""),
			message: "ANCHOR_NOT_FOUND",
			want:    godap.ErrorMessage{Id: 7000, Format: "no anchor", Variables: map[string]string{"code": "ANCHOR_NOT_FOUND"}},
		},
		{
			name: "internal", err: errors.New("secret value 42"),
			message: "INTERNAL",
			want: godap.ErrorMessage{
				Id: 7099, Format: "eyedbg failed to handle evaluate — see 'eyedbg daemon logs'",
				Variables: map[string]string{"code": "INTERNAL"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			message, body := errorResponse("evaluate", tt.err, tt.holder, tt.showUser)
			if message != tt.message || !reflect.DeepEqual(*body, tt.want) {
				t.Errorf("errorResponse = %q, %+v; want %q, %+v", message, *body, tt.message, tt.want)
			}
		})
	}

	for _, id := range []int{
		errorID(api.CodeInvalidRequest), errorID(api.CodeNoSession), errorID(api.CodeNotStopped), errorID(api.CodeNotRunning),
		errorID(api.CodeSessionExited), errorID(api.CodeLeaseHeld), errorID(api.CodeNotOwner), errorID(api.CodeUnsupported),
		errorID(api.CodeSideEffects), errorID(api.CodeAdapterFailed),
	} {
		if id < 7001 || id > 7010 {
			t.Errorf("error id %d out of the fixed range", id)
		}
	}
}

// TestAbandoned: only an error that is the connection context's own end
// goes unanswered; everything else, a context error while the connection
// lives included, is still answered (INTERNAL when it has no code).
func TestAbandoned(t *testing.T) {
	t.Parallel()

	ended, cancel := context.WithCancel(context.Background())
	cancel()

	canceled := fmt.Errorf("threads: %w", context.Canceled)

	tests := []struct {
		name string
		ctx  context.Context //nolint:containedctx // test input
		err  error
		want bool
	}{
		{name: "canceled by the connection's end", ctx: ended, err: canceled, want: true},
		{name: "wrapped twice", ctx: ended, err: fmt.Errorf("forward: %w", canceled), want: true},
		{name: "connection live", ctx: context.Background(), err: canceled, want: false},
		{name: "own timeout", ctx: ended, err: fmt.Errorf("threads: %w", context.DeadlineExceeded), want: false},
		{name: "other error", ctx: ended, err: errors.New("unexpected response type"), want: false},
		{name: "coded", ctx: ended, err: errors.Join(canceled, api.NewError(api.CodeSessionExited, "exited", "")), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := abandoned(tt.ctx, tt.err); got != tt.want {
				t.Errorf("abandoned = %v, want %v", got, tt.want)
			}
		})
	}
}
