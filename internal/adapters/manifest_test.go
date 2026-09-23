// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"encoding/json"
	"strings"
	"testing"
)

// validManifest returns a valid manifest of a Python-hosted language as
// JSON-shaped data, for tests to change one field of.
func validManifest() map[string]any {
	return map[string]any{
		"schema": 1, "name": "toy", "description": "A toy adapter", "homepage": "https://example.com/toy",
		"version": "1.0.0",
		"adapter": map[string]any{"id": "toy", "transport": "stdio", "runtime": "python", "entry": "toy/adapter"},
		"python": map[string]any{
			"env": "TOY_PYTHON", "option": "python", "commands": []any{[]any{"python3"}, []any{"py", "-3"}},
			"venvs": []any{".venv"}, "minVersion": "3.10", "module": "toy",
		},
		"install": map[string]any{"downloads": map[string]any{"*": map[string]any{
			"url": "https://example.com/toy.zip", "sha256": strings.Repeat("ab", 32), "archive": "zip", "root": "", "size": 10,
		}}},
		"language": map[string]any{"name": "toylang", "extensions": []any{".toy"}, "markers": []any{"toy.cfg", "*.toyproj"}},
		"options": map[string]any{
			"python": map[string]any{"help": "the interpreter"},
			"module": map[string]any{"help": "a module"},
			"fast":   map[string]any{"type": "bool", "default": "true", "help": "go fast"},
			"level":  map[string]any{"type": "int", "default": "2", "help": "a level"},
		},
		"launch": map[string]any{
			"require": []any{"program", "opt.module"},
			"arguments": map[string]any{
				"program": "${program}", "module": "${opt.module}", "args": "${args}", "python": "${runtime}",
				"stop": "${stopOnEntry}", "fast": "${opt.fast}", "title": "run ${program} at ${opt.level}",
			},
		},
		"attach":            map[string]any{"arguments": map[string]any{"processId": "${pid}"}},
		"attachUnsupported": "no attach",
		"exceptions":        map[string]any{"all": []any{"raised"}, "uncaught": []any{"uncaught"}},
		"evalGuard": map[string]any{
			"quotes": []any{"'", "\""}, "interpolatedPrefixes": []any{"f"}, "nonCallWords": []any{"and"},
			"safeCalls": []any{"len"}, "assignOps": []any{"=", ":="},
		},
	}
}

// at returns the object at the dotted path in m.
func at(m map[string]any, path string) map[string]any {
	for key := range strings.SplitSeq(path, ".") {
		if key == "" {
			continue
		}

		next, ok := m[key].(map[string]any)
		if !ok {
			panic("test manifest has no object at " + path)
		}

		m = next
	}

	return m
}

func TestParseValid(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}

	m, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(valid) = %v", err)
	}

	if m.Name != "toy" || m.LanguageName() != "toylang" || m.Builtin() || m.Adapter.Runtime != RuntimePython ||
		m.Options["fast"].Kind() != OptionBool || m.Options["python"].Kind() != OptionString {
		t.Fatalf("parsed = %+v", m)
	}

	if d, ok := m.DownloadFor("linux", "amd64"); !ok || d.Archive != archiveZip {
		t.Fatalf("DownloadFor = %+v, %v; want the * download", d, ok)
	}
}

func TestParseInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		change func(m map[string]any)
		want   string
	}{
		{"unknown field", func(m map[string]any) { m["capabilities"] = map[string]any{} }, `unknown field "capabilities"`},
		{"unknown nested field", func(m map[string]any) { at(m, "adapter")["port"] = 1 }, `unknown field "port"`},
		{"schema 2", func(m map[string]any) { m["schema"] = 2 }, "schema: 2 is not supported"},
		{"no schema", func(m map[string]any) { delete(m, "schema") }, "schema: 0 is not supported"},
		{"bad name", func(m map[string]any) { m["name"] = "Toy" }, "name:"},
		{"long name", func(m map[string]any) { m["name"] = "a" + strings.Repeat("b", 32) }, "name:"},
		{"missing description", func(m map[string]any) { delete(m, "description") }, "description: is required"},
		{"long description", func(m map[string]any) { m["description"] = strings.Repeat("x", 121) }, "description: is longer"},
		{"bad version", func(m map[string]any) { m["version"] = "1 0" }, "version:"},
		{"missing version", func(m map[string]any) { delete(m, "version") }, "version:"},
		{"http homepage", func(m map[string]any) { m["homepage"] = "http://example.com" }, "homepage:"},
		{"missing adapter id", func(m map[string]any) { delete(at(m, "adapter"), "id") }, "adapter.id: is required"},
		{"tcp transport", func(m map[string]any) { at(m, "adapter")["transport"] = "tcp" }, "not supported yet"},
		{"unknown runtime", func(m map[string]any) { at(m, "adapter")["runtime"] = "node" }, "adapter.runtime:"},
		{"missing entry", func(m map[string]any) { delete(at(m, "adapter"), "entry") }, "adapter.entry: is required"},
		{"python entry with ..", func(m map[string]any) { at(m, "adapter")["entry"] = "../x/adapter" }, "adapter.entry:"},
		{"absolute python entry", func(m map[string]any) { at(m, "adapter")["entry"] = "/x/adapter" }, "adapter.entry:"},
		{"python entry with backslash", func(m map[string]any) { at(m, "adapter")["entry"] = `toy\adapter` }, "adapter.entry:"},
		{"python adapter with env", func(m map[string]any) { at(m, "adapter")["env"] = "TOY" }, "only for native adapters"},
		{"python runtime without python", func(m map[string]any) { delete(m, "python") }, "python: is required"},
		{"python without python runtime", func(m map[string]any) {
			at(m, "adapter")["runtime"] = ""
			at(m, "adapter")["entry"] = "toy"
		}, "python: is only for adapter.runtime python"},
		{"native entry with separator", func(m map[string]any) {
			delete(m, "python")
			at(m, "adapter")["runtime"] = ""
			at(m, "adapter")["entry"] = "bin/toy"
			delete(at(m, "launch.arguments"), "python")
		}, "adapter.entry:"},
		{"bad environment name", func(m map[string]any) { at(m, "adapter")["environment"] = map[string]any{"A-B": "1"} }, "adapter.environment:"},
		{"no python commands", func(m map[string]any) { at(m, "python")["commands"] = []any{} }, "python.commands:"},
		{"empty python command", func(m map[string]any) { at(m, "python")["commands"] = []any{[]any{}} }, "python.commands:"},
		{"bad python module", func(m map[string]any) { at(m, "python")["module"] = "toy-x" }, "python.module:"},
		{"bad minVersion", func(m map[string]any) { at(m, "python")["minVersion"] = "3" }, "python.minVersion:"},
		{"python option not declared", func(m map[string]any) { at(m, "python")["option"] = "interp" }, "python.option:"},
		{"python option not a string", func(m map[string]any) { at(m, "python")["option"] = "fast" }, "python.option:"},
		{"venv with separator", func(m map[string]any) { at(m, "python")["venvs"] = []any{"a/b"} }, "python.venvs:"},
		{"no downloads", func(m map[string]any) { at(m, "install")["downloads"] = map[string]any{} }, "install.downloads:"},
		{"http url", func(m map[string]any) { at(m, "install.downloads.*")["url"] = "http://example.com/toy.zip" }, "must be an https URL"},
		{"short sha256", func(m map[string]any) { at(m, "install.downloads.*")["sha256"] = "abc" }, "sha256"},
		{"upper-case sha256", func(m map[string]any) { at(m, "install.downloads.*")["sha256"] = strings.Repeat("AB", 32) }, "sha256"},
		{"unknown platform", func(m map[string]any) {
			at(m, "install.downloads")["plan9/amd64"] = at(m, "install.downloads.*")
		}, "install.downloads[plan9/amd64]"},
		{"rar archive", func(m map[string]any) { at(m, "install.downloads.*")["archive"] = "rar" }, "archive"},
		{"root with ..", func(m map[string]any) { at(m, "install.downloads.*")["root"] = "../x" }, "root:"},
		{"negative size", func(m map[string]any) { at(m, "install.downloads.*")["size"] = -1 }, "negative"},
		{"bad language name", func(m map[string]any) { at(m, "language")["name"] = "Toy" }, "language.name:"},
		{"builtin with launch", func(m map[string]any) { at(m, "language")["builtin"] = true }, "language.builtin:"},
		{"non-builtin without launch", func(m map[string]any) { delete(m, "launch") }, "launch: is required"},
		{"launch without language", func(m map[string]any) { delete(m, "language") }, "language: is required"},
		{"bad extension", func(m map[string]any) { at(m, "language")["extensions"] = []any{"toy"} }, "language.extensions:"},
		{"bad marker", func(m map[string]any) { at(m, "language")["markers"] = []any{"a/b.cfg"} }, "language.markers:"},
		{"bad option name", func(m map[string]any) { at(m, "options")["no-dash"] = map[string]any{"help": "x"} }, "options:"},
		{"bad option type", func(m map[string]any) { at(m, "options.fast")["type"] = "float" }, "options.fast.type:"},
		{"bad bool default", func(m map[string]any) { at(m, "options.fast")["default"] = "yes" }, "options.fast.default:"},
		{"bad int default", func(m map[string]any) { at(m, "options.level")["default"] = "two" }, "options.level.default:"},
		{"option without help", func(m map[string]any) { delete(at(m, "options.module"), "help") }, "options.module.help:"},
		{"require undeclared option", func(m map[string]any) { at(m, "launch")["require"] = []any{"opt.nope"} }, "launch.require:"},
		{"require unknown input", func(m map[string]any) { at(m, "launch")["require"] = []any{"cwd"} }, "launch.require:"},
		{"empty require", func(m map[string]any) { at(m, "launch")["require"] = []any{} }, "launch.require:"},
		{"no launch arguments", func(m map[string]any) { delete(at(m, "launch"), "arguments") }, "launch.arguments: is required"},
		{"exceptions key none", func(m map[string]any) { at(m, "exceptions")["none"] = []any{"x"} }, "exceptions:"},
		{"empty filter list", func(m map[string]any) { at(m, "exceptions")["all"] = []any{} }, "exceptions.all:"},
		{"two-char quote", func(m map[string]any) { at(m, "evalGuard")["quotes"] = []any{"''"} }, "evalGuard.quotes:"},
		{"bad assign op", func(m map[string]any) { at(m, "evalGuard")["assignOps"] = []any{"=a"} }, "evalGuard.assignOps:"},
		{"bad safe call", func(m map[string]any) { at(m, "evalGuard")["safeCalls"] = []any{"a.b"} }, "evalGuard.safeCalls:"},
		{"template unknown variable", func(m map[string]any) { at(m, "launch.arguments")["x"] = "${nope}" }, "${nope} is not a variable"},
		{"pid in launch", func(m map[string]any) { at(m, "launch.arguments")["x"] = "${pid}" }, "only available in attach"},
		{"program in attach", func(m map[string]any) { at(m, "attach.arguments")["x"] = "${program}" }, "attach.arguments:"},
		{"runtime without runtime", func(m map[string]any) {
			delete(m, "python")
			at(m, "adapter")["runtime"] = ""
			at(m, "adapter")["entry"] = "toy"
		}, "${runtime} needs adapter.runtime python"},
		{"interpolated args", func(m map[string]any) { at(m, "launch.arguments")["x"] = "a ${args}" }, "can only stand alone"},
		{"interpolated bool option", func(m map[string]any) { at(m, "launch.arguments")["x"] = "a ${opt.fast}" }, "can only stand alone"},
		{"malformed reference", func(m map[string]any) { at(m, "launch.arguments")["x"] = "${program" }, "without a closing"},
		{"empty reference", func(m map[string]any) { at(m, "launch.arguments")["x"] = "${}" }, "is not a variable name"},
		{"reference in a key", func(m map[string]any) { at(m, "launch.arguments")["${program}"] = "x" }, "only allowed in values"},
		{"unknown option in nested array", func(m map[string]any) {
			at(m, "launch.arguments")["x"] = []any{map[string]any{"y": "${opt.nope}"}}
		}, "no such option"},
		{"attach require", func(m map[string]any) { at(m, "attach")["require"] = []any{"program"} }, "attach.require:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := validManifest()
			tt.change(m)

			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}

			_, err = Parse(raw)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

func TestParseTrailingData(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Parse(append(raw, []byte(" {}")...)); err == nil {
		t.Fatal("Parse accepted data after the manifest")
	}
}

func TestOptionParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ, in string
		want    any
		ok      bool
	}{
		{"", "x", "x", true},
		{"string", "", "", true},
		{"bool", "true", true, true},
		{"bool", "false", false, true},
		{"bool", "maybe", nil, false},
		{"int", "5", 5, true},
		{"int", "5.5", nil, false},
	}

	for _, tt := range tests {
		got, err := Option{Type: tt.typ}.Parse(tt.in)
		if (err == nil) != tt.ok || (tt.ok && got != tt.want) {
			t.Errorf("Option{%q}.Parse(%q) = %v, %v; want %v (ok %v)", tt.typ, tt.in, got, err, tt.want, tt.ok)
		}
	}
}
