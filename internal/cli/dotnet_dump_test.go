// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
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

// existingFiles makes a directory with a file, a dangling symlink (where
// symlinks work) and a subdirectory, and returns it.
func existingFiles(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file.dmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling.dmp"))

	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestDumpPlan(t *testing.T) {
	t.Parallel()

	dir := existingFiles(t)
	_, symlinks := os.Lstat(filepath.Join(dir, "dangling.dmp"))

	tests := []struct {
		name     string
		o        dumpOptions
		code     api.Code
		contains string
	}{
		{"defaults", dumpOptions{typ: "heap", timeout: time.Minute}, "", ""},
		{"pid", dumpOptions{pid: 7, typ: "full", timeout: time.Minute}, "", ""},
		{"out new", dumpOptions{typ: "mini", timeout: time.Minute, out: filepath.Join(dir, "new.dmp")}, "", ""},
		{"negative pid", dumpOptions{pid: -1, typ: "heap", timeout: time.Minute}, api.CodeInvalidRequest, "--pid must be positive"},
		{"pid and session", dumpOptions{pid: 1, typ: "heap", timeout: time.Minute, sessionSet: true}, api.CodeInvalidRequest, "--pid and -s"},
		{"bad type", dumpOptions{typ: "Heap", timeout: time.Minute}, api.CodeInvalidRequest, `--type must be heap, mini, triage or full, not "Heap"`},
		{"zero timeout", dumpOptions{typ: "heap"}, api.CodeInvalidRequest, "--timeout must be more than 0 and at most 1h0m0s, not 0s"},
		{"timeout 1h", dumpOptions{typ: "full", timeout: time.Hour}, "", ""},
		{"timeout over 1h", dumpOptions{typ: "full", timeout: 2 * time.Hour}, api.CodeInvalidRequest, "--timeout must be more than 0 and at most 1h0m0s, not 2h0m0s"},
		{"out exists", dumpOptions{typ: "heap", timeout: time.Minute, out: filepath.Join(dir, "file.dmp")}, api.CodeInvalidRequest, "exists; eyedbg never overwrites"},
		{"out is a directory", dumpOptions{typ: "heap", timeout: time.Minute, out: filepath.Join(dir, "sub")}, api.CodeInvalidRequest, "exists"},
		{"out parent missing", dumpOptions{typ: "heap", timeout: time.Minute, out: filepath.Join(dir, "no", "x.dmp")}, api.CodeInvalidRequest, "isn't a directory"},
		{"out parent a file", dumpOptions{typ: "heap", timeout: time.Minute, out: filepath.Join(dir, "file.dmp", "x.dmp")}, api.CodeInvalidRequest, "isn't a directory"},
	}

	if symlinks == nil {
		tests = append(tests, struct {
			name     string
			o        dumpOptions
			code     api.Code
			contains string
		}{"out dangling symlink", dumpOptions{typ: "heap", timeout: time.Minute, out: filepath.Join(dir, "dangling.dmp")}, api.CodeInvalidRequest, "exists"})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := tt.o.plan()
			if tt.code != "" {
				requireCode(t, err, tt.code, tt.contains)

				return
			}

			if err != nil || p.typ != tt.o.typ || p.pid != tt.o.pid || p.out != tt.o.out {
				t.Errorf("plan = %+v, %v", p, err)
			}
		})
	}
}

