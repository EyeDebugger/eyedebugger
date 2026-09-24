// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// jsonString returns a JSON string document of exactly n bytes.
func jsonString(n int) string { return `"` + strings.Repeat("x", n-2) + `"` }

// readResults reads c until io.EOF or a read error and renders each result:
// the message, "decode error", "read error" or "eof".
func readResults(c *Conn) []string {
	var out []string

	for range 10 {
		var v json.RawMessage

		err := c.Read(&v)

		switch {
		case err == nil:
			out = append(out, string(v))

			continue
		case errors.Is(err, io.EOF):
			out = append(out, "eof")
		case strings.HasPrefix(err.Error(), "decode message"):
			out = append(out, "decode error")

			continue
		default:
			out = append(out, "read error")
		}

		return out
	}

	return append(out, "too many reads")
}

func TestConnRead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want []string
	}{
		{name: "one line", in: "{\"a\":1}\n", want: []string{`{"a":1}`, "eof"}},
		{name: "crlf", in: "{\"a\":1}\r\n", want: []string{`{"a":1}`, "eof"}},
		{name: "final line without newline", in: `{"a":1}`, want: []string{`{"a":1}`, "eof"}},
		{name: "final line with only cr", in: "{\"a\":1}\r", want: []string{`{"a":1}`, "eof"}},
		{name: "empty line", in: "\n{\"a\":1}\n", want: []string{"decode error", `{"a":1}`, "eof"}},
		{name: "several lines", in: "{\"a\":1}\n{\"b\":2}\n3\n", want: []string{`{"a":1}`, `{"b":2}`, "3", "eof"}},
		{name: "empty stream", in: "", want: []string{"eof"}},
		{
			name: "longest line", in: jsonString(MaxMessageSize-1) + "\n",
			want: []string{jsonString(MaxMessageSize - 1), "eof"},
		},
		{
			name: "longest line with crlf", in: jsonString(MaxMessageSize-2) + "\r\n",
			want: []string{jsonString(MaxMessageSize - 2), "eof"},
		},
		{name: "line too long", in: jsonString(MaxMessageSize) + "\n", want: []string{"read error"}},
		{name: "unterminated line too long", in: jsonString(MaxMessageSize), want: []string{"read error"}},
		{name: "endless line", in: strings.Repeat("x", 3*MaxMessageSize), want: []string{"read error"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := readResults(NewConn(strings.NewReader(tt.in), io.Discard))
			if !slices.Equal(got, tt.want) {
				t.Errorf("reads = %.200q, want %.200q", got, tt.want)
			}
		})
	}
}

// TestConnHandOver checks that bytes a peer sends right after a message, in
// the same write or byte by byte, reach whoever takes over the connection.
func TestConnHandOver(t *testing.T) {
	t.Parallel()

	const (
		first = `{"jsonrpc":"2.0","id":2,"method":"facade.open"}`
		rest  = "Content-Length: 2\r\n\r\n{}Content-Length: 3\r\n\r\n{ }"
	)

	tests := []struct {
		name   string
		reader func(t *testing.T) io.Reader
	}{
		{name: "one write", reader: func(*testing.T) io.Reader { return strings.NewReader(first + "\n" + rest) }},
		{name: "byte by byte", reader: func(t *testing.T) io.Reader {
			t.Helper()

			pr, pw := io.Pipe()

			go func() {
				for _, b := range []byte(first + "\n" + rest) {
					if _, err := pw.Write([]byte{b}); err != nil {
						return
					}
				}

				_ = pw.Close()
			}()

			t.Cleanup(func() { _ = pr.Close() })

			return pr
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := NewConn(tt.reader(t), io.Discard)

			var req Request
			if err := c.Read(&req); err != nil || req.Method != MethodFacadeOpen {
				t.Fatalf("Read = %+v, %v; want the facade.open request", req, err)
			}

			got, err := io.ReadAll(c.Reader())
			if err != nil || !bytes.Equal(got, []byte(rest)) {
				t.Errorf("handed over %q, %v; want %q", got, err, rest)
			}
		})
	}
}

func TestConnWriteTooLarge(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	c := NewConn(strings.NewReader(""), &buf)
	if err := c.Write(strings.Repeat("x", MaxMessageSize)); err == nil || buf.Len() != 0 {
		t.Errorf("Write of an oversized message = %v, wrote %d bytes; want an error and nothing written", err, buf.Len())
	}
}
