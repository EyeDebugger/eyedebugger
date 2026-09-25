// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// dotnetFileInfo is a file or directory for a fake stat (fakeDotnet).
type dotnetFileInfo struct{ dir bool }

func (f dotnetFileInfo) Name() string       { return "x" }
func (f dotnetFileInfo) Size() int64        { return 1 }
func (f dotnetFileInfo) ModTime() time.Time { return time.Time{} }
func (f dotnetFileInfo) IsDir() bool        { return f.dir }
func (f dotnetFileInfo) Sys() any           { return nil }

func (f dotnetFileInfo) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o755
	}

	return 0o644
}

// fakeDotnet is a dotnetEnv over fake files, PATH and a host's runtimes.
type fakeDotnet struct {
	goos     string
	env      map[string]string
	path     map[string]string // command name → path on PATH
	files    map[string]bool   // existing regular files
	dirs     map[string]bool   // existing directories
	home     string
	runtimes string // what --list-runtimes prints
	listErr  error
	listed   *[]string // hosts --list-runtimes ran on
}

func (f fakeDotnet) dotnetEnv() dotnetEnv {
	return dotnetEnv{
		goos:   f.goos,
		getenv: func(k string) string { return f.env[k] },
		lookPath: func(name string) (string, error) {
			if p, ok := f.path[name]; ok {
				return p, nil
			}

			return "", errors.New("not found")
		},
		stat: func(p string) (fs.FileInfo, error) {
			switch {
			case f.files[p]:
				return dotnetFileInfo{}, nil
			case f.dirs[p]:
				return dotnetFileInfo{dir: true}, nil
			default:
				return nil, fs.ErrNotExist
			}
		},
		userHomeDir: func() (string, error) {
			if f.home == "" {
				return "", errors.New("no home")
			}

			return f.home, nil
		},
		dataDir: func() (string, error) { return filepath.FromSlash("/data"), nil },
		listRuntimes: func(_ context.Context, host, _ string) (string, error) {
			if f.listed != nil {
				*f.listed = append(*f.listed, host)
			}

			return f.runtimes, f.listErr
		},
	}
}

// toyDotnet is a parsed .NET adapter manifest (validDotnetManifest).
func toyDotnet(t *testing.T) *Manifest {
	t.Helper()

	m := &Manifest{
		Schema: 1, Name: "toydbg", Description: "d", Version: "0.1.0",
		Adapter: Adapter{ID: "coreclr", Runtime: RuntimeDotnet, Entry: "tools/Toy.Cli.dll", Env: "EYEDBG_TOYDBG"},
		Dotnet:  &Dotnet{MinRuntime: "10.0"},
		Install: &InstallSpec{Downloads: map[string]Download{"*": {
			URL: "https://example.com/t.nupkg", SHA256: strings.Repeat("ab", 32), Archive: archiveZip,
		}}},
	}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}

	return m
}

// Real 'dotnet --list-runtimes' output (macOS, Linux and Windows shapes).
const (
	runtimesMac = `Microsoft.AspNetCore.App 10.0.10 [/opt/homebrew/Cellar/dotnet/10.0.302/libexec/shared/Microsoft.AspNetCore.App]
Microsoft.NETCore.App 10.0.10 [/opt/homebrew/Cellar/dotnet/10.0.302/libexec/shared/Microsoft.NETCore.App]
`
	runtimes8 = `Microsoft.AspNetCore.App 8.0.19 [/usr/share/dotnet/shared/Microsoft.AspNetCore.App]
Microsoft.NETCore.App 8.0.19 [/usr/share/dotnet/shared/Microsoft.NETCore.App]
`
	runtimesWindows = "Microsoft.AspNetCore.App 9.0.8 [C:\\Program Files\\dotnet\\shared\\Microsoft.AspNetCore.App]\r\n" +
		"Microsoft.NETCore.App 8.0.19 [C:\\Program Files\\dotnet\\shared\\Microsoft.NETCore.App]\r\n" +
		"Microsoft.NETCore.App 10.0.12 [C:\\Program Files\\dotnet\\shared\\Microsoft.NETCore.App]\r\n" +
		"Microsoft.WindowsDesktop.App 10.0.12 [C:\\Program Files\\dotnet\\shared\\Microsoft.WindowsDesktop.App]\r\n"
	runtimesPreview = `Microsoft.NETCore.App 9.0.8 [/usr/share/dotnet/shared/Microsoft.NETCore.App]
Microsoft.NETCore.App 11.0.0-preview.1.26104.118 [/usr/share/dotnet/shared/Microsoft.NETCore.App]
`
)

