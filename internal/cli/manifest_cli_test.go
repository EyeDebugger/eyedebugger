// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

func TestParseOpts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      []string
		want    map[string]string
		wantErr string
	}{
		{nil, nil, ""},
		{[]string{"module=pytest", "justMyCode=false"}, map[string]string{"module": "pytest", "justMyCode": "false"}, ""},
		{[]string{"module="}, map[string]string{"module": ""}, ""},
		{[]string{"a=b=c"}, map[string]string{"a": "b=c"}, ""},
		{[]string{"module"}, nil, "want NAME=VALUE"},
		{[]string{"=x"}, nil, "want NAME=VALUE"},
		{[]string{"a=1", "a=2"}, nil, "given twice"},
	}

	for _, tt := range tests {
		got, err := parseOpts(tt.in)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("parseOpts(%q) err = %v, want %q", tt.in, err, tt.wantErr)
			}

			continue
		}

		if err != nil || !maps.Equal(got, tt.want) {
			t.Errorf("parseOpts(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
}

// fakeManifest is a user manifest of language "fakelang", served by the
// fake adapter (this test binary).
func fakeManifest(t *testing.T) map[string]any {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	return map[string]any{
		"schema": 1, "name": "fakedbg", "description": "The fake test adapter", "version": "1.0",
		"adapter": map[string]any{
			"id": "fake", "entry": exe, "args": []any{"-test.run=^$"}, "environment": map[string]any{daptest.EnvFakeAdapter: "1"},
		},
		"language": map[string]any{"name": "fakelang", "extensions": []any{".fake"}},
		"options":  map[string]any{"lines": map[string]any{"type": "int", "default": "10", "help": "the program's length"}},
		"launch": map[string]any{
			"require":   []any{"program"},
			"arguments": map[string]any{"program": "${program}", "lines": "${opt.lines}", "stopAtEntry": "${stopOnEntry}"},
		},
	}
}

// privateDir is a new temporary directory only the user can write: under
// umask 002 t.TempDir's own directories are group-writable, which the
// manifest permission check refuses.
func privateDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // A directory needs its x bit; 0700 is private.
		t.Fatal(err)
	}

	return dir
}

