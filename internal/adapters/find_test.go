// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeExe creates an executable file at dir/name.
func writeExe(t *testing.T, dir, name string) string {
	t.Helper()

	p := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // A test executable.
		t.Fatal(err)
	}

	return p
}

// TestFind checks netcoredbg's lookup order: env, installed, then PATH.
// Not parallel: it sets the environment.
func TestFind(t *testing.T) {
	m := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs}).Adapter("netcoredbg")
	exe := exeName("netcoredbg")

	data, bin := t.TempDir(), t.TempDir()
	installed := filepath.Join(data, "netcoredbg", "3.2.0-1092", exe)
	onPath := writeExe(t, bin, exe)
	fromEnv := writeExe(t, t.TempDir(), exe)

	t.Setenv(EnvDataDir, data)
	t.Setenv("PATH", bin)
	t.Setenv("EYEDBG_NETCOREDBG", "")

	check := func(want Location) {
		t.Helper()

		got, err := Find(m)
		if err != nil || got != want {
			t.Fatalf("Find = %+v, %v; want %+v", got, err, want)
		}
	}

	check(Location{Path: onPath, Source: FoundPath})

	writeExe(t, filepath.Dir(installed), exe)
	check(Location{Path: installed, Source: FoundInstalled})

	t.Setenv("EYEDBG_NETCOREDBG", fromEnv)
	check(Location{Path: fromEnv, Source: FoundEnv})

	missing := filepath.Join(t.TempDir(), "gone")
	t.Setenv("EYEDBG_NETCOREDBG", missing)

	if _, err := Find(m); err == nil || !strings.HasPrefix(err.Error(), "EYEDBG_NETCOREDBG="+missing+": ") {
		t.Fatalf("Find with a bad EYEDBG_NETCOREDBG = %v", err)
	}

	t.Setenv("EYEDBG_NETCOREDBG", "")
	t.Setenv(EnvDataDir, t.TempDir())
	t.Setenv("PATH", t.TempDir())

	if _, err := Find(m); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Find with nothing installed = %v, want ErrNotInstalled", err)
	}

	noPath := *m
	noPath.Adapter.Path = false
	t.Setenv("PATH", bin)

	if _, err := Find(&noPath); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Find without path: true = %v, want ErrNotInstalled", err)
	}
}

func TestFindAbsoluteEntry(t *testing.T) {
	t.Parallel()

	exe := writeExe(t, t.TempDir(), "dbg")
	m := &Manifest{Name: "dbg", Version: "1", Adapter: Adapter{ID: "x", Entry: exe}}

	if got, err := Find(m); err != nil || got != (Location{Path: exe, Source: FoundManifest}) {
		t.Fatalf("Find = %+v, %v", got, err)
	}

	m.Adapter.Entry = filepath.Join(t.TempDir(), "gone")
	if _, err := Find(m); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("Find of a missing absolute entry = %v, want ErrNotInstalled", err)
	}
}

func TestEnviron(t *testing.T) {
	t.Parallel()

	m := &Manifest{Adapter: Adapter{Environment: map[string]string{"B": "2", "A": "1"}}}
	if got := Environ(m); !slices.Equal(got, []string{"A=1", "B=2"}) {
		t.Fatalf("Environ = %v", got)
	}
}
