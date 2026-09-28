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
// configurationAttributes (D4a): every guide except the 'lang' index has at
// least one launch and one attach example, so this can't pass by finding
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
// against pkg and langs, and requires at least one launch and one attach
// example (the 'lang' index guide instead requires none: D5's table has no
// invented examples).
func assertGuideExamples(t *testing.T, g *cobra.Command, pkg extensionSchema, langs map[string]bool) {
	t.Helper()

	examples := jsonExamples(g.Long + "\n" + g.Example)

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

// singleQuotedInvocation matches a single-quoted 'eyedbg …' span, the
// package's prose convention for naming a command inline (docs/CONVENTIONS.md
// § Help text), as opposed to SKILL.md's markdown code spans.
var singleQuotedInvocation = regexp.MustCompile(`'eyedbg ([^']*)'`)

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

// TestGuideInvocationsResolve checks D4b: every "eyedbg …" invocation named
// in a guide's Long (single-quoted prose) or Example (command lines)
// resolves against the real command tree.
func TestGuideInvocationsResolve(t *testing.T) {
	t.Parallel()

	for _, g := range allGuides() {
		t.Run(g.CommandPath(), func(t *testing.T) {
			t.Parallel()

			invocations := longInvocations(g.Long)
			invocations = append(invocations, exampleInvocations(g.Example)...)

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
			}
		})
	}
}

// TestVSCodeGuideNamesExtensionIdentity checks D4a(c): the vscode guide
// names the extension's real id, publisher and debug type, read from
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

// TestLangGuideUnknownLanguageFails checks D1: an unknown language under
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

func TestVSCodeGuideLaunchJSONIsValidJSON(t *testing.T) {
	t.Parallel()

	// A guide's launch.json bodies must be strict JSON (D3: no comments, no
	// trailing commas); jsonExamples already requires this to find them at
	// all, but this pins the count for the two step-1-verified
	// serverReadyAction examples specifically, so a future edit that breaks
	// their JSON silently drops to zero instead of failing loudly.
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
