// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import "io/fs"

// On Windows user manifests are read from the per-user %AppData%
// directory, whose ACL admits only the user (and administrators), and
// there are no mode bits to check (docs/adapter-manifests.md): these
// checks accept everything.

func checkTrust(string) error { return nil }

func checkInfo(string, fs.FileInfo) error { return nil }

func ownedByUser(string) bool { return true }
