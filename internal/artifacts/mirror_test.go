// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// snapshot describes every entry below dir (symlinks not followed): kind,
// mode for directories and files, content for files, target for links.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()

	snap := map[string]string{}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	err = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		info, err := d.Info() // Lstat semantics: a link is described, not followed.
		if err != nil {
			return err
		}

		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := root.Readlink(rel)
			snap[rel] = "link -> " + target
		case info.IsDir():
			snap[rel] = "dir " + info.Mode().Perm().String()
		case info.Mode().IsRegular():
			b, _ := root.ReadFile(rel)
			snap[rel] = "file " + info.Mode().Perm().String() + " " + string(b)
		default:
			snap[rel] = "other " + info.Mode().String()
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return snap
}

func sameSnapshot(a, b map[string]string) (string, bool) {
	for k, v := range a {
		if b[k] != v {
			return k + ": " + v + " vs " + b[k], false
		}
	}

	for k, v := range b {
		if a[k] != v {
			return k + ": " + a[k] + " vs " + v, false
		}
	}

	return "", true
}

// writeMode writes a file with a mode (a test needs modes the lint rules
// call too open: it checks that Mirror normalises them).
func writeMode(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()

	writeFile(t, path, content)

	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// pair is a source and a destination directory.
func pair(t *testing.T) (src, dst string) {
	t.Helper()

	root := realTemp(t)
	src, dst = filepath.Join(root, "stage"), filepath.Join(root, "service")
	mkdirs(t, src, dst)

	return src, dst
}

func TestMirrorCopiesATree(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)

	writeMode(t, filepath.Join(src, "App.dll"), "dll", 0o600)
	writeMode(t, filepath.Join(src, "run.sh"), "#!/bin/sh", 0o700)
	writeMode(t, filepath.Join(src, "runtimes", "linux-x64", "native", "lib.so"), "so", 0o640)
	mkdirs(t, filepath.Join(src, "empty", "nested"))

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}

	got := snapshot(t, dst)

	if runtime.GOOS == "windows" {
		for _, p := range []string{"App.dll", "run.sh", filepath.Join("runtimes", "linux-x64", "native", "lib.so"), "empty", filepath.Join("empty", "nested")} {
			if _, ok := got[p]; !ok {
				t.Errorf("%s missing: %v", p, got)
			}
		}

		return
	}

	want := map[string]string{
		".":                                "dir -rwxr-xr-x",
		"App.dll":                          "file -rw-r--r-- dll",
		"run.sh":                           "file -rwxr-xr-x #!/bin/sh",
		"runtimes":                         "dir -rwxr-xr-x",
		"runtimes/linux-x64":               "dir -rwxr-xr-x",
		"runtimes/linux-x64/native":        "dir -rwxr-xr-x",
		"runtimes/linux-x64/native/lib.so": "file -rw-r--r-- so",
		"empty":                            "dir -rwxr-xr-x",
		"empty/nested":                     "dir -rwxr-xr-x",
	}

	if why, ok := sameSnapshot(got, want); !ok {
		t.Errorf("destination differs: %s\n%v", why, got)
	}
}

// TestMirrorNormalisesModes: a non-root container user must traverse the
// directories and read the files, whatever the umask the build ran under.
func TestMirrorNormalisesModes(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}

	src, dst := pair(t)

	// A build made under umask 077: 0700 directories, 0600 files.
	writeMode(t, filepath.Join(src, "sub", "a.dll"), "a", 0o600)

	if err := os.Chmod(filepath.Join(src, "sub"), 0o700); err != nil { //nolint:gosec // A build made under umask 077.
		t.Fatal(err)
	}

	// A destination left 0700 by an earlier run, with a stale narrow dir.
	if err := os.Chmod(dst, 0o700); err != nil { //nolint:gosec // Left narrow by an earlier run.
		t.Fatal(err)
	}

	mkdirs(t, filepath.Join(dst, "sub"))

	if err := os.Chmod(filepath.Join(dst, "sub"), 0o700); err != nil { //nolint:gosec // Left narrow by an earlier run.
		t.Fatal(err)
	}

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}

	for p, want := range map[string]fs.FileMode{dst: 0o755, filepath.Join(dst, "sub"): 0o755, filepath.Join(dst, "sub", "a.dll"): 0o644} {
		if info, err := os.Stat(p); err != nil || info.Mode().Perm() != want {
			t.Errorf("%s: mode %v (%v), want %v", p, info.Mode().Perm(), err, want)
		}
	}
}

