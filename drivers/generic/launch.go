// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// options checks the --opt values against the manifest and returns every
// option's typed value (set, else its default).
func (d *Driver) options(given map[string]string) (map[string]any, error) {
	for name, v := range given {
		o, ok := d.m.Options[name]
		if !ok {
			return nil, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("%s has no option %q", d.Name(), name), d.optionsHint())
		}

		if v == "" {
			continue
		}

		if _, err := o.Parse(v); err != nil {
			return nil, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("--opt %s=%s: %v", name, v, err), d.optionsHint())
		}
	}

	out := make(map[string]any, len(d.m.Options))

	for name, o := range d.m.Options {
		v := given[name]
		if v == "" {
			v = o.Default
		}

		if v == "" {
			continue
		}

		// Values were checked above and defaults when loading.
		out[name], _ = o.Parse(v)
	}

	return out, nil
}

// optionsHint lists the language's options: "name (type): help; ...".
func (d *Driver) optionsHint() string {
	names := make([]string, 0, len(d.m.Options))
	for name := range d.m.Options {
		names = append(names, name)
	}

	if len(names) == 0 {
		return d.Name() + " takes no --opt options"
	}

	slices.Sort(names)

	parts := make([]string, 0, len(names))
	for _, name := range names {
		o := d.m.Options[name]
		parts = append(parts, fmt.Sprintf("%s (%s): %s", name, o.Kind(), o.Help))
	}

	return d.Name() + " options: " + strings.Join(parts, "; ")
}

// required checks that one of the launch's required inputs is set and
// the program exists; it returns what runs, for status.
func (d *Driver) required(spec session.LaunchSpec, opts map[string]any) (string, error) {
	program := ""

	for _, r := range d.m.Launch.Require {
		name, isOpt := strings.CutPrefix(r, "opt.")

		switch {
		case !isOpt && spec.Program != "":
			program = spec.Program
		case isOpt && opts[name] != nil && opts[name] != "":
			program = fmt.Sprintf("%s=%v", name, opts[name])
		default:
			continue
		}

		break
	}

	if program == "" {
		return "", api.NewError(api.CodeInvalidRequest, d.Name()+" needs "+d.requireText(), "see 'eyedbg help start'")
	}

	if spec.Program != "" {
		if _, err := os.Stat(spec.Program); err != nil {
			return "", api.NewError(api.CodeInvalidRequest, "program "+spec.Program+" not found", "check the --program path")
		}
	}

	return program, nil
}

// requireText is "--program FILE or --opt module=MODULE".
func (d *Driver) requireText() string {
	var ways []string

	for _, r := range d.m.Launch.Require {
		if name, ok := strings.CutPrefix(r, "opt."); ok {
			ways = append(ways, "--opt "+name+"="+strings.ToUpper(name))
		} else {
			ways = append(ways, "--program FILE")
		}
	}

	return strings.Join(ways, " or ")
}

// workDir is the program's working directory: --cwd, else the client's,
// else the program's directory.
func workDir(spec session.LaunchSpec) (string, error) {
	if spec.Cwd != "" {
		if info, err := os.Stat(spec.Cwd); err != nil || !info.IsDir() {
			return "", api.NewError(api.CodeInvalidRequest, "--cwd "+spec.Cwd+" is not a directory", "")
		}

		return spec.Cwd, nil
	}

	if spec.ClientDir != "" {
		return spec.ClientDir, nil
	}

	if spec.Program != "" {
		return filepath.Dir(spec.Program), nil
	}

	return "", nil
}

// interpreterOption is the option naming a Python adapter's interpreter,
// "" for none.
func (d *Driver) interpreterOption() string {
	if d.m.Adapter.Runtime != adapters.RuntimePython {
		return ""
	}

	return d.m.Python.Option
}

// clientPath is an interpreter option's value, with a relative path (one
// with a separator) made absolute against the client's directory: the
// daemon's own working directory is not the caller's. A bare command name
// is left for a PATH lookup.
func clientPath(v any, clientDir string) string {
	s, _ := v.(string)
	if s == "" || clientDir == "" || filepath.IsAbs(s) || !strings.ContainsAny(s, `/\`) {
		return s
	}

	return filepath.Join(clientDir, s)
}

// adapter finds the adapter process and returns the Launch fields every
// session of the language shares, and the template variables so far.
func (d *Driver) adapter(ctx context.Context, opts map[string]any, in adapters.PythonInput) (session.Launch, adapters.Vars, error) {
	m := d.m
	vars := adapters.Vars{}

	for name, v := range opts {
		vars[adapters.OptVar(name)] = v
	}

	launch := session.Launch{
		AdapterEnv: adapters.Environ(m), AdapterID: m.Adapter.ID, AdapterName: m.Name,
		ExceptionFilters: exceptionFilters(m), SideEffects: sideEffects(m.EvalGuard),
	}

	if m.Adapter.Runtime == adapters.RuntimePython {
		rt, err := d.resolvePython(ctx, m, in)
		if err != nil {
			return session.Launch{}, nil, err
		}

		launch.Adapter = rt.Exe
		launch.AdapterArgs = append([]string{rt.Entry(m)}, m.Adapter.Args...)
		vars[adapters.VarRuntime] = rt.Exe

		return launch, vars, nil
	}

	loc, err := d.find(m)

	switch {
	case errors.Is(err, adapters.ErrNotInstalled):
		return session.Launch{}, nil, api.NewError(api.CodeAdapterMissing, m.Name+" is not installed", nativeHint(m))
	case err != nil:
		return session.Launch{}, nil, api.NewError(api.CodeAdapterMissing, err.Error(), nativeHint(m))
	}

	launch.Adapter = loc.Path

	if m.Adapter.Transport == adapters.TransportConnect {
		args := m.Adapter.Args
		launch.SocketArgs = func(socket string) []string {
			return adapters.RenderArgs(args, adapters.Vars{adapters.VarSocket: socket})
		}
	} else {
		launch.AdapterArgs = slices.Clone(m.Adapter.Args)
	}

	return launch, vars, nil
}

// nativeHint says how to get a native adapter.
func nativeHint(m *adapters.Manifest) string {
	var ways []string

	if m.Install != nil {
		ways = append(ways, "run 'eyedbg adapters install "+m.Name+"'")
	}

	if m.Adapter.Env != "" {
		ways = append(ways, "set "+m.Adapter.Env+" to it")
	}

	if m.Adapter.Path {
		ways = append(ways, "put "+m.Adapter.Entry+" on PATH")
	}

	if len(ways) == 0 {
		return "install " + m.Adapter.Entry
	}

	return strings.Join(ways, ", or ")
}

// exceptionFilters maps the manifest's exceptions to the session's modes.
func exceptionFilters(m *adapters.Manifest) map[api.ExceptionMode][]string {
	if len(m.Exceptions) == 0 {
		return nil
	}

	out := make(map[api.ExceptionMode][]string, len(m.Exceptions))
	for mode, filters := range m.Exceptions {
		out[api.ExceptionMode(mode)] = slices.Clone(filters)
	}

	return out
}
