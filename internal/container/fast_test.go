// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"text/template"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

const imageID = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// pinnedFastTemplate is the inspect template of InspectFast, spelled out: a
// change to it must be made here too, on purpose, after checking it still
// reads nothing but the fields docs/adr/0021 (D12) lists.
const pinnedFastTemplate = `[{{json .Id}},{{json .Name}},{{json .Image}},{{json .Platform}},{{json .State.Running}},` +
	`{{json .State.Paused}},{{json .State.Restarting}},{{with .Config.Entrypoint}}{{if and (eq (len .) 2) (eq (index . 0) "dotnet")}}` +
	`{{json (index . 1)}}{{else}}null{{end}}{{else}}null{{end}},{{if .Config.Cmd}}true{{else}}false{{end}},{{json .Config.WorkingDir}},` +
	`{{with .Config.Labels}}{{json (index . "com.docker.compose.project")}},{{json (index . "com.docker.compose.service")}},` +
	`{{json (index . "com.docker.compose.project.working_dir")}},{{json (index . "com.docker.compose.project.config_files")}},` +
	`{{json (index . "dev.izzat.eyedbg.fast.version")}},{{json (index . "dev.izzat.eyedbg.fast.override")}},` +
	`{{json (index . "dev.izzat.eyedbg.fast.dll")}},{{json (index . "dev.izzat.eyedbg.fast.workdir")}},` +
	`{{json (index . "dev.izzat.eyedbg.fast.project")}}{{else}}null,null,null,null,null,null,null,null,null{{end}}]`

// fastTemplateOf runs InspectFast once against a fake docker answering out
// and returns the template it sent and the call's argv.
func fastTemplateOf(t *testing.T, out string) (tpl string, argv []string) {
	t.Helper()

	calls := filepath.Join(t.TempDir(), "calls")
	e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{
		{Match: []string{"inspect"}, Stdout: out},
	}})

	_, _ = e.InspectFast(t.Context(), "web-1") // the answer may be invalid: only the call matters

	got := containertest.ReadCalls(t, calls)
	if len(got) != 1 {
		t.Fatalf("%d docker calls, want 1: %q", len(got), got)
	}

	for i, a := range got[0] {
		if a == "--format" {
			return got[0][i+1], got[0]
		}
	}

	t.Fatalf("no --format in %q", got[0])

	return "", nil
}

func TestInspectFastTemplateIsPinned(t *testing.T) {
	t.Parallel()

	tpl, argv := fastTemplateOf(t, "[]")

	if tpl != pinnedFastTemplate {
		t.Fatalf("the inspect template changed:\n got %s\nwant %s", tpl, pinnedFastTemplate)
	}

	want := []string{"inspect", "--type", "container", "--format", tpl, "--", "web-1"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}

	// What it must never name, whatever the pin says.
	for _, banned := range []string{".Config.Env", ".Env", "Args", "Healthcheck", "Test", ".HostConfig", "env_file", "Secrets"} {
		if strings.Contains(tpl, banned) {
			t.Errorf("the template names %s", banned)
		}
	}

	// Cmd only as a presence check; Entrypoint only inside the shape gate,
	// whose only output is the second element.
	if strings.Count(tpl, "Cmd") != 1 || !strings.Contains(tpl, "{{if .Config.Cmd}}true{{else}}false{{end}}") {
		t.Error("Cmd is named outside a presence check")
	}

	if strings.Count(tpl, "Entrypoint") != 1 || !strings.Contains(tpl, `{{with .Config.Entrypoint}}{{if and (eq (len .) 2) (eq (index . 0) "dotnet")}}{{json (index . 1)}}{{else}}null{{end}}{{else}}null{{end}}`) {
		t.Error("Entrypoint is named outside the shape gate")
	}

	if strings.Count(tpl, "index . 1") != 1 || strings.Count(tpl, "index . 0") != 1 {
		t.Error("the entrypoint's elements are read in more places than the gate")
	}
}

