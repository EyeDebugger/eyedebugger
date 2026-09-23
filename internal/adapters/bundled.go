// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"embed"
	"io/fs"
)

// bundled holds the manifests built into eyedbg (manifests/*.json).
//
//go:embed manifests/*.json
var bundled embed.FS

// Bundled returns the built-in manifests' files, *.json at the root.
func Bundled() fs.FS {
	sub, err := fs.Sub(bundled, "manifests")
	if err != nil {
		panic("embedded manifests: " + err.Error()) // The directory is embedded above.
	}

	return sub
}
