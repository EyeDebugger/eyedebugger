// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// packageJSONPath is extensions/vscode/package.json, relative to this
// package's own directory (the test's working directory).
const packageJSONPath = "../../extensions/vscode/package.json"

// extensionSchema is the parts of extensions/vscode/package.json a launch or
// attach JSON example is checked against.
type extensionSchema struct {
	Name        string `json:"name"`
	Publisher   string `json:"publisher"`
	Contributes struct {
		Debuggers []struct {
			Type                    string                        `json:"type"`
			ConfigurationAttributes map[string]configurationAttrs `json:"configurationAttributes"`
		} `json:"debuggers"`
	} `json:"contributes"`
}

type configurationAttrs struct {
	Required   []string                  `json:"required"`
	Properties map[string]propertySchema `json:"properties"`
}

type propertySchema struct {
	Type    string   `json:"type"`
	Enum    []string `json:"enum"`
	Pattern string   `json:"pattern"`
}

// loadExtensionSchema reads extensions/vscode/package.json at test time: the
// guides are checked against what the extension really declares, not a
// copy that can drift.
func loadExtensionSchema(t *testing.T) extensionSchema {
	t.Helper()

	data, err := os.ReadFile(packageJSONPath)
	if err != nil {
		t.Fatalf("read %s: %v", packageJSONPath, err)
	}

	var pkg extensionSchema
	if err := json.Unmarshal(data, &pkg); err != nil {
		t.Fatalf("parse %s: %v", packageJSONPath, err)
	}

	if len(pkg.Contributes.Debuggers) != 1 {
		t.Fatalf("%s: want exactly one debugger, got %d", packageJSONPath, len(pkg.Contributes.Debuggers))
	}

	return pkg
}

// guideGenericKeys are launch.json keys VS Code itself understands and
// forwards untouched (validate.ts), not part of the eyedbg debugger's own
// configurationAttributes.
var guideGenericKeys = map[string]bool{
	"type": true, "request": true, "name": true, "serverReadyAction": true,
	"preLaunchTask": true, "postDebugTask": true, "presentation": true, "internalConsoleOptions": true,
}

// jsonExamples finds every top-level JSON object in text whose "type" field
// is "eyedbg" (a launch.json example), decoding forward from every "{" and
// skipping ahead past whatever a successful decode consumed so a match
// inside an already-matched object is never revisited.
func jsonExamples(text string) []map[string]any {
	var out []map[string]any

	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}

		dec := json.NewDecoder(strings.NewReader(text[i:]))

		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			continue
		}

		if m["type"] != "eyedbg" {
			continue
		}

		out = append(out, m)
		i += int(dec.InputOffset()) - 1
	}

	return out
}

// validateExample checks one launch.json example against pkg's real
// configurationAttributes for m["request"], plus guideGenericKeys, and (for
// a launch example) that "lang" names a language eyedbg really has.
func validateExample(pkg extensionSchema, m map[string]any, langs map[string]bool) error {
	req, _ := m["request"].(string)

	attrs, ok := pkg.Contributes.Debuggers[0].ConfigurationAttributes[req]
	if !ok {
		return fmt.Errorf("unknown request %q", req)
	}

	for key, val := range m {
		if guideGenericKeys[key] {
			continue
		}

		prop, ok := attrs.Properties[key]
		if !ok {
			return fmt.Errorf("key %q is not in configurationAttributes[%q] or the generic keys", key, req)
		}

		if err := validateProperty(key, prop, val); err != nil {
			return err
		}
	}

	for _, rk := range attrs.Required {
		if _, ok := m[rk]; !ok {
			return fmt.Errorf("missing required key %q for request %q", rk, req)
		}
	}

	if req == "launch" {
		lang, _ := m["lang"].(string)
		if !langs[lang] {
			return fmt.Errorf("lang %q is not a language eyedbg has (see 'eyedbg adapters ls')", lang)
		}
	}

	return nil
}

// validateProperty checks val against prop's enum, pattern and JSON type.
func validateProperty(key string, prop propertySchema, val any) error {
	if err := validateEnum(key, prop, val); err != nil {
		return err
	}

	if err := validatePattern(key, prop, val); err != nil {
		return err
	}

	return validateJSONType(key, prop, val)
}

