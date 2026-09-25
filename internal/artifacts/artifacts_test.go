// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
)

func TestDumpNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		typ  string
	}{
		{"4321-20260925T150300Z-heap-1f3a9c0e.dmp", "heap"},
		{"1-20000101T000000Z-mini-00000000.dmp", "mini"},
		{"1-20000101T000000Z-triage-abcdef01.dmp", "triage"},
		{"1-20000101T000000Z-full-abcdef01.dmp", "full"},
		{"1-20000101T000000Z-heap-ABCDEF01.dmp", ""},
		{"1-20000101T000000Z-heap-abcdef0.dmp", ""},
		{"1-20000101T000000Z-other-abcdef01.dmp", ""},
		{"x-20000101T000000Z-heap-abcdef01.dmp", ""},
		{"1-20000101T000000Z-heap-abcdef01.dmp.bak", ""},
		{"notes.txt", ""},
		{"../1-20000101T000000Z-heap-abcdef01.dmp", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := TypeOf(Dumps, tt.name); got != tt.typ {
				t.Errorf("TypeOf(Dumps, %q) = %q, want %q", tt.name, got, tt.typ)
			}

			if got := IsName(Dumps, tt.name); got != (tt.typ != "") {
				t.Errorf("IsName(Dumps, %q) = %v", tt.name, got)
			}

			if IsName(Traces, tt.name) {
				t.Errorf("IsName(Traces, %q) = true", tt.name)
			}
		})
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 9, 25, 15, 3, 0, 0, time.FixedZone("x", 8*3600))

	name, err := Name(dir, Dumps, 4321, "heap", now, bytes.NewReader([]byte{0x1f, 0x3a, 0x9c, 0x0e}))
	if err != nil || name != "4321-20260925T070300Z-heap-1f3a9c0e.dmp" {
		t.Fatalf("Name = %q, %v", name, err)
	}

	// Taken names (a file, a dangling symlink) are skipped.
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	random := bytes.NewReader([]byte{0x1f, 0x3a, 0x9c, 0x0e, 0, 0, 0, 1})

	got, err := Name(dir, Dumps, 4321, "heap", now, random)
	if err != nil || got != "4321-20260925T070300Z-heap-00000001.dmp" {
		t.Fatalf("Name = %q, %v", got, err)
	}

	// Five taken names in a row: an error, no endless loop.
	same := bytes.NewReader(bytes.Repeat([]byte{0x1f, 0x3a, 0x9c, 0x0e}, nameTries))
	if _, err := Name(dir, Dumps, 4321, "heap", now, same); err == nil {
		t.Fatal("Name with every name taken succeeded")
	}

	if _, err := Name(dir, Dumps, 1, "heap", now, bytes.NewReader(nil)); err == nil {
		t.Fatal("Name without randomness succeeded")
	}
}

func TestDirCreatesAPrivateDirectory(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv(adapters.EnvHome, home)

	dir, err := Dir(Dumps)
	if err != nil {
		t.Fatal(err)
	}

	if dir != filepath.Join(home, "dumps") {
		t.Fatalf("Dir = %q", dir)
	}

	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("Lstat = %v, %v", info, err)
	}

	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700", info.Mode().Perm())
	}
}

func TestDirTightensAnOpenDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}

	home := t.TempDir()
	t.Setenv(adapters.EnvHome, home)

	dir := filepath.Join(home, Dumps)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o757); err != nil { //nolint:gosec // The test needs a directory others can write.
		t.Fatal(err)
	}

	if _, err := Dir(Dumps); err != nil {
		t.Fatal(err)
	}

	if info, _ := os.Stat(dir); info.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v, want 0700", info.Mode().Perm())
	}
}

