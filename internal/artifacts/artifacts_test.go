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

			if got := DumpType(tt.name); got != tt.typ {
				t.Errorf("DumpType(%q) = %q, want %q", tt.name, got, tt.typ)
			}

			if got := IsDumpName(tt.name); got != (tt.typ != "") {
				t.Errorf("IsDumpName(%q) = %v", tt.name, got)
			}
		})
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	now := time.Date(2026, 9, 25, 15, 3, 0, 0, time.FixedZone("x", 8*3600))

	name, err := Name(dir, 4321, "heap", now, bytes.NewReader([]byte{0x1f, 0x3a, 0x9c, 0x0e}))
	if err != nil || name != "4321-20260925T070300Z-heap-1f3a9c0e.dmp" {
		t.Fatalf("Name = %q, %v", name, err)
	}

	// Taken names (a file, a dangling symlink) are skipped.
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	random := bytes.NewReader([]byte{0x1f, 0x3a, 0x9c, 0x0e, 0, 0, 0, 1})

	got, err := Name(dir, 4321, "heap", now, random)
	if err != nil || got != "4321-20260925T070300Z-heap-00000001.dmp" {
		t.Fatalf("Name = %q, %v", got, err)
	}

	// Five taken names in a row: an error, no endless loop.
	same := bytes.NewReader(bytes.Repeat([]byte{0x1f, 0x3a, 0x9c, 0x0e}, nameTries))
	if _, err := Name(dir, 4321, "heap", now, same); err == nil {
		t.Fatal("Name with every name taken succeeded")
	}

	if _, err := Name(dir, 1, "heap", now, bytes.NewReader(nil)); err == nil {
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

			got := Prune(dir, now, MaxAge, tt.keep, protect)

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

	got := Prune(dir, now, MaxAge, Keep, "")
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

	if Prune(filepath.Join(dir, "missing"), now, MaxAge, Keep, "") != nil {
		t.Error("Prune of a missing directory removed something")
	}
}

func TestOwnDump(t *testing.T) {
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

	if got, ok := OwnDump(dir, own); !ok || got != realOwn {
		t.Errorf("OwnDump(private dump) = %q, %v; want %q, true", got, ok, realOwn)
	}

	for _, p := range []string{foreign, filepath.Join(dir, "notes.dmp"), filepath.Join(dir, "missing.dmp")} {
		if got, ok := OwnDump(dir, p); ok || got != "" {
			t.Errorf("OwnDump(%s) = %q, %v; want not own", p, got, ok)
		}
	}

	// A symlink to a private dump is own, and the path returned is the
	// private file itself; a private-looking symlink to a file outside isn't.
	toOwn := filepath.Join(outside, "link.dmp")
	pointingOut := filepath.Join(dir, "3-20000101T000000Z-heap-00000003.dmp")

	if os.Symlink(own, toOwn) != nil || os.Symlink(foreign, pointingOut) != nil {
		t.Skip("no symlinks here")
	}

	if got, ok := OwnDump(dir, toOwn); !ok || got != realOwn {
		t.Errorf("OwnDump(symlink to a private dump) = %q, %v; want %q, true", got, ok, realOwn)
	}

	if _, ok := OwnDump(dir, pointingOut); ok {
		t.Error("a symlink in the private dir pointing out is own")
	}
}