// dockerJSON renders template over an inspect document the way docker does
// (a map decoded from JSON, a missing key an error, json encoding without
// HTML escaping).
func dockerJSON(t *testing.T, tpl string, doc map[string]any) string {
	t.Helper()

	funcs := template.FuncMap{"json": func(v any) (string, error) {
		var buf bytes.Buffer

		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)

		if err := enc.Encode(v); err != nil {
			return "", err //nolint:wrapcheck // A test helper.
		}

		return strings.TrimSpace(buf.String()), nil
	}}

	tmpl, err := template.New("inspect").Funcs(funcs).Option("missingkey=error").Parse(tpl)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, doc); err != nil {
		t.Fatalf("the template fails on %v: %v", doc, err)
	}

	return out.String()
}

func inspectDoc(entrypoint, cmd, labels any) map[string]any {
	return map[string]any{
		"Id": fullID, "Name": "/web-1", "Image": imageID, "Platform": "linux",
		"State": map[string]any{"Running": true, "Paused": false, "Restarting": false},
		"Args":  []any{"canary-args"},
		"Config": map[string]any{
			"Entrypoint": entrypoint, "Cmd": cmd, "WorkingDir": "/app", "Labels": labels,
			"Env": []any{"EYEDBG_CANARY_ENV=canary-env", "TOKEN=canary-token"},
			"Healthcheck": map[string]any{
				"Test": []any{"CMD", "curl", "--header", "Authorization: canary-health"}, "Interval": 5e9,
			},
		},
		"HostConfig": map[string]any{"Init": true},
	}
}

// TestInspectFastTemplateReadsOnlyItsFields executes the real template over
// documents full of canaries (environment, arguments, command, healthcheck,
// entrypoint elements) and requires that none reaches the output.
func TestInspectFastTemplateReadsOnlyItsFields(t *testing.T) {
	t.Parallel()

	tpl, _ := fastTemplateOf(t, "[]")

	labels := map[string]any{
		"com.docker.compose.project": "myapp", "com.docker.compose.service": "web",
		"com.docker.compose.project.working_dir": t.TempDir(), "com.docker.compose.other": "canary-label",
		"dev.izzat.eyedbg.fast.canary": "canary-fast-label",
	}

	tests := []struct {
		name       string
		entrypoint any
		cmd        any
		labels     any
		wantDLL    string
		wantCmd    bool
	}{
		{"exact dotnet shape", []any{"dotnet", "Web.dll"}, nil, labels, "Web.dll", false},
		{"cmd present", []any{"dotnet", "Web.dll"}, []any{"--x", "canary-cmd"}, labels, "Web.dll", true},
		{"three elements", []any{"dotnet", "Web.dll", "canary-ep"}, nil, labels, "", false},
		{"one element", []any{"canary-ep"}, nil, labels, "", false},
		{"other program", []any{"sh", "canary-ep"}, nil, labels, "", false},
		{"eight elements", []any{"/opt/Rider/Worker", "a", "b", "c", "d", "e", "f", "canary-ep"}, nil, labels, "", false},
		{"null entrypoint", nil, nil, labels, "", false},
		{"empty entrypoint", []any{}, []any{"canary-cmd"}, labels, "", true},
		{"no labels at all", []any{"dotnet", "Web.dll"}, nil, nil, "Web.dll", false},
		{"empty labels", []any{"dotnet", "Web.dll"}, nil, map[string]any{}, "Web.dll", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := dockerJSON(t, tpl, inspectDoc(tt.entrypoint, tt.cmd, tt.labels))

			if strings.Contains(out, "canary") {
				t.Errorf("a canary reached the output: %s", out)
			}

			// Through the real parser too.
			e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{Match: []string{"inspect"}, Stdout: out}}})

			info, err := e.InspectFast(t.Context(), "web-1")
			if err != nil {
				t.Fatalf("InspectFast: %v (%s)", err, out)
			}

			if info.DLL != tt.wantDLL || info.HasCmd != tt.wantCmd || info.WorkDir != "/app" || info.ID != fullID {
				t.Errorf("info = %+v, want DLL %q cmd %v", info, tt.wantDLL, tt.wantCmd)
			}
		})
	}
}

// composeFixture is a compose directory with one project file, and the
// values of the labels InspectFast checks it against.
type composeFixture struct {
	dir, project string
	override     string
}

