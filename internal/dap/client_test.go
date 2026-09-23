// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dap

import (
	"bufio"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"
)

// fakeAdapter reads requests from the client and lets a test script replies.
type fakeAdapter struct {
	t   *testing.T
	in  *bufio.Reader // what the client wrote
	out io.WriteCloser
	seq int
}

// pipeClient returns a client wired to a fake adapter.
func pipeClient(t *testing.T, h Handlers) (*Client, *fakeAdapter) {
	t.Helper()

	toAdapterR, toAdapterW := io.Pipe()
	toClientR, toClientW := io.Pipe()

	c := NewClient(toClientR, toAdapterW, h)
	fa := &fakeAdapter{t: t, in: bufio.NewReader(toAdapterR), out: toClientW}

	t.Cleanup(func() {
		_ = toClientW.Close()
		_ = toAdapterW.Close()
		<-c.Done()
	})

	return c, fa
}

func (fa *fakeAdapter) read() godap.Message {
	fa.t.Helper()

	msg, err := godap.ReadProtocolMessage(fa.in)
	if err != nil {
		fa.t.Errorf("adapter read: %v", err)

		return nil
	}

	return msg
}

func (fa *fakeAdapter) write(m godap.Message) {
	fa.t.Helper()

	if err := godap.WriteProtocolMessage(fa.out, m); err != nil {
		fa.t.Errorf("adapter write: %v", err)
	}
}

func (fa *fakeAdapter) respond(req godap.RequestMessage, success bool, message string) {
	fa.seq++
	r := req.GetRequest()
	fa.write(&godap.Response{
		ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "response"},
		RequestSeq:      r.Seq, Success: success, Command: r.Command, Message: message,
	})
}

func TestDoCorrelatesOutOfOrderResponses(t *testing.T) {
	t.Parallel()

	c, fa := pipeClient(t, Handlers{})

	type result struct {
		cmd string
		err error
	}

	results := make(chan result, 2)

	var wg sync.WaitGroup
	for _, cmd := range []string{"threads", "configurationDone"} {
		wg.Go(func() {
			_, err := c.Do(t.Context(), &godap.Request{Command: cmd})
			results <- result{cmd, err}
		})
	}

	first, ok1 := fa.read().(godap.RequestMessage)
	second, ok2 := fa.read().(godap.RequestMessage)

	if !ok1 || !ok2 {
		t.Fatal("adapter did not receive two requests")
	}

	// Answer in reverse order; each caller must get its own response.
	fa.respond(second, true, "")
	fa.respond(first, false, "nope")

	wg.Wait()
	close(results)

	for r := range results {
		failed := r.cmd == first.GetRequest().Command

		var reqErr *RequestError
		if got := errors.As(r.err, &reqErr); got != failed {
			t.Errorf("%s: err = %v, want failure %v", r.cmd, r.err, failed)
		}
	}
}

func TestEventsAndReverseRequests(t *testing.T) {
	t.Parallel()

	events := make(chan string, 1)
	c, fa := pipeClient(t, Handlers{Event: func(e godap.EventMessage) { events <- e.GetEvent().Event }})

	fa.seq++
	fa.write(&godap.StoppedEvent{Event: godap.Event{ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "event"}, Event: "stopped"}})

	select {
	case got := <-events:
		if got != "stopped" {
			t.Errorf("event = %q, want stopped", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event not delivered")
	}

	// A reverse request with no handler gets an error response.
	fa.seq++
	fa.write(&godap.RunInTerminalRequest{Request: godap.Request{
		ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "request"}, Command: "runInTerminal",
	}})

	resp, ok := fa.read().(godap.ResponseMessage)
	if !ok {
		t.Fatal("no response to the reverse request")
	}

	if r := resp.GetResponse(); r.Success || r.RequestSeq != fa.seq || r.Command != "runInTerminal" {
		t.Errorf("reverse response = %+v, want a failed runInTerminal answer to seq %d", r, fa.seq)
	}

	_ = c
}

func TestDoFailsWhenAdapterExits(t *testing.T) {
	t.Parallel()

	c, fa := pipeClient(t, Handlers{})

	done := make(chan error, 1)

	go func() {
		_, err := c.Do(t.Context(), &godap.Request{Command: "threads"})
		done <- err
	}()

	fa.read()
	_ = fa.out.Close()

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("err = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do did not return after the adapter exited")
	}

	if _, err := c.Do(t.Context(), &godap.Request{Command: "threads"}); !errors.Is(err, ErrClosed) {
		t.Errorf("Do after close: err = %v, want ErrClosed", err)
	}
}
