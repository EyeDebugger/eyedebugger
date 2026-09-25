// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/helper"
	"github.com/eyedebugger/eyedebugger/internal/proc"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Bounds and defaults of 'eyedbg dotnet counters' (docs/adr/0016).
const (
	defaultCountersDuration = 5 * time.Second
	defaultCountersInterval = time.Second
	minCountersInterval     = time.Second
	maxCountersInterval     = time.Minute
	maxCountersDuration     = time.Hour
	// maxWatchDuration is the helper's own bound on one collection.
	maxWatchDuration = 24 * time.Hour
	// countersSlack is added to --duration for the default --timeout.
	countersSlack    = 30 * time.Second
	defaultPsTimeout = 30 * time.Second
	counterProvider  = "System.Runtime"
)

// lookupFunc tells what the system knows about a process (proc.Lookup).
type lookupFunc func(ctx context.Context, pid int) (proc.Info, error)

// dotnetTarget is the process 'eyedbg dotnet' inspects.
type dotnetTarget struct {
	PID  int    `json:"pid"`
	Name string `json:"name,omitempty"`
	// Session is the session whose debuggee it is, if it was chosen that way.
	Session string `json:"session,omitempty"`
}

// describe is "pid N (name)[, session ID]", fit for a terminal.
func (t dotnetTarget) describe() string {
	s := fmt.Sprintf("pid %d", t.PID)
	if t.Name != "" {
		s += " (" + displayName(t.Name) + ")"
	}

	if t.Session != "" {
		s += ", session " + t.Session
	}

	return s
}

const dotnetHelp = `

Runs the .NET side helper (helpers/dotnet next to eyedbg, or $EYEDBG_DOTNET_HELPER) with the
dotnet host (.NET 8 or later), one process per command; it talks to the target's diagnostics
endpoint read-only. Only your own processes: another user's is refused. Not a debug-session
action: no control lease, nothing in the session's events; it works with or without a session.

On a shared machine that check covers the process, not its endpoint: the .NET diagnostics client
finds the endpoint by name (a socket in the program's TMPDIR on Linux, in eyedbg's on macOS; a pipe
on Windows), so another local user who can create that name can pose as it: false or missing data,
and on Windows possibly acting with your identity. There, give the program and eyedbg a TMPDIR
only you can write (macOS's default already is). On Windows a decoy dotnet-diagnostic-dsrouter-PID
pipe is refused; other decoys are not, so don't use it where other users aren't trusted.

Exit codes: 0 success; 1 INVALID_REQUEST (bad flags, no such process, another user's); 2
NO_SESSION, NOT_RUNNING, SESSION_EXITED, NOT_DOTNET (no .NET diagnostics endpoint, or a session of
another language), DIAGNOSTICS_DISABLED (a .NET process with its endpoint off:
DOTNET_EnableDiagnostics=0, or another TMPDIR), DIAGNOSTICS_TIMEOUT (the process is paused or
hung); 3 HELPER_NOT_FOUND (no helper or no .NET runtime to run it), HELPER_MISMATCH; 4
HELPER_FAILED (the helper crashed or timed out; its stderr is shown).`

func newDotnetCommand(info version.Info, g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dotnet",
		Short: "Inspect .NET processes without pausing them: list them, sample runtime counters",
		Long: `Inspect running .NET (Core 3.0 or later) processes without pausing or debugging them, through
the runtime's diagnostics endpoint (EventPipe): 'eyedbg dotnet ps' lists the processes you can
inspect, 'eyedbg dotnet counters' samples one's CPU, memory, GC, thread pool, exceptions and JIT
counters. Use them to see how a program behaves (is it allocating, collecting, throwing, starving
its thread pool?) while it runs — e.g. the debuggee of a running session, or any .NET process of
yours by pid.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.` + dotnetHelp,
		Example: `  eyedbg dotnet ps
  eyedbg dotnet counters                          # the only session's program, 5s
  eyedbg dotnet counters --pid 4321 --watch`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newDotnetPsCommand(info, g), newDotnetCountersCommand(info, g))

	return cmd
}