func TestHeapPlan(t *testing.T) {
	t.Parallel()

	dir := existingFiles(t)
	file := filepath.Join(dir, "file.dmp")
	heap := func(mod func(*heapOptions)) heapOptions {
		o := heapOptions{analysisOptions: analysisOptions{dumpType: "heap", timeout: time.Minute}, top: 20, paths: 3}
		mod(&o)

		return o
	}

	tests := []struct {
		name     string
		o        heapOptions
		code     api.Code
		contains string
		check    func(t *testing.T, p heapPlan)
	}{
		{"session", heap(func(*heapOptions) {}), "", "", func(t *testing.T, p heapPlan) {
			t.Helper()

			if p.target.file != "" || p.dumpType != "heap" || p.params.Gcroot != nil || p.params.Paths != 1 {
				t.Errorf("plan = %+v", p)
			}
		}},
		{"file", heap(func(o *heapOptions) { o.file = file }), "", "", func(t *testing.T, p heapPlan) {
			t.Helper()

			if p.target.file != file {
				t.Errorf("file = %q", p.target.file)
			}
		}},
		{"gcroot type", heap(func(o *heapOptions) { o.gcroot, o.paths, o.pathsSet = "MyApp.Order", 5, true }), "", "", func(t *testing.T, p heapPlan) {
			t.Helper()

			if p.params.Gcroot == nil || p.params.Gcroot.Type != "MyApp.Order" || p.params.Paths != 5 {
				t.Errorf("params = %+v", p.params)
			}
		}},
		{"gcroot address", heap(func(o *heapOptions) { o.gcroot = "0X7F3A1C02E8" }), "", "", func(t *testing.T, p heapPlan) {
			t.Helper()

			if p.params.Gcroot == nil || p.params.Gcroot.Address != "0x7f3a1c02e8" {
				t.Errorf("params = %+v", p.params)
			}
		}},
		{"full dump", heap(func(o *heapOptions) { o.dumpType, o.dumpTypeSet = "full", true }), "", "", nil},
		{"file and pid", heap(func(o *heapOptions) { o.file, o.pid = file, 3 }), api.CodeInvalidRequest, "a DUMP file and --pid", nil},
		{"file and session", heap(func(o *heapOptions) { o.file, o.sessionSet = file, true }), api.CodeInvalidRequest, "a DUMP file and --pid or -s", nil},
		{"pid and session", heap(func(o *heapOptions) { o.pid, o.sessionSet = 3, true }), api.CodeInvalidRequest, "--pid and -s", nil},
		{"missing file", heap(func(o *heapOptions) { o.file = filepath.Join(dir, "none.dmp") }), api.CodeInvalidRequest, "no dump file at", nil},
		{"directory", heap(func(o *heapOptions) { o.file = filepath.Join(dir, "sub") }), api.CodeInvalidRequest, "isn't a file", nil},
		{"dump type with file", heap(func(o *heapOptions) { o.file, o.dumpType, o.dumpTypeSet = file, "heap", true }), api.CodeInvalidRequest, "--dump-type is for a process", nil},
		{"mini dump for heap", heap(func(o *heapOptions) { o.dumpType, o.dumpTypeSet = "mini", true }), api.CodeInvalidRequest, `--dump-type must be heap or full, not "mini"`, nil},
		{"top 0", heap(func(o *heapOptions) { o.top = 0 }), api.CodeInvalidRequest, "--top must be 1 to 1000", nil},
		{"top 1001", heap(func(o *heapOptions) { o.top = 1001 }), api.CodeInvalidRequest, "--top must be 1 to 1000", nil},
		{"paths 21", heap(func(o *heapOptions) { o.gcroot, o.paths = "T", 21 }), api.CodeInvalidRequest, "--paths must be 1 to 20", nil},
		{"paths without gcroot", heap(func(o *heapOptions) { o.paths, o.pathsSet = 2, true }), api.CodeInvalidRequest, "--paths goes with --gcroot", nil},
		{"bad address", heap(func(o *heapOptions) { o.gcroot = "0xzz" }), api.CodeInvalidRequest, "an address is 0x", nil},
		{"long address", heap(func(o *heapOptions) { o.gcroot = "0x" + strings.Repeat("f", 17) }), api.CodeInvalidRequest, "an address is 0x", nil},
		{"zero timeout", heap(func(o *heapOptions) { o.timeout = 0 }), api.CodeInvalidRequest, "--timeout must be more than 0", nil},
		{"timeout over 1h", heap(func(o *heapOptions) { o.timeout = 90 * time.Minute }), api.CodeInvalidRequest, "at most 1h0m0s, not 1h30m0s", nil},
		{"timeout 1h", heap(func(o *heapOptions) { o.timeout = time.Hour }), "", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			checkHeapPlan(t, tt.o, tt.code, tt.contains, tt.check)
		})
	}
}

