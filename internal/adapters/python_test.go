// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// debugpyManifest returns the bundled debugpy manifest.
func debugpyManifest(t *testing.T) *Manifest {
	t.Helper()

	m := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs}).Adapter("debugpy")
	if m == nil {
		t.Fatal("no bundled debugpy manifest")
	}

	return m
}

// fakeSystem is a pythonEnv over fake files, PATH and interpreters.
type fakeSystem struct {
	files   map[string]bool        // existing paths
	path    map[string]string      // command name → path on PATH
	env     map[string]string      // environment
	pythons map[string]probeResult // interpreter path → what it prints
	broken  map[string]bool        // interpreters whose probe fails
	probed  *[]string              // interpreters probed, in order
	foreign map[string]bool        // paths another user owns
	unsafe  map[string]bool        // paths others can write
}

func (f fakeSystem) pythonEnv(goos string) pythonEnv {
	return pythonEnv{
		goos:   goos,
		getenv: func(k string) string { return f.env[k] },
		lookPath: func(name string) (string, error) {
			if p, ok := f.path[name]; ok {
				return p, nil
			}

			return "", errors.New("not found")
		},
		stat: func(p string) (fs.FileInfo, error) {
			if f.files[p] {
				return nil, nil
			}

			return nil, fs.ErrNotExist
		},
		dataDir: func() (string, error) { return filepath.FromSlash("/data"), nil },
		owned:   func(p string) bool { return !f.foreign[p] },
		trust: func(p string) error {
			if f.foreign[p] || f.unsafe[p] {
				return errors.New(p + " is not yours alone")
			}

			return nil
		},
		probe: func(_ context.Context, argv []string, _, _ string) (probeResult, error) {
			*f.probed = append(*f.probed, strings.Join(argv, " "))

			if f.broken[argv[0]] {
				return probeResult{}, errors.New("exit status 1")
			}

			if r, ok := f.pythons[argv[0]]; ok {
				return r, nil
			}

			return probeResult{}, errors.New("no such interpreter")
		},
	}
}

// own is a probe result of an interpreter with its own debugpy.
func own(exe, version string) probeResult {
	return probeResult{Executable: exe, Version: version, Root: filepath.FromSlash("/site/" + version), ModuleVersion: "1.8.0"}
}

// bare is a probe result of an interpreter without debugpy.
func bare(exe, version string) probeResult { return probeResult{Executable: exe, Version: version} }

func p(s string) string { return filepath.FromSlash(s) }

