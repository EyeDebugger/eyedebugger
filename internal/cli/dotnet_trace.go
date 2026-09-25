// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/helper"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Bounds and defaults of 'eyedbg dotnet trace' (docs/adr/0016, P2-M8).
const (
	defaultTraceDuration = 10 * time.Second
	minTraceDuration     = time.Second
	maxTraceDuration     = 5 * time.Minute
	defaultTraceTop      = 10
	// traceSlack is added to --duration for the default --timeout: the
	// helper's starts, the stop (the rundown) and the summary.
	traceSlack              = time.Minute
	defaultTraceFileTimeout = 5 * time.Minute
	// fileScratchType names the scratch file of a FILE's summary.
	fileScratchType = "file"
)

const traceExitHelp = `

Exit codes: 0 success; 1 INVALID_REQUEST (bad flags, no such process or file, another user's
process, --out exists); 2 NO_SESSION, NOT_RUNNING (the session's program is stopped at a
breakpoint, or hasn't started), SESSION_EXITED, NOT_DOTNET, DIAGNOSTICS_DISABLED,
DIAGNOSTICS_TIMEOUT (the trace didn't start or didn't end in time — the program is paused — or the
summary ran out of time), TRACE_UNSUPPORTED (a file that can't be read: not a .nettrace, cut off,
or no permission; a readable trace with nothing in it is a success that says so); 3 HELPER_NOT_FOUND, HELPER_MISMATCH (a helper older than eyedbg); 4
HELPER_FAILED (the helper crashed or timed out, or writing the trace failed).`

const traceSharedHelp = `

Runs the .NET side helper (helpers/dotnet next to eyedbg, or $EYEDBG_DOTNET_HELPER) with the
dotnet host: one process traces, another summarizes; only your own processes, and the same caveat
about shared machines as 'eyedbg help dotnet'. Not a debug-session action: no control lease,
nothing in the session's events. Method names come from the trace itself: no symbol server,
nothing downloaded, no file the trace names is opened.`

const traceLong = `Record a short trace of a running .NET process and summarize it: why is it slow or busy?
--profile cpu (default) samples every managed thread's stack for --duration (default 10s, 1s to
5m) and lists the hottest methods: by samples at the top of the stack (exclusive: where the time
goes) and anywhere in it (inclusive: which callers lead there); then where the other threads were
— waiting (a lock, I/O, a sleep) or in native or runtime code — and a line on garbage collections.
Samples are of every managed thread, not CPU time: a waiting thread is sampled too, which is why
waits are listed apart. --profile gc records garbage collections (per generation, pauses) and
allocation ticks (one about every 100 KB allocated): which types are allocated most. --top N
(default 10) rows per list. Output shows method, module and type names and counts, never values.

The trace is a .nettrace file in eyedbg's private traces directory, named in the output: open it in
PerfView or with 'dotnet-trace convert'. Besides names and GC data, it holds what the runtime
records about the process: its command line and module paths. So it is kept where only you can read
it (<home>/traces, <home> being $EYEDBG_HOME or ~/.eyedbg; files 0600) and removed after 7 days,
keeping at most the newest 10 (files eyedbg didn't name are left alone). --out PATH puts it there
instead (a new file: eyedbg never overwrites, and doesn't create directories). 'eyedbg dotnet trace
FILE' summarizes an existing .nettrace (eyedbg's, or dotnet-trace's) without tracing anything.

Target: FILE, else a process — --pid N (see 'eyedbg dotnet ps'), else the session (-s ID,
$EYEDBG_SESSION, else the only session). The program must be running: one stopped at a breakpoint
can't start a trace (NOT_RUNNING at once: continue it first), and one that hits a breakpoint or is
suspended mid-trace can't end it: 30s after --duration that fails with DIAGNOSTICS_TIMEOUT and the
trace is discarded (don't trace across a breakpoint). A program that exits during the trace ends it
early; the output says so.

Effect on the program: the cpu profile pauses it about a thousand times a second to walk its
stacks, so expect it to run slower while traced; gc adds an event per about 100 KB allocated; the
runtime keeps a buffer of up to 256 MB in the process, and names every loaded method when the trace
stops. A trace ends early at 512 MiB (the output says so).

Blocks for --duration plus the helper's starts, the stop and the summary (a second or two for a 10s
trace); --timeout (default --duration + 1m, at most 1h; for a FILE 5m) bounds the whole command.
Interrupting it (Ctrl-C) discards the trace. --json: {"schema": 1, "target" (for a process),
"trace": {path, private, taken, bytes}, "profile", "endReason" ("duration", "exited" or "size"),
"durationMs", "eventsLost", "cpu": {samples, managed, other, threads, unresolvedFrames, exclusive:
[{method, module, samples}], exclusiveOmitted, inclusive, inclusiveOmitted, waiting,
waitingOmitted}, "gc": {collections, gen0, gen1, gen2, background, induced, pauseTotalMs,
pauseMaxMs}, "allocations": {ticks, bytes, typeCount, types: [{name, ticks, bytes}],
typesOmitted}}; cpu, gc and allocations only when the trace has them, profile and endReason only
for a process. A trace with none of them (a quiet process traced for gc) is still a success: the
text says what is missing in one line.`

