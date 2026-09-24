// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import (
	"context"
	"path/filepath"
	"slices"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Driver debugs one language as its adapter manifest describes.
type Driver struct {
	m *adapters.Manifest

	// The lookups, replaced in tests.
	find          func(m *adapters.Manifest) (adapters.Location, error)
	resolvePython func(ctx context.Context, m *adapters.Manifest, in adapters.PythonInput) (adapters.Runtime, error)
}

// New returns the driver for the language manifest m serves; m must have
// a language that is not built in.
func New(m *adapters.Manifest) *Driver {
	return &Driver{m: m, find: adapters.Find, resolvePython: adapters.ResolvePython}
}

// Drivers returns a driver for every language reg's manifests serve that
// no Go driver does (language.builtin unset).
func Drivers(reg *adapters.Registry) []session.Driver {
	var out []session.Driver

	for _, m := range reg.Adapters() {
		if m.LanguageName() != "" && !m.Builtin() {
			out = append(out, New(m))
		}
	}

	return out
}

// Name implements session.Driver: the manifest's language.
func (d *Driver) Name() string { return d.m.LanguageName() }

// envList renders env as "NAME=VALUE" strings sorted by name, for adapters
// (like lldb-dap <=19) whose launch "env" argument is an array, not an
// object.
func envList(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}

	slices.Sort(names)

	out := make([]string, 0, len(names))
	for _, k := range names {
		out = append(out, k+"="+env[k])
	}

	return out
}

// Manifest returns the manifest the driver follows.
func (d *Driver) Manifest() *adapters.Manifest { return d.m }

// Prepare implements session.Driver: it checks the options and inputs,
// finds the adapter (and, for a Python adapter, the interpreter) and
// renders the manifest's launch arguments. Project and NoBuild are
// ignored: nothing is built.
func (d *Driver) Prepare(ctx context.Context, spec session.LaunchSpec) (session.Launch, error) {
	opts, err := d.options(spec.Options)
	if err != nil {
		return session.Launch{}, err
	}

	program, err := d.required(spec, opts)
	if err != nil {
		return session.Launch{}, err
	}

	cwd, err := workDir(spec)
	if err != nil {
		return session.Launch{}, err
	}

	programDir := ""
	if spec.Program != "" {
		programDir = filepath.Dir(spec.Program)
	}

	in := adapters.PythonInput{Cwd: cwd, ProgramDir: programDir, VirtualEnv: spec.VirtualEnv}
	if o := d.interpreterOption(); o != "" {
		in.Interpreter = clientPath(opts[o], spec.ClientDir)
	}

	launch, vars, err := d.adapter(ctx, opts, in)
	if err != nil {
		return session.Launch{}, err
	}

	vars[adapters.VarProgram] = spec.Program
	vars[adapters.VarArgs] = spec.Args
	vars[adapters.VarCwd] = cwd
	vars[adapters.VarEnv] = spec.Env
	vars[adapters.VarEnvList] = envList(spec.Env)
	vars[adapters.VarStopOnEntry] = spec.StopOnEntry

	launch.Arguments = adapters.Render(d.m.Launch.Arguments, vars)
	launch.Program = program
	launch.Request = session.RequestLaunch

	return launch, nil
}

// nativeAttachHint lists why attaching might fail for a native (runtime
// "") manifest-driven adapter. Unlike .NET's own pipe transport (see
// drivers/dotnet's attachHint), these typically attach through the OS's own
// mechanism, restricted by default on both Linux and macOS.
const nativeAttachHint = "the process must be yours, not already under a debugger, and the OS must allow it: " +
	"on Linux /proc/sys/kernel/yama/ptrace_scope must be 0 (1, Ubuntu's default, only allows a debugger " +
	"to attach to its own child processes; `sudo sysctl kernel.yama.ptrace_scope=0`), " +
	"on macOS the adapter needs the task_for_pid entitlement (install Xcode's command line tools)"

// PrepareAttach implements session.Attacher. Without an attach template
// the manifest's language can't attach: UNSUPPORTED_BY_ADAPTER, with the
// manifest's hint.
func (d *Driver) PrepareAttach(ctx context.Context, spec api.AttachSpec) (session.Launch, error) {
	if d.m.Attach == nil {
		return session.Launch{}, api.NewError(api.CodeUnsupported,
			"the "+d.Name()+" debug adapter can't attach to a running process", d.m.AttachUnsupported)
	}

	opts, err := d.options(nil)
	if err != nil {
		return session.Launch{}, err
	}

	in := adapters.PythonInput{}
	if o := d.interpreterOption(); o != "" {
		in.Interpreter, _ = opts[o].(string)
	}

	launch, vars, err := d.adapter(ctx, opts, in)
	if err != nil {
		return session.Launch{}, err
	}

	vars[adapters.VarPID] = spec.PID

	launch.Arguments = adapters.Render(d.m.Attach.Arguments, vars)
	launch.Request = session.RequestAttach
	launch.PID = spec.PID

	if d.m.Adapter.Runtime == adapters.RuntimeNative {
		launch.AttachHint = nativeAttachHint
	}

	return launch, nil
}