func TestMirrorRemovesWhatTheSourceLacks(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)

	writeFile(t, filepath.Join(src, "App.dll"), "new")
	writeFile(t, filepath.Join(src, "keep", "inner.txt"), "new inner")

	writeFile(t, filepath.Join(dst, "App.dll"), "old")
	writeFile(t, filepath.Join(dst, "Stale.dll"), "stale")
	writeFile(t, filepath.Join(dst, "keep", "inner.txt"), "old inner")
	writeFile(t, filepath.Join(dst, "keep", "stale-inner.txt"), "stale")
	writeFile(t, filepath.Join(dst, "olddir", "deep", "x.txt"), "stale")
	writeFile(t, filepath.Join(dst, ".eyedbg-tmp-0123456789abcdef"), "left by a run that died")
	writeFile(t, filepath.Join(dst, "keep", ".eyedbg-tmp-fedcba9876543210"), "left by a run that died")

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}

	got := snapshot(t, dst)

	for _, gone := range []string{"Stale.dll", "olddir", filepath.Join("olddir", "deep"), filepath.Join("keep", "stale-inner.txt"), ".eyedbg-tmp-0123456789abcdef", filepath.Join("keep", ".eyedbg-tmp-fedcba9876543210")} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s survived: %v", gone, got)
		}
	}

	if mustRead(t, filepath.Join(dst, "App.dll")) != "new" || mustRead(t, filepath.Join(dst, "keep", "inner.txt")) != "new inner" {
		t.Error("contents not updated")
	}

	// The same as the source, exactly.
	if len(got) != 4 { // ".", App.dll, keep, keep/inner.txt
		t.Errorf("destination holds %d entries: %v", len(got), got)
	}
}

func TestMirrorReplacesWhatChangedKind(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)

	// The source has a directory where the destination has a file, and a
	// file where the destination has a directory.
	writeFile(t, filepath.Join(src, "wasfile", "inner.txt"), "dir now")
	writeFile(t, filepath.Join(src, "wasdir"), "file now")

	writeFile(t, filepath.Join(dst, "wasfile"), "file before")
	writeFile(t, filepath.Join(dst, "wasdir", "deep", "x.txt"), "dir before")

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}

	if mustRead(t, filepath.Join(dst, "wasfile", "inner.txt")) != "dir now" || mustRead(t, filepath.Join(dst, "wasdir")) != "file now" {
		t.Errorf("kinds not swapped: %v", snapshot(t, dst))
	}
}

// TestMirrorNeverFollowsASymlinkInTheDestination is the deletion-confinement
// test: links in dst — to a file, to a directory, nested, and named like
// something the source has — are removed or replaced themselves, and what
// they point at, outside dst, survives untouched.
func TestMirrorNeverFollowsASymlinkInTheDestination(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)
	outside := realTemp(t)

	writeFile(t, filepath.Join(outside, "dir", "precious.txt"), "precious")
	writeFile(t, filepath.Join(outside, "file.txt"), "precious file")

	before := snapshot(t, outside)

	// Absent from the source: removed as links.
	symlink(t, filepath.Join(outside, "dir"), filepath.Join(dst, "linkdir"))
	symlink(t, filepath.Join(outside, "file.txt"), filepath.Join(dst, "linkfile"))
	symlink(t, filepath.Join(outside, "nowhere"), filepath.Join(dst, "dangling"))
	mkdirs(t, filepath.Join(dst, "real"))
	symlink(t, filepath.Join(outside, "dir"), filepath.Join(dst, "real", "nested-link"))

	// Named like source entries: the source's entry replaces the link.
	writeFile(t, filepath.Join(src, "sub", "a.txt"), "from source")
	writeFile(t, filepath.Join(src, "file.dll"), "from source")
	symlink(t, filepath.Join(outside, "dir"), filepath.Join(dst, "sub"))
	symlink(t, filepath.Join(outside, "file.txt"), filepath.Join(dst, "file.dll"))

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}

	if why, ok := sameSnapshot(before, snapshot(t, outside)); !ok {
		t.Errorf("a symlink in the destination led Mirror out of it: %s", why)
	}

	got := snapshot(t, dst)
	for _, gone := range []string{"linkdir", "linkfile", "dangling", "real"} {
		if _, ok := got[gone]; ok {
			t.Errorf("%s survived: %v", gone, got)
		}
	}

	if mustRead(t, filepath.Join(dst, "sub", "a.txt")) != "from source" || mustRead(t, filepath.Join(dst, "file.dll")) != "from source" {
		t.Errorf("source entries not written over the links: %v", got)
	}

	for _, p := range []string{filepath.Join(dst, "sub"), filepath.Join(dst, "file.dll")} {
		if info, err := os.Lstat(p); err != nil || info.Mode()&fs.ModeSymlink != 0 {
			t.Errorf("%s is still a symlink", p)
		}
	}

	if exists(filepath.Join(outside, "dir", "a.txt")) {
		t.Error("a source file was written through a link into the outside directory")
	}
}

