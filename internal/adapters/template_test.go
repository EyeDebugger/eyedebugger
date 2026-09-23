// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"encoding/json"
	"testing"
)

func TestRender(t *testing.T) {
	t.Parallel()

	full := Vars{
		VarProgram: "/p/app.py", VarArgs: []string{"-x", "y"}, VarCwd: "/p", VarEnv: map[string]string{"A": "1"},
		VarStopOnEntry: false, VarRuntime: "/usr/bin/python3", OptVar("level"): 3, OptVar("fast"): true,
	}
	empty := Vars{VarStopOnEntry: true, VarArgs: []string{}, VarEnv: map[string]string{}, VarProgram: ""}

	tests := []struct {
		name string
		tmpl string
		vars Vars
		want string
	}{
		{
			"typed exact references", `{"p":"${program}","a":"${args}","e":"${env}","s":"${stopOnEntry}","l":"${opt.level}","f":"${opt.fast}"}`, full,
			`{"a":["-x","y"],"e":{"A":"1"},"f":true,"l":3,"p":"/p/app.py","s":false}`,
		},
		{
			"unset members are omitted", `{"p":"${program}","a":"${args}","e":"${env}","m":"${opt.module}","s":"${stopOnEntry}"}`, empty,
			`{"s":true}`,
		},
		{"unset array elements are omitted", `{"x":["a","${program}","${opt.module}","b"]}`, empty, `{"x":["a","b"]}`},
		{"lists are spliced into arrays", `{"x":["--","${args}","end"]}`, full, `{"x":["--","-x","y","end"]}`},
		{"interpolation", `{"t":"run ${program} at ${opt.level} in ${cwd}"}`, full, `{"t":"run /p/app.py at 3 in /p"}`},
		{"interpolating unset is empty", `{"t":"[${program}]"}`, empty, `{"t":"[]"}`},
		{
			"nested objects", `{"o":{"p":"${program}","deep":{"r":"${runtime}"},"gone":"${opt.module}"}}`, full,
			`{"o":{"deep":{"r":"/usr/bin/python3"},"p":"/p/app.py"}}`,
		},
		{
			"literals untouched", `{"n":1,"big":12345678901234567890,"b":true,"z":null,"s":"$ {x} $x","f":1.5}`, full,
			`{"b":true,"big":12345678901234567890,"f":1.5,"n":1,"s":"$ {x} $x","z":null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, err := Parse(manifestWithLaunch(t, tt.tmpl))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			got, err := json.Marshal(Render(m.Launch.Arguments, tt.vars))
			if err != nil {
				t.Fatal(err)
			}

			if string(got) != tt.want {
				t.Errorf("Render = %s\nwant     %s", got, tt.want)
			}
		})
	}
}

// manifestWithLaunch is validManifest with launch arguments tmpl (JSON).
func manifestWithLaunch(t *testing.T, tmpl string) []byte {
	t.Helper()

	m := validManifest()
	at(m, "launch")["arguments"] = json.RawMessage(tmpl)

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

func TestScan(t *testing.T) {
	t.Parallel()

	if name, ok := exactRef("${opt.x}"); !ok || name != "opt.x" {
		t.Errorf("exactRef(${opt.x}) = %q, %v", name, ok)
	}

	for _, s := range []string{"a${x}", "${x}${y}", "${x} ", "x"} {
		if _, ok := exactRef(s); ok {
			t.Errorf("exactRef(%q) = true, want false", s)
		}
	}
}
