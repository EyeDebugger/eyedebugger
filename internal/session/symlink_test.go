// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// A breakpoint through a symlinked directory is set on the real path,
// where the debug symbols put the source.
func TestResolveSpecResolvesSymlinks(t *testing.T) {
	t.Parallel()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(target, "Program.cs")
	if err := os.WriteFile(src, []byte("var x = 1;\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	tests := []struct {
		name string
		file string
		want string
	}{
		{"through a symlink", filepath.Join(link, "Program.cs"), src},
		{"real path", src, src},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveSpec(api.BreakpointSpec{File: tt.file, Line: 1}, true)
			if err != nil {
				t.Fatal(err)
			}

			if got.File != tt.want {
				t.Errorf("File = %q, want %q", got.File, tt.want)
			}
		})
	}
}

// A breakpoint in a file that isn't there is refused: the adapter would
// leave it pending for good.
func TestResolveSpecRefusesMissingFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := []struct {
		name string
		spec api.BreakpointSpec
	}{
		{"line", api.BreakpointSpec{File: filepath.Join(dir, "Gone.cs"), Line: 1}},
		{"anchor", api.BreakpointSpec{File: filepath.Join(dir, "Gone.cs"), Anchor: "x"}},
		{"directory", api.BreakpointSpec{File: dir, Line: 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := resolveSpec(tt.spec, true)
			if code := api.CodeOf(err); code != api.CodeInvalidRequest {
				t.Errorf("resolveSpec(%+v) = %v (code %q), want %s", tt.spec, err, code, api.CodeInvalidRequest)
			}
		})
	}
}

// sameFile matches a frame's file to run-until's target however the
// adapter spells it (Delve on Windows: forward slashes), and through
// symlinks, but never a different or missing file.
func TestSameFile(t *testing.T) {
	t.Parallel()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	src, other := filepath.Join(dir, "main.go"), filepath.Join(dir, "other.go")
	for _, f := range []string{src, other} {
		if err := os.WriteFile(f, []byte("package main\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	link := filepath.Join(dir, "link.go")
	if err := os.Symlink(src, link); err != nil {
		link = src // no symlinks (Windows without the privilege): the case still holds.
	}

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"same path", src, src, true},
		{"forward slashes", filepath.ToSlash(src), src, true},
		{"uncleaned", dir + string(filepath.Separator) + "." + string(filepath.Separator) + "main.go", src, true},
		{"through a symlink", link, src, true},
		{"other file", other, src, false},
		{"missing file", filepath.Join(dir, "gone.go"), src, false},
		{"both missing", filepath.Join(dir, "gone.go"), filepath.Join(dir, "lost.go"), false},
		{"empty", "", src, false},
		{"both empty", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := sameFile(tt.a, tt.b); got != tt.want {
				t.Errorf("sameFile(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

// checkStart gives the driver every directory resolved the same way: a
// driver defaults the working directory to ClientDir, and 'go build' from
// one spelling of a directory refuses a package named by another.
func TestCheckStartResolvesPaths(t *testing.T) {
	t.Parallel()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	got, err := checkStart(api.StartParams{LaunchSpec: api.LaunchSpec{Program: link, Cwd: link, Project: link, ClientDir: link}})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, got string
	}{
		{"program", got.Program},
		{"cwd", got.Cwd},
		{"project", got.Project},
		{"clientDir", got.ClientDir},
	}

	for _, tt := range tests {
		if tt.got != target {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, target)
		}
	}
}
