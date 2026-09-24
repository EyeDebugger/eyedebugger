// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Where a Python interpreter came from (Runtime.Source).
const (
	PythonFromOption     = "option"
	PythonFromEnv        = "env"
	PythonFromVirtualEnv = "virtualenv"
	PythonFromVenv       = "venv"
	PythonFromPath       = "path"
)

// Where a Python adapter's package came from (Runtime.RootSource).
const (
	// RootInstalled is the copy 'eyedbg adapters install' downloaded.
	RootInstalled = "installed"
	// RootPython is the interpreter's own copy (e.g. pip-installed).
	RootPython = "python"
)

// probeTimeout bounds one interpreter probe.
const probeTimeout = 10 * time.Second

// pythonProbe prints the interpreter's executable and version, and where
// the package named by argv[1] is, without importing it. It drops the
// working directory from sys.path first, so nothing in it is imported.
const pythonProbe = `import sys
if sys.path and sys.path[0] in ('', '.'): del sys.path[0]
import json, os, importlib.util
r = {"executable": sys.executable, "version": "%d.%d.%d" % sys.version_info[:3]}
try:
    s = importlib.util.find_spec(sys.argv[1])
    if s is not None and s.submodule_search_locations:
        r["root"] = os.path.dirname(list(s.submodule_search_locations)[0])
        from importlib.metadata import version
        r["moduleVersion"] = version(sys.argv[1])
except Exception as e:
    r["moduleError"] = str(e)
print(json.dumps(r))
`

// PythonInput is where interpreter discovery starts.
type PythonInput struct {
	// Interpreter is the manifest's interpreter option's value: a path or
	// a command name ("" for none).
	Interpreter string
	// Cwd is the program's working directory: the probe runs there, and
	// project venvs are looked for there first.
	Cwd string
	// ProgramDir is the program's directory: venvs are looked for there
	// and in its parents.
	ProgramDir string
	// VirtualEnv is the caller's $VIRTUAL_ENV, if any ("" for none): tried
	// as a venv (like a project venv) after Interpreter and the
	// manifest's environment variable, before a project venv.
	VirtualEnv string
}

// Runtime is the interpreter a Python adapter and its program run on.
type Runtime struct {
	// Exe is the interpreter's sys.executable.
	Exe string
	// Version is its version, X.Y.Z.
	Version string
	// Source is PythonFromOption, PythonFromEnv, PythonFromVirtualEnv,
	// PythonFromVenv or PythonFromPath.
	Source string
	// Root is the directory holding the adapter's package.
	Root string
	// RootSource is RootInstalled or RootPython.
	RootSource string
	// ModuleVersion is the package's version.
	ModuleVersion string
}

// Entry is the adapter's entry under the package root.
func (r Runtime) Entry(m *Manifest) string {
	return filepath.Join(r.Root, filepath.FromSlash(m.Adapter.Entry))
}

// probeResult is what the probe prints.
type probeResult struct {
	Executable    string `json:"executable"`
	Version       string `json:"version"`
	Root          string `json:"root,omitempty"`
	ModuleVersion string `json:"moduleVersion,omitempty"`
	ModuleError   string `json:"moduleError,omitempty"`
}

// pythonEnv is what discovery reads from the system; tests replace it.
type pythonEnv struct {
	goos     string
	getenv   func(string) string
	lookPath func(string) (string, error)
	stat     func(string) (fs.FileInfo, error)
	dataDir  func() (string, error)
	probe    func(ctx context.Context, argv []string, dir, module string) (probeResult, error)
	// trust checks a venv file or directory; owned tells whether the user
	// owns a directory (checkTrust and ownedByUser).
	trust func(string) error
	owned func(string) bool
}

func systemPythonEnv() pythonEnv {
	return pythonEnv{
		goos: runtime.GOOS, getenv: os.Getenv, lookPath: exec.LookPath, stat: os.Stat, dataDir: DataDir,
		probe: runProbe, trust: checkTrust, owned: ownedByUser,
	}
}

// ResolvePython finds the interpreter for Python adapter m and the
// package root to run the adapter from. Order: in.Interpreter, the
// manifest's environment variable, in.VirtualEnv, a project venv, then the
// manifest's commands on PATH; only PATH candidates fall through to the
// next.
func ResolvePython(ctx context.Context, m *Manifest, in PythonInput) (Runtime, error) {
	return systemPythonEnv().resolve(ctx, m, in)
}