func TestResolvePython(t *testing.T) {
	t.Parallel()

	installed := p("/data/debugpy/1.8.22/debugpy/adapter")

	tests := []struct {
		name   string
		goos   string
		sys    fakeSystem
		in     PythonInput
		want   Runtime
		probed string
		code   api.Code
		errHas string
	}{
		{
			name: "option wins over env, venv and PATH",
			sys: fakeSystem{
				files: map[string]bool{p("/opt/py"): true, p("/w/.venv/pyvenv.cfg"): true, p("/site/3.12.1/debugpy/adapter"): true},
				env:   map[string]string{"EYEDBG_PYTHON": p("/env/py")}, path: map[string]string{"python3": p("/usr/bin/python3")},
				pythons: map[string]probeResult{p("/opt/py"): own(p("/opt/py"), "3.12.1")},
			},
			in:     PythonInput{Interpreter: p("/opt/py"), Cwd: p("/w")},
			want:   Runtime{Exe: p("/opt/py"), Version: "3.12.1", Source: PythonFromOption, Root: p("/site/3.12.1"), RootSource: RootPython, ModuleVersion: "1.8.0"},
			probed: p("/opt/py"),
		},
		{
			name: "env wins over venv",
			sys: fakeSystem{
				files: map[string]bool{p("/env/py"): true, p("/w/.venv/pyvenv.cfg"): true, installed: true},
				env:   map[string]string{"EYEDBG_PYTHON": p("/env/py")}, pythons: map[string]probeResult{p("/env/py"): bare(p("/env/real"), "3.13.5")},
			},
			in:     PythonInput{Cwd: p("/w")},
			want:   Runtime{Exe: p("/env/real"), Version: "3.13.5", Source: PythonFromEnv, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/env/py"),
		},
		{
			name: "venv in the cwd before the program's",
			sys: fakeSystem{
				files:   map[string]bool{p("/w/venv/pyvenv.cfg"): true, p("/w/app/.venv/pyvenv.cfg"): true, installed: true},
				pythons: map[string]probeResult{p("/w/venv/bin/python"): bare(p("/w/venv/bin/python"), "3.11.2")},
			},
			in:     PythonInput{Cwd: p("/w"), ProgramDir: p("/w/app")},
			want:   Runtime{Exe: p("/w/venv/bin/python"), Version: "3.11.2", Source: PythonFromVenv, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/w/venv/bin/python"),
		},
		{
			name: "venv in a parent of the program's directory",
			sys: fakeSystem{
				files:   map[string]bool{p("/proj/.venv/pyvenv.cfg"): true, installed: true},
				pythons: map[string]probeResult{p("/proj/.venv/bin/python"): bare(p("/proj/.venv/bin/python"), "3.12.0")},
			},
			in:     PythonInput{Cwd: p("/elsewhere"), ProgramDir: p("/proj/src/pkg")},
			want:   Runtime{Exe: p("/proj/.venv/bin/python"), Version: "3.12.0", Source: PythonFromVenv, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/proj/.venv/bin/python"),
		},
		{
			name: "windows venv layout",
			goos: "windows",
			sys: fakeSystem{
				files:   map[string]bool{p("/w/.venv/pyvenv.cfg"): true, installed: true},
				pythons: map[string]probeResult{p("/w/.venv/Scripts/python.exe"): bare(p("/w/.venv/Scripts/python.exe"), "3.12.0")},
			},
			in:     PythonInput{Cwd: p("/w")},
			want:   Runtime{Exe: p("/w/.venv/Scripts/python.exe"), Version: "3.12.0", Source: PythonFromVenv, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/w/.venv/Scripts/python.exe"),
		},
		{
			name: "a directory without pyvenv.cfg is no venv",
			sys: fakeSystem{
				files: map[string]bool{p("/w/.venv/bin/python"): true, installed: true}, path: map[string]string{"python3": p("/usr/bin/python3")},
				pythons: map[string]probeResult{p("/usr/bin/python3"): bare(p("/usr/bin/python3.13"), "3.13.5")},
			},
			in:     PythonInput{Cwd: p("/w")},
			want:   Runtime{Exe: p("/usr/bin/python3.13"), Version: "3.13.5", Source: PythonFromPath, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/usr/bin/python3"),
		},
		{
			name: "PATH candidates fall through failed probes",
			sys: fakeSystem{
				files:  map[string]bool{installed: true},
				path:   map[string]string{"python3": p("/bin/python3"), "py": p("/bin/py")},
				broken: map[string]bool{p("/bin/python3"): true}, pythons: map[string]probeResult{p("/bin/py"): bare(p("/real/python"), "3.10.0")},
			},
			want:   Runtime{Exe: p("/real/python"), Version: "3.10.0", Source: PythonFromPath, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/bin/python3") + "," + p("/bin/py") + " -3",
		},
		{
			name: "a broken venv doesn't fall back",
			sys: fakeSystem{
				files: map[string]bool{p("/w/.venv/pyvenv.cfg"): true}, path: map[string]string{"python3": p("/bin/python3")},
				broken: map[string]bool{p("/w/.venv/bin/python"): true}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
			},
			in: PythonInput{Cwd: p("/w")}, probed: p("/w/.venv/bin/python"), code: api.CodeAdapterMissing, errHas: "does not run",
		},
		{
			name:   "a missing option interpreter is INVALID_REQUEST",
			sys:    fakeSystem{path: map[string]string{"python3": p("/bin/python3")}},
			in:     PythonInput{Interpreter: p("/nonexistent/python")},
			code:   api.CodeInvalidRequest,
			errHas: "--opt python=",
		},
		{
			name: "a broken option interpreter doesn't fall back",
			sys: fakeSystem{
				files: map[string]bool{p("/opt/py"): true}, broken: map[string]bool{p("/opt/py"): true},
				path: map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
			},
			in: PythonInput{Interpreter: p("/opt/py")}, probed: p("/opt/py"), code: api.CodeInvalidRequest, errHas: "does not run",
		},
		{
			name:   "a missing env interpreter doesn't fall back",
			sys:    fakeSystem{env: map[string]string{"EYEDBG_PYTHON": p("/gone/py")}, path: map[string]string{"python3": p("/bin/python3")}},
			code:   api.CodeAdapterMissing,
			errHas: "EYEDBG_PYTHON=",
		},
		{
			name:   "no interpreter at all",
			sys:    fakeSystem{},
			code:   api.CodeAdapterMissing,
			errHas: "no Python interpreter found (tried python3, python, py -3)",
		},
		{
			name: "no debugpy anywhere",
			sys: fakeSystem{
				path: map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
			},
			probed: p("/bin/python3"), code: api.CodeAdapterMissing,
			errHas: "debugpy is not installed for " + p("/bin/python3") + " (Python 3.13.5)",
		},
		{
			name: "too old for the installed copy: its own copy",
			sys: fakeSystem{
				files: map[string]bool{installed: true, p("/site/3.9.2/debugpy/adapter"): true},
				path:  map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): own(p("/bin/python3"), "3.9.2")},
			},
			want:   Runtime{Exe: p("/bin/python3"), Version: "3.9.2", Source: PythonFromPath, Root: p("/site/3.9.2"), RootSource: RootPython, ModuleVersion: "1.8.0"},
			probed: p("/bin/python3"),
		},
		{
			name: "too old for the installed copy and no own copy",
			sys: fakeSystem{
				files: map[string]bool{installed: true},
				path:  map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.9.2")},
			},
			probed: p("/bin/python3"), code: api.CodeAdapterMissing, errHas: "debugpy 1.8.22 needs Python 3.10+, and " + p("/bin/python3") + " is Python 3.9.2",
		},
		{
			name: "a venv in a directory another user owns is not searched",
			sys: fakeSystem{
				files:   map[string]bool{p("/tmp/.venv/pyvenv.cfg"): true, installed: true},
				foreign: map[string]bool{p("/tmp"): true, p("/tmp/.venv"): true, p("/"): true},
				path:    map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
			},
			in:     PythonInput{Cwd: p("/tmp"), ProgramDir: p("/tmp/work/sub")},
			want:   Runtime{Exe: p("/bin/python3"), Version: "3.13.5", Source: PythonFromPath, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/bin/python3"),
		},
		{
			name: "the walk up stops at the first directory another user owns",
			sys: fakeSystem{
				files:   map[string]bool{p("/srv/.venv/pyvenv.cfg"): true, installed: true},
				foreign: map[string]bool{p("/srv/shared"): true},
				path:    map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
			},
			in:     PythonInput{ProgramDir: p("/srv/shared/proj")},
			want:   Runtime{Exe: p("/bin/python3"), Version: "3.13.5", Source: PythonFromPath, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/bin/python3"),
		},
		{
			name: "a venv another user owns is refused",
			sys: fakeSystem{
				files: map[string]bool{p("/w/.venv/pyvenv.cfg"): true, installed: true}, foreign: map[string]bool{p("/w/.venv"): true},
				path: map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
			},
			in: PythonInput{Cwd: p("/w")}, code: api.CodeAdapterMissing, errHas: "is not safe to run",
		},
		{
			name: "a venv others can write is refused",
			sys: fakeSystem{
				files: map[string]bool{p("/w/.venv/pyvenv.cfg"): true, installed: true}, unsafe: map[string]bool{p("/w/.venv/bin"): true},
				path: map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
			},
			in: PythonInput{Cwd: p("/w")}, code: api.CodeAdapterMissing, errHas: "is not safe to run",
		},
		{
			name: "installed copy wins over the interpreter's own",
			sys: fakeSystem{
				files: map[string]bool{installed: true, p("/site/3.12.1/debugpy/adapter"): true},
				path:  map[string]string{"python3": p("/bin/python3")}, pythons: map[string]probeResult{p("/bin/python3"): own(p("/bin/python3"), "3.12.1")},
			},
			want:   Runtime{Exe: p("/bin/python3"), Version: "3.12.1", Source: PythonFromPath, Root: p("/data/debugpy/1.8.22"), RootSource: RootInstalled, ModuleVersion: "1.8.22"},
			probed: p("/bin/python3"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var probed []string

			tt.sys.probed = &probed

			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}

			got, err := tt.sys.pythonEnv(goos).resolve(t.Context(), debugpyManifest(t), tt.in)

			if tt.code != "" {
				if api.CodeOf(err) != tt.code || !strings.Contains(err.Error(), tt.errHas) {
					t.Fatalf("err = %v (code %s), want %s containing %q", err, api.CodeOf(err), tt.code, tt.errHas)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("resolve = %+v, %v\nwant %+v", got, err, tt.want)
			}

			if strings.Join(probed, ",") != tt.probed {
				t.Fatalf("probed %q, want %q", strings.Join(probed, ","), tt.probed)
			}
		})
	}
}

func TestResolvePythonMissingIsNotInstalled(t *testing.T) {
	t.Parallel()

	var probed []string

	sys := fakeSystem{
		probed: &probed, path: map[string]string{"python3": p("/bin/python3")},
		pythons: map[string]probeResult{p("/bin/python3"): bare(p("/bin/python3"), "3.13.5")},
	}

	_, err := sys.pythonEnv("linux").resolve(t.Context(), debugpyManifest(t), PythonInput{})
	if !errors.Is(err, ErrNotInstalled) || api.CodeOf(err) != api.CodeAdapterMissing {
		t.Fatalf("err = %v; want ErrNotInstalled and ADAPTER_NOT_INSTALLED", err)
	}

	hint := ""
	if e, ok := errors.AsType[*api.Error](err); ok {
		hint = e.Hint
	}

	want := "run 'eyedbg adapters install debugpy' (downloads debugpy 1.8.22 once), or '" + p("/bin/python3") + " -m pip install debugpy'"
	if hint != want {
		t.Fatalf("hint = %q\nwant %q", hint, want)
	}
}

func TestVersionAtLeast(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		v, minimum string
		want       bool
	}{
		{"3.10.0", "3.10", true},
		{"3.9.18", "3.10", false},
		{"3.13.5", "3.10", true},
		{"4.0.0", "3.10", true},
		{"2.7.18", "3.10", false},
		{"3", "3.10", false},
		{"x.y", "3.10", false},
	} {
		if got := versionAtLeast(tt.v, tt.minimum); got != tt.want {
			t.Errorf("versionAtLeast(%q, %q) = %v", tt.v, tt.minimum, got)
		}
	}
}
