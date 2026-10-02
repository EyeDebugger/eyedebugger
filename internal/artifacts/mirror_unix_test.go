// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package artifacts

import (
	"path/filepath"
	"syscall"
	"testing"
)

// TestMirrorRefusesAFIFOInTheSource: anything but a regular file or a
// directory is an error before the destination is touched (and opening a
// FIFO would block).
func TestMirrorRefusesAFIFOInTheSource(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)
	writeFile(t, filepath.Join(src, "App.dll"), "new")
	writeFile(t, filepath.Join(dst, "App.dll"), "old")
	writeFile(t, filepath.Join(dst, "Stale.dll"), "stale")

	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	before := snapshot(t, dst)

	if err := Mirror(src, dst); err == nil {
		t.Fatal("Mirror accepted a FIFO in the source")
	}

	if why, ok := sameSnapshot(before, snapshot(t, dst)); !ok {
		t.Errorf("the destination was touched: %s", why)
	}
}
