// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package sitecheck

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
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

// toneWords are the AI-tell words the site's tone rule (p2-discoverability
// plan, step 7) bans: matched word-bounded and case-insensitive. "delve" is
// deliberately excluded: it's the name of the Go adapter eyedbg drives, not
// the verb.
var toneWords = []string{"seamless", "seamlessly", "powerful", "robust", "leverage", "unlock"}

var toneWordRE = func() []*regexp.Regexp {
	res := make([]*regexp.Regexp, len(toneWords))
	for i, w := range toneWords {
		res[i] = regexp.MustCompile(`(?i)\b` + w + `\b`)
	}
	return res
}()

// toneWordHits returns one message per line in text that uses a banned tone
// word.
func toneWordHits(text string) []string {
	var hits []string
	for i, line := range strings.Split(text, "\n") {
		lineNo := i + 1
		for j, re := range toneWordRE {
			if re.MatchString(line) {
				hits = append(hits, fmt.Sprintf("line %d: uses banned tone word %q", lineNo, toneWords[j]))
			}
		}
	}
	return hits
}

func TestToneWordHits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want int
	}{
		{"seamless fails", "A seamless experience for agents.", 1},
		{"seamlessly fails", "It works seamlessly with your editor.", 1},
		{"powerful fails", "A powerful new debugger.", 1},
		{"robust fails", "Built on a robust foundation.", 1},
		{"leverage fails", "Leverage the daemon to share state.", 1},
		{"unlock fails", "Unlock deeper diagnostics.", 1},
		{"delve passes, it's the Go adapter's name", "Go uses Delve as its adapter.", 0},
		{"clean line passes", "eyedbg gives your agent a real debugger.", 0},
		{"two hits on one line", "A powerful, robust CLI.", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := toneWordHits(tt.text)
			if len(got) != tt.want {
				t.Errorf("toneWordHits(%q) = %v, want %d hit(s)", tt.text, got, tt.want)
			}
		})
	}
}

// emDashHits returns one message per line in text that contains a U+2014 em
// dash (p2-discoverability plan, step 7: the site's tone rule bans them in
// favor of commas, periods and parentheses).
func emDashHits(text string) []string {
	var hits []string
	for i, line := range strings.Split(text, "\n") {
		if strings.ContainsRune(line, '—') {
			hits = append(hits, fmt.Sprintf("line %d: contains an em dash (U+2014)", i+1))
		}
	}
	return hits
}

func TestEmDashHits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want int
	}{
		{"em dash fails", "eyedbg drives it — you join it.", 1},
		{"hyphen passes", "A CLI-first debugger.", 0},
		{"en dash passes", "Linux – macOS – Windows.", 0},
		{"comma passes", "eyedbg drives it, you join it.", 0},
		{"two em dashes on one line still one hit", "a — b — c", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := emDashHits(tt.text)
			if len(got) != tt.want {
				t.Errorf("emDashHits(%q) = %v, want %d hit(s)", tt.text, got, tt.want)
			}
		})
	}
}

// TestSiteToneRules walks site/, except site/README.md (the one file Pages
// doesn't publish), and fails on an em dash or a banned tone word in any
// text file (p2-discoverability plan, step 7).
func TestSiteToneRules(t *testing.T) {
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
		text := string(data)
		for _, hit := range emDashHits(text) {
			t.Errorf("site/%s: %s", filepath.ToSlash(rel), hit)
		}
		for _, hit := range toneWordHits(text) {
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

// codeElementContent returns the text strictly between the closing '>' of
// the first HTML opening tag containing openTagContains and the next
// "</code>". It errors if no such tag exists, its opening tag is
// unterminated, or there is no closing "</code>" after it.
func codeElementContent(text, openTagContains string) (string, error) {
	i := strings.Index(text, openTagContains)
	if i < 0 {
		return "", fmt.Errorf("no element found containing %q", openTagContains)
	}
	tagEnd := strings.IndexByte(text[i:], '>')
	if tagEnd < 0 {
		return "", fmt.Errorf("unterminated opening tag containing %q", openTagContains)
	}
	rest := text[i+tagEnd+1:]
	content, _, ok := strings.Cut(rest, "</code>")
	if !ok {
		return "", fmt.Errorf("no closing </code> after the tag containing %q", openTagContains)
	}
	return content, nil
}

func TestCodeElementContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		text            string
		openTagContains string
		want            string
		wantErr         bool
	}{
		{
			name:            "extracts between tag and closing code",
			text:            `<pre><code id="install-prompt">a &amp; b</code></pre>`,
			openTagContains: `id="install-prompt"`,
			want:            "a &amp; b",
		},
		{"missing tag", `<code id="other">x</code>`, `id="install-prompt"`, "", true},
		{"unterminated opening tag", `<code id="install-prompt"`, `id="install-prompt"`, "", true},
		{"missing closing tag", `<code id="install-prompt">x`, `id="install-prompt"`, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := codeElementContent(tt.text, tt.openTagContains)
			if (err != nil) != tt.wantErr {
				t.Fatalf("codeElementContent(%q, %q) error = %v, wantErr %v", tt.text, tt.openTagContains, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("codeElementContent(%q, %q) = %q, want %q", tt.text, tt.openTagContains, got, tt.want)
			}
		})
	}
}

// TestInstallPromptSyncedEverywhere checks that the install prompt copy in
// README.md, site/llms.txt and site/index.html is byte-for-byte identical to
// site/install-prompt.txt, the canonical file (D6, step 5's contract;
// index.html joined step 6's).
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
		name    string
		path    string
		extract func(string) (string, error)
	}{
		{"README.md", filepath.Join("..", "..", "README.md"), fencedPromptBetweenMarkers},
		{"site/llms.txt", filepath.Join("..", "..", "site", "llms.txt"), fencedPromptBetweenMarkers},
		{"site/index.html", filepath.Join("..", "..", "site", "index.html"), func(text string) (string, error) {
			block, err := codeElementContent(text, `id="install-prompt"`)
			if err != nil {
				return "", err
			}
			return html.UnescapeString(block), nil
		}},
	}
	for _, c := range copies {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(c.path)
			if err != nil {
				t.Fatalf("read %s: %v", c.name, err)
			}
			block, err := c.extract(string(data))
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
