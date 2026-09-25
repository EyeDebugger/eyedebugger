// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// fakeFile is a regular file or directory for helperPath's stat.
type fakeFile struct{ dir bool }

func (f fakeFile) Name() string       { return "" }
func (f fakeFile) Size() int64        { return 0 }
func (f fakeFile) Mode() fs.FileMode  { return map[bool]fs.FileMode{true: fs.ModeDir}[f.dir] }
func (f fakeFile) ModTime() time.Time { return time.Time{} }
func (f fakeFile) IsDir() bool        { return f.dir }
func (f fakeFile) Sys() any           { return nil }

func TestHelperPath(t *testing.T) {
	t.Parallel()

	root := t.TempDir() // absolute on every OS; nothing is created in it
	bin := filepath.Join(root, "bin")
	cellar := filepath.Join(root, "cellar", "eyedbg", "1.0", "bin")
	nextToBin := filepath.Join(bin, "helpers", "dotnet", HelperDLL)
	nextToCellar := filepath.Join(cellar, "helpers", "dotnet", HelperDLL)
	custom := filepath.Join(root, "custom", HelperDLL)
	customExe := filepath.Join(root, "custom", "helper.exe")
	customBat := filepath.Join(root, "custom", "helper.bat")
	customBin := filepath.Join(root, "custom", "fake-helper")

	noLinks := func(p string) (string, error) { return p, nil }
	linked := func(string) (string, error) { return filepath.Join(cellar, "eyedbg"), nil }
	brokenLinks := func(string) (string, error) { return "", errors.New("loop") }

	tests := []struct {
		name       string
		env, exe   string
		eval       func(string) (string, error)
		files      map[string]bool // path → is a directory
		goos       string
		want       string
		wantDirect bool
		wantErr    []string // HELPER_NOT_FOUND message parts
	}{
		{name: "env dll", env: custom, files: map[string]bool{custom: false}, want: custom},
		{name: "env executable", env: customBin, files: map[string]bool{customBin: false}, want: customBin, wantDirect: true},
		{name: "env exe on windows", env: customExe, files: map[string]bool{customExe: false}, goos: "windows", want: customExe, wantDirect: true},
		{name: "env DLL case", env: strings.TrimSuffix(custom, ".dll") + ".DLL", files: map[string]bool{strings.TrimSuffix(custom, ".dll") + ".DLL": false}, want: strings.TrimSuffix(custom, ".dll") + ".DLL"},
		{name: "env wins over next to eyedbg", env: custom, exe: filepath.Join(bin, "eyedbg"), files: map[string]bool{custom: false, nextToBin: false}, want: custom},
		{name: "env relative", env: filepath.Join("custom", HelperDLL), wantErr: []string{"$" + EnvHelper + " must be an absolute path"}},
		{name: "env missing", env: custom, wantErr: []string{"$" + EnvHelper + " names " + custom + ", which isn't a file"}},
		{name: "env directory", env: custom, files: map[string]bool{custom: true}, wantErr: []string{"which isn't a file"}},
		{name: "env bat on windows", env: customBat, files: map[string]bool{customBat: false}, goos: "windows", wantErr: []string{"must be a .dll or .exe"}},
		{name: "env no extension on windows", env: customBin, files: map[string]bool{customBin: false}, goos: "windows", wantErr: []string{"must be a .dll or .exe"}},
		{name: "next to eyedbg", exe: filepath.Join(bin, "eyedbg"), files: map[string]bool{nextToBin: false}, want: nextToBin},
		{name: "next to a symlinked eyedbg's target", exe: filepath.Join(bin, "eyedbg"), eval: linked, files: map[string]bool{nextToCellar: false}, want: nextToCellar},
		{name: "the link's own dir first", exe: filepath.Join(bin, "eyedbg"), eval: linked, files: map[string]bool{nextToBin: false, nextToCellar: false}, want: nextToBin},
		{
			name: "nowhere", exe: filepath.Join(bin, "eyedbg"), eval: linked,
			wantErr: []string{"isn't installed: looked in " + filepath.Dir(nextToBin) + " and " + filepath.Dir(nextToCellar)},
		},
		{name: "broken link", exe: filepath.Join(bin, "eyedbg"), eval: brokenLinks, wantErr: []string{"looked in " + filepath.Dir(nextToBin)}},
		{name: "a directory named like the dll", exe: filepath.Join(bin, "eyedbg"), files: map[string]bool{nextToBin: true}, wantErr: []string{"isn't installed"}},
		{name: "unknown executable", wantErr: []string{"looked in nowhere"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eval := tt.eval
			if eval == nil {
				eval = noLinks
			}

			goos := tt.goos
			if goos == "" {
				goos = "linux"
			}

			got, direct, err := helperPath(tt.env, tt.exe, eval, fakeStat(tt.files), goos)
			if len(tt.wantErr) > 0 {
				requireNotFound(t, err, tt.wantErr...)

				return
			}

			if err != nil || got != tt.want || direct != tt.wantDirect {
				t.Errorf("helperPath = %q, %v, %v; want %q, %v", got, direct, err, tt.want, tt.wantDirect)
			}
		})
	}
}

