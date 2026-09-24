// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// envE2E gates the real-adapter cases, as in drivers/generic and
// drivers/dotnet (docs/CONVENTIONS.md § Testing).
const envE2E = "EYEDBG_E2E"

// envE2ELangs narrows which languages the end-to-end tests exercise: unset
// runs every language; set (comma-separated) skips a language not listed,
// naming the variable (never silently) — duplicated per package, like
// markerLine.
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

// langCase is one language's script data: what to start, where the
// anchor/run-until/logpoint locations are, and expressions whose values
// and side-effect status are known ahead of time.
type langCase struct {
	name string
	lang string
	// require skips t unless this case can run here.
	require func(t *testing.T)
	// startArgs are the 'eyedbg start' flags naming the program (e.g.
	// --program or --project).
	startArgs func(t *testing.T) []string
	file      string
	anchor    int // the breakpoint 'eyedbg start' sets
	target    int // run-until's location
	// okExpr evaluates without side effects to okValue at the target line.
	okExpr, okValue string
	// sideEffectExpr is flagged as SIDE_EFFECTS at the target line.
	sideEffectExpr string
	// logExpr is a logpoint expression at the anchor line, once re-added
	// with --log; its value need not be predictable, only present.
	logExpr string
	// attachSupported skips the attach step (dotnet: M5's e2e covers it).
	attachSupported bool
}

// langCases returns every language case; each decides for itself whether
// it can run (fake always can; python/dotnet need EYEDBG_E2E=1 and, for
// dotnet, a supported platform).
func langCases(t *testing.T) []langCase {
	t.Helper()

	return []langCase{fakeCase(t), pythonCase(t), dotnetCase(t), cCase(t)}
}

// fakeCase is always on: the fake adapter is this test binary
// (daptest.MaybeRun, main_test.go).
func fakeCase(t *testing.T) langCase {
	t.Helper()

	dir := t.TempDir()
	program := filepath.Join(dir, "prog.fake")

	if err := os.WriteFile(program, []byte("fake program\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return langCase{
		name:    "fake",
		lang:    "fakelang",
		require: func(*testing.T) {},
		startArgs: func(*testing.T) []string {
			// laps=2: a plain 1..10 pass never revisits the anchor line, so
			// the logpoint step (marker3 reuses it) would never fire.
			return []string{"--program", program, "--opt", "laps=2"}
		},
		file: program, anchor: 3, target: 6,
		okExpr: "line", okValue: "6",
		sideEffectExpr: "x = 5",
		logExpr:        "x",
	}
}

// pythonCase needs EYEDBG_E2E=1 (a real Python with debugpy); its script
// positions come from testdata/apps/python/basic/app.py's own markers. Its
// require closure skips before the script ever reads anchor/target/file, so
// the app is copied and its markers scanned only when EYEDBG_E2E=1: without
// it there is no app.py to scan.
func pythonCase(t *testing.T) langCase {
	t.Helper()

	lc := langCase{
		name: "python",
		lang: "python",
		require: func(t *testing.T) {
			t.Helper()

			if os.Getenv(envE2E) != "1" {
				t.Skip("set " + envE2E + "=1 (needs Python 3.10+ with debugpy, or 'eyedbg adapters install debugpy')")
			}

			requireLang(t, "python")
		},
		okExpr: "total", okValue: "10",
		sideEffectExpr:  "total = 5",
		logExpr:         "total",
		attachSupported: false,
	}

	if os.Getenv(envE2E) != "1" {
		return lc
	}

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "apps", "python", "basic"))); err != nil {
		t.Fatal(err)
	}

	_ = os.RemoveAll(filepath.Join(dir, "__pycache__"))

	file := filepath.Join(dir, "app.py")
	lc.file = file
	lc.anchor = markerLine(t, file, "loop-body")
	lc.target = markerLine(t, file, "append")
	lc.startArgs = func(*testing.T) []string { return []string{"--program", file} }

	return lc
}

// dotnetCase needs EYEDBG_E2E=1 and a platform netcoredbg has a download
// for; testdata/apps/dotnet/console/Program.cs has no markers, so its
// lines are literal.
func dotnetCase(t *testing.T) langCase {
	t.Helper()

	dir := t.TempDir()

	if os.Getenv(envE2E) == "1" {
		if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "apps", "dotnet", "console"))); err != nil {
			t.Fatal(err)
		}

		// A build in the source tree (ignored by git) is not the test's
		// (drivers/dotnet's copyApp does the same).
		for _, d := range []string{"bin", "obj"} {
			if err := os.RemoveAll(filepath.Join(dir, d)); err != nil {
				t.Fatal(err)
			}
		}
	}

	file := filepath.Join(dir, "Program.cs")

	return langCase{
		name: "dotnet",
		lang: "dotnet",
		require: func(t *testing.T) {
			t.Helper()
			requireDotnetE2E(t)
		},
		startArgs: func(*testing.T) []string {
			return []string{"--project", dir}
		},
		file: file, anchor: 5, target: 6,
		okExpr: "total", okValue: "1",
		sideEffectExpr:  "total = 5",
		logExpr:         "total",
		attachSupported: true,
	}
}

