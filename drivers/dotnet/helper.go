// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/helper"
)

// The .NET side helper (helpers/dotnet, docs/adr/0016): where it is and
// what it speaks.
const (
	// EnvHelper overrides where the helper is: an absolute path to its
	// .dll (run with the dotnet host) or to an executable (run directly).
	EnvHelper = "EYEDBG_DOTNET_HELPER"
	// HelperDLL is the helper's file name, under helpers/dotnet next to
	// eyedbg.
	HelperDLL = "eyedbg-dotnet-helper.dll"
	// HelperProtocol is the helper protocol this eyedbg speaks.
	HelperProtocol = 1

	helperName = "the .NET helper"
)

// Helper methods and notifications (protocol 1).
const (
	// MethodProcesses lists the pids of processes with a .NET diagnostics
	// endpoint: ProcessesResult.
	MethodProcesses = "processes"
	// MethodCounters collects System.Runtime counters: CountersParams,
	// NotifyCounterSample notifications, then CountersResult.
	MethodCounters = "counters"
	// NotifyCounterSample carries one interval's counters: CounterSample.
	NotifyCounterSample = "counters.sample"
)

// Counter kinds and the reasons a counters call ends.
const (
	// KindGauge is a counter whose value is a level (a mean over the
	// interval): CPU, memory, queue lengths.
	KindGauge = "gauge"
	// KindSum is a counter whose value is how much it grew in the interval:
	// GC counts, exceptions, allocated bytes.
	KindSum = "sum"
	// EndDuration: the collection ran its full duration.
	EndDuration = "duration"
	// EndExited: the process exited before the duration was over.
	EndExited = "exited"
)

// ProcessesResult is the processes method's result.
type ProcessesResult struct {
	Processes []HelperProcess `json:"processes"`
}

// HelperProcess is a process with a .NET diagnostics endpoint.
type HelperProcess struct {
	PID int `json:"pid"`
}

// CountersParams are the counters method's params.
type CountersParams struct {
	PID         int `json:"pid"`
	IntervalSec int `json:"intervalSec"`
	// DurationMs is how long to collect; 0 until the helper's stdin
	// closes.
	DurationMs int64 `json:"durationMs"`
}

// CounterSample is one interval's counters.
type CounterSample struct {
	// ElapsedMs is when the interval's counters arrived, since the
	// collection started.
	ElapsedMs int64          `json:"elapsedMs"`
	Counters  []CounterValue `json:"counters"`
}

// CounterValue is one counter's value in a sample.
type CounterValue struct {
	Provider    string  `json:"provider"`
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	Unit        string  `json:"unit"`
	Kind        string  `json:"kind"`
	Value       float64 `json:"value"`
}

// CountersResult is the counters method's result.
type CountersResult struct {
	Samples   int    `json:"samples"`
	EndReason string `json:"endReason"`
}

const goosWindows = "windows"

// hostFrameworkMissing is the dotnet host's exit code when no runtime
// satisfies the app (FrameworkMissingFailure); Unix keeps its low byte.
const hostFrameworkMissing = 0x80008096

// HelperSpec finds the .NET helper and says how to run it: $EYEDBG_DOTNET_HELPER,
// else helpers/dotnet next to eyedbg (or next to the file a symlinked
// eyedbg points to). A .dll runs with the dotnet host (FindHost).
func HelperSpec() (helper.Spec, error) {
	exe, err := os.Executable()
	if err != nil {
		exe = "" // only the environment override can help then
	}

	path, direct, err := helperPath(os.Getenv(EnvHelper), exe, filepath.EvalSymlinks, os.Stat, runtime.GOOS)
	if err != nil {
		return helper.Spec{}, err
	}

	spec := helper.Spec{Name: helperName, Path: path, Protocol: HelperProtocol, RuntimeMissing: runtimeMissing(runtime.GOOS)}
	if direct {
		return spec, nil
	}

	host, err := FindHost()
	if err != nil {
		return helper.Spec{}, api.NewError(api.CodeHelperNotFound, "no dotnet host to run "+helperName,
			"install .NET 8 or later (runtime or SDK), or put dotnet on PATH / set DOTNET_ROOT")
	}

	spec.Path, spec.Args = host, []string{path}

	return spec, nil
}

// helperPath returns the helper's path and whether it runs directly (an
// executable) rather than as a .dll under the dotnet host. env is
// $EYEDBG_DOTNET_HELPER and exe eyedbg's own path ("" if unknown). It never
// looks in the working directory, a project, or PATH.
func helperPath(
	env, exe string, evalSymlinks func(string) (string, error), stat func(string) (fs.FileInfo, error), goos string,
) (path string, direct bool, err error) {
	if env != "" {
		return envHelperPath(env, stat, goos)
	}

	var dirs []string

	if exe != "" {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "helpers", "dotnet"))

		if resolved, err := evalSymlinks(exe); err == nil && filepath.Dir(resolved) != filepath.Dir(exe) {
			dirs = append(dirs, filepath.Join(filepath.Dir(resolved), "helpers", "dotnet"))
		}
	}

	for _, dir := range dirs {
		p := filepath.Join(dir, HelperDLL)
		if fi, err := stat(p); err == nil && !fi.IsDir() {
			return p, false, nil
		}
	}

	looked := "nowhere (eyedbg's own path is unknown)"
	if len(dirs) > 0 {
		looked = strings.Join(dirs, " and ")
	}

	return "", false, api.NewError(api.CodeHelperNotFound,
		fmt.Sprintf("%s (%s) isn't installed: looked in %s", helperName, HelperDLL, looked),
		"keep the helpers/ directory next to eyedbg, as in the release archive; from source, 'task build' builds it "+
			"(needs the .NET SDK); or set "+EnvHelper+" to its absolute path")
}

// envHelperPath checks $EYEDBG_DOTNET_HELPER: absolute, an existing file,
// and on Windows a .dll or .exe (never a script cmd.exe would run).
func envHelperPath(env string, stat func(string) (fs.FileInfo, error), goos string) (path string, direct bool, err error) {
	fail := func(problem string) (string, bool, error) {
		return "", false, api.NewError(api.CodeHelperNotFound, "$"+EnvHelper+" "+problem,
			"set it to the absolute path of "+HelperDLL+", or unset it to use the helper next to eyedbg")
	}

	if !filepath.IsAbs(env) {
		return fail(fmt.Sprintf("must be an absolute path, not %q", env))
	}

	fi, err := stat(env)
	if err != nil || fi.IsDir() {
		return fail(fmt.Sprintf("names %s, which isn't a file", env))
	}

	ext := strings.ToLower(filepath.Ext(env))

	switch {
	case ext == ".dll":
		return env, false, nil
	case goos == goosWindows && ext != ".exe":
		return fail(fmt.Sprintf("names %s: on Windows it must be a .dll or .exe", env))
	default:
		return env, true, nil
	}
}

// runtimeMissing maps the host's "framework missing" exit to
// HELPER_NOT_FOUND.
func runtimeMissing(goos string) func(exitCode int) *api.Error {
	return func(code int) *api.Error {
		if int64(code) == hostFrameworkMissing || (goos != goosWindows && code == hostFrameworkMissing&0xff) {
			return api.NewError(api.CodeHelperNotFound, "no .NET 8 or later runtime to run "+helperName,
				"install the .NET 8 (or later) runtime or SDK")
		}

		return nil
	}
}