func TestNetCoreVersions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, out string
		want      []string
		best      string // the newest at least 10.0
	}{
		{"macOS 10.0.10", runtimesMac, []string{"10.0.10"}, "10.0.10"},
		{"8.0 only", runtimes8, []string{"8.0.19"}, ""},
		{"Windows, CRLF, several", runtimesWindows, []string{"8.0.19", "10.0.12"}, "10.0.12"},
		{"11.0 preview", runtimesPreview, []string{"9.0.8", "11.0.0-preview.1.26104.118"}, "11.0.0-preview.1.26104.118"},
		{"empty", "", nil, ""},
		{"garbage", "dotnet: command not found\n" + netCoreApp + "\n" + netCoreApp + " ten [x]\n", nil, ""},
		{"duplicates", runtimesMac + runtimesMac, []string{"10.0.10"}, "10.0.10"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := netCoreVersions(tt.out)
			if !slices.Equal(got, tt.want) {
				t.Errorf("netCoreVersions = %q, want %q", got, tt.want)
			}

			if best := newestAtLeast(got, "10.0"); best != tt.best {
				t.Errorf("newestAtLeast(%q, 10.0) = %q, want %q", got, best, tt.best)
			}
		})
	}
}

func TestNewestAtLeast(t *testing.T) {
	t.Parallel()

	tests := []struct {
		versions []string
		minimum  string
		want     string
	}{
		{[]string{"10.0.2", "10.0.10", "10.0.9"}, "10.0", "10.0.10"},
		{[]string{"10.1.0", "10.0.99"}, "10.0", "10.1.0"},
		{[]string{"11.0.0-preview.2", "11.0.0"}, "10.0", "11.0.0"},
		{[]string{"11.0.0-preview.1", "11.0.0-preview.2"}, "10.0", "11.0.0-preview.2"},
		{[]string{"9.0.8", "10.0.0"}, "10.1", ""},
		{[]string{"10.0.0"}, "10.0", "10.0.0"},
		{nil, "10.0", ""},
	}

	for _, tt := range tests {
		if got := newestAtLeast(tt.versions, tt.minimum); got != tt.want {
			t.Errorf("newestAtLeast(%q, %q) = %q, want %q", tt.versions, tt.minimum, got, tt.want)
		}
	}
}

