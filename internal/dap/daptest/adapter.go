// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"

	godap "github.com/google/go-dap"
)

// EnvFakeAdapter is the variable that, set to "1", makes [MaybeRun] serve DAP
// on stdin and stdout.
const EnvFakeAdapter = "EYEDBG_TEST_FAKE_ADAPTER"

// noTestsArg re-executes a test binary without running any of its tests, so
// it only does what [MaybeRun] or [MaybeRunRunner] makes it do.
const noTestsArg = "-test.run=^$"

// MaybeRun serves DAP on stdin and stdout until the client disconnects or
// closes stdin, and returns true, if this process was started by [Command]
// or [CommandWith]. Otherwise it returns false at once. Call it first in
// TestMain and return when it returns true.
func MaybeRun() bool {
	if os.Getenv(EnvFakeAdapter) != "1" {
		return false
	}

	var opts Options

	if raw := os.Getenv(EnvFakeOptions); raw != "" {
		if err := json.Unmarshal([]byte(raw), &opts); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "fake adapter: options:", err)

			return true
		}
	}

	if err := ServeWith(os.Stdin, os.Stdout, opts); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "fake adapter:", err)
	}

	return true
}

// Command returns how to start the fake adapter: this test binary, with no
// tests to run and [EnvFakeAdapter] set.
func Command() (path string, args, env []string, err error) {
	return CommandWith(Options{})
}

// Arguments returns the launch arguments for a fake program: lines 1 to n
// of the file program. stopAtEntry stops at line 1; hang keeps it running
// after its last line instead of exiting. [ProgramArgs] has more.
func Arguments(program string, n int, stopAtEntry, hang bool) map[string]any {
	return ProgramArgs{Program: program, Lines: n, StopAtEntry: stopAtEntry, Hang: hang}.Map()
}

// launchArgs is the body of a launch or attach request.
type launchArgs = ProgramArgs

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
	opts        Options
	w           io.Writer
	seq         int
	err         error // first write error; ends Serve
	done        bool  // disconnected
	prog        *program
	stopAtEntry bool
	attach      *launchArgs // the attach request's arguments
	// gate is the connection to a fake test runner this adapter attached
	// to; the runner waits until it closes.
	gate net.Conn
}

// Serve speaks DAP on r and w until the client disconnects or r ends.
func Serve(r io.Reader, w io.Writer) error {
	return ServeWith(r, w, Options{})
}