// checkHeapPlan checks o's plan fails with code, or passes check.
func checkHeapPlan(t *testing.T, o heapOptions, code api.Code, contains string, check func(t *testing.T, p heapPlan)) {
	t.Helper()

	p, err := o.plan()
	if code != "" {
		requireCode(t, err, code, contains)

		return
	}

	if err != nil {
		t.Fatal(err)
	}

	if check != nil {
		check(t, p)
	}
}

func TestThreadsPlan(t *testing.T) {
	t.Parallel()

	threads := []struct {
		o        threadsOptions
		code     api.Code
		contains string
	}{
		{threadsOptions{analysisOptions{dumpType: "heap", timeout: time.Minute}, 10}, "", ""},
		{threadsOptions{analysisOptions{dumpType: "mini", dumpTypeSet: true, timeout: time.Minute}, 64}, "", ""},
		{threadsOptions{analysisOptions{dumpType: "full", dumpTypeSet: true, timeout: time.Minute}, 10}, "", ""},
		{threadsOptions{analysisOptions{dumpType: "triage", dumpTypeSet: true, timeout: time.Minute}, 10}, api.CodeInvalidRequest, `heap, mini or full, not "triage"`},
		{threadsOptions{analysisOptions{dumpType: "heap", timeout: time.Minute}, 65}, api.CodeInvalidRequest, "--frames must be 1 to 64"},
		{threadsOptions{analysisOptions{dumpType: "heap", timeout: time.Minute}, 0}, api.CodeInvalidRequest, "--frames must be 1 to 64"},
		{threadsOptions{analysisOptions{dumpType: "heap", timeout: 61 * time.Minute}, 10}, api.CodeInvalidRequest, "--timeout must be more than 0 and at most 1h0m0s"},
	}

	for i, tt := range threads {
		t.Run("threads/"+strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()

			p, err := tt.o.plan()
			if tt.code != "" {
				requireCode(t, err, tt.code, tt.contains)

				return
			}

			if err != nil || p.frames != tt.o.frames || p.dumpType != tt.o.dumpType {
				t.Errorf("plan = %+v, %v", p, err)
			}
		})
	}
}

func TestResolveSessionTargetAllowStopped(t *testing.T) {
	t.Parallel()

	session := func(state api.SessionState, pid int) api.Snapshot {
		return api.Snapshot{Session: api.SessionInfo{ID: "s-1", Lang: dotnet.Language, State: state, PID: pid}}
	}

	tests := []struct {
		name  string
		snap  api.Snapshot
		allow bool
		code  api.Code
	}{
		{"stopped allowed", session(api.StateStopped, 11), true, ""},
		{"stopped refused", session(api.StateStopped, 11), false, api.CodeNotRunning},
		{"running", session(api.StateRunning, 11), true, ""},
		{"stopped without a pid", session(api.StateStopped, 0), true, api.CodeNotRunning},
		{"starting", session(api.StateStarting, 0), true, api.CodeNotRunning},
		{"exited", session(api.StateExited, 11), true, api.CodeSessionExited},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveSessionTarget(t.Context(), tt.snap, fakeLookup(testProcs), tt.allow)
			if tt.code != "" {
				requireCode(t, err, tt.code)

				return
			}

			if err != nil || got != (dotnetTarget{PID: 11, Name: "api", Session: "s-1"}) {
				t.Errorf("resolveSessionTarget = %+v, %v", got, err)
			}
		})
	}
}

