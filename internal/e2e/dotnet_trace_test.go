// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The trace command's end-to-end tests (docs/adr/0016, P2-M8): the real
// helper against testdata/apps/dotnet/breadth in its 'burn' mode — a main
// thread spinning in Burn.Spin and a thread allocating 512-byte arrays.
// Assertions are on method, module and type names and counts only; the
// text output is printed only when a check fails.

// traceNameRE is the name eyedbg gives a trace in its private directory.
var traceNameRE = regexp.MustCompile(`^\d+-\d{8}T\d{6}Z-(cpu|gc)-[0-9a-f]{8}\.nettrace$`)

// hotMethod is the method a cpu trace of 'breadth burn' finds hottest.
const hotMethod = "Burn.Spin(int32)"

// traceJSON is the part of 'eyedbg dotnet trace --json' the tests read.
type traceJSON struct {
	Schema int `json:"schema"`
	Target *struct {
		PID     int    `json:"pid"`
		Session string `json:"session"`
	} `json:"target"`
	Trace struct {
		Path    string `json:"path"`
		Private bool   `json:"private"`
		Taken   bool   `json:"taken"`
		Bytes   int64  `json:"bytes"`
	} `json:"trace"`
	Profile   string `json:"profile"`
	EndReason string `json:"endReason"`
	CPU       *struct {
		Samples   int64           `json:"samples"`
		Managed   int64           `json:"managed"`
		Exclusive []methodSamples `json:"exclusive"`
	} `json:"cpu"`
	GC *struct {
		Collections int `json:"collections"`
		Gen0        int `json:"gen0"`
	} `json:"gc"`
	Allocations *struct {
		Types []allocatedType `json:"types"`
	} `json:"allocations"`
}

// allocatedType is a row of the allocations list.
type allocatedType struct {
	Name string `json:"name"`
}

// methodSamples is a row of the cpu lists.
type methodSamples struct {
	Method  string `json:"method"`
	Module  string `json:"module"`
	Samples int64  `json:"samples"`
}

// hotInTop5 says the exclusive list's first five rows have Burn.Spin.
func (tr traceJSON) hotInTop5() bool {
	if tr.CPU == nil {
		return false
	}

	return slices.ContainsFunc(tr.CPU.Exclusive[:min(5, len(tr.CPU.Exclusive))], func(m methodSamples) bool {
		return m.Method == hotMethod && m.Module == "breadth"
	})
}

// traceDirFiles lists the harness's traces directory.
func (h *harness) traceDirFiles() []string {
	h.t.Helper()

	entries, err := os.ReadDir(filepath.Join(h.homeDir, "traces"))
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		h.t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// TestDotnetTraceLive traces a live process: cpu and gc profiles, the
// file's summary, --out, pruning and unreadable files.
func TestDotnetTraceLive(t *testing.T) {
	t.Parallel()

	host, h := requireDotnetSDK(t)
	pid := strconv.Itoa(startBreadth(t, host, "burn"))

	path := checkTraceCPU(t, h, pid)
	checkTraceGC(t, h, pid)
	checkTraceFile(t, h, path)
	checkTraceOut(t, h, pid)
	checkTracePrune(t, h, pid)
	checkTraceUnreadable(t, h, path)
}

// checkTraceCPU is case 1: a cpu trace in the private directory, 0600 in a
// 0700 directory (Unix), Burn.Spin among the top 5 exclusive, a GC line,
// and no scratch file left. It returns the trace's path.
func checkTraceCPU(t *testing.T, h *harness, pid string) string {
	t.Helper()

	r := h.run("dotnet", "trace", "--pid", pid, "--duration", "2s", "--json").wantCode(exitOK)

	var tr traceJSON

	r.decode(&tr)

	dir := filepath.Join(h.homeDir, "traces")
	if tr.Schema != 1 || tr.Profile != "cpu" || tr.EndReason != "duration" || !tr.Trace.Private || !tr.Trace.Taken ||
		filepath.Dir(tr.Trace.Path) != dir || !traceNameRE.MatchString(filepath.Base(tr.Trace.Path)) {
		t.Fatalf("trace = %+v, want a cpu trace in %s", tr, dir)
	}

	if !tr.hotInTop5() || tr.GC == nil || tr.GC.Collections < 1 {
		t.Errorf("%s not among the top 5 exclusive, or no GC; text output:\n%s", hotMethod, h.run("dotnet", "trace", tr.Trace.Path).stdout)
	}

	checkTraceFileModes(t, tr.Trace.Path, tr.Trace.Bytes)

	if files := h.traceDirFiles(); !slices.Equal(files, []string{filepath.Base(tr.Trace.Path)}) {
		t.Errorf("traces directory %v, want only the trace", files)
	}

	return tr.Trace.Path
}

// checkTraceFileModes checks a private trace has its reported size, and
// on Unix mode 0600 in a 0700 directory.
func checkTraceFileModes(t *testing.T, path string, size int64) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil || info.Size() != size || size == 0 {
		t.Fatalf("trace file: %v, %v; want %d bytes", info, err, size)
	}

	if runtime.GOOS != "windows" {
		dirInfo, _ := os.Stat(filepath.Dir(path))
		if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
			t.Errorf("modes: trace %v, directory %v; want 0600, 0700", info.Mode().Perm(), dirInfo.Mode().Perm())
		}
	}
}

