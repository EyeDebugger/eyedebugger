// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package sitecheck

import (
	"encoding/json"
	"errors"
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

// installPromptStartMarker and installPromptEndMarker bracket the copy of
// site/install-prompt.txt kept verbatim in README.md and site/llms.txt (D6
// in the p2-discoverability plan).
const (
	installPromptStartMarker = "<!-- install-prompt:start -->"
	installPromptEndMarker   = "<!-- install-prompt:end -->"
)

// fencedPromptBetweenMarkers returns the content of the single ```text
// fenced code block between installPromptStartMarker and
// installPromptEndMarker in text. It errors if either marker is missing, out
// of order, or the region between them doesn't hold exactly one fenced
// block.
func fencedPromptBetweenMarkers(text string) (string, error) {
	start := strings.Index(text, installPromptStartMarker)
	if start < 0 {
		return "", fmt.Errorf("missing %q", installPromptStartMarker)
	}
	end := strings.Index(text, installPromptEndMarker)
	if end < 0 {
		return "", fmt.Errorf("missing %q", installPromptEndMarker)
	}
	if end < start {
		return "", fmt.Errorf("%q appears before %q", installPromptEndMarker, installPromptStartMarker)
	}
	between := text[start+len(installPromptStartMarker) : end]
	if n := strings.Count(between, "```"); n != 2 {
		return "", fmt.Errorf("want exactly one fenced block (2 backtick fences) between the markers, found %d", n)
	}
	_, rest, ok := strings.Cut(between, "```text\n")
	if !ok {
		return "", fmt.Errorf("fenced block must open with %s", "```text")
	}
	content, _, ok := strings.Cut(rest, "```")
	if !ok {
		return "", errors.New("unterminated fenced block")
	}
	return content, nil
}

// normalizePromptBytes normalises CRLF to LF and trims exactly one leading
// and one trailing newline, matching the contract in the p2-discoverability
// plan step 5 ("after trimming one leading/trailing newline; CRLF
// normalised"): a fenced block's content starts right after the opening
// fence's newline and ends right before the closing fence, so it carries no
// leading newline and exactly one trailing one; install-prompt.txt itself is
// plain text ending in one trailing newline and no leading one. Both sides
// go through the same normalisation so either form matches.
func normalizePromptBytes(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimPrefix(s, "\n")
	s = strings.TrimSuffix(s, "\n")
	return s
}

func TestFencedPromptBetweenMarkers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		want    string
		wantErr bool
	}{
		{
			name: "one fenced block extracts",
			text: "before\n<!-- install-prompt:start -->\n```text\nhello\nworld\n```\n<!-- install-prompt:end -->\nafter",
			want: "hello\nworld\n",
		},
		{"missing start marker", "```text\nhi\n```\n<!-- install-prompt:end -->", "", true},
		{"missing end marker", "<!-- install-prompt:start -->\n```text\nhi\n```", "", true},
		{"end before start", "<!-- install-prompt:end -->\n<!-- install-prompt:start -->", "", true},
		{
			name:    "two fenced blocks is an error",
			text:    "<!-- install-prompt:start -->\n```text\na\n```\n```text\nb\n```\n<!-- install-prompt:end -->",
			wantErr: true,
		},
		{
			name:    "zero fenced blocks is an error",
			text:    "<!-- install-prompt:start -->\nno code block here\n<!-- install-prompt:end -->",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := fencedPromptBetweenMarkers(tt.text)
			if (err != nil) != tt.wantErr {
				t.Fatalf("fencedPromptBetweenMarkers(%q) error = %v, wantErr %v", tt.text, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("fencedPromptBetweenMarkers(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// TestInstallPromptSyncedEverywhere checks that the fenced copy of the
// install prompt in README.md and site/llms.txt is byte-for-byte identical
// to site/install-prompt.txt, the canonical file (D6, step 5's contract).
func TestInstallPromptSyncedEverywhere(t *testing.T) {
	t.Parallel()

	canonicalRaw, err := os.ReadFile(filepath.Join("..", "..", "site", "install-prompt.txt"))
	if err != nil {
		t.Fatalf("read site/install-prompt.txt: %v", err)
	}
	canonical := normalizePromptBytes(string(canonicalRaw))
	if canonical == "" {
		t.Fatal("site/install-prompt.txt is empty")
	}

	copies := []struct {
		name string
		path string
	}{
		{"README.md", filepath.Join("..", "..", "README.md")},
		{"site/llms.txt", filepath.Join("..", "..", "site", "llms.txt")},
	}
	for _, c := range copies {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(c.path)
			if err != nil {
				t.Fatalf("read %s: %v", c.name, err)
			}
			block, err := fencedPromptBetweenMarkers(string(data))
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			got := normalizePromptBytes(block)
			if got != canonical {
				t.Errorf("%s's install-prompt block does not match site/install-prompt.txt byte for byte", c.name)
			}
		})
	}
}

// vscodeExtensionManifest is the subset of extensions/vscode/package.json
// sitecheck reads to keep site/llms.txt's extension facts honest.
type vscodeExtensionManifest struct {
	Name       string `json:"name"`
	Publisher  string `json:"publisher"`
	Contribute struct {
		Debuggers []struct {
			Type string `json:"type"`
		} `json:"debuggers"`
	} `json:"contributes"`
}

func readVSCodeExtensionManifest(t *testing.T) vscodeExtensionManifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "extensions", "vscode", "package.json"))
	if err != nil {
		t.Fatalf("read extensions/vscode/package.json: %v", err)
	}
	var m vscodeExtensionManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse extensions/vscode/package.json: %v", err)
	}
	if len(m.Contribute.Debuggers) == 0 {
		t.Fatal("extensions/vscode/package.json declares no debugger contribution")
	}
	return m
}

// TestLlmsTxtHasExtensionFacts checks that site/llms.txt names the real
// extension id (publisher.name), publisher and debug type read from
// extensions/vscode/package.json, so the two can't silently drift apart.
func TestLlmsTxtHasExtensionFacts(t *testing.T) {
	t.Parallel()

	m := readVSCodeExtensionManifest(t)
	id := m.Publisher + "." + m.Name
	debugType := m.Contribute.Debuggers[0].Type

	data, err := os.ReadFile(filepath.Join("..", "..", "site", "llms.txt"))
	if err != nil {
		t.Fatalf("read site/llms.txt: %v", err)
	}
	text := string(data)

	for _, want := range []string{id, m.Publisher, "`" + debugType + "`"} {
		if !strings.Contains(text, want) {
			t.Errorf("site/llms.txt does not mention %q (from extensions/vscode/package.json)", want)
		}
	}
}
