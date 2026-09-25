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
	// MethodDump makes a process's runtime write a dump of it: DumpParams,
	// then DumpResult.
	MethodDump = "dump"
	// MethodHeap analyzes a dump's managed heap: HeapParams, then
	// HeapResult.
	MethodHeap = "heap"
	// MethodThreads reads a dump's managed threads and locks:
	// ThreadsParams, then ThreadsResult.
	MethodThreads = "threads"
	// MethodTrace records an EventPipe trace of a process into a file:
	// TraceParams, then TraceResult.
	MethodTrace = "trace"
	// MethodTraceSummary summarizes a .nettrace file: TraceSummaryParams,
	// then TraceSummaryResult.
	MethodTraceSummary = "traceSummary"
)

// Trace profiles (TraceParams.Profile).
const (
	// ProfileCPU samples every managed thread's stack about a thousand
	// times a second, with GC start and end events.
	ProfileCPU = "cpu"
	// ProfileGC records garbage collections and allocation ticks (an event
	// per about 100 KB allocated).
	ProfileGC = "gc"
)

// Dump types (DumpParams.Type).
const (
	// DumpHeap has the managed heap: what heap and threads' locks need.
	DumpHeap = "heap"
	// DumpMini has threads and stacks, no heap.
	DumpMini = "mini"
	// DumpTriage is a mini dump with less memory (no heap).
	DumpTriage = "triage"
	// DumpFull has all of the process's memory.
	DumpFull = "full"
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
	// EndSize: the trace reached its size limit before the duration was
	// over.
	EndSize = "size"
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

// DumpParams are the dump method's params.
type DumpParams struct {
	PID  int    `json:"pid"`
	Type string `json:"type"`
	// Path is where the runtime writes the dump: absolute, nothing there.
	Path      string `json:"path"`
	TimeoutMs int64  `json:"timeoutMs"`
}

// DumpResult is the dump method's result.
type DumpResult struct {
	Bytes     int64 `json:"bytes"`
	ElapsedMs int64 `json:"elapsedMs"`
}

// HeapParams are the heap method's params.
type HeapParams struct {
	Path string `json:"path"`
	// TrustRecordedRuntime lets the dump's recorded runtime directory
	// supply the analysis library (DAC): only for eyedbg's own dumps.
	TrustRecordedRuntime bool `json:"trustRecordedRuntime"`
	Top                  int  `json:"top"`
	// TypeFilter keeps types whose full name contains it (case-sensitive).
	TypeFilter string        `json:"typeFilter,omitempty"`
	Gcroot     *GcrootParams `json:"gcroot,omitempty"`
	// Paths is how many root paths --gcroot finds.
	Paths     int   `json:"paths"`
	TimeoutMs int64 `json:"timeoutMs"`
}

// GcrootParams says what --gcroot asks about: a type (exact full name,
// else a unique substring) or an object's address ("0x1a2b").
type GcrootParams struct {
	Type    string `json:"type,omitempty"`
	Address string `json:"address,omitempty"`
}

// DumpRuntime is the .NET runtime a dump was taken with.
type DumpRuntime struct {
	Version string `json:"version"`
	// GC is "workstation" or "server".
	GC       string `json:"gc"`
	Heaps    int    `json:"heaps"`
	Platform string `json:"platform"`
	Arch     string `json:"arch"`
}

// HeapResult is the heap method's result.
type HeapResult struct {
	Runtime      DumpRuntime      `json:"runtime"`
	Objects      int64            `json:"objects"`
	Bytes        int64            `json:"bytes"`
	Generations  []GenerationStat `json:"generations"`
	TypeCount    int              `json:"typeCount"`
	Types        []TypeStat       `json:"types"`
	TypesOmitted int              `json:"typesOmitted"`
	Gcroot       *GcrootResult    `json:"gcroot,omitempty"`
}

// GenerationStat is one generation's objects and bytes: gen0, gen1, gen2,
// loh, poh, frozen, and unknown when an object's generation couldn't be
// told.
type GenerationStat struct {
	Name    string `json:"name"`
	Objects int64  `json:"objects"`
	Bytes   int64  `json:"bytes"`
}

// TypeStat is one type's objects and bytes.
type TypeStat struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
	Bytes int64  `json:"bytes"`
}

// GcrootResult says why the target is alive.
type GcrootResult struct {
	Target GcrootTarget `json:"target"`
	Paths  []RootPath   `json:"paths"`
	// PathsFound counts the paths found before the search stopped (at
	// Paths, the result's size limit, or the timeout).
	PathsFound int `json:"pathsFound"`
	// Complete says every root was searched: no more paths exist.
	Complete bool `json:"complete"`
}

// GcrootTarget is what --gcroot resolved to: a type and its instances, or
// an object.
type GcrootTarget struct {
	Type      string `json:"type,omitempty"`
	Address   string `json:"address,omitempty"`
	Instances *int64 `json:"instances,omitempty"`
}

// RootPath is a chain of references from a root to the target.
type RootPath struct {
	Root  RootLabel   `json:"root"`
	Chain []ChainLink `json:"chain"`
	// ChainOmitted counts links left out of a long chain's middle.
	ChainOmitted int `json:"chainOmitted,omitempty"`
}

// RootLabel is a GC root: Kind is static (Name: Type.field), stack
// (Thread, Frame), strong, pinned, async-pinned, ref-counted, sized-ref,
// finalizer, thread-static or other.
type RootLabel struct {
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	Thread *int   `json:"thread,omitempty"`
	Frame  string `json:"frame,omitempty"`
}

// ChainLink is one object on a root path.
type ChainLink struct {
	Address string `json:"address"`
	Type    string `json:"type"`
}

