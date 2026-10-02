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
	"slices"
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

// TestReverseAnsweredLater: a reverse request Reverse takes is answered
// when reply is called, from another goroutine, exactly once; one it
// leaves goes to Request (else "not supported"); a reply once the stream
// has ended returns without writing.
func TestReverseAnsweredLater(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		take    bool
		wantPID int // 0: a failed "not supported" answer
	}{
		{name: "taken", take: true, wantPID: 42},
		{name: "left", take: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			replies := make(chan func(godap.ResponseMessage), 1)
			c, fa := pipeClient(t, Handlers{Reverse: func(_ godap.RequestMessage, reply func(godap.ResponseMessage)) bool {
				if tt.take {
					replies <- reply
				}

				return tt.take
			}})

			fa.seq++
			fa.write(&godap.RunInTerminalRequest{Request: godap.Request{
				ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "request"}, Command: "runInTerminal",
			}})

			// Two replies race; the pipe blocks each write until it is read,
			// so the replies are awaited after the read.
			var wg sync.WaitGroup

			if tt.take {
				reply := <-replies

				for range 2 {
					wg.Go(func() {
						reply(&godap.RunInTerminalResponse{
							Response: godap.Response{Success: true},
							Body:     godap.RunInTerminalResponseBody{ProcessId: tt.wantPID},
						})
					})
				}
			}

			resp := fa.read()
			wg.Wait()
			checkTerminalResponse(t, resp, fa.seq, tt.wantPID)

			// The next message is the client's own request: no second
			// response was written.
			go func() { _, _ = c.Do(t.Context(), &godap.Request{Command: "threads"}) }()

			if next, ok := fa.read().(godap.RequestMessage); !ok || next.GetRequest().Command != "threads" {
				t.Errorf("next message = %#v, want the threads request", next)
			}
		})
	}
}

// checkTerminalResponse checks msg is the answer to runInTerminal request
// seq: processId pid, or with pid 0 a failure.
func checkTerminalResponse(t *testing.T, msg godap.Message, seq, pid int) {
	t.Helper()

	resp, ok := msg.(godap.ResponseMessage)
	if !ok {
		t.Fatalf("got %#v, want a response to the reverse request", msg)
	}

	r := resp.GetResponse()
	if r.RequestSeq != seq || r.Command != "runInTerminal" || r.Type != typeResponse || r.Seq < 1 {
		t.Errorf("response header = %+v, want a response to runInTerminal seq %d", r, seq)
	}

	if pid == 0 {
		if r.Success {
			t.Errorf("response = %+v, want a failed one", r)
		}

		return
	}

	if rt, ok := resp.(*godap.RunInTerminalResponse); !ok || !r.Success || rt.Body.ProcessId != pid {
		t.Errorf("response = %#v, want processId %d", resp, pid)
	}
}

func TestReverseReplyAfterClose(t *testing.T) {
	t.Parallel()

	replies := make(chan func(godap.ResponseMessage), 1)
	c, fa := pipeClient(t, Handlers{Reverse: func(_ godap.RequestMessage, reply func(godap.ResponseMessage)) bool {
		replies <- reply

		return true
	}})

	fa.write(&godap.RunInTerminalRequest{Request: godap.Request{
		ProtocolMessage: godap.ProtocolMessage{Seq: 1, Type: "request"}, Command: "runInTerminal",
	}})

	reply := <-replies

	_ = fa.out.Close()
	<-c.Done()

	// Nothing reads the pipe to the adapter any more: a write would block.
	done := make(chan struct{})

	go func() {
		reply(&godap.RunInTerminalResponse{Response: godap.Response{Success: true}})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reply after the stream ended did not return")
	}
}

// prefixTranslator rewrites source paths with a prefix each way and records
// what it saw.
type prefixTranslator struct {
	mu   sync.Mutex
	seqs []int    // seqs of the requests Outgoing saw
	seen []string // types Incoming saw
}

func (p *prefixTranslator) Outgoing(req godap.RequestMessage) {
	p.mu.Lock()
	p.seqs = append(p.seqs, req.GetRequest().Seq)
	p.mu.Unlock()

	if r, ok := req.(*godap.SourceRequest); ok && r.Arguments.Source != nil {
		r.Arguments.Source.Path = "/remote" + r.Arguments.Source.Path
	}
}

func (p *prefixTranslator) Incoming(msg godap.Message) {
	p.mu.Lock()
	p.seen = append(p.seen, fmt.Sprintf("%T", msg))
	p.mu.Unlock()

	switch m := msg.(type) {
	case *godap.StackTraceResponse:
		for i := range m.Body.StackFrames {
			if src := m.Body.StackFrames[i].Source; src != nil {
				src.Path = strings.TrimPrefix(src.Path, "/remote")
			}
		}
	case *godap.OutputEvent:
		if m.Body.Source != nil {
			m.Body.Source.Path = strings.TrimPrefix(m.Body.Source.Path, "/remote")
		}
	}
}