func validateEnum(key string, prop propertySchema, val any) error {
	if len(prop.Enum) == 0 {
		return nil
	}

	s, ok := val.(string)
	if !ok || !slices.Contains(prop.Enum, s) {
		return fmt.Errorf("%q = %v is not one of %v", key, val, prop.Enum)
	}

	return nil
}

func validatePattern(key string, prop propertySchema, val any) error {
	if prop.Pattern == "" {
		return nil
	}

	s, ok := val.(string)
	if !ok {
		return fmt.Errorf("%q must be a string to match pattern %q", key, prop.Pattern)
	}

	matched, err := regexp.MatchString(prop.Pattern, s)
	if err != nil {
		return fmt.Errorf("%q: bad pattern %q: %w", key, prop.Pattern, err)
	}

	if !matched {
		return fmt.Errorf("%q = %q does not match pattern %q", key, s, prop.Pattern)
	}

	return nil
}

// validateJSONType checks val's Go type against prop.Type's JSON schema
// type ("string" only when prop has no enum: an enum already checked it).
func validateJSONType(key string, prop propertySchema, val any) error {
	var ok bool

	switch prop.Type {
	case "boolean":
		_, ok = val.(bool)
	case "array":
		_, ok = val.([]any)
	case "object":
		_, ok = val.(map[string]any)
	case "string":
		if len(prop.Enum) > 0 {
			return nil
		}

		_, ok = val.(string)
	default:
		return nil
	}

	if !ok {
		return fmt.Errorf("%q = %v is not a %s", key, val, prop.Type)
	}

	return nil
}

// bundledLanguages are the languages 'eyedbg adapters ls' really lists, read
// from the loaded manifests instead of a hard-coded copy that can drift.
func bundledLanguages() map[string]bool {
	out := map[string]bool{}

	for _, m := range loadRegistry().Adapters() {
		if lang := m.LanguageName(); lang != "" {
			out[lang] = true
		}
	}

	return out
}

// walkGuides returns g and every command under it (depth-first).
func walkGuides(g *cobra.Command) []*cobra.Command {
	out := []*cobra.Command{g}
	for _, sub := range g.Commands() {
		out = append(out, walkGuides(sub)...)
	}

	return out
}

// allGuides are every guide command: 'vscode' and 'lang' (with its
// children).
func allGuides() []*cobra.Command {
	var out []*cobra.Command
	out = append(out, walkGuides(newVSCodeGuide())...)
	out = append(out, walkGuides(newLangCommand())...)

	return out
}

// TestGuideExamplesMatchExtensionSchema checks every launch.json object in a
// guide's Long and Example against extensions/vscode/package.json's real
// configurationAttributes (docs/CONVENTIONS.md § Help text): every guide
// except the 'lang' index has at least one launch and one attach example, so
// this can't pass by finding
// zero examples.
func TestGuideExamplesMatchExtensionSchema(t *testing.T) {
	t.Parallel()

	pkg := loadExtensionSchema(t)
	langs := bundledLanguages()

	for _, g := range allGuides() {
		t.Run(g.CommandPath(), func(t *testing.T) {
			t.Parallel()

			assertGuideExamples(t, g, pkg, langs)
		})
	}
}

// assertGuideExamples validates every JSON example in g's Long and Example
// against pkg and langs, requires at least one launch and one attach example
// (the 'lang' index guide instead requires none: its table names real
// commands, not invented JSON examples), and checks that every "--
// launch.json: ..." caption really introduces one.
func assertGuideExamples(t *testing.T, g *cobra.Command, pkg extensionSchema, langs map[string]bool) {
	t.Helper()

	text := g.Long + "\n" + g.Example
	examples := jsonExamples(text)

	var hasLaunch, hasAttach bool

	for _, m := range examples {
		if err := validateExample(pkg, m, langs); err != nil {
			t.Errorf("%v: %v", m, err)
		}

		switch m["request"] {
		case "launch":
			hasLaunch = true
		case "attach":
			hasAttach = true
		}
	}

	assertLaunchJSONCaptionsParse(t, g, text)

	if g.Use == "lang" {
		if len(examples) != 0 {
			t.Errorf("the lang index guide should have no JSON examples of its own, got %d", len(examples))
		}

		return
	}

	if !hasLaunch {
		t.Errorf("%s: no launch example", g.CommandPath())
	}

	if !hasAttach {
		t.Errorf("%s: no attach example", g.CommandPath())
	}
}