func TestDirRefusesASymlinkOrAFile(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, dir string)
		want string
	}{
		{"symlink", func(t *testing.T, dir string) {
			t.Helper()

			if err := os.Symlink(t.TempDir(), dir); err != nil {
				t.Skipf("no symlinks here: %v", err)
			}
		}, "is a symlink"},
		{"file", func(t *testing.T, dir string) {
			t.Helper()

			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv(adapters.EnvHome, home)
			tt.make(t, filepath.Join(home, Dumps))

			_, err := Dir(Dumps)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Dir = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// dumpAt writes a dump-named file in dir with the given age.
func dumpAt(t *testing.T, dir, name string, now time.Time, age time.Duration) {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	mod := now.Add(-age)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	name := func(i int) string {
		return fmt.Sprintf("1-20260925T120000Z-heap-%08x.dmp", i)
	}

	tests := []struct {
		name    string
		ages    []time.Duration // of name(0), name(1), …
		keep    int
		protect int // index, or -1
		removed []int
	}{
		{"nothing old", []time.Duration{time.Hour, 2 * time.Hour}, 10, -1, nil},
		{"older than 7 days", []time.Duration{time.Hour, 8 * 24 * time.Hour, 7*24*time.Hour + time.Second}, 10, -1, []int{1, 2}},
		{"exactly 7 days is kept", []time.Duration{7 * 24 * time.Hour}, 10, -1, nil},
		{"beyond keep, oldest go", []time.Duration{time.Minute, 5 * time.Minute, 2 * time.Minute, 4 * time.Minute, 3 * time.Minute}, 3, -1, []int{1, 3}},
		{"protected stays", []time.Duration{time.Minute, 8 * 24 * time.Hour, 2 * time.Minute}, 1, 1, []int{2}},
		{"protected newest counts", []time.Duration{time.Hour, 2 * time.Hour}, 1, 0, []int{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for i, age := range tt.ages {
				dumpAt(t, dir, name(i), now, age)
			}

			protect := ""
			if tt.protect >= 0 {
				protect = filepath.Join(dir, name(tt.protect))
			}

			got := Prune(dir, Dumps, now, MaxAge, tt.keep, protect)

			var want []string
			for _, i := range tt.removed {
				want = append(want, name(i))
			}

			slices.Sort(got)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Fatalf("Prune removed %v, want %v", got, want)
			}

			for i := range tt.ages {
				_, err := os.Lstat(filepath.Join(dir, name(i)))
				if gone := errors.Is(err, os.ErrNotExist); gone != slices.Contains(tt.removed, i) {
					t.Errorf("%s gone = %v", name(i), gone)
				}
			}
		})
	}
}

func TestPruneTouchesOnlyItsOwnRegularFiles(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dir := t.TempDir()
	outside := t.TempDir()
	old := 30 * 24 * time.Hour

	dumpAt(t, dir, "notes.txt", now, old)
	dumpAt(t, dir, "1-20000101T000000Z-heap-0000000z.dmp", now, old) // not hex
	dumpAt(t, outside, "2-20000101T000000Z-heap-00000002.dmp", now, old)

	if err := os.Mkdir(filepath.Join(dir, "3-20000101T000000Z-heap-00000003.dmp"), 0o700); err != nil {
		t.Fatal(err)
	}

	symlinked := os.Symlink(filepath.Join(outside, "2-20000101T000000Z-heap-00000002.dmp"),
		filepath.Join(dir, "2-20000101T000000Z-heap-00000002.dmp")) == nil

	dumpAt(t, dir, "4-20000101T000000Z-heap-00000004.dmp", now, old)

	got := Prune(dir, Dumps, now, MaxAge, Keep, "")
	if !slices.Equal(got, []string{"4-20000101T000000Z-heap-00000004.dmp"}) {
		t.Fatalf("Prune removed %v", got)
	}

	for _, p := range []string{
		filepath.Join(dir, "notes.txt"),
		filepath.Join(dir, "1-20000101T000000Z-heap-0000000z.dmp"),
		filepath.Join(dir, "3-20000101T000000Z-heap-00000003.dmp"),
		filepath.Join(outside, "2-20000101T000000Z-heap-00000002.dmp"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}

	if symlinked {
		if _, err := os.Lstat(filepath.Join(dir, "2-20000101T000000Z-heap-00000002.dmp")); err != nil {
			t.Errorf("the symlink was removed: %v", err)
		}
	}

	if Prune(filepath.Join(dir, "missing"), Dumps, now, MaxAge, Keep, "") != nil {
		t.Error("Prune of a missing directory removed something")
	}
}

func TestOwn(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	own := filepath.Join(dir, "1-20000101T000000Z-heap-00000001.dmp")
	foreign := filepath.Join(outside, "2-20000101T000000Z-heap-00000002.dmp")

	for _, p := range []string{own, foreign, filepath.Join(dir, "notes.dmp")} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	realOwn, err := filepath.EvalSymlinks(own)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := Own(dir, Dumps, own); !ok || got != realOwn {
		t.Errorf("Own(private dump) = %q, %v; want %q, true", got, ok, realOwn)
	}

	for _, p := range []string{foreign, filepath.Join(dir, "notes.dmp"), filepath.Join(dir, "missing.dmp")} {
		if got, ok := Own(dir, Dumps, p); ok || got != "" {
			t.Errorf("Own(%s) = %q, %v; want not own", p, got, ok)
		}
	}

	// A symlink to a private dump is own, and the path returned is the
	// private file itself; a private-looking symlink to a file outside isn't.
	toOwn := filepath.Join(outside, "link.dmp")
	pointingOut := filepath.Join(dir, "3-20000101T000000Z-heap-00000003.dmp")

	if os.Symlink(own, toOwn) != nil || os.Symlink(foreign, pointingOut) != nil {
		t.Skip("no symlinks here")
	}

	if got, ok := Own(dir, Dumps, toOwn); !ok || got != realOwn {
		t.Errorf("Own(symlink to a private dump) = %q, %v; want %q, true", got, ok, realOwn)
	}

	if _, ok := Own(dir, Dumps, pointingOut); ok {
		t.Error("a symlink in the private dir pointing out is own")
	}
}

func TestTraceNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		typ     string
		scratch bool
	}{
		{"4321-20260925T150300Z-cpu-1f3a9c0e.nettrace", "cpu", false},
		{"1-20000101T000000Z-gc-00000000.nettrace", "gc", false},
		{"1-20000101T000000Z-file-00000000.nettrace", "", false},
		{"1-20000101T000000Z-heap-00000000.nettrace", "", false},
		{"1-20000101T000000Z-cpu-00000000.dmp", "", false},
		{"1-20000101T000000Z-cpu-0000000.nettrace", "", false},
		{"1-20000101T000000Z-cpu-00000000.nettrace.etlx", "", false},
		{"1-20000101T000000Z-cpu-00000000.etlx", "", true},
		{"0-20000101T000000Z-file-00000000.etlx", "", true},
		{"1-20000101T000000Z-gc-00000000.etlx.new", "", true},
		{"1-20000101T000000Z-heap-00000000.etlx", "", false},
		{"1-20000101T000000Z-cpu-00000000.etlx.old", "", false},
		{"../1-20000101T000000Z-cpu-00000000.nettrace", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := TypeOf(Traces, tt.name); got != tt.typ {
				t.Errorf("TypeOf(Traces, %q) = %q, want %q", tt.name, got, tt.typ)
			}

			if got := IsName(Traces, tt.name); got != (tt.typ != "") {
				t.Errorf("IsName(Traces, %q) = %v", tt.name, got)
			}

			if got := matches(scratchPattern(Traces), tt.name); got != tt.scratch {
				t.Errorf("scratch %q = %v, want %v", tt.name, got, tt.scratch)
			}

			if IsName(Dumps, tt.name) || matches(scratchPattern(Dumps), tt.name) || IsName("other", tt.name) {
				t.Errorf("%q is a dump name, a dump scratch name or another kind's", tt.name)
			}
		})
	}
}

