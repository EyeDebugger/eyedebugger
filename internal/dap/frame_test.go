// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dap

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadMessage(t *testing.T) {
	t.Parallel()

	const maxContent = 16

	tests := []struct {
		name    string
		in      string
		want    []string // contents read in order, then wantErr
		wantErr error
	}{
		{name: "valid", in: "Content-Length: 2\r\n\r\n{}", want: []string{"{}"}, wantErr: io.EOF},
		{name: "two messages", in: "Content-Length: 2\r\n\r\n{}Content-Length: 3\r\n\r\n{ }", want: []string{"{}", "{ }"}, wantErr: io.EOF},
		{name: "zero length", in: "Content-Length: 0\r\n\r\n", want: []string{""}, wantErr: io.EOF},
		{name: "length at the limit", in: "Content-Length: 16\r\n\r\n" + strings.Repeat("x", 16), want: []string{strings.Repeat("x", 16)}, wantErr: io.EOF},
		{name: "leading zeros", in: "Content-Length: 0000000002\r\n\r\n{}", want: []string{"{}"}, wantErr: io.EOF},
		{name: "length over the limit", in: "Content-Length: 17\r\n\r\n" + strings.Repeat("x", 17), wantErr: ErrFraming},
		{name: "huge claim", in: "Content-Length: 9999999999\r\n\r\n{}", wantErr: ErrFraming},
		{name: "claim of 1<<40", in: "Content-Length: 1099511627776\r\n\r\n{}", wantErr: ErrFraming},
		{name: "eleven digits", in: "Content-Length: 00000000002\r\n\r\n{}", wantErr: ErrFraming},
		{name: "junk header", in: strings.Repeat("A", 256) + "\r\n\r\n{}", wantErr: ErrFraming},
		{name: "json-rpc line", in: `{"jsonrpc":"2.0","id":3,"method":"daemon.status"}` + "\n", wantErr: ErrFraming},
		{name: "no carriage return", in: "Content-Length: " + strings.Repeat("1", 8<<10), wantErr: ErrFraming},
		{name: "lowercase name", in: "content-length: 2\r\n\r\n{}", wantErr: ErrFraming},
		{name: "no space", in: "Content-Length:2\r\n\r\n{}", wantErr: ErrFraming},
		{name: "two spaces", in: "Content-Length:  2\r\n\r\n{}", wantErr: ErrFraming},
		{name: "trailing space", in: "Content-Length: 2 \r\n\r\n{}", wantErr: ErrFraming},
		{name: "no digits", in: "Content-Length: \r\n\r\n{}", wantErr: ErrFraming},
		{name: "not digits", in: "Content-Length: 1a\r\n\r\n{}", wantErr: ErrFraming},
		{name: "negative", in: "Content-Length: -1\r\n\r\n{}", wantErr: ErrFraming},
		{name: "plus sign", in: "Content-Length: +2\r\n\r\n{}", wantErr: ErrFraming},
		{name: "second header", in: "Content-Length: 2\r\nContent-Type: x\r\n\r\n{}", wantErr: ErrFraming},
		{name: "one crlf", in: "Content-Length: 2\r\n{}", wantErr: ErrFraming},
		{name: "cr without lf", in: "Content-Length: 2\r\r\r\r{}", wantErr: ErrFraming},
		{name: "lf only", in: "Content-Length: 2\n\n{}", wantErr: ErrFraming},
		{name: "truncated content", in: "Content-Length: 5\r\n\r\n{}", wantErr: io.ErrUnexpectedEOF},
		{name: "truncated delimiter", in: "Content-Length: 2\r\n", wantErr: io.ErrUnexpectedEOF},
		{name: "truncated header", in: "Content-Len", wantErr: io.ErrUnexpectedEOF},
		{name: "empty stream", in: "", wantErr: io.EOF},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := bufio.NewReader(strings.NewReader(tt.in))

			for _, want := range tt.want {
				got, err := ReadMessage(r, maxContent)
				if err != nil || string(got) != want {
					t.Fatalf("ReadMessage = %q, %v; want %q", got, err, want)
				}
			}

			got, err := ReadMessage(r, maxContent)
			if !errors.Is(err, tt.wantErr) || got != nil {
				t.Errorf("ReadMessage = %q, %v; want %v", got, err, tt.wantErr)
			}

			if errors.Is(tt.wantErr, ErrFraming) && r.Size() != 4096 {
				t.Errorf("reader buffer grew to %d", r.Size())
			}
		})
	}
}

func TestWriteMessageRoundTrip(t *testing.T) {
	t.Parallel()

	for _, content := range []string{"", "{}", strings.Repeat("é", 1000)} {
		var buf bytes.Buffer
		if err := writeMessage(&buf, []byte(content)); err != nil {
			t.Fatal(err)
		}

		got, err := ReadMessage(bufio.NewReader(&buf), MaxAdapterMessage)
		if err != nil || string(got) != content {
			t.Errorf("round trip of %d bytes = %d bytes, %v", len(content), len(got), err)
		}
	}
}
