// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import "strings"

// Reasons SideEffects gives.
const (
	reasonNew          = "create an object"
	reasonAssign       = "assign"
	reasonIncrement    = "increment or decrement"
	reasonInterpolated = "use an interpolated string (it can call methods)"
)

// SideEffects finds what in a C# expression visibly changes the program:
// a method call, new, an assignment, ++ or --, or an interpolated string.
// netcoredbg evaluates everything with function evaluation on, so this is
// all eyedbg can check. It is best-effort, a scan of the text: property
// getters, indexers and operators it can't see, and a delegate called
// through a parenthesized name ("(f)(1)") passes as a cast.
func SideEffects(expr string) (reason string, found bool) {
	sc := scanner{src: expr}

	for sc.pos < len(sc.src) {
		if r, ok := sc.next(); ok {
			return r, true
		}
	}

	return "", false
}

// scanner walks an expression one token at a time.
type scanner struct {
	src string
	pos int
	// prev is the previous token: an identifier, or a one-character
	// punctuator; prevEnd is where it ended (to tell "directly after").
	prev    string
	prevEnd int
	// generics are the identifiers before each open generic "<".
	generics []string
	// closed is the identifier whose generic ">" is prev.
	closed string
	// parens are the open non-call parentheses; castClose means prev is a
	// ")" that closed one holding only a (dotted) name: a cast like
	// "(int)", which the "(" after it doesn't call.
	parens    []paren
	castClose bool
}

// paren is an open parenthesis: name while it holds only identifiers and
// dots, empty while it holds nothing.
type paren struct {
	name, empty bool
}

// content notes a token inside the innermost open parenthesis: part of a
// dotted name (an identifier or ".") or not.
func (sc *scanner) content(partOfName bool) {
	if n := len(sc.parens); n > 0 {
		sc.parens[n-1].empty = false
		sc.parens[n-1].name = sc.parens[n-1].name && partOfName
	}
}

// next reads one token and reports a side effect it shows.
func (sc *scanner) next() (string, bool) {
	c := sc.src[sc.pos]

	if sc.skipSpace() {
		return "", false
	}

	switch {
	case c == '$' || c == '@' || c == '"' || c == '\'':
		if r, ok, done := sc.literal(); done {
			return r, ok
		}

		return sc.punct()
	case isIdentStart(c):
		return sc.ident()
	case c >= '0' && c <= '9':
		sc.number()

		return "", false
	default:
		return sc.punct()
	}
}

// literal reads a string, character, interpolated string or $/@ identifier
// starting at pos; done is false when none starts there.
func (sc *scanner) literal() (reason string, found, done bool) {
	c, n := sc.src[sc.pos], sc.after()

	switch {
	case c == '$' && (n == '"' || n == '@'), c == '@' && n == '$':
		return reasonInterpolated, true, true
	case c == '"':
		sc.skipString(sc.pos+1, false)
	case c == '@' && n == '"':
		sc.skipString(sc.pos+2, true)
	case c == '\'':
		sc.skipChar()
	case isIdentStart(n):
		reason, found = sc.ident()

		return reason, found, true
	default:
		return "", false, false
	}

	return "", false, true
}

// after returns the character after pos (0 at the end).
func (sc *scanner) after() byte {
	if sc.pos+1 < len(sc.src) {
		return sc.src[sc.pos+1]
	}

	return 0
}

// isIdentStart reports whether c starts an identifier. Every byte of a
// non-ASCII character counts (C# identifiers are Unicode letters), so
// "Méthode" is one word.
func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

func (sc *scanner) token(tok string, end int) {
	sc.prev, sc.prevEnd, sc.pos = tok, end, end
	sc.closed, sc.castClose = "", false
}

// skipSpace skips whitespace or a comment at pos, reporting whether it
// did.
func (sc *scanner) skipSpace() bool {
	c := sc.src[sc.pos]

	switch {
	case c == ' ' || c == '\t' || c == '\r' || c == '\n':
		sc.pos++
	case c == '/' && (sc.after() == '*' || sc.after() == '/'):
		sc.skipComment()
	default:
		return false
	}

	return true
}

// skipComment skips a /* */ or // comment, like whitespace.
func (sc *scanner) skipComment() {
	if sc.after() == '/' {
		sc.pos = len(sc.src)

		return
	}

	if end := strings.Index(sc.src[sc.pos+2:], "*/"); end >= 0 {
		sc.pos += 2 + end + 2
	} else {
		sc.pos = len(sc.src)
	}
}