func TestTraceAndScratchName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 9, 25, 15, 3, 0, 0, time.UTC)
	rnd := func(b ...byte) *bytes.Reader { return bytes.NewReader(b) }

	name, err := Name(dir, Traces, 4321, "cpu", now, rnd(0x1f, 0x3a, 0x9c, 0x0e))
	if err != nil || name != "4321-20260925T150300Z-cpu-1f3a9c0e.nettrace" {
		t.Fatalf("Name(Traces) = %q, %v", name, err)
	}

	// Names Prune wouldn't know are refused, not retried.
	for _, typ := range []string{"file", "heap", "CPU", "../x", ""} {
		if got, err := Name(dir, Traces, 1, typ, now, rnd(0, 0, 0, 1)); err == nil {
			t.Errorf("Name(Traces, %q) = %q", typ, got)
		}
	}

	if got, err := Name(dir, "other", 1, "cpu", now, rnd(0, 0, 0, 1)); err == nil {
		t.Errorf("Name(other kind) = %q", got)
	}

	scratch, err := ScratchName(dir, 0, "file", now, rnd(0, 0, 0, 2))
	if err != nil || scratch != "0-20260925T150300Z-file-00000002.etlx" {
		t.Fatalf("ScratchName = %q, %v", scratch, err)
	}

	// A name whose .new sibling exists is taken: TraceLog would write there.
	if err := os.WriteFile(filepath.Join(dir, scratch+".new"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ScratchName(dir, 0, "file", now, rnd(0, 0, 0, 2, 0, 0, 0, 3))
	if err != nil || got != "0-20260925T150300Z-file-00000003.etlx" {
		t.Fatalf("ScratchName with .new taken = %q, %v", got, err)
	}

	if got, err := ScratchName(dir, 1, "heap", now, rnd(0, 0, 0, 4)); err == nil {
		t.Errorf("ScratchName(heap) = %q", got)
	}
}

func TestDirTraces(t *testing.T) {
	home := t.TempDir()
	t.Setenv(adapters.EnvHome, home)

	dir, err := Dir(Traces)
	if err != nil || dir != filepath.Join(home, "traces") {
		t.Fatalf("Dir(Traces) = %q, %v", dir, err)
	}

	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Fatalf("Lstat = %v, %v", info, err)
	}
}