// ThreadsParams are the threads method's params.
type ThreadsParams struct {
	Path                 string `json:"path"`
	TrustRecordedRuntime bool   `json:"trustRecordedRuntime"`
	// Frames is how many frames per thread, from the top.
	Frames    int   `json:"frames"`
	TimeoutMs int64 `json:"timeoutMs"`
}

// ThreadsResult is the threads method's result.
type ThreadsResult struct {
	Runtime        DumpRuntime  `json:"runtime"`
	Threads        []DumpThread `json:"threads"`
	ThreadsOmitted int          `json:"threadsOmitted"`
	Locks          []DumpLock   `json:"locks"`
	// LocksAvailable is false for a dump without a heap (mini, triage).
	LocksAvailable bool `json:"locksAvailable"`
}

// DumpThread is a managed thread in a dump.
type DumpThread struct {
	ManagedID  int    `json:"managedId"`
	OSID       uint32 `json:"osId"`
	Alive      bool   `json:"alive"`
	Background bool   `json:"background"`
	Threadpool bool   `json:"threadpool"`
	Finalizer  bool   `json:"finalizer"`
	GC         bool   `json:"gc"`
	// Exception is the type name of the thread's current exception.
	Exception     string      `json:"exception,omitempty"`
	Frames        []DumpFrame `json:"frames"`
	FramesOmitted int         `json:"framesOmitted"`
}

// DumpFrame is a stack frame: Kind "managed" (Method, Module, ILOffset,
// File, Line) or "runtime" (Method is the runtime frame's name).
type DumpFrame struct {
	Kind     string `json:"kind"`
	Method   string `json:"method"`
	Module   string `json:"module,omitempty"`
	ILOffset *int   `json:"ilOffset,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
}

// DumpLock is a monitor that is held or waited on: the object, its type,
// the owner's managed thread id and how many threads wait.
type DumpLock struct {
	Object  string `json:"object"`
	Type    string `json:"type"`
	Owner   *int   `json:"owner,omitempty"`
	Waiting int    `json:"waiting"`
}

// TraceParams are the trace method's params.
type TraceParams struct {
	PID        int    `json:"pid"`
	Profile    string `json:"profile"`
	DurationMs int64  `json:"durationMs"`
	// Path is where the trace is written: absolute, nothing there.
	Path string `json:"path"`
}

// TraceResult is the trace method's result.
type TraceResult struct {
	Bytes     int64  `json:"bytes"`
	ElapsedMs int64  `json:"elapsedMs"`
	EndReason string `json:"endReason"`
}

// TraceSummaryParams are the traceSummary method's params.
type TraceSummaryParams struct {
	// Path is the .nettrace file: absolute.
	Path string `json:"path"`
	// Scratch is where the summary's temporary file goes: absolute,
	// nothing there nor at Scratch + ".new".
	Scratch   string `json:"scratch"`
	Top       int    `json:"top"`
	TimeoutMs int64  `json:"timeoutMs"`
}

// TraceSummaryResult is the traceSummary method's result: CPU is there
// when the trace has samples, GC with a collection, Allocations with an
// allocation tick.
type TraceSummaryResult struct {
	DurationMs  int64              `json:"durationMs"`
	EventsLost  int64              `json:"eventsLost"`
	CPU         *CPUSummary        `json:"cpu,omitempty"`
	GC          *GCSummary         `json:"gc,omitempty"`
	Allocations *AllocationSummary `json:"allocations,omitempty"`
}

// CPUSummary is a trace's CPU samples: all of them, those in managed code,
// the others (waiting, or in native or runtime code); methods by samples
// at the top of the stack (Exclusive), anywhere in it (Inclusive), and
// where the others left managed code (Waiting).
type CPUSummary struct {
	Samples          int64           `json:"samples"`
	Managed          int64           `json:"managed"`
	Other            int64           `json:"other"`
	Threads          int             `json:"threads"`
	UnresolvedFrames int64           `json:"unresolvedFrames"`
	Exclusive        []MethodSamples `json:"exclusive"`
	ExclusiveOmitted int             `json:"exclusiveOmitted"`
	Inclusive        []MethodSamples `json:"inclusive"`
	InclusiveOmitted int             `json:"inclusiveOmitted"`
	Waiting          []MethodSamples `json:"waiting"`
	WaitingOmitted   int             `json:"waitingOmitted"`
}

// MethodSamples is a method (its module without extension, when known)
// and its samples; Method "(unresolved)" stands for frames without a name.
type MethodSamples struct {
	Method  string `json:"method"`
	Module  string `json:"module,omitempty"`
	Samples int64  `json:"samples"`
}

// GCSummary is a trace's garbage collections.
type GCSummary struct {
	Collections  int     `json:"collections"`
	Gen0         int     `json:"gen0"`
	Gen1         int     `json:"gen1"`
	Gen2         int     `json:"gen2"`
	Background   int     `json:"background"`
	Induced      int     `json:"induced"`
	PauseTotalMs float64 `json:"pauseTotalMs"`
	PauseMaxMs   float64 `json:"pauseMaxMs"`
}

// AllocationSummary is a trace's allocation ticks (about one per 100 KB
// allocated) by type.
type AllocationSummary struct {
	Ticks        int64           `json:"ticks"`
	Bytes        int64           `json:"bytes"`
	TypeCount    int             `json:"typeCount"`
	Types        []AllocatedType `json:"types"`
	TypesOmitted int             `json:"typesOmitted"`
}

// AllocatedType is a type's allocation ticks and the bytes they account
// for.
type AllocatedType struct {
	Name  string `json:"name"`
	Ticks int64  `json:"ticks"`
	Bytes int64  `json:"bytes"`
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
