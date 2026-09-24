// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
)

// bundledRegistry is the bundled manifests only.
func bundledRegistry() *adapters.Registry {
	return adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: builtinLanguages()})
}

// fakeDoctor is a doctor over fake lookups: find and resolvePython return
// the given results, the dotnet host is host (or missing), and probes of
// the paths in broken fail.
func fakeDoctor(loc adapters.Location, findErr error, host string, broken map[string]bool, rt adapters.Runtime, pyErr error) doctor {
	return doctor{
		find: func(*adapters.Manifest) (adapters.Location, error) { return loc, findErr },
		findHost: func() (string, error) {
			if host == "" {
				return "", errors.New("no host")
			}

			return host, nil
		},
		probe: func(_ context.Context, exe string, _ ...string) (string, error) {
			if broken[exe] {
				return "", errors.New("exit status 1")
			}

			if exe == host {
				return "10.0.100\n", nil
			}

			return "NET Core debugger 3.2.0-1092 (x)\n\nCopyright\n", nil
		},
		resolvePython: func(context.Context, *adapters.Manifest, adapters.PythonInput) (adapters.Runtime, error) {
			return rt, pyErr
		},
	}
}

// TestDoctorNetcoredbgTexts pins netcoredbg's and the dotnet host's
// doctor lines to what eyedbg printed before manifests.
func TestDoctorNetcoredbgTexts(t *testing.T) {
	t.Parallel()

	const ncd, host = "/d/netcoredbg", "/usr/bin/dotnet"

	tests := []struct {
		name string
		d    doctor
		want []doctorCheck
	}{
		{"ok", fakeDoctor(adapters.Location{Path: ncd, Source: "installed"}, nil, host, nil, adapters.Runtime{}, nil), []doctorCheck{
			{Name: "netcoredbg", OK: true, Detail: "NET Core debugger 3.2.0-1092 (x) (installed, /d/netcoredbg)"},
			{Name: "dotnet", OK: true, Detail: "SDK 10.0.100 (/usr/bin/dotnet)"},
		}},
		{"not found", fakeDoctor(adapters.Location{}, adapters.ErrNotInstalled, "", nil, adapters.Runtime{}, nil), []doctorCheck{
			{Name: "netcoredbg", Detail: "not found", Fix: "run 'eyedbg adapters install netcoredbg' or set EYEDBG_NETCOREDBG"},
			{Name: "dotnet", Detail: "the dotnet host was not found", Fix: "install the .NET SDK and put dotnet on PATH or set DOTNET_ROOT (the daemon uses the environment of the eyedbg that started it)"},
		}},
		{"bad env", fakeDoctor(adapters.Location{}, errors.New("EYEDBG_NETCOREDBG=/x: no such file"), host, nil, adapters.Runtime{}, nil), []doctorCheck{
			{Name: "netcoredbg", Detail: "EYEDBG_NETCOREDBG=/x: no such file", Fix: "fix or unset EYEDBG_NETCOREDBG"},
			{Name: "dotnet", OK: true, Detail: "SDK 10.0.100 (/usr/bin/dotnet)"},
		}},
		{"does not run", fakeDoctor(adapters.Location{Path: ncd, Source: "path"}, nil, host, map[string]bool{ncd: true, host: true}, adapters.Runtime{}, nil), []doctorCheck{
			{Name: "netcoredbg", Detail: "/d/netcoredbg does not run: exit status 1", Fix: "reinstall it, or check that the .NET runtime it needs is present"},
			{Name: "dotnet", Detail: "/usr/bin/dotnet does not run: exit status 1", Fix: "repair the .NET SDK install"},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Named: a missing adapter is a problem, as before manifests.
			got, err := tt.d.run(t.Context(), bundledRegistry(), []string{"dotnet"})
			if err != nil {
				t.Fatal(err)
			}

			if len(got) != len(tt.want) {
				t.Fatalf("checks = %+v, want %+v", got, tt.want)
			}

			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("check %d = %+v\nwant     %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestDoctorPython(t *testing.T) {
	t.Parallel()

	rt := adapters.Runtime{
		Exe: "/usr/bin/python3.13", Version: "3.13.5", Source: "path", Root: "/data/debugpy/1.8.22", RootSource: "installed", ModuleVersion: "1.8.22",
	}
	missing := errors.Join(api.NewError(api.CodeAdapterMissing, "debugpy is not installed for /usr/bin/python3 (Python 3.13.5)", "run 'eyedbg adapters install debugpy'"),
		adapters.ErrNotInstalled)
	broken := api.NewError(api.CodeAdapterMissing, "EYEDBG_PYTHON=/gone: not found", "fix or unset EYEDBG_PYTHON")

	tests := []struct {
		name  string
		rt    adapters.Runtime
		err   error
		named bool
		want  doctorCheck
	}{
		{"ok", rt, nil, false, doctorCheck{
			Name: "debugpy", OK: true, Detail: "debugpy 1.8.22 (installed, /data/debugpy/1.8.22) on Python 3.13.5 (/usr/bin/python3.13, path)",
		}},
		{"missing, not named", adapters.Runtime{}, missing, false, doctorCheck{
			Name: "debugpy", Detail: missing.Error(), Fix: "run 'eyedbg adapters install debugpy'", Missing: true,
		}},
		{"missing, named", adapters.Runtime{}, missing, true, doctorCheck{
			Name: "debugpy", Detail: missing.Error(), Fix: "run 'eyedbg adapters install debugpy'",
		}},
		{"broken, not named", adapters.Runtime{}, broken, false, doctorCheck{
			Name: "debugpy", Detail: "EYEDBG_PYTHON=/gone: not found", Fix: "fix or unset EYEDBG_PYTHON",
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := fakeDoctor(adapters.Location{}, adapters.ErrNotInstalled, "", nil, tt.rt, tt.err)

			var names []string
			if tt.named {
				names = []string{"python"}
			}

			checks, err := d.run(t.Context(), bundledRegistry(), names)
			if err != nil {
				t.Fatal(err)
			}

			var got *doctorCheck

			for i := range checks {
				if checks[i].Name == "debugpy" {
					got = &checks[i]
				}
			}

			if got == nil || *got != tt.want {
				t.Fatalf("debugpy check = %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

// TestDoctorAll: without names every adapter is checked, netcoredbg (and
// the dotnet host) first, and missing ones are not problems.
func TestDoctorAll(t *testing.T) {
	t.Parallel()

	d := fakeDoctor(adapters.Location{}, adapters.ErrNotInstalled, "", nil, adapters.Runtime{},
		errors.Join(errors.New("no Python interpreter found"), adapters.ErrNotInstalled))

	checks, err := d.run(t.Context(), bundledRegistry(), nil)
	if err != nil {
		t.Fatal(err)
	}

	var names []string

	for _, c := range checks {
		names = append(names, c.Name)

		if c.OK || !c.Missing {
			t.Errorf("check %+v: want missing, not a problem", c)
		}
	}

	want := []string{"netcoredbg", "dotnet", "debugpy", "delve", "lldb-dap-c", "lldb-dap-cpp", "lldb-dap-rust"}
	if !slices.Equal(names, want) {
		t.Fatalf("checks = %v, want %v", names, want)
	}

	if _, err := d.run(t.Context(), bundledRegistry(), []string{"cobol"}); err == nil {
		t.Fatal("doctor cobol: want an unknown adapter error")
	}
}