func TestMirrorRefusesASymlinkInTheSource(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		make func(t *testing.T, src, outside string)
	}{
		{"link to a file", func(t *testing.T, src, outside string) {
			t.Helper()
			symlink(t, filepath.Join(outside, "f.txt"), filepath.Join(src, "a-link"))
		}},
		{"link to a directory", func(t *testing.T, src, outside string) {
			t.Helper()
			symlink(t, outside, filepath.Join(src, "a-link"))
		}},
		{"dangling link", func(t *testing.T, src, _ string) {
			t.Helper()
			symlink(t, filepath.Join(src, "nowhere"), filepath.Join(src, "a-link"))
		}},
		{"nested link", func(t *testing.T, src, outside string) {
			t.Helper()
			mkdirs(t, filepath.Join(src, "deep", "er"))
			symlink(t, outside, filepath.Join(src, "deep", "er", "a-link"))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src, dst := pair(t)
			outside := realTemp(t)
			writeFile(t, filepath.Join(outside, "f.txt"), "outside")

			writeFile(t, filepath.Join(src, "App.dll"), "new")
			writeFile(t, filepath.Join(dst, "App.dll"), "old")
			writeFile(t, filepath.Join(dst, "Stale.dll"), "stale")
			tt.make(t, src, outside)

			before := snapshot(t, dst)

			if err := Mirror(src, dst); err == nil {
				t.Fatal("Mirror accepted a symlink in the source")
			}

			// Refused before any change: not even stale files went.
			if why, ok := sameSnapshot(before, snapshot(t, dst)); !ok {
				t.Errorf("the destination was touched: %s", why)
			}
		})
	}
}

func TestMirrorRefusesADirectoryThatIsASymlink(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outside := realTemp(t)
	realDir := filepath.Join(root, "real")

	mkdirs(t, realDir)
	writeFile(t, filepath.Join(realDir, "App.dll"), "x")
	writeFile(t, filepath.Join(outside, "keep.txt"), "keep")
	writeFile(t, filepath.Join(outside, "stale.txt"), "keep too")

	srcLink, dstLink := filepath.Join(root, "srclink"), filepath.Join(root, "dstlink")
	symlink(t, realDir, srcLink)
	symlink(t, outside, dstLink)

	before := snapshot(t, outside)

	if err := Mirror(srcLink, filepath.Join(root, "dst")); err == nil {
		t.Error("a symlinked source was accepted")
	}

	mkdirs(t, filepath.Join(root, "dst2"))

	if err := Mirror(realDir, dstLink); err == nil {
		t.Error("a symlinked destination was accepted")
	}

	if why, ok := sameSnapshot(before, snapshot(t, outside)); !ok {
		t.Errorf("the symlink's target was touched: %s", why)
	}
}

func TestMirrorRefusesMissingAndFileDirectories(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)
	writeFile(t, filepath.Join(src, "a"), "x")
	writeFile(t, filepath.Join(src, "afile"), "x")

	for _, tt := range []struct{ name, src, dst string }{
		{"missing destination", src, filepath.Join(dst, "nowhere")},
		{"missing source", filepath.Join(src, "nowhere"), dst},
		{"destination is a file", src, filepath.Join(src, "afile")},
		{"source is a file", filepath.Join(src, "afile"), dst},
	} {
		if err := Mirror(tt.src, tt.dst); err == nil {
			t.Errorf("%s: no error", tt.name)
		}
	}

	if exists(filepath.Join(dst, "nowhere")) {
		t.Error("a missing destination was created")
	}
}

func TestMirrorRefusesOverlappingDirectories(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outer := filepath.Join(root, "outer")
	inner := filepath.Join(outer, "inner")

	mkdirs(t, inner)

	for _, tt := range []struct{ name, src, dst string }{
		{"the same directory", outer, outer},
		{"source inside destination", inner, outer},
		{"destination inside source", outer, inner},
	} {
		if err := Mirror(tt.src, tt.dst); err == nil {
			t.Errorf("%s: no error", tt.name)
		}
	}

	// A sibling whose name only starts with the other's is not nested.
	sibling := filepath.Join(root, "outer2")
	mkdirs(t, sibling)

	if err := Mirror(sibling, outer); err != nil {
		t.Errorf("a sibling with a longer name: %v", err)
	}
}

