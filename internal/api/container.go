// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

// PathMapping pairs a directory in a container with the host directory
// holding the same sources (docs/adr/0020, D5): Remote is an absolute
// POSIX path as the debug adapter in the container names sources, Local an
// absolute host directory.
type PathMapping struct {
	Remote string `json:"remote"`
	Local  string `json:"local"`
}
