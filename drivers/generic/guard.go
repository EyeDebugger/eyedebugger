// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"strings"
	"unicode"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
)

// opChars are the characters of operators; an operator is a run of them.
const opChars = "=<>!:+-*/%&|^~@"

// guard is a manifest's evalGuard, ready to scan with. The scan is a
// best-effort text check, not a parser (docs/adapter-manifests.md): it
// finds calls, assignments and interpolated strings.
type guard struct {
	quotes   string
	prefixes map[string]bool // lower-case
	nonCall  map[string]bool
	safe     map[string]bool
	assign   []string
}

// sideEffects returns the eval check for manifest guard g, nil without one.
func sideEffects(g *adapters.EvalGuard) func(string) (string, bool) {
	if g == nil {
		return nil
	}

	gd := guard{
		quotes: strings.Join(g.Quotes, ""), prefixes: set(g.InterpolatedPrefixes, strings.ToLower),
		nonCall: set(g.NonCallWords, nil), safe: set(g.SafeCalls, nil), assign: g.AssignOps,
	}

	return gd.check
}

func set(words []string, norm func(string) string) map[string]bool {
	out := make(map[string]bool, len(words))

	for _, w := range words {
		if norm != nil {
			w = norm(w)
		}

		out[w] = true
	}

	return out
}

// token kinds the scan remembers: what came before a "(".
type tokenKind int

const (
	tokOther  tokenKind = iota
	tokName             // an identifier
	tokCloser           // ")" or "]"
)

// scanner walks an expression's runes.
type scanner struct {
	g    guard
	rs   []rune
	i    int
	prev tokenKind
	name string // the previous identifier
	// dotted: the previous identifier followed a "."
	dotted bool
	// last is the last rune that wasn't a space.
	last rune
}

// check reports the first thing in expr that can change the program.
func (g guard) check(expr string) (string, bool) {
	s := &scanner{g: g, rs: []rune(expr)}

	for s.i < len(s.rs) {
		if reason, found, done := s.step(); found || done {
			return reason, found
		}
	}

	return "", false
}

// unterminated is the finding for a string that doesn't end: the rest
// of the expression can't be checked.
const unterminated = "use an unterminated string (the rest can't be checked)"

// step scans one token; done stops the scan.
func (s *scanner) step() (reason string, found, done bool) {
	c := s.rs[s.i]

	switch {
	case unicode.IsSpace(c):
		s.i++

		return "", false, false
	case strings.ContainsRune(s.g.quotes, c):
		if !s.skipString() {
			return unterminated, true, true
		}

		return "", false, false
	case isIdentStart(c):
		return s.identifier()
	case c >= '0' && c <= '9':
		s.skipNumber()
	case c == '(':
		if reason, found := s.call(); found {
			return reason, true, false
		}

		s.i++
		s.prev = tokOther
	case c == ')' || c == ']':
		s.i++
		s.prev = tokCloser
	case strings.ContainsRune(opChars, c):
		if op, ok := s.g.assignment(s.operator()); ok {
			return "assign (" + op + ")", true, false
		}
	default:
		s.i++
		s.prev = tokOther
	}

	s.last = c

	return "", false, false
}

// identifier scans a name, or a string prefix and its string.
func (s *scanner) identifier() (reason string, found, done bool) {
	start := s.i
	for s.i < len(s.rs) && isIdentPart(s.rs[s.i]) {
		s.i++
	}

	word := string(s.rs[start:s.i])

	if s.i < len(s.rs) && strings.ContainsRune(s.g.quotes, s.rs[s.i]) {
		if s.g.prefixes[strings.ToLower(word)] {
			return "use an interpolated string (it can run code)", true, false
		}

		s.prev, s.last = tokOther, s.rs[s.i]

		if !s.skipString() {
			return unterminated, true, true
		}

		return "", false, false
	}

	s.prev, s.name, s.dotted, s.last = tokName, word, s.last == '.', s.rs[s.i-1]

	return "", false, false
}

// call reports a "(" that calls something.
func (s *scanner) call() (string, bool) {
	switch s.prev {
	case tokName:
		if s.g.nonCall[s.name] || (s.g.safe[s.name] && !s.dotted) {
			return "", false
		}

		return "call a function (" + s.name + ")", true
	case tokCloser:
		return "call a function", true
	case tokOther:
		return "", false
	}

	return "", false
}

// skipString skips the string at s.i, a tripled quote to the matching
// triple; false when it doesn't end.
func (s *scanner) skipString() bool {
	q := s.rs[s.i]
	n := 1

	if s.i+2 < len(s.rs) && s.rs[s.i+1] == q && s.rs[s.i+2] == q {
		n = 3
	}

	for s.i += n; s.i < len(s.rs); s.i++ {
		switch {
		case s.rs[s.i] == '\\':
			s.i++
		case s.closes(q, n):
			s.i += n
			s.prev, s.last = tokOther, q

			return true
		}
	}

	return false
}

// closes reports whether n quotes q start at s.i.
func (s *scanner) closes(q rune, n int) bool {
	if s.i+n > len(s.rs) {
		return false
	}

	for k := range n {
		if s.rs[s.i+k] != q {
			return false
		}
	}

	return true
}

// skipNumber skips a number literal (digits, letters, "_" and ".").
func (s *scanner) skipNumber() {
	for s.i < len(s.rs) && (isIdentPart(s.rs[s.i]) || s.rs[s.i] == '.') {
		s.i++
	}

	s.prev = tokOther
}

// assignment finds the assignment operator run starts with: all of run,
// or followed by something other than "=" (so "=" matches "=-" in
// "x=-1", but not "==").
func (g guard) assignment(run string) (string, bool) {
	for _, op := range g.assign {
		if run == op || (strings.HasPrefix(run, op) && run[len(op)] != '=') {
			return op, true
		}
	}

	return "", false
}

// operator scans a run of operator characters.
func (s *scanner) operator() string {
	start := s.i
	for s.i < len(s.rs) && strings.ContainsRune(opChars, s.rs[s.i]) {
		s.i++
	}

	s.prev = tokOther

	return string(s.rs[start:s.i])
}

func isIdentStart(c rune) bool { return c == '_' || c >= 0x80 || unicode.IsLetter(c) }

func isIdentPart(c rune) bool { return isIdentStart(c) || unicode.IsDigit(c) }
