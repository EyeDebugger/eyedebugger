// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
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
		{name: "unknown daemon subcommand", root: NewEyedbgCommand(testInfo), args: []string{"daemon", "bogus"}, wantStderr: "eyedbg: unknown command"},
		{name: "daemon extra args", root: NewDaemonCommand(testInfo), args: []string{"bogus"}, wantStderr: "eyedbgd: unknown command"},
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

// TestCompletionUsesConfiguredWriter guards against completion scripts
// bypassing the command's configured output writer. cobra's
// InitDefaultCompletionCmd captures the writer once, when it runs, and every
// shell sub-command's RunE reuses that captured writer forever after; if
// finalizeCommandTree ran before a caller's SetOut (as it once did, in
// New*Command), the script would go to the process's real stdout instead of
// wherever the caller pointed it.
//
// It builds its tree with NewEyedbgCommand directly, not via rootFactories,
// so that (like a real caller) SetOut runs before this tree is ever
// finalized.
func TestCompletionUsesConfiguredWriter(t *testing.T) {
	t.Parallel()

	for _, shell := range []string{"bash", "zsh"} {
		t.Run(shell, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"completion", shell})
			if code != 0 {
				t.Fatalf("completion %s: exit code = %d, stderr = %q", shell, code, stderr)
			}

			if len(stdout) == 0 {
				t.Fatalf("completion %s: no output landed in the configured writer (it went elsewhere, e.g. os.Stdout)", shell)
			}

			if !strings.Contains(string(stdout), "eyedbg") {
				t.Errorf("completion %s: output doesn't mention \"eyedbg\"\n--- stdout ---\n%s", shell, stdout)
			}
		})
	}
}

// productionFactories returns a fresh, unfinalized command tree per binary,
// built exactly as cmd/* builds it: Run finalizes it, so help output rendered
// from these trees proves the production path adds cobra's generated commands
// and their Examples.
func productionFactories() map[string]func() *cobra.Command {
	return map[string]func() *cobra.Command{
		"eyedbg":  func() *cobra.Command { return NewEyedbgCommand(testInfo) },
		"eyedbgd": func() *cobra.Command { return NewDaemonCommand(testInfo) },
	}
}

// rootFactories returns a fresh, already-finalized command tree per binary,
// so every test below covers every binary in the scaffold (docs/DESIGN.md
// §4). A factory (not a shared instance) matters: cobra command state must
// not be reused across multiple Execute calls.
//
// These trees are only walked and searched (Commands, Find). Anything that
// renders output uses productionFactories instead, so a missing finalize in
// Run can't hide behind the finalize done here.
func rootFactories() map[string]func() *cobra.Command {
	factories := productionFactories()
	for name, build := range factories {
		factories[name] = func() *cobra.Command {
			root := build()
			finalizeCommandTree(root)

			return root
		}
	}

	return factories
}

// TestAllCommandsHaveHelp walks every command and subcommand of every binary
// and fails if any non-hidden command lacks Short, Long or Example, so a
// future command can't regress docs/DESIGN.md §4's "help is the interface"
// rule.
func TestAllCommandsHaveHelp(t *testing.T) {
	t.Parallel()

	for name, newRoot := range rootFactories() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			assertHasHelp(t, newRoot())
		})
	}
}

func assertHasHelp(t *testing.T, cmd *cobra.Command) {
	t.Helper()

	if !cmd.Hidden {
		path := cmd.CommandPath()

		if cmd.Short == "" {
			t.Errorf("%s: missing Short", path)
		}

		if cmd.Long == "" {
			t.Errorf("%s: missing Long", path)
		}

		if cmd.Example == "" {
			t.Errorf("%s: missing Example", path)
		}
	}

	for _, sub := range cmd.Commands() {
		assertHasHelp(t, sub)
	}
}

