// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// ErrExists is returned when something is already at the --out path:
// eyedbg never replaces it.
var ErrExists = errors.New("exists")

// Place moves the private dump src to out, which must not exist (a
// dangling symlink counts as existing): a hard link when src and out share
// a filesystem, else a copy into a file created exclusively (0600). src is
// removed only once out holds the dump. On failure, only a file this call
// created is removed, and src stays. Parent directories are never created.
func Place(src, out string) error {
	return place(src, out, os.Link)
}

// place is Place with the link function injectable (tests).
func place(src, out string, link func(oldname, newname string) error) error {
	err := link(src, out)
	if err == nil {
		_ = os.Remove(src) // a leftover private copy is pruned later

		return nil
	}

	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s: %w", out, ErrExists)
	}

	// Another filesystem (EXDEV), or one without hard links: copy.
	if err := copyExclusive(src, out); err != nil {
		return err
	}

	_ = os.Remove(src)

	return nil
}

// copyExclusive copies src into a new file out (O_EXCL: an existing name,
// symlink included, fails), synced before it counts as done.
func copyExclusive(src, out string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		// Windows reports an existing directory as "is a directory", not
		// as existing.
		if _, lerr := os.Lstat(out); errors.Is(err, fs.ErrExist) || lerr == nil {
			return fmt.Errorf("%s: %w", out, ErrExists)
		}

		return fmt.Errorf("create %s: %w", out, err)
	}

	_, err = io.Copy(f, in)
	if err == nil {
		err = f.Sync()
	}

	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = os.Remove(out) // ours: created exclusively above

		return fmt.Errorf("copy the dump to %s: %w", out, err)
	}

	return nil
}
