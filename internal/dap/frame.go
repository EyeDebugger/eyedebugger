// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dap

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Framing limits.
const (
	// MaxAdapterMessage bounds one message's content from an adapter (as
	// go-dap does).
	MaxAdapterMessage = 4 << 20
	// maxLengthDigits bounds the Content-Length value's digits.
	maxLengthDigits = 10
)

// ErrFraming means a peer broke DAP's base protocol framing; the stream
// can't be read further.
var ErrFraming = errors.New("invalid DAP framing")

// TooLargeError is a message whose header is valid but announces more
// content than the reader accepts. The content has not been read: a reader
// that skips Size bytes stays in sync with the stream. It wraps
// [ErrFraming].
type TooLargeError struct {
	Size, Max int64
}

// Error implements error.
func (e *TooLargeError) Error() string {
	return fmt.Sprintf("%v: content of %d bytes exceeds %d", ErrFraming, e.Size, e.Max)
}

// Unwrap returns [ErrFraming].
func (e *TooLargeError) Unwrap() error { return ErrFraming }

// contentLengthPrefix starts the one header field DAP defines.
const contentLengthPrefix = "Content-Length: "

// ReadMessage reads one DAP message from r and returns its content: exactly
// "Content-Length: N\r\n\r\n" (N is 1 to 10 ASCII digits, at most
// maxContent), then N bytes. This is the language go-dap's reader accepts,
// minus absurd lengths. The header is checked byte by byte, so the first
// byte that can't belong to it fails the read at once (at most 30 header
// bytes are ever read), and N is checked before anything is allocated.
//
// It returns io.EOF when r ends before a message starts, and
// io.ErrUnexpectedEOF when it ends inside one; a *[TooLargeError] when N is
// over maxContent. Other violations wrap [ErrFraming]; I/O errors pass
// through.
func ReadMessage(r *bufio.Reader, maxContent int) ([]byte, error) {
	n, err := readHeader(r)
	if err != nil {
		return nil, err
	}

	if n > int64(maxContent) {
		return nil, &TooLargeError{Size: n, Max: int64(maxContent)}
	}

	content := make([]byte, n)
	if _, err := io.ReadFull(r, content); err != nil {
		return nil, unexpectedEOF(err)
	}

	return content, nil
}

// readHeader reads the header and its delimiter and returns the content
// length.
func readHeader(r *bufio.Reader) (int64, error) {
	for i := range len(contentLengthPrefix) {
		b, err := r.ReadByte()

		switch {
		case err != nil && i == 0:
			return 0, err
		case err != nil:
			return 0, unexpectedEOF(err)
		case b != contentLengthPrefix[i]:
			return 0, fmt.Errorf("%w: the header is not %q", ErrFraming, contentLengthPrefix+"N")
		}
	}

	n, err := readLength(r)
	if err != nil {
		return 0, err
	}

	for _, want := range []byte("\n\r\n") {
		b, err := r.ReadByte()
		if err != nil {
			return 0, unexpectedEOF(err)
		}

		if b != want {
			return 0, fmt.Errorf("%w: the header is not followed by \\r\\n\\r\\n", ErrFraming)
		}
	}

	return n, nil
}

// readLength reads the Content-Length digits and the carriage return after
// them.
func readLength(r *bufio.Reader) (int64, error) {
	var n int64

	for digits := 0; ; digits++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, unexpectedEOF(err)
		}

		switch {
		case b >= '0' && b <= '9' && digits < maxLengthDigits:
			n = n*10 + int64(b-'0')
		case b == '\r' && digits > 0:
			return n, nil
		default:
			return 0, fmt.Errorf("%w: the content length is not 1 to %d digits", ErrFraming, maxLengthDigits)
		}
	}
}

// unexpectedEOF reports an end of stream inside a message as
// io.ErrUnexpectedEOF.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}

	return err
}

// writeMessage writes content framed as one DAP message, in one write.
func writeMessage(w io.Writer, content []byte) error {
	buf := make([]byte, 0, len(contentLengthPrefix)+maxLengthDigits+4+len(content))
	buf = append(buf, contentLengthPrefix...)
	buf = strconv.AppendInt(buf, int64(len(content)), 10)
	buf = append(buf, "\r\n\r\n"...)
	buf = append(buf, content...)

	_, err := w.Write(buf)

	return err
}
