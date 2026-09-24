// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

const prog = "/src/prog.txt"

// session is a DAP client connected in-process to a fake adapter, with every
// event it received.
type session struct {
	t      *testing.T
	c      *dap.Client
	events chan godap.EventMessage
	served chan error
}

func newSession(t *testing.T) *session {
	t.Helper()

	return newSessionWith(t, daptest.Options{})
}

func newSessionWith(t *testing.T, opts daptest.Options) *session {
	t.Helper()

	toAdapterR, toAdapterW := io.Pipe()
	toClientR, toClientW := io.Pipe()

	s := &session{t: t, events: make(chan godap.EventMessage, 1000), served: make(chan error, 1)}

	go func() {
		err := daptest.ServeWith(toAdapterR, toClientW, opts)
		_ = toClientW.Close()
		s.served <- err
	}()

	s.c = dap.NewClient(toClientR, toAdapterW, dap.Handlers{Event: func(e godap.EventMessage) { s.events <- e }})

	t.Cleanup(func() {
		_ = toAdapterW.Close()
		<-s.c.Done()
	})

	return s
}

func (s *session) do(req godap.RequestMessage) godap.Message {
	s.t.Helper()

	ctx, cancel := context.WithTimeout(s.t.Context(), 10*time.Second)
	defer cancel()

	msg, err := s.c.Do(ctx, req)
	if err != nil {
		s.t.Fatalf("%s: %v", req.GetRequest().Command, err)
	}

	return msg
}

// next returns the next event, skipping output and the kinds in skip.
func (s *session) next(skip ...string) godap.EventMessage {
	s.t.Helper()

	for {
		select {
		case e := <-s.events:
			name := e.GetEvent().Event
			if name == "output" || strings.Contains(strings.Join(skip, ","), name) {
				continue
			}

			return e
		case <-time.After(10 * time.Second):
			s.t.Fatal("no event")

			return nil
		}
	}
}

func (s *session) expectStop(reason string, line int) {
	s.t.Helper()

	e, ok := s.next("continued", "process", "thread", "initialized").(*godap.StoppedEvent)
	if !ok || e.Body.Reason != reason {
		s.t.Fatalf("event = %+v, want stopped (%s)", e, reason)
	}

	if got := s.eval("line"); got != strconv.Itoa(line) {
		s.t.Fatalf("stopped at line %s, want %d", got, line)
	}
}

func (s *session) eval(expr string) string {
	s.t.Helper()

	resp, ok := s.do(&godap.EvaluateRequest{Request: godap.Request{Command: "evaluate"}, Arguments: godap.EvaluateArguments{Expression: expr}}).(*godap.EvaluateResponse)
	if !ok {
		s.t.Fatal("evaluate: unexpected response")
	}

	return resp.Body.Result
}

func (s *session) setBreakpoints(lines ...godap.SourceBreakpoint) error {
	ctx, cancel := context.WithTimeout(s.t.Context(), 10*time.Second)
	defer cancel()

	_, err := s.c.Do(ctx, &godap.SetBreakpointsRequest{
		Request:   godap.Request{Command: "setBreakpoints"},
		Arguments: godap.SetBreakpointsArguments{Source: godap.Source{Path: prog}, Breakpoints: lines},
	})

	return err
}

// launch runs the start-up sequence with the given breakpoints.
func (s *session) launch(lines int, stopAtEntry, hang bool, bps ...godap.SourceBreakpoint) {
	s.t.Helper()

	s.launchWith(daptest.ProgramArgs{Program: prog, Lines: lines, StopAtEntry: stopAtEntry, Hang: hang}, nil, bps...)
}

// launchWith runs the start-up sequence for args, calling configure (if
// set) before configurationDone.
func (s *session) launchWith(pa daptest.ProgramArgs, configure func(), bps ...godap.SourceBreakpoint) {
	s.t.Helper()

	s.do(&godap.InitializeRequest{Request: godap.Request{Command: "initialize"}})

	args, err := json.Marshal(pa.Map())
	if err != nil {
		s.t.Fatal(err)
	}

	s.do(&godap.LaunchRequest{Request: godap.Request{Command: "launch"}, Arguments: args})

	if configure != nil {
		configure()
	}

	if err := s.setBreakpoints(bps...); err != nil {
		s.t.Fatal(err)
	}

	s.do(&godap.ConfigurationDoneRequest{Request: godap.Request{Command: "configurationDone"}})
}