func newDotnetPsCommand(info version.Info, g *globals) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List your .NET processes that can be inspected",
		Long: `List your processes that publish a .NET diagnostics endpoint — the ones 'eyedbg dotnet counters
--pid' can inspect: pid, process name and, when the running daemon debugs it, the session (else
"-"), under a header line. Other users' processes are left out; so are .NET processes with
diagnostics off (DOTNET_EnableDiagnostics=0). Names come from the system (a program started as
'dotnet app.dll' is "dotnet"); command lines are never read.

Read-only; never starts the daemon. Returns within a second or two (the helper's start); --timeout
bounds it. Prints nothing when there are none (an empty "processes" array in --json).` + dotnetHelp,
		Example: `  eyedbg dotnet ps
  eyedbg dotnet ps --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rows, err := dotnetPs(cmd.Context(), info, timeout)
			if err != nil {
				return err
			}

			return writeDotnetPs(cmd.OutOrStdout(), rows, g.json)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", defaultPsTimeout, "how long to wait for the helper, e.g. 30s")

	return cmd
}

// psRow is a line of 'eyedbg dotnet ps'.
type psRow struct {
	PID     int    `json:"pid"`
	Name    string `json:"name"`
	Session string `json:"session,omitempty"`
}

func dotnetPs(ctx context.Context, info version.Info, timeout time.Duration) ([]psRow, error) {
	if timeout <= 0 {
		return nil, api.NewError(api.CodeInvalidRequest, "--timeout must be positive", "")
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var res dotnet.ProcessesResult
	if err := withHelper(ctx, timeout, func(h *helper.Helper) error {
		return h.Call(ctx, dotnet.MethodProcesses, struct{}{}, &res)
	}); err != nil {
		return nil, err
	}

	pids := make([]int, 0, len(res.Processes))
	for _, p := range res.Processes {
		pids = append(pids, p.PID)
	}

	return psRows(ctx, pids, proc.Lookup, liveSessions(ctx, info)), nil
}

// liveSessions lists the running daemon's sessions; none when there is no
// daemon or it can't be asked. It never starts one.
func liveSessions(ctx context.Context, info version.Info) []api.SessionInfo {
	ctx, cancel := context.WithTimeout(ctx, daemonCallTimeout)
	defer cancel()

	cl, err := dialRunning(ctx, info)
	if err != nil {
		return nil
	}
	defer cl.Close()

	var list []api.SessionInfo
	if err := cl.Call(ctx, api.MethodSessionList, nil, &list); err != nil {
		return nil
	}

	return list
}

// psRows keeps the pids of processes that still exist and are the
// caller's, named, sorted, each with the live session debugging it.
func psRows(ctx context.Context, pids []int, lookup lookupFunc, sessions []api.SessionInfo) []psRow {
	bySession := map[int]string{}

	for i := range sessions {
		if s := &sessions[i]; s.PID > 0 && s.State != api.StateExited && s.State != api.StateLost {
			bySession[s.PID] = s.ID
		}
	}

	rows := []psRow{}

	for _, pid := range slices.Sorted(slices.Values(pids)) {
		if len(rows) > 0 && rows[len(rows)-1].PID == pid {
			continue
		}

		p, err := lookup(ctx, pid)
		if err != nil || !p.SameUser {
			continue // gone, or another user's (Windows lists every user's pipes)
		}

		rows = append(rows, psRow{PID: pid, Name: p.Name, Session: bySession[pid]})
	}

	return rows
}

// countersOptions are 'eyedbg dotnet counters' flags.
type countersOptions struct {
	pid      int
	duration time.Duration
	interval time.Duration
	names    []string
	watch    bool
	timeout  time.Duration

	durationSet, timeoutSet, sessionSet bool
}

// countersPlan is what a counters command will do, from valid flags.
type countersPlan struct {
	params  dotnet.CountersParams
	names   []string
	watch   bool
	timeout time.Duration // 0: none
}

// plan validates the flags: --interval whole seconds in 1s–60s;
// --duration at least one interval, at most an hour (a day with --watch;
// with --watch and no --duration, until interrupted); --timeout longer than
// --duration (default --duration + 30s; none for an open-ended watch).
func (o countersOptions) plan() (countersPlan, error) {
	if err := o.checkTarget(); err != nil {
		return countersPlan{}, err
	}

	if o.interval < minCountersInterval || o.interval > maxCountersInterval || o.interval%time.Second != 0 {
		return countersPlan{}, badCounters("--interval must be whole seconds from 1s to 60s, not %s", o.interval)
	}

	duration, err := o.effectiveDuration()
	if err != nil {
		return countersPlan{}, err
	}

	timeout, err := o.effectiveTimeout(duration)
	if err != nil {
		return countersPlan{}, err
	}

	return countersPlan{
		params: dotnet.CountersParams{
			PID: o.pid, IntervalSec: int(o.interval / time.Second), DurationMs: duration.Milliseconds(),
		},
		names: o.names, watch: o.watch, timeout: timeout,
	}, nil
}

func badCounters(format string, a ...any) error {
	return api.NewError(api.CodeInvalidRequest, fmt.Sprintf(format, a...), "see 'eyedbg help dotnet counters'")
}

func (o countersOptions) checkTarget() error {
	switch {
	case o.pid < 0:
		return badCounters("--pid must be positive")
	case o.pid > 0 && o.sessionSet:
		return badCounters("--pid and -s/--session both name a target; pass one")
	default:
		return nil
	}
}

// effectiveDuration is --duration, 0 for an open-ended watch.
func (o countersOptions) effectiveDuration() (time.Duration, error) {
	if o.watch && !o.durationSet {
		return 0, nil
	}

	limit := maxCountersDuration
	if o.watch {
		limit = maxWatchDuration
	}

	if o.duration < o.interval || o.duration > limit {
		return 0, badCounters("--duration must be from one --interval (%s) to %s, not %s", o.interval, limit, o.duration)
	}

	return o.duration, nil
}

// effectiveTimeout is --timeout, by default duration + 30s (0, none, for
// an open-ended watch).
func (o countersOptions) effectiveTimeout(duration time.Duration) (time.Duration, error) {
	if !o.timeoutSet {
		if duration == 0 {
			return 0, nil
		}

		return duration + countersSlack, nil
	}

	if o.timeout <= duration {
		return 0, badCounters("--timeout must be longer than --duration (%s), not %s", duration, o.timeout)
	}

	return o.timeout, nil
}

func newDotnetCountersCommand(info version.Info, g *globals) *cobra.Command {
	var o countersOptions

	cmd := &cobra.Command{
		Use:   "counters",
		Short: "Sample a .NET process's runtime counters (CPU, memory, GC, thread pool) without pausing it",
		Long: `Sample the System.Runtime counters of a running .NET process for --duration (default 5s), one
