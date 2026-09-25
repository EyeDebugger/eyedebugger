// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

// installedEntry is the path of m's entry inside its install directory.
func installedEntry(m *Manifest, dir string) string {
	if m.Adapter.Runtime == RuntimePython {
		return filepath.Join(dir, filepath.FromSlash(m.Adapter.Entry))
	}

	return filepath.Join(dir, exeName(m.Adapter.Entry))
}

// exeName adds ".exe" on Windows.
func exeName(name string) string {
	if runtime.GOOS == goosWindows {
		return name + ".exe"
	}

	return name
}