// TestHelpReachableForEveryCommand asserts that every command and subcommand
// of every binary (including the root command itself) is reachable both as
// '<path...> --help' and as 'help <path...>', and that both forms print that
// command's own Long and Example text (docs/DESIGN.md §4).
func TestHelpReachableForEveryCommand(t *testing.T) {
	t.Parallel()

	production := productionFactories()

	for name, newRoot := range rootFactories() {
		for _, path := range commandPaths(newRoot()) {
			t.Run(name+"/"+strings.Join(append([]string{"root"}, path...), " "), func(t *testing.T) {
				t.Parallel()

				target, _, err := newRoot().Find(path)
				if err != nil {
					t.Fatalf("Find(%v): %v", path, err)
				}

				assertHelpOutput(t, production[name](), append(append([]string{}, path...), "--help"), target.Long, target.Example)
				assertHelpOutput(t, production[name](), append([]string{"help"}, path...), target.Long, target.Example)
			})
		}
	}
}

// TestCommandTreeMatchesHelpOutput guards TestAllCommandsHaveHelp and
// TestHelpReachableForEveryCommand against going vacuous again the way they
// did before finalizeCommandTree existed (docs/DESIGN.md §4): it re-derives,
// from actual '--help' output, the set of sub-commands at every level, and
// asserts it equals what commandPaths' tree-walk (the thing those two tests
// rely on) found at that same level. A future cobra-added command that
// finalizeCommandTree doesn't account for would show up in '--help' but not
// in the walk, and fail here.
func TestCommandTreeMatchesHelpOutput(t *testing.T) {
	t.Parallel()

	production := productionFactories()

	for name, newRoot := range rootFactories() {
		for _, path := range commandPaths(newRoot()) {
			t.Run(name+"/"+strings.Join(append([]string{"root"}, path...), " "), func(t *testing.T) {
				t.Parallel()

				assertCommandSetMatchesHelp(t, newRoot, production[name], path)
			})
		}
	}
}

// assertCommandSetMatchesHelp compares the non-hidden sub-commands of the
// command at path (found via cobra's own tree-walk) against the ones listed
// in that same command's real '--help' output, rendered from an unfinalized
// production tree.
func assertCommandSetMatchesHelp(t *testing.T, newRoot, newProduction func() *cobra.Command, path []string) {
	t.Helper()

	target, _, err := newRoot().Find(path)
	if err != nil {
		t.Fatalf("Find(%v): %v", path, err)
	}

	walked := make(map[string]bool)
	for _, sub := range target.Commands() {
		if !sub.Hidden {
			walked[sub.Name()] = true
		}
	}

	stdout, stderr, code := execute(t, newProduction(), append(append([]string{}, path...), "--help"))
	if code != 0 {
		t.Fatalf("--help exit code = %d, stderr = %q", code, stderr)
	}

	fromHelp := availableCommandsIn(string(stdout))

	if !maps.Equal(walked, fromHelp) {
		t.Errorf("command set mismatch for %q\n  tree-walk: %v\n  --help:    %v", strings.Join(path, " "), walked, fromHelp)
	}
}

// availableCommandsIn extracts the command names listed under a rendered
// help page's "Available Commands:" section (empty if there is none).
func availableCommandsIn(help string) map[string]bool {
	names := make(map[string]bool)

	lines := strings.Split(help, "\n")
	inSection := false

	for _, line := range lines {
		if strings.HasPrefix(line, "Available Commands:") {
			inSection = true

			continue
		}

		if !inSection {
			continue
		}

		if strings.TrimSpace(line) == "" {
			break
		}

		if fields := strings.Fields(line); len(fields) > 0 {
			names[fields[0]] = true
		}
	}

	return names
}

// commandPaths returns the argument path (one element per level, matching
// Command.Name()) of every non-hidden command in root's tree, including the
// root itself as an empty path.
func commandPaths(root *cobra.Command) [][]string {
	var paths [][]string

	var walk func(cmd *cobra.Command, prefix []string)
	walk = func(cmd *cobra.Command, prefix []string) {
		if cmd.Hidden {
			return
		}

		paths = append(paths, append([]string{}, prefix...))

		for _, sub := range cmd.Commands() {
			walk(sub, append(prefix, sub.Name()))
		}
	}

	walk(root, nil)

	return paths
}