func newComposeFixture(t *testing.T) composeFixture {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	proj := filepath.Join(dir, "Web", "Web.csproj")
	if err := os.MkdirAll(filepath.Dir(proj), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(proj, []byte("<Project/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	return composeFixture{dir: dir, project: "Web/Web.csproj", override: filepath.Join(t.TempDir(), "override.yml")}
}

// out is a valid answer for the fixture, with any value replaced by set.
func (f composeFixture) out(set map[int]any) string {
	v := []any{
		fullID, "/web-1", imageID, "linux", true, false, false, "Web.dll", false, "/app",
		"myapp", "web", f.dir, filepath.Join(f.dir, "compose.yml") + "," + filepath.Join(f.dir, "compose.override.yml"),
		"1", f.override, "Web.dll", "/app", f.project,
	}

	for i, x := range set {
		v[i] = x
	}

	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return string(b)
}

func inspectFast(t *testing.T, out string) (container.FastInfo, error) {
	t.Helper()

	e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{Match: []string{"inspect"}, Stdout: out}}})

	return e.InspectFast(t.Context(), "web-1")
}

func TestInspectFastFields(t *testing.T) {
	t.Parallel()

	f := newComposeFixture(t)

	got, err := inspectFast(t, f.out(nil))
	if err != nil {
		t.Fatal(err)
	}

	want := container.FastInfo{
		ID: fullID, Name: "web-1", Image: imageID, Platform: "linux", Running: true,
		DLL: "Web.dll", WorkDir: "/app",
		Project: "myapp", Service: "web", ComposeDir: f.dir,
		ConfigFiles: []string{filepath.Join(f.dir, "compose.yml"), filepath.Join(f.dir, "compose.override.yml")},
		Fast: &container.FastLabels{
			Version: "1", Override: f.override, DLL: "Web.dll", WorkDir: "/app",
			Project: f.project, ProjectPath: filepath.Join(f.dir, "Web", "Web.csproj"),
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("info = %+v (fast %+v)\nwant   %+v (fast %+v)", got, got.Fast, want, want.Fast)
	}
}

func TestInspectFastAsBuilt(t *testing.T) {
	t.Parallel()

	f := newComposeFixture(t)

	tests := []struct {
		name    string
		set     map[int]any
		wantDLL string
		wantWD  string
		wantErr string
		cmd     bool
	}{
		{"dll with ./ is normalised", map[int]any{7: "./Web.dll", 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "Web.dll", "/app", "", false},
		{"dll that isn't a plain name", map[int]any{7: "bin/Web.dll", 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "", "/app", "", false},
		{"dll without .dll", map[int]any{7: "Web", 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "", "/app", "", false},
		{"no entrypoint", map[int]any{7: nil, 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "", "/app", "", false},
		{"cmd", map[int]any{8: true, 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "Web.dll", "/app", "", true},
		{"root working dir", map[int]any{9: "/", 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "Web.dll", "", "can't hold the build", false},
		{"no working dir", map[int]any{9: "", 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "Web.dll", "", "it has none", false},
		{"system working dir", map[int]any{9: "/usr/app", 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, "Web.dll", "", "system directory", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, err := inspectFast(t, f.out(tt.set))
			if err != nil {
				t.Fatal(err)
			}

			if info.DLL != tt.wantDLL || info.WorkDir != tt.wantWD || info.HasCmd != tt.cmd || info.Fast != nil {
				t.Errorf("info = %+v", info)
			}

			if (tt.wantErr == "") != (info.WorkDirErr == nil) || (tt.wantErr != "" && !strings.Contains(info.WorkDirErr.Error(), tt.wantErr)) {
				t.Errorf("WorkDirErr = %v, want %q", info.WorkDirErr, tt.wantErr)
			}
		})
	}
}

func TestInspectFastRejectsBadValues(t *testing.T) {
	t.Parallel()

	f := newComposeFixture(t)
	other := t.TempDir()

	if err := os.WriteFile(filepath.Join(other, "Evil.csproj"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(f.dir, "linkdir")
	if err := os.Symlink(other, link); err != nil {
		t.Skipf("symlinks: %v", err)
	}

	tests := []struct {
		name string
		set  map[int]any
		want string
	}{
		{"control char in service", map[int]any{11: "we\nb"}, "service"},
		{"bad service", map[int]any{11: "-x"}, "service"},
		{"bad project", map[int]any{10: "My App"}, "project"},
		{"relative compose dir", map[int]any{12: "rel/dir"}, "working_dir"},
		{"control char in compose dir", map[int]any{12: "/a\tb"}, "working_dir"},
		{"relative config file", map[int]any{13: "/ok/compose.yml,relative.yml"}, "config_files"},
		{"comma inside a config path", map[int]any{13: "/a/b,c.yml"}, "config_files"},
		{"control char in config file", map[int]any{13: "/a/b.yml\n"}, "config_files"},
		{"too many config files", map[int]any{13: strings.Repeat("/a.yml,", 16) + "/a.yml"}, "config_files"},
		{"control char in version", map[int]any{14: "1\n"}, "version"},
		{"non-numeric version", map[int]any{14: "one"}, "version"},
		{"long version", map[int]any{14: "1234"}, "version"},
		{"relative override", map[int]any{15: "override.yml"}, "override"},
		{"control char in override", map[int]any{15: f.dir + string(filepath.Separator) + "o\x07.yml"}, "override"},
		{"missing override", map[int]any{15: ""}, "override"},
		{"dll with a slash", map[int]any{16: "a/Web.dll"}, "dll"},
		{"dll with ./", map[int]any{16: "./Web.dll"}, "dll"},
		{"empty dll", map[int]any{16: ""}, "dll"},
		{"control char in dll", map[int]any{16: "We\x00b.dll"}, "dll"},
		{"system workdir", map[int]any{17: "/usr/app"}, "workdir"},
		{"relative workdir", map[int]any{17: "app"}, "workdir"},
		{"empty workdir", map[int]any{17: ""}, "workdir"},
		{"dotdot in project", map[int]any{18: "../Web.csproj"}, "project"},
		{"absolute project", map[int]any{18: filepath.Join(f.dir, "Web", "Web.csproj")}, "project"},
		{"backslash project", map[int]any{18: `Web\Web.csproj`}, "project"},
		{"not a project file", map[int]any{18: "Web/Program.cs"}, "project"},
		{"unclean project", map[int]any{18: "Web//Web.csproj"}, "project"},
		{"project through a symlink out of the compose dir", map[int]any{18: "linkdir/Evil.csproj"}, "project"},
		{"control char in project", map[int]any{18: "Web/\x01.csproj"}, "project"},
		{"invalid utf-8 in dll", map[int]any{16: "\xff.dll"}, "dll"},
		{"label too long", map[int]any{17: "/" + strings.Repeat("a", 5000)}, "workdir"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := inspectFast(t, f.out(tt.set))
			if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v (%s), want INVALID_REQUEST naming %q", err, api.CodeOf(err), tt.want)
			}
		})
	}
}

func TestInspectFastIncompleteLabels(t *testing.T) {
	t.Parallel()

	f := newComposeFixture(t)

	// Eyedbg's labels without the override label are incomplete.
	if _, err := inspectFast(t, f.out(map[int]any{15: ""})); api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("err = %v", err)
	}
}

func TestInspectFastLenientCases(t *testing.T) {
	t.Parallel()

	f := newComposeFixture(t)
	full := func() *container.FastLabels {
		return &container.FastLabels{Version: "1", Override: f.override, DLL: "Web.dll", WorkDir: "/app"}
	}

	withProject := full()
	withProject.Project, withProject.ProjectPath = f.project, filepath.Join(f.dir, "Web", "Web.csproj")

	tests := []struct {
		name string
		set  map[int]any
		want *container.FastLabels
	}{
		// A later version is not read further: even garbage must not fail it.
		{"later version", map[int]any{14: "2", 16: "not a dll", 17: "/usr/x", 18: "../x"}, &container.FastLabels{Version: "2", Override: f.override}},
		// A project file that is gone, or a compose dir to check it in that is
		// gone, leaves the project empty (the caller searches again).
		{"project file gone", map[int]any{18: "Gone/Gone.csproj"}, full()},
		{"compose dir gone", map[int]any{12: filepath.Join(f.dir, "nowhere")}, full()},
		{"no project label", map[int]any{18: nil}, full()},
		{"project resolved", nil, withProject},
		{"no labels at all", map[int]any{14: nil, 15: nil, 16: nil, 17: nil, 18: nil}, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, err := inspectFast(t, f.out(tt.set))
			if err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(info.Fast, tt.want) {
				t.Errorf("fast = %+v, want %+v", info.Fast, tt.want)
			}
		})
	}

	t.Run("no compose labels", func(t *testing.T) {
		t.Parallel()

		info, err := inspectFast(t, f.out(map[int]any{10: nil, 11: nil, 12: nil, 13: nil, 14: nil, 15: nil, 16: nil, 17: nil, 18: nil}))
		if err != nil || info.Fast != nil || info.Project != "" || info.Service != "" || info.ConfigFiles != nil || info.ComposeDir != "" {
			t.Errorf("info = %+v, %v", info, err)
		}
	})
}

func TestInspectFastRejectsMalformedAnswers(t *testing.T) {
	t.Parallel()

	f := newComposeFixture(t)

	tests := []struct {
		name string
		out  string
		code api.Code
	}{
		{"not json", "hello", api.CodeAttachFailed},
		{"too few values", `["a"]`, api.CodeAttachFailed},
		{"too many values", strings.TrimSuffix(f.out(nil), "]") + `,null]`, api.CodeAttachFailed},
		{"wrong type", f.out(map[int]any{4: "yes"}), api.CodeAttachFailed},
		{"short id", f.out(map[int]any{0: "0123456789ab"}), api.CodeAttachFailed},
		{"bad name", f.out(map[int]any{1: "/-web"}), api.CodeAttachFailed},
		{"bad image", f.out(map[int]any{2: "nope"}), api.CodeAttachFailed},
		{"bad platform", f.out(map[int]any{3: "Linux"}), api.CodeAttachFailed},
		{"control char in the entrypoint dll", f.out(map[int]any{7: "a\nb.dll"}), api.CodeInvalidRequest},
		{"control char in the working dir", f.out(map[int]any{9: "/ap\x01p"}), api.CodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := inspectFast(t, tt.out); api.CodeOf(err) != tt.code {
				t.Errorf("err = %v (%s), want %s", err, api.CodeOf(err), tt.code)
			}
		})
	}
}

func TestInspectFastDockerErrors(t *testing.T) {
	t.Parallel()

	calls := filepath.Join(t.TempDir(), "calls")

	rules := map[string]containertest.Rule{
		"no such": {Match: []string{"inspect"}, Exit: 1, Stderr: "Error: No such container: web"},
		"down":    {Match: []string{"inspect"}, Exit: 1, Stderr: "Cannot connect to the Docker daemon"},
	}

	for name, rule := range rules {
		e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{rule}})

		_, err := e.InspectFast(t.Context(), "web")
		if name == "no such" && (api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "no container")) {
			t.Errorf("%s: err = %v", name, err)
		}

		if name == "down" && (api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "Cannot connect")) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// A reference that isn't a container reference never reaches docker.
	bad := filepath.Join(t.TempDir(), "calls")
	e := containertest.Engine(t, containertest.Scenario{Calls: bad})

	if _, err := e.InspectFast(t.Context(), "--privileged"); api.CodeOf(err) != api.CodeInvalidRequest || len(containertest.ReadCalls(t, bad)) != 0 {
		t.Errorf("err = %v", err)
	}
}

func TestValidAppDir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		dir string
		ok  bool
	}{
		{"/app", true},
		{"/app/web", true},
		{"/a/b/c/d", true},
		{"/home/app", true},
		{"/srv/My_App-1.0", true},
		{"/opt/x", true},
		{"/src", true},
		{"", false},
		{"/", false},
		{"app", false},
		{"./app", false},
		{"/a/b/c/d/e", false},
		{"/app/", false},
		{"//app", false},
		{"/app//web", false},
		{"/app/./web", false},
		{"/app/../etc", false},
		{"/..", false},
		{"/.", false},
		{"/app/..", false},
		{"/app/web/..", false},
		{"/ap p", false},
		{"/app\n", false},
		{"/app\x00", false},
		{"/ap$p", false},
		{"/app\\web", false},
		{"/app:x", false},
		{"/\xff", false},
		{"/" + strings.Repeat("a", 255), false},
		{"/bin", false},
		{"/boot/x", false},
		{"/dev", false},
		{"/etc/app", false},
		{"/lib", false},
		{"/lib32", false},
		{"/lib64", false},
		{"/libx32", false},
		{"/proc", false},
		{"/run/x", false},
		{"/sbin", false},
		{"/sys", false},
		{"/tmp", false},
		{"/usr/share/app", false},
		{"/var/app", false},
		{"/.eyedbg-netcoredbg-3.2.0-1092", false},
		{"/.eyedbg-x/app", false},
		{"/home/usr", true},
		{"/app/usr", true},
	}

	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			t.Parallel()

			err := container.ValidAppDir(tt.dir)
			if (err == nil) != tt.ok {
				t.Errorf("ValidAppDir(%q) = %v, want ok=%v", tt.dir, err, tt.ok)
			}

			if err != nil && api.CodeOf(err) != api.CodeInvalidRequest {
				t.Errorf("code = %s", api.CodeOf(err))
			}
		})
	}
}

