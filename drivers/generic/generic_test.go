// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// bundled returns the bundled manifests.
func bundled() *adapters.Registry {
	return adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: []string{"dotnet"}})
}

// pythonDriver is the driver of the bundled python manifest, with an
// interpreter resolver that records its input and returns rt.
func pythonDriver(t *testing.T, got *adapters.PythonInput) *Driver {
	t.Helper()

	m := bundled().Language("python")
	if m == nil {
		t.Fatal("no bundled python manifest")
	}

	d := New(m)
	d.resolvePython = func(_ context.Context, _ *adapters.Manifest, in adapters.PythonInput) (adapters.Runtime, error) {
		*got = in

		return adapters.Runtime{Exe: "/usr/bin/python3.13", Version: "3.13.5", Root: filepath.FromSlash("/data/debugpy/1.8.22")}, nil
	}

	return d
}

// appFile creates an empty program file and returns its path.
func appFile(t *testing.T) string {
	t.Helper()

	p := filepath.Join(t.TempDir(), "app.py")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

func TestPrepareDebugpyProgram(t *testing.T) {
	t.Parallel()

	var in adapters.PythonInput

	d := pythonDriver(t, &in)
	app := appFile(t)
	client := t.TempDir()

	launch, err := d.Prepare(t.Context(), session.LaunchSpec{Program: app, ClientDir: client, Project: "/ignored", NoBuild: true})
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]any{
		"name": "eyedbg", "type": "debugpy", "request": "launch", "program": app, "cwd": client,
		"python": "/usr/bin/python3.13", "stopOnEntry": false, "justMyCode": true, "console": "internalConsole",
		"redirectOutput": true, "subProcess": false, "showReturnValue": true,
		"variablePresentation": map[string]any{"special": "hide", "function": "hide", "class": "group", "protected": "inline"},
	}

	if got, w := jsonOf(t, launch.Arguments), jsonOf(t, want); got != w {
		t.Fatalf("arguments = %s\nwant        %s", got, w)
	}

	entry := filepath.Join(filepath.FromSlash("/data/debugpy/1.8.22"), "debugpy", "adapter")
	if launch.Adapter != "/usr/bin/python3.13" || !slices.Equal(launch.AdapterArgs, []string{entry}) || launch.AdapterID != "debugpy" ||
		launch.Program != app || launch.Request != session.RequestLaunch || launch.SideEffects == nil {
		t.Fatalf("launch = %+v", launch)
	}

	if in != (adapters.PythonInput{Cwd: client, ProgramDir: filepath.Dir(app)}) {
		t.Fatalf("python input = %+v", in)
	}

	wantFilters := map[api.ExceptionMode][]string{api.ExceptionsAll: {"raised", "uncaught"}, api.ExceptionsUncaught: {"uncaught", "userUnhandled"}}
	if jsonOf(t, launch.ExceptionFilters) != jsonOf(t, wantFilters) {
		t.Fatalf("exception filters = %v", launch.ExceptionFilters)
	}
}

func TestPrepareDebugpyModule(t *testing.T) {
	t.Parallel()

	var in adapters.PythonInput

	d := pythonDriver(t, &in)
	cwd := t.TempDir()

	launch, err := d.Prepare(t.Context(), session.LaunchSpec{
		Cwd: cwd, Args: []string{"-x", "tests/test_a.py"}, Env: map[string]string{"A": "1"}, StopOnEntry: true,
		Options: map[string]string{"module": "pytest", "justMyCode": "false", "python": "/opt/py"},
	})
	if err != nil {
		t.Fatal(err)
	}

	args := launch.Arguments
	if args["module"] != "pytest" || jsonOf(t, args["args"]) != `["-x","tests/test_a.py"]` || jsonOf(t, args["env"]) != `{"A":"1"}` ||
		args["stopOnEntry"] != true || args["justMyCode"] != false || args["cwd"] != cwd {
		t.Fatalf("arguments = %s", jsonOf(t, args))
	}

	if _, ok := args["program"]; ok {
		t.Fatalf("program is set without --program: %s", jsonOf(t, args))
	}

	if launch.Program != "module=pytest" || in.Interpreter != "/opt/py" || in.Cwd != cwd || in.ProgramDir != "" {
		t.Fatalf("program %q, python input %+v", launch.Program, in)
	}
}

