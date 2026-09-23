// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
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
	scanner *bufio.Scanner
	w       io.Writer
}

// NewConn returns a Conn reading from r and writing to w.
func NewConn(r io.Reader, w io.Writer) *Conn {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 0, 4096), MaxMessageSize)

	return &Conn{scanner: s, w: w}
}

// Read decodes the next message into v. It returns io.EOF at a clean end of
// stream.
func (c *Conn) Read(v any) error {
	if !c.scanner.Scan() {
		if err := c.scanner.Err(); err != nil {
			return fmt.Errorf("read message: %w", err)
		}

		return io.EOF
	}

	if err := json.Unmarshal(c.scanner.Bytes(), v); err != nil {
		return fmt.Errorf("decode message: %w", err)
	}

	return nil
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