// requireDotnetE2E skips t unless EYEDBG_E2E=1 and netcoredbg's bundled
// manifest has a download for this platform, or netcoredbg can otherwise be
// found (EYEDBG_NETCOREDBG, already installed, or on PATH) — drivers/dotnet's
// dotnetE2ESkip; duplicated here since that helper is unexported in a
// _test.go file of another package.
func requireDotnetE2E(t *testing.T) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK and 'eyedbg adapters install netcoredbg')")
	}

	requireLang(t, dotnet.Language)

	reg := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: []string{dotnet.Language}})

	m := reg.Language(dotnet.Language)
	if m == nil || m.Install == nil {
		t.Fatal("no bundled netcoredbg manifest for dotnet (or it has no install section)")
	}

	_, hasDownload := m.Install.Downloads[runtime.GOOS+"/"+runtime.GOARCH]
	_, findErr := adapters.Find(m)

	if !hasDownload && findErr != nil {
		t.Skipf("netcoredbg has no download for %s/%s, and none was otherwise found (EYEDBG_NETCOREDBG, installed, or PATH)", runtime.GOOS, runtime.GOARCH)
	}
}

// cCase needs EYEDBG_E2E=1 (a real cc and lldb-dap, or EYEDBG_LLDB_DAP); its
// script positions come from testdata/apps/c/basic/main.c's own markers.
// Unlike python and dotnet there is no bundled download to fall back on: a
// missing cc or lldb-dap fails the test, it doesn't skip. Like pythonCase,
// compiling is deferred until EYEDBG_E2E=1 is confirmed. attachSupported is
// true: a real attach is exercised by drivers/generic's TestCAttach, not
// here.
func cCase(t *testing.T) langCase {
	t.Helper()

	lc := langCase{
		name: "c",
		lang: "c",
		require: func(t *testing.T) {
			t.Helper()

			if os.Getenv(envE2E) != "1" {
				t.Skip("set " + envE2E + "=1 (needs cc and lldb-dap, or EYEDBG_LLDB_DAP)")
			}

			requireLang(t, "c")
		},
		okExpr: "total", okValue: "10",
		sideEffectExpr:  "total = 5",
		logExpr:         "total",
		attachSupported: true,
	}

	if os.Getenv(envE2E) != "1" {
		return lc
	}

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "apps", "c", "basic"))); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(dir, "main.c")
	bin := filepath.Join(dir, "app")

	cmd := exec.CommandContext(t.Context(), "cc", "-g", "-O0", "-o", bin, file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cc -g -O0 -o %s %s: %v\n%s", bin, file, err, out)
	}

	lc.file = file
	lc.anchor = markerLine(t, file, "loop-body")
	lc.target = markerLine(t, file, "append")
	lc.startArgs = func(*testing.T) []string { return []string{"--program", bin} }

	return lc
}

// markerLine returns the line of file that ends in "# marker: NAME" (Python)
// or "// marker: NAME" (C, testdata/apps/c/basic/main.c); duplicated from
// drivers/generic's unexported pyApp.line and lldbApp.line.
func markerLine(t *testing.T, file, marker string) int {
	t.Helper()

	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		if strings.HasSuffix(strings.TrimSpace(sc.Text()), "marker: "+marker) {
			return n
		}
	}

	t.Fatalf("no marker %q in %s", marker, file)

	return 0
}

// fakeManifestFiles writes the fakelang user manifest (this test binary as
// the adapter, EnvFakeAdapter=1; see main_test.go) into dir/adapters,
// created private (internal/adapters/trust_unix.go's permission check):
// dir must not exist yet.
func fakeManifestFiles(t *testing.T, configDir string) {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Under umask 002 t.TempDir's directories are group-writable, which the
	// manifest permission check refuses; both the config dir and its
	// adapters/ subdirectory must be private.
	if err := os.Chmod(configDir, 0o700); err != nil { //nolint:gosec // A directory needs its x bit; 0700 is private.
		t.Fatal(err)
	}

	dir := filepath.Join(configDir, "adapters")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(daptest.UserManifest(exe, ""))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "fakelang.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
