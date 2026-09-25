// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// builtinLangs are the languages Go drivers serve in eyedbg.
var builtinLangs = []string{"dotnet"}

func TestBundledManifestsValid(t *testing.T) {
	t.Parallel()

	reg := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs})
	if p := reg.Problems(); len(p) > 0 {
		t.Fatalf("bundled manifests have problems: %+v", p)
	}

	names, err := fs.Glob(Bundled(), "*.json")
	if err != nil {
		t.Fatal(err)
	}

	if len(reg.Adapters()) != len(names) || len(names) == 0 {
		t.Fatalf("loaded %d of %d bundled manifests", len(reg.Adapters()), len(names))
	}

	for _, m := range reg.Adapters() {
		if m.Source != SourceBundled || m.Name+".json" != findName(names, m.Name) {
			t.Errorf("manifest %s: source %q, file names %v (want NAME.json)", m.Name, m.Source, names)
		}
	}

	checkNoBundledConflicts(t, reg)
}

// checkNoBundledConflicts fails if two bundled manifests share a name or a
// language: the registry only conflict-checks user manifests
// (dropConflicts), so a collision between bundled ones would otherwise load
// silently, first by name winning.
func checkNoBundledConflicts(t *testing.T, reg *Registry) {
	t.Helper()

	names, langs := map[string]string{}, map[string]string{}

	for _, m := range reg.Adapters() {
		if other, ok := names[m.Name]; ok {
			t.Errorf("bundled manifests %s and %s share name %q", other, m.Name, m.Name)
		}

		names[m.Name] = m.Name

		if l := m.LanguageName(); l != "" {
			if other, ok := langs[l]; ok {
				t.Errorf("bundled manifests %s and %s share language %q", other, m.Name, l)
			}

			langs[l] = m.Name
		}
	}
}

func findName(names []string, name string) string {
	if slices.Contains(names, name+".json") {
		return name + ".json"
	}

	return ""
}

// TestNetcoredbgManifestUnchanged pins the bundled netcoredbg manifest to
// the values eyedbg used before manifests existed (M5's netcoredbg.go).
func TestNetcoredbgManifestUnchanged(t *testing.T) {
	t.Parallel()

	m := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs}).Adapter("netcoredbg")
	if m == nil {
		t.Fatal("no bundled netcoredbg manifest")
	}

	const base = "https://github.com/Samsung/netcoredbg/releases/download/3.2.0-1092/"

	checkNetcoredbgFields(t, m)

	want := map[string]Download{
		"linux/amd64":   {base + "netcoredbg-linux-amd64.tar.gz", "080eb3b2d2152465f599d3b33d1ee6e747794e11cc0a3773ec689f5e5f2c5afa", archiveTarGz, "netcoredbg", 0},
		"linux/arm64":   {base + "netcoredbg-linux-arm64.tar.gz", "065ff49badec8a695dbea2de6ab6a330c774a191e426a217ab8cc05250627ccb", archiveTarGz, "netcoredbg", 0},
		"darwin/arm64":  {base + "netcoredbg-osx-arm64.zip", "f4fa33b3ff874910cc184b4bb3b9c56d0abdf5c6521cee0b144d7c6e4a6e59ea", archiveZip, "netcoredbg", 0},
		"windows/amd64": {base + "netcoredbg-win64.zip", "3c410a45fa502415203a94fcb88654af65bf8e3dac158a5527a722e7a6b9274a", archiveZip, "netcoredbg", 0},
	}

	if len(m.Install.Downloads) != len(want) {
		t.Fatalf("downloads = %v, want %d", m.Install.Downloads, len(want))
	}

	for key, w := range want {
		goos, goarch, _ := strings.Cut(key, "/")
		if got, ok := m.DownloadFor(goos, goarch); !ok || got != w {
			t.Errorf("download %s = %+v, want %+v", key, got, w)
		}
	}

	if _, ok := m.DownloadFor("darwin", "amd64"); ok {
		t.Error("darwin/amd64 has a download; netcoredbg publishes none")
	}
}