func TestValidDLL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ok   bool
	}{
		{"Web.dll", true},
		{"My.App-1_2.dll", true},
		{"a.dll", true},
		{".dll", false},
		{"Web", false},
		{"Web.DLL", false},
		{"Web.exe", false},
		{"./Web.dll", false},
		{"a/Web.dll", false},
		{`a\Web.dll`, false},
		{"../Web.dll", false},
		{"We b.dll", false},
		{"We\nb.dll", false},
		{"Web.dll\n", false},
		{"Web.dll ", false},
		{"", false},
		{strings.Repeat("a", 252) + ".dll", false},
		{strings.Repeat("a", 251) + ".dll", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := container.ValidDLL(tt.name); got != tt.ok {
				t.Errorf("ValidDLL(%q) = %v, want %v", tt.name, got, tt.ok)
			}
		})
	}
}

func TestValidProjectRel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		rel string
		ok  bool
	}{
		{"Web/Web.csproj", true},
		{"Web.csproj", true},
		{"src/My App/Web.fsproj", true},
		{"a/b/c/Web.vbproj", true},
		{"", false},
		{"/Web.csproj", false},
		{"../Web.csproj", false},
		{"a/../Web.csproj", false},
		{"a//Web.csproj", false},
		{"./Web.csproj", false},
		{"a/./Web.csproj", false},
		{"Web.cs", false},
		{".csproj", false},
		{"a/.csproj", false},
		{`a\Web.csproj`, false},
		{"a/Web.csproj/", false},
		{"a/\nWeb.csproj", false},
		{"a/\xffWeb.csproj", false},
		{strings.Repeat("a/", 600) + "Web.csproj", false},
	}

	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			t.Parallel()

			if got := container.ValidProjectRel(tt.rel); got != tt.ok {
				t.Errorf("ValidProjectRel(%q) = %v, want %v", tt.rel, got, tt.ok)
			}
		})
	}
}

