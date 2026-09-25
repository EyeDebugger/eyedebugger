// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/helper/helpertest"
	"github.com/eyedebugger/eyedebugger/internal/proc"
)

// fakeLookup knows the processes in procs; others don't exist.
func fakeLookup(procs map[int]proc.Info) lookupFunc {
	return func(_ context.Context, pid int) (proc.Info, error) {
		if p, ok := procs[pid]; ok {
			if p.PID < 0 {
				return proc.Info{}, errors.New("access denied")
			}

			return p, nil
		}

		return proc.Info{}, proc.ErrNoProcess
	}
}

var testProcs = map[int]proc.Info{
	10: {PID: 10, Name: "dotnet", Owner: "me", SameUser: true},
	11: {PID: 11, Name: "api", Owner: "me", SameUser: true},
	20: {PID: 20, Name: "dotnet", Owner: "root", SameUser: false},
	21: {PID: 21, Name: "Runner.Listener", SameUser: false},
	30: {PID: -1},
}

func requireCode(t *testing.T, err error, code api.Code, contains ...string) {
	t.Helper()

	e, ok := errors.AsType[*api.Error](err)
	if !ok || e.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}

	for _, s := range contains {
		if !strings.Contains(e.Message+" | "+e.Hint, s) {
			t.Errorf("error %q (hint %q) lacks %q", e.Message, e.Hint, s)
		}
	}
}

func TestResolvePIDTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pid      int
		want     dotnetTarget
		code     api.Code
		contains string
	}{
		{pid: 10, want: dotnetTarget{PID: 10, Name: "dotnet"}},
		{pid: 99, code: api.CodeInvalidRequest, contains: "no process 99"},
		{pid: 20, code: api.CodeInvalidRequest, contains: "process 20 belongs to another user (root): eyedbg inspects only your own processes"},
		{pid: 21, code: api.CodeInvalidRequest, contains: "another user (unknown)"},
		{pid: 30, code: api.CodeInvalidRequest, contains: "look up process 30: access denied"},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.pid), func(t *testing.T) {
			t.Parallel()

			got, err := resolvePIDTarget(t.Context(), tt.pid, fakeLookup(testProcs))
			if tt.code != "" {
				requireCode(t, err, tt.code, tt.contains)

				return
			}

			if err != nil || got != tt.want {
				t.Errorf("resolvePIDTarget = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestResolveSessionTarget(t *testing.T) {
	t.Parallel()

	session := func(lang string, state api.SessionState, pid int) api.Snapshot {
		s := api.Snapshot{Session: api.SessionInfo{ID: "s-1", Lang: lang, State: state, PID: pid}}
		if state == api.StateStopped {
			s.Session.Stop = &api.StopInfo{Reason: "breakpoint"}
		}

		return s
	}

	tests := []struct {
		name     string
		snap     api.Snapshot
		want     dotnetTarget
		code     api.Code
		contains []string
	}{
		{"running", session("dotnet", api.StateRunning, 11), dotnetTarget{PID: 11, Name: "api", Session: "s-1"}, "", nil},
		{"python", session("python", api.StateRunning, 11), dotnetTarget{}, api.CodeNotDotnet, []string{"session s-1 debugs python", "--pid N"}},
		{"exited", session("dotnet", api.StateExited, 11), dotnetTarget{}, api.CodeSessionExited, []string{"has ended (exited)"}},
		{"lost", session("dotnet", api.StateLost, 11), dotnetTarget{}, api.CodeSessionExited, []string{"(lost)"}},
		{"starting", session("dotnet", api.StateStarting, 0), dotnetTarget{}, api.CodeNotRunning, []string{"still starting"}},
		{"no pid", session("dotnet", api.StateRunning, 0), dotnetTarget{}, api.CodeNotRunning, []string{"still starting"}},
		{
			"stopped", session("dotnet", api.StateStopped, 11),
			dotnetTarget{},
			api.CodeNotRunning,
			[]string{"session s-1 is stopped (breakpoint): a stopped .NET runtime can't start a diagnostics session", "eyedbg continue"},
		},
		{"gone", session("dotnet", api.StateRunning, 99), dotnetTarget{}, api.CodeInvalidRequest, []string{"no process 99"}},
		{"foreign", session("dotnet", api.StateRunning, 20), dotnetTarget{}, api.CodeInvalidRequest, []string{"another user"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveSessionTarget(t.Context(), tt.snap, fakeLookup(testProcs))
			if tt.code != "" {
				requireCode(t, err, tt.code, tt.contains...)

				return
			}

			if err != nil || got != tt.want {
				t.Errorf("resolveSessionTarget = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestPsRows(t *testing.T) {
	t.Parallel()

	sessions := []api.SessionInfo{
		{ID: "s-live", PID: 11, State: api.StateRunning},
		{ID: "s-old", PID: 10, State: api.StateExited},
		{ID: "s-lost", PID: 10, State: api.StateLost},
	}

	got := psRows(t.Context(), []int{21, 11, 99, 10, 20, 30, 11}, fakeLookup(testProcs), sessions)
	want := []psRow{{PID: 10, Name: "dotnet"}, {PID: 11, Name: "api", Session: "s-live"}}

	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("psRows = %+v, want %+v", got, want)
	}

	if got := psRows(t.Context(), nil, fakeLookup(testProcs), nil); got == nil || len(got) != 0 {
		t.Errorf("psRows(nil) = %#v, want an empty slice", got)
	}
}

func TestCountersPlan(t *testing.T) {
	t.Parallel()

	base := countersOptions{duration: defaultCountersDuration, interval: defaultCountersInterval}
	with := func(f func(*countersOptions)) countersOptions {
		o := base
		f(&o)

		return o
	}

	tests := []struct {
		name     string
		opts     countersOptions
		want     countersPlan
		contains string
	}{
		{"defaults", base, countersPlan{params: dotnet.CountersParams{IntervalSec: 1, DurationMs: 5000}, timeout: 35 * time.Second}, ""},
		{
			"pid, 10s every 2s", with(func(o *countersOptions) {
				o.pid, o.duration, o.durationSet, o.interval = 7, 10*time.Second, true, 2*time.Second
			}),
			countersPlan{params: dotnet.CountersParams{PID: 7, IntervalSec: 2, DurationMs: 10000}, timeout: 40 * time.Second},
			"",
		},
		{"watch", with(func(o *countersOptions) { o.watch = true }), countersPlan{params: dotnet.CountersParams{IntervalSec: 1}, watch: true}, ""},
		{
			"watch with duration", with(func(o *countersOptions) { o.watch, o.duration, o.durationSet = true, 2*time.Hour, true }),
			countersPlan{params: dotnet.CountersParams{IntervalSec: 1, DurationMs: 7200000}, watch: true, timeout: 2*time.Hour + 30*time.Second},
			"",
		},
		{
			"timeout", with(func(o *countersOptions) { o.timeout, o.timeoutSet = 6*time.Second, true }),
			countersPlan{params: dotnet.CountersParams{IntervalSec: 1, DurationMs: 5000}, timeout: 6 * time.Second},
			"",
		},
		{
			"names", with(func(o *countersOptions) { o.names = []string{"cpu-usage"} }),
			countersPlan{params: dotnet.CountersParams{IntervalSec: 1, DurationMs: 5000}, names: []string{"cpu-usage"}, timeout: 35 * time.Second},
			"",
		},
		{"pid and session", with(func(o *countersOptions) { o.pid, o.sessionSet = 7, true }), countersPlan{}, "--pid and -s/--session"},
		{"negative pid", with(func(o *countersOptions) { o.pid = -2 }), countersPlan{}, "--pid must be positive"},
		{"fraction interval", with(func(o *countersOptions) { o.interval = 1500 * time.Millisecond }), countersPlan{}, "whole seconds from 1s to 60s, not 1.5s"},
		{"long interval", with(func(o *countersOptions) { o.interval = 2 * time.Minute }), countersPlan{}, "--interval"},
		{"short duration", with(func(o *countersOptions) { o.interval = 2 * time.Second; o.duration = time.Second }), countersPlan{}, "from one --interval (2s) to 1h0m0s, not 1s"},
		{"long duration", with(func(o *countersOptions) { o.duration = 2 * time.Hour }), countersPlan{}, "to 1h0m0s"},
		{"watch too long", with(func(o *countersOptions) { o.watch, o.duration, o.durationSet = true, 25*time.Hour, true }), countersPlan{}, "to 24h0m0s"},
		{"timeout not longer", with(func(o *countersOptions) { o.timeout, o.timeoutSet = 5*time.Second, true }), countersPlan{}, "--timeout must be longer than --duration (5s), not 5s"},
		{"zero timeout", with(func(o *countersOptions) { o.watch, o.timeoutSet = true, true }), countersPlan{}, "--timeout must be longer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.opts.plan()
			if tt.contains != "" {
				requireCode(t, err, api.CodeInvalidRequest, tt.contains)

				return
			}

			if err != nil || got.params != tt.want.params || got.watch != tt.want.watch || got.timeout != tt.want.timeout ||
				strings.Join(got.names, ",") != strings.Join(tt.want.names, ",") {
				t.Errorf("plan() = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func counter(name, unit, kind string, value float64) dotnet.CounterValue {
	return dotnet.CounterValue{Provider: "System.Runtime", Name: name, DisplayName: "Display " + name, Unit: unit, Kind: kind, Value: value}
}

// testSamples are three 1s samples of a few counters, out of order, with
// one unknown counter.
func testSamples() []dotnet.CounterSample {
	return []dotnet.CounterSample{
		{ElapsedMs: 1100, Counters: []dotnet.CounterValue{
			counter("zz-custom", "", dotnet.KindGauge, 1), counter("exception-count", "", dotnet.KindSum, 2),
			counter("cpu-usage", "%", dotnet.KindGauge, 0.5), counter("working-set", "MB", dotnet.KindGauge, 49.25),
			counter("alloc-rate", "B", dotnet.KindSum, 1024), counter("aa-custom", "", dotnet.KindGauge, 3),
		}},
		{ElapsedMs: 2100, Counters: []dotnet.CounterValue{
			counter("zz-custom", "", dotnet.KindGauge, 1), counter("exception-count", "", dotnet.KindSum, 0),
			counter("cpu-usage", "%", dotnet.KindGauge, 12.345), counter("working-set", "MB", dotnet.KindGauge, 49.25),
			counter("alloc-rate", "B", dotnet.KindSum, 2048), counter("aa-custom", "", dotnet.KindGauge, 3),
		}},
		{ElapsedMs: 3100, Counters: []dotnet.CounterValue{
			counter("zz-custom", "", dotnet.KindGauge, 1), counter("exception-count", "", dotnet.KindSum, 4),
			counter("cpu-usage", "%", dotnet.KindGauge, 0.0286), counter("working-set", "MB", dotnet.KindGauge, 50.5),
			counter("alloc-rate", "B", dotnet.KindSum, 3072), counter("aa-custom", "", dotnet.KindGauge, 3),
		}},
	}
}

var testTarget = dotnetTarget{PID: 4321, Name: "dotnet", Session: "s-k3f9"}

func TestAggregate(t *testing.T) {
	t.Parallel()

	params := dotnet.CountersParams{PID: 4321, IntervalSec: 1, DurationMs: 5000}
	s := aggregate(testTarget, params, testSamples(), dotnet.CountersResult{Samples: 3, EndReason: dotnet.EndDuration})

	var names []string
	for _, c := range s.Counters {
		names = append(names, c.Name)
	}

	if got := strings.Join(names, ","); got != "cpu-usage,working-set,alloc-rate,exception-count,aa-custom,zz-custom" {
		t.Errorf("order = %s", got)
	}

	checkGauge(t, s.Counters[0], 0.0286, 0.0286, 12.345)
	checkSum(t, s.Counters[3], 6, 2)

	if s.Samples != 3 || s.MissedIntervals != 2 || s.Provider != "System.Runtime" || s.Target != testTarget {
		t.Errorf("summary = %+v", s)
	}

	exited := aggregate(testTarget, params, testSamples()[:1], dotnet.CountersResult{Samples: 1, EndReason: dotnet.EndExited})
	if exited.MissedIntervals != 0 || exited.Samples != 1 {
		t.Errorf("exited summary = %+v", exited)
	}

	checkSum(t, exited.Counters[3], 2, 2)

	empty := aggregate(testTarget, params, nil, dotnet.CountersResult{EndReason: dotnet.EndExited})
	if empty.Counters == nil || len(empty.Counters) != 0 {
		t.Errorf("empty summary counters = %#v", empty.Counters)
	}
}

func checkGauge(t *testing.T, c counterStat, last, lo, hi float64) {
	t.Helper()

	if c.Kind != dotnet.KindGauge || c.Last == nil || *c.Last != last || *c.Min != lo || *c.Max != hi || c.Total != nil || c.PerSecond != nil {
		t.Errorf("%s = %+v, want gauge %v (%v–%v)", c.Name, c, last, lo, hi)
	}
}

func checkSum(t *testing.T, c counterStat, total, perSecond float64) {
	t.Helper()

	if c.Kind != dotnet.KindSum || c.Total == nil || *c.Total != total || *c.PerSecond != perSecond || c.Last != nil {
		t.Errorf("%s = %+v, want sum %v (%v/s)", c.Name, c, total, perSecond)
	}
}

func TestCounterNames(t *testing.T) {
	t.Parallel()

	first := testSamples()[0]

	if err := checkCounterNames(first, []string{"cpu-usage", "alloc-rate"}); err != nil {
		t.Errorf("known names: %v", err)
	}

	requireCode(t, checkCounterNames(first, []string{"cpu-usage", "cpu", "gc"}), api.CodeInvalidRequest,
		"no counter named cpu, gc", "available: cpu-usage, working-set, alloc-rate, exception-count, aa-custom, zz-custom")

	got := filterCounters(first, []string{"working-set", "cpu-usage"})
	if len(got.Counters) != 2 || got.Counters[0].Name != "cpu-usage" || got.Counters[1].Name != "working-set" || got.ElapsedMs != 1100 {
		t.Errorf("filterCounters = %+v", got)
	}

	if all := filterCounters(first, nil); len(all.Counters) != len(first.Counters) {
		t.Errorf("filterCounters(nil) kept %d", len(all.Counters))
	}
}

func TestFormatNumber(t *testing.T) {
	t.Parallel()

	tests := map[float64]string{
		0: "0", 3: "3", -4: "-4", 1e14: "100000000000000", 123.456: "123", 49.332: "49.33", 1.5: "1.5", 1.004: "1",
		0.028634025171522536: "0.0286", 0.5: "0.5", 0.000012345: "1.23e-05", -0.25: "-0.25",
	}

	for v, want := range tests {
		if got := formatNumber(v); got != want {
			t.Errorf("formatNumber(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestDescribeTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		target dotnetTarget
		want   string
	}{
		{dotnetTarget{PID: 7}, "pid 7"},
		{dotnetTarget{PID: 7, Name: "api", Session: "s-1"}, "pid 7 (api), session s-1"},
		{dotnetTarget{PID: 7, Name: "api\x1b[2J\u009b\xff"}, "pid 7 (api?[2J??)"},
	}

	for _, tt := range tests {
		if got := tt.target.describe(); got != tt.want {
			t.Errorf("describe(%+v) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestDotnetGoldens(t *testing.T) {
	t.Parallel()

	params := dotnet.CountersParams{PID: 4321, IntervalSec: 1, DurationMs: 5000}
	summary := aggregate(testTarget, params, testSamples(), dotnet.CountersResult{Samples: 3, EndReason: dotnet.EndExited})
	summary.MissedIntervals = 2 // both notes in one golden
	rows := []psRow{{PID: 812, Name: "dotnet", Session: "s-k3f9"}, {PID: 4321, Name: "api\x1b[2J"}, {PID: 70000}}

	// Text renders names and units from other processes fit for a terminal.
	hostileSample := dotnet.CounterSample{ElapsedMs: 4100, Counters: []dotnet.CounterValue{
		counter("evil\x1b]0;x\a", "B\x1b[2J", dotnet.KindSum, 1), counter("cpu-usage", "%\u009b", dotnet.KindGauge, 1),
	}}
	hostile := aggregate(dotnetTarget{PID: 4321, Name: "api\x1b[2J"}, params, append(testSamples()[:1], hostileSample),
		dotnet.CountersResult{Samples: 2, EndReason: dotnet.EndDuration})

	tests := []struct {
		golden string
		write  func(w *bytes.Buffer) error
	}{
		{"dotnet_ps.golden", func(w *bytes.Buffer) error { return writeDotnetPs(w, rows, false) }},
		{"dotnet_ps_json.golden", func(w *bytes.Buffer) error { return writeDotnetPs(w, rows, true) }},
		{"dotnet_counters.golden", func(w *bytes.Buffer) error {
			if err := writeCountersSummary(w, summary, false); err != nil {
				return err
			}

			return writeCountersSummary(w, hostile, false)
		}},
		{"dotnet_counters_json.golden", func(w *bytes.Buffer) error { return writeCountersSummary(w, summary, true) }},
		{"dotnet_counters_watch.golden", func(w *bytes.Buffer) error {
			for _, s := range testSamples() {
				if err := writeCounterSample(w, filterCounters(s, []string{"cpu-usage", "working-set", "exception-count"}), false); err != nil {
					return err
				}
			}

			return writeCounterSample(w, hostileSample, false)
		}},
		{"dotnet_counters_watch_json.golden", func(w *bytes.Buffer) error {
			for _, s := range testSamples()[:2] {
				if err := writeCounterSample(w, s, true); err != nil {
					return err
				}
			}

			return nil
		}},
	}

	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := tt.write(&b); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}

	var empty bytes.Buffer
	if err := writeDotnetPs(&empty, nil, false); err != nil || empty.Len() != 0 {
		t.Errorf("empty ps printed %q, %v", empty.String(), err)
	}
}

// useFakeHelper points eyedbg at the fake helper (this test binary) in mode.
// Not for parallel tests.
func useFakeHelper(t *testing.T, mode string) {
	t.Helper()

	exe, env, err := helpertest.Command(mode)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(dotnet.EnvHelper, exe)

	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
}

// TestDotnetWithFakeHelper runs the dotnet commands against the fake
// helper. Not parallel: it sets environment variables.
func TestDotnetWithFakeHelper(t *testing.T) {
	isolate(t)
	useFakeHelper(t, helpertest.ModeOK)

	self := strconv.Itoa(os.Getpid())

	// The fake lists itself (gone once closed: dropped) and its parent, us.
	expectOutput(t, run(t, 0, "dotnet", "ps"), "PID", "NAME", "SESSION", "\n"+self+"  ")

	var ps struct {
		Schema    int     `json:"schema"`
		Processes []psRow `json:"processes"`
	}

	decodeJSON(t, run(t, 0, "dotnet", "ps", "--json"), &ps)

	if ps.Schema != 1 || len(ps.Processes) != 1 || ps.Processes[0].PID != os.Getpid() || ps.Processes[0].Session != "" {
		t.Errorf("ps --json = %+v", ps)
	}

	expectOutput(t, run(t, 0, "dotnet", "counters", "--pid", self, "--duration", "3s"),
		"pid "+self+" (", "System.Runtime, 3 samples over 3s (every 1s)", "cpu-usage        30% (10–30)",
		"working-set      103 MB (101–103)", "exception-count  total 6 (2/s)")

	var summary countersSummary

	decodeJSON(t, run(t, 0, "dotnet", "counters", "--pid", self, "--duration", "3s", "--counters", "exception-count", "--json"), &summary)

	if summary.Samples != 3 || summary.MissedIntervals != 0 || summary.EndReason != "duration" || len(summary.Counters) != 1 ||
		*summary.Counters[0].Total != 6 || summary.Target.PID != os.Getpid() {
		t.Errorf("counters --json = %+v", summary)
	}

	out := run(t, 0, "dotnet", "counters", "--pid", self, "--watch", "--duration", "3s", "--json")
	if lines := strings.Split(strings.TrimSpace(out), "\n"); len(lines) != 3 || !strings.HasPrefix(lines[2], `{"schema":1,"elapsedMs":3000,"counters":[{"name":"cpu-usage"`) {
		t.Errorf("watch --json:\n%s", out)
	}

	expectOutput(t, run(t, exitError, "dotnet", "counters", "--pid", self, "--counters", "nope"),
		"no counter named nope [INVALID_REQUEST]", "available: cpu-usage, working-set, exception-count")
	expectOutput(t, run(t, exitError, "dotnet", "counters", "--pid", "999999999"), "no process 999999999 [INVALID_REQUEST]")

	t.Setenv(helpertest.EnvEnd, dotnet.EndExited)
	expectOutput(t, run(t, 0, "dotnet", "counters", "--pid", self, "--duration", "3s"), "note: the process exited during the collection")
	expectOutput(t, run(t, 0, "dotnet", "counters", "--pid", self, "--duration", "3s", "--watch"), "+3s  cpu-usage=30%", "(pid "+self+" (")
}

// TestDotnetWatchInterrupted ends an open-ended watch by canceling its
// context: exit 0, the samples printed.
func TestDotnetWatchInterrupted(t *testing.T) {
	isolate(t)
	useFakeHelper(t, helpertest.ModeOK)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var errOut bytes.Buffer

	out := &cancelAfterLines{n: 3, cancel: cancel}
	root := NewEyedbgCommand(testInfo)
	root.SetOut(out)
	root.SetErr(&errOut)

	if code := Run(ctx, root, []string{"dotnet", "counters", "--pid", strconv.Itoa(os.Getpid()), "--watch"}); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, errOut.String())
	}

	if got := strings.Count(out.String(), "\n"); got != 3 {
		t.Errorf("printed %d lines:\n%s", got, out.String())
	}
}

// cancelAfterLines cancels once n lines were written to it.
type cancelAfterLines struct {
	mu     sync.Mutex
	b      bytes.Buffer
	n      int
	cancel context.CancelFunc
}

func (c *cancelAfterLines) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, err := c.b.Write(p)
	if strings.Count(c.b.String(), "\n") >= c.n {
		c.cancel()
	}

	return n, err
}

func (c *cancelAfterLines) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.b.String()
}

// TestDotnetHelperFailures checks the helper errors reach the user. Not
// parallel: it sets environment variables.
func TestDotnetHelperFailures(t *testing.T) {
	isolate(t)

	self := strconv.Itoa(os.Getpid())

	useFakeHelper(t, helpertest.ModeProtocol2)
	expectOutput(t, run(t, exitEnvironment, "dotnet", "ps"), "[HELPER_MISMATCH]", "protocol 2")

	useFakeHelper(t, helpertest.ModeCrash)
	expectOutput(t, run(t, exitAdapter, "dotnet", "counters", "--pid", self), "[HELPER_FAILED]", "exited (exit status 3) before answering", "kaboom")

	useFakeHelper(t, helpertest.ModeErrorPrefix+"DIAGNOSTICS_DISABLED")
	expectOutput(t, run(t, exitState, "dotnet", "counters", "--pid", self), "[DIAGNOSTICS_DISABLED]", "hint: fake hint")

	t.Setenv(dotnet.EnvHelper, filepath.Join(t.TempDir(), dotnet.HelperDLL))
	expectOutput(t, run(t, exitEnvironment, "dotnet", "ps"), "[HELPER_NOT_FOUND]", dotnet.EnvHelper)
}

// TestDotnetSessionTargets resolves -s against an in-process daemon; each
// case fails before a helper would start (none is set up: the fake
// adapters the daemon starts would inherit its mode). Not parallel: it
// sets environment variables.
func TestDotnetSessionTargets(t *testing.T) {
	p := isolate(t)
	t.Setenv(dotnet.EnvHelper, filepath.Join(t.TempDir(), "never-started.dll"))

	expectOutput(t, run(t, exitState, "dotnet", "counters"), "[NO_SESSION]", "or pass --pid N (see 'eyedbg dotnet ps')")

	serveWithDrivers(t, p, fakeDriver{}, namedDriver{fakeDriver{}, dotnet.Language})

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out := run(t, 0, "start", "fake", "--program", prog, "--stop-on-entry", "--timeout", "20s")
	fakeID := strings.Fields(strings.TrimPrefix(out, "session "))[0]
	expectOutput(t, run(t, exitState, "dotnet", "counters", "-s", fakeID), "[NOT_DOTNET]", "debugs fake")

	out = run(t, 0, "start", "dotnet", "--program", prog, "--stop-on-entry", "--timeout", "20s")
	dotnetID := strings.Fields(strings.TrimPrefix(out, "session "))[0]
	expectOutput(t, run(t, exitState, "dotnet", "counters", "-s", dotnetID), "[NOT_RUNNING]", "is stopped (entry)")
	expectOutput(t, run(t, exitError, "dotnet", "counters", "-s", dotnetID, "--pid", "1"), "[INVALID_REQUEST]", "--pid and -s")

	run(t, 0, "stop", "-s", fakeID)
	run(t, 0, "stop", "-s", dotnetID)
}

// namedDriver is a driver under another name.
type namedDriver struct {
	fakeDriver

	name string
}

func (d namedDriver) Name() string { return d.name }

func decodeJSON(t *testing.T, out string, v any) {
	t.Helper()

	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
}
