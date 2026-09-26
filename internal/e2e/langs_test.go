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
	"github.com/eyedebugger/eyedebugger/internal/api"
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

	return []langCase{fakeCase(t), pythonCase(t), dotnetCase(t), sharpdbgCase(t), cCase(t), goCase(t)}
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

// dotnetCase needs EYEDBG_E2E=1 and a dotnet adapter this platform
// defaults to (requireDotnetE2E); testdata/apps/dotnet/console/Program.cs
// has no markers, so its lines are literal. It starts with the default
// adapter (no --adapter): netcoredbg, or SharpDbg where netcoredbg has no
// build.
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

// sharpdbgCase is dotnetCase with --adapter sharpdbg, wherever SharpDbg
// can run (requireSharpDbgE2E).
func sharpdbgCase(t *testing.T) langCase {
	t.Helper()

	lc := dotnetCase(t)
	startArgs := lc.startArgs

	lc.name = dotnet.SharpDbg
	lc.require = func(t *testing.T) {
		t.Helper()
		requireSharpDbgE2E(t)
	}
	lc.startArgs = func(t *testing.T) []string {
		t.Helper()

		return append(startArgs(t), "--adapter", dotnet.SharpDbg)
	}

	return lc
}

// expectE2EExitCode checks a --json snapshot's session exit code against
// want, unless its adapter's exit codes can't be trusted here
// (dotnet.ExitCodeUnknown): then it requires none, and an end reason
// saying so — for the dotnet cases' shared 'eyedbg --json' checks (dumps,
// trace).
func expectE2EExitCode(t *testing.T, info api.SessionInfo, want int) {
	t.Helper()

	if why := dotnet.ExitCodeUnknown(info.Adapter, runtime.GOOS); why != "" {
		if info.ExitCode != nil {
			t.Errorf("exit code = %d, want none (%s)", *info.ExitCode, why)
		}

		if !strings.Contains(info.EndReason, "exit code is unknown") {
			t.Errorf("end reason = %q, want it to say the exit code is unknown", info.EndReason)
		}

		return
	}

	if info.ExitCode == nil || *info.ExitCode != want {
		t.Errorf("exit code = %v, want %d", info.ExitCode, want)
	}
}

// envE2EAdapters narrows which .NET adapters the end-to-end tests run
// with, as in drivers/dotnet: unset runs both; set (comma-separated) skips
// an adapter not listed, naming the variable.
const envE2EAdapters = "EYEDBG_E2E_DOTNET_ADAPTERS"

// requireDotnetAdapter skips t unless EYEDBG_E2E=1, EYEDBG_E2E_LANGS allows
// dotnet and EYEDBG_E2E_DOTNET_ADAPTERS allows adapter.
func requireDotnetAdapter(t *testing.T, adapter string) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK, 'eyedbg adapters install netcoredbg' and 'eyedbg adapters install sharpdbg')")
	}

	requireLang(t, dotnet.Language)

	if list := os.Getenv(envE2EAdapters); list != "" && !slices.Contains(strings.Split(list, ","), adapter) {
		t.Skipf("%s=%s excludes %s", envE2EAdapters, list, adapter)
	}
}

// requireDotnetE2E skips t unless the adapter a dotnet session uses here by
// default can run (requireDotnetAdapter): netcoredbg where its bundled
// manifest has a download for this platform or it can otherwise be found
// (EYEDBG_NETCOREDBG, already installed, or on PATH), else SharpDbg (the
// driver's default there, once installed), unless the driver withholds that
// default on this platform. Uninstalled, the adapter fails the test.
func requireDotnetE2E(t *testing.T) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET SDK, 'eyedbg adapters install netcoredbg' and 'eyedbg adapters install sharpdbg')")
	}

	reg := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: []string{dotnet.Language}})

	m := reg.Language(dotnet.Language)
	if m == nil || m.Install == nil {
		t.Fatal("no bundled netcoredbg manifest for dotnet (or it has no install section)")
	}

	_, hasDownload := m.Install.Downloads[runtime.GOOS+"/"+runtime.GOARCH]
	_, findErr := adapters.Find(m)

	if hasDownload || findErr == nil {
		requireDotnetAdapter(t, m.Name)

		return
	}

	platform := runtime.GOOS + "/" + runtime.GOARCH
	if reason := dotnet.SharpDbgWithheld(platform); reason != "" {
		t.Skipf("netcoredbg has no download for %s, none was otherwise found, and SharpDbg is withheld there: %s", platform, reason)
	}

	requireDotnetAdapter(t, dotnet.SharpDbg)
}

// requireSharpDbgE2E skips t unless SharpDbg's e2e can run here
// (requireDotnetAdapter), and the driver doesn't withhold SharpDbg on this
// platform. Uninstalled, SharpDbg fails the test.
func requireSharpDbgE2E(t *testing.T) {
	t.Helper()

	requireDotnetAdapter(t, dotnet.SharpDbg)

	platform := runtime.GOOS + "/" + runtime.GOARCH
	if reason := dotnet.SharpDbgWithheld(platform); reason != "" {
		t.Skipf("SharpDbg is withheld on %s (drivers/dotnet SharpDbgWithheld): %s", platform, reason)
	}
}

// cCase needs EYEDBG_E2E=1 (a real cc and lldb-dap, or EYEDBG_LLDB_DAP); its
// script positions come from testdata/apps/c/basic/main.c's own markers.
// Unlike python and dotnet there is no bundled download to fall back on: a
// missing cc or lldb-dap fails the test, it doesn't skip. Like pythonCase,
// compiling is deferred until EYEDBG_E2E=1 is confirmed. attachSupported is
// true: a real attach is exercised by drivers/generic's TestCAttach, not
// here. The build directory is resolved with filepath.EvalSymlinks (see
// drivers/generic/e2e_lldb_test.go's compileApp): on macOS t.TempDir() is
// under /var/folders, a symlink to /private/var, and lldb-dap's full-path
// match against the session's resolved breakpoint path would otherwise never
// bind.
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

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

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

// goCase needs EYEDBG_E2E=1 (a real go and Delve, or EYEDBG_DLV, or a
// managed install) and connects through schema 1's connect transport
// (Step 7): its script positions come from testdata/apps/go/basic/main.go's
// own markers. --program is the package directory (Delve's debug mode
// builds it), so --cwd is also given: workDir would otherwise default to
// the client's own directory (drivers/generic/launch.go's workDir), not
// --program's directory, so naming --cwd explicitly pins Delve's build to
// the package directory, matching how a real invocation would run it. Unlike
// python and dotnet there is no bundled download to fall back on except
// Delve's own managed install: a missing go or dlv fails the test, it
// doesn't skip. attachSupported is true: a real attach is exercised by
// drivers/generic's TestGoAttach, not here.
func goCase(t *testing.T) langCase {
	t.Helper()

	lc := langCase{
		name: "go",
		lang: "go",
		require: func(t *testing.T) {
			t.Helper()

			if os.Getenv(envE2E) != "1" {
				t.Skip("set " + envE2E + "=1 (needs go and dlv, or EYEDBG_DLV, or 'eyedbg adapters install delve')")
			}

			requireLang(t, "go")
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
	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "apps", "go", "basic"))); err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(dir, "main.go")

	lc.file = file
	lc.anchor = markerLine(t, file, "loop-body")
	lc.target = markerLine(t, file, "append")
	lc.startArgs = func(*testing.T) []string { return []string{"--program", dir, "--cwd", dir} }

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
