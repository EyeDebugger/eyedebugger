// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/skill"
)

// eyedbgWord matches the whole word "eyedbg" (not "eyedbgd": word boundaries
// sit between non-word and word characters, and "g" and "d" are both word
// characters, so \b never falls between them).
var eyedbgWord = regexp.MustCompile(`\beyedbg\b`)

// inlineSpan matches a markdown inline code span's content.
var inlineSpan = regexp.MustCompile("`([^`]*)`")

// skillInvocations finds every "eyedbg …" invocation in md, inside a fenced
// code block (one per line) or an inline code span, and returns each as its
// words after "eyedbg" (shell-aware: quotes group words; a comment, "|",
// ";" or "&&" outside quotes ends the invocation).
func skillInvocations(md string) [][]string {
	var out [][]string

	for _, region := range invocationRegions(md) {
		for _, loc := range eyedbgWord.FindAllStringIndex(region, -1) {
			out = append(out, splitShellWords(truncateAtDelimiter(region[loc[1]:])))
		}
	}

	return out
}

// invocationRegions returns md's fenced code block lines and, outside
// fences, the content of its inline code spans.
func invocationRegions(md string) []string {
	var regions []string

	inFence := false

	for line := range strings.SplitSeq(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence

			continue
		}

		if inFence {
			regions = append(regions, line)

			continue
		}

		for _, m := range inlineSpan.FindAllStringSubmatch(line, -1) {
			regions = append(regions, m[1])
		}
	}

	return regions
}

// truncateAtDelimiter cuts s at the first "#", "|", ";" or "&&" outside
// single or double quotes.
func truncateAtDelimiter(s string) string {
	var inSingle, inDouble bool

	for i := range s {
		switch c := s[i]; {
		case inSingle:
			inSingle = c != '\''
		case inDouble:
			inDouble = c != '"'
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == '#' || c == '|' || c == ';':
			return s[:i]
		case c == '&' && i+1 < len(s) && s[i+1] == '&':
			return s[:i]
		}
	}

	return s
}

// splitShellWords splits s on whitespace outside single or double quotes,
// dropping the quotes themselves.
func splitShellWords(s string) []string {
	var (
		words              []string
		cur                strings.Builder
		inSingle, inDouble bool
	)

	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}

	for _, r := range s {
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			} else {
				cur.WriteRune(r)
			}
		case inDouble:
			if r == '"' {
				inDouble = false
			} else {
				cur.WriteRune(r)
			}
		case r == '\'':
			inSingle = true
		case r == '"':
			inDouble = true
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}

	flush()

	return words
}

// resolveInvocation resolves args (an eyedbg invocation's words, without
// "eyedbg" itself) against root's finalized command tree. It fails if the
// command path is unknown, or if a flag before a bare "--" does not exist
// on the resolved command (local or inherited).
func resolveInvocation(root *cobra.Command, args []string) error {
	target, _, err := root.Find(args)
	if err != nil {
		return err
	}

	local, inherited := target.LocalFlags(), target.InheritedFlags()

	for _, a := range args {
		if a == "--" {
			break
		}

		if a == "-" || !strings.HasPrefix(a, "-") {
			continue
		}

		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")

		var known bool
		if strings.HasPrefix(a, "--") {
			known = local.Lookup(name) != nil || inherited.Lookup(name) != nil
		} else if name != "" {
			known = local.ShorthandLookup(name[:1]) != nil || inherited.ShorthandLookup(name[:1]) != nil
		}

		if !known {
			return fmt.Errorf("%s: unknown flag %q on %q", strings.Join(args, " "), a, target.CommandPath())
		}
	}

	return nil
}

// exitCodeSection matches an exit-code bullet: "- N — rest".
var exitCodeSection = regexp.MustCompile(`(?m)^- (\d+) — (.+)$`)

// exitCodeName matches an error code like INVALID_REQUEST.
var exitCodeName = regexp.MustCompile(`\b[A-Z][A-Z_]+\b`)

// exitCodeLines parses SKILL.md's exit-code bullets ("- N — CODE, CODE
// (note)") into the codes named at each exit status.
func exitCodeLines(md string) map[int][]string {
	out := map[int][]string{}

	for _, m := range exitCodeSection.FindAllStringSubmatch(md, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}

		out[n] = exitCodeName.FindAllString(m[2], -1)
	}

	return out
}

