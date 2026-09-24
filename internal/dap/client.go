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
	"strings"
	"sync"

	godap "github.com/google/go-dap"
)

// ErrClosed means the adapter's stream ended before a response arrived.
var ErrClosed = errors.New("debug adapter connection closed")

// RequestError is a failed DAP response.
type RequestError struct {
	Command string
	Message string
}

// Error implements error.
func (e *RequestError) Error() string {
	return fmt.Sprintf("debug adapter rejected %s: %s", e.Command, e.Message)
}

// Handlers receive what the adapter sends on its own initiative. Both are
// called from the client's read goroutine, one message at a time, so they
// must not block on the client.
type Handlers struct {
	// Event receives every event.
	Event func(godap.EventMessage)
	// Request answers a reverse request (e.g. runInTerminal); nil, or a nil
	// result, makes the client reply with a "not supported" error.
	Request func(godap.RequestMessage) godap.ResponseMessage
	// Codec decodes what the peer sends; nil is go-dap's standard messages
	// only (an unknown event is dropped, an unknown response reaches its
	// waiter without its body). A codec with custom messages registered
	// decodes them too.
	Codec *godap.Codec
}

// Client speaks DAP to one adapter over a byte stream (usually its stdio).
type Client struct {
	w        io.Writer
	handlers Handlers

	wmu sync.Mutex // serializes writes and seq assignment
	seq int

	mu      sync.Mutex
	pending map[int]chan godap.Message
	err     error // set once the read loop ends

	done chan struct{}
}

// NewClient starts reading from r and returns a client that writes to w.
// The read loop ends when r does; [Client.Done] is closed then.
func NewClient(r io.Reader, w io.Writer, h Handlers) *Client {
	c := &Client{w: w, handlers: h, pending: make(map[int]chan godap.Message), done: make(chan struct{})}

	go c.readLoop(bufio.NewReader(r))

	return c
}

// Done is closed when the adapter's output stream ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns why the read loop ended, once Done is closed.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

// Do sends req (its seq is assigned here) and waits for the matching
// response. A failed response is returned as a *RequestError.
func (c *Client) Do(ctx context.Context, req godap.RequestMessage) (godap.Message, error) {
	ch := make(chan godap.Message, 1)

	c.wmu.Lock()
	c.seq++
	r := req.GetRequest()
	r.Seq = c.seq
	r.Type = typeRequest

	if !c.register(r.Seq, ch) {
		c.wmu.Unlock()

		return nil, fmt.Errorf("%s: %w", r.Command, ErrClosed)
	}

	err := godap.WriteProtocolMessage(c.w, req)
	c.wmu.Unlock()

	if err != nil {
		c.unregister(r.Seq)

		return nil, fmt.Errorf("send %s: %w", r.Command, err)
	}

	select {
	case msg, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: %w", r.Command, ErrClosed)
		}

		return msg, responseError(r.Command, msg)
	case <-ctx.Done():
		c.unregister(r.Seq)

		return nil, fmt.Errorf("%s: %w", r.Command, ctx.Err())
	}
}

// Call sends req and decodes the response into T, the expected response type.
func Call[T godap.ResponseMessage](ctx context.Context, c *Client, req godap.RequestMessage) (T, error) {
	var zero T

	msg, err := c.Do(ctx, req)
	if err != nil {
		return zero, err
	}

	resp, ok := msg.(T)
	if !ok {
		return zero, fmt.Errorf("%s: unexpected response type %T", req.GetRequest().Command, msg)
	}

	return resp, nil
}

func (c *Client) register(seq int, ch chan godap.Message) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.pending == nil {
		return false
	}

	c.pending[seq] = ch

	return true
}

func (c *Client) unregister(seq int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.pending, seq)
}

func (c *Client) readLoop(r *bufio.Reader) {
	var err error

	for {
		var raw []byte

		raw, err = ReadMessage(r, MaxAdapterMessage)
		if tooLarge, ok := errors.AsType[*TooLargeError](err); ok {
			// Skip it, staying in sync: one huge value (a response's waiter
			// then times out) must not end the session.
			if _, err = io.CopyN(io.Discard, r, tooLarge.Size); err != nil {
				err = unexpectedEOF(err)

				break
			}

			continue
		}

		if err != nil {
			break
		}

		c.dispatch(raw)
	}

	c.mu.Lock()
	if errors.Is(err, io.EOF) {
		err = ErrClosed
	}

	c.err = err
	for _, ch := range c.pending {
		close(ch)
	}

	c.pending = nil
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) dispatch(raw []byte) {
	var (
		msg godap.Message
		err error
	)

	if c.handlers.Codec != nil {
		msg, err = c.handlers.Codec.DecodeMessage(raw)
	} else {
		msg, err = godap.DecodeProtocolMessage(raw)
	}

	if err != nil {
		// A message go-dap has no type for (an adapter-specific event or
		// response). Responses must still reach their waiter.
		c.deliverUnknown(raw)

		return
	}

	switch m := msg.(type) {
	case godap.ResponseMessage:
		c.deliver(m.GetResponse().RequestSeq, m)
	case godap.EventMessage:
		if c.handlers.Event != nil {
			c.handlers.Event(m)
		}
	case godap.RequestMessage:
		c.answer(m)
	}
}

func (c *Client) deliver(seq int, msg godap.Message) {
	c.mu.Lock()
	ch, ok := c.pending[seq]
	delete(c.pending, seq)
	c.mu.Unlock()

	if ok {
		ch <- msg
	}
}

func (c *Client) deliverUnknown(raw []byte) {
	var resp godap.Response
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Type != typeResponse {
		return
	}

	c.deliver(resp.RequestSeq, &resp)
}

func (c *Client) answer(req godap.RequestMessage) {
	var resp godap.ResponseMessage
	if c.handlers.Request != nil {
		resp = c.handlers.Request(req)
	}

	r := req.GetRequest()
	if resp == nil {
		resp = &godap.ErrorResponse{
			Response: godap.Response{Command: r.Command, Message: "not supported"},
			Body:     godap.ErrorResponseBody{Error: &godap.ErrorMessage{Format: r.Command + " is not supported by eyedbg"}},
		}
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	c.seq++
	out := resp.GetResponse()
	out.Seq = c.seq
	out.Type = typeResponse
	out.RequestSeq = r.Seq
	out.Command = r.Command

	_ = godap.WriteProtocolMessage(c.w, resp)
}

func responseError(command string, msg godap.Message) error {
	switch m := msg.(type) {
	case *godap.ErrorResponse:
		text := m.Message
		if m.Body.Error != nil && m.Body.Error.Format != "" {
			text = formatErrorMessage(m.Body.Error)
		}

		return &RequestError{Command: command, Message: text}
	case godap.ResponseMessage:
		if r := m.GetResponse(); !r.Success {
			return &RequestError{Command: command, Message: r.Message}
		}
	}

	return nil
}

// formatErrorMessage substitutes {name} variables in a DAP error format.
func formatErrorMessage(e *godap.ErrorMessage) string {
	s := e.Format
	for k, v := range e.Variables {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}

	return s
}
