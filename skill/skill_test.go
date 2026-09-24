// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package skill_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/skill"
)

func TestMarkdown(t *testing.T) {
	t.Parallel()

	md := skill.Markdown()
	if md == "" {
		t.Fatal("Markdown() is empty")
	}

	if !strings.HasPrefix(md, "---\n") {
		t.Fatalf("Markdown() does not start with frontmatter: %q", md[:min(20, len(md))])
	}
}

func TestDefaultRoot(t *testing.T) {
	t.Parallel()

	root, err := skill.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(".claude", "skills")
	if !strings.HasSuffix(root, want) {
		t.Errorf("DefaultRoot() = %q, want a path ending in %q", root, want)
	}
}

func TestInstall(t *testing.T) {
	// Subtests share root and path and run in this order deliberately, so
	// they don't call t.Parallel().
	root := t.TempDir()
	path := filepath.Join(root, skill.Name, "SKILL.md")

	t.Run("installed", func(t *testing.T) { testInstallFresh(t, root, path) })
	t.Run("unchanged", func(t *testing.T) { testInstallUnchanged(t, root) })
	t.Run("differs", func(t *testing.T) { testInstallDiffers(t, root, path) })
	t.Run("forced", func(t *testing.T) { testInstallForced(t, root, path) })
}

// testInstallFresh checks that Install writes a new file.
func testInstallFresh(t *testing.T, root, path string) {
	t.Helper()

	result, err := skill.Install(root, false)
	if err != nil {
		t.Fatal(err)
	}

	if result.Status != skill.StatusInstalled {
		t.Errorf("status = %q, want %q", result.Status, skill.StatusInstalled)
	}

	if result.Path != path {
		t.Errorf("path = %q, want %q", result.Path, path)
	}

	assertContent(t, path, skill.Markdown())

	if runtime.GOOS != "windows" {
		assertMode(t, path, 0o600)
	}
}

// testInstallUnchanged checks that installing the same content again is a
// no-op.
func testInstallUnchanged(t *testing.T, root string) {
	t.Helper()

	result, err := skill.Install(root, false)
	if err != nil {
		t.Fatal(err)
	}

	if result.Status != skill.StatusUnchanged {
		t.Errorf("status = %q, want %q", result.Status, skill.StatusUnchanged)
	}
}

// testInstallDiffers checks that Install refuses to overwrite an edited
// file without force.
func testInstallDiffers(t *testing.T, root, path string) {
	t.Helper()

	if err := os.WriteFile(path, []byte("edited by the user\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := skill.Install(root, false); err == nil {
		t.Fatal("Install over an edited file: want an error")
	} else if _, ok := errors.AsType[*skill.DiffersError](err); !ok {
		t.Fatalf("Install over an edited file: err = %v, want *DiffersError", err)
	}

	assertContent(t, path, "edited by the user\n")
}

// testInstallForced checks that force replaces an edited file.
func testInstallForced(t *testing.T, root, path string) {
	t.Helper()

	result, err := skill.Install(root, true)
	if err != nil {
		t.Fatal(err)
	}

	if result.Status != skill.StatusReplaced {
		t.Errorf("status = %q, want %q", result.Status, skill.StatusReplaced)
	}

	assertContent(t, path, skill.Markdown())
}

// assertContent fails the test unless path holds want.
func assertContent(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Errorf("%s: content does not match", path)
	}
}

// assertMode fails the test unless path's permission bits are want.
func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if mode := info.Mode().Perm(); mode != want {
		t.Errorf("%s: mode = %o, want %o", path, mode, want)
	}
}

// TestInstallReplacesSymlinkInsteadOfWritingThrough checks Install's
// documented atomic-write property: when SKILL.md is a symlink to another
// file, Install's rename replaces the symlink itself rather than writing
// through it to the link's target. Unix-only: creating a symlink on Windows
// needs elevated privileges or developer mode, so it is not portable here.
func TestInstallReplacesSymlinkInsteadOfWritingThrough(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges or developer mode on Windows")
	}

	root := t.TempDir()
	dir := filepath.Join(root, skill.Name)
	path := filepath.Join(dir, "SKILL.md")

	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}

	elsewhere := filepath.Join(root, "elsewhere.md")
	const elsewhereContent = "not the skill, do not touch\n"

	if err := os.WriteFile(elsewhere, []byte(elsewhereContent), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}

	result, err := skill.Install(root, true)
	if err != nil {
		t.Fatal(err)
	}

	if result.Status != skill.StatusReplaced {
		t.Errorf("status = %q, want %q", result.Status, skill.StatusReplaced)
	}

	assertContent(t, elsewhere, elsewhereContent)

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("%s: still a symlink after Install, want a regular file", path)
	}

	assertContent(t, path, skill.Markdown())
}

func TestInstallCreatesRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")

	result, err := skill.Install(root, false)
	if err != nil {
		t.Fatal(err)
	}

	if result.Status != skill.StatusInstalled {
		t.Errorf("status = %q, want %q", result.Status, skill.StatusInstalled)
	}

	if _, err := os.Stat(result.Path); err != nil {
		t.Fatal(err)
	}
}
