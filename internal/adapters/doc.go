// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package adapters finds and installs debug adapters (docs/DESIGN.md §7,
// §8). Downloads are pinned to one release per adapter and verified against
// a SHA-256 digest recorded in this package before anything is extracted.
package adapters
