// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Anchor matching bounds. Suggestions score at most maxScoredLines lines,
// each cut to maxLineTokens tokens, against at most maxAnchorTokens anchor
// tokens: at most 5000 × 64 × 200 steps of the longest-common-subsequence
// table, whatever the file.
const (
	maxAmbiguousLines = 10
	maxSuggestions    = 3
	maxScoredLines    = 5000
	maxAnchorTokens   = 64
	maxLineTokens     = 200
	minScore          = 0.5
)

// resolveAnchor returns the one line of the file at path that holds text,
// comparing with runs of whitespace as one space (case-sensitive). No line
// is ANCHOR_NOT_FOUND with the closest lines as a hint; several are
// ANCHOR_AMBIGUOUS.
func resolveAnchor(path, text string) (int, error) {
	want := normalize(text)
	if want == "" || strings.ContainsAny(text, "\r\n") {
		return 0, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("invalid anchor %q", text),
			`an anchor is text from one line, e.g. Program.cs@"total += price"`)
	}

	lines, err := sourceLines(path)
	if err != nil {
		return 0, err
	}

	var found []int

	for i, l := range lines {
		if strings.Contains(normalize(l), want) {
			found = append(found, i+1)
		}
	}

	base := filepath.Base(path)

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return 0, api.NewError(api.CodeAnchorNotFound, fmt.Sprintf("no line of %s holds %q", base, want), notFoundHint(lines, want))
	default:
		shown := found[:min(len(found), maxAmbiguousLines)]

		nums := make([]string, len(shown))
		for i, n := range shown {
			nums[i] = strconv.Itoa(n)
		}

		list := strings.Join(nums, ", ")
		if len(found) > len(shown) {
			list += fmt.Sprintf(" and %d more", len(found)-len(shown))
		}

		return 0, api.NewError(api.CodeAnchorAmbiguous, fmt.Sprintf("%q is on %d lines of %s: %s", want, len(found), base, list),
			"quote more of the line so only one matches, or use FILE:LINE")
	}
}

// sourceLines reads a source file's lines (without a BOM or line ends).
func sourceLines(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, api.NewError(api.CodeInvalidRequest, "can't read "+path+" to find the anchor: "+err.Error(), "")
	}

	if info.Size() > maxSourceFileSize {
		return nil, api.NewError(api.CodeInvalidRequest,
			fmt.Sprintf("%s is too big to search for an anchor (%d bytes; the limit is %d)", path, info.Size(), maxSourceFileSize),
			"use FILE:LINE")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, api.NewError(api.CodeInvalidRequest, "can't read "+path+" to find the anchor: "+err.Error(), "")
	}

	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}

	return lines, nil
}

// normalize trims s and collapses its whitespace runs to one space.
func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func notFoundHint(lines []string, want string) string {
	closest := closestLines(lines, want, maxSuggestions)
	if len(closest) == 0 {
		return "anchors match case-sensitively, with whitespace runs as one space; or use FILE:LINE"
	}

	parts := make([]string, len(closest))
	for i, n := range closest {
		parts[i] = fmt.Sprintf("%d: %s", n, strings.TrimSpace(lines[n-1]))
	}

	return "closest lines: " + strings.Join(parts, " | ")
}

// closestLines returns up to n line numbers whose tokens contain at least
// minScore of want's tokens in order (the longest common subsequence),
// best first, ties by line. Only lines sharing an identifier with want are
// scored.
func closestLines(lines []string, want string, n int) []int {
	wantTokens := tokens(want, maxAnchorTokens)
	if len(wantTokens) == 0 {
		return nil
	}

	idents := map[string]bool{}

	for _, t := range wantTokens {
		if isIdentToken(t) {
			idents[t] = true
		}
	}

	type scored struct {
		line  int
		score float64
	}

	var best []scored

	scoredLines := 0

	for i, l := range lines {
		if scoredLines >= maxScoredLines {
			break
		}

		lt := tokens(l, maxLineTokens)
		if !sharesIdent(lt, idents) || float64(min(len(lt), len(wantTokens)))/float64(len(wantTokens)) < minScore {
			continue
		}

		scoredLines++

		if score := float64(lcs(wantTokens, lt)) / float64(len(wantTokens)); score >= minScore {
			best = append(best, scored{line: i + 1, score: score})
		}
	}

	sort.SliceStable(best, func(i, j int) bool { return best[i].score > best[j].score })

	out := make([]int, 0, n)
	for _, b := range best[:min(len(best), n)] {
		out = append(out, b.line)
	}

	return out
}

func sharesIdent(lt []string, idents map[string]bool) bool {
	for _, t := range lt {
		if idents[t] {
			return true
		}
	}

	return false
}

// tokens splits s into identifiers, numbers and single other characters,
// at most limit of them.
func tokens(s string, limit int) []string {
	var out []string

	rs := []rune(s)
	for i := 0; i < len(rs) && len(out) < limit; {
		r := rs[i]

		j := i + 1

		switch {
		case unicode.IsSpace(r):
			i++

			continue
		case r == '_' || unicode.IsLetter(r):
			for j < len(rs) && (rs[j] == '_' || unicode.IsLetter(rs[j]) || unicode.IsDigit(rs[j])) {
				j++
			}
		case unicode.IsDigit(r):
			for j < len(rs) && unicode.IsDigit(rs[j]) {
				j++
			}
		}

		out = append(out, string(rs[i:j]))
		i = j
	}

	return out
}

func isIdentToken(t string) bool {
	r, _ := utf8.DecodeRuneInString(t)

	return r == '_' || unicode.IsLetter(r)
}

// lcs is the length of the longest common subsequence of a and b, in
// O(len(a)·len(b)) time and O(len(b)) space.
func lcs(a, b []string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)

	for _, x := range a {
		for j, y := range b {
			switch {
			case x == y:
				cur[j+1] = prev[j] + 1
			case prev[j+1] >= cur[j]:
				cur[j+1] = prev[j+1]
			default:
				cur[j+1] = cur[j]
			}
		}

		prev, cur = cur, prev
	}

	return prev[len(b)]
}

// staleNote is the note for a line breakpoint in path when the file changed
// after since (the session's start), or "".
func staleNote(path string, since time.Time) string {
	info, err := os.Stat(path)
	if err != nil || !info.ModTime().After(since) {
		return ""
	}

	return filepath.Base(path) + " changed after the session started: the program runs the code it was built from, " +
		"so this line may not match it (restart the session to debug the new code)"
}