func TestPrepareErrors(t *testing.T) {
	t.Parallel()

	app := appFile(t)

	tests := []struct {
		name string
		spec session.LaunchSpec
		code api.Code
		want string
	}{
		{"unknown option", session.LaunchSpec{Program: app, Options: map[string]string{"nope": "1"}}, api.CodeInvalidRequest, `python has no option "nope"`},
		{"bad bool", session.LaunchSpec{Program: app, Options: map[string]string{"justMyCode": "maybe"}}, api.CodeInvalidRequest, "--opt justMyCode=maybe"},
		{"nothing to run", session.LaunchSpec{}, api.CodeInvalidRequest, "python needs --program FILE or --opt module=MODULE"},
		{"empty module", session.LaunchSpec{Options: map[string]string{"module": ""}}, api.CodeInvalidRequest, "python needs"},
		{"missing program", session.LaunchSpec{Program: filepath.Join(t.TempDir(), "gone.py")}, api.CodeInvalidRequest, "not found"},
		{"missing cwd", session.LaunchSpec{Program: app, Cwd: filepath.Join(t.TempDir(), "gone")}, api.CodeInvalidRequest, "is not a directory"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var in adapters.PythonInput

			d := pythonDriver(t, &in)
			d.resolvePython = func(context.Context, *adapters.Manifest, adapters.PythonInput) (adapters.Runtime, error) {
				t.Fatal("the interpreter was looked for")

				return adapters.Runtime{}, nil
			}

			_, err := d.Prepare(t.Context(), tt.spec)
			if api.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Prepare = %v, want %s containing %q", err, tt.code, tt.want)
			}
		})
	}
}

func TestUnknownOptionHint(t *testing.T) {
	t.Parallel()

	var in adapters.PythonInput

	_, err := pythonDriver(t, &in).Prepare(t.Context(), session.LaunchSpec{Options: map[string]string{"x": "1"}})

	e, ok := err.(*api.Error) //nolint:errorlint // Prepare returns the *api.Error itself.
	if !ok || !strings.Contains(e.Hint, "justMyCode (bool): ") || !strings.Contains(e.Hint, "module (string): ") {
		t.Fatalf("err = %#v; want a hint listing the options", err)
	}
}

func TestCwdDefaults(t *testing.T) {
	t.Parallel()

	app := appFile(t)
	client, explicit := t.TempDir(), t.TempDir()

	tests := []struct {
		name string
		spec session.LaunchSpec
		want string
	}{
		{"--cwd", session.LaunchSpec{Program: app, Cwd: explicit, ClientDir: client}, explicit},
		{"the client's directory", session.LaunchSpec{Program: app, ClientDir: client}, client},
		{"the program's directory", session.LaunchSpec{Program: app}, filepath.Dir(app)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var in adapters.PythonInput

			launch, err := pythonDriver(t, &in).Prepare(t.Context(), tt.spec)
			if err != nil || launch.Arguments["cwd"] != tt.want || in.Cwd != tt.want {
				t.Fatalf("cwd = %v (probe in %q), %v; want %s", launch.Arguments["cwd"], in.Cwd, err, tt.want)
			}
		})
	}
}

func TestPrepareAttach(t *testing.T) {
	t.Parallel()

	var in adapters.PythonInput

	d := pythonDriver(t, &in)

	_, err := d.PrepareAttach(t.Context(), api.AttachSpec{PID: 42})
	if api.CodeOf(err) != api.CodeUnsupported || !strings.Contains(err.Error(), "the python debug adapter can't attach") {
		t.Fatalf("PrepareAttach = %v, want UNSUPPORTED_BY_ADAPTER", err)
	}

	if e, ok := err.(*api.Error); !ok || !strings.Contains(e.Hint, "gdb or lldb") { //nolint:errorlint // The *api.Error itself.
		t.Fatalf("hint = %#v; want the manifest's attachUnsupported", err)
	}

	withAttach := *d.m
	withAttach.Attach = &adapters.Template{Arguments: map[string]any{"request": "attach", "processId": "${pid}", "python": "${runtime}"}}
	d.m = &withAttach

	launch, err := d.PrepareAttach(t.Context(), api.AttachSpec{PID: 42})
	if err != nil || launch.Request != session.RequestAttach || launch.PID != 42 || launch.Arguments["processId"] != 42 ||
		launch.Arguments["python"] != "/usr/bin/python3.13" || launch.SideEffects == nil || launch.ExceptionFilters == nil ||
		launch.AttachHint != "" {
		t.Fatalf("PrepareAttach = %+v, %v", launch, err)
	}
}

// TestPrepareAttachNative checks a native (runtime "") manifest-driven
// adapter's ATTACH_FAILED gets the generic ptrace_scope/task_for_pid hint;
// TestPrepareAttach already covers a python-runtime one, which doesn't.
func TestPrepareAttachNative(t *testing.T) {
	t.Parallel()

	m := &adapters.Manifest{
		Name: "toydbg", Version: "1", Adapter: adapters.Adapter{ID: "toy", Entry: "toydbg", Env: "TOYDBG", Path: true},
		Language: &adapters.Language{Name: "toy"},
		Attach:   &adapters.Template{Arguments: map[string]any{"processId": "${pid}"}},
	}
	d := New(m)
	d.find = func(*adapters.Manifest) (adapters.Location, error) {
		return adapters.Location{Path: "/usr/bin/toydbg", Source: adapters.FoundPath}, nil
	}

	launch, err := d.PrepareAttach(t.Context(), api.AttachSpec{PID: 42})
	if err != nil || launch.PID != 42 || !strings.Contains(launch.AttachHint, "ptrace_scope") ||
		!strings.Contains(launch.AttachHint, "task_for_pid") {
		t.Fatalf("PrepareAttach = %+v, %v; want a ptrace_scope/task_for_pid hint", launch, err)
	}
}