// fakeStat stats files, a map from path to whether it is a directory.
func fakeStat(files map[string]bool) func(string) (fs.FileInfo, error) {
	return func(p string) (fs.FileInfo, error) {
		if dir, ok := files[p]; ok {
			return fakeFile{dir: dir}, nil
		}

		return nil, fs.ErrNotExist
	}
}

func requireNotFound(t *testing.T, err error, parts ...string) {
	t.Helper()

	e, ok := errors.AsType[*api.Error](err)
	if !ok || e.Code != api.CodeHelperNotFound || e.Hint == "" {
		t.Fatalf("err = %v, want HELPER_NOT_FOUND with a hint", err)
	}

	for _, s := range parts {
		if !strings.Contains(e.Message, s) {
			t.Errorf("message %q lacks %q", e.Message, s)
		}
	}
}

func TestRuntimeMissing(t *testing.T) {
	t.Parallel()

	tests := []struct {
		goos string
		code int
		want bool
	}{
		{"linux", 150, true},
		{"darwin", 150, true},
		{"windows", 150, false},
		{"windows", 0x80008096, true}, // os/exec's ExitCode on Windows: the DWORD, as a positive int
		{"linux", 1, false},
		{"linux", -1, false},
		{"windows", 0, false},
	}

	for _, tt := range tests {
		e := runtimeMissing(tt.goos)(tt.code)
		if (e != nil) != tt.want || (e != nil && e.Code != api.CodeHelperNotFound) {
			t.Errorf("runtimeMissing(%s)(%d) = %v, want missing %v", tt.goos, tt.code, e, tt.want)
		}
	}
}

// TestHelperSpecWithoutHost can't run in parallel: it empties the
// environment the host lookup reads.
func TestHelperSpecWithoutHost(t *testing.T) {
	dir := t.TempDir()
	dll := filepath.Join(dir, HelperDLL)

	if err := os.WriteFile(dll, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvHelper, dll)
	t.Setenv("DOTNET_HOST_PATH", "")
	t.Setenv("DOTNET_ROOT", "")
	t.Setenv("PATH", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	_, err := HelperSpec()

	e, ok := errors.AsType[*api.Error](err)
	if !ok || e.Code != api.CodeHelperNotFound || !strings.Contains(e.Message, "no dotnet host") || !strings.Contains(e.Hint, "install .NET 8 or later") {
		t.Fatalf("err = %v, want HELPER_NOT_FOUND about the dotnet host", err)
	}
}

func TestHelperSpecDirect(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "fake-helper.exe")

	if err := os.WriteFile(exe, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvHelper, exe)

	spec, err := HelperSpec()
	if err != nil || spec.Path != exe || len(spec.Args) != 0 || spec.Protocol != HelperProtocol || spec.RuntimeMissing == nil {
		t.Fatalf("HelperSpec() = %+v, %v", spec, err)
	}
}