sample every --interval (default 1s), and print a summary: per counter, gauges (cpu-usage %,
working-set MB, gc-heap-size MB, thread pool queue length, …) as their last value with the range
when it moved, sums (GC counts, exceptions, allocated bytes, jitted methods, …) as the total and
the rate per second. Use it to see whether a program is busy, allocating, collecting, throwing or
starving its thread pool, without stopping it. --counters NAME,... keeps only those counters (a
name no sample has fails with the available names). --watch prints each sample instead, one line
each ("+1s name=value…"; a sum's value is its growth in that interval), until --duration ends or
you interrupt it (Ctrl-C: exit 0); --json prints one JSON document per sample then. A line appears
about one interval after its sample is taken (the event parser holds a sample's values until the
next sample's arrive), and interrupting drops the newest sample.

Target: --pid N (see 'eyedbg dotnet ps'), else the session (-s ID, $EYEDBG_SESSION, else the only
session) — its program must be running: a program stopped at a breakpoint can't start a
diagnostics session, so that fails at once with NOT_RUNNING (continue it first), and --pid of a
paused process (someone else's debugger, suspended) fails after 5s with DIAGNOSTICS_TIMEOUT. A
process that exits during the collection ends it early; the summary says so.

Blocks for --duration plus the helper's start (about 0.1–1s); --timeout (default --duration +
30s) bounds the whole command. Read-only: an EventPipe session inside the process, negligible
overhead; the program keeps running. --json: {"schema": 1, "target": {pid, name, session},
"provider", "intervalSec", "durationMs", "samples", "missedIntervals", "endReason" ("duration" or
"exited"), "counters": [{name, displayName, unit, kind: "gauge", last, min, max} or {…, kind:
"sum", total, perSecond}]}; with --watch, per sample {"schema": 1, "elapsedMs", "counters":
[{name, unit, kind, value}]}.` + dotnetHelp,
		Example: `  eyedbg dotnet counters                               # the only session's program
  eyedbg dotnet counters --pid 4321 --duration 10s
  eyedbg dotnet counters -s s-k3f9 --counters cpu-usage,gc-heap-size,exception-count
  eyedbg dotnet counters --pid 4321 --watch --interval 2s --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.durationSet = cmd.Flags().Changed("duration")
			o.timeoutSet = cmd.Flags().Changed("timeout")
			o.sessionSet = g.session != ""

			p, err := o.plan()
			if err != nil {
				return err
			}

			return runCounters(cmd, info, g, p)
		},
	}

	f := cmd.Flags()
	f.IntVar(&o.pid, "pid", 0, "the process to sample (see 'eyedbg dotnet ps'); default: the session's program")
	f.DurationVar(&o.duration, "duration", defaultCountersDuration, "how long to sample, e.g. 10s (with --watch: until interrupted)")
	f.DurationVar(&o.interval, "interval", defaultCountersInterval, "time between samples, whole seconds from 1s to 60s")
	f.StringSliceVar(&o.names, "counters", nil, "only these counters, comma-separated names (default: all)")
	f.BoolVar(&o.watch, "watch", false, "print each sample (about one interval after it is taken)")
	f.DurationVar(&o.timeout, "timeout", 0, "bound on the whole command (default: --duration + 30s)")

	return cmd
}

