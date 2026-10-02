// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	testID    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testImage = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// TestInspectTemplateSelectsOnlyWhatIsRead pins what eyedbg asks docker for:
// never the environment, the command, an entrypoint's arguments or a
// healthcheck's command.
func TestInspectTemplateSelectsOnlyWhatIsRead(t *testing.T) {
	t.Parallel()

	for _, forbidden := range []string{"Env", "Cmd", "Args", "Entrypoint", "Shell", "ExposedPorts", "Volumes", "Binds", "Mounts", ".Config}}", "json .}}", "json .Config}}", "json .Config."} {
		if strings.Contains(inspectTemplate, forbidden) {
			t.Errorf("inspectTemplate names %q", forbidden)
		}
	}

	// Config is reached only for the healthcheck and the labels, each by key.
	var config []string

	for part := range strings.SplitSeq(inspectTemplate, ".Config") {
		config = append(config, part)
	}

	for _, part := range config[1:] {
		if !strings.HasPrefix(part, ` "Healthcheck"`) && !strings.HasPrefix(part, ".Labels ") {
			t.Errorf("inspectTemplate reads .Config%.30s", part)
		}
	}

	// Test is read only to compare it with NONE: never printed.
	if !strings.Contains(inspectTemplate, `eq (index . 0) "NONE"`) || strings.Contains(inspectTemplate, `json (index . "Test")`) {
		t.Error("the healthcheck's Test must only be compared with NONE")
	}
}

func inspectJSON(mod func(f []string)) []byte {
	f := []string{
		`"` + testID + `"`, `"/web-1"`, `"` + testImage + `"`, `"dotnet"`, `true`, `false`, `false`, `null`, `null`,
		`"my-app"`, `"web"`, `"/home/me/app"`, `""`,
	}
	if mod != nil {
		mod(f)
	}

	return []byte("[" + strings.Join(f, ",") + "]")
}

type inspectCase struct {
	name string
	mod  func(f []string)
	// want is "" when i is as the case expects, else what is wrong.
	want func(i Info) string
}

func stateCases() []inspectCase {
	return []inspectCase{
		{"init", func(f []string) { f[7] = "true" }, func(i Info) string { return wrongIf(!i.Init, "Init false") }},
		{"paused and restarting", func(f []string) { f[5], f[6] = "true", "true" }, func(i Info) string {
			return wrongIf(!i.Paused || !i.Restarting, "Paused or Restarting false")
		}},
		{"fast mode", func(f []string) { f[12] = `"/x/override.yml"` }, func(i Info) string { return wrongIf(!i.FastMode, "FastMode false") }},
	}
}

func healthCases() []inspectCase {
	return []inspectCase{
		{"healthcheck", func(f []string) { f[8] = `[5000000000,3000000000,3,false]` }, func(i Info) string {
			ok := i.Health != nil && *i.Health == Health{Interval: 5 * time.Second, Timeout: 3 * time.Second, Retries: 3} && i.UnhealthyAfter() == 18*time.Second

			return wrongIf(!ok, "wrong healthcheck")
		}},
		{"healthcheck defaults", func(f []string) { f[8] = `[null,null,null,false]` }, func(i Info) string {
			return wrongIf(i.UnhealthyAfter() != 3*30*time.Second+30*time.Second, "not docker's 3 x 30s + 30s")
		}},
		{"healthcheck NONE", func(f []string) { f[8] = `[null,null,null,true]` }, func(i Info) string {
			return wrongIf(i.Health != nil || i.UnhealthyAfter() != 0, "NONE must mean no healthcheck")
		}},
		{"huge healthcheck cannot overflow", func(f []string) { f[8] = `[9e18,9e18,9e18,false]` }, func(i Info) string {
			return wrongIf(i.UnhealthyAfter() <= 0, "overflowed")
		}},
	}
}

