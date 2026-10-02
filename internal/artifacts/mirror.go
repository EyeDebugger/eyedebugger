// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Bounds of what Mirror copies.
const (
	// maxMirrorEntries bounds the files and directories of a build.
	maxMirrorEntries = 200_000
	// maxMirrorDepth bounds how deep a build's directories go.
	maxMirrorDepth = 64

	mirrorDirMode  = 0o755
	mirrorFileMode = 0o644
	mirrorExecMode = 0o755
	mirrorTempMode = 0o600
	mirrorTempName = ".eyedbg-tmp-"
)

// kind is what a path of the source is.
type kind int

const (
	kindDir kind = iota + 1
	kindFile
)

// srcFile is a regular file of the source.
type srcFile struct {
	rel  string
	exec bool
}

// srcTree is the source directory, scanned: its directories top-down, its
// files, and what every path is.
type srcTree struct {
	dirs  []string
	files []srcFile
	kinds map[string]kind
}

// Mirror makes the real directory dst hold exactly what the real directory
// src holds (docs/adr/0021, D11): directories 0755, files 0644 or 0755 (any
// exec bit in the source), each written to a temporary file next to its
// target and renamed over it. The source is scanned first, and anything in
// it that isn't a regular file or a directory (a symlink, a FIFO, ...) is an
// error before dst is touched. Entries of dst that the source doesn't have —
// or has as another kind — are then removed: files and symlinks with a
// plain remove (a link is never followed: its target survives) and
// directories with a recursive one, through an [os.Root] on dst, so nothing
// is ever followed out of it. dst must already be a real directory (never a
// symlink, never replaced); its own mode is made 0755.
//
// An error after the scan may leave dst partly updated: the caller must not
// run what is in it.
func Mirror(src, dst string) error { return mirror(src, dst, foldCase()) }

func mirror(src, dst string, fold bool) error {
	srcRoot, err := openRealDir(src)
	if err != nil {
		return fmt.Errorf("mirror source: %w", err)
	}
	defer srcRoot.Close()

	dstRoot, err := openRealDir(dst)
	if err != nil {
		return fmt.Errorf("mirror destination: %w", err)
	}
	defer dstRoot.Close()

	if err := distinct(src, dst); err != nil {
		return err
	}

	tree, err := scanTree(srcRoot, fold, limits{entries: maxMirrorEntries, depth: maxMirrorDepth})
	if err != nil {
		return fmt.Errorf("scan %s: %w", src, err)
	}

	if err := dstRoot.Chmod(".", mirrorDirMode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", dst, err)
	}

	if err := pruneDst(dstRoot, ".", tree.kinds); err != nil {
		return fmt.Errorf("clear %s: %w", dst, err)
	}

	for _, d := range tree.dirs {
		if err := ensureDstDir(dstRoot, d); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Join(dst, d), err)
		}
	}

	for _, f := range tree.files {
		if err := copyFileTo(srcRoot, dstRoot, f); err != nil {
			return fmt.Errorf("copy %s: %w", filepath.Join(src, f.rel), err)
		}
	}

	return nil
}

// distinct fails when src and dst are one directory or one is inside the
// other (both resolved): clearing dst would delete src.
func distinct(src, dst string) error {
	rs, err := filepath.EvalSymlinks(src)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", src, err)
	}

	rd, err := filepath.EvalSymlinks(dst)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", dst, err)
	}

	if rs == rd || within(rs, rd) || within(rd, rs) {
		return fmt.Errorf("%s and %s are the same directory or one is inside the other", src, dst)
	}

	return nil
}

// within reports whether p is inside root (both resolved).
func within(root, p string) bool {
	return strings.HasPrefix(p, strings.TrimRight(root, string(filepath.Separator))+string(filepath.Separator))
}

// limits are the bounds of a scan.
type limits struct{ entries, depth int }

// scanner reads a source tree.
type scanner struct {
	root  *os.Root
	fold  bool
	lim   limits
	count int
	tree  *srcTree
}

// scanTree reads the whole source tree through root, refusing anything that
// isn't a regular file or a directory, a name that isn't local to its
// directory (a Windows reserved name, say), too many entries or too deep a
// tree; on a host that folds case, two names of one directory that differ
// only in case.
func scanTree(root *os.Root, fold bool, lim limits) (*srcTree, error) {
	s := &scanner{root: root, fold: fold, lim: lim, tree: &srcTree{kinds: map[string]kind{}}}

	if err := s.walk(".", 0); err != nil {
		return nil, err
	}

	return s.tree, nil
}

// walk scans dir (a path inside the root; "." the root itself).
func (s *scanner) walk(dir string, depth int) error {
	if depth > s.lim.depth {
		return fmt.Errorf("directories more than %d deep", s.lim.depth)
	}

	names, err := listDir(s.root, dir)
	if err != nil {
		return err
	}

	seen := map[string]string{}

	for _, name := range names {
		if err := s.checkName(name, seen); err != nil {
			return err
		}

		if err := s.visit(joinRel(dir, name), depth); err != nil {
			return err
		}
	}

	return nil
}

