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
	"github.com/spf13/pflag"

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
// code block (one per line, except a ```json fence: JSON, not shell) or an
// inline code span, and returns each as its words after "eyedbg"
// (shell-aware: quotes group words; a comment, "|", ";" or "&&" outside
// quotes ends the invocation). An "eyedbg" sitting inside a double-quoted
// JSON string, key or value (e.g. `"type": "eyedbg"`, or "eyedbg" as a plain
// word inside `"name": "Join eyedbg session"`), is data, not an invocation,
// and is skipped: an inline span can hold prose-quoted JSON even outside a
// ```json fence, so this is checked by double-quote parity from the
// region's start, not by only looking at the character right before the
// match.
func skillInvocations(md string) [][]string {
	var out [][]string

	for _, region := range invocationRegions(md) {
		for _, loc := range eyedbgWord.FindAllStringIndex(region, -1) {
			if insideDoubleQuotes(region, loc[0]) {
				continue
			}

			out = append(out, splitShellWords(truncateAtDelimiter(region[loc[1]:])))
		}
	}

	return out
}

// invocationRegions returns md's fenced code block lines (except a ```json
// fence, which holds data, not shell invocations) and, outside fences, the
// content of its inline code spans. A non-fenced paragraph (its lines up to
// the next blank line) is joined with single spaces before spans are
// matched, so a span the markdown line width wraps mid-invocation still
// pairs its backticks correctly: CommonMark lets an inline code span cross a
// line ending (rendering it as a single space), but matching line by line,
// as this used to, drops half of a wrapped span and mis-pairs the rest.
func invocationRegions(md string) []string {
	var (
		regions   []string
		paragraph []string
		inFence   bool
		fenceJSON bool
	)

	flushParagraph := func() {
		if len(paragraph) == 0 {
			return
		}

		for _, m := range inlineSpan.FindAllStringSubmatch(strings.Join(paragraph, " "), -1) {
			regions = append(regions, m[1])
		}

		paragraph = nil
	}

	for line := range strings.SplitSeq(md, "\n") {
		trimmed := strings.TrimSpace(line)

		if info, ok := strings.CutPrefix(trimmed, "```"); ok {
			// CommonMark lets a fence interrupt a paragraph: flush before
			// toggling so prose above the fence isn't later joined to prose
			// after it (which would mis-pair their backticks). No-op on the
			// closing fence: nothing is added to paragraph while inFence.
			flushParagraph()

			if !inFence {
				fenceJSON = info == "json"
			}

			inFence = !inFence

			continue
		}

		if inFence {
			if !fenceJSON {
				regions = append(regions, line)
			}

			continue
		}

		if trimmed == "" {
			flushParagraph()

			continue
		}

		paragraph = append(paragraph, line)
	}

	flushParagraph()

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

// insideDoubleQuotes reports whether pos in s sits inside a double-quoted
// string, counting unescaped '"' from s's start: an odd count means pos is
// inside one.
func insideDoubleQuotes(s string, pos int) bool {
	inside := false

	for i := 0; i < pos && i < len(s); i++ {
		if s[i] == '"' && (i == 0 || s[i-1] != '\\') {
			inside = !inside
		}
	}

	return inside
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
// command path is unknown, if a word before "--" is a bare positional left
// on a command that still has subcommands (cobra's Find only rejects an
// unknown word at the root; under a parent it silently becomes a
// positional, which is how a typo'd nested subcommand like "bp exeptions"
// slips through), or if a flag before a bare "--" does not exist on the
// resolved command (local or inherited).
func resolveInvocation(root *cobra.Command, args []string) error {
	target, rest, err := root.Find(args)
	if err != nil {
		return err
	}

	if err := rejectUnknownSubcommand(target, rest, args); err != nil {
		return err
	}

	return rejectUnknownFlag(target, args)
}

// rejectUnknownSubcommand fails if rest (root.Find's leftover words) has a
// bare positional before "--" while target still has subcommands: cobra's
// Find only rejects an unknown word at the root, so under a parent it
// silently becomes a positional, which is how a typo'd nested subcommand
// like "bp exeptions" slips through.
func rejectUnknownSubcommand(target *cobra.Command, rest, args []string) error {
	if !target.HasSubCommands() {
		return nil
	}

	for _, a := range rest {
		if a == "--" {
			break
		}

		if !strings.HasPrefix(a, "-") {
			return fmt.Errorf("%s: %q is not a subcommand of %q", strings.Join(args, " "), a, target.CommandPath())
		}
	}

	return nil
}

// rejectUnknownFlag fails if a flag before a bare "--" does not exist on
// target (local or inherited).
func rejectUnknownFlag(target *cobra.Command, args []string) error {
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

// resolveHelpInvocation additionally checks a "help path..." invocation's
// path against the real command tree: resolveInvocation alone can't
// catch a bad one, because cobra's help command has no subcommands of its
// own, so root.Find silently treats anything typed after "help" as a
// leftover positional on the help command itself instead of rejecting it —
// the same reason rejectUnknownSubcommand only fires when the target still
// has subcommands. Not a "help" invocation: does nothing. A path containing
// a "<placeholder>" (e.g. "eyedbg help <command>", a real span in SKILL.md)
// names no real command by design and is left alone.
func resolveHelpInvocation(root *cobra.Command, args []string) error {
	if len(args) == 0 || args[0] != "help" {
		return nil
	}

	var path []string

	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			continue // --all, --json
		}

		if strings.Contains(a, "<") {
			return nil
		}

		path = append(path, a)
	}

	if len(path) == 0 {
		return nil // "eyedbg help" alone: always valid
	}

	target, rest, err := root.Find(path)
	if err != nil {
		return err
	}

	if len(rest) > 0 {
		return fmt.Errorf("eyedbg help %s: %q does not resolve", strings.Join(args[1:], " "), strings.Join(rest, " "))
	}

	if target == root {
		return fmt.Errorf("eyedbg help %s: does not resolve to a specific command", strings.Join(args[1:], " "))
	}

	return nil
}

// flagOnlySpans returns md's code spans (invocationRegions) that name only
// a flag in prose (e.g. "--budget N" or "-s ID"), not a full "eyedbg …"
// invocation: skillInvocations only looks at spans containing "eyedbg", so
// these would otherwise never be checked against the real flags.
func flagOnlySpans(md string) []string {
	var out []string

	for _, region := range invocationRegions(md) {
		trimmed := strings.TrimSpace(region)
		if eyedbgWord.MatchString(region) || !strings.HasPrefix(trimmed, "-") || trimmed == "-" || trimmed == "--" {
			continue
		}

		out = append(out, trimmed)
	}

	return out
}

// resolveFlag checks a flag-only span's first word (the flag itself; the
// rest, if any, is a value placeholder) against the union of every flag
// (local or persistent) anywhere in root's command tree: a bare mention in
// prose isn't tied to one specific subcommand, so any command's flag makes
// it valid.
func resolveFlag(root *cobra.Command, span string) error {
	word, _, _ := strings.Cut(span, " ")

	name, _, _ := strings.Cut(strings.TrimLeft(word, "-"), "=")

	names, shorthands := allFlags(root)

	var known bool
	if strings.HasPrefix(word, "--") {
		known = names[name]
	} else if name != "" {
		known = shorthands[name[:1]]
	}

	if !known {
		return fmt.Errorf("%q is not a known flag anywhere in the command tree", word)
	}

	return nil
}

// allFlags returns the union of every flag's long name and shorthand across
// root's entire command tree.
func allFlags(root *cobra.Command) (names, shorthands map[string]bool) {
	names = map[string]bool{}
	shorthands = map[string]bool{}

	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		// LocalFlags merges this command's own persistent flags in first
		// (mergePersistentFlags); Flags() alone would miss them unless
		// something else already triggered that merge.
		cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
			names[f.Name] = true
			if f.Shorthand != "" {
				shorthands[f.Shorthand] = true
			}
		})

		for _, sub := range cmd.Commands() {
			walk(sub)
		}
	}

	walk(root)

	return names, shorthands
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

