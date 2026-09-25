// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/helper/helpertest"
)

func TestTracePlan(t *testing.T) {
	t.Parallel()

	dir := existingFiles(t)
	file := filepath.Join(dir, "file.dmp")
	cpu := traceOptions{profile: "cpu", duration: 10 * time.Second, top: 10}

	with := func(f func(o *traceOptions)) traceOptions {
		o := cpu
		f(&o)

		return o
	}

	tests := []struct {
		name     string
		o        traceOptions
		code     api.Code
		contains string
		check    func(p tracePlan) bool
	}{
		{"defaults", cpu, "", "", func(p tracePlan) bool {
			return p.profile == "cpu" && p.duration == 10*time.Second && p.timeout == 70*time.Second && p.file == "" && p.top == 10
		}},
		{"gc for a pid", with(func(o *traceOptions) { o.pid, o.profile = 7, "gc" }), "", "", func(p tracePlan) bool { return p.pid == 7 && p.profile == "gc" }},
		{"bad profile", with(func(o *traceOptions) { o.profile = "CPU" }), api.CodeInvalidRequest, `--profile must be cpu or gc, not "CPU"`, nil},
		{"duration 1s", with(func(o *traceOptions) { o.duration = time.Second }), "", "", nil},
		{"duration 5m", with(func(o *traceOptions) { o.duration = 5 * time.Minute }), "", "", func(p tracePlan) bool { return p.timeout == 6*time.Minute }},
		{"duration under 1s", with(func(o *traceOptions) { o.duration = 999 * time.Millisecond }), api.CodeInvalidRequest, "--duration must be from 1s to 5m0s", nil},
		{"duration over 5m", with(func(o *traceOptions) { o.duration = 5*time.Minute + time.Second }), api.CodeInvalidRequest, "--duration must be from 1s to 5m0s", nil},
		{"top 0", with(func(o *traceOptions) { o.top = 0 }), api.CodeInvalidRequest, "--top must be 1 to 1000, not 0", nil},
		{"top 1001", with(func(o *traceOptions) { o.top = 1001 }), api.CodeInvalidRequest, "--top must be 1 to 1000", nil},
		{"top 1000", with(func(o *traceOptions) { o.top = 1000 }), "", "", nil},
		{"timeout set", with(func(o *traceOptions) { o.timeout, o.timeoutSet = 11*time.Second, true }), "", "", func(p tracePlan) bool { return p.timeout == 11*time.Second }},
		{"timeout not over duration", with(func(o *traceOptions) { o.timeout, o.timeoutSet = 10*time.Second, true }), api.CodeInvalidRequest, "--timeout must be longer than --duration (10s)", nil},
		{"timeout over 1h", with(func(o *traceOptions) { o.timeout, o.timeoutSet = time.Hour+time.Second, true }), api.CodeInvalidRequest, "at most 1h0m0s", nil},
		{"negative pid", with(func(o *traceOptions) { o.pid = -1 }), api.CodeInvalidRequest, "--pid must be positive", nil},
		{"pid and session", with(func(o *traceOptions) { o.pid, o.sessionSet = 1, true }), api.CodeInvalidRequest, "--pid and -s", nil},
		{"out new", with(func(o *traceOptions) { o.out = filepath.Join(dir, "new.nettrace") }), "", "", func(p tracePlan) bool { return p.out == filepath.Join(dir, "new.nettrace") }},
		{"out exists", with(func(o *traceOptions) { o.out = file }), api.CodeInvalidRequest, "exists; eyedbg never overwrites", nil},
		{"out parent missing", with(func(o *traceOptions) { o.out = filepath.Join(dir, "no", "x") }), api.CodeInvalidRequest, "isn't a directory", nil},
		{"file", with(func(o *traceOptions) { o.file = file }), "", "", func(p tracePlan) bool {
			return p.file == file && p.timeout == defaultTraceFileTimeout && p.profile == "" && p.duration == 0
		}},
		{"file with timeout", with(func(o *traceOptions) { o.file, o.timeout, o.timeoutSet = file, time.Second, true }), "", "", func(p tracePlan) bool { return p.timeout == time.Second }},
		{"file timeout 0", with(func(o *traceOptions) { o.file, o.timeoutSet = file, true }), api.CodeInvalidRequest, "--timeout must be more than 0", nil},
		{"file timeout over 1h", with(func(o *traceOptions) { o.file, o.timeout, o.timeoutSet = file, 2*time.Hour, true }), api.CodeInvalidRequest, "at most 1h0m0s", nil},
		{"file and pid", with(func(o *traceOptions) { o.file, o.pid = file, 3 }), api.CodeInvalidRequest, "a FILE and --pid or -s/--session both name a target", nil},
		{"file and session", with(func(o *traceOptions) { o.file, o.sessionSet = file, true }), api.CodeInvalidRequest, "a FILE and --pid", nil},
		{"file and profile", with(func(o *traceOptions) { o.file, o.profileSet = file, true }), api.CodeInvalidRequest, "--profile, --duration and --out are for a process", nil},
		{"file and duration", with(func(o *traceOptions) { o.file, o.durationSet = file, true }), api.CodeInvalidRequest, "a FILE is summarized as it is", nil},
		{"file and out", with(func(o *traceOptions) { o.file, o.out = file, filepath.Join(dir, "o") }), api.CodeInvalidRequest, "--out are for a process", nil},
		{"missing file", with(func(o *traceOptions) { o.file = filepath.Join(dir, "none.nettrace") }), api.CodeInvalidRequest, "no trace file at", nil},
		{"directory", with(func(o *traceOptions) { o.file = filepath.Join(dir, "sub") }), api.CodeInvalidRequest, "isn't a file", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := tt.o.plan()
			if tt.code != "" {
				requireCode(t, err, tt.code, tt.contains)

				return
			}

			if err != nil || (tt.check != nil && !tt.check(p)) {
				t.Errorf("plan = %+v, %v", p, err)
			}
		})
	}
}