func runCounters(cmd *cobra.Command, info version.Info, g *globals, p countersPlan) error {
	ctx := cmd.Context()

	target, err := counterTarget(cmd, info, g, p.params.PID)
	if err != nil {
		return err
	}

	p.params.PID = target.PID
	out := cmd.OutOrStdout()
	c := &counterCollector{names: p.names}

	if p.watch {
		c.onSample = func(s dotnet.CounterSample) error { return writeCounterSample(out, s, g.json) }
	}

	callCtx, cancel := ctx, context.CancelFunc(func() {})
	if p.timeout > 0 {
		callCtx, cancel = context.WithTimeout(ctx, p.timeout)
	}
	defer cancel()

	var res dotnet.CountersResult

	err = withHelper(callCtx, p.timeout, func(h *helper.Helper) error {
		return h.Stream(callCtx, dotnet.MethodCounters, p.params, c.notify, &res)
	})

	switch {
	case p.watch && err != nil && ctx.Err() != nil:
		return nil // interrupted: a watch's normal end
	case err != nil:
		return err
	case p.watch:
		if res.EndReason == dotnet.EndExited && !g.json {
			return writeText(out, fmt.Sprintf("(%s exited)\n", target.describe()))
		}

		return nil
	}

	return writeCountersSummary(out, aggregate(target, p.params, c.samples, res), g.json)
}

// counterTarget resolves --pid, else the session, to a process.
func counterTarget(cmd *cobra.Command, info version.Info, g *globals, pid int) (dotnetTarget, error) {
	ctx := cmd.Context()
	if pid > 0 {
		return resolvePIDTarget(ctx, pid, proc.Lookup)
	}

	var snap api.Snapshot

	err := call(cmd, info, daemonCallTimeout, api.MethodSessionStatus, api.StatusParams{SessionRef: g.ref()}, &snap)
	if e, ok := errors.AsType[*api.Error](err); ok && e.Code == api.CodeNoSession {
		hint := e.Hint
		if hint != "" {
			hint += ", "
		}

		return dotnetTarget{}, api.NewError(e.Code, e.Message, hint+"or pass --pid N (see 'eyedbg dotnet ps')")
	}

	if err != nil {
		return dotnetTarget{}, err
	}

	return resolveSessionTarget(ctx, snap, proc.Lookup)
}

// resolvePIDTarget checks pid is a process of the caller's (D10).
func resolvePIDTarget(ctx context.Context, pid int, lookup lookupFunc) (dotnetTarget, error) {
	p, err := lookup(ctx, pid)

	switch {
	case errors.Is(err, proc.ErrNoProcess):
		return dotnetTarget{}, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("no process %d", pid), "'eyedbg dotnet ps' lists the ones you can inspect")
	case err != nil:
		return dotnetTarget{}, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("look up process %d: %v", pid, err), "")
	case !p.SameUser:
		owner := p.Owner
		if owner == "" {
			owner = "unknown"
		}

		return dotnetTarget{}, api.NewError(api.CodeInvalidRequest,
			fmt.Sprintf("process %d belongs to another user (%s): eyedbg inspects only your own processes", pid, owner), "")
	}

	return dotnetTarget{PID: pid, Name: p.Name}, nil
}