// TestInvocationRegionsFlushesBeforeFence checks that a fence interrupts the
// current paragraph instead of letting prose from either side of it join
// into one flushed region: without the flush, the stray backtick before the
// fence pairs with the first backtick after it instead of its own partner,
// and the span naming "eyedbg lease bogus" is silently dropped rather than
// found and rejected.
func TestInvocationRegionsFlushesBeforeFence(t *testing.T) {
	t.Parallel()

	md := "Text with stray ` backtick\n```sh\neyedbg next\n```\nthen `eyedbg lease bogus` here"

	want := []string{"eyedbg next", "eyedbg lease bogus"}

	got := invocationRegions(md)
	if !equalStrings(got, want) {
		t.Fatalf("invocationRegions(%q) = %q, want %q", md, got, want)
	}
}

// TestSkillInvocationResolution covers skillInvocations and resolveInvocation
// together over small snippets, including negative cases.
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
		{name: "unknown nested subcommand", snippet: "`eyedbg bp exeptions all`", wantErr: true},
		{name: "global flag before subcommand", snippet: "`eyedbg --json status`"},
		{name: "help --all", snippet: "`eyedbg help --all`"},
		{name: "eyedbgd not matched", snippet: "`eyedbgd version`", wantNone: true},
		{name: "env prefix matched", snippet: "```sh\nEYEDBG_CLIENT=agent:x eyedbg next\n```"},
		{name: "args after -- ignored", snippet: "`eyedbg start dotnet --project P -- --weird`"},
		{name: "span wrapped mid-invocation by a line break", snippet: "See `eyedbg lease\ntake --force` here."},
		{name: "JSON span with eyedbg as a value, not an invocation", snippet: "`{ \"type\": \"eyedbg\", \"request\": \"attach\" }`", wantNone: true},
		{name: "json fence holds data, not shell invocations", snippet: "```json\n{ \"type\": \"eyedbg\" }\n```", wantNone: true},
	}

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

			// cobra's Find merges flags into the tree: one root per subtest.
			err := resolveInvocation(rootFactories()["eyedbg"](), invocations[0])
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveInvocation(%v) err = %v, want error = %v", invocations[0], err, tt.wantErr)
			}
		})
	}
}