// TestLldbDapManifests pins the three lldb-dap manifests' adapter fields:
// one executable (lldb-dap), one language each.
func TestLldbDapManifests(t *testing.T) {
	t.Parallel()

	reg := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs})

	tests := []struct {
		name, lang       string
		extensions       []string
		hasExceptions    bool
		wantNonCallWords []string
		wantAssignOps    []string
	}{
		{
			"lldb-dap-c", "c",
			[]string{".c", ".h"},
			false,
			[]string{"sizeof", "alignof", "_Alignof"},
			[]string{"=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>=", "++", "--"},
		},
		{
			"lldb-dap-cpp", "cpp",
			[]string{".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx"},
			true,
			[]string{"sizeof", "alignof", "_Alignof", "decltype", "typeid"},
			[]string{"=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>=", "++", "--"},
		},
		{
			"lldb-dap-rust", "rust",
			[]string{".rs"},
			false,
			[]string{"sizeof", "alignof", "_Alignof"},
			[]string{"=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>="},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := reg.Adapter(tt.name)
			if m == nil {
				t.Fatalf("no bundled %s manifest", tt.name)
			}

			checkLldbDapAdapter(t, m, tt.name, tt.lang)
			checkLldbDapLanguageAndTemplates(t, m, tt.name, tt.extensions, tt.hasExceptions)
			checkLldbDapEvalGuard(t, m, tt.name, tt.wantNonCallWords, tt.wantAssignOps)
		})
	}

	if m := reg.Adapter("lldb-dap-cpp"); m == nil || !slices.Equal(m.Exceptions["all"], []string{"cpp_throw"}) || m.Exceptions["uncaught"] != nil {
		t.Fatalf("lldb-dap-cpp exceptions = %+v, want only all: [cpp_throw]", m.Exceptions)
	}
}

// checkLldbDapAdapter checks the fields the three lldb-dap manifests share.
func checkLldbDapAdapter(t *testing.T, m *Manifest, name, lang string) {
	t.Helper()

	a := m.Adapter
	if a.ID != "lldb-dap" || a.Entry != "lldb-dap" || !slices.Equal(a.Args, []string{"--repl-mode", "variable"}) ||
		a.Env != "EYEDBG_LLDB_DAP" || !a.Path || !slices.Equal(a.VersionArgs, []string{"--help"}) ||
		a.Runtime != RuntimeNative || m.LanguageName() != lang || m.Builtin() ||
		m.License != "Apache-2.0 WITH LLVM-exception" || m.Homepage != "https://lldb.llvm.org/use/lldbdap.html" {
		t.Fatalf("%s adapter = %+v, manifest %+v", name, a, m)
	}
}

// checkLldbDapLanguageAndTemplates checks language detection and the
// launch/attach templates.
func checkLldbDapLanguageAndTemplates(t *testing.T, m *Manifest, name string, extensions []string, hasExceptions bool) {
	t.Helper()

	if !slices.Equal(m.Language.Extensions, extensions) {
		t.Errorf("%s extensions = %v, want %v", name, m.Language.Extensions, extensions)
	}

	if (m.Exceptions != nil) != hasExceptions {
		t.Errorf("%s exceptions = %v, want set: %v", name, m.Exceptions, hasExceptions)
	}

	if m.Attach == nil || m.Attach.Arguments["pid"] != "${pid}" || m.Launch == nil ||
		!slices.Equal(m.Launch.Require, []string{"program"}) || m.Launch.Arguments["env"] != "${envList}" {
		t.Fatalf("%s launch/attach = %+v / %+v", name, m.Launch, m.Attach)
	}
}

// checkLldbDapEvalGuard checks the manifest's evalGuard word lists.
func checkLldbDapEvalGuard(t *testing.T, m *Manifest, name string, nonCallWords, assignOps []string) {
	t.Helper()

	if m.EvalGuard == nil || !slices.Equal(m.EvalGuard.NonCallWords, nonCallWords) ||
		!slices.Equal(m.EvalGuard.AssignOps, assignOps) || len(m.EvalGuard.SafeCalls) != 0 {
		t.Fatalf("%s evalGuard = %+v", name, m.EvalGuard)
	}
}

