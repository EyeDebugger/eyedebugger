// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package adapters

import (
	"path/filepath"
	"syscall"
	"testing"
)

// A FIFO swapped in after the regular-file check must not block the open.
func TestOpenManifestDoesNotBlockOnFIFO(t *testing.T) {
	t.Parallel()

	p := filepath.Join(t.TempDir(), "x.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	f, err := openManifest(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	if manifestFile(info) == nil {
		t.Fatal("manifestFile accepted a FIFO")
	}
}