// traceOptions are 'eyedbg dotnet trace' flags.
type traceOptions struct {
	file     string
	pid      int
	profile  string
	duration time.Duration
	top      int
	out      string
	timeout  time.Duration

	sessionSet, profileSet, durationSet, timeoutSet bool
}

// tracePlan is what a trace command will do.
type tracePlan struct {
	file     string // absolute; "" for a process
	pid      int
	profile  string
	duration time.Duration
	top      int
	out      string // absolute, or ""
	timeout  time.Duration
}

func badTrace(format string, a ...any) error {
	return api.NewError(api.CodeInvalidRequest, fmt.Sprintf(format, a...), "see 'eyedbg help dotnet trace'")
}

// plan validates the flags: one target at most (FILE, --pid, -s);
// --profile, --duration and --out only for a process; --duration 1s–5m;
// --top 1–1000; --timeout longer than --duration (default --duration +
// 1m) for a process, else positive (default 5m), at most 1h either way.
func (o traceOptions) plan() (tracePlan, error) {
	if err := o.checkTarget(); err != nil {
		return tracePlan{}, err
	}

	if o.top < 1 || o.top > maxTop {
		return tracePlan{}, badTrace("--top must be 1 to %d, not %d", maxTop, o.top)
	}

	if o.file != "" {
		return o.planFile()
	}

	return o.planProcess()
}

// planFile validates the flags for a FILE.
func (o traceOptions) planFile() (tracePlan, error) {
	if o.profileSet || o.durationSet || o.out != "" {
		return tracePlan{}, badTrace("--profile, --duration and --out are for a process (--pid, -s); a FILE is summarized as it is")
	}

	file, err := fileArg(o.file, "trace", "record one with 'eyedbg dotnet trace --pid N'")
	if err != nil {
		return tracePlan{}, err
	}

	p := tracePlan{file: file, top: o.top, timeout: defaultTraceFileTimeout}

	if o.timeoutSet {
		if o.timeout <= 0 || o.timeout > maxDumpTimeout {
			return tracePlan{}, badTrace("--timeout must be more than 0 and at most %s, not %s", maxDumpTimeout, o.timeout)
		}

		p.timeout = o.timeout
	}

	return p, nil
}

// planProcess validates the flags for a process.
func (o traceOptions) planProcess() (tracePlan, error) {
	if o.profile != dotnet.ProfileCPU && o.profile != dotnet.ProfileGC {
		return tracePlan{}, badTrace("--profile must be cpu or gc, not %q", o.profile)
	}

	if o.duration < minTraceDuration || o.duration > maxTraceDuration {
		return tracePlan{}, badTrace("--duration must be from %s to %s, not %s", minTraceDuration, maxTraceDuration, o.duration)
	}

	p := tracePlan{pid: o.pid, profile: o.profile, duration: o.duration, top: o.top, timeout: o.duration + traceSlack}

	if o.timeoutSet {
		if o.timeout <= o.duration || o.timeout > maxDumpTimeout {
			return tracePlan{}, badTrace("--timeout must be longer than --duration (%s) and at most %s, not %s", o.duration, maxDumpTimeout, o.timeout)
		}

		p.timeout = o.timeout
	}

	if o.out != "" {
		out, err := checkOut(o.out)
		if err != nil {
			return tracePlan{}, err
		}

		p.out = out
	}

	return p, nil
}

func (o traceOptions) checkTarget() error {
	switch {
	case o.pid < 0:
		return badTrace("--pid must be positive")
	case o.pid > 0 && o.sessionSet:
		return badTrace("--pid and -s/--session both name a target; pass one")
	case o.file != "" && (o.pid > 0 || o.sessionSet):
		return badTrace("a FILE and --pid or -s/--session both name a target; pass one")
	default:
		return nil
	}
}