func TestTraceGoldens(t *testing.T) {
	t.Parallel()

	target := dotnetTarget{PID: 4321, Name: "dotnet", Session: "s-k3f9"}
	private := "/home/u/.eyedbg/traces/4321-20260925T150300Z-cpu-1f3a9c0e.nettrace"
	cpu := fakeResult[dotnet.TraceSummaryResult](t, helpertest.FakeTraceSummary("cpu"))
	gc := fakeResult[dotnet.TraceSummaryResult](t, helpertest.FakeTraceSummary("gc"))

	cpuOut := traceOutput{
		Schema: 1, Target: &target, Trace: traceRef{Path: private, Private: true, Taken: true, Bytes: 5557452},
		Profile: "cpu", EndReason: "duration", TraceSummaryResult: cpu,
	}

	gcOut := traceOutput{
		Schema: 1, Target: &dotnetTarget{PID: 77, Name: "evil\x1b[2J"}, Trace: traceRef{Path: "/tmp/my traces/gc.nettrace", Taken: true, Bytes: 26214400},
		Profile: "gc", EndReason: "exited", TraceSummaryResult: gc,
	}

	// A FILE with both CPU samples and GCs; a cpu trace cut at the size limit with no samples.
	fileOut := traceOutput{Schema: 1, Trace: traceRef{Path: "/tmp/app.nettrace", Bytes: 1234567}, TraceSummaryResult: cpu}
	fileOut.Allocations = gc.Allocations

	// Readable traces with nothing in them (M8-Q13): a quiet process traced for gc, a FILE.
	quiet := fakeResult[dotnet.TraceSummaryResult](t, helpertest.FakeTraceSummary("empty"))
	quietGC := traceOutput{
		Schema: 1, Target: &target, Trace: traceRef{Path: strings.Replace(private, "-cpu-", "-gc-", 1), Private: true, Taken: true, Bytes: 2048},
		Profile: "gc", EndReason: "duration", TraceSummaryResult: quiet,
	}
	gcNoTicks := quietGC
	gcNoTicks.GC = gc.GC
	quietFile := traceOutput{Schema: 1, Trace: traceRef{Path: "/tmp/quiet.nettrace", Bytes: 2048}, TraceSummaryResult: quiet}
	empty := traceOutput{
		Schema: 1, Target: &target, Trace: traceRef{Path: private, Private: true, Taken: true, Bytes: 600},
		Profile: "cpu", EndReason: "size", TraceSummaryResult: dotnet.TraceSummaryResult{DurationMs: 3000, GC: gc.GC},
	}

	tests := []struct {
		golden string
		write  func(w *bytes.Buffer) error
	}{
		{"dotnet_trace_cpu.golden", func(w *bytes.Buffer) error {
			if err := writeTrace(w, cpuOut, false); err != nil {
				return err
			}

			return writeTrace(w, empty, false)
		}},
		{"dotnet_trace_cpu_json.golden", func(w *bytes.Buffer) error { return writeTrace(w, cpuOut, true) }},
		{"dotnet_trace_gc.golden", writeTraces(false, gcOut, quietGC, gcNoTicks)},
		{"dotnet_trace_gc_json.golden", writeTraces(true, gcOut, quietGC)},
		{"dotnet_trace_file.golden", writeTraces(false, fileOut, quietFile)},
	}

	for _, tt := range tests {
		t.Run(tt.golden, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := tt.write(&b); err != nil {
				t.Fatal(err)
			}

			// Text shows '?' for control characters; JSON escapes C0 ones
			// (encoding/json leaves C1 as is, as in every --json output).
			bad := "\x1b\x07\u009b"
			if strings.HasSuffix(tt.golden, "_json.golden") {
				bad = "\x1b\x07"
			}

			if strings.ContainsAny(strings.ReplaceAll(b.String(), "\n", ""), bad) {
				t.Errorf("control characters reached the output:\n%q", b.String())
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

// writeTraces returns a golden writer printing outs one after another.
func writeTraces(asJSON bool, outs ...traceOutput) func(w *bytes.Buffer) error {
	return func(w *bytes.Buffer) error {
		for _, o := range outs {
			if err := writeTrace(w, o, asJSON); err != nil {
				return err
			}
		}

		return nil
	}
}

func TestPercent(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		n, total int64
		want     string
	}{
		{4120, 6001, "68.7%"},
		{1, 1, "100.0%"},
		{0, 10, "0.0%"},
		{5, 0, ""},
	} {
		if got := percent(tt.n, tt.total); got != tt.want {
			t.Errorf("percent(%d, %d) = %q, want %q", tt.n, tt.total, got, tt.want)
		}
	}
}

// quotedPrefix is s as the start of a JSON string (an opening quote, no
// closing one).
func quotedPrefix(s string) string { return strings.TrimSuffix(strconv.Quote(s), `"`) }

// traceDir is the traces directory under $EYEDBG_HOME.
func traceDir() string { return filepath.Join(os.Getenv(adapters.EnvHome), artifacts.Traces) }

// traceFiles lists the traces directory.
func traceFiles(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(traceDir())
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// TestDotnetTraceWithFakeHelper runs trace against the fake helper, with
// $EYEDBG_HOME in the test's directory. Not parallel: it sets environment
// variables.
func TestDotnetTraceWithFakeHelper(t *testing.T) {
	isolate(t)
	useFakeHelper(t, helpertest.ModeOK)

	calls := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(helpertest.EnvCalls, calls)

	self := strconv.Itoa(os.Getpid())
	path := checkPrivateTrace(t, self, calls)

	checkTraceText(t, self)
	checkTraceOut(t, self)
	checkTraceFiles(t, calls, path)
}

// checkPrivateTrace runs a cpu trace: a fresh name in the private
// directory, 0700 and 0600, the scratch file gone, the helper's params. It
// returns the trace's path.
func checkPrivateTrace(t *testing.T, self, calls string) string {
	t.Helper()

	var tr traceOutput

	decodeJSON(t, run(t, 0, "dotnet", "trace", "--pid", self, "--duration", "2s", "--top", "3", "--json"), &tr)

	path := tr.Trace.Path
	want := traceRef{Path: path, Private: true, Taken: true, Bytes: int64(len(helpertest.TraceContent("cpu")))}

	if filepath.Dir(path) != traceDir() || artifacts.TypeOf(artifacts.Traces, filepath.Base(path)) != "cpu" || tr.Trace != want {
		t.Fatalf("trace --json: trace %+v", tr.Trace)
	}

	if tr.Target == nil || tr.Target.PID != os.Getpid() || tr.Profile != "cpu" || tr.EndReason != "duration" || tr.CPU == nil || tr.CPU.Samples != 9812 {
		t.Fatalf("trace --json = %+v", tr)
	}

	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{path: 0o600, traceDir(): 0o700} {
			if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: mode %v, %v; want %v", p, info.Mode().Perm(), err, want)
			}
		}
	}

	// The scratch file (and .new) the summary left is gone: only the trace remains.
	if got := traceFiles(t); !slices.Equal(got, []string{filepath.Base(path)}) {
		t.Errorf("traces dir = %v, want only the trace", got)
	}

	requireCalls(t, calls,
		[]string{"trace ", `"profile":"cpu"`, `"durationMs":2000`, `"path":` + strconv.Quote(path)},
		[]string{"traceSummary ", `"path":` + strconv.Quote(path), `"scratch":` + quotedPrefix(traceDir()), "-cpu-", `.etlx"`, `"top":3`, `"timeoutMs":`})

	return path
}

// checkTraceText runs a gc trace in text.
func checkTraceText(t *testing.T, self string) {
	t.Helper()

	t.Setenv(helpertest.EnvEnd, "exited")

	text := run(t, 0, "dotnet", "trace", "--pid", self, "--profile", "gc")
	expectOutput(t, text, "gc trace of pid "+self+" (", "-gc-", "private to you, removed after 7 days (at most 10 kept)",
		"GC: 3,048 collections", "allocated ≈ 17.8 GiB", "System.Byte[]", "App.Evil?]0;x?Type", "the process exited after 10s",
		"5 events were lost")

	t.Setenv(helpertest.EnvEnd, "")
}

// checkTraceOut runs trace --out: the trace is placed there, the private
// directory keeps nothing new, and an existing --out is refused.
func checkTraceOut(t *testing.T, self string) {
	t.Helper()

	before := traceFiles(t)
	out := filepath.Join(t.TempDir(), "x.nettrace")
	text := run(t, 0, "dotnet", "trace", "--pid", self, "--out", out)
	expectOutput(t, text, "\n"+out+" (", "open it in PerfView")

	if strings.Contains(text, "private to you") {
		t.Errorf("--out output mentions retention:\n%s", text)
	}

	if b, err := os.ReadFile(out); err != nil || string(b) != helpertest.TraceContent("cpu") {
		t.Errorf("--out file = %q, %v", b, err)
	}

	if after := traceFiles(t); !slices.Equal(after, before) {
		t.Errorf("private traces %v, want %v", after, before)
	}

	expectOutput(t, run(t, exitError, "dotnet", "trace", "--pid", self, "--out", out), "exists; eyedbg never overwrites [INVALID_REQUEST]")

	if after := traceFiles(t); !slices.Equal(after, before) {
		t.Errorf("a refused --out left %v, want %v", after, before)
	}
}

// checkTraceFiles runs trace FILE: eyedbg's own (private, the checked
// file itself) and another; then $EYEDBG_SESSION doesn't apply.
func checkTraceFiles(t *testing.T, calls, private string) {
	t.Helper()

	_ = os.Remove(calls)

	var own traceOutput

	decodeJSON(t, run(t, 0, "dotnet", "trace", private, "--json"), &own)

	if !own.Trace.Private || own.Trace.Taken || own.Target != nil || own.Profile != "" || own.EndReason != "" || own.CPU == nil {
		t.Errorf("trace FILE --json = %+v", own)
	}

	other := filepath.Join(t.TempDir(), "other.nettrace")
	if err := os.WriteFile(other, []byte(helpertest.TraceContent("gc")), 0o600); err != nil {
		t.Fatal(err)
	}

	expectOutput(t, run(t, 0, "dotnet", "trace", other), "trace "+other+" (", "GC: 3,048 collections")

	realOther, _ := filepath.EvalSymlinks(other)
	realPrivate, _ := filepath.EvalSymlinks(private)
	requireCalls(t, calls,
		[]string{"traceSummary ", `"path":` + strconv.Quote(realPrivate), `"scratch":` + quotedPrefix(traceDir()), "-file-", `"top":10`},
		[]string{"traceSummary ", `"path":` + strconv.Quote(realOther)})

	// The cpu and gc traces; --out's went there, and no scratch file stayed.
	if got := traceFiles(t); len(got) != 2 || slices.ContainsFunc(got, func(n string) bool { return strings.Contains(n, ".etlx") }) {
		t.Errorf("traces dir = %v, want 2 traces and no scratch file", got)
	}

	// A readable trace with nothing in it is an answer (M8-Q13), not TRACE_UNSUPPORTED.
	quiet := filepath.Join(t.TempDir(), "quiet.nettrace")
	if err := os.WriteFile(quiet, []byte(helpertest.TraceContent("empty")), 0o600); err != nil {
		t.Fatal(err)
	}

	expectOutput(t, run(t, 0, "dotnet", "trace", quiet), "no CPU samples or GC events in this trace")

	var q traceOutput

	decodeJSON(t, run(t, 0, "dotnet", "trace", quiet, "--json"), &q)

	if q.CPU != nil || q.GC != nil || q.Allocations != nil || q.DurationMs != 3001 {
		t.Errorf("quiet trace --json = %+v", q)
	}

	t.Setenv(envSession, "s-nope")
	expectOutput(t, run(t, 0, "dotnet", "trace", other), "trace "+other)
	expectOutput(t, run(t, exitError, "dotnet", "trace", other, "-s", "s-nope"), "a FILE and --pid or -s/--session both name a target")
}

// TestDotnetTraceFailures checks a trace that doesn't arrive, an older
// helper, a summary error (the trace is kept) and pruning. Not parallel:
// it sets environment variables.
func TestDotnetTraceFailures(t *testing.T) {
	isolate(t)

	self := strconv.Itoa(os.Getpid())

	useFakeHelper(t, helpertest.ModeTraceNoFile)
	expectOutput(t, run(t, exitAdapter, "dotnet", "trace", "--pid", self), "the .NET helper reported a trace but none is at", "[HELPER_FAILED]")

	useFakeHelper(t, helpertest.ModeOld)
	expectOutput(t, run(t, exitEnvironment, "dotnet", "trace", "--pid", self), `the .NET helper doesn't know "trace" (older than eyedbg?) [HELPER_MISMATCH]`)

	useFakeHelper(t, helpertest.ModeErrorPrefix+"NOT_DOTNET")
	expectOutput(t, run(t, exitState, "dotnet", "trace", "--pid", self), "[NOT_DOTNET]")

	if n := traceFiles(t); len(n) != 0 {
		t.Errorf("failed traces left %v", n)
	}

	useFakeHelper(t, helpertest.ModeErrorPrefix+"TRACE_UNSUPPORTED")
	expectOutput(t, run(t, exitState, "dotnet", "trace", filepath.Join(existingFiles(t), "file.dmp")), "[TRACE_UNSUPPORTED]")

	// A summary that fails keeps the trace it recorded, and says where.
	useFakeHelper(t, helpertest.ModeSummaryErrorPrefix+"TRACE_UNSUPPORTED")

	_, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"dotnet", "trace", "--pid", self})
	left := traceFiles(t)

	if code != exitState || len(left) != 1 || !strings.Contains(string(stderr), "the trace is kept at "+filepath.Join(traceDir(), left[0])) ||
		!strings.Contains(string(stderr), "summarize it again: eyedbg dotnet trace ") {
		t.Errorf("summary error: exit %d, left %v\n%s", code, left, stderr)
	}

	useFakeHelper(t, helpertest.ModeOld)
	expectOutput(t, run(t, exitEnvironment, "dotnet", "trace", filepath.Join(traceDir(), left[0])), `doesn't know "traceSummary"`)

	// Pruning: an old trace and a stale scratch file go, anything else stays.
	old := filepath.Join(traceDir(), "1-20000101T000000Z-cpu-00000000.nettrace")
	stale := filepath.Join(traceDir(), "1-20000101T000000Z-file-00000000.etlx")
	notes := filepath.Join(traceDir(), "notes.txt")

	for p, age := range map[string]time.Duration{old: 8 * 24 * time.Hour, stale: 2 * time.Hour, notes: 30 * 24 * time.Hour} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age)); err != nil {
			t.Fatal(err)
		}
	}

	useFakeHelper(t, helpertest.ModeOK)
	run(t, 0, "dotnet", "trace", "--pid", self)

	for p, gone := range map[string]bool{old: true, stale: true, notes: false} {
		if _, err := os.Stat(p); os.IsNotExist(err) != gone {
			t.Errorf("%s: gone = %v, want %v", p, os.IsNotExist(err), gone)
		}
	}
}

