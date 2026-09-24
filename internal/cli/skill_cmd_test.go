// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/skill"
)

func TestSkillPrint(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"skill", "print"})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}

	if string(stdout) != skill.Markdown() {
		t.Errorf("skill print output != skill.Markdown() byte for byte")
	}
}

func TestSkillPrintJSON(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"skill", "print", "--json"})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}

	var got skillPrintOutput
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}

	if got.Schema != jsonSchemaVersion || got.Name != skill.Name || got.Content != skill.Markdown() {
		t.Errorf("skill print --json = %+v", got)
	}
}

// withoutRoot replaces root in text output with <root>, and the path
// separators after it with slashes, so goldens hold on Windows too.
func withoutRoot(out []byte, root string) []byte {
	s := strings.ReplaceAll(string(out), root, "<root>")

	return []byte(strings.ReplaceAll(s, `\`, "/"))
}

func TestSkillInstall(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"skill", "install", "--dir", root})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}

	assertGolden(t, filepath.Join("testdata", "skill_install.golden"), withoutRoot(stdout, root))

	// A second install of the same content is unchanged, not installed.
	stdout, stderr, code = execute(t, NewEyedbgCommand(testInfo), []string{"skill", "install", "--dir", root})
	if code != 0 {
		t.Fatalf("second install: exit code = %d, stderr = %q", code, stderr)
	}

	if !strings.HasPrefix(string(stdout), "unchanged ") {
		t.Errorf("second install stdout = %q, want prefix %q", stdout, "unchanged ")
	}
}

func TestSkillInstallJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"skill", "install", "--dir", root, "--json"})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}

	assertGolden(t, filepath.Join("testdata", "skill_install_json.golden"), withoutRootJSON(stdout, root))
}

// withoutRootJSON is withoutRoot for JSON output, where a Windows path's
// backslashes are escaped.
func withoutRootJSON(out []byte, root string) []byte {
	s := strings.ReplaceAll(string(out), strings.ReplaceAll(root, `\`, `\\`), "<root>")

	return []byte(strings.ReplaceAll(s, `\\`, "/"))
}

func TestSkillInstallDiffersRefusesWithoutForce(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	if _, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"skill", "install", "--dir", root}); code != 0 {
		t.Fatalf("first install: exit code = %d, stderr = %q", code, stderr)
	}

	path := filepath.Join(root, skill.Name, "SKILL.md")
	if err := os.WriteFile(path, []byte("different content\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"skill", "install", "--dir", root})
	if code != 1 {
		t.Fatalf("exit code = %d, stdout = %q, stderr = %q; want 1", code, stdout, stderr)
	}

	if !strings.Contains(string(stderr), "[INVALID_REQUEST]") {
		t.Errorf("stderr = %q, want [INVALID_REQUEST]", stderr)
	}

	if !strings.Contains(string(stderr), "--force") {
		t.Errorf("stderr = %q, want the --force hint", stderr)
	}

	// --force replaces it.
	stdout, stderr, code = execute(t, NewEyedbgCommand(testInfo), []string{"skill", "install", "--dir", root, "--force"})
	if code != 0 {
		t.Fatalf("forced install: exit code = %d, stderr = %q", code, stderr)
	}

	if !strings.HasPrefix(string(stdout), "replaced ") {
		t.Errorf("forced install stdout = %q, want prefix %q", stdout, "replaced ")
	}
}

// TestSkillInstallRelativeDirResolvedToAbsolute does not call t.Parallel():
// t.Chdir changes the process's working directory.
func TestSkillInstallRelativeDirResolvedToAbsolute(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"skill", "install", "--dir", "skills", "--json"})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}

	var got skillInstallOutput
	if err := json.Unmarshal(stdout, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}

	if !filepath.IsAbs(got.Path) {
		t.Errorf("Path = %q, want an absolute path", got.Path)
	}

	want := filepath.Join(cwd, "skills", skill.Name, "SKILL.md")
	if got.Path != want {
		t.Errorf("Path = %q, want %q", got.Path, want)
	}
}
