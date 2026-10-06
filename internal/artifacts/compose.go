// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
)

// The layout of <home>/compose/<project>, docs/adr/0021 (D15, D17).
const (
	servicesDir = "services"
	stagePrefix = "stage-"
	lockName    = "lock"
	// LockMaxAge is how long a lock may stay before the next run takes it
	// over: a run is bounded by its publish (15 minutes) and a recreate.
	LockMaxAge = time.Hour

	// serviceDirMode is a service directory's mode: it is bind-mounted, and a
	// non-root user in the container must traverse it. Its parents' modes
	// don't matter (the container engine mounts it itself).
	serviceDirMode = 0o755
	privateMode    = 0o700
)

// ErrLocked is [Lock]'s error when another run holds the project's lock.
var ErrLocked = errors.New("another eyedbg compose run holds the lock")

// foldCase reports whether the host's usual file systems compare names
// without regard to case (macOS, Windows): two names equal under
// [strings.EqualFold] then name one file.
func foldCase() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "windows" }

// openRealDir opens dir as an [os.Root] (nothing under it can lead out of
// it) after checking it is a real directory — not a symlink — and that what
// was opened is what was checked.
func openRealDir(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", dir, err)
	}

	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%s is not a real directory (a symlink or a file?)", dir)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}

	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()

		return nil, fmt.Errorf("%s changed while it was opened", dir)
	}

	return root, nil
}

// ProjectDir returns <root>/<project>, creating it if needed: 0700, a real
// directory and not a symlink, owned by the user on Unix (as [Dir]). root is
// [Dir]([Compose]); project is a compose project name (api.CheckGroup:
// lowercase letters, digits, '_' and '-').
func ProjectDir(root, project string) (string, error) {
	if err := api.CheckGroup(project); err != nil {
		return "", err
	}

	dir := filepath.Join(root, project)
	if err := prepare(dir); err != nil {
		return "", err
	}

	return dir, nil
}

// ServiceDir returns <projectDir>/services/<service>, creating it (0755) if
// needed: the directory a service's build is mounted from. service is a
// compose service name; a real directory is required at every level, and on
// a case-insensitive host a service whose name differs only in case from an
// existing directory is refused (they would be one directory).
func ServiceDir(projectDir, service string) (string, error) {
	if err := container.ValidateService(service); err != nil {
		return "", err
	}

	if err := requireRealDir(projectDir); err != nil {
		return "", err
	}

	services := filepath.Join(projectDir, servicesDir)
	if err := ensureDir(services, privateMode); err != nil {
		return "", err
	}

	if err := noCaseAlias(services, service); err != nil {
		return "", err
	}

	dir := filepath.Join(services, service)
	if err := ensureDir(dir, serviceDirMode); err != nil {
		return "", err
	}

	return dir, nil
}

// requireRealDir fails unless dir is a real directory.
func requireRealDir(dir string) error {
	root, err := openRealDir(dir)
	if err != nil {
		return err
	}

	return root.Close()
}

// ensureDir creates dir with mode if it is missing (its parent exists), and
// requires a real directory; it then makes the mode exact (the umask may
// have taken bits off).
func ensureDir(dir string, mode fs.FileMode) error {
	if err := os.Mkdir(dir, mode); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	if err := requireRealDir(dir); err != nil {
		return err
	}

	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", dir, err)
	}

	return nil
}

// noCaseAlias fails when dir holds an entry whose name equals name only
// under case folding, on a host that folds case.
func noCaseAlias(dir, name string) error {
	if !foldCase() {
		return nil
	}

	return checkAlias(dir, name)
}

func checkAlias(dir, name string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.Name() != name && strings.EqualFold(e.Name(), name) {
			return fmt.Errorf("service %q and the existing directory %q differ only in case, which this file system doesn't tell apart", name, e.Name())
		}
	}

	return nil
}

// CheckServiceNames fails when two of names are equal under case folding on
// a host whose file systems fold case (macOS, Windows): their build
// directories would be one.
func CheckServiceNames(names []string) error { return checkFold(names, foldCase()) }

func checkFold(names []string, fold bool) error {
	if !fold {
		return nil
	}

	seen := make(map[string]string, len(names))

	for _, n := range names {
		key := strings.ToLower(n)
		if other, dup := seen[key]; dup && other != n {
			return fmt.Errorf("services %q and %q differ only in case, which this file system doesn't tell apart", other, n)
		}

		seen[key] = n
	}

	return nil
}

// NewStage creates a fresh staging directory <projectDir>/stage-<8 hex>
// (0700) for one run's build, and returns its path.
func NewStage(projectDir string) (string, error) {
	if err := requireRealDir(projectDir); err != nil {
		return "", err
	}

	for range nameTries {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("pick a staging directory name: %w", err)
		}

		dir := filepath.Join(projectDir, stagePrefix+hex.EncodeToString(b[:]))

		err := os.Mkdir(dir, privateMode)
		if errors.Is(err, fs.ErrExist) {
			continue
		}

		if err != nil {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}

		return dir, nil
	}

	return "", fmt.Errorf("pick a staging directory in %s: %d names taken", projectDir, nameTries)
}

