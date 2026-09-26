// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
)

// The dump commands' end-to-end tests (docs/adr/0016, P2-M7): the real
// helper against testdata/apps/dotnet/breadth in its 'hold' mode — 5000
// Retained objects kept by the static Holder.Keep, and a lock one thread
// holds while another waits. Assertions are on types, counts and names
// only: dump contents are never printed.

// dumpNameRE is the name eyedbg gives a dump in its private directory.
var dumpNameRE = regexp.MustCompile(`^[0-9]+-[0-9]{8}T[0-9]{6}Z-(heap|mini|triage|full)-[0-9a-f]{8}\.dmp$`)

// dumpJSON is 'eyedbg dotnet dump --json'.
type dumpJSON struct {
	Schema int    `json:"schema"`
	Type   string `json:"type"`
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	// Private: in eyedbg's dumps directory.
	Private bool `json:"private"`
}

// heapJSON is the part of 'eyedbg dotnet heap --json' the tests read.
type heapJSON struct {
	Dump struct {
		Path  string `json:"path"`
		Taken bool   `json:"taken"`
	} `json:"dump"`
	Objects     int64 `json:"objects"`
	Bytes       int64 `json:"bytes"`
	Generations []struct {
		Name  string `json:"name"`
		Bytes int64  `json:"bytes"`
	} `json:"generations"`
	Types  []heapType `json:"types"`
	Gcroot *struct {
		Paths []rootPath `json:"paths"`
	} `json:"gcroot"`
}

