// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package helper

import (
	"strings"
	"sync"
	"unicode"
)

// Bounds on the stderr a failure shows.
const (
	tailBytes = 4 << 10
	tailLines = 20
)

// tail keeps the last tailBytes written to it: a helper's stderr, shown
// only when the helper fails without an answer. Safe for concurrent use.
type tail struct {
	mu  sync.Mutex
	buf []byte
}

func newTail() *tail { return &tail{buf: make([]byte, 0, tailBytes)} }

// Write implements io.Writer; it never fails.
func (t *tail) Write(p []byte) (int, error) {
	n := len(p)

	t.mu.Lock()
	defer t.mu.Unlock()

	if len(p) >= tailBytes {
		t.buf = append(t.buf[:0], p[len(p)-tailBytes:]...)

		return n, nil
	}

	if over := len(t.buf) + len(p) - tailBytes; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}

	t.buf = append(t.buf, p...)

	return n, nil
}

// Sanitized returns the last tailLines non-empty lines kept, as valid UTF-8
// with control characters (tabs and carriage returns included) turned into
// spaces, so helper output can't drive the reader's terminal.
func (t *tail) Sanitized() string {
	t.mu.Lock()
	s := string(t.buf)
	t.mu.Unlock()

	return sanitizeLines(s, tailLines)
}

// sanitizeLines keeps the last max non-blank lines of s, each passed
// through sanitizeLine.
func sanitizeLines(s string, maxLines int) string {
	lines := make([]string, 0, maxLines)

	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(sanitizeLine(line)); line != "" {
			lines = append(lines, line)
		}
	}

	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	return strings.Join(lines, "\n")
}

// sanitizeLine returns s as valid UTF-8 with every control character
// (C0, DEL, C1, including '\n') replaced by a space.
func sanitizeLine(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(s, "�"))
}
