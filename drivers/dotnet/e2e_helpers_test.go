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
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// appsDir holds the sample apps (testdata/apps/dotnet at the repo root).
var appsDir = filepath.Join("..", "..", "testdata", "apps", "dotnet")

// envE2ELangs narrows which languages the end-to-end tests exercise: unset
// runs every language; set (comma-separated) skips a language not listed,
// naming the variable (never silently: docs/CONVENTIONS.md § Testing) —
// duplicated per package, like lineOf.
const envE2ELangs = "EYEDBG_E2E_LANGS"

// requireLang skips t unless EYEDBG_E2E_LANGS is unset or names lang.
func requireLang(t *testing.T, lang string) {
	t.Helper()

	list := os.Getenv(envE2ELangs)
	if list == "" {
		return
	}

	if !slices.Contains(strings.Split(list, ","), lang) {
		t.Skipf("%s=%s excludes %s", envE2ELangs, list, lang)
	}
}

// envE2EAdapters narrows which .NET adapters the end-to-end tests run
// with: unset runs both; set (comma-separated) skips an adapter not listed,
// naming the variable (never silently).
const envE2EAdapters = "EYEDBG_E2E_DOTNET_ADAPTERS"

// e2eAdapterNames are the adapters every .NET e2e test runs with, each as
// a subtest.
func e2eAdapterNames() []string { return []string{"netcoredbg", dotnet.SharpDbg} }

// traits is what differs between the adapters, as the tests see it
// (docs/adr/0017's parity table).
type traits struct {
	// pause: pause stops the program (SharpDbg's is refused:
	// UNSUPPORTED_BY_ADAPTER).
	pause bool
	// display: eval shows [DebuggerDisplay] values and runs lambdas
	// (netcoredbg shows {Type} and can't parse a lambda).
	display bool
}

// adapterTraits are each adapter's traits.
func adapterTraits(adapter string) traits {
	if adapter == dotnet.SharpDbg {
		return traits{pause: false, display: true}
	}

	return traits{pause: true, display: false}
}

// forEachAdapter runs test as a subtest per adapter (named after it), each
// behind requireAdapter.
func forEachAdapter(t *testing.T, test func(t *testing.T, adapter string)) {
	t.Helper()

	for _, a := range e2eAdapterNames() {
		t.Run(a, func(t *testing.T) {
			requireAdapter(t, a)
			test(t, a)
		})
	}
}

// requireE2E skips the test unless the end-to-end tests are enabled for
// dotnet (EYEDBG_E2E=1, and EYEDBG_E2E_LANGS unset or naming dotnet).
func requireE2E(t *testing.T) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK, 'eyedbg adapters install netcoredbg' and 'eyedbg adapters install sharpdbg')")
	}

	requireLang(t, dotnet.Language)
}

// requireAdapter skips t unless adapter can run here (requireE2E first):
// EYEDBG_E2E_DOTNET_ADAPTERS names it (or is unset), and its bundled
// manifest has a download for this platform or it is otherwise found
// (EYEDBG_NETCOREDBG / EYEDBG_SHARPDBG, already installed, or on PATH). An
// adapter with a download here that isn't installed fails the test (never
// skips): CI installs both. e2e-count=5 in CI only runs where the adapter
// can actually run, but a self-built netcoredbg on a platform with no
// official download (osx-x64, win-arm64) can still be validated this way.
func requireAdapter(t *testing.T, adapter string) {
	t.Helper()

	requireE2E(t)

	if list := os.Getenv(envE2EAdapters); list != "" && !slices.Contains(strings.Split(list, ","), adapter) {
		t.Skipf("%s=%s excludes %s", envE2EAdapters, list, adapter)
	}

	reg := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: []string{dotnet.Language}})

	m := reg.Adapter(adapter)
	if m == nil || m.Install == nil {
		t.Fatalf("no bundled %s manifest (or it has no install section)", adapter)
	}

	_, hasDownload := m.DownloadFor(runtime.GOOS, runtime.GOARCH)

	found := hasDownload
	if !found && m.Adapter.Runtime == adapters.RuntimeNative {
		_, err := adapters.Find(m)
		found = err == nil
	}

	if skip, reason := dotnetE2ESkip(adapter, runtime.GOOS, runtime.GOARCH, found); skip {
		t.Skip(reason)
	}

	platform := runtime.GOOS + "/" + runtime.GOARCH
	if reason := dotnet.SharpDbgWithheld(platform); adapter == dotnet.SharpDbg && reason != "" {
		t.Skipf("SharpDbg is withheld on %s (drivers/dotnet SharpDbgWithheld): %s", platform, reason)
	}
}

// dotnetE2ESkip decides whether the .NET e2e tests with adapter should
// skip on this platform: found reports whether its bundled manifest has a
// download for goos/goarch, or it can otherwise be found.
func dotnetE2ESkip(adapter, goos, goarch string, found bool) (skip bool, reason string) {
	if !found {
		return true, fmt.Sprintf("%s has no download for %s/%s, and none was otherwise found (its environment variable, installed, or PATH)",
			adapter, goos, goarch)
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
// decides, the adapter and goos/goarch merely shape the reason.
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
		skip, reason := dotnetE2ESkip("netcoredbg", tt.goos, tt.goarch, tt.found)
		if skip != tt.wantSkip {
			t.Errorf("dotnetE2ESkip(%q, %q, %v) skip = %v, want %v", tt.goos, tt.goarch, tt.found, skip, tt.wantSkip)
		}

		if skip && (!strings.Contains(reason, tt.goos+"/"+tt.goarch) || !strings.HasPrefix(reason, "netcoredbg ")) {
			t.Errorf("dotnetE2ESkip(%q, %q, %v) reason = %q, want it to name the adapter and the platform", tt.goos, tt.goarch, tt.found, reason)
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
