// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
)

// Location sources (Location.Source).
const (
	// FoundEnv is the manifest's environment variable (e.g.
	// EYEDBG_NETCOREDBG).
	FoundEnv = "env"
	// FoundInstalled is 'eyedbg adapters install'.
	FoundInstalled = "installed"
	// FoundPath is PATH.
	FoundPath = "path"
	// FoundManifest is an absolute entry in the manifest.
	FoundManifest = "manifest"
)

// ErrNotInstalled means an adapter was not found anywhere.
var ErrNotInstalled = errors.New("adapter not installed")

// Location says where an adapter was found and how.
type Location struct {
	Path string `json:"path"`
	// Source is FoundEnv, FoundInstalled, FoundPath or FoundManifest.
	Source string `json:"source"`
}

// Find locates native adapter m: its environment variable, else an
// absolute entry, else the pinned installed copy, else PATH (when the
// manifest allows it). It returns ErrNotInstalled if none exists.
func Find(m *Manifest) (Location, error) {
	a := m.Adapter

	if a.Env != "" {
		if p := os.Getenv(a.Env); p != "" {
			if _, err := os.Stat(p); err != nil { //nolint:gosec // The user's own setting names the file to use.
				return Location{}, fmt.Errorf("%s=%s: %w", a.Env, p, err)
			}

			return Location{Path: p, Source: FoundEnv}, nil
		}
	}

	if filepath.IsAbs(a.Entry) {
		if _, err := os.Stat(a.Entry); err != nil {
			return Location{}, fmt.Errorf("%s (adapter.entry): %w", a.Entry, ErrNotInstalled)
		}

		return Location{Path: a.Entry, Source: FoundManifest}, nil
	}

	if dir, err := InstallDir(m); err == nil {
		p := installedEntry(m, dir)
		if _, err := os.Stat(p); err == nil {
			return Location{Path: p, Source: FoundInstalled}, nil
		}
	}

	if a.Path {
		if p, err := exec.LookPath(exeName(a.Entry)); err == nil {
			return Location{Path: p, Source: FoundPath}, nil
		}
	}

	return Location{}, ErrNotInstalled
}

// Environ returns m's adapter environment as sorted NAME=VALUE entries.
func Environ(m *Manifest) []string {
	env := make([]string, 0, len(m.Adapter.Environment))
	for k, v := range m.Adapter.Environment {
		env = append(env, k+"="+v)
	}

	slices.Sort(env)

	return env
}