// resolveFixture is a compose directory with a project and the files around
// it that ResolveProject must refuse or follow.
type resolveFixture struct {
	composeFixture

	outside, sibling string
	symlinks         bool
	abs              string
}

func newResolveFixture(t *testing.T) resolveFixture {
	t.Helper()

	f := resolveFixture{composeFixture: newComposeFixture(t), outside: t.TempDir()}
	write := func(p string) {
		t.Helper()

		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(filepath.Join(f.outside, "Out.csproj"))

	// A sibling whose name starts with the root's: a prefix test without a
	// separator would accept it.
	f.sibling = f.dir + "-sibling"
	if err := os.MkdirAll(f.sibling, 0o750); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(f.sibling) })

	write(filepath.Join(f.sibling, "Sib.csproj"))
	write(filepath.Join(f.dir, "Web", "Program.cs"))

	f.abs = filepath.Join(f.dir, "Web", "Web.csproj")
	f.symlinks = os.Symlink(f.outside, filepath.Join(f.dir, "out")) == nil
	_ = os.Symlink(filepath.Join(f.outside, "Out.csproj"), filepath.Join(f.dir, "Web", "Link.csproj"))
	_ = os.Symlink(f.abs, filepath.Join(f.dir, "Web", "Inside.csproj"))

	return f
}