// checkName counts an entry and checks its name: local, and (where the
// host folds case) not equal to a sibling's under case folding.
func (s *scanner) checkName(name string, seen map[string]string) error {
	s.count++
	if s.count > s.lim.entries {
		return fmt.Errorf("more than %d entries", s.lim.entries)
	}

	if !filepath.IsLocal(name) {
		return fmt.Errorf("%q is not a name a file system keeps here", name)
	}

	if s.fold {
		key := strings.ToLower(name)
		if other, dup := seen[key]; dup {
			return fmt.Errorf("%q and %q differ only in case", other, name)
		}

		seen[key] = name
	}

	return nil
}

// visit records the entry rel and descends into a directory.
func (s *scanner) visit(rel string, depth int) error {
	info, err := s.root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}

	switch {
	case info.Mode()&fs.ModeSymlink == 0 && info.IsDir():
		s.tree.dirs = append(s.tree.dirs, rel)
		s.tree.kinds[rel] = kindDir

		return s.walk(rel, depth+1)
	case info.Mode().IsRegular():
		s.tree.files = append(s.tree.files, srcFile{rel: rel, exec: info.Mode()&0o111 != 0})
		s.tree.kinds[rel] = kindFile

		return nil
	default:
		return fmt.Errorf("%s is not a regular file or a directory (%s)", rel, info.Mode().Type())
	}
}

// joinRel is name inside dir ("." is the root).
func joinRel(dir, name string) string {
	if dir == "." {
		return name
	}

	return filepath.Join(dir, name)
}

// listDir lists the names in dir of root (sorted).
func listDir(root *os.Root, dir string) ([]string, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names, nil
}

// pruneDst removes, under dir of root (a real directory of dst), every
// entry the source doesn't have as the same kind: a real directory the
// source has as a directory is entered, a regular file the source has as a
// file stays (it is replaced by a rename later), everything else goes.
func pruneDst(root *os.Root, dir string, kinds map[string]kind) error {
	names, err := listDir(root, dir)
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := pruneEntry(root, joinRel(dir, name), kinds); err != nil {
			return err
		}
	}

	return nil
}

// pruneEntry is pruneDst's decision for one entry.
func pruneEntry(root *os.Root, rel string, kinds map[string]kind) error {
	info, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("stat %s: %w", rel, err)
	}

	want := kinds[rel]
	realDir := info.Mode()&fs.ModeSymlink == 0 && info.IsDir()

	switch {
	case realDir && want == kindDir:
		return pruneDst(root, rel, kinds)
	case info.Mode().IsRegular() && want == kindFile:
		return nil
	case realDir:
		if err := root.RemoveAll(rel); err != nil {
			return fmt.Errorf("remove %s: %w", rel, err)
		}
	default:
		// A file, a symlink (never followed), anything else.
		if err := root.Remove(rel); err != nil {
			return fmt.Errorf("remove %s: %w", rel, err)
		}
	}

	return nil
}

// ensureDstDir makes rel (a directory of the source) a real directory of
// dst, 0755.
func ensureDstDir(root *os.Root, rel string) error {
	info, err := root.Lstat(rel)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := root.Mkdir(rel, mirrorDirMode); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
	case err != nil:
		return fmt.Errorf("stat: %w", err)
	case info.Mode()&fs.ModeSymlink != 0 || !info.IsDir():
		return errors.New("it is not a real directory")
	}

	if err := root.Chmod(rel, mirrorDirMode); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	return nil
}

// copyFileTo writes the source file f to dst: a fresh temporary file in its
// directory, mode set, renamed over the target.
func copyFileTo(srcRoot, dstRoot *os.Root, f srcFile) (err error) {
	in, err := srcRoot.Open(f.rel)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer in.Close()

	if info, serr := in.Stat(); serr != nil || !info.Mode().IsRegular() {
		return errors.New("it changed since it was scanned")
	}

	tmp, out, err := createDstTemp(dstRoot, filepath.Dir(f.rel))
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			_ = dstRoot.Remove(tmp)
		}
	}()

	_, cerr := io.Copy(out, in)
	if cerr == nil {
		cerr = out.Sync()
	}

	if clerr := out.Close(); cerr == nil {
		cerr = clerr
	}

	if cerr != nil {
		return fmt.Errorf("write: %w", cerr)
	}

	mode := fs.FileMode(mirrorFileMode)
	if f.exec {
		mode = mirrorExecMode
	}

	if err := dstRoot.Chmod(tmp, mode); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	if err := dstRoot.Rename(tmp, f.rel); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// createDstTemp creates a fresh temporary file in dir of root and returns
// its path inside root and the open file.
func createDstTemp(root *os.Root, dir string) (string, *os.File, error) {
	for range nameTries {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, fmt.Errorf("pick a file name: %w", err)
		}

		rel := mirrorTempName + hex.EncodeToString(b[:])
		if dir != "." {
			rel = filepath.Join(dir, rel)
		}

		f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mirrorTempMode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}

		if err != nil {
			return "", nil, fmt.Errorf("create a temporary file: %w", err)
		}

		return rel, f, nil
	}

	return "", nil, fmt.Errorf("create a temporary file in %s: %d names taken", dir, nameTries)
}