// TestHelpInvocationResolution covers resolveHelpInvocation directly:
// a "help path" invocation whose path doesn't resolve to a real, specific
// command is an error, even though plain resolveInvocation lets it through
// (help.HasSubCommands() is false, so rejectUnknownSubcommand never fires).
func TestHelpInvocationResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "not a help invocation", args: []string{"status"}},
		{name: "help alone", args: []string{"help"}},
		{name: "help --all", args: []string{"help", "--all"}},
		{name: "help a real command", args: []string{"help", "version"}},
		{name: "help a real nested command", args: []string{"help", "adapters", "install"}},
		{name: "help placeholder", args: []string{"help", "<command>"}},
		{name: "help unknown command", args: []string{"help", "bogus"}, wantErr: true},
		{name: "help leftover after a leaf", args: []string{"help", "version", "extra"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// cobra's Find merges flags into the tree: one root per subtest.
			err := resolveHelpInvocation(rootFactories()["eyedbg"](), tt.args)
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveHelpInvocation(%v) err = %v, want error = %v", tt.args, err, tt.wantErr)
			}
		})
	}
}

// TestFlagOnlySpanResolution covers flagOnlySpans and resolveFlag together:
// a bare flag mention in prose (no "eyedbg" in the span) is checked against
// the union of every flag in the command tree, not just one command's.
func TestFlagOnlySpanResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		snippet string
		wantErr bool
		// wantNone means the snippet must yield no flag-only spans at all.
		wantNone bool
	}{
		{name: "root persistent flag", snippet: "`--budget N`"},
		{name: "flag from an unrelated subcommand", snippet: "`--allow-side-effects`"},
		{name: "shorthand flag", snippet: "`-s ID`"},
		{name: "unknown flag", snippet: "`--nope-flag X`", wantErr: true},
		{name: "eyedbg invocation not a bare flag span", snippet: "`eyedbg vars --budget 500`", wantNone: true},
		{name: "not a flag span", snippet: "`Program.cs:12`", wantNone: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			spans := flagOnlySpans(tt.snippet)

			if tt.wantNone {
				if len(spans) != 0 {
					t.Fatalf("flagOnlySpans(%q) = %v, want none", tt.snippet, spans)
				}

				return
			}

			if len(spans) != 1 {
				t.Fatalf("flagOnlySpans(%q) = %v, want exactly one", tt.snippet, spans)
			}

			// cobra's Find/LocalFlags merges flags into the tree: one root
			// per subtest.
			err := resolveFlag(rootFactories()["eyedbg"](), spans[0])
			if (err != nil) != tt.wantErr {
				t.Errorf("resolveFlag(%v) err = %v, want error = %v", spans[0], err, tt.wantErr)
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
// tree: every "eyedbg …" invocation it shows must resolve, its exit
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

		if err := resolveHelpInvocation(root, args); err != nil {
			t.Errorf("SKILL.md: %v", err)
		}
	}

	for _, span := range flagOnlySpans(md) {
		if err := resolveFlag(root, span); err != nil {
			t.Errorf("SKILL.md: flag mention %q: %v", span, err)
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

// assertSkillSize checks SKILL.md's size cap: at most 10 KiB and under 500 lines.
func assertSkillSize(t *testing.T, md string) {
	t.Helper()

	if n := len(md); n > 10*1024 {
		t.Errorf("SKILL.md is %d bytes, want at most 10 KiB", n)
	}

	if n := strings.Count(md, "\n") + 1; n >= 500 {
		t.Errorf("SKILL.md is %d lines, want under 500", n)
	}
}