func TestResolveProject(t *testing.T) {
	t.Parallel()

	f := newResolveFixture(t)

	tests := []struct {
		name    string
		p       string
		wantErr error
		needSym bool
	}{
		{"relative", "Web/Web.csproj", nil, false},
		{"absolute", f.abs, nil, false},
		{"inside link resolved", "Web/Inside.csproj", nil, true},
		{"dotdot out", "../" + filepath.Base(f.dir) + "-sibling/Sib.csproj", container.ErrOutsideRoot, false},
		{"sibling with the same prefix", filepath.Join(f.sibling, "Sib.csproj"), container.ErrOutsideRoot, false},
		{"absolute elsewhere", filepath.Join(f.outside, "Out.csproj"), container.ErrOutsideRoot, false},
		{"directory symlink out", "out/Out.csproj", container.ErrOutsideRoot, true},
		{"file symlink out", "Web/Link.csproj", container.ErrOutsideRoot, true},
		{"not a project file", "Web/Program.cs", container.ErrOutsideRoot, false},
		{"a directory", "Web", container.ErrOutsideRoot, false},
		{"the root", ".", container.ErrOutsideRoot, false},
		{"missing", "Web/Nope.csproj", fs.ErrNotExist, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.needSym && !f.symlinks {
				t.Skip("no symlinks")
			}

			got, rel, err := container.ResolveProject(f.dir, tt.p)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("ResolveProject(%q) = %q, %v, want %v", tt.p, got, err, tt.wantErr)
				}

				return
			}

			if err != nil || rel != "Web/Web.csproj" || got != f.abs {
				t.Errorf("ResolveProject(%q) = %q, %q, %v", tt.p, got, rel, err)
			}
		})
	}

	if _, _, err := container.ResolveProject(filepath.Join(f.dir, "nowhere"), "x.csproj"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing root: %v", err)
	}
}