func TestFormatBytesAndCounts(t *testing.T) {
	t.Parallel()

	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 414933: "405.2 KiB", 358305992: "341.7 MiB", 7730941132: "7.2 GiB",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}

	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 5332: "5,332", 1234567: "1,234,567", -4500: "-4,500"} {
		if got := formatCount(n); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestShellArg(t *testing.T) {
	t.Parallel()

	sep := string(filepath.Separator)
	for in, want := range map[string]string{
		sep + "home" + sep + "u" + sep + "4321-x.dmp": sep + "home" + sep + "u" + sep + "4321-x.dmp",
		"/a b/it's.dmp":  `'/a b/it'\''s.dmp'`,
		"/a/$HOME.dmp":   "'/a/$HOME.dmp'",
		"/a/\x1b[2J.dmp": "'/a/?[2J.dmp'",
	} {
		if got := shellArg(in); got != want {
			t.Errorf("shellArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHelperTimeoutMs(t *testing.T) {
	t.Parallel()

	now := time.Now()
	for left, want := range map[time.Duration]int64{
		5 * time.Minute: (5*time.Minute - 2*time.Second).Milliseconds(),
		3 * time.Second: 1000,
		time.Second:     1000,
		0:               1000,
	} {
		if got := helperTimeoutMs(now.Add(left), now); got != want {
			t.Errorf("helperTimeoutMs(%s left) = %d, want %d", left, got, want)
		}
	}
}

// fakeResult decodes one of helpertest's fixed results.
func fakeResult[T any](t *testing.T, v map[string]any) T {
	t.Helper()

	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}

	return out
}

func TestDumpGoldens(t *testing.T) {
	t.Parallel()

	target := dotnetTarget{PID: 4321, Name: "dotnet", Session: "s-k3f9"}
	private := "/home/u/.eyedbg/dumps/4321-20260925T150300Z-heap-1f3a9c0e.dmp"
	dump := dumpOutput{Schema: 1, Target: target, Type: "heap", Path: private, Bytes: 358305992, Private: true, ElapsedMs: 912}
	outDump := dump
	outDump.Target, outDump.Path, outDump.Private = dotnetTarget{PID: 77, Name: "evil\x1b[2J"}, "/tmp/my dumps/app.dmp", false

	heap := fakeResult[dotnet.HeapResult](t, helpertest.FakeHeap())
	noRoots := heap
	noRoots.Gcroot = nil
	threads := fakeResult[dotnet.ThreadsResult](t, helpertest.FakeThreads())
	mini := threads
	mini.Locks, mini.LocksAvailable = []dotnet.DumpLock{}, false

	live := dumpRef{Path: private, Private: true, Taken: true}
	file := dumpRef{Path: "/tmp/app.dmp"}
	heapParams := dotnet.HeapParams{Top: 20}
	gcrootParams := dotnet.HeapParams{Top: 4, TypeFilter: "Retained", Gcroot: &dotnet.GcrootParams{Type: "Retained"}, Paths: 3}

	tests := []struct {
		golden string
		write  func(w *bytes.Buffer) error
	}{
		{"dotnet_dump.golden", func(w *bytes.Buffer) error {
			if err := writeDump(w, dump, false); err != nil {
				return err
			}

			return writeDump(w, outDump, false)
		}},
		{"dotnet_dump_json.golden", func(w *bytes.Buffer) error { return writeDump(w, dump, true) }},
		{"dotnet_heap.golden", func(w *bytes.Buffer) error {
			if err := writeHeap(w, heapOutput{Schema: 1, Target: &target, Dump: live, HeapResult: noRoots}, heapParams, false); err != nil {
				return err
			}

			empty := noRoots
			empty.TypeCount, empty.Types, empty.TypesOmitted = 0, []dotnet.TypeStat{}, 0

			return writeHeap(w, heapOutput{Schema: 1, Dump: file, HeapResult: empty}, dotnet.HeapParams{Top: 20, TypeFilter: "Nope"}, false)
		}},
		{"dotnet_heap_json.golden", func(w *bytes.Buffer) error {
			return writeHeap(w, heapOutput{Schema: 1, Target: &target, Dump: live, HeapResult: heap}, gcrootParams, true)
		}},
		{"dotnet_heap_gcroot.golden", func(w *bytes.Buffer) error {
			if err := writeHeap(w, heapOutput{Schema: 1, Dump: file, HeapResult: heap}, gcrootParams, false); err != nil {
				return err
			}

			none := heap
			none.Gcroot = &dotnet.GcrootResult{Target: dotnet.GcrootTarget{Type: "System.Object", Address: "0x1234"}, Paths: []dotnet.RootPath{}, Complete: true}

			return writeHeap(w, heapOutput{Schema: 1, Dump: file, HeapResult: none}, gcrootParams, false)
		}},
		{"dotnet_threads.golden", func(w *bytes.Buffer) error {
			if err := writeThreads(w, threadsOutput{Schema: 1, Target: &target, Dump: live, ThreadsResult: threads}, false); err != nil {
				return err
			}

			return writeThreads(w, threadsOutput{Schema: 1, Dump: file, ThreadsResult: mini}, false)
		}},
		{"dotnet_threads_json.golden", func(w *bytes.Buffer) error {
			return writeThreads(w, threadsOutput{Schema: 1, Dump: file, ThreadsResult: threads}, true)
		}},
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

// helperCalls reads the fake helper's call log: method and params per call.
func helperCalls(t *testing.T, path string) []string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// privateDumpFiles lists the dumps directory under $EYEDBG_HOME.
func privateDumpFiles(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(os.Getenv(adapters.EnvHome), artifacts.Dumps))
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// TestDotnetDumpWithFakeHelper runs dump, heap and threads against the fake
// helper, with $EYEDBG_HOME in the test's directory. Not parallel: it sets
// environment variables.
func TestDotnetDumpWithFakeHelper(t *testing.T) {
	isolate(t)
	useFakeHelper(t, helpertest.ModeOK)

	calls := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv(helpertest.EnvCalls, calls)

	self := strconv.Itoa(os.Getpid())
	private := checkPrivateDump(t, self)
	out := checkDumpOut(t, self)

	checkLiveAnalysis(t, self, calls)
	checkDumpFiles(t, self, calls, private, out)

	// $EYEDBG_SESSION doesn't apply with a DUMP or --pid.
	t.Setenv(envSession, "s-nope")
	expectOutput(t, run(t, 0, "dotnet", "threads", private), "threads of dump")
	expectOutput(t, run(t, exitError, "dotnet", "threads", private, "-s", "s-nope"), "a DUMP file and --pid or -s/--session both name a target")
}

// checkPrivateDump runs dump: a fresh name in the private directory, 0700
// and 0600. It returns the dump's path.
func checkPrivateDump(t *testing.T, self string) string {
	t.Helper()

	var d dumpOutput

	decodeJSON(t, run(t, 0, "dotnet", "dump", "--pid", self, "--json"), &d)

	if filepath.Dir(d.Path) != filepath.Join(os.Getenv(adapters.EnvHome), "dumps") || !artifacts.IsDumpName(filepath.Base(d.Path)) ||
		artifacts.DumpType(filepath.Base(d.Path)) != "heap" || !d.Private || d.Bytes != int64(len(helpertest.DumpContent)) {
		t.Fatalf("dump --json = %+v", d)
	}

	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{d.Path: 0o600, filepath.Dir(d.Path): 0o700} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: mode %v, %v; want %v", path, info.Mode().Perm(), err, want)
			}
		}
	}

	expectOutput(t, run(t, 0, "dotnet", "dump", "--pid", self, "--type", "mini"),
		"dump of pid "+self+" (", ": mini, 10 B in 0s\n", "private to you, removed after 7 days", "analyze: eyedbg dotnet heap ")

	return d.Path
}

// requireCalls checks the fake helper's call log since the last reset:
// one entry per want, each containing its strings; then resets it.
func requireCalls(t *testing.T, calls string, want ...[]string) {
	t.Helper()

	log := helperCalls(t, calls)
	if len(want) == 0 && (len(log) != 1 || log[0] != "") {
		t.Errorf("the helper ran: %v", log)
	}

	if len(want) > 0 && len(log) != len(want) {
		t.Errorf("helper calls:\n%s", strings.Join(log, "\n"))
	}

	for i := range min(len(log), len(want)) {
		for _, w := range want[i] {
			if !strings.Contains(log[i], w) {
				t.Errorf("helper call %d lacks %q: %s", i, w, log[i])
			}
		}
	}

	_ = os.Remove(calls)
}

// checkLiveAnalysis runs heap and threads of a live process: a dump, then
// the analysis of that dump, trusted.
func checkLiveAnalysis(t *testing.T, self, calls string) {
	t.Helper()

	_ = os.Remove(calls)

	var h heapOutput

	decodeJSON(t, run(t, 0, "dotnet", "heap", "--pid", self, "--gcroot", "Retained", "--json"), &h)

	if h.Target == nil || h.Target.PID != os.Getpid() || !h.Dump.Taken || !h.Dump.Private || h.Objects != 5332 || h.Gcroot == nil {
		t.Errorf("heap --json = %+v", h)
	}

	requireCalls(t, calls, []string{"dump ", `"type":"heap"`},
		[]string{"heap ", `"trustRecordedRuntime":true`, `"gcroot":{"type":"Retained"}`, `"path":` + strconv.Quote(h.Dump.Path)})

	expectOutput(t, run(t, 0, "dotnet", "threads", "--pid", self, "--dump-type", "mini"),
		"threads of pid "+self+" (", "locks:\n  System.Object 0x13de6eb78 held by thread 4, 1 waiting",
		"threads 7, 8, 9 (3 threads, background, threadpool):", "same dump: eyedbg dotnet heap ")
	requireCalls(t, calls, []string{"dump ", `"type":"mini"`}, []string{"threads ", `"trustRecordedRuntime":true`, `"frames":10`})
}

// checkDumpFiles runs heap and threads on DUMP files: eyedbg's own
// (trusted), another (not), and a private mini dump to heap (refused
// before any helper runs).
func checkDumpFiles(t *testing.T, self, calls, private, other string) {
	t.Helper()

	expectOutput(t, run(t, 0, "dotnet", "heap", private), "heap of dump "+private+" (.NET 10.0.12, workstation GC)")
	expectOutput(t, run(t, 0, "dotnet", "threads", other), "threads of dump "+other)
	requireCalls(t, calls, []string{`"trustRecordedRuntime":true`}, []string{`"trustRecordedRuntime":false`})

	var mini dumpOutput

	decodeJSON(t, run(t, 0, "dotnet", "dump", "--pid", self, "--type", "mini", "--json"), &mini)
	_ = os.Remove(calls)

	expectOutput(t, run(t, exitError, "dotnet", "heap", mini.Path), "a mini dump has no heap to analyze [INVALID_REQUEST]")
	requireCalls(t, calls)
}

// checkDumpOut runs dump --out: the dump is placed there, nothing is left
// in the private directory, and an existing --out is refused. It returns
// the --out path.
func checkDumpOut(t *testing.T, self string) string {
	t.Helper()

	before := privateDumpFiles(t)
	out := filepath.Join(t.TempDir(), "x.dmp")
	text := run(t, 0, "dotnet", "dump", "--pid", self, "--out", out)
	expectOutput(t, text, "\n"+out+"\nanalyze: eyedbg dotnet heap ")

	if strings.Contains(text, "private to you") {
		t.Errorf("--out output mentions retention:\n%s", text)
	}

	if b, err := os.ReadFile(out); err != nil || string(b) != helpertest.DumpContent {
		t.Errorf("--out file = %q, %v", b, err)
	}

	if after := privateDumpFiles(t); !slices.Equal(after, before) {
		t.Errorf("private dumps %v, want %v", after, before)
	}

	expectOutput(t, run(t, exitError, "dotnet", "dump", "--pid", self, "--out", out), "exists; eyedbg never overwrites [INVALID_REQUEST]")

	if after := privateDumpFiles(t); !slices.Equal(after, before) {
		t.Errorf("a refused --out left %v, want %v", after, before)
	}

	return out
}

// TestDotnetDumpFailures checks a dump that doesn't arrive, an older helper,
// and pruning. Not parallel: it sets environment variables.
func TestDotnetDumpFailures(t *testing.T) {
	isolate(t)

	self := strconv.Itoa(os.Getpid())

	useFakeHelper(t, helpertest.ModeDumpNoFile)
	expectOutput(t, run(t, exitAdapter, "dotnet", "dump", "--pid", self), "the runtime reported a dump but none is at", "[DUMP_FAILED]", "container")
	expectOutput(t, run(t, exitAdapter, "dotnet", "heap", "--pid", self), "[DUMP_FAILED]")

	useFakeHelper(t, helpertest.ModeOld)
	expectOutput(t, run(t, exitEnvironment, "dotnet", "dump", "--pid", self), `the .NET helper doesn't know "dump" (older than eyedbg?) [HELPER_MISMATCH]`)

	useFakeHelper(t, helpertest.ModeErrorPrefix+"DUMP_UNSUPPORTED")
	expectOutput(t, run(t, exitState, "dotnet", "threads", filepath.Join(existingFiles(t), "file.dmp")), "[DUMP_UNSUPPORTED]")

	useFakeHelper(t, helpertest.ModeErrorPrefix+"DUMP_RUNTIME_MISSING")
	expectOutput(t, run(t, exitEnvironment, "dotnet", "heap", filepath.Join(existingFiles(t), "file.dmp")), "[DUMP_RUNTIME_MISSING]")

	// Pruning: an old eyedbg dump goes, anything else stays; failed dumps left nothing.
	if n := privateDumpFiles(t); len(n) != 0 {
		t.Errorf("failed dumps left %v", n)
	}

	dir, err := artifacts.Dir(artifacts.Dumps)
	if err != nil {
		t.Fatal(err)
	}

	old := filepath.Join(dir, "1-20000101T000000Z-heap-00000000.dmp")
	notes := filepath.Join(dir, "notes.txt")

	for _, p := range []string{old, notes} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chtimes(p, time.Now().Add(-8*24*time.Hour), time.Now().Add(-8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	useFakeHelper(t, helpertest.ModeOK)
	run(t, 0, "dotnet", "dump", "--pid", self)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the old dump is still there: %v", err)
	}

	if _, err := os.Stat(notes); err != nil {
		t.Errorf("notes.txt was removed: %v", err)
	}
}

// TestDotnetDumpSessionTargets checks dump, heap and threads accept a
// stopped session where counters refuses it. The fake session's pid is the
// fake adapter's constant, which may or may not be a process here: either
// way the state check passed. Not parallel: it sets environment variables.
func TestDotnetDumpSessionTargets(t *testing.T) {
	p := isolate(t)
	t.Setenv(dotnet.EnvHelper, filepath.Join(t.TempDir(), "never-started.dll"))

	expectOutput(t, run(t, exitState, "dotnet", "dump"), "[NO_SESSION]", "or pass --pid N")

	serveWithDrivers(t, p, fakeDriver{}, namedDriver{fakeDriver{}, dotnet.Language})

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out := run(t, 0, "start", "dotnet", "--program", prog, "--stop-on-entry", "--timeout", "20s")
	id := strings.Fields(strings.TrimPrefix(out, "session "))[0]

	expectOutput(t, run(t, exitState, "dotnet", "counters", "-s", id), "[NOT_RUNNING]", "is stopped (entry)")

	// The daemon has started its fake adapter: children started from now on
	// don't exist, so the fake helper's mode can be set.
	useFakeHelper(t, helpertest.ModeOK)

	for _, cmd := range []string{"dump", "heap", "threads"} {
		stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"dotnet", cmd, "-s", id})
		if text := string(stdout) + string(stderr); code == exitState || strings.Contains(text, "NOT_RUNNING") {
			t.Errorf("%s refused a stopped session: exit %d\n%s", cmd, code, text)
		}
	}

	run(t, 0, "stop", "-s", id)
}

// TestFileInputTrustsOnlyTheCheckedFile checks that a DUMP's trust and the
// path the helper opens come from one check: whatever path the caller
// resolved, a trusted input always names the private file OwnDump found,
// and an untrusted one never gets trust.
func TestFileInputTrustsOnlyTheCheckedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	own := filepath.Join(dir, "1-20000101T000000Z-heap-00000001.dmp")
	mini := filepath.Join(dir, "1-20000101T000000Z-mini-00000002.dmp")
	foreign := filepath.Join(outside, "3-20000101T000000Z-heap-00000003.dmp")

	for _, p := range []string{own, mini, foreign} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	realOwn, _ := filepath.EvalSymlinks(own)
	noRefusal := func(string) error { return nil }

	// A path that was resolved once and then became a symlink (the race): the
	// input names the private file the check found, not that path.
	swapped := filepath.Join(outside, "swapped.dmp")
	if err := os.Symlink(own, swapped); err != nil {
		t.Skip("no symlinks here")
	}

	tests := []struct {
		name     string
		resolved string
		trusted  bool
		path     string
	}{
		{"private dump", own, true, realOwn},
		{"resolved path became a symlink to a private dump", swapped, true, realOwn},
		{"foreign dump", foreign, false, "/user/given.dmp"},
		{"missing", filepath.Join(outside, "gone.dmp"), false, "/user/given.dmp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in, err := fileInput(dir, "/user/given.dmp", tt.resolved, noRefusal)
			if err != nil {
				t.Fatal(err)
			}

			if in.trusted != tt.trusted || in.path != tt.path || in.ref.Private != tt.trusted || in.ref.Path != "/user/given.dmp" {
				t.Errorf("fileInput = %+v; want trusted %v, path %q", in, tt.trusted, tt.path)
			}
		})
	}

	// The type check reads the checked file's name.
	_, err := fileInput(dir, "/user/given.dmp", mini, func(typ string) error {
		return api.NewError(api.CodeInvalidRequest, "refused "+typ, "")
	})
	requireCode(t, err, api.CodeInvalidRequest, "refused mini")
}

