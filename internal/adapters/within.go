// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"path/filepath"
	"strings"
)

// withinDir reports whether p is root or inside it. fold compares case
// insensitively, as Windows paths are.
func withinDir(root, p string, fold bool) bool {
	if root == "" || p == "" {
		return false
	}

	root, p = filepath.Clean(root), filepath.Clean(p)
	if fold {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}

	rel, err := filepath.Rel(root, p)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