func TestPruneTraces(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	trace := func(i int) string { return fmt.Sprintf("1-20260925T120000Z-cpu-%08x.nettrace", i) }

	tests := []struct {
		name    string
		traces  []time.Duration
		scratch map[string]time.Duration
		protect int
		removed []string
	}{
		{"old trace goes", []time.Duration{time.Hour, 8 * 24 * time.Hour}, nil, -1, []string{trace(1)}},
		{
			"keep 10, scratch not counted",
			[]time.Duration{1 * time.Minute, 2 * time.Minute, 3 * time.Minute, 4 * time.Minute, 5 * time.Minute, 6 * time.Minute, 7 * time.Minute, 8 * time.Minute, 9 * time.Minute, 10 * time.Minute, 11 * time.Minute},
			map[string]time.Duration{"1-20260925T120000Z-cpu-000000aa.etlx": time.Minute, "0-20260925T120000Z-file-000000ab.etlx.new": time.Minute},
			-1,
			[]string{trace(10)},
		},
		{"protected stays", []time.Duration{8 * 24 * time.Hour}, nil, 0, nil},
		{
			"stale scratch goes",
			nil,
			map[string]time.Duration{
				"1-20260925T120000Z-cpu-000000aa.etlx":      2 * time.Hour,
				"1-20260925T120000Z-gc-000000ab.etlx.new":   time.Hour + time.Second,
				"0-20260925T120000Z-file-000000ac.etlx":     59 * time.Minute,
				"0-20260925T120000Z-file-000000ad.etlx.new": time.Hour,
			},
			-1,
			[]string{"1-20260925T120000Z-cpu-000000aa.etlx", "1-20260925T120000Z-gc-000000ab.etlx.new"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()

			var all []string

			for i, age := range tt.traces {
				dumpAt(t, dir, trace(i), now, age)
				all = append(all, trace(i))
			}

			for name, age := range tt.scratch {
				dumpAt(t, dir, name, now, age)
				all = append(all, name)
			}

			protect := ""
			if tt.protect >= 0 {
				protect = filepath.Join(dir, trace(tt.protect))
			}

			got := Prune(dir, Traces, now, MaxAge, Keep, protect)
			slices.Sort(got)

			want := slices.Clone(tt.removed)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Fatalf("Prune removed %v, want %v", got, want)
			}

			for _, name := range all {
				_, err := os.Lstat(filepath.Join(dir, name))
				if gone := errors.Is(err, os.ErrNotExist); gone != slices.Contains(tt.removed, name) {
					t.Errorf("%s gone = %v", name, gone)
				}
			}
		})
	}
}