// checkTraceGC is case 2: a gc trace has gen0 collections and System.Byte[]
// among the allocated types, and no CPU section.
func checkTraceGC(t *testing.T, h *harness, pid string) {
	t.Helper()

	var tr traceJSON

	h.run("dotnet", "trace", "--pid", pid, "--profile", "gc", "--duration", "2s", "--json").wantCode(exitOK).decode(&tr)

	if tr.Profile != "gc" || tr.CPU != nil || tr.GC == nil || tr.GC.Gen0 < 1 || tr.Allocations == nil ||
		!slices.ContainsFunc(tr.Allocations.Types, func(ty allocatedType) bool { return ty.Name == "System.Byte[]" }) {
		t.Errorf("gc trace: profile %q, cpu %v, gc %+v, allocations %+v; want gen0 and System.Byte[]", tr.Profile, tr.CPU != nil, tr.GC, tr.Allocations)
	}
}

// checkTraceFile is case 3: summarizing case 1's file finds Burn.Spin
// again; the file is eyedbg's own, not taken by this command.
func checkTraceFile(t *testing.T, h *harness, path string) {
	t.Helper()

	var tr traceJSON

	h.run("dotnet", "trace", path, "--json").wantCode(exitOK).decode(&tr)

	if !tr.hotInTop5() || !tr.Trace.Private || tr.Trace.Taken || tr.Target != nil || tr.Trace.Path != path {
		t.Errorf("trace FILE = %+v (hot in top 5: %v)", tr.Trace, tr.hotInTop5())
	}

	h.run("dotnet", "trace", path, "--top", "3").wantCode(exitOK).wantStdout("hottest methods")

	if files := h.traceDirFiles(); slices.ContainsFunc(files, func(n string) bool { return strings.Contains(n, ".etlx") }) {
		t.Errorf("a scratch file stayed: %v", files)
	}
}

// checkTraceOut is case 4: --out gets the trace and the private directory
// nothing; the same --out again is refused.
func checkTraceOut(t *testing.T, h *harness, pid string) {
	t.Helper()

	before := h.traceDirFiles()
	out := filepath.Join(t.TempDir(), "x.nettrace")

	h.run("dotnet", "trace", "--pid", pid, "--duration", "1s", "--out", out).wantCode(exitOK).wantStdout(out)

	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("--out file: %v, %v", info, err)
	}

	if after := h.traceDirFiles(); !slices.Equal(after, before) {
		t.Errorf("traces directory %v, want %v", after, before)
	}

	h.run("dotnet", "trace", "--pid", pid, "--duration", "1s", "--out", out).wantCode(exitError).wantStderr("[INVALID_REQUEST]")
}

