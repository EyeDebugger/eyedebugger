// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDataDirDefault checks DataDir falls back to <user home>/.eyedbg/tools
// when neither EYEDBG_DATA_DIR nor EYEDBG_HOME is set. It only computes the
// expected path from os.UserHomeDir; it never touches the real home
// directory.
func TestDataDirDefault(t *testing.T) {
	t.Setenv(EnvDataDir, "")
	t.Setenv(EnvHome, "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}

	got, err := DataDir()
	want := filepath.Join(home, ".eyedbg", "tools")

	if err != nil || got != want {
		t.Fatalf("DataDir = %q, %v; want %q", got, err, want)
	}
}

// TestDataDirHome checks EYEDBG_HOME moves DataDir under <home>/tools.
func TestDataDirHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	t.Setenv(EnvDataDir, "")

	got, err := DataDir()
	want := filepath.Join(dir, "tools")

	if err != nil || got != want {
		t.Fatalf("DataDir = %q, %v; want %q", got, err, want)
	}
}

// TestDataDirEnvOverridesHome checks EYEDBG_DATA_DIR wins over EYEDBG_HOME.
func TestDataDirEnvOverridesHome(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvDataDir, data)

	got, err := DataDir()
	if err != nil || got != data {
		t.Fatalf("DataDir = %q, %v; want %q", got, err, data)
	}
}

// TestUserDirHome checks EYEDBG_HOME moves UserDir under <home>/adapters.
func TestUserDirHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvHome, dir)
	t.Setenv(EnvConfigDir, "")

	got, err := UserDir()
	want := filepath.Join(dir, "adapters")

	if err != nil || got != want {
		t.Fatalf("UserDir = %q, %v; want %q", got, err, want)
	}
}

// TestUserDirConfigOverridesHome checks EYEDBG_CONFIG_DIR wins over
// EYEDBG_HOME.
func TestUserDirConfigOverridesHome(t *testing.T) {
	home, config := t.TempDir(), t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvConfigDir, config)

	got, err := UserDir()
	want := filepath.Join(config, "adapters")

	if err != nil || got != want {
		t.Fatalf("UserDir = %q, %v; want %q", got, err, want)
	}
}
