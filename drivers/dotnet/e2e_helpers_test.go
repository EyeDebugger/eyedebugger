// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// appsDir holds the sample apps (testdata/apps/dotnet at the repo root).
var appsDir = filepath.Join("..", "..", "testdata", "apps", "dotnet")

// requireE2E skips the test unless the end-to-end tests are enabled.
func requireE2E(t *testing.T) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK and 'eyedbg adapters install netcoredbg')")
	}
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
