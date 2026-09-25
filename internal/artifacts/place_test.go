// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// privateDump writes a dump into a fresh directory and returns its path.
func privateDump(t *testing.T) string {
	t.Helper()

	src := filepath.Join(t.TempDir(), "1-20000101T000000Z-heap-00000001.dmp")
	if err := os.WriteFile(src, []byte("dump bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	return src
}

func TestPlaceLinks(t *testing.T) {
	t.Parallel()

	src := privateDump(t)
	// A second name for src: Windows' os.SameFile reads the file id from
	// the path, which must still exist when it compares.
	keep := src + ".keep"
	if err := os.Link(src, keep); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "x.dmp")

	if err := Place(src, out); err != nil {
		t.Fatal(err)
	}

	before, err := os.Stat(keep)
	if err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(before, after) {
		t.Error("out isn't the same file (no hard link)")
	}

	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("src still there: %v", err)
	}
}

func TestPlaceNeverReplaces(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		make func(t *testing.T, out string) bool // false: can't here
	}{
		{"file", func(t *testing.T, out string) bool {
			t.Helper()

			return os.WriteFile(out, []byte("mine"), 0o600) == nil
		}},
		{"directory", func(t *testing.T, out string) bool {
			t.Helper()

			return os.Mkdir(out, 0o700) == nil
		}},
		{"dangling symlink", func(t *testing.T, out string) bool {
			t.Helper()

			return os.Symlink(filepath.Join(filepath.Dir(out), "target"), out) == nil
		}},
		{"symlink to a file", func(t *testing.T, out string) bool {
			t.Helper()

			target := filepath.Join(filepath.Dir(out), "target")

			return os.WriteFile(target, []byte("mine"), 0o600) == nil && os.Symlink(target, out) == nil
		}},
	}
	for _, tt := range tests {
		for _, via := range []string{"link", "copy"} {
			t.Run(tt.name+"/"+via, func(t *testing.T) {
				t.Parallel()

				src := privateDump(t)
				out := filepath.Join(t.TempDir(), "x.dmp")

				if !tt.make(t, out) {
					t.Skip("can't make it here")
				}

				link := os.Link
				if via == "copy" {
					link = func(string, string) error { return syscall.EXDEV }
				}

				if err := place(src, out, link); !errors.Is(err, ErrExists) {
					t.Fatalf("place = %v, want ErrExists", err)
				}

				requireUntouched(t, src, out, tt.name == "dangling symlink")
			})
		}
	}
}

// requireUntouched checks a refused Place kept src and wrote nothing at out
// or at the "target" next to it.
func requireUntouched(t *testing.T, src, out string, dangling bool) {
	t.Helper()

	if _, err := os.Stat(src); err != nil {
		t.Errorf("src lost: %v", err)
	}

	target := filepath.Join(filepath.Dir(out), "target")

	b, err := os.ReadFile(target)
	switch {
	case dangling && err == nil:
		t.Error("the dangling symlink's target was created")
	case err == nil && string(b) != "mine":
		t.Errorf("the symlink's target was written: %q", b)
	}

	if b, err := os.ReadFile(out); err == nil && string(b) != "mine" {
		t.Errorf("out was written: %q", b)
	}
}

func TestPlaceCopiesAcrossFilesystems(t *testing.T) {
	t.Parallel()

	src := privateDump(t)
	out := filepath.Join(t.TempDir(), "x.dmp")

	if err := place(src, out, func(string, string) error { return syscall.EXDEV }); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(out)
	if err != nil || string(b) != "dump bytes" {
		t.Fatalf("out = %q, %v", b, err)
	}

	if info, _ := os.Stat(out); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}

	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("src still there: %v", err)
	}
}

func TestPlaceFailures(t *testing.T) {
	t.Parallel()

	exdev := func(string, string) error { return syscall.EXDEV }

	t.Run("missing parent", func(t *testing.T) {
		t.Parallel()

		src := privateDump(t)
		out := filepath.Join(t.TempDir(), "no", "x.dmp")

		if err := Place(src, out); err == nil || errors.Is(err, ErrExists) {
			t.Fatalf("Place = %v, want a plain error", err)
		}

		if _, err := os.Stat(filepath.Dir(out)); !errors.Is(err, os.ErrNotExist) {
			t.Error("a parent directory was created")
		}

		if _, err := os.Stat(src); err != nil {
			t.Errorf("src lost: %v", err)
		}
	})

	t.Run("copy fails", func(t *testing.T) {
		t.Parallel()

		// src is a directory: the copy's read fails after out was created.
		src := t.TempDir()
		out := filepath.Join(t.TempDir(), "x.dmp")

		if err := place(src, out, exdev); err == nil {
			t.Fatal("place succeeded")
		}

		if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the partial copy stayed: %v", err)
		}

		if _, err := os.Stat(src); err != nil {
			t.Errorf("src lost: %v", err)
		}
	})
}