// isStageName reports whether name is a staging directory's name:
// "stage-" and 8 lowercase hex digits.
func isStageName(name string) bool {
	hexPart, ok := strings.CutPrefix(name, stagePrefix)
	if !ok || len(hexPart) != 8 {
		return false
	}

	for i := range len(hexPart) {
		if c := hexPart[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}

	return true
}

// RemoveStage removes the staging directory stage (a path NewStage returned)
// of projectDir with everything in it: only a real directory directly in
// projectDir with a staging name, removed through projectDir, so no symlink
// in it is followed.
func RemoveStage(projectDir, stage string) error {
	name := filepath.Base(stage)
	if stage != filepath.Join(projectDir, name) || !isStageName(name) {
		return fmt.Errorf("%s is not a staging directory of %s", stage, projectDir)
	}

	return removeChildDir(projectDir, name)
}

// removeChildDir removes the real directory name inside dir; anything else
// at name (a file, a symlink) is an error and stays.
func removeChildDir(dir, name string) error {
	root, err := openRealDir(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	info, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("stat %s: %w", filepath.Join(dir, name), err)
	}

	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", filepath.Join(dir, name))
	}

	if err := root.RemoveAll(name); err != nil {
		return fmt.Errorf("remove %s: %w", filepath.Join(dir, name), err)
	}

	return nil
}

// PruneStages removes the staging directories of projectDir other than keep
// (a path, or "": none): what runs that died left. The caller holds the
// project's [Lock], so no other run's stage is among them. It returns the
// names removed; what can't be removed is left for the next run.
func PruneStages(projectDir, keep string) []string {
	return pruneChildren(projectDir, func(name string) bool {
		return isStageName(name) && (keep == "" || name != filepath.Base(keep))
	})
}

// PruneServices removes the service directories under <projectDir>/services
// whose service is not in keep (the services that still have a container in
// fast mode): only real directories with a service-name grammar, removed
// through the services directory; a symlink, a file or any other name is left
// alone. On a host that folds case a kept name also keeps a directory that
// differs only in case. It returns the names removed.
func PruneServices(projectDir string, keep map[string]bool) []string {
	fold := foldCase()

	return pruneChildren(filepath.Join(projectDir, servicesDir), func(name string) bool {
		if container.ValidateService(name) != nil {
			return false
		}

		return !inKeep(keep, name, fold)
	})
}

// inKeep reports whether name is in keep, ignoring case when fold.
func inKeep(keep map[string]bool, name string, fold bool) bool {
	if keep[name] {
		return true
	}

	if !fold {
		return false
	}

	for k, v := range keep {
		if v && strings.EqualFold(k, name) {
			return true
		}
	}

	return false
}

// pruneChildren removes, from the real directory dir, the real directories
// directly inside it whose names pick accepts. It returns the names
// removed; failures leave the directory for the next run.
func pruneChildren(dir string, pick func(name string) bool) []string {
	root, err := openRealDir(dir)
	if err != nil {
		return nil
	}
	defer root.Close()

	f, err := root.Open(".")
	if err != nil {
		return nil
	}

	entries, err := f.ReadDir(-1)
	_ = f.Close()

	if err != nil {
		return nil
	}

	var removed []string

	for _, e := range entries {
		name := e.Name()
		if !pick(name) {
			continue
		}

		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			continue
		}

		if root.RemoveAll(name) == nil {
			removed = append(removed, name)
		}
	}

	return removed
}

// RemoveProjectDir removes <root>/<project> with everything in it: only a
// real directory, removed through root (the [Compose] directory), so no
// symlink in it is followed. A project directory that isn't there is not an
// error.
func RemoveProjectDir(root, project string) error {
	if err := api.CheckGroup(project); err != nil {
		return err
	}

	return removeChildDir(root, project)
}

// Lock takes the lock of the project directory projectDir: the file
// <projectDir>/lock, created exclusively (0600) and kept until the returned
// unlock runs. A lock that exists is [ErrLocked], unless it is older than
// [LockMaxAge]: that one is taken over once (a run that died). unlock
// removes only the file this call created.
//
// A lock is told apart from another by what it holds — this run's pid and
// a fresh random token, written by the run that created it — and never by
// its file identity: once a stale lock is taken over, the new lock may get
// the old one's inode number (ext4 reuses a freed inode at once), so
// [os.SameFile] can't tell them apart.
func Lock(projectDir string) (unlock func(), err error) {
	return lock(projectDir, time.Now())
}

// lockReadMax bounds how much of a lock file is read: a lock is one short
// line ("<pid> <token>\n").
const lockReadMax = 256

// errNotLock is readLock's error for a lock path that holds something no
// run writes: not a regular file (a symlink, say), too long, or not one
// complete line (a run that died while writing it). Such a lock is never
// taken over, however old: only a user may delete it.
var errNotLock = errors.New("not a complete eyedbg lock file")