func TestFakeAdapter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		run  func(s *session)
	}{
		{"runs to a breakpoint and steps", func(s *session) {
			s.launch(5, false, false, godap.SourceBreakpoint{Line: 2}, godap.SourceBreakpoint{Line: 4, Condition: "false"})
			s.expectStop("breakpoint", 2)
			s.do(&godap.NextRequest{Request: godap.Request{Command: "next"}})
			s.expectStop("step", 3)

			if got := s.eval("$bps"); got != "2,4 if false" {
				s.t.Errorf("$bps = %q", got)
			}
		}},
		{"conditions", func(s *session) {
			s.launch(9, false, false, godap.SourceBreakpoint{Line: 3, Condition: "line == 7"}, godap.SourceBreakpoint{Line: 7, Condition: "line == 7"})
			s.expectStop("breakpoint", 7)
		}},
		{"rejects duplicate lines", func(s *session) {
			s.launch(5, true, false)
			s.expectStop("entry", 1)

			var reqErr *dap.RequestError
			if err := s.setBreakpoints(godap.SourceBreakpoint{Line: 3}, godap.SourceBreakpoint{Line: 3, Condition: "true"}); !errors.As(err, &reqErr) {
				s.t.Errorf("duplicate lines: err = %v, want a rejected request", err)
			}
		}},
		{"pauses when hung", func(s *session) {
			s.launch(3, false, true)
			s.do(&godap.PauseRequest{Request: godap.Request{Command: "pause"}})
			s.expectStop("pause", 3)
		}},
		{"exits with terminated", func(s *session) {
			s.launch(2, true, false)
			s.expectStop("entry", 1)
			s.do(&godap.ContinueRequest{Request: godap.Request{Command: "continue"}})

			if e, ok := s.next("continued").(*godap.ExitedEvent); !ok || e.Body.ExitCode != 0 {
				s.t.Fatalf("event = %+v, want exited 0", e)
			}

			if _, ok := s.next().(*godap.TerminatedEvent); !ok {
				s.t.Fatal("want terminated")
			}

			s.do(&godap.DisconnectRequest{Request: godap.Request{Command: "disconnect"}})

			if err := <-s.served; err != nil {
				s.t.Errorf("Serve = %v", err)
			}
		}},
		{"continued comes before the response", func(s *session) {
			s.launch(3, true, false)
			s.expectStop("entry", 1)
			s.do(&godap.StepInRequest{Request: godap.Request{Command: "stepIn"}})

			// The response has arrived, so the continued event was read
			// before it (the client handles messages in order).
			select {
			case e := <-s.events:
				if e.GetEvent().Event != "continued" {
					s.t.Errorf("first event after stepIn = %s, want continued", e.GetEvent().Event)
				}
			default:
				s.t.Error("no continued event before the stepIn response")
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.run(newSession(t))
		})
	}
}

// newSocketSession is newSession over a Unix socket the fake adapter dials
// in to (ServeConnect).
func newSocketSession(t *testing.T) *session {
	t.Helper()

	path := filepath.Join(t.TempDir(), "s")

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	s := &session{t: t, events: make(chan godap.EventMessage, 1000), served: make(chan error, 1)}

	go func() { s.served <- daptest.ServeConnect(t.Context(), path, daptest.Options{}) }()

	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}

	s.c = dap.NewClient(conn, conn, dap.Handlers{Event: func(e godap.EventMessage) { s.events <- e }})

	t.Cleanup(func() {
		_ = conn.Close()
		<-s.c.Done()
	})

	return s
}

func TestServeConnect(t *testing.T) {
	t.Parallel()

	t.Run("serves on the socket", func(t *testing.T) {
		t.Parallel()

		s := newSocketSession(t)
		s.launch(3, true, false, godap.SourceBreakpoint{Line: 2})
		s.expectStop("entry", 1)
		s.do(&godap.ContinueRequest{Request: godap.Request{Command: "continue"}})
		s.expectStop("breakpoint", 2)
		s.do(&godap.DisconnectRequest{Request: godap.Request{Command: "disconnect"}})

		if err := <-s.served; err != nil {
			t.Errorf("ServeConnect = %v", err)
		}
	})

	t.Run("fails without a listener", func(t *testing.T) {
		t.Parallel()

		if err := daptest.ServeConnect(t.Context(), filepath.Join(t.TempDir(), "none"), daptest.Options{}); err == nil {
			t.Error("ServeConnect to a missing socket succeeded")
		}
	})
}
