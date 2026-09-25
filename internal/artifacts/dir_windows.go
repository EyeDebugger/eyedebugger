// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package artifacts

import "io/fs"

// checkPrivate is a no-op on Windows: the default directory is in the user
// profile, whose inherited ACL admits only the user; an EYEDBG_HOME
// elsewhere is the user's choice, like the shell's configuration.
func checkPrivate(string, fs.FileInfo) error { return nil }