// launchJSONCaptionRE matches a "-- launch.json: ..." caption line, the
// package's convention (docs/CONVENTIONS.md § Help text) for introducing a
// launch.json body meant to be copied verbatim.
var launchJSONCaptionRE = regexp.MustCompile(`(?m)^[ \t]*-- launch\.json:.*$`)

// launchJSONCaptionErrors checks that the JSON object right after every
// "-- launch.json:" caption in text really decodes as an "eyedbg" example:
// jsonExamples silently drops anything that fails to decode (by design, so
// one bad "{" doesn't desync the scan), so without a check tied to the
// caption itself, a broken example — the exact text an agent then pastes —
// would only be caught by luck, through the weaker "at least one launch and
// one attach" count in assertGuideExamples. Split out from
// assertLaunchJSONCaptionsParse so the check itself (a trailing comma, a
// caption with no object following it, a wrong "type") is unit-testable
// without a *cobra.Command or a *testing.T.
func launchJSONCaptionErrors(text string) []error {
	var errs []error

	for _, loc := range launchJSONCaptionRE.FindAllStringIndex(text, -1) {
		caption := strings.TrimSpace(text[loc[0]:loc[1]])
		rest := text[loc[1]:]

		start := strings.IndexByte(rest, '{')
		if start < 0 {
			errs = append(errs, fmt.Errorf("%q: no JSON object follows this caption", caption))
			continue
		}

		var m map[string]any
		if err := json.NewDecoder(strings.NewReader(rest[start:])).Decode(&m); err != nil {
			errs = append(errs, fmt.Errorf("%q: JSON does not decode: %w", caption, err))
			continue
		}

		if m["type"] != "eyedbg" {
			errs = append(errs, fmt.Errorf("%q: JSON's \"type\" is %v, want %q", caption, m["type"], "eyedbg"))
		}
	}

	return errs
}

// assertLaunchJSONCaptionsParse reports every error launchJSONCaptionErrors
// finds in text against g.
func assertLaunchJSONCaptionsParse(t *testing.T, g *cobra.Command, text string) {
	t.Helper()

	for _, err := range launchJSONCaptionErrors(text) {
		t.Errorf("%s: %v", g.CommandPath(), err)
	}
}

// singleQuotedInvocation matches a single-quoted 'eyedbg …' span, the
// package's prose convention for naming a command inline (docs/CONVENTIONS.md
// § Help text), as opposed to SKILL.md's markdown code spans. "eyedbg" and
// the rest are separated by \s+, not a literal space, so a span the
// ~100-column wrap broke right after the word ("'eyedbg\nadapters doctor'")
// still matches: the space that would otherwise sit there became the line
// break instead.
var singleQuotedInvocation = regexp.MustCompile(`'eyedbg\s+([^']*)'`)

// longInvocations finds every single-quoted 'eyedbg …' span in long's prose.
// A span the ~100-column wrap (docs/CONVENTIONS.md) broke across a line
// still reads as one invocation: internal whitespace (including the
// newline) is collapsed to single spaces first.
func longInvocations(long string) [][]string {
	var out [][]string

	for _, m := range singleQuotedInvocation.FindAllStringSubmatch(long, -1) {
		collapsed := strings.Join(strings.Fields(m[1]), " ")
		out = append(out, splitShellWords(truncateAtDelimiter(collapsed)))
	}

	return out
}

// tableCellRE splits a table row into cells on runs of 2+ spaces, the lang
// index guide's own table format (guides.go's langLong).
var tableCellRE = regexp.MustCompile(`  +`)

// tableInvocations finds every "eyedbg …" invocation sitting in its own
// cell of a table row in long: it is neither single-quoted prose (matched by
// longInvocations) nor an Example line (matched by exampleInvocations), so
// without this the lang index's INSTALL and GUIDE columns are never checked
// at all.
func tableInvocations(long string) [][]string {
	var out [][]string

	for line := range strings.SplitSeq(long, "\n") {
		for _, cell := range tableCellRE.Split(strings.TrimSpace(line), -1) {
			if !strings.HasPrefix(cell, "eyedbg ") {
				continue
			}

			out = append(out, splitShellWords(truncateAtDelimiter(strings.TrimPrefix(cell, "eyedbg"))))
		}
	}

	return out
}