// heapType is a row of heap's types.
type heapType struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// rootPath is a path of heap's gcroot.
type rootPath struct {
	Root struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"root"`
	Chain []struct {
		Type string `json:"type"`
	} `json:"chain"`
}

// threadsJSON is the part of 'eyedbg dotnet threads --json' the tests read.
type threadsJSON struct {
	Threads []dumpThread `json:"threads"`
	Locks   []struct {
		Owner   *int `json:"owner"`
		Waiting int  `json:"waiting"`
	} `json:"locks"`
	LocksAvailable bool `json:"locksAvailable"`
}

// dumpThread is a thread of threads' output.
type dumpThread struct {
	ManagedID int         `json:"managedId"`
	Frames    []dumpFrame `json:"frames"`
}

// dumpFrame is a frame of a dumpThread.
type dumpFrame struct {
	Method string `json:"method"`
	File   string `json:"file"`
	Line   int    `json:"line"`
}

// hasFrame says a thread has a frame whose method starts with method.
func (t dumpThread) hasFrame(method string) bool {
	return slices.ContainsFunc(t.Frames, func(f dumpFrame) bool { return strings.HasPrefix(f.Method, method) })
}

// privateDumps lists the dump files in the harness's private directory.
func (h *harness) privateDumps() []string {
	h.t.Helper()

	entries, err := os.ReadDir(filepath.Join(h.homeDir, "dumps"))
	if err != nil {
		h.t.Fatal(err)
	}

	var names []string

	for _, e := range entries {
		if dumpNameRE.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}

	return names
}

// TestE2EHarnessIsolatesHome guards the rule that tests never write into
// the developer's ~/.eyedbg: every eyedbg call gets a temporary
// EYEDBG_HOME.
func TestE2EHarnessIsolatesHome(t *testing.T) {
	t.Parallel()

	h := &harness{t: t, homeDir: t.TempDir(), dataDir: "/data"}

	var home, data string

	for _, kv := range h.env(nil) {
		if v, ok := strings.CutPrefix(kv, adapters.EnvHome+"="); ok {
			home = v
		}

		if v, ok := strings.CutPrefix(kv, adapters.EnvDataDir+"="); ok {
			data = v
		}
	}

	if home != h.homeDir || !strings.HasPrefix(home, filepath.Dir(filepath.Dir(t.TempDir()))) {
		t.Errorf("EYEDBG_HOME = %q, want the test's %q", home, h.homeDir)
	}

	if data != "/data" {
		t.Errorf("EYEDBG_DATA_DIR = %q, want the adapters' own", data)
	}
}

// TestDotnetDumpLive dumps a live process and analyzes the dump: private
// directory, heap and gcroot, threads and locks, live heap, --out, pruning
// and the refusals.
func TestDotnetDumpLive(t *testing.T) {
	t.Parallel()

	host, h := requireDotnetSDK(t)
	pid := strconv.Itoa(startBreadth(t, host, "hold"))

	path := checkDumpPrivate(t, h, pid)
	checkDumpHeap(t, h, path)
	checkDumpThreads(t, h, path)

	h.run("dotnet", "heap", "--pid", pid).wantCode(exitOK).wantStdout("Retained")

	if n := h.privateDumps(); len(n) != 2 {
		t.Errorf("private dumps %v, want 2", n)
	}

	checkDumpOut(t, h, pid)
	checkDumpPrune(t, h, pid)
	checkDumpRefusals(t, h, pid)
}

// checkDumpPrivate is case 1: a heap dump in the private directory, 0600
// in a 0700 directory (Unix), bytes the file's size.
func checkDumpPrivate(t *testing.T, h *harness, pid string) string {
	t.Helper()

	var d dumpJSON

	h.run("dotnet", "dump", "--pid", pid, "--json").wantCode(exitOK).decode(&d)

	dir := filepath.Join(h.homeDir, "dumps")
	if d.Schema != 1 || d.Type != "heap" || !d.Private || filepath.Dir(d.Path) != dir || !dumpNameRE.MatchString(filepath.Base(d.Path)) {
		t.Fatalf("dump = %+v, want a heap dump in %s", d, dir)
	}

	info, err := os.Stat(d.Path)
	if err != nil || info.Size() != d.Bytes || d.Bytes == 0 {
		t.Fatalf("dump file: %v, %v; want %d bytes", info, err, d.Bytes)
	}

	if runtime.GOOS != "windows" {
		dirInfo, _ := os.Stat(dir)
		if info.Mode().Perm() != 0o600 || dirInfo.Mode().Perm() != 0o700 {
			t.Errorf("modes: dump %v, directory %v; want 0600, 0700", info.Mode().Perm(), dirInfo.Mode().Perm())
		}
	}

	return d.Path
}

// checkDumpHeap is case 2: Retained is a top type with 5000 objects, the
// generations add up, and the static Holder.Keep keeps them alive.
func checkDumpHeap(t *testing.T, h *harness, path string) {
	t.Helper()

	var heap heapJSON

	h.run("dotnet", "heap", path, "--json").wantCode(exitOK).decode(&heap)

	i := slices.IndexFunc(heap.Types, func(ty heapType) bool { return ty.Name == "Retained" })
	if i < 0 || heap.Types[i].Count != 5000 {
		t.Errorf("Retained isn't among the top %d types with 5000 objects", len(heap.Types))
	}

	var sum int64
	for _, g := range heap.Generations {
		sum += g.Bytes
	}

	if heap.Objects == 0 || sum != heap.Bytes {
		t.Errorf("generations add up to %d, want %d (%d objects)", sum, heap.Bytes, heap.Objects)
	}

	var roots heapJSON

	h.run("dotnet", "heap", path, "--type", "Retained", "--gcroot", "Retained", "--json").wantCode(exitOK).decode(&roots)

	if roots.Gcroot == nil || !slices.ContainsFunc(roots.Gcroot.Paths, func(p rootPath) bool {
		return p.Root.Kind == "static" && p.Root.Name == "Holder.Keep" && len(p.Chain) > 0 &&
			strings.HasPrefix(p.Chain[0].Type, "System.Collections.Generic.List<Retained>")
	}) {
		t.Errorf("no root path from static Holder.Keep through List<Retained>: %+v", roots.Gcroot)
	}

	h.run("dotnet", "heap", path, "--gcroot", "Retained").wantCode(exitOK).wantStdout("static Holder.Keep → System.Collections.Generic.List<Retained>")
}

// checkDumpThreads is case 3: the main thread and the lock holder's
// frames, with source lines, and the held lock with one waiter.
func checkDumpThreads(t *testing.T, h *harness, path string) {
	t.Helper()

	var th threadsJSON

	h.run("dotnet", "threads", path, "--json").wantCode(exitOK).decode(&th)

	has := func(method string) bool {
		return slices.ContainsFunc(th.Threads, func(t dumpThread) bool { return t.hasFrame(method) })
	}

	if !has("Program.<Main>$") || !has("Hold.Own(") {
		t.Errorf("no main thread or lock holder in %d threads", len(th.Threads))
	}

	lines := 0

	for _, t := range th.Threads {
		for _, f := range t.Frames {
			if (strings.HasSuffix(f.File, "Hold.cs") || strings.HasSuffix(f.File, "Program.cs")) && f.Line > 0 {
				lines++
			}
		}
	}

	// A Windows heap dump lacks the JIT's IL maps (only a full dump has them): no lines there.
	if lines == 0 && runtime.GOOS != "windows" {
		t.Error("no frame with a Hold.cs or Program.cs line")
	}

	if !th.LocksAvailable || len(th.Locks) != 1 || th.Locks[0].Owner == nil || th.Locks[0].Waiting != 1 {
		t.Errorf("locks = %+v (available %v), want one held with 1 waiting", th.Locks, th.LocksAvailable)
	}

	h.run("dotnet", "threads", path).wantCode(exitOK).wantStdout("held by thread ")
}

// checkDumpOut is case 5: --out gets the dump and the private directory
// nothing; an existing --out, or a dangling symlink there, is refused.
func checkDumpOut(t *testing.T, h *harness, pid string) {
	t.Helper()

	before := h.privateDumps()
	out := filepath.Join(t.TempDir(), "x.dmp")

	h.run("dotnet", "dump", "--pid", pid, "--out", out).wantCode(exitOK).wantStdout(out)

	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("--out file: %v, %v", info, err)
	}

	if after := h.privateDumps(); !slices.Equal(after, before) {
		t.Errorf("private dumps %v, want %v", after, before)
	}

	h.run("dotnet", "dump", "--pid", pid, "--out", out).wantCode(exitError).wantStderr("[INVALID_REQUEST]")

	if runtime.GOOS == "windows" {
		return
	}

	target := filepath.Join(t.TempDir(), "target.dmp")
	link := filepath.Join(filepath.Dir(out), "dangling.dmp")

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	h.run("dotnet", "dump", "--pid", pid, "--out", link).wantCode(exitError).wantStderr("[INVALID_REQUEST]")

	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the symlink's target was created: %v", err)
	}
}

// checkDumpPrune is case 6: an eyedbg dump older than 7 days goes at the
// next dump; another file stays.
func checkDumpPrune(t *testing.T, h *harness, pid string) {
	t.Helper()

	dir := filepath.Join(h.homeDir, "dumps")
	old := filepath.Join(dir, "1-20000101T000000Z-heap-00000000.dmp")
	notes := filepath.Join(dir, "notes.txt")
	past := time.Now().Add(-8 * 24 * time.Hour)

	for _, p := range []string{old, notes} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}

	h.run("dotnet", "dump", "--pid", pid, "--type", "mini").wantCode(exitOK)

	if _, err := os.Stat(old); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the 8-day-old dump is still there: %v", err)
	}

	if _, err := os.Stat(notes); err != nil {
		t.Errorf("notes.txt was removed: %v", err)
	}
}

// checkDumpRefusals is case 7: a text file isn't a dump; a mini dump has no
// heap (refused at once when eyedbg's own, by the helper otherwise) but has
// threads.
func checkDumpRefusals(t *testing.T, h *harness, pid string) {
	t.Helper()

	text := filepath.Join(t.TempDir(), "notes.dmp")
	if err := os.WriteFile(text, []byte("not a dump\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	h.run("dotnet", "threads", text).wantCode(exitState).wantStderr("[DUMP_UNSUPPORTED]")

	var mini dumpJSON

	h.run("dotnet", "dump", "--pid", pid, "--type", "mini", "--json").wantCode(exitOK).decode(&mini)
	h.run("dotnet", "heap", mini.Path).wantCode(exitError).wantStderr("a mini dump has no heap to analyze [INVALID_REQUEST]")

	out := filepath.Join(t.TempDir(), "m.dmp")

	h.run("dotnet", "dump", "--pid", pid, "--type", "mini", "--out", out).wantCode(exitOK)
	h.run("dotnet", "heap", out).wantCode(exitState).wantStderr("no managed heap")

	var th threadsJSON

	h.run("dotnet", "threads", out, "--json").wantCode(exitOK).decode(&th)

	if th.LocksAvailable || len(th.Threads) == 0 || len(th.Threads[0].Frames) == 0 {
		t.Errorf("threads of a mini dump: %d threads, locksAvailable %v; want frames and no locks", len(th.Threads), th.LocksAvailable)
	}
}

// TestDotnetDumpSession is case 8 (R4): a program stopped at a breakpoint
// can be dumped and its dump read, counters still refuse it, and it runs
// on to a clean exit afterwards.
func TestDotnetDumpSession(t *testing.T) {
	t.Parallel()

	_, h := requireDotnetSDK(t)
	requireDotnetE2E(t)

	src := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(breadthDLL)))), "Hold.cs")
	marker := filepath.Join(t.TempDir(), "marker")

	var start snapshotEnvelope

	h.run("start", "dotnet", "--program", breadthDLL, "--bp", src+`@"ticks++;"`, "--timeout", startTimeout, "--json",
		"--", "hold", marker).wantCode(exitOK).decode(&start)

	if start.Session.State != "stopped" || start.Session.PID <= 0 {
		t.Fatalf("start = %+v, want stopped at the breakpoint with a pid", start.Session)
	}

	var d dumpJSON

	h.run("dotnet", "dump", "--json").wantCode(exitOK).decode(&d)

	var th threadsJSON

	h.run("dotnet", "threads", d.Path, "--json").wantCode(exitOK).decode(&th)

	if !slices.ContainsFunc(th.Threads, func(t dumpThread) bool { return t.hasFrame("Hold.Run(") }) {
		t.Errorf("no thread in Hold.Run among %d", len(th.Threads))
	}

	h.run("dotnet", "counters").wantCode(exitState).wantStderr("[NOT_RUNNING]")

	h.run("bp", "rm", "all").wantCode(exitOK)

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var end snapshotEnvelope

	h.run("continue", "--timeout", startTimeout, "--json").wantCode(exitOK).decode(&end)

	if end.Session.State != "exited" {
		t.Errorf("after continue: %+v, want exited", end.Session)
	}

	expectE2EExitCode(t, end.Session, 0)
}

// TestDotnetDumpNotDotnet is case 9: this Go test process has no .NET
// diagnostics endpoint.
func TestDotnetDumpNotDotnet(t *testing.T) {
	t.Parallel()

	_, h := requireDotnetSDK(t)

	h.run("dotnet", "dump", "--pid", strconv.Itoa(os.Getpid())).wantCode(exitState).wantStderr("[NOT_DOTNET]")

	if n := h.privateDumps(); len(n) != 0 {
		t.Errorf("a refused dump left %v", n)
	}
}