func newDotnetTraceCommand(info version.Info, g *globals) *cobra.Command {
	var o traceOptions

	cmd := &cobra.Command{
		Use:   "trace [FILE]",
		Short: "Trace a .NET process for a few seconds: its hottest methods, waits, GCs and allocations",
		Long:  traceLong + traceSharedHelp + traceExitHelp,
		Example: `  eyedbg dotnet trace                                  # the only session's program, 10s of CPU samples
  eyedbg dotnet trace --pid 4321 --duration 30s --top 20
  eyedbg dotnet trace --pid 4321 --profile gc
  eyedbg dotnet trace ~/.eyedbg/traces/4321-20260925T150300Z-cpu-1f3a9c0e.nettrace --top 30`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.file = args[0]
			}

			f := cmd.Flags()
			o.sessionSet = g.session != ""
			o.profileSet, o.durationSet, o.timeoutSet = f.Changed("profile"), f.Changed("duration"), f.Changed("timeout")

			p, err := o.plan()
			if err != nil {
				return err
			}

			if p.file != "" {
				return runTraceFile(cmd, g, p)
			}

			return runTrace(cmd, info, g, p)
		},
	}

	f := cmd.Flags()
	f.IntVar(&o.pid, "pid", 0, "the process to trace (see 'eyedbg dotnet ps'); default: the session's program")
	f.StringVar(&o.profile, "profile", dotnet.ProfileCPU, "what to record: cpu (stack samples) or gc (collections and allocations)")
	f.DurationVar(&o.duration, "duration", defaultTraceDuration, "how long to trace, 1s to 5m, e.g. 30s")
	f.IntVar(&o.top, "top", defaultTraceTop, "rows per list, most first (1-1000)")
	f.StringVar(&o.out, "out", "", "write the trace to this new file instead of eyedbg's private directory")
	f.DurationVar(&o.timeout, "timeout", 0, "bound on the whole command, at most 1h (default: --duration + 1m; for a FILE 5m)")

	return cmd
}

// traceRef is the "trace" member of the --json output.
type traceRef struct {
	Path string `json:"path"`
	// Private: in eyedbg's traces directory (recorded by eyedbg).
	Private bool `json:"private"`
	// Taken: this command recorded it.
	Taken bool  `json:"taken"`
	Bytes int64 `json:"bytes"`
}

// traceOutput is the --json output of 'eyedbg dotnet trace'.
type traceOutput struct {
	Schema    int           `json:"schema"`
	Target    *dotnetTarget `json:"target,omitempty"`
	Trace     traceRef      `json:"trace"`
	Profile   string        `json:"profile,omitempty"`
	EndReason string        `json:"endReason,omitempty"`

	dotnet.TraceSummaryResult //nolint:embeddedstructfieldcheck // JSON order: schema, target, trace, then the helper's result.
}

func runTrace(cmd *cobra.Command, info version.Info, g *globals, p tracePlan) error {
	target, err := resolveTarget(cmd, info, g, p.pid, false)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), p.timeout)
	defer cancel()

	dir, err := privateTraces()
	if err != nil {
		return err
	}

	path, res, err := takeTrace(ctx, dir, target, p)
	if err != nil {
		return err
	}

	scratch, err := artifacts.ScratchName(dir, target.PID, p.profile, time.Now(), rand.Reader)
	if err != nil {
		return keptTrace(err, path)
	}

	sum, err := summarizeTrace(ctx, p.timeout, path, filepath.Join(dir, scratch), p.top)

	switch {
	case err != nil && cmd.Context().Err() != nil:
		removeStray(path) // interrupted: the trace is discarded, as while recording

		return err
	case err != nil:
		return keptTrace(err, path)
	}

	out := traceOutput{
		Schema: jsonSchemaVersion, Target: &target, Trace: traceRef{Path: path, Private: true, Taken: true, Bytes: res.Bytes},
		Profile: p.profile, EndReason: res.EndReason, TraceSummaryResult: sum,
	}

	// After the summary: the helper reads the private file, never the
	// user's location.
	if p.out != "" {
		if err := place("trace", path, p.out); err != nil {
			return err
		}

		out.Trace.Path, out.Trace.Private = p.out, false
	}

	return writeTrace(cmd.OutOrStdout(), out, g.json)
}