func lock(projectDir string, now time.Time) (func(), error) {
	root, err := openRealDir(projectDir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	path := filepath.Join(projectDir, lockName)

	for attempt := range 2 {
		content, err := createLock(root)
		if err == nil {
			return unlockFunc(projectDir, content), nil
		}

		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("take the lock %s: %w", path, err)
		}

		held, info, rerr := readLock(root, lockName)
		if errors.Is(rerr, fs.ErrNotExist) {
			continue // released between the two calls
		}

		if rerr != nil || attempt > 0 || now.Sub(info.ModTime()) <= LockMaxAge {
			return nil, busyError(path, info, now)
		}

		if !removeLock(root, held) {
			return nil, busyError(path, info, now)
		}
	}

	return nil, busyError(path, nil, now)
}

// createLock creates the lock file exclusively in root and writes this
// run's line to it, which it returns.
func createLock(root *os.Root) ([]byte, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, fmt.Errorf("make a lock token: %w", err)
	}

	content := []byte(strconv.Itoa(os.Getpid()) + " " + hex.EncodeToString(token[:]) + "\n")

	f, err := root.OpenFile(lockName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err //nolint:wrapcheck // The caller tests the cause with errors.Is.
	}

	_, werr := f.Write(content)

	if cerr := f.Close(); werr == nil {
		werr = cerr
	}

	if werr != nil {
		// Still this run's: only a lock older than LockMaxAge is taken over.
		_ = root.Remove(lockName)

		return nil, werr
	}

	return content, nil
}

// readLock reads the lock file name in root without following a symlink:
// its content and its info, both from one open file (so the age goes with
// the content). The info is returned with errNotLock too, for the age.
func readLock(root *os.Root, name string) ([]byte, fs.FileInfo, error) {
	seen, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err //nolint:wrapcheck // The caller tests the cause with errors.Is.
	}

	if !seen.Mode().IsRegular() {
		return nil, seen, errNotLock
	}

	f, err := root.Open(name)
	if err != nil {
		return nil, seen, err //nolint:wrapcheck // The caller tests the cause with errors.Is.
	}
	defer f.Close()

	// What was opened is what was checked (not a symlink put there since):
	// both exist at once here, so their identities can be compared.
	info, err := f.Stat()
	if err != nil || !os.SameFile(seen, info) {
		return nil, seen, errNotLock
	}

	content, err := io.ReadAll(io.LimitReader(f, lockReadMax+1))
	if err != nil {
		return nil, info, fmt.Errorf("read the lock: %w", err)
	}

	// One complete line: a lock being written (or never finished) is not
	// one, so its content can't match a lock read before.
	if len(content) == 0 || len(content) > lockReadMax || bytes.IndexByte(content, '\n') != len(content)-1 {
		return nil, info, errNotLock
	}

	return content, info, nil
}

// removeLock removes the lock in root if it holds want, and reports whether
// it did. The lock is renamed to a fresh private name first and checked
// there, so what is removed is exactly what was checked. A lock that isn't
// want (another run took it over in between) is put back.
//
// What it removes is race-free: the private name is this call's alone. The
// residual race is in the put-back: the lock name is empty between the
// rename and the link back, so a third run may take the lock then (the
// moved one is then dropped, and its holder runs alongside the third). That
// takes another run replacing want between the check below and the rename,
// which only happens to a lock older than LockMaxAge: two runs taking over
// one stale lock at once, or a run outliving LockMaxAge — one whose
// exclusivity was already gone.
func removeLock(root *os.Root, want []byte) bool {
	// Narrow the put-back window: only a lock that holds want is moved.
	if cur, _, err := readLock(root, lockName); err != nil || !bytes.Equal(cur, want) {
		return false
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return false
	}

	moved := lockName + ".stale-" + hex.EncodeToString(b[:])
	if err := root.Rename(lockName, moved); err != nil {
		return false
	}

	if got, _, err := readLock(root, moved); err == nil && bytes.Equal(got, want) {
		_ = root.Remove(moved)

		return true
	}

	// Not want: a run took the lock in between. Put it back (Link fails if
	// yet another run took the name; then that one owns it).
	_ = root.Link(moved, lockName)
	_ = root.Remove(moved)

	return false
}

// unlockFunc removes the lock in projectDir if it still holds content, the
// line this run wrote: a lock taken over after it went stale is the new
// holder's.
func unlockFunc(projectDir string, content []byte) func() {
	return func() {
		root, err := openRealDir(projectDir)
		if err != nil {
			return
		}
		defer root.Close()

		removeLock(root, content)
	}
}

func busyError(path string, held fs.FileInfo, now time.Time) error {
	age := ""
	if held != nil {
		age = " (" + now.Sub(held.ModTime()).Round(time.Second).String() + " old)"
	}

	return fmt.Errorf("the compose lock %s is held%s; if no run is going on, delete it: %w", path, age, ErrLocked)
}
