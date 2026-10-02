// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckPlatform(t *testing.T) {
	t.Parallel()

	for _, p := range []string{"linux/amd64", "linux/arm64", "windows/amd64", "darwin/arm64", "plan9/386"} {
		if err := CheckPlatform(p); err != nil {
			t.Errorf("CheckPlatform(%q) = %v", p, err)
		}
	}

	for _, p := range []string{"", "linux", "linux/", "/arm64", "linux/amd64/v8", "Linux/amd64", "linux/../x", "linux/a b", "..//..", "li-nux/amd64", "linux/amd64\n"} {
		if err := CheckPlatform(p); err == nil {
			t.Errorf("CheckPlatform(%q) accepted", p)
		}
	}
}

// foreignPlatform is a platform that is not the host's.
func foreignPlatform() string {
	if HostPlatform() == "linux/arm64" {
		return "linux/amd64"
	}

	return "linux/arm64"
}

// TestInstallForForeignPlatform installs for a platform that is not the
// host's: into _platform/<os>-<arch>, the host's own install untouched, no
// ".exe" for a Linux target, and found by InstalledFor only.
func TestInstallForForeignPlatform(t *testing.T) { //nolint:paralleltest // Sets environment variables.
	data := t.TempDir()
	t.Setenv(EnvDataDir, data)
	t.Setenv(EnvHome, "")
	t.Setenv("EYEDBG_NETCOREDBG", "")

	body := tarGzOf(t, archiveFile{"netcoredbg/netcoredbg", "bin"}, archiveFile{"netcoredbg/lib.so", "x"})
	srv, hits := serve(t, body)
	m := nativeManifest(Download{URL: srv.URL + "/a", SHA256: digest(body), Archive: archiveTarGz, Root: "netcoredbg", Size: int64(len(body))})

	platform := foreignPlatform()
	if _, _, err := InstalledFor(m, platform); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("InstalledFor before the install = %v, want ErrNotInstalled", err)
	}

	got, err := InstallFor(t.Context(), srv.Client(), m, platform)
	if err != nil {
		t.Fatal(err)
	}

	osArch := strings.Replace(platform, "/", "-", 1)
	wantDir := filepath.Join(data, "_platform", osArch, "netcoredbg", "3.2.0-1092")

	if want := filepath.Join(wantDir, "netcoredbg"); got != want {
		t.Errorf("InstallFor = %q, want %q (no .exe for a linux target on any host)", got, want)
	}

	dir, entry, err := InstalledFor(m, platform)
	if err != nil || dir != wantDir || entry != got {
		t.Errorf("InstalledFor = %q, %q, %v", dir, entry, err)
	}

	if _, err := os.Stat(filepath.Join(data, "netcoredbg")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the host's install directory was touched: %v", err)
	}

	// A second call is a no-op, and the host's platform is still not installed.
	if again, err := InstallFor(t.Context(), srv.Client(), m, platform); err != nil || again != got || hits.Load() != 1 {
		t.Errorf("second InstallFor = %q, %v after %d downloads", again, err, hits.Load())
	}

	if _, _, err := InstalledFor(m, HostPlatform()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("InstalledFor(host) = %v, want ErrNotInstalled", err)
	}
}

// TestInstalledForIgnoresEnvAndPath: the host's EYEDBG_NETCOREDBG and PATH
// name programs for the host, never for another platform.
func TestInstalledForIgnoresEnvAndPath(t *testing.T) { //nolint:paralleltest // Sets environment variables.
	data, bin := t.TempDir(), t.TempDir()
	t.Setenv(EnvDataDir, data)

	fake := filepath.Join(bin, exeName("netcoredbg"))
	if err := os.WriteFile(fake, []byte("x"), 0o700); err != nil { //nolint:gosec // A test's executable stand-in.
		t.Fatal(err)
	}

	t.Setenv("EYEDBG_NETCOREDBG", fake)
	t.Setenv("PATH", bin)

	m := nativeManifest(Download{})
	m.Adapter.Path = true

	if _, _, err := InstalledFor(m, foreignPlatform()); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("InstalledFor found %v through the environment or PATH", err)
	}
}

func TestInstallForWindowsTargetHasExe(t *testing.T) { //nolint:paralleltest // Sets environment variables.
	data := t.TempDir()
	t.Setenv(EnvDataDir, data)

	body := zipOf(t, archiveFile{"netcoredbg/netcoredbg.exe", "bin"})
	srv, _ := serve(t, body)
	m := nativeManifest(Download{URL: srv.URL + "/a", SHA256: digest(body), Archive: archiveZip, Root: "netcoredbg", Size: int64(len(body))})

	platform := "windows/amd64"
	if HostPlatform() == platform {
		platform = "windows/arm64"
	}

	got, err := InstallFor(t.Context(), srv.Client(), m, platform)
	if err != nil || filepath.Base(got) != "netcoredbg.exe" {
		t.Errorf("InstallFor(%s) = %q, %v; want netcoredbg.exe", platform, got, err)
	}
}

func TestInstallForNoRelease(t *testing.T) { //nolint:paralleltest // Sets environment variables.
	t.Setenv(EnvDataDir, t.TempDir())

	m := nativeManifest(Download{})
	m.Install.Downloads = map[string]Download{"linux/amd64": {URL: "https://example.invalid/a"}}

	_, err := InstallFor(t.Context(), nil, m, "linux/arm64")
	if err == nil || !strings.Contains(err.Error(), "has no prebuilt release for linux/arm64") {
		t.Errorf("InstallFor = %v, want NoRelease", err)
	}

	if _, err := InstallFor(t.Context(), nil, m, "linux/../x"); err == nil {
		t.Error("InstallFor accepted a platform that isn't OS/ARCH")
	}
}

func TestInstallDirForHostIsInstallDir(t *testing.T) { //nolint:paralleltest // Sets environment variables.
	t.Setenv(EnvDataDir, t.TempDir())

	m := nativeManifest(Download{})

	want, err := InstallDir(m)
	if err != nil {
		t.Fatal(err)
	}

	if got, err := InstallDirFor(m, HostPlatform()); err != nil || got != want {
		t.Errorf("InstallDirFor(host) = %q, %v; want %q", got, err, want)
	}
}