// exampleInvocations finds every Example line that is itself an "eyedbg …"
// invocation (its trimmed text starts with "eyedbg "): a launch.json body
// line or a "-- caption" line never does, so nothing else needs skipping.
func exampleInvocations(example string) [][]string {
	var out [][]string

	for line := range strings.SplitSeq(example, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "eyedbg ") {
			continue
		}

		out = append(out, splitShellWords(truncateAtDelimiter(strings.TrimPrefix(trimmed, "eyedbg"))))
	}

	return out
}

// equalInvocationSlices reports whether a and b hold the same invocations in
// the same order.
func equalInvocationSlices(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if !equalStrings(a[i], b[i]) {
			return false
		}
	}

	return true
}

// TestLongInvocations covers longInvocations directly: reverting its regex
// from `'eyedbg\s+` back to a literal `'eyedbg ` (or dropping the
// whitespace-collapse before splitting) would make it silently miss a span
// the ~100-column wrap broke right after the word, and TestGuideInvocationsResolve
// alone would keep passing (nothing else in a real guide happens to hit that
// exact spot today).
func TestLongInvocations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		long string
		want [][]string
	}{
		{
			name: "plain single-quoted span",
			long: "Run 'eyedbg version' first.",
			want: [][]string{{"version"}},
		},
		{
			name: "span wrapped right after the word",
			long: "See 'eyedbg\nadapters doctor' for details.",
			want: [][]string{{"adapters", "doctor"}},
		},
		{
			name: "span wrapped mid-invocation",
			long: "See 'eyedbg lease\ntake --force' here.",
			want: [][]string{{"lease", "take", "--force"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := longInvocations(tt.long); !equalInvocationSlices(got, tt.want) {
				t.Errorf("longInvocations(%q) = %v, want %v", tt.long, got, tt.want)
			}
		})
	}
}

// TestTableInvocations covers tableInvocations directly, over a row shaped
// like the 'lang' index guide's own table: reverting or removing the
// tableInvocations call would make the INSTALL/GUIDE columns silently
// unchecked again (F2's second hole), and TestGuideInvocationsResolve alone
// wouldn't show it, since the other scanners already find enough
// invocations elsewhere in the same guide to pass its "found something"
// check.
func TestTableInvocations(t *testing.T) {
	t.Parallel()

	long := "LANG      INSTALL                                GUIDE\n" +
		"dotnet    eyedbg adapters install netcoredbg      eyedbg help lang dotnet\n"

	want := [][]string{
		{"adapters", "install", "netcoredbg"},
		{"help", "lang", "dotnet"},
	}

	if got := tableInvocations(long); !equalInvocationSlices(got, want) {
		t.Errorf("tableInvocations(%q) = %v, want %v", long, got, want)
	}
}

// TestLaunchJSONCaptionErrors covers launchJSONCaptionErrors directly: a
// trailing comma, a caption with no JSON object after it, and a wrong
// "type" must all be reported, and a caption whose JSON is a real "eyedbg"
// example must not be.
func TestLaunchJSONCaptionErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		wantErr bool
	}{
		{
			name: "valid caption",
			text: "-- launch.json:\n{\"type\": \"eyedbg\", \"request\": \"launch\"}\n",
		},
		{
			name:    "no object follows the caption",
			text:    "-- launch.json:\nsee below\n",
			wantErr: true,
		},
		{
			name:    "trailing comma",
			text:    "-- launch.json:\n{\"type\": \"eyedbg\",}\n",
			wantErr: true,
		},
		{
			name:    "wrong type",
			text:    "-- launch.json:\n{\"type\": \"other\"}\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			errs := launchJSONCaptionErrors(tt.text)
			if (len(errs) > 0) != tt.wantErr {
				t.Errorf("launchJSONCaptionErrors(%q) = %v, want error = %v", tt.text, errs, tt.wantErr)
			}
		})
	}
}

