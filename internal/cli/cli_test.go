// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/version"
)

// updateGoldenEnv makes assertGolden rewrite golden files instead of comparing
// (task test:golden). Review the resulting diff before committing it.
const updateGoldenEnv = "EYEDBG_UPDATE_GOLDEN"

var testInfo = version.Info{
	Version:   "v1.2.3",
	Commit:    "0123abcd",
	Date:      "2026-09-01T10:00:00Z",
	GoVersion: "go1.26.0",
	Platform:  "linux/amd64",
}

func TestVersionOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		golden string
	}{
		{name: "text", args: []string{"version"}, golden: "version.golden"},
		{name: "json", args: []string{"version", "--json"}, golden: "version_json.golden"},
		{name: "flag", args: []string{"--version"}, golden: "version_flag.golden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), tt.args)
			if code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), stdout)
		})
	}
}

func TestErrorsExitNonZero(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		root       *cobra.Command
		args       []string
		wantStderr string
	}{
		{name: "unknown command", root: NewEyedbgCommand(testInfo), args: []string{"no-such-command"}, wantStderr: "eyedbg: "},
		{name: "daemon stub", root: NewDaemonCommand(testInfo), wantStderr: "eyedbgd: the daemon"},
		{name: "mcp stub", root: NewMCPCommand(testInfo), wantStderr: "eyedbg-mcp: the MCP server"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, code := execute(t, tt.root, tt.args)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}

			if !strings.HasPrefix(string(stderr), tt.wantStderr) {
				t.Errorf("stderr = %q, want prefix %q", stderr, tt.wantStderr)
			}
		})
	}
}

func execute(t *testing.T, root *cobra.Command, args []string) (stdout, stderr []byte, code int) {
	t.Helper()

	var out, errOut bytes.Buffer

	root.SetOut(&out)
	root.SetErr(&errOut)
	code = Run(t.Context(), root, args)

	return out.Bytes(), errOut.Bytes(), code
}

func assertGolden(t *testing.T, path string, got []byte) {
	t.Helper()

	if os.Getenv(updateGoldenEnv) != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("update golden file: %v", err)
		}

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (set %s=1 to create it): %v", updateGoldenEnv, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("output mismatch for %s\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}