func TestNativeAdapterNotInstalled(t *testing.T) {
	t.Parallel()

	m := &adapters.Manifest{
		Name: "toydbg", Version: "1", Adapter: adapters.Adapter{ID: "toy", Entry: "toydbg", Env: "TOYDBG", Path: true},
		Language: &adapters.Language{Name: "toy"},
		Launch:   &adapters.Template{Require: []string{"program"}, Arguments: map[string]any{"program": "${program}"}},
	}
	d := New(m)
	d.find = func(*adapters.Manifest) (adapters.Location, error) {
		return adapters.Location{}, adapters.ErrNotInstalled
	}

	_, err := d.Prepare(t.Context(), session.LaunchSpec{Program: appFile(t)})
	if api.CodeOf(err) != api.CodeAdapterMissing || !strings.Contains(err.Error(), "toydbg is not installed") {
		t.Fatalf("Prepare = %v, want ADAPTER_NOT_INSTALLED", err)
	}

	if e, ok := err.(*api.Error); !ok || e.Hint != "set TOYDBG to it, or put toydbg on PATH" { //nolint:errorlint // The *api.Error itself.
		t.Fatalf("hint = %#v", err)
	}
}

// TestPrepareEnvList: a manifest whose launch "env" argument is
// "${envList}" (lldb-dap <=19's shape) gets a sorted "NAME=VALUE" list, and
// omits the key when env is empty.
func TestPrepareEnvList(t *testing.T) {
	t.Parallel()

	m := &adapters.Manifest{
		Name: "toydbg", Version: "1", Adapter: adapters.Adapter{ID: "toy", Entry: "toydbg", Env: "TOYDBG", Path: true},
		Language: &adapters.Language{Name: "toy"},
		Launch: &adapters.Template{
			Require:   []string{"program"},
			Arguments: map[string]any{"program": "${program}", "env": "${envList}"},
		},
	}
	d := New(m)
	d.find = func(*adapters.Manifest) (adapters.Location, error) {
		return adapters.Location{Path: "/usr/bin/toydbg", Source: adapters.FoundPath}, nil
	}

	app := appFile(t)

	launch, err := d.Prepare(t.Context(), session.LaunchSpec{Program: app, Env: map[string]string{"B": "2", "A": "1"}})
	if err != nil || jsonOf(t, launch.Arguments["env"]) != `["A=1","B=2"]` {
		t.Fatalf("Prepare env = %v, %v; want [A=1 B=2]", launch.Arguments["env"], err)
	}

	launch, err = d.Prepare(t.Context(), session.LaunchSpec{Program: app})
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := launch.Arguments["env"]; ok {
		t.Fatalf("empty env should omit the key: %s", jsonOf(t, launch.Arguments))
	}
}

func TestDrivers(t *testing.T) {
	t.Parallel()

	var names []string
	for _, d := range Drivers(bundled()) {
		names = append(names, d.Name())
	}

	if !slices.Equal(names, []string{"python"}) {
		t.Fatalf("Drivers = %v, want [python] (dotnet is built in)", names)
	}

	if _, ok := Drivers(bundled())[0].(session.Attacher); !ok {
		t.Fatal("the generic driver is not a session.Attacher")
	}
}

// TestRelativeInterpreter: --opt python=.venv/bin/python is relative to
// the directory eyedbg ran in, not the daemon's; a bare name stays a
// PATH lookup and an absolute path stays as is.
func TestRelativeInterpreter(t *testing.T) {
	t.Parallel()

	app := appFile(t)
	client := t.TempDir()

	tests := []struct{ opt, want string }{
		{filepath.Join(".venv", "bin", "python"), filepath.Join(client, ".venv", "bin", "python")},
		{"python3.12", "python3.12"},
		{filepath.Join(client, "py"), filepath.Join(client, "py")},
	}

	for _, tt := range tests {
		var in adapters.PythonInput

		_, err := pythonDriver(t, &in).Prepare(t.Context(), session.LaunchSpec{
			Program: app, ClientDir: client, Options: map[string]string{"python": tt.opt},
		})
		if err != nil || in.Interpreter != tt.want {
			t.Errorf("--opt python=%s: interpreter %q, %v; want %q", tt.opt, in.Interpreter, err, tt.want)
		}
	}
}

// TestVirtualEnvPassedThrough: the start request's VirtualEnv (the
// caller's $VIRTUAL_ENV, read by the CLI) reaches PythonInput unchanged.
func TestVirtualEnvPassedThrough(t *testing.T) {
	t.Parallel()

	app := appFile(t)
	venv := filepath.Join(t.TempDir(), "venv")

	var in adapters.PythonInput

	_, err := pythonDriver(t, &in).Prepare(t.Context(), session.LaunchSpec{Program: app, VirtualEnv: venv})
	if err != nil || in.VirtualEnv != venv {
		t.Fatalf("VirtualEnv = %q, %v; want %q", in.VirtualEnv, err, venv)
	}
}
