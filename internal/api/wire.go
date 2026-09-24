// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// MaxMessageSize bounds one framed message; longer lines are a protocol error.
const MaxMessageSize = 1 << 20

const jsonrpcVersion = "2.0"

// errorCodeApplication is the JSON-RPC error code carried by every [Error];
// the stable [Code] travels in the error's data.
const errorCodeApplication = -32000

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result and Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireError      `json:"error,omitempty"`
}

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    *Error `json:"data,omitempty"`
}

// NewRequest builds a request with params encoded as JSON (nil for none).
func NewRequest(id int64, method string, params any) (Request, error) {
	req := Request{JSONRPC: jsonrpcVersion, ID: id, Method: method}

	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return Request{}, fmt.Errorf("encode %s params: %w", method, err)
		}

		req.Params = b
	}

	return req, nil
}

// NewResult builds a success response.
func NewResult(id int64, result any) (Response, error) {
	b, err := json.Marshal(result)
	if err != nil {
		return Response{}, fmt.Errorf("encode result: %w", err)
	}

	return Response{JSONRPC: jsonrpcVersion, ID: id, Result: b}, nil
}

// NewErrorResponse builds an error response from e.
func NewErrorResponse(id int64, e *Error) Response {
	return Response{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &wireError{Code: errorCodeApplication, Message: e.Message, Data: e},
	}
}

// Decode unmarshals the response's result into v, or returns its error as an
// *Error.
func (r Response) Decode(v any) error {
	if r.Error != nil {
		if r.Error.Data != nil {
			return r.Error.Data
		}

		return NewError(CodeInternal, r.Error.Message, "")
	}

	if v == nil {
		return nil
	}

	if err := json.Unmarshal(r.Result, v); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}

	return nil
}

// Conn frames JSON messages as one JSON document per line over a stream.
// It is not safe for concurrent use.
type Conn struct {
	r *bufio.Reader
	w io.Writer
}

// errLineTooLong is a line of MaxMessageSize bytes or more.
var errLineTooLong = errors.New("message too long")

// NewConn returns a Conn reading from r and writing to w.
func NewConn(r io.Reader, w io.Writer) *Conn {
	return &Conn{r: bufio.NewReader(r), w: w}
}

// Reader returns the connection's buffered reader, for handing the
// connection over to another protocol: it yields the bytes already read
// past the last message first. Don't use the Conn afterwards.
func (c *Conn) Reader() *bufio.Reader { return c.r }

// Read decodes the next message into v. It returns io.EOF at a clean end of
// stream.
func (c *Conn) Read(v any) error {
	line, err := c.readLine()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return io.EOF
		}

		return fmt.Errorf("read message: %w", err)
	}

	if err := json.Unmarshal(line, v); err != nil {
		return fmt.Errorf("decode message: %w", err)
	}

	return nil
}

// readLine reads one line: up to a newline, without it and one carriage
// return before it; at the end of the stream, what is left if anything
// (else io.EOF). It reads nothing past the newline, and fails on a line of
// MaxMessageSize bytes or more rather than cut it.
func (c *Conn) readLine() ([]byte, error) {
	var line []byte

	for {
		frag, err := c.r.ReadSlice('\n')
		if len(line)+len(frag) > MaxMessageSize || (err != nil && len(line)+len(frag) >= MaxMessageSize) {
			return nil, errLineTooLong
		}

		line = append(line, frag...)

		switch {
		case err == nil:
			return bytes.TrimSuffix(line[:len(line)-1], []byte{'\r'}), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return bytes.TrimSuffix(line, []byte{'\r'}), nil
		default:
			return nil, err
		}
	}
}

// Write encodes v as one line.
func (c *Conn) Write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode message: %w", err)
	}

	if len(b) >= MaxMessageSize {
		return errors.New("encode message: message too large")
	}

	if _, err := c.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	return nil
}