func TestSplitShellWords(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want []string
	}{
		{" start dotnet --bp Program.cs:12", []string{"start", "dotnet", "--bp", "Program.cs:12"}},
		{" run-until Orders.cs:42 --if 'i == 3'", []string{"run-until", "Orders.cs:42", "--if", "i == 3"}},
		{` --bp 'Orders.cs@"var total ="'`, []string{"--bp", `Orders.cs@"var total ="`}},
		{" help <command>", []string{"help", "<command>"}},
	}

	for _, tt := range tests {
		if got := splitShellWords(tt.in); !equalStrings(got, tt.want) {
			t.Errorf("splitShellWords(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func TestTruncateAtDelimiter(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{" vars --changed                             # what that move changed", " vars --changed                             "},
		{" bp add Foo.cs:20 --log 'i={i}'", " bp add Foo.cs:20 --log 'i={i}'"},
		{" a | b", " a "},
		{" a && b", " a "},
		{" a; b", " a"},
	}

	for _, tt := range tests {
		if got := truncateAtDelimiter(tt.in); got != tt.want {
			t.Errorf("truncateAtDelimiter(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSkillInvocationResolution covers skillInvocations and resolveInvocation
// together over small snippets, including the negatives D4 calls for.
func TestSkillInvocationResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		snippet string
		wantErr bool
		// wantNone means the snippet must yield no invocations at all
		// (nothing named "eyedbg" was found).
		wantNone bool
	}{
		{name: "unknown command", snippet: "`eyedbg bogus`", wantErr: true},
		{name: "unknown flag", snippet: "`eyedbg vars --nope`", wantErr: true},
		{name: "global flag before subcommand", snippet: "`eyedbg --json status`"},
		{name: "help --all", snippet: "`eyedbg help --all`"},
		{name: "eyedbgd not matched", snippet: "`eyedbgd version`", wantNone: true},
		{name: "env prefix matched", snippet: "```sh\nEYEDBG_CLIENT=agent:x eyedbg next\n```"},
		{name: "args after -- ignored", snippet: "`eyedbg start dotnet --project P -- --weird`"},
	}

	root := rootFactories()["eyedbg"]()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			invocations := skillInvocations(tt.snippet)

			if tt.wantNone {
				if len(invocations) != 0 {
					t.Fatalf("skillInvocations(%q) = %v, want none", tt.snippet, invocations)
				}

				return
			}

			if len(invocations) != 1 {
				t.Fatalf("skillInvocations(%q) = %v, want exactly one", tt.snippet, invocations)
			}

			err := resolveInvocation(root, invocations[0])
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveInvocation(%v) err = %v, want error = %v", invocations[0], err, tt.wantErr)
			}
		})
	}
}

func TestExitCodeLines(t *testing.T) {
	t.Parallel()

	md := `## Exit codes

- 0 — success, including a wait that timed out (read the output)
- 1 — INVALID_REQUEST, SIDE_EFFECTS
- 2 — NO_SESSION, LEASE_HELD, NOT_OWNER
`

	got := exitCodeLines(md)

	want := map[int][]string{
		0: nil,
		1: {"INVALID_REQUEST", "SIDE_EFFECTS"},
		2: {"NO_SESSION", "LEASE_HELD", "NOT_OWNER"},
	}

	if len(got) != len(want) {
		t.Fatalf("exitCodeLines() = %v, want %v", got, want)
	}

	for n, codes := range want {
		if !equalStrings(got[n], codes) {
			t.Errorf("exitCodeLines()[%d] = %v, want %v", n, got[n], codes)
		}
	}
}

// TestSkillMatchesCommandTree keeps SKILL.md in sync with the real command
// tree (D4): every "eyedbg …" invocation it shows must resolve, its exit
// codes must map to the classes it states, and its frontmatter and size
// must meet the Agent Skills spec.
func TestSkillMatchesCommandTree(t *testing.T) {
	t.Parallel()

	md := skill.Markdown()
	root := rootFactories()["eyedbg"]()

	for _, args := range skillInvocations(md) {
		if err := resolveInvocation(root, args); err != nil {
			t.Errorf("SKILL.md: %q does not resolve against the command tree: %v", "eyedbg "+strings.Join(args, " "), err)
		}
	}

	for n, codes := range exitCodeLines(md) {
		for _, code := range codes {
			err := api.NewError(api.Code(code), "", "")
			if got := exitCode(err); got != n {
				t.Errorf("SKILL.md: %s listed under exit %d, but exitCode() gives %d", code, n, got)
			}

			if !strings.Contains(eyedbgLong, code) {
				t.Errorf("SKILL.md: %s is not named in the root command's Long text", code)
			}
		}
	}

	assertSkillFrontmatter(t, md)
	assertSkillSize(t, md)
}

// assertSkillFrontmatter checks the Agent Skills spec's frontmatter rules:
// name equals the skill's directory, and description is 1-1024 characters.
func assertSkillFrontmatter(t *testing.T, md string) {
	t.Helper()

	fields, err := parseFrontmatter(md)
	if err != nil {
		t.Fatalf("SKILL.md: %v", err)
	}

	if fields["name"] != skill.Name {
		t.Errorf("SKILL.md: frontmatter name = %q, want %q (the directory name)", fields["name"], skill.Name)
	}

	if n := len(fields["description"]); n == 0 || n > 1024 {
		t.Errorf("SKILL.md: frontmatter description is %d characters, want 1-1024", n)
	}
}

// parseFrontmatter reads the "---\n…\n---\n" header at the start of md into
// name → value.
func parseFrontmatter(md string) (map[string]string, error) {
	const marker = "---\n"

	if !strings.HasPrefix(md, marker) {
		return nil, errors.New("no frontmatter")
	}

	rest := md[len(marker):]

	header, _, found := strings.Cut(rest, "\n---\n")
	if !found {
		return nil, errors.New("unterminated frontmatter")
	}

	fields := map[string]string{}

	for line := range strings.SplitSeq(header, "\n") {
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}

		fields[strings.TrimSpace(name)] = strings.TrimSpace(val)
	}

	return fields, nil
}

// assertSkillSize checks D4's cap: at most 10 KiB and under 500 lines.
func assertSkillSize(t *testing.T, md string) {
	t.Helper()

	if n := len(md); n > 10*1024 {
		t.Errorf("SKILL.md is %d bytes, want at most 10 KiB", n)
	}

	if n := strings.Count(md, "\n") + 1; n >= 500 {
		t.Errorf("SKILL.md is %d lines, want under 500", n)
	}
}
