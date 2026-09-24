// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dap

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	godap "github.com/google/go-dap"
)

// DAP message types.
const (
	typeRequest  = "request"
	typeResponse = "response"
	typeEvent    = "event"
)

// DefaultWriteTimeout bounds each message a [Server] writes: a peer that
// stops reading fails the write.
const DefaultWriteTimeout = 30 * time.Second

// errBroken is returned by writes after one failed: the stream may hold a
// partial message.
var errBroken = errors.New("DAP connection broken by an earlier write error")

// DecodeError is a request [Server.ReadRequest] could read but not decode:
// an unknown command, or arguments of the wrong shape. It can be answered
// (Seq and Command are the request's); the connection stays usable.
type DecodeError struct {
	Seq     int
	Command string
	Err     error
}

// Error implements error.
func (e *DecodeError) Error() string {
	return fmt.Sprintf("decode %s request %d: %v", e.Command, e.Seq, e.Err)
}

// Unwrap returns the decoding error.
func (e *DecodeError) Unwrap() error { return e.Err }

// deadlineWriter is a writer with write deadlines (a net.Conn).
type deadlineWriter interface {
	SetWriteDeadline(t time.Time) error
}

// Server is the server side of one DAP connection, facing an editor: it
// reads requests and writes responses and events with its own sequence
// numbers. Reading is for one goroutine; writing is safe for concurrent
// use, and messages go out whole, in the order of their seqs.
type Server struct {
	r          *bufio.Reader
	codec      *godap.Codec
	maxContent int

	wmu     sync.Mutex // serializes seq assignment and writes
	w       io.Writer
	seq     int
	timeout time.Duration
	broken  bool
}

// NewServer returns a server reading requests of at most maxContent bytes
// from r with codec, and writing to w. If w has write deadlines (a
// net.Conn), each write gets [DefaultWriteTimeout].
func NewServer(r *bufio.Reader, w io.Writer, codec *godap.Codec, maxContent int) *Server {
	return &Server{r: r, w: w, codec: codec, maxContent: maxContent, timeout: DefaultWriteTimeout}
}

// SetWriteTimeout changes the per-write deadline; call it before the
// server is used.
func (s *Server) SetWriteTimeout(d time.Duration) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	s.timeout = d
}

// ReadRequest returns the next request, skipping any other message the
// peer sends. A request that can't be decoded is a *[DecodeError] and can
// be answered; any other error (framing, content that isn't a DAP message,
// I/O) means the stream can't be read further.
func (s *Server) ReadRequest() (godap.RequestMessage, error) {
	for {
		raw, err := ReadMessage(s.r, s.maxContent)
		if err != nil {
			return nil, err
		}

		var head struct {
			Seq     int    `json:"seq"`
			Type    string `json:"type"`
			Command string `json:"command"`
		}

		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("%w: not a DAP message: %w", ErrFraming, err)
		}

		if head.Type != typeRequest {
			continue
		}

		msg, err := s.codec.DecodeMessage(raw)
		if err != nil {
			return nil, &DecodeError{Seq: head.Seq, Command: head.Command, Err: err}
		}

		req, ok := msg.(godap.RequestMessage)
		if !ok { // unreachable: the codec decodes type request as requests
			return nil, fmt.Errorf("%w: %T is not a request", ErrFraming, msg)
		}

		return req, nil
	}
}

// Respond sends resp as the successful response to req.
func (s *Server) Respond(req godap.RequestMessage, resp godap.ResponseMessage) error {
	r := resp.GetResponse()
	r.Type, r.RequestSeq, r.Command, r.Success = typeResponse, req.GetRequest().Seq, req.GetRequest().Command, true

	return s.write(resp, &r.Seq)
}

// Fail sends a failed response to req: message is its short form, body the
// details (nil for none).
func (s *Server) Fail(req godap.RequestMessage, message string, body *godap.ErrorMessage) error {
	resp := &godap.ErrorResponse{
		Response: godap.Response{
			ProtocolMessage: godap.ProtocolMessage{Type: typeResponse},
			RequestSeq:      req.GetRequest().Seq, Command: req.GetRequest().Command, Message: message,
		},
		Body: godap.ErrorResponseBody{Error: body},
	}

	return s.write(resp, &resp.Seq)
}

// Send sends an event.
func (s *Server) Send(ev godap.EventMessage) error {
	e := ev.GetEvent()
	e.Type = typeEvent

	return s.write(ev, &e.Seq)
}

// write assigns the message's seq, then encodes and writes it, under wmu.
func (s *Server) write(m godap.Message, seq *int) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	if s.broken {
		return errBroken
	}

	s.seq++
	*seq = s.seq

	content, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode DAP message: %w", err)
	}

	if dw, ok := s.w.(deadlineWriter); ok && s.timeout > 0 {
		_ = dw.SetWriteDeadline(time.Now().Add(s.timeout))
	}

	if err := writeMessage(s.w, content); err != nil {
		s.broken = true

		return fmt.Errorf("write DAP message: %w", err)
	}

	return nil
}
