// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package sitecheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// bannedWords are the competing debuggers and IDEs the published site must
// never name (AGENTS.md rule 1, D8 in the p2-discoverability plan): matched
// word-bounded and case-insensitive.
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
// IDE, which "Visual Studio Code" does not.
var visualStudioRE = regexp.MustCompile(`(?i)\bVisual\s+Studio\b(\s+Code\b)?`)

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

// bannedNameHits returns one message per line in text that names a
// competing debugger or IDE eyedbg doesn't ship, or "Visual Studio" without
// "Code".
func bannedNameHits(text string) []string {
	var hits []string
	for i, line := range strings.Split(text, "\n") {
		lineNo := i + 1
		for j, re := range bannedWordRE {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("line %d: names %q", lineNo, bannedWords[j]))
			}
		}
		if m := visualStudioRE.FindStringSubmatch(line); len(m) > 1 && m[1] == "" {
			hits = append(hits, fmt.Sprintf("line %d: names %q without %q", lineNo, "Visual Studio", "Code"))
		}
	}
	return hits
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