func TestPruneKindsStayApart(t *testing.T) {
	t.Parallel()

	now := time.Now()
	dir := t.TempDir()
	outside := t.TempDir()
	old := 30 * 24 * time.Hour

	dump := "1-20000101T000000Z-heap-00000001.dmp"
	trace := "2-20000101T000000Z-cpu-00000002.nettrace"
	scratch := "3-20000101T000000Z-cpu-00000003.etlx"

	for _, n := range []string{dump, trace, scratch, "notes.txt"} {
		dumpAt(t, dir, n, now, old)
	}

	dumpAt(t, outside, "4-20000101T000000Z-gc-00000004.etlx", now, old)

	if err := os.Mkdir(filepath.Join(dir, "5-20000101T000000Z-gc-00000005.etlx"), 0o700); err != nil {
		t.Fatal(err)
	}

	symlinked := os.Symlink(filepath.Join(outside, "4-20000101T000000Z-gc-00000004.etlx"),
		filepath.Join(dir, "4-20000101T000000Z-gc-00000004.etlx")) == nil

	// Pruning dumps never touches traces or scratch files, and the reverse.
	if got := Prune(dir, Dumps, now, MaxAge, Keep, ""); !slices.Equal(got, []string{dump}) {
		t.Fatalf("Prune(Dumps) removed %v", got)
	}

	dumpAt(t, dir, dump, now, old)

	got := Prune(dir, Traces, now, MaxAge, Keep, "")
	slices.Sort(got)

	if !slices.Equal(got, []string{trace, scratch}) {
		t.Fatalf("Prune(Traces) removed %v", got)
	}

	for _, p := range []string{
		filepath.Join(dir, dump),
		filepath.Join(dir, "notes.txt"),
		filepath.Join(dir, "5-20000101T000000Z-gc-00000005.etlx"),
		filepath.Join(outside, "4-20000101T000000Z-gc-00000004.etlx"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}

	if symlinked {
		if _, err := os.Lstat(filepath.Join(dir, "4-20000101T000000Z-gc-00000004.etlx")); err != nil {
			t.Errorf("the symlink was removed: %v", err)
		}
	}
}

func TestOwnTrace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	own := filepath.Join(dir, "1-20000101T000000Z-gc-00000001.nettrace")

	for _, p := range []string{own, filepath.Join(dir, "1-20000101T000000Z-gc-00000002.etlx"), filepath.Join(dir, "1-20000101T000000Z-heap-00000003.dmp")} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	realOwn, err := filepath.EvalSymlinks(own)
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := Own(dir, Traces, own); !ok || got != realOwn {
		t.Errorf("Own(Traces, trace) = %q, %v; want %q, true", got, ok, realOwn)
	}

	if _, ok := Own(dir, Dumps, own); ok {
		t.Error("a trace is an own dump")
	}

	for _, n := range []string{"1-20000101T000000Z-gc-00000002.etlx", "1-20000101T000000Z-heap-00000003.dmp"} {
		if _, ok := Own(dir, Traces, filepath.Join(dir, n)); ok {
			t.Errorf("Own(Traces, %s) = true", n)
		}
	}

	link := filepath.Join(outside, "link.nettrace")
	if os.Symlink(own, link) != nil {
		t.Skip("no symlinks here")
	}

	if got, ok := Own(dir, Traces, link); !ok || got != realOwn {
		t.Errorf("Own(Traces, symlink to a trace) = %q, %v; want %q, true", got, ok, realOwn)
	}
}