// TestDelveManifest pins the bundled delve manifest's adapter and language
// fields, and checks every download URL names the pinned version (catching
// a half-done version bump) on the five platforms Delve publishes.
func TestDelveManifest(t *testing.T) {
	t.Parallel()

	m := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs}).Adapter("delve")
	if m == nil {
		t.Fatal("no bundled delve manifest")
	}

	checkDelveAdapter(t, m)
	checkDelveDownloads(t, m)

	if m.Attach == nil || m.Attach.Arguments["mode"] != "local" || m.Attach.Arguments["processId"] != "${pid}" {
		t.Fatalf("delve attach = %+v", m.Attach)
	}

	wantExceptions := []string{"unrecovered-panic", "runtime-fatal-throw"}
	if !slices.Equal(m.Exceptions["all"], wantExceptions) || !slices.Equal(m.Exceptions["uncaught"], wantExceptions) {
		t.Fatalf("delve exceptions = %+v", m.Exceptions)
	}
}

// checkDelveAdapter checks the delve manifest's adapter and top-level
// fields.
func checkDelveAdapter(t *testing.T, m *Manifest) {
	t.Helper()

	a := m.Adapter
	if a.ID != "go" || a.Transport != TransportConnect || a.Entry != "dlv" ||
		!slices.Equal(a.Args, []string{"dap", "--client-addr=unix:${socket}"}) ||
		a.Env != "EYEDBG_DLV" || !a.Path || !slices.Equal(a.VersionArgs, []string{"version"}) ||
		a.Runtime != RuntimeNative || m.LanguageName() != "go" || m.Builtin() || m.Version != "1.27.2" ||
		m.License != "MIT" || m.Homepage != "https://github.com/go-delve/delve" {
		t.Fatalf("delve adapter = %+v, manifest %+v", a, m)
	}
}

// checkDelveDownloads checks that every platform Delve publishes is
// present, and every URL names the pinned version (catching a half-done
// version bump).
func checkDelveDownloads(t *testing.T, m *Manifest) {
	t.Helper()

	wantPlatforms := []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64"}
	if len(m.Install.Downloads) != len(wantPlatforms) {
		t.Fatalf("downloads = %v, want exactly %v", m.Install.Downloads, wantPlatforms)
	}

	for _, key := range wantPlatforms {
		goos, goarch, _ := strings.Cut(key, "/")

		d, ok := m.DownloadFor(goos, goarch)
		if !ok {
			t.Errorf("no download for %s", key)

			continue
		}

		if !strings.Contains(d.URL, "/v"+m.Version+"/") || !strings.Contains(d.URL, "dlv_"+m.Version+"_") || d.Root != "" {
			t.Errorf("download %s url = %q, root %q; want /v%s/ and dlv_%s_, root \"\"", key, d.URL, d.Root, m.Version, m.Version)
		}
	}
}

func checkNetcoredbgFields(t *testing.T, m *Manifest) {
	t.Helper()

	if m.Version != "3.2.0-1092" || m.Adapter.Env != "EYEDBG_NETCOREDBG" || m.Adapter.Entry != "netcoredbg" ||
		!slices.Equal(m.Adapter.Args, []string{"--interpreter=vscode"}) || m.Adapter.ID != "coreclr" ||
		!m.Adapter.Path || m.Adapter.Runtime != RuntimeNative || m.LanguageName() != "dotnet" || !m.Builtin() ||
		m.Homepage != "https://github.com/Samsung/netcoredbg" || !slices.Equal(m.Adapter.VersionArgs, []string{"--version"}) ||
		m.Adapter.NotRunningHint != "reinstall it, or check that the .NET runtime it needs is present" {
		t.Fatalf("netcoredbg manifest = %+v, adapter %+v", m, m.Adapter)
	}
}