// userManifests writes manifests (file name → content; a string is
// written as is) to a new config dir's adapters/ and returns the config
// dir.
func userManifests(t *testing.T, files map[string]any) string {
	t.Helper()

	cfg := privateDir(t)
	dir := filepath.Join(cfg, "adapters")

	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	for name, m := range files {
		raw, ok := m.(string)
		if !ok {
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}

			raw = string(b)
		}

		if err := os.WriteFile(filepath.Join(dir, name), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return cfg
}

// testRegistry loads the bundled manifests and cfg's user manifests.
func testRegistry(cfg string) *adapters.Registry {
	return adapters.Load(adapters.LoadConfig{
		UserDir: filepath.Join(cfg, "adapters"), Builtin: builtinLanguages(), Bundled: adapters.Bundled(),
	})
}

// withoutDir replaces dir in text output with <config>, and the path
// separators after it with slashes, so goldens hold on Windows too.
func withoutDir(out, dir string) []byte {
	out = strings.ReplaceAll(out, dir, "<config>")

	return []byte(strings.ReplaceAll(out, `\`, "/"))
}

// withoutDirJSON is withoutDir for JSON output, where a Windows path's
// backslashes are escaped.
func withoutDirJSON(out, dir string) []byte {
	out = strings.ReplaceAll(out, strings.ReplaceAll(dir, `\`, `\\`), "<config>")

	return []byte(strings.ReplaceAll(out, `\\`, "/"))
}

func TestAdaptersList(t *testing.T) {
	t.Parallel()

	toy := fakeManifest(t)
	toy["adapter"] = map[string]any{"id": "toy", "entry": "toydbg", "path": true}
	toy["name"], toy["language"] = "toydbg", map[string]any{"name": "toy", "extensions": []any{".toy"}, "markers": []any{"toy.cfg"}}

	cfg := userManifests(t, map[string]any{"toy.json": toy, "broken.json": `{"schema": 1, "name": "x"}`})
	reg := testRegistry(cfg)

	assertGolden(t, filepath.Join("testdata", "adapters_ls.golden"), withoutDir(listText(reg), cfg))

	var out bytes.Buffer
	if err := writeJSON(&out, listJSON(reg, "linux/amd64")); err != nil {
		t.Fatal(err)
	}

	assertGolden(t, filepath.Join("testdata", "adapters_ls_json.golden"), withoutDirJSON(out.String(), cfg))
}

func TestDaemonDrivers(t *testing.T) {
	t.Parallel()

	dotnetShadow := fakeManifest(t)
	dotnetShadow["name"], dotnetShadow["language"] = "shadow", map[string]any{"name": "dotnet"}

	cfg := userManifests(t, map[string]any{"fake.json": fakeManifest(t), "shadow.json": dotnetShadow})

	var names []string
	for _, d := range daemonDrivers(t.Context(), testRegistry(cfg), slog.New(slog.DiscardHandler)) {
		names = append(names, d.Name())
	}

	slices.Sort(names)

	// shadow may not take dotnet from its Go driver: it is ignored.
	if !slices.Equal(names, []string{"c", "cpp", "dotnet", "fakelang", "python", "rust"}) {
		t.Fatalf("drivers = %v, want c, cpp, dotnet, fakelang, python and rust", names)
	}
}

// TestManifestLanguageCLI debugs a language added by a user manifest
// alone through the real commands and an in-process daemon wired like
// eyedbgd. Not parallel: it sets environment variables.
func TestManifestLanguageCLI(t *testing.T) {
	p := isolate(t)
	cfg := userManifests(t, map[string]any{"fake.json": fakeManifest(t)})
	t.Setenv(adapters.EnvConfigDir, cfg)

	serveWith(t, p, loadRegistry())

	prog := filepath.Join(t.TempDir(), "p.fake")
	if err := os.WriteFile(prog, []byte(strings.Repeat("line\n", 5)), 0o600); err != nil {
		t.Fatal(err)
	}

	expectOutput(t, run(t, 0, "adapters", "ls"), "fakedbg", "fakelang", "user", "options: lines=int (default 10)")

	out := run(t, 0, "start", "fakelang", "--program", prog, "--opt", "lines=5", "--bp", prog+":3", "--timeout", "20s")
	expectOutput(t, out, "stopped: breakpoint")
	expectOutput(t, run(t, 0, "vars"), "Locals", "line", "x")
	expectOutput(t, run(t, 0, "eval", "line"), "3")
	expectOutput(t, run(t, 0, "continue", "--timeout", "20s"), "exited")
	run(t, 0, "stop")

	expectOutput(t, run(t, exitError, "start", "fakelang", "--program", filepath.Join(t.TempDir(), "gone.fake")), "[INVALID_REQUEST]", "not found")
	expectOutput(t, run(t, exitError, "start", "fakelang", "--program", prog, "--opt", "speed=9"), "[INVALID_REQUEST]", `no option "speed"`, "lines (int)")
	expectOutput(t, run(t, exitError, "start", "dotnet", "--program", prog, "--opt", "x=1"), "dotnet takes no --opt options")
}

// fakeManifestConnect is a user manifest of language "fakeconn", served by
// the fake adapter over the connect transport ([daptest.UserManifestConnect]):
// [TestManifestLanguageCLI]'s twin, proving out schema 1's socket transport
// end to end through the real commands.
func fakeManifestConnect(t *testing.T) map[string]any {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	m := daptest.UserManifestConnect(exe)
	m["name"], m["language"] = "fakedbg-connect", map[string]any{"name": "fakeconn", "extensions": []any{".fake"}}

	return m
}

// TestManifestLanguageConnectCLI is [TestManifestLanguageCLI] over the
// connect transport (adapter.transport "connect", ${socket} in
// adapter.args): the session listens on a Unix socket instead of speaking
// DAP on the adapter's stdio. Not parallel: it sets environment variables.
func TestManifestLanguageConnectCLI(t *testing.T) {
	p := isolate(t)
	cfg := userManifests(t, map[string]any{"fake.json": fakeManifestConnect(t)})
	t.Setenv(adapters.EnvConfigDir, cfg)

	serveWith(t, p, loadRegistry())

	prog := filepath.Join(t.TempDir(), "p.fake")
	if err := os.WriteFile(prog, []byte(strings.Repeat("line\n", 5)), 0o600); err != nil {
		t.Fatal(err)
	}

	expectOutput(t, run(t, 0, "adapters", "ls"), "fakedbg-connect", "fakeconn", "user")

	out := run(t, 0, "start", "fakeconn", "--program", prog, "--opt", "lines=5", "--bp", prog+":3", "--timeout", "20s")
	expectOutput(t, out, "stopped: breakpoint")
	expectOutput(t, run(t, 0, "vars"), "Locals", "line", "x")
	expectOutput(t, run(t, 0, "eval", "line"), "3")
	expectOutput(t, run(t, 0, "continue", "--timeout", "20s"), "exited")
	run(t, 0, "stop")
}

// serveWith runs an in-process daemon with the drivers eyedbgd would
// build from reg, until the test ends.
func serveWith(t *testing.T, p daemon.Paths, reg *adapters.Registry) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	logger := slog.New(slog.DiscardHandler)

	go func() {
		done <- daemon.Serve(ctx, daemon.Config{
			Paths: p, IdleTimeout: time.Hour, Info: testInfo, Logger: logger,
			Drivers: daemonDrivers(ctx, reg, logger),
		})
	}()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("Serve = %v", err)
		}
	})

	waitForDaemon(t, p, done)
}
