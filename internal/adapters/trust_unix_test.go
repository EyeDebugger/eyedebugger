// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package adapters

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeInfo is a FileInfo with a chosen mode and owner.
type fakeInfo struct {
	mode     fs.FileMode
	uid, gid uint32
}

func (f fakeInfo) Name() string      { return "x" }
func (fakeInfo) Size() int64         { return 0 }
func (f fakeInfo) Mode() fs.FileMode { return f.mode }
func (fakeInfo) ModTime() time.Time  { return time.Time{} }
func (f fakeInfo) IsDir() bool       { return f.mode.IsDir() }
func (f fakeInfo) Sys() any          { return &syscall.Stat_t{Uid: f.uid, Gid: f.gid} }

func TestTrustChecker(t *testing.T) {
	t.Parallel()

	const me, myGroup, other = 501, 20, 777

	tests := []struct {
		name string
		info fakeInfo
		want string // "" = trusted
	}{
		{"own 0600 file", fakeInfo{0o600, me, myGroup}, ""},
		{"own 0644 file", fakeInfo{0o644, me, myGroup}, ""},
		{"own 0755 dir", fakeInfo{fs.ModeDir | 0o755, me, myGroup}, ""},
		{"own 0660 file, own primary group", fakeInfo{0o660, me, myGroup}, "writable by others"},
		{"own 0775 dir, own primary group", fakeInfo{fs.ModeDir | 0o775, me, myGroup}, "chmod go-w"},
		{"0666 file", fakeInfo{0o666, me, myGroup}, "writable by others"},
		{"0777 dir", fakeInfo{fs.ModeDir | 0o777, me, myGroup}, "chmod go-w"},
		{"group-writable, other group", fakeInfo{0o660, me, other}, "writable by others"},
		{"another user's file", fakeInfo{0o600, other, myGroup}, "owned by uid 777"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := trustChecker{uid: me}.check("/cfg/x.json", tt.info)
			if (tt.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("check = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestTrustRealFiles checks real permissions: a symlink is judged by its
// target, and a world-writable manifest or directory is refused.
func TestTrustRealFiles(t *testing.T) {
	t.Parallel()

	dir := userDir(t)
	target := writeManifest(t, t.TempDir(), "real.json", validManifest())

	if err := os.Symlink(target, filepath.Join(dir, "toy.json")); err != nil {
		t.Fatal(err)
	}

	if m := load(dir).Language("toylang"); m == nil {
		t.Fatal("a symlink to a 0600 manifest was refused")
	}

	if err := os.Chmod(target, 0o666); err != nil { //nolint:gosec // The test makes the file unsafe on purpose.
		t.Fatal(err)
	}

	reg := load(dir)
	if got := problemAt(reg, filepath.Join(dir, "toy.json")); !strings.Contains(got, "chmod go-w") || reg.Language("toylang") != nil {
		t.Fatalf("0666 target: problem %q, loaded %v", got, reg.Language("toylang") != nil)
	}

	if err := os.Chmod(target, 0o620); err != nil { //nolint:gosec // The test makes the file unsafe on purpose.
		t.Fatal(err)
	}

	if got := problemAt(load(dir), filepath.Join(dir, "toy.json")); !strings.Contains(got, "writable by others") {
		t.Fatalf("0620 target (group-writable, own group): problem %q", got)
	}

	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // The test makes the directory unsafe on purpose.
		t.Fatal(err)
	}

	reg = load(dir)
	if got := problemAt(reg, dir); !strings.Contains(got, "writable by others") || reg.Language("toylang") != nil {
		t.Fatalf("0777 dir: problem %q", got)
	}
}

// TestVenvRealPermissions: a project venv is run only while it is the
// user's alone.
func TestVenvRealPermissions(t *testing.T) {
	t.Parallel()

	proj := t.TempDir()
	bin := filepath.Join(proj, ".venv", "bin")

	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(proj, ".venv", "pyvenv.cfg"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	env := systemPythonEnv()
	env.dataDir = func() (string, error) { return t.TempDir(), nil }
	env.probe = func(_ context.Context, argv []string, _, _ string) (probeResult, error) {
		return probeResult{Executable: argv[0], Version: "3.12.0", Root: proj}, nil
	}
	m := debugpyManifest(t)

	if err := os.MkdirAll(filepath.Join(proj, "debugpy", "adapter"), 0o700); err != nil {
		t.Fatal(err)
	}

	rt, err := env.resolve(t.Context(), m, PythonInput{Cwd: proj})
	if err != nil || rt.Source != PythonFromVenv {
		t.Fatalf("resolve = %+v, %v; want the project's venv", rt, err)
	}

	if err := os.Chmod(bin, 0o770); err != nil { //nolint:gosec // The test makes the venv unsafe on purpose.
		t.Fatal(err)
	}

	if _, err := env.resolve(t.Context(), m, PythonInput{Cwd: proj}); err == nil || !strings.Contains(err.Error(), "writable by others") {
		t.Fatalf("resolve with a group-writable venv bin = %v, want refused", err)
	}
}