// writeManifest writes m (JSON-shaped data) to dir/file.
func writeManifest(t *testing.T, dir, file string, m any) string {
	t.Helper()

	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	return p
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

// userDir returns a new, empty user manifest directory.
func userDir(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(privateDir(t), "adapters")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	return dir
}

// bundledNetcoredbg returns the bundled netcoredbg manifest as JSON data.
func bundledNetcoredbg(t *testing.T) map[string]any {
	t.Helper()

	raw, err := fs.ReadFile(Bundled(), "netcoredbg.json")
	if err != nil {
		t.Fatal(err)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	return m
}

// load loads the bundled manifests and dir's.
func load(dir string) *Registry {
	return Load(LoadConfig{UserDir: dir, Builtin: builtinLangs, Bundled: Bundled()})
}

// problemAt returns the problem reported for path, or "".
func problemAt(reg *Registry, path string) string {
	for _, p := range reg.Problems() {
		if p.Path == path {
			return p.Err.Error()
		}
	}

	return ""
}

func TestUserReplacesBundled(t *testing.T) {
	t.Parallel()

	byName := bundledNetcoredbg(t)
	byName["version"] = "9.9.9"

	byLang := bundledNetcoredbg(t)
	byLang["name"] = "mydbg"

	tests := []struct {
		name     string
		manifest map[string]any
		lookup   string
	}{
		{"by name", byName, "netcoredbg"},
		{"by language", byLang, "mydbg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := userDir(t)
			p := writeManifest(t, dir, "mine.json", tt.manifest)
			reg := load(dir)

			m := reg.Language("dotnet")
			if m == nil || m.Name != tt.lookup || m.Source != SourceUser || m.Path != p || m.Replaces != "netcoredbg" {
				t.Fatalf("dotnet manifest = %+v, want the user's %s replacing netcoredbg", m, tt.lookup)
			}

			if tt.lookup != "netcoredbg" && reg.Adapter("netcoredbg") != nil {
				t.Fatal("the replaced bundled netcoredbg is still listed")
			}

			if len(reg.Problems()) > 0 {
				t.Fatalf("problems: %+v", reg.Problems())
			}
		})
	}
}

func TestUserManifestProblems(t *testing.T) {
	t.Parallel()

	nonBuiltinDotnet := validManifest()
	at(nonBuiltinDotnet, "language")["name"] = "dotnet"

	builtinNoDriver := bundledNetcoredbg(t)
	at(builtinNoDriver, "language")["name"] = "cobol"
	builtinNoDriver["name"] = "cobdbg"

	tests := []struct {
		name     string
		manifest any
		want     string
	}{
		{"invalid", map[string]any{"schema": 1}, "name:"},
		{"not JSON", json.RawMessage(`"x"`), "parse:"},
		{"non-builtin claims a Go driver's language", nonBuiltinDotnet, "must set language.builtin"},
		{"builtin without a Go driver", builtinNoDriver, "no built-in driver serves it"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := userDir(t)
			p := writeManifest(t, dir, "bad.json", tt.manifest)
			reg := load(dir)

			if got := problemAt(reg, p); !strings.Contains(got, tt.want) {
				t.Fatalf("problem = %q, want %q (all: %+v)", got, tt.want, reg.Problems())
			}

			if m := reg.Adapter("netcoredbg"); m == nil || m.Source != SourceBundled {
				t.Fatalf("bundled netcoredbg = %+v, want kept", m)
			}
		})
	}
}

func TestUserManifestConflicts(t *testing.T) {
	t.Parallel()

	a, b := validManifest(), validManifest()
	sameLang := validManifest()
	sameLang["name"] = "toy2"

	tests := []struct {
		name  string
		first map[string]any
		other map[string]any
		want  string
	}{
		{"same name", a, b, "also named"},
		{"same language", validManifest(), sameLang, "also serves language"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := userDir(t)
			p1 := writeManifest(t, dir, "one.json", tt.first)
			p2 := writeManifest(t, dir, "two.json", tt.other)
			reg := load(dir)

			if reg.Language("toylang") != nil {
				t.Fatal("a conflicting manifest was loaded")
			}

			for _, p := range []string{p1, p2} {
				if got := problemAt(reg, p); !strings.Contains(got, tt.want) {
					t.Errorf("problem for %s = %q, want %q", filepath.Base(p), got, tt.want)
				}
			}
		})
	}
}