func (p *prefixTranslator) record() (seqs []int, seen []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Clone(p.seqs), slices.Clone(p.seen)
}

// translatedRoundTrip sends req through c, checks that the adapter got its
// source path as wire ("" for none), answers it with a stack frame at
// /remote/src/b.cs and returns the response c delivered.
func translatedRoundTrip(t *testing.T, c *Client, fa *fakeAdapter, req godap.RequestMessage, wire string) godap.Message {
	t.Helper()

	done := make(chan godap.Message, 1)

	go func() {
		msg, err := c.Do(t.Context(), req)
		if err != nil {
			t.Errorf("Do: %v", err)
		}

		done <- msg
	}()

	got, ok := fa.read().(godap.RequestMessage)
	if !ok {
		t.Fatal("the adapter got no request")
	}

	if r, ok := got.(*godap.SourceRequest); ok && r.Arguments.Source.Path != wire {
		t.Errorf("adapter got path %q, want %q", r.Arguments.Source.Path, wire)
	}

	fa.seq++
	fa.write(&godap.StackTraceResponse{
		Response: godap.Response{
			ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "response"},
			RequestSeq:      got.GetRequest().Seq, Success: true, Command: got.GetRequest().Command,
		},
		Body: godap.StackTraceResponseBody{StackFrames: []godap.StackFrame{{Id: 1, Source: &godap.Source{Path: "/remote/src/b.cs"}}}},
	})

	select {
	case msg := <-done:
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("no response")

		return nil
	}
}

// TestTranslator: Outgoing sees every request with its seq assigned and its
// rewrite is what goes on the wire; Incoming rewrites responses and events
// before they are delivered, and never sees a reverse request or a message
// that doesn't decode.
func TestTranslator(t *testing.T) {
	t.Parallel()

	tr := &prefixTranslator{}
	events := make(chan *godap.OutputEvent, 1)
	c, fa := pipeClient(t, Handlers{Translate: tr, Event: func(e godap.EventMessage) {
		if o, ok := e.(*godap.OutputEvent); ok {
			events <- o
		}
	}})

	tests := []struct {
		name string
		req  godap.RequestMessage
		// wire is the source path the adapter must receive ("" for none).
		wire string
	}{
		{"source", &godap.SourceRequest{Request: godap.Request{Command: "source"}, Arguments: godap.SourceArguments{Source: &godap.Source{Path: "/src/a.cs"}}}, "/remote/src/a.cs"},
		{"stackTrace", &godap.StackTraceRequest{Request: godap.Request{Command: "stackTrace"}}, ""},
	}

	// In turn, not as subtests: they share the adapter's stream.
	for _, tt := range tests {
		msg := translatedRoundTrip(t, c, fa, tt.req, tt.wire)

		// go-dap decodes a response by its command: only the stackTrace
		// answer carries the frame.
		st, ok := msg.(*godap.StackTraceResponse)
		if ok != (tt.name == "stackTrace") || ok && st.Body.StackFrames[0].Source.Path != "/src/b.cs" {
			t.Errorf("%s: response = %+v, want a stackTrace response's frame at /src/b.cs only for stackTrace", tt.name, msg)
		}
	}

	// A reverse request: answered, never translated. A custom event go-dap
	// can't decode: dropped, never translated. Then an output event is.
	fa.seq++
	fa.write(&godap.RunInTerminalRequest{Request: godap.Request{
		ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "request"}, Command: "runInTerminal",
	}})
	fa.read()

	fa.seq++
	fa.write(&godap.Event{ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "event"}, Event: "custom"})

	fa.seq++
	fa.write(&godap.OutputEvent{
		Event: godap.Event{ProtocolMessage: godap.ProtocolMessage{Seq: fa.seq, Type: "event"}, Event: "output"},
		Body:  godap.OutputEventBody{Output: "x", Source: &godap.Source{Path: "/remote/src/c.cs"}},
	})

	select {
	case o := <-events:
		if o.Body.Source.Path != "/src/c.cs" {
			t.Errorf("output event source = %q, want /src/c.cs", o.Body.Source.Path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("output event not delivered")
	}

	seqs, seen := tr.record()
	if !slices.Equal(seqs, []int{1, 2}) {
		t.Errorf("Outgoing saw seqs %v, want [1 2]", seqs)
	}

	want := []string{"*dap.SourceResponse", "*dap.StackTraceResponse", "*dap.OutputEvent"}
	if !slices.Equal(seen, want) {
		t.Errorf("Incoming saw %v, want %v", seen, want)
	}
}