// candidate is an interpreter to probe.
type candidate struct {
	argv   []string
	source string
}

func (e pythonEnv) resolve(ctx context.Context, m *Manifest, in PythonInput) (Runtime, error) {
	if m.Python == nil {
		return Runtime{}, fmt.Errorf("%s is not a python adapter", m.Name)
	}

	c, found, err := e.explicit(m, in)
	if err != nil {
		return Runtime{}, err
	}

	if !found {
		if found, err = e.virtualEnv(m, in, &c); err != nil {
			return Runtime{}, err
		}
	}

	if !found {
		if found, err = e.venv(m, in, &c); err != nil {
			return Runtime{}, err
		}
	}

	if found {
		p, err := e.probe(ctx, c.argv, in.Cwd, m.Python.Module)
		if err != nil {
			return Runtime{}, e.probeFailed(m, c, err)
		}

		return e.withRoot(m, c, p)
	}

	var tried []string

	for _, cmd := range m.Python.Commands {
		tried = append(tried, strings.Join(cmd, " "))

		exe, err := e.lookPath(cmd[0])
		if err != nil {
			continue
		}

		c := candidate{argv: append([]string{exe}, cmd[1:]...), source: PythonFromPath}
		if p, err := e.probe(ctx, c.argv, in.Cwd, m.Python.Module); err == nil {
			return e.withRoot(m, c, p)
		}
	}

	return Runtime{}, notInstalled(api.NewError(api.CodeAdapterMissing,
		"no Python interpreter found (tried "+strings.Join(tried, ", ")+")", interpreterHint(m, "install Python")))
}

// explicit returns the interpreter the option or environment variable
// names, if either does.
func (e pythonEnv) explicit(m *Manifest, in PythonInput) (candidate, bool, error) {
	if in.Interpreter != "" {
		exe, err := e.command(in.Interpreter)
		if err != nil {
			return candidate{}, false, api.NewError(api.CodeInvalidRequest,
				fmt.Sprintf("--opt %s=%s: %v", m.Python.Option, in.Interpreter, err), "pass an existing Python interpreter")
		}

		return candidate{argv: []string{exe}, source: PythonFromOption}, true, nil
	}

	if m.Python.Env == "" {
		return candidate{}, false, nil
	}

	v := e.getenv(m.Python.Env)
	if v == "" {
		return candidate{}, false, nil
	}

	exe, err := e.command(v)
	if err != nil {
		return candidate{}, false, api.NewError(api.CodeAdapterMissing, fmt.Sprintf("%s=%s: %v", m.Python.Env, v, err),
			"fix or unset "+m.Python.Env+" (the daemon reads it when it starts: 'eyedbg daemon stop' to restart it)")
	}

	return candidate{argv: []string{exe}, source: PythonFromEnv}, true, nil
}

