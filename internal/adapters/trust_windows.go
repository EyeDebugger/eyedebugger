// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"io/fs"
	"os"
	"path/filepath"
)

// On Windows user manifests are read from the per-user profile directory
// (~/.eyedbg by default, %USERPROFILE%\.eyedbg), whose ACL admits only the
// user (and administrators), and there are no mode bits to check
// (docs/adapter-manifests.md): checkTrust and checkInfo accept everything.

func checkTrust(string) error { return nil }

func checkInfo(string, fs.FileInfo) error { return nil }

// ownedByUser counts only the user's profile directory and what is inside
// it as the user's: other users may create folders in C:\ and modify what
// is under them, so a venv found there is not searched for. A project
// outside the profile names its interpreter with --opt python.
func ownedByUser(p string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return false
	}

	return withinDir(home, abs, true)
}

// openManifest opens a user manifest for reading.
func openManifest(p string) (*os.File, error) {
	return os.Open(p)
}
