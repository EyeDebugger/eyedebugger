// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// EnvRuntimeDir overrides the per-user runtime directory.
const EnvRuntimeDir = "EYEDBG_RUNTIME_DIR"

// maxSocketPath is the conservative limit for a Unix socket path (sun_path is
// 104 bytes on macOS, 108 on Linux and Windows, including the NUL).
const maxSocketPath = 103

// Paths locates the daemon's per-user files. All of them live in one
// directory that only the user can access.
type Paths struct {
	// Dir is the runtime directory.
	Dir string
	// Socket is the Unix domain socket the daemon listens on.
	Socket string
	// Token holds the per-daemon secret every connection must present.
	Token string
	// Lock is held exclusively by the running daemon for its whole life.
	Lock string
	// Log receives the daemon's stderr when it is auto-started.
	Log string
}

// PathsIn returns the paths inside dir.
func PathsIn(dir string) Paths {
	return Paths{
		Dir:    dir,
		Socket: filepath.Join(dir, "eyedbgd.sock"),
		Token:  filepath.Join(dir, "token"),
		Lock:   filepath.Join(dir, "eyedbgd.lock"),
		Log:    filepath.Join(dir, "eyedbgd.log"),
	}
}

// DefaultPaths resolves the runtime directory: $EYEDBG_RUNTIME_DIR if set,
// else $XDG_RUNTIME_DIR/eyedbg on Unix when set, else the user cache
// directory (~/Library/Caches/eyedbg, ~/.cache/eyedbg, %LocalAppData%\eyedbg).
func DefaultPaths() (Paths, error) {
	if dir := os.Getenv(EnvRuntimeDir); dir != "" {
		return PathsIn(dir), nil
	}

	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && runtime.GOOS != "windows" {
		return PathsIn(filepath.Join(xdg, "eyedbg")), nil
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return Paths{}, fmt.Errorf("locate runtime directory (set %s): %w", EnvRuntimeDir, err)
	}

	return PathsIn(filepath.Join(cache, "eyedbg")), nil
}

// Prepare creates the runtime directory if needed and checks that only the
// current user can access it.
func (p Paths) Prepare() error {
	if len(p.Socket) > maxSocketPath {
		return fmt.Errorf("socket path %s is longer than %d bytes (set %s to a shorter directory)",
			p.Socket, maxSocketPath, EnvRuntimeDir)
	}

	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return fmt.Errorf("create runtime directory: %w", err)
	}

	info, err := os.Lstat(p.Dir)
	if err != nil {
		return fmt.Errorf("stat runtime directory: %w", err)
	}

	if !info.IsDir() {
		return errors.New("runtime directory " + p.Dir + " is not a directory")
	}

	return checkPrivate(p.Dir, info)
}