func TestFindDotnetHost(t *testing.T) {
	t.Parallel()

	root := filepath.FromSlash("/opt/dn")
	home := filepath.FromSlash("/home/u")

	tests := []struct {
		name string
		f    fakeDotnet
		want string // "" for not found
	}{
		{"DOTNET_HOST_PATH first, unchecked", fakeDotnet{
			env: map[string]string{"DOTNET_HOST_PATH": "/x/dotnet", "DOTNET_ROOT": root}, path: map[string]string{"dotnet": "/bin/dotnet"},
		}, "/x/dotnet"},
		{"PATH before DOTNET_ROOT", fakeDotnet{
			env: map[string]string{"DOTNET_ROOT": root}, path: map[string]string{"dotnet": "/bin/dotnet"},
			files: map[string]bool{filepath.Join(root, "dotnet"): true},
		}, "/bin/dotnet"},
		{"DOTNET_ROOT", fakeDotnet{
			env: map[string]string{"DOTNET_ROOT": root}, home: home,
			files: map[string]bool{filepath.Join(root, "dotnet"): true, filepath.Join(home, ".dotnet", "dotnet"): true},
		}, filepath.Join(root, "dotnet")},
		{"~/.dotnet", fakeDotnet{home: home, files: map[string]bool{filepath.Join(home, ".dotnet", "dotnet"): true}}, filepath.Join(home, ".dotnet", "dotnet")},
		{"windows adds .exe", fakeDotnet{goos: goosWindows, path: map[string]string{"dotnet.exe": `C:\dn\dotnet.exe`}}, `C:\dn\dotnet.exe`},
		{"DOTNET_ROOT without a host", fakeDotnet{env: map[string]string{"DOTNET_ROOT": root}}, ""},
		{"none", fakeDotnet{home: home}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.f.goos == "" {
				tt.f.goos = "linux"
			}

			got, err := tt.f.dotnetEnv().findHost()

			switch {
			case tt.want == "" && api.CodeOf(err) != api.CodeAdapterMissing:
				t.Fatalf("findHost = %q, %v; want ADAPTER_NOT_INSTALLED", got, err)
			case tt.want != "" && (err != nil || got != tt.want):
				t.Fatalf("findHost = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestResolveDotnet(t *testing.T) {
	t.Parallel()

	installed := filepath.Join(filepath.FromSlash("/data"), "toydbg", "0.1.0", "tools", "Toy.Cli.dll")
	own := filepath.FromSlash("/src/bin/Toy.Cli.dll")
	host := filepath.FromSlash("/bin/dotnet")
	onPath := map[string]string{"dotnet": host}

	tests := []struct {
		name       string
		f          fakeDotnet
		wantEntry  string
		wantSource string
		// wantErr is in the error; notInstalled means errors.Is(err,
		// ErrNotInstalled).
		wantErr      string
		notInstalled bool
	}{
		{
			"installed",
			fakeDotnet{path: onPath, files: map[string]bool{installed: true}, runtimes: runtimesMac},
			installed, FoundInstalled, "", false,
		},
		{"env before installed", fakeDotnet{
			env: map[string]string{"EYEDBG_TOYDBG": own}, path: onPath, files: map[string]bool{installed: true, own: true}, runtimes: runtimesMac,
		}, own, FoundEnv, "", false},
		{"env missing is broken, not missing", fakeDotnet{
			env: map[string]string{"EYEDBG_TOYDBG": own}, path: onPath, files: map[string]bool{installed: true}, runtimes: runtimesMac,
		}, "", "", "EYEDBG_TOYDBG=" + own + ": not found", false},
		{"env not a dll", fakeDotnet{
			env: map[string]string{"EYEDBG_TOYDBG": filepath.FromSlash("/src/toydbg")}, path: onPath,
			files: map[string]bool{filepath.FromSlash("/src/toydbg"): true}, runtimes: runtimesMac,
		}, "", "", "not a .dll", false},
		{"env a directory", fakeDotnet{
			env: map[string]string{"EYEDBG_TOYDBG": own}, path: onPath, dirs: map[string]bool{own: true}, runtimes: runtimesMac,
		}, "", "", "not a file", false},
		{
			"installed entry a directory",
			fakeDotnet{path: onPath, dirs: map[string]bool{installed: true}, runtimes: runtimesMac},
			"", "", "toydbg is not installed", true,
		},
		{"not installed", fakeDotnet{path: onPath, runtimes: runtimesMac}, "", "", "toydbg is not installed", true},
		{"no host", fakeDotnet{files: map[string]bool{installed: true}}, "", "", "the dotnet host was not found", false},
		{
			"runtime too old",
			fakeDotnet{path: onPath, files: map[string]bool{installed: true}, runtimes: runtimes8},
			"", "", "needs the .NET 10.0+ runtime, and " + host + " has Microsoft.NETCore.App 8.0.19", false,
		},
		{
			"no runtimes",
			fakeDotnet{path: onPath, files: map[string]bool{installed: true}},
			"", "", "has Microsoft.NETCore.App none", false,
		},
		{
			"host fails",
			fakeDotnet{path: onPath, files: map[string]bool{installed: true}, listErr: errors.New("exit status 1")},
			"", "", "--list-runtimes failed: exit status 1", false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tt.f.goos = "linux"

			var listed []string

			tt.f.listed = &listed

			rt, err := tt.f.dotnetEnv().resolve(t.Context(), toyDotnet(t))
			if tt.wantErr != "" {
				checkResolveError(t, err, tt.wantErr, tt.notInstalled, listed)

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			want := DotnetRuntime{Host: host, Entry: tt.wantEntry, Source: tt.wantSource, Runtime: "10.0.10"}
			if rt != want {
				t.Errorf("resolve = %+v, want %+v", rt, want)
			}
		})
	}
}

// checkResolveError checks a failed resolve's error: it contains want, is
// ADAPTER_NOT_INSTALLED, and is ErrNotInstalled iff notInstalled (then the
// host was never run: listed is empty).
func checkResolveError(t *testing.T, err error, want string, notInstalled bool, listed []string) {
	t.Helper()

	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("resolve err = %v; want an error containing %q", err, want)
	}

	if api.CodeOf(err) != api.CodeAdapterMissing {
		t.Errorf("code = %q, want ADAPTER_NOT_INSTALLED", api.CodeOf(err))
	}

	if got := errors.Is(err, ErrNotInstalled); got != notInstalled {
		t.Errorf("errors.Is(err, ErrNotInstalled) = %v, want %v", got, notInstalled)
	}

	if notInstalled && len(listed) > 0 {
		t.Errorf("--list-runtimes ran for a missing adapter: %q", listed)
	}
}

func TestResolveDotnetRefusesOtherRuntimes(t *testing.T) {
	t.Parallel()

	m := toyDotnet(t)
	m.Adapter.Runtime = RuntimeNative

	if _, err := (fakeDotnet{}).dotnetEnv().resolve(t.Context(), m); err == nil {
		t.Fatal("resolve accepted a native adapter")
	}
}

func TestDotnetInstallHint(t *testing.T) {
	t.Parallel()

	m := toyDotnet(t)

	got := dotnetInstallHint(m)
	for _, want := range []string{"eyedbg adapters install toydbg", "downloads toydbg 0.1.0", "set EYEDBG_TOYDBG to its Toy.Cli.dll"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q lacks %q", got, want)
		}
	}
}