// TestGuideInvocationsResolve keeps a guide's own invocations honest: every
// "eyedbg …" it names, in its Long (single-quoted prose), its Example
// (command lines) or its "help …" cross-references, resolves against the
// real command tree.
func TestGuideInvocationsResolve(t *testing.T) {
	t.Parallel()

	for _, g := range allGuides() {
		t.Run(g.CommandPath(), func(t *testing.T) {
			t.Parallel()

			invocations := longInvocations(g.Long)
			invocations = append(invocations, exampleInvocations(g.Example)...)
			invocations = append(invocations, tableInvocations(g.Long)...)

			if len(invocations) == 0 {
				t.Fatalf("%s: no 'eyedbg …' invocations found to check", g.CommandPath())
			}

			// cobra's Find/LocalFlags merges flags into the tree: one root
			// per subtest (skill_test.go's TestSkillInvocationResolution
			// follows the same rule) — sharing one across parallel
			// subtests races on cobra's own lazy flag-merging.
			root := rootFactories()["eyedbg"]()

			for _, args := range invocations {
				if err := resolveInvocation(root, args); err != nil {
					t.Errorf("%s: %q does not resolve: %v", g.CommandPath(), "eyedbg "+strings.Join(args, " "), err)
				}

				if err := resolveHelpInvocation(root, args); err != nil {
					t.Errorf("%s: %v", g.CommandPath(), err)
				}
			}
		})
	}
}

// TestVSCodeGuideNamesExtensionIdentity checks that the vscode guide names
// the extension's real id, publisher and debug type, read from
// package.json, not a copy that can drift.
func TestVSCodeGuideNamesExtensionIdentity(t *testing.T) {
	t.Parallel()

	pkg := loadExtensionSchema(t)
	long := newVSCodeGuide().Long

	id := pkg.Publisher + "." + pkg.Name
	if !strings.Contains(long, id) {
		t.Errorf("vscode guide does not name the extension id %q", id)
	}

	if !strings.Contains(long, pkg.Publisher) {
		t.Errorf("vscode guide does not name the publisher %q", pkg.Publisher)
	}

	debugType := pkg.Contributes.Debuggers[0].Type
	if !strings.Contains(long, `"`+debugType+`"`) {
		t.Errorf("vscode guide does not name the debug type %q", debugType)
	}
}

// TestLangGuideUnknownLanguageFails checks that an unknown language under
// 'lang' exits 1 (cobra's own "unknown command" handling), both directly
// and through 'help'.
func TestLangGuideUnknownLanguageFails(t *testing.T) {
	t.Parallel()

	tests := [][]string{
		{"lang", "ruby"},
		{"help", "lang", "ruby"},
	}

	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()

			_, _, code := execute(t, NewEyedbgCommand(testInfo), args)
			if code != 1 {
				t.Errorf("eyedbg %s: exit code = %d, want 1", strings.Join(args, " "), code)
			}
		})
	}
}

// TestVSCodeGuideServerReadyActionCount pins the count of the two
// step-1-verified serverReadyAction examples (dotnet, flask): a guide's
// launch.json bodies must be strict JSON (no comments, no trailing commas),
// which assertLaunchJSONCaptionsParse already requires of every captioned
// example; this additionally catches one of the two silently losing its
// serverReadyAction key while staying otherwise valid JSON.
func TestVSCodeGuideServerReadyActionCount(t *testing.T) {
	t.Parallel()

	examples := jsonExamples(newVSCodeGuide().Long)

	var serverReady int

	for _, m := range examples {
		if _, ok := m["serverReadyAction"]; ok {
			serverReady++
		}
	}

	if serverReady != 2 {
		t.Errorf("vscode guide: found %d serverReadyAction examples, want 2 (dotnet, flask)", serverReady)
	}
}

// TestGuidesReferencedFromRootAndDap checks that the root and 'dap'
// commands' Long text point at the new guides, so an agent reading either
// finds them.
func TestGuidesReferencedFromRootAndDap(t *testing.T) {
	t.Parallel()

	if !strings.Contains(eyedbgLong, "eyedbg help lang") {
		t.Error("root Long does not point to 'eyedbg help lang'")
	}

	if !strings.Contains(eyedbgLong, "eyedbg help vscode") {
		t.Error("root Long does not point to 'eyedbg help vscode'")
	}

	if !strings.Contains(dapLong, "eyedbg help vscode") {
		t.Error("dap Long does not point to 'eyedbg help vscode'")
	}

	if !strings.Contains(dapLong, "eyedbg help lang") {
		t.Error("dap Long does not point to 'eyedbg help lang'")
	}
}