// checkTracePrune is case 5: an eyedbg trace older than 7 days and a
// scratch file older than an hour go at the next trace; another file stays.
func checkTracePrune(t *testing.T, h *harness, pid string) {
	t.Helper()

	dir := filepath.Join(h.homeDir, "traces")
	old := filepath.Join(dir, "1-20000101T000000Z-cpu-00000000.nettrace")
	scratch := filepath.Join(dir, "1-20000101T000000Z-file-00000000.etlx")
	notes := filepath.Join(dir, "notes.txt")

	for p, age := range map[string]time.Duration{old: 8 * 24 * time.Hour, scratch: 2 * time.Hour, notes: 8 * 24 * time.Hour} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age)); err != nil {
			t.Fatal(err)
		}
	}

	h.run("dotnet", "trace", "--pid", pid, "--duration", "1s").wantCode(exitOK)

	for p, gone := range map[string]bool{old: true, scratch: true, notes: false} {
		if _, err := os.Stat(p); os.IsNotExist(err) != gone {
			t.Errorf("%s: gone = %v, want %v", filepath.Base(p), os.IsNotExist(err), gone)
		}
	}
}

// checkTraceUnreadable is case 6: a text file and the first 4 KiB of a
// real trace can't be summarized.
func checkTraceUnreadable(t *testing.T, h *harness, path string) {
	t.Helper()

	text := filepath.Join(t.TempDir(), "notes.nettrace")
	if err := os.WriteFile(text, []byte("not a trace\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.run("dotnet", "trace", text).wantCode(exitState).wantStderr("[TRACE_UNSUPPORTED]")

	b, err := os.ReadFile(path)
	if err != nil || len(b) <= 4096 {
		t.Fatalf("read %s: %d bytes, %v", path, len(b), err)
	}

	cut := filepath.Join(t.TempDir(), "cut.nettrace")
	if err := os.WriteFile(cut, b[:4096], 0o600); err != nil { //nolint:gosec // cut is in the test's temporary directory.
		t.Fatal(err)
	}

	h.run("dotnet", "trace", cut).wantCode(exitState).wantStderr("[TRACE_UNSUPPORTED]")
}

// TestDotnetTraceSession is case 7: a program stopped at a breakpoint
// can't be traced; once it runs again it can, and the output names the
// session; it runs on to a clean exit afterwards.
func TestDotnetTraceSession(t *testing.T) {
	t.Parallel()

	_, h := requireDotnetSDK(t)
	requireDotnetE2E(t)

	src := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(breadthDLL)))), "Burn.cs")
	marker := filepath.Join(t.TempDir(), "marker")

	var start snapshotEnvelope

	h.run("start", "dotnet", "--program", breadthDLL, "--bp", src+`@"total += Spin(100_000);"`, "--timeout", startTimeout, "--json",
		"--", "burn", marker).wantCode(exitOK).decode(&start)

	if start.Session.State != "stopped" || start.Session.PID <= 0 {
		t.Fatalf("start = %+v, want stopped at the breakpoint with a pid", start.Session)
	}

	h.run("dotnet", "trace").wantCode(exitState).wantStderr("[NOT_RUNNING]")

	if files := h.traceDirFiles(); len(files) != 0 {
		t.Errorf("a refused trace left %v", files)
	}

	h.run("bp", "rm", "all").wantCode(exitOK)
	h.run("continue", "--timeout", "1s").wantCode(exitOK) // still running when it returns

	var tr traceJSON

	h.run("dotnet", "trace", "--duration", "2s", "--json").wantCode(exitOK).decode(&tr)

	if tr.Target == nil || tr.Target.Session != start.Session.ID || tr.Target.PID != start.Session.PID || tr.CPU == nil || tr.CPU.Managed == 0 {
		t.Errorf("trace of the session: target %+v, cpu %v", tr.Target, tr.CPU != nil)
	}

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var end snapshotEnvelope

	h.run("wait", "--timeout", startTimeout, "--json").wantCode(exitOK).decode(&end)

	if end.Session.State != "exited" || end.Session.ExitCode == nil || *end.Session.ExitCode != 0 {
		t.Errorf("after the marker: %+v, want exited 0", end.Session)
	}
}

// TestDotnetTraceNotDotnet is case 8: this Go test process has no .NET
// diagnostics endpoint, and nothing is left behind.
func TestDotnetTraceNotDotnet(t *testing.T) {
	t.Parallel()

	_, h := requireDotnetSDK(t)

	h.run("dotnet", "trace", "--pid", strconv.Itoa(os.Getpid())).wantCode(exitState).wantStderr("[NOT_DOTNET]")

	if files := h.traceDirFiles(); len(files) != 0 {
		t.Errorf("a refused trace left %v", files)
	}
}