// TestDotnetDumpKeepsTen checks the retention bound counts the new dump:
// with 12 fresh eyedbg dumps already there, a dump leaves the newest 10 —
// the new one among them. Not parallel: it sets environment variables.
func TestDotnetDumpKeepsTen(t *testing.T) {
	isolate(t)
	useFakeHelper(t, helpertest.ModeOK)

	dir, err := artifacts.Dir(artifacts.Dumps)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()

	for i := range 12 {
		p := filepath.Join(dir, fmt.Sprintf("1-20260925T000000Z-heap-%08x.dmp", i))
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		mod := now.Add(-time.Duration(i+1) * time.Minute)
		if err := os.Chtimes(p, mod, mod); err != nil {
			t.Fatal(err)
		}
	}

	var d dumpOutput

	decodeJSON(t, run(t, 0, "dotnet", "dump", "--pid", strconv.Itoa(os.Getpid()), "--json"), &d)

	left := privateDumpFiles(t)
	if len(left) != artifacts.Keep || !slices.Contains(left, filepath.Base(d.Path)) {
		t.Fatalf("%d dumps left (%v), want %d incl. the new %s", len(left), left, artifacts.Keep, filepath.Base(d.Path))
	}

	// The oldest three planted ones went (the new dump counts).
	for i := 9; i < 12; i++ {
		if slices.Contains(left, fmt.Sprintf("1-20260925T000000Z-heap-%08x.dmp", i)) {
			t.Errorf("planted dump %d (older) is still there", i)
		}
	}
}
