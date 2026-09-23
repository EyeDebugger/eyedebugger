// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	godap "github.com/google/go-dap"
)

// EnvFakeAdapter is the variable that, set to "1", makes [MaybeRun] serve DAP
// on stdin and stdout.
const EnvFakeAdapter = "EYEDBG_TEST_FAKE_ADAPTER"

// MaybeRun serves DAP on stdin and stdout until the client disconnects or
// closes stdin, and returns true, if this process was started by [Command].
// Otherwise it returns false at once. Call it first in TestMain and return
// when it returns true.
func MaybeRun() bool {
	if os.Getenv(EnvFakeAdapter) != "1" {
		return false
	}

	if err := Serve(os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "fake adapter:", err)
	}

	return true
}

// Command returns how to start the fake adapter: this test binary, with no
// tests to run and [EnvFakeAdapter] set.
func Command() (path string, args, env []string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, nil, fmt.Errorf("locate the test binary: %w", err)
	}

	return exe, []string{"-test.run=^$"}, []string{EnvFakeAdapter + "=1"}, nil
}

// Arguments returns the launch arguments for a fake program: lines 1 to n
// of the file program. stopAtEntry stops at line 1; hang keeps it running
// after its last line instead of exiting.
func Arguments(program string, n int, stopAtEntry, hang bool) map[string]any {
	return map[string]any{"program": program, "lines": n, "stopAtEntry": stopAtEntry, "hang": hang}
}

// launchArgs is the body of the launch request [Arguments] builds.
type launchArgs struct {
	Program     string `json:"program"`
	Lines       int    `json:"lines"`
	StopAtEntry bool   `json:"stopAtEntry"`
	Hang        bool   `json:"hang"`
}

// response and event are DAP messages with any body.
type response struct {
	godap.Response

	Body any `json:"body,omitempty"`
}

type event struct {
	godap.Event

	Body any `json:"body,omitempty"`
}

// adapter answers one client's requests, one at a time.
type adapter struct {
	w           io.Writer
	seq         int
	err         error // first write error; ends Serve
	done        bool  // disconnected
	prog        *program
	stopAtEntry bool
}

// Serve speaks DAP on r and w until the client disconnects or r ends.
func Serve(r io.Reader, w io.Writer) error {
	a := &adapter{w: w}
	a.prog = newProgram(a.emit)
	br := bufio.NewReader(r)

	for !a.done && a.err == nil {
		msg, err := godap.ReadProtocolMessage(br)
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read request: %w", err)
		}

		if req, ok := msg.(godap.RequestMessage); ok {
			a.handle(req)
		}
	}

	return a.err
}

func (a *adapter) write(m godap.Message) {
	if a.err == nil {
		a.err = godap.WriteProtocolMessage(a.w, m)
	}
}

func (a *adapter) emit(name string, body any) {
	a.seq++
	a.write(&event{Event: godap.Event{ProtocolMessage: godap.ProtocolMessage{Seq: a.seq, Type: "event"}, Event: name}, Body: body})
}

func (a *adapter) respond(req godap.RequestMessage, body any) {
	a.seq++
	r := req.GetRequest()
	a.write(&response{
		Response: godap.Response{
			ProtocolMessage: godap.ProtocolMessage{Seq: a.seq, Type: "response"},
			RequestSeq:      r.Seq, Success: true, Command: r.Command,
		},
		Body: body,
	})
}

func (a *adapter) fail(req godap.RequestMessage, msg string) {
	a.seq++
	r := req.GetRequest()
	a.write(&response{
		Response: godap.Response{
			ProtocolMessage: godap.ProtocolMessage{Seq: a.seq, Type: "response"},
			RequestSeq:      r.Seq, Command: r.Command, Message: msg,
		},
		Body: godap.ErrorResponseBody{Error: &godap.ErrorMessage{Format: msg}},
	})
}

func (a *adapter) handle(req godap.RequestMessage) {
	switch r := req.(type) {
	case *godap.InitializeRequest:
		a.respond(req, godap.Capabilities{SupportsConfigurationDoneRequest: true, SupportsConditionalBreakpoints: true})
	case *godap.LaunchRequest:
		a.launch(r)
	case *godap.SetBreakpointsRequest:
		a.setBreakpoints(r)
	case *godap.ConfigurationDoneRequest:
		a.respond(req, nil)
		a.prog.start(a.stopAtEntry)
	case *godap.ContinueRequest, *godap.NextRequest, *godap.StepInRequest, *godap.StepOutRequest:
		a.resume(req)
	case *godap.PauseRequest:
		a.respond(req, nil)
		a.prog.pause()
	case *godap.EvaluateRequest:
		a.evaluate(r)
	case *godap.DisconnectRequest:
		a.respond(req, nil)
		a.emit("terminated", nil)
		a.done = true
	default:
		a.inspect(req)
	}
}

// inspect answers the requests that only read state.
func (a *adapter) inspect(req godap.RequestMessage) {
	switch req.(type) {
	case *godap.SetExceptionBreakpointsRequest:
		a.respond(req, nil)
	case *godap.ThreadsRequest:
		a.respond(req, godap.ThreadsResponseBody{Threads: []godap.Thread{{Id: threadID, Name: "main"}}})
	case *godap.StackTraceRequest:
		a.respond(req, godap.StackTraceResponseBody{StackFrames: []godap.StackFrame{a.prog.frame()}, TotalFrames: 1})
	case *godap.ScopesRequest:
		a.respond(req, godap.ScopesResponseBody{Scopes: []godap.Scope{{Name: "Locals", VariablesReference: localsRef}}})
	case *godap.VariablesRequest:
		a.respond(req, godap.VariablesResponseBody{Variables: a.prog.locals()})
	default:
		a.fail(req, req.GetRequest().Command+" is not supported by the fake adapter")
	}
}

func (a *adapter) launch(req *godap.LaunchRequest) {
	var args launchArgs
	if err := json.Unmarshal(req.Arguments, &args); err != nil || args.Program == "" || args.Lines < 1 {
		a.fail(req, "invalid launch arguments: want program and lines")

		return
	}

	a.prog.load(args.Program, args.Lines, args.Hang)
	a.stopAtEntry = args.StopAtEntry
	a.respond(req, nil)
	// Only now: configuration must not start before the launch arrived
	// (the session sends launch without waiting for its answer).
	a.emit("initialized", nil)
}

func (a *adapter) setBreakpoints(req *godap.SetBreakpointsRequest) {
	got, err := a.prog.setBreakpoints(req.Arguments.Source.Path, req.Arguments.Breakpoints)
	if err != nil {
		a.fail(req, err.Error())

		return
	}

	a.respond(req, godap.SetBreakpointsResponseBody{Breakpoints: got})
}

// resume answers continue and the steps: continued first, then the
// response, then whatever running produces.
func (a *adapter) resume(req godap.RequestMessage) {
	a.emit("continued", godap.ContinuedEventBody{ThreadId: threadID, AllThreadsContinued: true})

	if _, ok := req.(*godap.ContinueRequest); ok {
		a.respond(req, godap.ContinueResponseBody{AllThreadsContinued: true})
		a.prog.cont()

		return
	}

	a.respond(req, nil)
	a.prog.step()
}

func (a *adapter) evaluate(req *godap.EvaluateRequest) {
	value, ok := a.prog.evaluate(req.Arguments.Expression)
	if !ok {
		a.fail(req, "cannot evaluate "+req.Arguments.Expression)

		return
	}

	a.respond(req, godap.EvaluateResponseBody{Result: value, Type: "string"})
}