// resolveSessionTarget picks a session's debuggee: a running .NET program.
func resolveSessionTarget(ctx context.Context, snap api.Snapshot, lookup lookupFunc) (dotnetTarget, error) {
	s := snap.Session
	orPID := "or pass --pid N (see 'eyedbg dotnet ps')"

	switch {
	case s.Lang != dotnet.Language:
		return dotnetTarget{}, api.NewError(api.CodeNotDotnet, fmt.Sprintf("session %s debugs %s, not .NET", s.ID, s.Lang),
			"pick a dotnet session with -s ID, "+orPID)
	case s.State == api.StateExited || s.State == api.StateLost:
		return dotnetTarget{}, api.NewError(api.CodeSessionExited, fmt.Sprintf("session %s has ended (%s)", s.ID, s.State),
			"start it again, "+orPID)
	case s.State == api.StateStopped:
		reason := "stopped"
		if s.Stop != nil && s.Stop.Reason != "" {
			reason += " (" + s.Stop.Reason + ")"
		}

		return dotnetTarget{}, api.NewError(api.CodeNotRunning,
			fmt.Sprintf("session %s is %s: a stopped .NET runtime can't start a diagnostics session", s.ID, reason),
			"continue it ('eyedbg continue'), or read state with 'eyedbg vars'/'eyedbg stack'")
	case s.State != api.StateRunning || s.PID <= 0:
		return dotnetTarget{}, api.NewError(api.CodeNotRunning, fmt.Sprintf("session %s is still starting (no process yet)", s.ID),
			"wait for it ('eyedbg wait'), then try again")
	}

	t, err := resolvePIDTarget(ctx, s.PID, lookup)
	if err != nil {
		return dotnetTarget{}, err
	}

	t.Session = s.ID

	return t, nil
}

// withHelper starts the .NET helper, runs f with it and closes it; ctx
// bounds both. timeout (0: none) names the bound in the error when it
// expires.
func withHelper(ctx context.Context, timeout time.Duration, f func(*helper.Helper) error) error {
	spec, err := dotnet.HelperSpec()
	if err != nil {
		return err
	}

	h, err := helper.Start(ctx, spec)
	if err == nil {
		err = f(h)
		_ = h.Close() //nolint:contextcheck // Close is bounded by helper.WaitDelay and must run even when ctx is done.
	}

	if errors.Is(err, context.DeadlineExceeded) && timeout > 0 {
		return api.NewError(api.CodeHelperFailed, fmt.Sprintf("the .NET helper didn't finish within %s", timeout),
			"raise --timeout; if the target is paused or hung, it can't answer")
	}

	return err
}

// counterCollector gathers a counters call's samples, keeping only the
// requested counters, and hands each to onSample (--watch).
type counterCollector struct {
	names    []string
	onSample func(dotnet.CounterSample) error
	samples  []dotnet.CounterSample
}

func (c *counterCollector) notify(method string, params json.RawMessage) error {
	if method != dotnet.NotifyCounterSample {
		return api.NewError(api.CodeHelperFailed, fmt.Sprintf("the .NET helper sent an unknown notification %.100q", method), "")
	}

	var s dotnet.CounterSample
	if err := json.Unmarshal(params, &s); err != nil {
		return api.NewError(api.CodeHelperFailed, "the .NET helper sent a bad counters sample: "+err.Error(), "")
	}

	if len(c.samples) == 0 {
		if err := checkCounterNames(s, c.names); err != nil {
			return err
		}
	}

	s = filterCounters(s, c.names)
	c.samples = append(c.samples, s)

	if c.onSample != nil {
		return c.onSample(s)
	}

	return nil
}

// checkCounterNames fails on a --counters name the first sample lacks.
func checkCounterNames(s dotnet.CounterSample, names []string) error {
	have := map[string]bool{}
	for _, v := range s.Counters {
		have[v.Name] = true
	}

	var missing []string

	for _, n := range names {
		if !have[n] {
			missing = append(missing, n)
		}
	}

	if len(missing) == 0 {
		return nil
	}

	available := make([]string, 0, len(have))
	for _, v := range orderCounters(s.Counters) {
		available = append(available, printable(v.Name))
	}

	return api.NewError(api.CodeInvalidRequest, "no counter named "+strings.Join(missing, ", "),
		"available: "+strings.Join(available, ", "))
}

// filterCounters keeps the counters named (all when names is empty).
func filterCounters(s dotnet.CounterSample, names []string) dotnet.CounterSample {
	if len(names) == 0 {
		return s
	}

	kept := make([]dotnet.CounterValue, 0, len(names))

	for _, v := range s.Counters {
		if slices.Contains(names, v.Name) {
			kept = append(kept, v)
		}
	}

	s.Counters = kept

	return s
}

