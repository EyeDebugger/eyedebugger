// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// dapTimeout bounds each DAP request and each wait for an event: generous
// enough for a real adapter.
const dapTimeout = 60 * time.Second

// dapProc is a running 'eyedbg dap', driven by a spec-following DAP client
// (internal/dap's, the one eyedbg itself uses toward adapters).
type dapProc struct {
	t      *testing.T
	client *dap.Client
	stderr *bytes.Buffer
	exited chan int // the exit code, once

	mu     sync.Mutex
	events []godap.EventMessage
	grown  chan struct{} // closed and replaced when events grows
	cursor int           // where the next waitEvent starts
}

// dap starts 'eyedbg dap --as client' against h's daemon.
func (h *harness) dap(client string, args ...string) *dapProc {
	h.t.Helper()

	cmd := exec.CommandContext(context.Background(), h.eyedbg, append([]string{"dap", "--as", client}, args...)...) //nolint:gosec // The path is what binariesFor just built; args are test-controlled.
	cmd.Env = h.env(nil)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		h.t.Fatal(err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		h.t.Fatal(err)
	}

	p := &dapProc{t: h.t, stderr: &bytes.Buffer{}, exited: make(chan int, 1), grown: make(chan struct{})}
	cmd.Stderr = p.stderr

	if err := cmd.Start(); err != nil {
		h.t.Fatalf("start eyedbg dap: %v", err)
	}

	p.client = dap.NewClient(stdout, stdin, dap.Handlers{Event: p.onEvent})

	go func() {
		<-p.client.Done()

		code := 0

		var exitErr *exec.ExitError
		if err := cmd.Wait(); errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		} else if err != nil {
			code = -1
		}

		p.exited <- code
	}()

	h.t.Cleanup(func() {
		_ = stdin.Close()

		select {
		case <-p.exited:
		case <-time.After(dapTimeout):
			_ = cmd.Process.Kill()

			h.t.Error("eyedbg dap did not exit after its stdin closed")
		}
	})

	return p
}

func (p *dapProc) onEvent(ev godap.EventMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.events = append(p.events, ev)
	close(p.grown)
	p.grown = make(chan struct{})
}

// do sends a request and returns its response; a failed response is
// returned as it is, with no error.
func (p *dapProc) do(req godap.RequestMessage) godap.ResponseMessage {
	p.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), dapTimeout)
	defer cancel()

	msg, err := p.client.Do(ctx, req)
	if _, failed := errors.AsType[*dap.RequestError](err); err != nil && !failed {
		p.t.Fatalf("%s: %v\neyedbg dap stderr: %s", req.GetRequest().Command, err, p.stderr)
	}

	resp, ok := msg.(godap.ResponseMessage)
	if !ok {
		p.t.Fatalf("%s: response %T", req.GetRequest().Command, msg)
	}

	return resp
}

// ok is do, failing the test on a failed response.
func (p *dapProc) ok(req godap.RequestMessage) godap.ResponseMessage {
	p.t.Helper()

	resp := p.do(req)
	if !resp.GetResponse().Success {
		p.t.Fatalf("%s failed: %s", req.GetRequest().Command, errorCode(resp))
	}

	return resp
}

// errorCode is a failed response's eyedbg code (body.error.variables.code).
func errorCode(resp godap.ResponseMessage) string {
	if e, ok := resp.(*godap.ErrorResponse); ok && e.Body.Error != nil {
		return e.Body.Error.Variables["code"] + ": " + e.Body.Error.Format
	}

	return resp.GetResponse().Message
}

// fails is do, failing the test unless the response failed with code.
func (p *dapProc) fails(req godap.RequestMessage, code string) {
	p.t.Helper()

	resp := p.do(req)

	e, ok := resp.(*godap.ErrorResponse)
	if !ok || resp.GetResponse().Success || e.Body.Error == nil || e.Body.Error.Variables["code"] != code || e.Message != code {
		p.t.Fatalf("%s = %+v, want error %s", req.GetRequest().Command, resp, code)
	}
}

// waitEvent returns the first event named name after the last one it
// returned, waiting up to dapTimeout.
func (p *dapProc) waitEvent(name string) godap.EventMessage {
	p.t.Helper()

	deadline := time.After(dapTimeout)

	for {
		p.mu.Lock()
		for i := p.cursor; i < len(p.events); i++ {
			if ev := p.events[i]; ev.GetEvent().Event == name {
				p.cursor = i + 1
				p.mu.Unlock()

				return ev
			}
		}

		grown := p.grown
		p.mu.Unlock()

		select {
		case <-grown:
		case <-p.client.Done():
			p.t.Fatalf("eyedbg dap ended while waiting for %s; stderr: %s", name, p.stderr)
		case <-deadline:
			p.t.Fatalf("no %s event within %s", name, dapTimeout)
		}
	}
}

// wait returns the exit code of 'eyedbg dap', waiting up to dapTimeout.
func (p *dapProc) wait() int {
	p.t.Helper()

	select {
	case code := <-p.exited:
		p.exited <- code

		return code
	case <-time.After(dapTimeout):
		p.t.Fatal("eyedbg dap did not exit")

		return -1
	}
}

// Requests.

func request(command string) godap.Request { return godap.Request{Command: command} }

func initializeReq() *godap.InitializeRequest {
	return &godap.InitializeRequest{Request: request("initialize"), Arguments: godap.InitializeRequestArguments{
		ClientID: "e2e", AdapterID: "eyedbg", LinesStartAt1: true, ColumnsStartAt1: true, PathFormat: "path",
		SupportsInvalidatedEvent: true,
	}}
}

func setBreakpointsReq(file string, lines ...int) *godap.SetBreakpointsRequest {
	bps := make([]godap.SourceBreakpoint, len(lines))
	for i, l := range lines {
		bps[i] = godap.SourceBreakpoint{Line: l}
	}

	return &godap.SetBreakpointsRequest{Request: request("setBreakpoints"), Arguments: godap.SetBreakpointsArguments{
		Source: godap.Source{Path: file}, Breakpoints: bps,
	}}
}

func threadReq(command string, thread int) godap.RequestMessage {
	switch command {
	case "continue":
		return &godap.ContinueRequest{Request: request(command), Arguments: godap.ContinueArguments{ThreadId: thread}}
	case "next":
		return &godap.NextRequest{Request: request(command), Arguments: godap.NextArguments{ThreadId: thread}}
	default:
		return &godap.StackTraceRequest{Request: request(command), Arguments: godap.StackTraceArguments{ThreadId: thread, Levels: 1}}
	}
}

func evaluateReq(expr, evalContext string, frame int) *godap.EvaluateRequest {
	return &godap.EvaluateRequest{Request: request("evaluate"), Arguments: godap.EvaluateArguments{Expression: expr, Context: evalContext, FrameId: frame}}
}
