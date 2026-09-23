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