// runTraceFile summarizes a .nettrace file.
func runTraceFile(cmd *cobra.Command, g *globals, p tracePlan) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), p.timeout)
	defer cancel()

	resolved, err := filepath.EvalSymlinks(p.file)
	if err != nil {
		return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("trace %s: %v", p.file, err), "")
	}

	dir, err := privateTraces()
	if err != nil {
		return err
	}

	artifacts.Prune(dir, artifacts.Traces, time.Now(), artifacts.MaxAge, artifacts.Keep, resolved)

	// One resolution: the file checked is the file the helper reads (for
	// eyedbg's own trace, the private file itself).
	input, private := resolved, false
	if own, ok := artifacts.Own(dir, artifacts.Traces, resolved); ok {
		input, private = own, true
	}

	info, err := os.Stat(input)
	if err != nil {
		return api.NewError(api.CodeInvalidRequest, "no trace file at "+p.file, "")
	}

	scratch, err := artifacts.ScratchName(dir, 0, fileScratchType, time.Now(), rand.Reader)
	if err != nil {
		return err
	}

	sum, err := summarizeTrace(ctx, p.timeout, input, filepath.Join(dir, scratch), p.top)
	if err != nil {
		return err
	}

	out := traceOutput{
		Schema: jsonSchemaVersion, Trace: traceRef{Path: p.file, Private: private, Bytes: info.Size()}, TraceSummaryResult: sum,
	}

	return writeTrace(cmd.OutOrStdout(), out, g.json)
}

// privateTraces prepares eyedbg's traces directory.
func privateTraces() (string, error) {
	dir, err := artifacts.Dir(artifacts.Traces)
	if err != nil {
		return "", api.NewError(api.CodeInvalidRequest, "eyedbg's traces directory: "+err.Error(),
			"fix it or set $"+adapters.EnvHome+" to a directory of yours")
	}

	return dir, nil
}

// takeTrace records a trace of target under a fresh name in dir, checks it
// arrived (a regular file, not empty, 0600), then prunes dir with it
// protected. On failure or interruption no trace of this call is left.
func takeTrace(ctx context.Context, dir string, target dotnetTarget, p tracePlan) (string, dotnet.TraceResult, error) {
	name, err := artifacts.Name(dir, artifacts.Traces, target.PID, p.profile, time.Now(), rand.Reader)
	if err != nil {
		return "", dotnet.TraceResult{}, err
	}

	path := filepath.Join(dir, name)
	params := dotnet.TraceParams{PID: target.PID, Profile: p.profile, DurationMs: p.duration.Milliseconds(), Path: path}

	var res dotnet.TraceResult

	err = withHelper(ctx, p.timeout, func(h *helper.Helper) error {
		return mismatch(dotnet.MethodTrace, h.Call(ctx, dotnet.MethodTrace, params, &res))
	})
	if err == nil {
		err = checkTrace(path)
	}

	if err != nil {
		// The helper has exited (withHelper waits): nothing holds the file.
		removeStray(path)

		return "", dotnet.TraceResult{}, err
	}

	// After the trace, so that it counts: at most artifacts.Keep remain.
	artifacts.Prune(dir, artifacts.Traces, time.Now(), artifacts.MaxAge, artifacts.Keep, path)

	return path, res, nil
}

// checkTrace checks the helper's trace is at path: a regular file (not a
// symlink), not empty; on Unix it is made 0600 again.
func checkTrace(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return api.NewError(api.CodeHelperFailed, "the .NET helper reported a trace but none is at "+path, "")
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return api.NewError(api.CodeHelperFailed, "restrict "+path+" to you: "+err.Error(), "")
		}
	}

	return nil
}

// summarizeTrace has the helper summarize the trace at path, with its
// scratch file at scratch; the scratch file (and TraceLog's .new sibling)
// is removed afterwards whatever happened.
func summarizeTrace(ctx context.Context, timeout time.Duration, path, scratch string, top int) (dotnet.TraceSummaryResult, error) {
	deadline, _ := ctx.Deadline()
	params := dotnet.TraceSummaryParams{Path: path, Scratch: scratch, Top: top, TimeoutMs: helperTimeoutMs(deadline, time.Now())}

	var res dotnet.TraceSummaryResult

	err := withHelper(ctx, timeout, func(h *helper.Helper) error {
		return mismatch(dotnet.MethodTraceSummary, h.Call(ctx, dotnet.MethodTraceSummary, params, &res))
	})

	removeStray(scratch)
	removeStray(scratch + ".new")

	return res, err
}

// keptTrace adds to a summary's error that the recorded trace is kept, and
// where.
func keptTrace(err error, path string) error {
	kept := "the trace is kept at " + path + " (summarize it again: eyedbg dotnet trace " + shellArg(path) + ")"

	if e, ok := errors.AsType[*api.Error](err); ok {
		hint := kept
		if e.Hint != "" {
			hint = e.Hint + "; " + kept
		}

		return api.NewError(e.Code, e.Message, hint)
	}

	return fmt.Errorf("%w; %s", err, kept)
}