// TestMirrorOverlapDeletesNothing runs the refusals alone and checks the
// tree is untouched.
func TestMirrorOverlapDeletesNothing(t *testing.T) {
	t.Parallel()

	root := realTemp(t)
	outer := filepath.Join(root, "outer")
	inner := filepath.Join(outer, "inner")

	mkdirs(t, inner)
	writeFile(t, filepath.Join(outer, "a.txt"), "a")
	writeFile(t, filepath.Join(inner, "b.txt"), "b")

	before := snapshot(t, outer)

	for _, pair := range [][2]string{{outer, outer}, {inner, outer}, {outer, inner}} {
		_ = Mirror(pair[0], pair[1])
	}

	if why, ok := sameSnapshot(before, snapshot(t, outer)); !ok {
		t.Errorf("an overlapping mirror changed the tree: %s", why)
	}
}

func TestMirrorIsIdempotent(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)
	writeMode(t, filepath.Join(src, "a.dll"), "a", 0o755)
	writeFile(t, filepath.Join(src, "d", "b.dll"), "b")

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}

	first := snapshot(t, dst)

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}

	if why, ok := sameSnapshot(first, snapshot(t, dst)); !ok {
		t.Errorf("a second mirror changed the result: %s", why)
	}

	// An empty source empties the destination.
	empty := filepath.Join(realTemp(t), "empty")
	mkdirs(t, empty)

	if err := Mirror(empty, dst); err != nil {
		t.Fatal(err)
	}

	if got := snapshot(t, dst); len(got) != 1 {
		t.Errorf("an empty source left %v", got)
	}
}

func TestMirrorFoldedNamesInTheSource(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)
	writeFile(t, filepath.Join(src, "A.txt"), "A")
	writeFile(t, filepath.Join(src, "a.txt"), "a")

	if len(snapshot(t, src)) != 3 {
		t.Skip("a case-insensitive file system: the two names are one file")
	}

	// Where the host folds case, the two would land on one file.
	if err := mirror(src, dst, true); err == nil || len(snapshot(t, dst)) != 1 {
		t.Errorf("folded twins were accepted: %v", err)
	}

	// Where it doesn't, they are two files.
	if err := mirror(src, dst, false); err != nil {
		t.Fatal(err)
	}

	if mustRead(t, filepath.Join(dst, "A.txt")) != "A" || mustRead(t, filepath.Join(dst, "a.txt")) != "a" {
		t.Error("case twins not both copied")
	}
}

func TestScanTreeLimits(t *testing.T) {
	t.Parallel()

	src, _ := pair(t)

	deep := src
	for range 6 {
		deep = filepath.Join(deep, "d")
	}

	mkdirs(t, deep)

	for i := range 10 {
		writeFile(t, filepath.Join(src, "f", strings.Repeat("x", i+1)), "x")
	}

	root, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	if _, err := scanTree(root, false, limits{entries: 1000, depth: 3}); err == nil {
		t.Error("a tree deeper than the limit was accepted")
	}

	if _, err := scanTree(root, false, limits{entries: 5, depth: 64}); err == nil {
		t.Error("a tree with more entries than the limit was accepted")
	}

	tree, err := scanTree(root, false, limits{entries: 1000, depth: 64})
	if err != nil || len(tree.files) != 10 {
		t.Errorf("within the limits: %v, %d files", err, len(tree.files))
	}
}

// TestCopyFileLeavesNoTemporaryFile: a rename that fails removes its
// temporary file.
func TestCopyFileLeavesNoTemporaryFile(t *testing.T) {
	t.Parallel()

	src, dst := pair(t)
	writeFile(t, filepath.Join(src, "a.dll"), "a")
	// Where the file goes, a non-empty directory: the rename fails.
	writeFile(t, filepath.Join(dst, "a.dll", "inner"), "x")

	srcRoot, err := os.OpenRoot(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcRoot.Close()

	dstRoot, err := os.OpenRoot(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer dstRoot.Close()

	if err := copyFileTo(srcRoot, dstRoot, srcFile{rel: "a.dll"}); err == nil {
		t.Fatal("copying a file over a directory worked")
	}

	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mirrorTempName) {
			t.Errorf("temporary file left: %s", e.Name())
		}
	}
}
