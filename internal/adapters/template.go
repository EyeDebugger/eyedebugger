// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Template variables (docs/adapter-manifests.md § Templates).
const (
	// VarProgram is --program (string).
	VarProgram = "program"
	// VarArgs are the program's arguments (list).
	VarArgs = "args"
	// VarCwd is the program's working directory (string).
	VarCwd = "cwd"
	// VarEnv is the program's extra environment (map).
	VarEnv = "env"
	// VarStopOnEntry is --stop-on-entry (bool, always set).
	VarStopOnEntry = "stopOnEntry"
	// VarRuntime is the interpreter of a Python adapter (string).
	VarRuntime = "runtime"
	// VarPID is the process to attach to (int; attach only).
	VarPID = "pid"

	varProgram = VarProgram
	optPrefix  = "opt."
)

// OptVar is the template variable of option name.
func OptVar(name string) string { return optPrefix + name }

// Vars are the values of template variables: string, int, bool, []string
// or map[string]string. A missing variable, an empty string, list or map
// is unset.
type Vars map[string]any

// varKind is a template variable's type.
type varKind int

const (
	kindNone varKind = iota
	kindString
	kindInt
	kindBool
	kindList
	kindMap
)

// refPattern is a variable name inside ${...}.
const refPattern = `^[A-Za-z][A-Za-z0-9]*(\.[A-Za-z][A-Za-z0-9]*)?$`

// varKinds returns the variables a launch (or attach) template may use.
func (m *Manifest) varKinds(attach bool) map[string]varKind {
	kinds := map[string]varKind{}

	if attach {
		kinds[VarPID] = kindInt
	} else {
		kinds[VarProgram], kinds[VarArgs], kinds[VarCwd] = kindString, kindList, kindString
		kinds[VarEnv], kinds[VarStopOnEntry] = kindMap, kindBool
	}

	if m.Adapter.Runtime == RuntimePython {
		kinds[VarRuntime] = kindString
	}

	for name, o := range m.Options {
		switch o.Kind() {
		case OptionBool:
			kinds[OptVar(name)] = kindBool
		case OptionInt:
			kinds[OptVar(name)] = kindInt
		default:
			kinds[OptVar(name)] = kindString
		}
	}

	return kinds
}

// part is a piece of a template string: literal text, or a reference.
type part struct {
	text string
	ref  bool
}

// scan splits s into literal text and ${name} references.
func scan(s string) ([]part, error) {
	var parts []part

	for {
		i := strings.Index(s, "${")
		if i < 0 {
			break
		}

		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return nil, fmt.Errorf("%q has a '${' without a closing '}'", s)
		}

		name := s[i+2 : i+end]
		if !regexp.MustCompile(refPattern).MatchString(name) {
			return nil, fmt.Errorf("%q: %q is not a variable name", s, name)
		}

		if i > 0 {
			parts = append(parts, part{text: s[:i]})
		}

		parts = append(parts, part{text: name, ref: true})
		s = s[i+end+1:]
	}

	if s != "" {
		parts = append(parts, part{text: s})
	}

	return parts, nil
}

// exactRef returns the variable s refers to when s is exactly "${name}".
func exactRef(s string) (string, bool) {
	parts, err := scan(s)
	if err != nil || len(parts) != 1 || !parts[0].ref {
		return "", false
	}

	return parts[0].text, true
}

// checkTemplate checks every reference in v against kinds.
func checkTemplate(v any, kinds map[string]varKind) error {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if strings.Contains(k, "${") {
				return fmt.Errorf("key %q: references are only allowed in values", k)
			}

			if err := checkTemplate(e, kinds); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := checkTemplate(e, kinds); err != nil {
				return err
			}
		}
	case string:
		return checkString(t, kinds)
	}

	return nil
}

func checkString(s string, kinds map[string]varKind) error {
	parts, err := scan(s)
	if err != nil {
		return err
	}

	exact := len(parts) == 1 && parts[0].ref

	for _, p := range parts {
		if !p.ref {
			continue
		}

		k := kinds[p.text]

		switch {
		case k == kindNone:
			return unknownVar(p.text)
		case !exact && k != kindString && k != kindInt:
			return fmt.Errorf("%q: ${%s} can only stand alone (it isn't a string or an integer)", s, p.text)
		}
	}

	return nil
}

// unknownVar explains a reference to a variable that isn't available.
func unknownVar(name string) error {
	switch {
	case name == VarPID:
		return errors.New("${pid} is only available in attach")
	case name == VarRuntime:
		return errors.New("${runtime} needs adapter.runtime python")
	case strings.HasPrefix(name, optPrefix):
		return fmt.Errorf("${%s}: no such option", name)
	default:
		return fmt.Errorf("${%s} is not a variable here", name)
	}
}

// Render fills tmpl's references from vars: a value that is exactly
// "${x}" becomes x's typed value, and is omitted (from an object or an
// array) when x is unset; a list inside an array is spliced; references
// inside other text are interpolated (unset: ""). tmpl must have passed
// validation.
func Render(tmpl map[string]any, vars Vars) map[string]any {
	out := make(map[string]any, len(tmpl))

	for k, v := range tmpl {
		if r, ok := renderValue(v, vars); ok {
			out[k] = r
		}
	}

	return out
}

func renderValue(v any, vars Vars) (any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return Render(t, vars), true
	case []any:
		return renderList(t, vars), true
	case string:
		if name, ok := exactRef(t); ok {
			return lookup(vars, name)
		}

		return interpolate(t, vars), true
	default:
		return v, true
	}
}

func renderList(list []any, vars Vars) []any {
	out := make([]any, 0, len(list))

	for _, e := range list {
		if s, ok := e.(string); ok {
			if name, ok := exactRef(s); ok {
				if l, ok := vars[name].([]string); ok {
					for _, x := range l {
						out = append(out, x)
					}

					continue
				}
			}
		}

		if r, ok := renderValue(e, vars); ok {
			out = append(out, r)
		}
	}

	return out
}

// lookup returns name's value and whether it is set.
func lookup(vars Vars, name string) (any, bool) {
	v, ok := vars[name]
	if !ok {
		return nil, false
	}

	switch t := v.(type) {
	case string:
		return t, t != ""
	case []string:
		return t, len(t) > 0
	case map[string]string:
		return t, len(t) > 0
	default:
		return v, true
	}
}

func interpolate(s string, vars Vars) string {
	parts, err := scan(s)
	if err != nil {
		return s
	}

	var b strings.Builder

	for _, p := range parts {
		if !p.ref {
			b.WriteString(p.text)

			continue
		}

		switch v := vars[p.text].(type) {
		case string:
			b.WriteString(v)
		case int:
			b.WriteString(strconv.Itoa(v))
		}
	}

	return b.String()
}