// skipString skips a string literal whose text starts at from.
func (sc *scanner) skipString(from int, verbatim bool) {
	sc.content(false)

	i := from
	for i < len(sc.src) {
		switch {
		case verbatim && sc.src[i] == '"' && i+1 < len(sc.src) && sc.src[i+1] == '"':
			i += 2
		case !verbatim && sc.src[i] == '\\':
			i += 2
		case sc.src[i] == '"':
			sc.token(`"`, i+1)

			return
		default:
			i++
		}
	}

	sc.token(`"`, len(sc.src))
}

func (sc *scanner) skipChar() {
	sc.content(false)

	i := sc.pos + 1
	for i < len(sc.src) && sc.src[i] != '\'' {
		if sc.src[i] == '\\' {
			i++
		}

		i++
	}

	sc.token(`'`, min(i+1, len(sc.src)))
}

func (sc *scanner) number() {
	sc.content(false)

	i := sc.pos
	for i < len(sc.src) && (isIdentPart(sc.src[i]) || sc.src[i] == '.') {
		i++
	}

	sc.token("0", i)
}

func (sc *scanner) ident() (string, bool) {
	i := sc.pos + 1
	for i < len(sc.src) && isIdentPart(sc.src[i]) {
		i++
	}

	word := sc.src[sc.pos:i]
	if word == "new" {
		return reasonNew, true
	}

	sc.content(true)
	sc.token(word, i)

	return "", false
}

// punct handles one punctuator character (and the operator it starts).
func (sc *scanner) punct() (string, bool) {
	c := sc.src[sc.pos]
	rest := sc.src[sc.pos:]

	switch {
	case strings.HasPrefix(rest, "++"), strings.HasPrefix(rest, "--"):
		return reasonIncrement, true
	case c == '(' || c == ')' && len(sc.parens) > 0:
		return sc.paren()
	case c == '=':
		return sc.equals()
	case c == '<' && sc.prevEnd == sc.pos && isIdent(sc.prev) && sc.after() != '=' && sc.after() != '<':
		sc.generics = append(sc.generics, sc.prev)
	case c == '>' && len(sc.generics) > 0 && sc.after() != '=':
		name := sc.generics[len(sc.generics)-1]
		sc.generics = sc.generics[:len(sc.generics)-1]
		sc.content(false)
		sc.token(">", sc.pos+1)
		sc.closed = name

		return "", false
	}

	sc.content(c == '.')
	sc.token(string(c), sc.pos+1)

	return "", false
}

// paren handles "(" (a call, or an open parenthesis) and a ")" that closes
// an open one.
func (sc *scanner) paren() (string, bool) {
	if sc.src[sc.pos] == ')' {
		p := sc.parens[len(sc.parens)-1]
		sc.parens = sc.parens[:len(sc.parens)-1]
		sc.token(")", sc.pos+1)
		sc.castClose = p.name && !p.empty

		return "", false
	}

	if name, ok := sc.callee(); ok {
		return "call a method" + name, true
	}

	sc.content(false)
	sc.parens = append(sc.parens, paren{name: true, empty: true})
	sc.token("(", sc.pos+1)

	return "", false
}

// callee reports whether a "(" at pos calls something, and names it
// (" (NAME)", or "" when it has no name).
func (sc *scanner) callee() (string, bool) {
	switch {
	case isIdent(sc.prev) && !notCall(sc.prev):
		return " (" + strings.TrimLeft(sc.prev, "@$") + ")", true
	case sc.prevEnd != sc.pos:
		return "", false
	case sc.prev == ")" && !sc.castClose, sc.prev == "]":
		return "", true
	case sc.prev == ">" && sc.closed != "":
		return " (" + strings.TrimLeft(sc.closed, "@$") + ")", true
	default:
		return "", false
	}
}

// equals classifies an "=" at pos: part of ==, !=, <=, >= or => is not an
// assignment; every other one is (=, +=, <<=, ??=, ...).
func (sc *scanner) equals() (string, bool) {
	sc.content(false)

	next := sc.after()
	if next == '=' || next == '>' {
		sc.token("=", sc.pos+2)

		return "", false
	}

	var before, before2 byte
	if sc.pos > 0 {
		before = sc.src[sc.pos-1]
	}

	if sc.pos > 1 {
		before2 = sc.src[sc.pos-2]
	}

	switch {
	case before == '!':
	case (before == '<' || before == '>') && before2 != before:
	default:
		return reasonAssign, true
	}

	sc.token("=", sc.pos+1)

	return "", false
}

// notCall reports whether word is a keyword that takes parentheses without
// calling a method.
func notCall(word string) bool {
	switch word {
	case "typeof", "sizeof", "default", "nameof", "checked", "unchecked", "is", "and", "or", "not":
		return true
	default:
		return false
	}
}

func isIdent(tok string) bool {
	return tok != "" && (isIdentStart(tok[0]) || tok[0] == '$' || tok[0] == '@')
}
