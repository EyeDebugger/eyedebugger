// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvHome overrides eyedbg's home directory (~/.eyedbg by default), which
// holds both configuration (its adapters/ subdirectory, unless
// EnvConfigDir overrides it) and data (its tools/ subdirectory, unless
// EnvDataDir overrides it). It does not move the runtime directory
// (daemon.EnvRuntimeDir), which is unrelated.
const EnvHome = "EYEDBG_HOME"

// EnvDataDir overrides where adapters are installed.
const EnvDataDir = "EYEDBG_DATA_DIR"

// goosWindows is runtime.GOOS on Windows.
const goosWindows = "windows"

// HomeDir returns eyedbg's home directory: $EYEDBG_HOME, else
// <user home dir>/.eyedbg. Adapters keep their manifests and tools under
// it, and internal/artifacts its private dumps directory.
func HomeDir() (string, error) {
	if dir := os.Getenv(EnvHome); dir != "" {
		return dir, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory (set %s): %w", EnvHome, err)
	}

	return filepath.Join(home, ".eyedbg"), nil
}

// DataDir returns where adapters are installed: $EYEDBG_DATA_DIR, else
// <home>/tools.
func DataDir() (string, error) {
	if dir := os.Getenv(EnvDataDir); dir != "" {
		return dir, nil
	}

	home, err := HomeDir()
	if err != nil {
		return "", fmt.Errorf("locate adapter directory (set %s or %s): %w", EnvDataDir, EnvHome, err)
	}

	return filepath.Join(home, "tools"), nil
}

// InstallDir is where m's pinned version is (or will be) installed:
// <DataDir>/<name>/<version>.
func InstallDir(m *Manifest) (string, error) {
	data, err := DataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(data, m.Name, m.Version), nil
}

// platformDirName is the directory under DataDir holding adapters for
// platforms other than the host's: a leading '_' can't start a manifest name,
// so it can't collide with an adapter's directory.
const platformDirName = "_platform"

// HostPlatform is this machine's platform, "os/arch" (GOOS/GOARCH).
func HostPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }

// CheckPlatform checks that platform is "os/arch" of lowercase letters and
// digits (Go's GOOS/GOARCH names, which docker's os and architecture also
// use), so it is safe in a directory name.
func CheckPlatform(platform string) error {
	goos, arch, ok := strings.Cut(platform, "/")
	if ok && goos != "" && arch != "" && isPlatformName(goos) && isPlatformName(arch) {
		return nil
	}

	return fmt.Errorf("platform %q is not OS/ARCH (for example linux/arm64)", platform)
}

func isPlatformName(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool { return (r < 'a' || r > 'z') && (r < '0' || r > '9') })
}

// InstallDirFor is where m's pinned version is (or will be) installed for
// platform ("os/arch"): InstallDir for the host's, else
// <DataDir>/_platform/<os>-<arch>/<name>/<version>.
func InstallDirFor(m *Manifest, platform string) (string, error) {
	if err := CheckPlatform(platform); err != nil {
		return "", err
	}

	if platform == HostPlatform() {
		return InstallDir(m)
	}

	data, err := DataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(data, platformDirName, strings.Replace(platform, "/", "-", 1), m.Name, m.Version), nil
}

// installedEntry is the path of m's entry inside its install directory for
// the host.
func installedEntry(m *Manifest, dir string) string { return installedEntryFor(m, dir, runtime.GOOS) }

// installedEntryFor is installedEntry for a program that runs on goos: only
// a Windows target's native executable has ".exe".
func installedEntryFor(m *Manifest, dir, goos string) string {
	if m.Adapter.Runtime == RuntimePython || m.Adapter.Runtime == RuntimeDotnet {
		return filepath.Join(dir, filepath.FromSlash(m.Adapter.Entry))
	}

	if goos == goosWindows {
		return filepath.Join(dir, m.Adapter.Entry+".exe")
	}

	return filepath.Join(dir, m.Adapter.Entry)
}

// exeName adds ".exe" on Windows.
func exeName(name string) string {
	if runtime.GOOS == goosWindows {
		return name + ".exe"
	}

	return name
}