func labelCases() []inspectCase {
	return []inspectCase{
		{"labels that fail their grammar are dropped", func(f []string) {
			f[9], f[10], f[11] = `"UPPER case"`, `"a b"`, `"/x\ny"`
		}, func(i Info) string {
			return wrongIf(i.Project != "" || i.Service != "" || i.WorkingDir != "", "a bad label was kept")
		}},
		{"missing labels", func(f []string) { f[9], f[10], f[11] = `""`, `""`, `""` }, func(i Info) string {
			return wrongIf(i.Project != "" || i.Service != "" || i.WorkingDir != "", "labels from nowhere")
		}},
		{"null labels (docker 26 for a missing key)", func(f []string) { f[9], f[10], f[11], f[12] = `null`, `null`, `null`, `null` }, func(i Info) string {
			return wrongIf(i.Project != "" || i.Service != "" || i.WorkingDir != "" || i.FastMode, "labels from nowhere")
		}},
	}
}

func wrongIf(bad bool, msg string) string {
	if bad {
		return msg
	}

	return ""
}

func TestParseInspect(t *testing.T) {
	t.Parallel()

	got, err := parseInspect(inspectJSON(nil))
	if err != nil {
		t.Fatal(err)
	}

	want := Info{
		ID: testID, Name: "web-1", Image: testImage, Path: "dotnet", Running: true,
		Project: "my-app", Service: "web", WorkingDir: "/home/me/app",
	}
	if got != want {
		t.Errorf("parseInspect = %+v, want %+v", got, want)
	}

	for _, tt := range slices.Concat(stateCases(), healthCases(), labelCases()) {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			i, err := parseInspect(inspectJSON(tt.mod))
			if err != nil {
				t.Fatal(err)
			}

			if msg := tt.want(i); msg != "" {
				t.Errorf("%s: %+v", msg, i)
			}
		})
	}
}

func TestParseInspectFailsClosed(t *testing.T) {
	t.Parallel()

	bad := map[string][]byte{
		"empty":            nil,
		"not an array":     []byte(`{"a":1}`),
		"too short":        []byte(`["x"]`),
		"trailing data":    append(inspectJSON(nil), []byte(` {}`)...),
		"short id":         inspectJSON(func(f []string) { f[0] = `"abc123"` }),
		"id not hex":       inspectJSON(func(f []string) { f[0] = `"` + strings.Repeat("g", 64) + `"` }),
		"name is a flag":   inspectJSON(func(f []string) { f[1] = `"/--privileged"` }),
		"name has a space": inspectJSON(func(f []string) { f[1] = `"/a b"` }),
		"image is a flag":  inspectJSON(func(f []string) { f[2] = `"--format"` }),
		"running not bool": inspectJSON(func(f []string) { f[4] = `"yes"` }),
		"init not bool":    inspectJSON(func(f []string) { f[7] = `1` }),
		"health shape":     inspectJSON(func(f []string) { f[8] = `[1,2,3]` }),
		"health types":     inspectJSON(func(f []string) { f[8] = `["a",null,null,false]` }),
		"label not string": inspectJSON(func(f []string) { f[9] = `5` }),
	}

	for name, out := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := parseInspect(out); err == nil {
				t.Error("parseInspect accepted it")
			}
		})
	}
}

func TestProcessName(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"/usr/share/dotnet/dotnet": "dotnet",
		"dotnet":                   "dotnet",
		"/bin/sh":                  "sh",
		"/x/a\nb":                  "a b",
		"":                         ".",
	}

	for path, want := range tests {
		if got := (Info{Path: path}).ProcessName(); got != want {
			t.Errorf("ProcessName(%q) = %q, want %q", path, got, want)
		}
	}

	if got := (Info{Path: "/" + strings.Repeat("a", 500)}).ProcessName(); len(got) != 64 {
		t.Errorf("ProcessName of a long path has %d bytes, want 64", len(got))
	}
}

func TestWithoutEngineVars(t *testing.T) {
	t.Parallel()

	env := []string{"PATH=/bin", "DOCKER_HOST=tcp://stale:1", "DOCKER_CONTEXT=old", "docker_host=x", "DOCKER_CONFIG=/home/me/.docker", "DOCKER_HOSTILE=1", "HOME=/h"}

	got := withoutEngineVars(env)
	want := []string{"PATH=/bin", "DOCKER_CONFIG=/home/me/.docker", "DOCKER_HOSTILE=1", "HOME=/h"}

	if !slices.Equal(got, want) {
		t.Errorf("withoutEngineVars = %q, want %q", got, want)
	}

	if len(env) != 7 {
		t.Error("withoutEngineVars changed its input")
	}
}
