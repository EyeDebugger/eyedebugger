// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A container's labels are not its compose's: an image's own LABELs are copied
// onto every container made from it, so one run with plain 'docker run' can
// carry any com.docker.compose.* value its image author chose. The project
// directory a label names is therefore used (to map source paths, to build in,
// to pick compose files) only when the container's other labels corroborate it
// (docs/adr/0020 D5, docs/adr/0021).

// CorroborateComposeDir checks the compose project directory dir (the
// container's project.working_dir label) against files (its
// project.config_files label) and returns dir with its symlinks resolved. It
// holds only when dir is an absolute existing directory below the file system
// root, and at least one of files is a compose file by name (.yml or .yaml,
// in any case), exists as a regular file, and lies inside dir once both have
// their symlinks resolved: compose made the container from files kept there.
// A label an image author picked names a directory with no such file in it
// unless the author knows a compose file on this machine. The error says what
// failed, for a message; the values are never read for anything else.
func CorroborateComposeDir(dir string, files []string) (string, error) {
	if dir == "" || !plainAbsPath(dir) || filepath.Dir(dir) == dir {
		return "", errors.New("no compose project directory is recorded")
	}

	resolved, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("the compose project directory %s is not on this machine", show(dir, 80))
	}

	if st, err := os.Stat(resolved); err != nil || !st.IsDir() {
		return "", fmt.Errorf("the compose project directory %s is not a directory", show(dir, 80))
	}

	for _, f := range files {
		if !isComposeFileName(f) {
			continue
		}

		rf, err := filepath.EvalSymlinks(filepath.Clean(f))
		if err != nil || !within(resolved, rf) {
			continue
		}

		if st, err := os.Stat(rf); err == nil && st.Mode().IsRegular() {
			return resolved, nil
		}
	}

	return "", fmt.Errorf("none of the compose files the container records is a file inside %s: "+
		"this machine's compose project isn't what created it", show(dir, 80))
}

// isComposeFileName reports whether the base name of path ends in .yml or
// .yaml, in any case, with something before it.
func isComposeFileName(path string) bool {
	base := strings.ToLower(filepath.Base(path))

	for _, ext := range []string{".yml", ".yaml"} {
		if strings.HasSuffix(base, ext) && len(base) > len(ext) {
			return true
		}
	}

	return false
}

// composeSourceDir is [CorroborateComposeDir] of an inspected container's
// labels, "" when they don't corroborate the directory.
func composeSourceDir(dir, configFiles string) string {
	files, err := splitConfigFiles(configFiles)
	if err != nil {
		return ""
	}

	resolved, err := CorroborateComposeDir(dir, files)
	if err != nil {
		return ""
	}

	return resolved
}