// counterOrder is the summary's order: CPU, memory, GC, allocation,
// exceptions, thread pool, locks, JIT, assemblies, timers; any other
// counter follows, by name.
var counterOrder = []string{ //nolint:gochecknoglobals // A read-only table.
	"cpu-usage",
	"working-set", "gc-heap-size", "gc-committed", "gc-fragmentation",
	"gen-0-gc-count", "gen-1-gc-count", "gen-2-gc-count", "time-in-gc", "total-pause-time-by-gc",
	"gen-0-gc-budget", "gen-0-size", "gen-1-size", "gen-2-size", "loh-size", "poh-size",
	"alloc-rate",
	"exception-count",
	"threadpool-thread-count", "threadpool-queue-length", "threadpool-completed-items-count",
	"monitor-lock-contention-count",
	"methods-jitted-count", "il-bytes-jitted", "time-in-jit",
	"assembly-count",
	"active-timer-count",
}

// orderCounters returns the values in counterOrder.
func orderCounters(values []dotnet.CounterValue) []dotnet.CounterValue {
	rank := func(name string) int {
		if i := slices.Index(counterOrder, name); i >= 0 {
			return i
		}

		return len(counterOrder)
	}

	out := slices.Clone(values)
	slices.SortStableFunc(out, func(a, b dotnet.CounterValue) int {
		if ra, rb := rank(a.Name), rank(b.Name); ra != rb {
			return ra - rb
		}

		return strings.Compare(a.Name, b.Name)
	})

	return out
}

// countersSummary is the --json output of 'eyedbg dotnet counters'.
type countersSummary struct {
	Schema          int           `json:"schema"`
	Target          dotnetTarget  `json:"target"`
	Provider        string        `json:"provider"`
	IntervalSec     int           `json:"intervalSec"`
	DurationMs      int64         `json:"durationMs"`
	Samples         int           `json:"samples"`
	MissedIntervals int           `json:"missedIntervals"`
	EndReason       string        `json:"endReason"`
	Counters        []counterStat `json:"counters"`
}

// counterStat summarizes one counter: a gauge's last, min and max, or a
// sum's total and rate per second.
type counterStat struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	Unit        string   `json:"unit"`
	Kind        string   `json:"kind"`
	Last        *float64 `json:"last,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Total       *float64 `json:"total,omitempty"`
	PerSecond   *float64 `json:"perSecond,omitempty"`

	seen int // samples that had it
}

// aggregate summarizes the samples (D7): per counter, a gauge's last, min,
// max; a sum's total and total per second over the intervals it was seen
// in. Missed intervals are those of the requested duration that brought
// no sample (the process was paused); none when it exited early.
func aggregate(target dotnetTarget, params dotnet.CountersParams, samples []dotnet.CounterSample, res dotnet.CountersResult) countersSummary {
	stats := map[string]*counterStat{}

	var order []dotnet.CounterValue

	for _, s := range samples {
		for _, v := range s.Counters {
			st, ok := stats[v.Name]
			if !ok {
				st = &counterStat{Name: v.Name, DisplayName: v.DisplayName, Unit: v.Unit, Kind: v.Kind}
				stats[v.Name] = st
				order = append(order, v)
			}

			st.add(v.Value)
		}
	}

	out := countersSummary{
		Schema: jsonSchemaVersion, Target: target, Provider: counterProvider, IntervalSec: params.IntervalSec,
		DurationMs: params.DurationMs, Samples: len(samples), EndReason: res.EndReason, Counters: []counterStat{},
	}

	if res.EndReason != dotnet.EndExited && params.IntervalSec > 0 {
		expected := int(params.DurationMs / (int64(params.IntervalSec) * 1000))
		out.MissedIntervals = max(0, expected-len(samples))
	}

	for _, v := range orderCounters(order) {
		st := stats[v.Name]
		if st.Kind == dotnet.KindSum {
			rate := *st.Total / float64(st.seen*params.IntervalSec)
			st.PerSecond = &rate
		}

		out.Counters = append(out.Counters, *st)
	}

	return out
}

func (st *counterStat) add(v float64) {
	st.seen++

	if st.Kind == dotnet.KindSum {
		total := v
		if st.Total != nil {
			total += *st.Total
		}

		st.Total = &total

		return
	}

	last, lo, hi := v, v, v
	if st.Min != nil {
		lo, hi = min(*st.Min, v), max(*st.Max, v)
	}

	st.Last, st.Min, st.Max = &last, &lo, &hi
}
