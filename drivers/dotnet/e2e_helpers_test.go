// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// appsDir holds the sample apps (testdata/apps/dotnet at the repo root).
var appsDir = filepath.Join("..", "..", "testdata", "apps", "dotnet")

// requireE2E skips the test unless the end-to-end tests are enabled, and
// unless netcoredbg's bundled manifest has a download for this platform or
// netcoredbg can otherwise be found (EYEDBG_NETCOREDBG, already installed,
// or on PATH): e2e-count=5 in CI only runs where the adapter can actually
// run, but a self-built netcoredbg on a platform with no official download
// (osx-x64, win-arm64) can still be validated this way.
func requireE2E(t *testing.T) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK and 'eyedbg adapters install netcoredbg')")
	}

	reg := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: []string{dotnet.Language}})

	m := reg.Language(dotnet.Language)
	if m == nil || m.Install == nil {
		t.Fatal("no bundled netcoredbg manifest for dotnet (or it has no install section)")
	}

	_, hasDownload := m.Install.Downloads[runtime.GOOS+"/"+runtime.GOARCH]
	_, findErr := adapters.Find(m)
	found := hasDownload || findErr == nil

	if skip, reason := dotnetE2ESkip(runtime.GOOS, runtime.GOARCH, found); skip {
		t.Skip(reason)
	}
}

// dotnetE2ESkip decides whether the .NET e2e tests should skip on this
// platform: found reports whether netcoredbg's bundled manifest has a
// download for goos/goarch, or netcoredbg can otherwise be found.
func dotnetE2ESkip(goos, goarch string, found bool) (skip bool, reason string) {
	if !found {
		return true, fmt.Sprintf("netcoredbg has no download for %s/%s, and none was otherwise found (EYEDBG_NETCOREDBG, installed, or PATH)", goos, goarch)
	}

	return false, ""
}

// copyApp copies sample app name to a temporary directory (without bin and
// obj) and returns the copy.
func copyApp(t *testing.T, name string) string {
	t.Helper()

	dst := t.TempDir()

	if err := os.CopyFS(dst, os.DirFS(filepath.Join(appsDir, name))); err != nil {
		t.Fatalf("copy app %s: %v", name, err)
	}

	// A build in the source tree (ignored by git) is not the test's.
	for _, d := range []string{"bin", "obj"} {
		if err := os.RemoveAll(filepath.Join(dst, d)); err != nil {
			t.Fatal(err)
		}
	}

	return dst
}

// lineOf returns the line of file that ends in "// marker: NAME".
func lineOf(t *testing.T, file, marker string) int {
	t.Helper()

	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), "// marker: "+marker) {
			return n
		}
	}

	t.Fatalf("no marker %q in %s", marker, file)

	return 0
}

// buildApp builds the app in dir (Debug) and returns its .dll.
func buildApp(t *testing.T, dir, name string) string {
	t.Helper()

	host, err := dotnet.FindHost()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), host, "build", dir, "-c", "Debug", "-nologo")
	cmd.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dotnet build %s: %v\n%s", dir, err, out)
	}

	dll := filepath.Join(dir, "bin", "Debug", "net10.0", name+".dll")
	if _, err := os.Stat(dll); err != nil {
		t.Fatal(err)
	}

	return dll
}

// TestDotnetE2ESkip covers dotnetE2ESkip's platform decision: only found
// decides, goos/goarch merely shape the reason.
func TestDotnetE2ESkip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		goos, goarch string
		found        bool
		wantSkip     bool
	}{
		{goos: "darwin", goarch: "amd64", found: false, wantSkip: true},
		{goos: "darwin", goarch: "amd64", found: true, wantSkip: false},
		{goos: "windows", goarch: "arm64", found: false, wantSkip: true},
		{goos: "windows", goarch: "arm64", found: true, wantSkip: false},
		{goos: "linux", goarch: "amd64", found: false, wantSkip: true},
		{goos: "linux", goarch: "amd64", found: true, wantSkip: false},
	}

	for _, tt := range tests {
		skip, reason := dotnetE2ESkip(tt.goos, tt.goarch, tt.found)
		if skip != tt.wantSkip {
			t.Errorf("dotnetE2ESkip(%q, %q, %v) skip = %v, want %v", tt.goos, tt.goarch, tt.found, skip, tt.wantSkip)
		}

		if skip && !strings.Contains(reason, tt.goos+"/"+tt.goarch) {
			t.Errorf("dotnetE2ESkip(%q, %q, %v) reason = %q, want it to name the platform", tt.goos, tt.goarch, tt.found, reason)
		}

		if !skip && reason != "" {
			t.Errorf("dotnetE2ESkip(%q, %q, %v) reason = %q, want empty when not skipping", tt.goos, tt.goarch, tt.found, reason)
		}
	}
}

// newManager returns a manager with the .NET driver, stopped at cleanup.
// Its context is not the test's: that one ends before cleanup, and ending
// it kills the adapters before StopAll could end their debuggees.
func newManager(t *testing.T) *session.Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := session.NewManager(ctx, session.Config{
		Drivers: []session.Driver{dotnet.New()}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard,
	})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	return m
}