// command resolves an interpreter given as a path or a command name.
func (e pythonEnv) command(s string) (string, error) {
	if !strings.ContainsAny(s, `/\`) {
		p, err := e.lookPath(s)
		if err != nil {
			return "", fmt.Errorf("not found on PATH: %w", err)
		}

		return p, nil
	}

	if _, err := e.stat(s); err != nil {
		return "", fmt.Errorf("not found: %w", err)
	}

	return s, nil
}

// venv looks for a project virtual environment: in the working directory,
// then in the program's directory and its parents, searching only
// directories the user owns (the walk up stops at the first other one).
// A venv found must be the user's and writable by no one else (its root,
// its bin or Scripts directory and pyvenv.cfg): its interpreter is run.
func (e pythonEnv) venv(m *Manifest, in PythonInput, c *candidate) (bool, error) {
	for _, dir := range venvDirs(in, e.owned) {
		for _, name := range m.Python.Venvs {
			root := filepath.Join(dir, name)

			bin, exe, found, err := e.venvRoot(root)
			if !found {
				continue
			}

			if err != nil {
				return false, api.NewError(api.CodeAdapterMissing, "the virtual environment "+root+" is not safe to run: "+err.Error(),
					"fix its permissions, or choose the interpreter"+optionOrEnv(m))
			}

			*c = candidate{argv: []string{filepath.Join(bin, exe)}, source: PythonFromVenv}

			return true, nil
		}
	}

	return false, nil
}

// virtualEnv checks the caller's $VIRTUAL_ENV (in.VirtualEnv), if set, as a
// venv root directly, not a directory to search: unlike a project venv, a
// missing pyvenv.cfg or a failed permission check is a hard failure, like
// in.Interpreter and the manifest's environment variable, not silently
// skipped.
func (e pythonEnv) virtualEnv(m *Manifest, in PythonInput, c *candidate) (bool, error) {
	if in.VirtualEnv == "" {
		return false, nil
	}

	bin, exe, found, err := e.venvRoot(in.VirtualEnv)

	switch {
	case !found:
		return false, api.NewError(api.CodeAdapterMissing, "$VIRTUAL_ENV="+in.VirtualEnv+" has no pyvenv.cfg",
			"fix $VIRTUAL_ENV, or choose the interpreter"+optionOrEnv(m))
	case err != nil:
		return false, api.NewError(api.CodeAdapterMissing, "$VIRTUAL_ENV="+in.VirtualEnv+" is not safe to run: "+err.Error(),
			"fix its permissions, or choose the interpreter"+optionOrEnv(m))
	}

	*c = candidate{argv: []string{filepath.Join(bin, exe)}, source: PythonFromVirtualEnv}

	return true, nil
}

// venvRoot checks whether root is a venv (it has pyvenv.cfg) and, if so,
// that it passes the manifest permission check (root, its bin or Scripts
// directory and pyvenv.cfg all the user's and writable by no one else).
// found is false when root has no pyvenv.cfg; err is the permission
// failure, if any.
func (e pythonEnv) venvRoot(root string) (bin, exe string, found bool, err error) {
	cfg := filepath.Join(root, "pyvenv.cfg")
	if _, statErr := e.stat(cfg); statErr != nil {
		return "", "", false, nil //nolint:nilerr // no pyvenv.cfg means "not a venv", not a failure.
	}

	bin, exe = filepath.Join(root, "bin"), "python"
	if e.goos == goosWindows {
		bin, exe = filepath.Join(root, "Scripts"), "python.exe"
	}

	for _, p := range []string{root, bin, cfg} {
		if trustErr := e.trust(p); trustErr != nil {
			return "", "", true, trustErr
		}
	}

	return bin, exe, true, nil
}

// venvDirs are the directories searched for a venv, nearest first: the
// working directory if the user owns it, then the program's directory and
// its parents up to the first one the user doesn't own.
func venvDirs(in PythonInput, owned func(string) bool) []string {
	var dirs []string

	if in.Cwd != "" && owned(in.Cwd) {
		dirs = append(dirs, filepath.Clean(in.Cwd))
	}

	for dir := in.ProgramDir; dir != ""; {
		dir = filepath.Clean(dir)
		if !owned(dir) {
			break
		}

		if len(dirs) == 0 || dirs[0] != dir {
			dirs = append(dirs, dir)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}

		dir = parent
	}

	return dirs
}

// probeFailed is the error for an option, environment or venv interpreter
// that doesn't run.
func (e pythonEnv) probeFailed(m *Manifest, c candidate, err error) error {
	msg := fmt.Sprintf("%s (from %s) does not run: %v", c.argv[0], c.source, err)

	switch c.source {
	case PythonFromOption:
		return api.NewError(api.CodeInvalidRequest, msg, "pass a working Python interpreter")
	case PythonFromEnv:
		return api.NewError(api.CodeAdapterMissing, msg, "fix or unset "+m.Python.Env)
	default:
		return api.NewError(api.CodeAdapterMissing, msg,
			"repair the virtual environment, or choose another interpreter"+optionOrEnv(m))
	}
}

// withRoot picks the package root: the installed copy if it exists and
// the interpreter is new enough for it, else the interpreter's own.
func (e pythonEnv) withRoot(m *Manifest, c candidate, p probeResult) (Runtime, error) {
	rt := Runtime{Exe: p.Executable, Version: p.Version, Source: c.source}
	if rt.Exe == "" {
		rt.Exe = c.argv[0]
	}

	installed, haveInstalled := e.installedRoot(m)
	oldEnough := m.Python.MinVersion == "" || versionAtLeast(p.Version, m.Python.MinVersion)

	switch {
	case haveInstalled && oldEnough:
		rt.Root, rt.RootSource, rt.ModuleVersion = installed, RootInstalled, m.Version
	case p.Root != "":
		if _, err := e.stat(filepath.Join(p.Root, filepath.FromSlash(m.Adapter.Entry))); err != nil {
			return Runtime{}, api.NewError(api.CodeAdapterMissing,
				fmt.Sprintf("%s %s for %s has no %s", m.Python.Module, p.ModuleVersion, rt.Exe, m.Adapter.Entry), installHint(m, rt.Exe))
		}

		rt.Root, rt.RootSource, rt.ModuleVersion = p.Root, RootPython, p.ModuleVersion
	case haveInstalled:
		return Runtime{}, api.NewError(api.CodeAdapterMissing,
			fmt.Sprintf("%s %s needs Python %s+, and %s is Python %s", m.Name, m.Version, m.Python.MinVersion, rt.Exe, p.Version),
			interpreterHint(m, "use a newer Python")+", or '"+rt.Exe+" -m pip install "+m.Python.Module+"'")
	default:
		return Runtime{}, notInstalled(api.NewError(api.CodeAdapterMissing,
			fmt.Sprintf("%s is not installed for %s (Python %s)", m.Python.Module, rt.Exe, p.Version), installHint(m, rt.Exe)))
	}

	return rt, nil
}

// installedRoot is the downloaded package's root, if it is installed.
func (e pythonEnv) installedRoot(m *Manifest) (string, bool) {
	data, err := e.dataDir()
	if err != nil {
		return "", false
	}

	root := filepath.Join(data, m.Name, m.Version)
	if _, err := e.stat(installedEntry(m, root)); err != nil {
		return "", false
	}

	return root, true
}

// installHint says how to get m's package for interpreter exe.
func installHint(m *Manifest, exe string) string {
	pip := "'" + exe + " -m pip install " + m.Python.Module + "'"
	if m.Install == nil {
		return "run " + pip
	}

	return fmt.Sprintf("run 'eyedbg adapters install %s' (downloads %s %s once), or %s", m.Name, m.Name, m.Version, pip)
}

// interpreterHint starts with what, then names the ways to choose an
// interpreter.
func interpreterHint(m *Manifest, what string) string {
	if m.Python.MinVersion != "" {
		what += " " + m.Python.MinVersion + "+"
	}

	return what + optionOrEnv(m)
}

// optionOrEnv lists the manifest's ways to name an interpreter.
func optionOrEnv(m *Manifest) string {
	var ways []string

	if m.Python.Option != "" {
		ways = append(ways, "--opt "+m.Python.Option+"=PATH")
	}

	if m.Python.Env != "" {
		ways = append(ways, "set "+m.Python.Env+" for the daemon")
	}

	if len(ways) == 0 {
		return ""
	}

	return " (" + strings.Join(ways, ", or ") + ")"
}

// versionAtLeast reports whether version "X.Y[.Z]" is at least min "X.Y".
func versionAtLeast(version, minimum string) bool {
	v, ok1 := majorMinor(version)
	m, ok2 := majorMinor(minimum)

	return ok1 && ok2 && (v[0] > m[0] || (v[0] == m[0] && v[1] >= m[1]))
}

func majorMinor(s string) ([2]int, bool) {
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 2 {
		return [2]int{}, false
	}

	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])

	return [2]int{major, minor}, err1 == nil && err2 == nil
}

// runProbe runs the probe on an interpreter, in dir.
func runProbe(ctx context.Context, argv []string, dir, module string) (probeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	args := append(append([]string{}, argv[1:]...), "-c", pythonProbe, module)
	cmd := exec.CommandContext(ctx, argv[0], args...) //nolint:gosec // The user's interpreter, chosen by their settings or PATH; no shell.

	if dir != "" {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			cmd.Dir = dir
		}
	}

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return probeResult{}, fmt.Errorf("%w: %s", err, lastLine(stderr.String()))
	}

	var r probeResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &r); err != nil || r.Version == "" {
		return probeResult{}, errors.New("it printed no version: is it Python 3?")
	}

	return r, nil
}

// lastLine is the last non-empty line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(lines[len(lines)-1])
}

// notInstalledError is an ADAPTER_NOT_INSTALLED error for something that
// is missing, not broken: errors.Is(err, ErrNotInstalled) holds.
type notInstalledError struct {
	err *api.Error
}

func notInstalled(err *api.Error) error { return &notInstalledError{err: err} }

func (e *notInstalledError) Error() string   { return e.err.Error() }
func (e *notInstalledError) Unwrap() []error { return []error{e.err, ErrNotInstalled} }
