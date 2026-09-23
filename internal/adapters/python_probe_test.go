// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// realPython returns an interpreter on PATH, or skips the test.
func realPython(t *testing.T) string {
	t.Helper()

	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}

	t.Skip("no python3 or python on PATH")

	return ""
}

// TestProbeRealPython runs the probe on a real interpreter.
func TestProbeRealPython(t *testing.T) {
	t.Parallel()

	py := realPython(t)

	r, err := runProbe(t.Context(), []string{py}, t.TempDir(), "json")
	if err != nil {
		// A Python 2 or a broken interpreter: nothing to check here.
		t.Skipf("%s doesn't run the probe: %v", py, err)
	}

	if r.Executable == "" || r.Version == "" || r.Root == "" {
		t.Fatalf("probe(json) = %+v; want executable, version and root", r)
	}

	r, err = runProbe(t.Context(), []string{py}, t.TempDir(), "no_such_module_xyz")
	if err != nil || r.Root != "" || r.Version == "" {
		t.Fatalf("probe(no_such_module_xyz) = %+v, %v; want a version and no root", r, err)
	}
}

// TestProbeIgnoresWorkingDirectory: a package and a sitecustomize.py
// planted in the directory the probe runs in are neither found nor run,
// so a cloned repository can't make eyedbg run its code.
func TestProbeIgnoresWorkingDirectory(t *testing.T) {
	t.Parallel()

	py := realPython(t)
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")

	files := map[string]string{
		filepath.Join(dir, "planted_pkg_xyz", "__init__.py"): "",
		filepath.Join(dir, "planted_mod_xyz.py"):             "",
		filepath.Join(dir, "sitecustomize.py"):               "open(" + pyString(marker) + ", 'w').close()\n",
		filepath.Join(dir, "usercustomize.py"):               "open(" + pyString(marker) + ", 'w').close()\n",
	}

	for p, content := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, module := range []string{"planted_pkg_xyz", "planted_mod_xyz"} {
		r, err := runProbe(t.Context(), []string{py}, dir, module)
		if err != nil {
			t.Skipf("%s doesn't run the probe: %v", py, err)
		}

		if r.Root != "" || r.ModuleVersion != "" {
			t.Fatalf("probe found %s in the working directory: %+v", module, r)
		}
	}

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a sitecustomize.py or usercustomize.py in the working directory ran")
	}
}

// pyString quotes s as a Python string literal.
func pyString(s string) string {
	out := []byte{'\''}

	for _, c := range []byte(s) {
		if c == '\\' || c == '\'' {
			out = append(out, '\\')
		}

		out = append(out, c)
	}

	return string(append(out, '\''))
}