// ServeWith is [Serve] with opts.
func ServeWith(r io.Reader, w io.Writer, opts Options) error {
	a := &adapter{opts: opts, w: w}
	a.prog = newProgram(a.emit, opts.StepHitsBreakpoints, opts.LateVerify)
	br := bufio.NewReader(r)

	defer func() {
		if a.gate != nil {
			_ = a.gate.Close()
		}
	}()

	for !a.done && a.err == nil {
		content, err := godap.ReadBaseMessage(br)
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return fmt.Errorf("read request: %w", err)
		}

		msg, err := godap.DecodeProtocolMessage(content)
		if err != nil {
			return fmt.Errorf("decode request: %w", err)
		}

		if req, ok := msg.(godap.RequestMessage); ok {
			a.handle(req, content)
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

// respondOr responds with body, or fails with err.
func (a *adapter) respondOr(req godap.RequestMessage, body any, err error) {
	if err != nil {
		a.fail(req, err.Error())

		return
	}

	a.respond(req, body)
}

func (a *adapter) handle(req godap.RequestMessage, raw []byte) {
	switch r := req.(type) {
	case *godap.InitializeRequest:
		a.respond(req, a.opts.caps())
	case *godap.LaunchRequest:
		a.launch(req, r.Arguments, false)
	case *godap.AttachRequest:
		a.launch(req, r.Arguments, true)
	case *godap.ConfigurationDoneRequest:
		a.configurationDone(req)
	case *godap.ContinueRequest, *godap.NextRequest, *godap.StepInRequest, *godap.StepOutRequest:
		a.resume(req)
	case *godap.PauseRequest:
		a.respond(req, nil)
		a.prog.pause()
	case *godap.DisconnectRequest:
		a.disconnect(req, raw)
	default:
		a.configure(req)
	}
}

// configure answers the requests that set breakpoints or change variables.
func (a *adapter) configure(req godap.RequestMessage) {
	switch r := req.(type) {
	case *godap.SetBreakpointsRequest:
		if a.opts.VerifyOnSetBreakpoints {
			a.prog.verifyPending()
		}

		got, err := a.prog.setBreakpoints(r.Arguments.Source.Path, r.Arguments.Breakpoints)
		a.respondOr(req, godap.SetBreakpointsResponseBody{Breakpoints: got}, err)
	case *godap.SetFunctionBreakpointsRequest:
		got, err := a.prog.setFunctionBreakpoints(r.Arguments.Breakpoints)
		a.respondOr(req, godap.SetFunctionBreakpointsResponseBody{Breakpoints: got}, err)
	case *godap.SetExceptionBreakpointsRequest:
		a.respondOr(req, nil, a.prog.setFilters(r.Arguments.Filters))
	case *godap.SetExpressionRequest:
		err := a.prog.set(r.Arguments.Expression, r.Arguments.Value)
		a.respondOr(req, godap.SetExpressionResponseBody{Value: a.prog.x, Type: typeInt}, err)
	case *godap.SetVariableRequest:
		err := a.prog.set(r.Arguments.Name, r.Arguments.Value)
		if r.Arguments.VariablesReference != localsRef {
			err = fmt.Errorf("no variables with reference %d", r.Arguments.VariablesReference)
		}

		a.respondOr(req, godap.SetVariableResponseBody{Value: a.prog.x, Type: typeInt}, err)
	default:
		a.inspect(req)
	}
}

// inspect answers the requests that only read state.
func (a *adapter) inspect(req godap.RequestMessage) {
	switch r := req.(type) {
	case *godap.ThreadsRequest:
		a.respond(req, godap.ThreadsResponseBody{Threads: []godap.Thread{{Id: threadID, Name: "main"}}})
	case *godap.StackTraceRequest:
		a.respond(req, godap.StackTraceResponseBody{StackFrames: []godap.StackFrame{a.prog.frame()}, TotalFrames: 1})
	case *godap.ScopesRequest:
		a.respond(req, godap.ScopesResponseBody{Scopes: a.scopes()})
	case *godap.VariablesRequest:
		vars, ok := a.prog.variables(r.Arguments.VariablesReference)
		if !ok {
			a.fail(req, fmt.Sprintf("no variables with reference %d", r.Arguments.VariablesReference))

			return
		}

		a.respond(req, godap.VariablesResponseBody{Variables: vars})
	case *godap.EvaluateRequest:
		a.evaluate(r)
	case *godap.ExceptionInfoRequest:
		info, ok := a.prog.exceptionInfo()
		if !ok {
			a.fail(req, "no exception")

			return
		}

		a.respond(req, info)
	default:
		a.fail(req, req.GetRequest().Command+" is not supported by the fake adapter")
	}
}

// scopes are the frame's scopes: Locals, and Globals with
// [Options.GlobalsScope].
func (a *adapter) scopes() []godap.Scope {
	if !a.opts.GlobalsScope {
		return []godap.Scope{{Name: "Locals", VariablesReference: localsRef}}
	}

	return []godap.Scope{
		{Name: "Locals", PresentationHint: "locals", VariablesReference: localsRef},
		{Name: "Globals", VariablesReference: globalsRef},
	}
}

// launch loads the program of a launch or attach request.
func (a *adapter) launch(req godap.RequestMessage, raw []byte, attach bool) {
	var args launchArgs
	if err := json.Unmarshal(raw, &args); err != nil || args.Program == "" || args.Lines < 1 {
		a.fail(req, "invalid "+req.GetRequest().Command+" arguments: want program and lines")

		return
	}

	a.prog.load(args, attach)
	a.stopAtEntry = args.StopAtEntry && !attach

	if attach {
		a.attach = &args
	}

	a.respond(req, nil)
	// Only now: configuration must not start before the launch arrived
	// (the session sends launch without waiting for its answer).
	a.emit("initialized", nil)
}

// configurationDone starts the program. A failing attach is reported
// here, as netcoredbg does.
func (a *adapter) configurationDone(req godap.RequestMessage) {
	if a.attach != nil && a.attach.FailAttach {
		a.fail(req, "Failed command 'configurationDone' : 0x80070057")

		return
	}

	if a.attach != nil && a.attach.ProcessID > 0 {
		a.gate = dialGate(a.attach.ProcessID)
	}

	a.respond(req, nil)
	a.prog.start(a.stopAtEntry)
}

// disconnect ends the session. An attached one first says how it was asked
// to end (terminateDebuggee true, false or unset) in an output event.
func (a *adapter) disconnect(req godap.RequestMessage, raw []byte) {
	var msg struct {
		Arguments struct {
			TerminateDebuggee *bool `json:"terminateDebuggee"`
		} `json:"arguments"`
	}

	terminate := "unset"
	if err := json.Unmarshal(raw, &msg); err == nil && msg.Arguments.TerminateDebuggee != nil {
		terminate = strconv.FormatBool(*msg.Arguments.TerminateDebuggee)
	}

	if a.attach != nil {
		a.emit("output", godap.OutputEventBody{Category: "console", Output: "fake: disconnect terminateDebuggee=" + terminate + "\n"})
	}

	a.respond(req, nil)
	a.emit("terminated", nil)
	a.done = true
}

// resume answers continue and the steps: breakpoints LateVerify left
// pending are verified, then continued, the response, and whatever running
// produces.
func (a *adapter) resume(req godap.RequestMessage) {
	a.prog.verifyPending()
	a.emit("continued", godap.ContinuedEventBody{ThreadId: threadID, AllThreadsContinued: true})

	if _, ok := req.(*godap.ContinueRequest); ok {
		a.respond(req, godap.ContinueResponseBody{AllThreadsContinued: true})
		a.prog.cont()

		return
	}

	a.respond(req, nil)
	a.prog.step()
}

// evaluate answers the program's expressions; "$context" is the request's
// context.
func (a *adapter) evaluate(req *godap.EvaluateRequest) {
	if req.Arguments.Expression == "$context" {
		a.respond(req, godap.EvaluateResponseBody{Result: req.Arguments.Context, Type: "string"})

		return
	}

	value, ref, ok := a.prog.evaluate(req.Arguments.Expression)
	if !ok {
		a.fail(req, "cannot evaluate "+req.Arguments.Expression)

		return
	}

	a.respond(req, godap.EvaluateResponseBody{Result: value, Type: "string", VariablesReference: ref})
}
