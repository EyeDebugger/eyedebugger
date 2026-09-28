// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package sitecheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// bannedWords are the competing debuggers and IDEs the published site must
// never name (AGENTS.md rule 1): matched word-bounded and case-insensitive.
var bannedWords = []string{
	"vsdbg", "rider", "jetbrains", "intellij", "pycharm", "goland", "clion",
	"webstorm", "rustrover", "xcode", "eclipse", "windbg", "gdb",
}

var bannedWordRE = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(bannedWords))
	for i, w := range bannedWords {
		res[i] = regexp.MustCompile(`(?i)\b` + w + `\b`)
	}
	return res
}()

// visualStudioRE finds "Visual Studio", capturing a trailing "Code" if
// present: a bare "Visual Studio" (no capture) reads as naming Microsoft's
// IDE, which "Visual Studio Code" does not. \s already matches a newline, so
// this also catches a name a hard wrap splits across two lines.
var visualStudioRE = regexp.MustCompile(`(?i)\bVisual\s+Studio\b(\s+Code\b)?`)

// nbspRE matches a non-breaking space, in the forms a hand-typed or
// templated line might use it, normalized to an ordinary space before
// matching: "Visual&nbsp;Studio" should be caught the same as "Visual
// Studio".
var nbspRE = regexp.MustCompile(`&nbsp;|&#160;|\x{00A0}`)

// textExtensions are the file types sitecheck reads as text; everything
// else under site/ (images, video) is skipped.
var textExtensions = map[string]bool{
	".html": true,
	".txt":  true,
	".md":   true,
	".css":  true,
	".js":   true,
	".svg":  true,
	".json": true,
	".xml":  true,
}

// bannedNameHits returns one message per hit in text that names a competing
// debugger or IDE eyedbg doesn't ship, or "Visual Studio" without "Code".
// Matching runs over the whole text, not line by line, so a name split
// across a line wrap (e.g. by site/llms.txt's own markdown wrap) is still
// caught, and every hit on a line is reported, not just the first; each
// hit's line number is then computed from its match offset.
func bannedNameHits(text string) []string {
	normalized := nbspRE.ReplaceAllString(text, " ")

	type hit struct {
		line int
		msg  string
	}

	var hits []hit

	for j, re := range bannedWordRE {
		for _, loc := range re.FindAllStringIndex(normalized, -1) {
			hits = append(hits, hit{lineAt(normalized, loc[0]), fmt.Sprintf("names %q", bannedWords[j])})
		}
	}

	for _, m := range visualStudioRE.FindAllStringSubmatchIndex(normalized, -1) {
		if m[2] == -1 { // group 1 ("Code") did not participate: a bare "Visual Studio"
			hits = append(hits, hit{lineAt(normalized, m[0]), fmt.Sprintf("names %q without %q", "Visual Studio", "Code")})
		}
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].line < hits[j].line })

	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = fmt.Sprintf("line %d: %s", h.line, h.msg)
	}

	return out
}

// lineAt returns the 1-based line number containing byte offset in text.
func lineAt(text string, offset int) int {
	return strings.Count(text[:offset], "\n") + 1
}

func TestBannedNameHits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want int
	}{
		{"visual studio code passes", "Install the Visual Studio Code extension.", 0},
		{"visual studio alone fails", "Also works alongside Visual Studio 2022.", 1},
		{"rider fails", "Not built for Rider users.", 1},
		{"jetbrains fails, case-insensitive", "No JetBrains plugin yet.", 1},
		{"provider passes", "Ask your provider for details.", 0},
		{"override passes", "You can override this setting.", 0},
		{"vsdbg fails", "eyedbg never downloads vsdbg.", 1},
		{"gdb fails", "No gdb support here.", 1},
		{"gdbserver passes, not a whole word", "No gdbserver support here.", 0},
		{"clean line passes", "eyedbg uses netcoredbg and SharpDbg.", 0},
		{"two hits on one line", "Never vsdbg or gdb.", 2},
		{"visual studio code then visual studio alone on one line", "Works in Visual Studio Code and Visual Studio 2022.", 1},
		{"visual studio split across a line wrap", "Works in Visual\nStudio 2022.", 1},
		{"visual studio code split across a line wrap passes", "Install the Visual Studio\nCode extension.", 0},
		{"visual studio joined by an nbsp entity", "Also works alongside Visual&nbsp;Studio 2022.", 1},
		{"same banned word twice on one line", "No gdb here, really, no gdb.", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := bannedNameHits(tt.text)
			if len(got) != tt.want {
				t.Errorf("bannedNameHits(%q) = %v, want %d hit(s)", tt.text, got, tt.want)
			}
		})
	}
}

// TestSiteNamesNoCompetingDebugger walks site/, except site/README.md (the
// one file Pages doesn't publish), and fails on any banned name in a text
// file.
func TestSiteNamesNoCompetingDebugger(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "site")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "README.md" {
			return nil
		}
		if !textExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from filepath.WalkDir over a fixed, repo-local root.
		if err != nil {
			return err
		}
		for _, hit := range bannedNameHits(string(data)) {
			t.Errorf("site/%s: %s", filepath.ToSlash(rel), hit)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}