func assertHelpOutput(t *testing.T, root *cobra.Command, args []string, wantLong, wantExample string) {
	t.Helper()

	stdout, stderr, code := execute(t, root, args)
	if code != 0 {
		t.Fatalf("eyedbg %s: exit code = %d, stderr = %q", strings.Join(args, " "), code, stderr)
	}

	if !strings.Contains(string(stdout), wantLong) {
		t.Errorf("eyedbg %s: stdout missing Long text\n--- stdout ---\n%s", strings.Join(args, " "), stdout)
	}

	if !strings.Contains(string(stdout), wantExample) {
		t.Errorf("eyedbg %s: stdout missing Example text\n--- stdout ---\n%s", strings.Join(args, " "), stdout)
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

func TestExitCodeClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		err  error
		want int
	}{
		{errors.New("bad flag"), exitError},
		{api.NewError(api.CodeInvalidRequest, "", ""), exitError},
		{api.NewError(api.CodeNotStopped, "", ""), exitState},
		{fmt.Errorf("wrapped: %w", api.NewError(api.CodeNoSession, "", "")), exitState},
		{api.NewError(api.CodeBuildFailed, "", ""), exitEnvironment},
		{api.NewError(api.CodeAdapterFailed, "", ""), exitAdapter},
	}

	for _, tt := range tests {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("exitCode(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestJSONErrors(t *testing.T) {
	t.Parallel()

	// An unknown command fails before flags are parsed; --json still applies.
	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"no-such-command", "--json"})
	if code != exitError || len(stderr) != 0 {
		t.Fatalf("exit code = %d, stderr = %q; want %d and nothing on stderr", code, stderr, exitError)
	}

	var out struct {
		Schema int       `json:"schema"`
		Error  api.Error `json:"error"`
	}

	if err := json.Unmarshal(stdout, &out); err != nil || out.Schema != jsonSchemaVersion || out.Error.Code != "ERROR" || out.Error.Message == "" {
		t.Errorf("stdout = %s (%v), want a JSON error", stdout, err)
	}
}

func TestHelpAll(t *testing.T) {
	t.Parallel()

	for name, newRoot := range productionFactories() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, code := execute(t, newRoot(), []string{"help", "--all"})
			if code != 0 {
				t.Fatalf("exit code = %d, stderr = %q", code, stderr)
			}

			tree := newRoot()
			finalizeCommandTree(tree)

			for _, path := range commandPaths(tree) {
				header := "\n" + strings.Join(append([]string{name}, path...), " ") + "\n"
				if !bytes.Contains(stdout, []byte(header)) {
					t.Errorf("help --all has no section for %q", strings.TrimSpace(header))
				}
			}

			stdout, stderr, code = execute(t, newRoot(), []string{"help", "--all", "--json"})
			if code != 0 {
				t.Fatalf("--json: exit code = %d, stderr = %q", code, stderr)
			}

			var out helpOutput
			if err := json.Unmarshal(stdout, &out); err != nil {
				t.Fatalf("--json: %v", err)
			}

			if got, want := countHelpDocs(t, out.Command), len(commandPaths(tree)); got != want {
				t.Errorf("help --all --json has %d commands, want %d", got, want)
			}
		})
	}
}

// countHelpDocs counts d and its subcommands, checking each is complete.
func countHelpDocs(t *testing.T, d helpDoc) int {
	t.Helper()

	if d.Long == "" || d.Example == "" {
		t.Errorf("%s: JSON help lacks long or example", d.Path)
	}

	n := 1
	for i := range d.Commands {
		n += countHelpDocs(t, d.Commands[i])
	}

	return n
}

func TestHelpJSONForOneCommand(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), []string{"bp", "add", "--help", "--json"})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr)
	}

	var out helpOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		t.Fatal(err)
	}

	names := map[string]bool{}
	for _, f := range append(out.Command.Flags, out.Command.InheritedFlags...) {
		names[f.Name] = true
	}

	if out.Command.Path != "eyedbg bp add" || !names["if"] || !names["session"] || !names["budget"] {
		t.Errorf("help = %+v, want bp add with --if and the inherited --session and --budget", out.Command)
	}
}