// TestDotnetTraceKeepsTen checks the retention bound counts the new trace.
// Not parallel: it sets environment variables.
func TestDotnetTraceKeepsTen(t *testing.T) {
	isolate(t)
	useFakeHelper(t, helpertest.ModeOK)

	dir, err := artifacts.Dir(artifacts.Traces)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()

	for i := range 12 {
		p := filepath.Join(dir, fmt.Sprintf("1-20260925T000000Z-gc-%08x.nettrace", i))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		mod := now.Add(-time.Duration(i+1) * time.Minute)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	var tr traceOutput

	decodeJSON(t, run(t, 0, "dotnet", "trace", "--pid", strconv.Itoa(os.Getpid()), "--json"), &tr)

	left := traceFiles(t)
	if len(left) != artifacts.Keep || !slices.Contains(left, filepath.Base(tr.Trace.Path)) {
		t.Fatalf("%d traces left (%v), want %d incl. the new %s", len(left), left, artifacts.Keep, filepath.Base(tr.Trace.Path))
	}
}

// TestDotnetTraceSessionTargets checks trace refuses a stopped session
// before any helper starts, like counters. Not parallel: it sets
// environment variables.
func TestDotnetTraceSessionTargets(t *testing.T) {
	p := isolate(t)
	t.Setenv(dotnet.EnvHelper, filepath.Join(t.TempDir(), "never-started.dll"))

	expectOutput(t, run(t, exitState, "dotnet", "trace"), "[NO_SESSION]", "or pass --pid N")

	serveWithDrivers(t, p, fakeDriver{}, namedDriver{fakeDriver{}, dotnet.Language})

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out := run(t, 0, "start", "dotnet", "--program", prog, "--stop-on-entry", "--timeout", "20s")
	id := strings.Fields(strings.TrimPrefix(out, "session "))[0]

	// The helper path doesn't exist: a helper start would fail with HELPER_NOT_FOUND.
	expectOutput(t, run(t, exitState, "dotnet", "trace", "-s", id), "[NOT_RUNNING]", "is stopped (entry)", "can't start a diagnostics session")

	if _, err := os.Stat(traceDir()); err == nil {
		if n := traceFiles(t); len(n) != 0 {
			t.Errorf("a refused trace left %v", n)
		}
	}

	run(t, 0, "stop", "-s", id)
}