func fastServicesEngine(t *testing.T, r containertest.Rule) (eng container.Engine, calls string) {
	t.Helper()

	calls = filepath.Join(t.TempDir(), "calls")
	r.Match = []string{"ps"}

	return containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{r}}), calls
}

func TestFastServicesAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
		want   []string
	}{
		{"two", "web\nworker\n", []string{"web", "worker"}},
		{"duplicates collapse", "web\nworker\nweb\n", []string{"web", "worker"}},
		{"none", "", nil},
		{"crlf", "web\r\nworker\r\n", []string{"web", "worker"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, calls := fastServicesEngine(t, containertest.Rule{Stdout: tt.stdout})
			e.Context = "remote"

			got, err := e.FastServices(t.Context(), "my-app")
			if err != nil {
				t.Fatal(err)
			}

			var names []string
			for s := range got {
				names = append(names, s)
			}

			slices.Sort(names)

			if !slices.Equal(names, tt.want) {
				t.Errorf("got %v, want %v", names, tt.want)
			}

			want := []string{
				"--context=remote", "ps", "--all", "--filter=label=com.docker.compose.project=my-app",
				"--filter=label=dev.izzat.eyedbg.fast.override", `--format={{.Label "com.docker.compose.service"}}`,
			}

			if argv := containertest.ReadCalls(t, calls)[0]; !slices.Equal(argv, want) {
				t.Errorf("argv = %q\nwant   %q", argv, want)
			}
		})
	}
}

// TestFastServicesBadAnswers: a partial answer must never come back, since
// the caller deletes the directories of services not in it.
func TestFastServicesBadAnswers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		stdout string
		exit   int
	}{
		{name: "blank line", stdout: "web\n\nworker\n"},
		{name: "not a service name", stdout: "web\n--rm\n"},
		{name: "a name with a space", stdout: "web app\n"},
		{name: "control character", stdout: "we\x01b\n"},
		{name: "docker fails", exit: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, _ := fastServicesEngine(t, containertest.Rule{Stdout: tt.stdout, Exit: tt.exit, Stderr: "boom"})

			got, err := e.FastServices(t.Context(), "my-app")
			if err == nil || got != nil {
				t.Errorf("got %v, %v; want an error and no map", got, err)
			}
		})
	}
}

func TestFastServicesChecksProjectNames(t *testing.T) {
	t.Parallel()

	e, calls := fastServicesEngine(t, containertest.Rule{})

	for _, p := range []string{"", "My App", "a,b", "--x", "UP", "a=b"} {
		if _, err := e.FastServices(t.Context(), p); api.CodeOf(err) != api.CodeInvalidRequest {
			t.Errorf("project %q: err = %v", p, err)
		}
	}

	if n := len(containertest.ReadCalls(t, calls)); n != 0 {
		t.Errorf("docker ran %d times for invalid projects", n)
	}
}