func TestUserDirSkipsAndLimits(t *testing.T) {
	t.Parallel()

	dir := userDir(t)
	writeManifest(t, dir, "toy.json", validManifest())
	writeManifest(t, dir, "notes.txt", map[string]any{"schema": 99})

	if err := os.Mkdir(filepath.Join(dir, "sub.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, maxManifestSize+1), 0o600); err != nil {
		t.Fatal(err)
	}

	reg := load(dir)

	if m := reg.Language("toylang"); m == nil || m.Source != SourceUser {
		t.Fatalf("toy manifest = %+v, want loaded", m)
	}

	if got := problemAt(reg, big); !strings.Contains(got, "larger than") {
		t.Fatalf("big.json problem = %q", got)
	}

	if n := len(reg.Problems()); n != 1 {
		t.Fatalf("problems = %+v, want only big.json's", reg.Problems())
	}

	if reg.Resolve("toy") != reg.Resolve("toylang") || reg.Resolve("nope") != nil {
		t.Fatal("Resolve by name and language disagree")
	}
}

func TestMissingUserDir(t *testing.T) {
	t.Parallel()

	reg := load(filepath.Join(t.TempDir(), "missing"))
	if len(reg.Problems()) > 0 || reg.Adapter("netcoredbg") == nil {
		t.Fatalf("registry = %+v, problems %+v", reg.Adapters(), reg.Problems())
	}
}

func TestUserDirEnv(t *testing.T) {
	dir := privateDir(t)
	t.Setenv(EnvConfigDir, dir)

	got, err := UserDir()
	if err != nil || got != filepath.Join(dir, "adapters") {
		t.Fatalf("UserDir = %q, %v; want %s", got, err, filepath.Join(dir, "adapters"))
	}

	if err := os.Mkdir(got, 0o700); err != nil {
		t.Fatal(err)
	}

	writeManifest(t, got, "toy.json", validManifest())

	if m := Default("dotnet").Language("toylang"); m == nil {
		t.Fatal("Default did not read $EYEDBG_CONFIG_DIR/adapters")
	}
}

// TestSharpdbgManifest pins the bundled sharpdbg manifest: its one
// platform-independent download from nuget.org (the SharpDbg.Cli nupkg,
// SHA-256 and size), its .NET runtime and entry, and its license note (it
// bundles a Microsoft-licensed library: docs/adr/0017). It serves no
// language: the .NET driver chooses it (--adapter sharpdbg).
func TestSharpdbgManifest(t *testing.T) {
	t.Parallel()

	m := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs}).Adapter("sharpdbg")
	if m == nil {
		t.Fatal("no bundled sharpdbg manifest")
	}

	checkSharpdbgAdapter(t, m)

	want := Download{
		URL:     "https://api.nuget.org/v3-flatcontainer/sharpdbg.cli/0.1.17/sharpdbg.cli.0.1.17.nupkg",
		SHA256:  "549fe48fd42dd00af1ab923c2a9d617502dddd1b6461c6dbdbd641561e961309",
		Archive: archiveZip, Root: "tools/net10.0/any", Size: 7951923,
	}
	if len(m.Install.Downloads) != 1 || m.Install.Downloads["*"] != want {
		t.Fatalf("sharpdbg downloads = %+v, want only * = %+v", m.Install.Downloads, want)
	}

	if !strings.Contains(want.URL, "/"+m.Version+"/") || !strings.HasSuffix(want.URL, "."+m.Version+".nupkg") {
		t.Fatalf("download URL %q does not name version %s", want.URL, m.Version)
	}
}

// checkSharpdbgAdapter checks the sharpdbg manifest's adapter and top-level
// fields.
func checkSharpdbgAdapter(t *testing.T, m *Manifest) {
	t.Helper()

	a := m.Adapter
	if a.ID != "coreclr" || a.Runtime != RuntimeDotnet || a.Entry != "SharpDbg.Cli.dll" || a.Env != "EYEDBG_SHARPDBG" ||
		!slices.Equal(a.Args, []string{"--interpreter=vscode"}) || a.Path || len(a.VersionArgs) > 0 {
		t.Fatalf("sharpdbg adapter = %+v", a)
	}

	if m.Dotnet == nil || m.Dotnet.MinRuntime != "10.0" || m.Language != nil || m.Version != "0.1.17" ||
		m.Homepage != "https://github.com/MattParkerDev/sharpdbg" ||
		!strings.Contains(m.License, "Microsoft.VisualStudio.Shared.VSCodeDebugProtocol under Microsoft Software License Terms") {
		t.Fatalf("sharpdbg manifest = %+v", m)
	}
}
