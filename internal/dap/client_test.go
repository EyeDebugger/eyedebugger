// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
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

// TestDebugpyTraffic: debugpy's startDebugging reverse request gets a
// failed answer, and its custom debugpyAttach event (which go-dap can't
// decode) is dropped without losing the events after it.
func TestDebugpyTraffic(t *testing.T) {
	t.Parallel()

	events := make(chan string, 2)
	c, fa := pipeClient(t, Handlers{Event: func(e godap.EventMessage) { events <- e.GetEvent().Event }})

	fa.seq++
	fa.write(&godap.StartDebuggingRequest{Request: godap.Request{
		ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "request"}, Command: "startDebugging",
	}})

	resp, ok := fa.read().(godap.ResponseMessage)
	if !ok {
		t.Fatal("no response to startDebugging")
	}

	if r := resp.GetResponse(); r.Success || r.RequestSeq != fa.seq || r.Command != "startDebugging" {
		t.Errorf("startDebugging response = %+v, want a failed answer to seq %d", r, fa.seq)
	}

	fa.seq++
	raw := fmt.Sprintf(`{"seq":%d,"type":"event","event":"debugpyAttach","body":{"name":"child","request":"attach"}}`, fa.seq)

	if err := godap.WriteBaseMessage(fa.out, []byte(raw)); err != nil {
		t.Fatal(err)
	}

	fa.seq++
	fa.write(&godap.StoppedEvent{Event: godap.Event{ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "event"}, Event: "stopped"}})

	select {
	case got := <-events:
		if got != "stopped" {
			t.Errorf("event = %q, want stopped (debugpyAttach dropped)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stopped not delivered after debugpyAttach")
	}

	_ = c
}

// TestDoSkipsTooLargeMessages: a message over MaxAdapterMessage is
// skipped, a response (its waiter then times out) or an event; the
// connection stays up and in sync.
func TestDoSkipsTooLargeMessages(t *testing.T) {
	t.Parallel()

	c, fa := pipeClient(t, Handlers{})

	evalCtx, cancelEval := context.WithCancel(t.Context())
	evalDone, threadsDone := make(chan error, 1), make(chan error, 1)

	go func() {
		_, err := c.Do(evalCtx, &godap.Request{Command: "evaluate"})
		evalDone <- err
	}()

	go func() {
		_, err := c.Do(t.Context(), &godap.Request{Command: "threads"})
		threadsDone <- err
	}()

	// The adapter, once it has both requests: evaluate's response and an
	// event, both too large, then threads' response. Writes stop at the
	// first error: a client that stopped reading must fail the test, not
	// hang it.
	go func() {
		seqs := map[string]int{}

		for range 2 {
			msg, err := godap.ReadProtocolMessage(fa.in)
			if err != nil {
				return
			}

			if r, ok := msg.(godap.RequestMessage); ok {
				seqs[r.GetRequest().Command] = r.GetRequest().Seq
			}
		}

		big := `"` + strings.Repeat("x", MaxAdapterMessage) + `"`
		for _, m := range []string{
			`{"seq":1,"type":"response","request_seq":` + strconv.Itoa(seqs["evaluate"]) + `,"command":"evaluate","success":true,"body":{"result":` + big + `}}`,
			`{"seq":2,"type":"event","event":"output","body":{"output":` + big + `}}`,
			`{"seq":3,"type":"response","request_seq":` + strconv.Itoa(seqs["threads"]) + `,"command":"threads","success":true,"body":{"threads":[]}}`,
		} {
			if _, err := io.WriteString(fa.out, "Content-Length: "+strconv.Itoa(len(m))+"\r\n\r\n"+m); err != nil {
				return
			}
		}
	}()

	if err := <-threadsDone; err != nil {
		t.Errorf("threads after too-large messages: %v", err)
	}

	cancelEval()

	if err := <-evalDone; !errors.Is(err, context.Canceled) {
		t.Errorf("evaluate whose response was skipped: err = %v, want its context's", err)
	}

	select {
	case <-c.Done():
		t.Errorf("the connection ended: %v", c.Err())
	default:
	}
}

// customEvent and customResponse are messages go-dap doesn't know.
type customEvent struct {
	godap.Event

	Body struct {
		N int `json:"n"`
	} `json:"body"`
}

type customResponse struct {
	godap.Response

	Body struct {
		N int `json:"n"`
	} `json:"body"`
}

// TestCodec: with a codec, custom events and responses arrive typed, with
// their bodies; without, an unknown event is dropped and an unknown
// response arrives bare.
func TestCodec(t *testing.T) {
	t.Parallel()

	codec := godap.NewCodec()
	if err := codec.RegisterEvent("x/event", func() godap.Message { return &customEvent{} }); err != nil {
		t.Fatal(err)
	}

	if err := codec.RegisterRequest("x/req", func() godap.Message { return &godap.Request{} },
		func() godap.Message { return &customResponse{} }); err != nil {
		t.Fatal(err)
	}

	for _, withCodec := range []bool{true, false} {
		t.Run(strconv.FormatBool(withCodec), func(t *testing.T) {
			t.Parallel()

			events := make(chan godap.EventMessage, 2)
			h := Handlers{Event: func(e godap.EventMessage) { events <- e }}

			if withCodec {
				h.Codec = codec
			}

			resp := customExchange(t, h)
			first := <-events

			if e, isCustom := first.(*customEvent); withCodec != isCustom || (isCustom && e.Body.N != 7) {
				t.Errorf("first event = %#v", first)
			}

			if !withCodec && first.GetEvent().Event != "stopped" {
				t.Errorf("without a codec the custom event arrived: %#v", first)
			}

			if r, isCustom := resp.(*customResponse); withCodec != isCustom || (isCustom && r.Body.N != 8) {
				t.Errorf("response = %#v", resp)
			}
		})
	}
}

// customExchange sends an x/req request through a client with h; the
// adapter answers with a custom event, a stopped event and a custom
// response, which it returns.
func customExchange(t *testing.T, h Handlers) godap.Message {
	t.Helper()

	c, fa := pipeClient(t, h)

	type result struct {
		resp godap.Message
		err  error
	}

	done := make(chan result, 1)

	go func() {
		resp, err := c.Do(t.Context(), &godap.Request{Command: "x/req"})
		done <- result{resp, err}
	}()

	rawReq, err := ReadMessage(fa.in, 1<<20)
	if err != nil {
		t.Fatal(err)
	}

	var req godap.Request
	if err := json.Unmarshal(rawReq, &req); err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{
		`{"seq":1,"type":"event","event":"x/event","body":{"n":7}}`,
		`{"seq":2,"type":"event","event":"stopped","body":{"reason":"pause","threadId":1}}`,
		`{"seq":3,"type":"response","request_seq":` + strconv.Itoa(req.Seq) + `,"command":"x/req","success":true,"body":{"n":8}}`,
	} {
		_, _ = io.WriteString(fa.out, "Content-Length: "+strconv.Itoa(len(s))+"\r\n\r\n"+s)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("Do: %v", got.err)
	}

	return got.resp
}
