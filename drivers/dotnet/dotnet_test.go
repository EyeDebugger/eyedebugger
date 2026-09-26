// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

func TestWithoutResultJSON(t *testing.T) {
	t.Parallel()

	out := "{\n  \"Properties\": {\n    \"TargetPath\": \"/x.dll\"\n  }\n}\n/src/Program.cs(1,9): error CS0029: bad\n"
	if got, want := withoutResultJSON(out), "/src/Program.cs(1,9): error CS0029: bad\n"; got != want {
		t.Errorf("withoutResultJSON = %q, want %q", got, want)
	}

	if got := withoutResultJSON("error: no json\n"); got != "error: no json\n" {
		t.Errorf("non-JSON output changed: %q", got)
	}
}

func TestTail(t *testing.T) {
	t.Parallel()

	if got := tail("a\n\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("tail = %q, want %q", got, "b\nc")
	}
}

// TestStreamsBuilds checks that the driver is one session.Launch streams
// the build of: an OptionsPreparer.
func TestStreamsBuilds(t *testing.T) {
	t.Parallel()

	if _, ok := session.Driver(NewWith(nil)).(session.OptionsPreparer); !ok {
		t.Error("the .NET driver is not a session.OptionsPreparer")
	}
}

// TestStreamedBuildArgs checks the streamed build's two invocations: a
// plain Debug build, then the query for what it built.
func TestStreamedBuildArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		got  []string
		want []string
	}{
		{"build", streamedBuildArgs("/src/app.csproj"), []string{"build", "/src/app.csproj", "-c", "Debug", "-nologo", "-tl:off"}},
		{"target path", targetPathArgs("/src/app.csproj"), []string{
			"msbuild", "/src/app.csproj", "-nologo", "-getProperty:TargetPath", "-p:Configuration=Debug",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if !slices.Equal(tt.got, tt.want) {
				t.Errorf("args = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// TestParseTargetPath checks reading the one property msbuild was asked
// for: its bare value, or JSON.
func TestParseTargetPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want string
		ok   bool
	}{
		{"bare", "/src/bin/Debug/net10.0/app.dll\n", "/src/bin/Debug/net10.0/app.dll", true},
		{"bare, CRLF", "C:\\src\\bin\\app.dll\r\n", "C:\\src\\bin\\app.dll", true},
		{"JSON", "{\n  \"Properties\": {\n    \"TargetPath\": \"/x.dll\"\n  }\n}\n", "/x.dll", true},
		{"JSON without it", "{\n  \"Properties\": {}\n}\n", "", false},
		{"empty", "\n", "", false},
		{"more than a path", "warning: x\n/x.dll\n", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := parseTargetPath([]byte(tt.out))
			if ok != tt.ok || (ok && got != tt.want) {
				t.Errorf("parseTargetPath(%q) = %q, %v; want %q, %v", tt.out, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestStreamedBuildErr checks the error of a streamed build that failed:
// one that ran and failed points at its streamed output, one that didn't
// run says why, and a canceled start is the context's error.
func TestStreamedBuildErr(t *testing.T) {
	t.Parallel()

	// This test binary, given a flag it doesn't know, exits with code 2.
	exitErr := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$", "-no-such-flag").Run() //nolint:gosec // This test binary, fixed arguments.
	if _, ok := errors.AsType[*exec.ExitError](exitErr); !ok {
		t.Fatalf("setup: %v, want an exit error", exitErr)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context //nolint:containedctx // A test case's input.
		err  error
		code api.Code
		msg  string
		hint string
	}{
		{
			name: "build failed", ctx: t.Context(), err: exitErr, code: api.CodeBuildFailed,
			msg: "dotnet build /src/app.csproj failed (its output is above)", hint: "fix the build errors above, then start again",
		},
		{
			name: "didn't run", ctx: t.Context(), err: exec.ErrNotFound, code: api.CodeBuildFailed,
			msg: "dotnet build /src/app.csproj failed: " + exec.ErrNotFound.Error(), hint: "check the .NET SDK ('dotnet --info')",
		},
		{name: "canceled", ctx: canceled, err: exitErr, msg: "dotnet build /src/app.csproj: " + context.Canceled.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := streamedBuildErr(tt.ctx, "/src/app.csproj", tt.err)

			if tt.code == "" {
				if !errors.Is(err, context.Canceled) || err.Error() != tt.msg {
					t.Fatalf("err = %v, want %q wrapping context.Canceled", err, tt.msg)
				}

				return
			}

			e, ok := errors.AsType[*api.Error](err)
			if !ok || e.Code != tt.code || e.Message != tt.msg || e.Hint != tt.hint {
				t.Errorf("err = %#v, want %s %q (hint %q)", err, tt.code, tt.msg, tt.hint)
			}
		})
	}
}

func TestPrepareRejectsOptions(t *testing.T) {
	t.Parallel()

	// Options are refused before netcoredbg or the dotnet host is looked
	// for, so this holds on a machine without either.
	_, err := New().Prepare(t.Context(), session.LaunchSpec{Program: "/nonexistent/app.dll", Options: map[string]string{"x": "1"}})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "--opt") {
		t.Fatalf("Prepare with options: err = %v, want INVALID_REQUEST about --opt", err)
	}
}

// bundled returns the bundled registry's netcoredbg and sharpdbg manifests.
func bundled(t *testing.T) (netcoredbg, sharpdbg *adapters.Manifest) {
	t.Helper()

	reg := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: []string{lang}})

	netcoredbg, sharpdbg = reg.Language(lang), reg.Adapter(SharpDbg)
	if netcoredbg == nil || netcoredbg.Name != "netcoredbg" || sharpdbg == nil {
		t.Fatalf("bundled manifests: %v, %v", netcoredbg, sharpdbg)
	}

	return netcoredbg, sharpdbg
}

// fakeAdapters is what a test's driver finds: whether netcoredbg is found
// (or broken) and whether SharpDbg resolves (or is broken).
type fakeAdapters struct {
	netcoredbg string // "found", "missing" or "broken"
	sharpdbg   string // "found", "missing" or "broken"
}

func (f fakeAdapters) env(goos, goarch string) driverEnv {
	return driverEnv{
		goos: goos, goarch: goarch,
		find: func(m *adapters.Manifest) (adapters.Location, error) {
			switch f.netcoredbg {
			case "found":
				return adapters.Location{Path: "/tools/" + m.Name, Source: adapters.FoundInstalled}, nil
			case "broken":
				return adapters.Location{}, errors.New("EYEDBG_NETCOREDBG=/x: no such file")
			default:
				return adapters.Location{}, adapters.ErrNotInstalled
			}
		},
		resolveDotnet: func(_ context.Context, m *adapters.Manifest) (adapters.DotnetRuntime, error) {
			switch f.sharpdbg {
			case "found":
				return adapters.DotnetRuntime{Host: "/sdk/dotnet", Entry: "/tools/" + m.Name + "/SharpDbg.Cli.dll", Runtime: "10.0.10"}, nil
			case "broken":
				return adapters.DotnetRuntime{}, api.NewError(api.CodeAdapterMissing, "sharpdbg 0.1.17 needs the .NET 10.0+ runtime", "install it")
			default:
				return adapters.DotnetRuntime{}, fmt.Errorf("sharpdbg is not installed: %w", adapters.ErrNotInstalled)
			}
		},
	}
}

func TestAdapterChoice(t *testing.T) {
	t.Parallel()

	netcoredbg, sharpdbg := bundled(t)

	tests := []struct {
		name         string
		pick         string
		goos, goarch string
		have         fakeAdapters
		noSharp      bool   // the registry has no sharpdbg manifest
		want         string // the adapter chosen; "" for an error
		wantErr      string // in the error
	}{
		{"default: netcoredbg", "", "linux", "amd64", fakeAdapters{"found", "found"}, false, "netcoredbg", ""},
		{
			"netcoredbg missing where it has a build: no fallback", "", "linux", "amd64",
			fakeAdapters{"missing", "found"},
			false, "",
			"run 'eyedbg adapters install netcoredbg', or set EYEDBG_NETCOREDBG",
		},
		{"intel mac: installed sharpdbg is the default", "", "darwin", "amd64", fakeAdapters{"missing", "found"}, false, "sharpdbg", ""},
		{
			"windows arm64: sharpdbg is withheld", "", "windows", "arm64",
			fakeAdapters{"missing", "found"},
			false, "",
			"netcoredbg has no build for windows/arm64: run 'eyedbg adapters install sharpdbg' and start with --adapter sharpdbg (not verified here: ",
		},
		{"windows arm64: --adapter sharpdbg still chooses it", "sharpdbg", "windows", "arm64", fakeAdapters{"missing", "found"}, false, "sharpdbg", ""},
		{"intel mac: a netcoredbg of your own wins", "", "darwin", "amd64", fakeAdapters{"found", "found"}, false, "netcoredbg", ""},
		{"intel mac: a broken EYEDBG_NETCOREDBG is reported", "", "darwin", "amd64", fakeAdapters{"broken", "found"}, false, "", "EYEDBG_NETCOREDBG=/x"},
		{
			"intel mac: nothing installed says install sharpdbg", "", "darwin", "amd64",
			fakeAdapters{"missing", "missing"},
			false, "",
			"netcoredbg has no build for darwin/amd64: run 'eyedbg adapters install sharpdbg' (sessions here then use it unless --adapter or EYEDBG_DOTNET_ADAPTER picks netcoredbg), or set EYEDBG_NETCOREDBG to your own netcoredbg build",
		},
		{"intel mac: a broken sharpdbg is reported", "", "darwin", "amd64", fakeAdapters{"missing", "broken"}, false, "", "needs the .NET 10.0+ runtime"},
		{
			"intel mac without a sharpdbg manifest", "", "darwin", "amd64",
			fakeAdapters{"missing", "found"},
			true, "",
			"run 'eyedbg adapters install netcoredbg', or set EYEDBG_NETCOREDBG",
		},
		{"--adapter sharpdbg", "sharpdbg", "linux", "amd64", fakeAdapters{"found", "found"}, false, "sharpdbg", ""},
		{"--adapter sharpdbg, missing", "sharpdbg", "linux", "amd64", fakeAdapters{"found", "missing"}, false, "", "sharpdbg is not installed"},
		{
			"--adapter netcoredbg on an intel mac", "netcoredbg", "darwin", "amd64",
			fakeAdapters{"missing", "found"},
			false, "",
			"netcoredbg has no build for darwin/amd64",
		},
		{"--adapter netcoredbg", "netcoredbg", "linux", "amd64", fakeAdapters{"found", "found"}, false, "netcoredbg", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := &Driver{m: netcoredbg, sharp: sharpdbg, env: tt.have.env(tt.goos, tt.goarch)}
			if tt.noSharp {
				d.sharp = nil
			}

			launch, err := bind(t, d, tt.pick).adapterLaunch(t.Context())
			if tt.want == "" {
				if err == nil || !strings.Contains(err.Error()+" "+hintOf(err), tt.wantErr) {
					t.Fatalf("adapterLaunch = %+v, %v (hint %q); want an error containing %q", launch, err, hintOf(err), tt.wantErr)
				}

				return
			}

			if err != nil || launch.AdapterName != tt.want {
				t.Fatalf("adapterLaunch = %q, %v; want %s", launch.AdapterName, err, tt.want)
			}

			checkLaunch(t, launch)

			if got, want := launch.ExitCodeUnknown, ExitCodeUnknown(launch.AdapterName, tt.goos); got != want {
				t.Errorf("ExitCodeUnknown = %q, want %q", got, want)
			}
		})
	}
}

// TestExitCodeUnknown: netcoredbg on macOS, any arch, and SharpDbg
// everywhere are untrusted.
func TestExitCodeUnknown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		adapter, goos string
		want          bool
	}{
		{"netcoredbg", "darwin", true},
		{"netcoredbg", "linux", false},
		{"netcoredbg", "windows", false},
		{"sharpdbg", "darwin", true},
		{"sharpdbg", "linux", true},
		{"sharpdbg", "windows", true},
	}

	for _, tt := range tests {
		t.Run(tt.adapter+"/"+tt.goos, func(t *testing.T) {
			t.Parallel()

			if got := ExitCodeUnknown(tt.adapter, tt.goos) != ""; got != tt.want {
				t.Errorf("ExitCodeUnknown(%q, %q) = %q, want untrusted=%v", tt.adapter, tt.goos, ExitCodeUnknown(tt.adapter, tt.goos), tt.want)
			}
		})
	}
}

// bind is d bound to adapter pick by WithAdapter, or d for "".
func bind(t *testing.T, d *Driver, pick string) *Driver {
	t.Helper()

	if pick == "" {
		return d
	}

	drv, err := d.WithAdapter(pick)
	if err != nil {
		t.Fatal(err)
	}

	bound, ok := drv.(*Driver)
	if !ok {
		t.Fatalf("WithAdapter returned a %T", drv)
	}

	return bound
}

// hintOf is err's hint, if it is an *api.Error.
func hintOf(err error) string {
	if e, ok := errors.AsType[*api.Error](err); ok {
		return e.Hint
	}

	return ""
}

// checkLaunch checks the process and the settings of a chosen adapter:
// netcoredbg runs itself; SharpDbg runs on the dotnet host and can't pause
// but sets by evaluate; both share the rest.
func checkLaunch(t *testing.T, launch session.Launch) {
	t.Helper()

	want := session.Launch{
		Adapter: "/tools/netcoredbg", AdapterArgs: []string{"--interpreter=vscode"}, AdapterID: "coreclr",
		AdapterName: "netcoredbg", AttachHint: attachHint,
	}
	if launch.AdapterName == SharpDbg {
		want.Adapter, want.AdapterArgs = "/sdk/dotnet", []string{"/tools/sharpdbg/SharpDbg.Cli.dll", "--interpreter=vscode"}
		want.AdapterName, want.PauseUnsupported, want.SetByEval = SharpDbg, sharpdbgPauseHint, true
	}

	if launch.Adapter != want.Adapter || !slices.Equal(launch.AdapterArgs, want.AdapterArgs) || launch.AdapterID != want.AdapterID ||
		launch.AttachHint != want.AttachHint || launch.PauseUnsupported != want.PauseUnsupported || launch.SetByEval != want.SetByEval {
		t.Errorf("launch = %+v, want %+v", launch, want)
	}

	filters := map[api.ExceptionMode][]string{api.ExceptionsAll: {"all"}, api.ExceptionsUncaught: {"user-unhandled"}}
	if !maps.EqualFunc(launch.ExceptionFilters, filters, slices.Equal) || launch.SideEffects == nil {
		t.Errorf("exception filters = %v (side effects set: %v), want %v", launch.ExceptionFilters, launch.SideEffects != nil, filters)
	}
}

func TestWithAdapter(t *testing.T) {
	t.Parallel()

	netcoredbg, sharpdbg := bundled(t)

	// A user manifest serving dotnet under another name replaces
	// netcoredbg's: "netcoredbg" is then unknown.
	mine := *netcoredbg
	mine.Name = "mydbg"

	tests := []struct {
		name     string
		m, sharp *adapters.Manifest
		pick     string
		wantErr  string
	}{
		{"netcoredbg", netcoredbg, sharpdbg, "netcoredbg", ""},
		{"sharpdbg", netcoredbg, sharpdbg, "sharpdbg", ""},
		{"unknown", netcoredbg, sharpdbg, "vsdbg", "dotnet has no adapter vsdbg (its adapters: netcoredbg, sharpdbg)"},
		{"no sharpdbg manifest", netcoredbg, nil, "sharpdbg", "(its adapters: netcoredbg)"},
		{"replaced netcoredbg", &mine, sharpdbg, "netcoredbg", "(its adapters: mydbg, sharpdbg)"},
		{"replaced netcoredbg by name", &mine, sharpdbg, "mydbg", ""},
		{"no manifests", nil, nil, "netcoredbg", "(its adapters: none)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := &Driver{m: tt.m, sharp: tt.sharp, env: fakeAdapters{"found", "found"}.env("linux", "amd64")}

			if tt.wantErr != "" {
				_, err := d.WithAdapter(tt.pick)
				if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("WithAdapter(%q) = %v, want INVALID_REQUEST containing %q", tt.pick, err, tt.wantErr)
				}

				return
			}

			if name, err := bind(t, d, tt.pick).Adapter(t.Context()); err != nil || name != tt.pick {
				t.Fatalf("bound driver's adapter = %q, %v; want %s", name, err, tt.pick)
			}

			if d.pick != "" {
				t.Error("WithAdapter changed the original driver")
			}
		})
	}
}

func TestSharpdbgKnobsOnlyForSharpdbg(t *testing.T) {
	t.Parallel()

	netcoredbg, _ := bundled(t)

	// The manifest serving dotnet, whatever its runtime, gets SharpDbg's
	// settings only when it is named sharpdbg.
	for _, name := range []string{"netcoredbg", SharpDbg} {
		m := *netcoredbg
		m.Name = name

		d := &Driver{m: &m, env: fakeAdapters{"found", "found"}.env("linux", "amd64")}

		launch, err := d.adapterLaunch(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		if got := launch.SetByEval && launch.PauseUnsupported != ""; got != (name == SharpDbg) {
			t.Errorf("%s: SetByEval %v, PauseUnsupported %q", name, launch.SetByEval, launch.PauseUnsupported)
		}
	}
}

func TestNoManifest(t *testing.T) {
	t.Parallel()

	_, err := NewWith(nil).adapterLaunch(t.Context())
	if api.CodeOf(err) != api.CodeAdapterMissing || !strings.Contains(err.Error(), "no adapter manifest serves dotnet") {
		t.Fatalf("adapterLaunch without a manifest = %v", err)
	}
}
