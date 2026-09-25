// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package artifacts

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
)

// Dumps is the kind (subdirectory of eyedbg's home) that holds dumps.
const Dumps = "dumps"

// Retention of eyedbg's own dumps (Prune).
const (
	// MaxAge is how long a dump is kept.
	MaxAge = 7 * 24 * time.Hour
	// Keep is how many dumps are kept at most: a heap dump of a small app
	// is hundreds of MB.
	Keep = 10
)

// dumpNamePattern matches the names Name gives: <pid>-<UTC time>-<type>-<8 hex>.dmp.
const dumpNamePattern = `^[0-9]+-[0-9]{8}T[0-9]{6}Z-(heap|mini|triage|full)-[0-9a-f]{8}\.dmp$`

// nameTries bounds picking a fresh name.
const nameTries = 5

// IsDumpName reports whether name is one Name gives.
func IsDumpName(name string) bool {
	return regexp.MustCompile(dumpNamePattern).MatchString(name)
}

// DumpType is the dump type a name of Name's form records, or "".
func DumpType(name string) string {
	m := regexp.MustCompile(dumpNamePattern).FindStringSubmatch(name)
	if m == nil {
		return ""
	}

	return m[1]
}

// Dir returns the private directory for kind (Dumps), creating it if
// needed: 0700, a real directory and not a symlink, and on Unix owned by
// the user (group or other access is removed).
func Dir(kind string) (string, error) {
	home, err := adapters.HomeDir()
	if err != nil {
		return "", fmt.Errorf("locate the %s directory: %w", kind, err)
	}

	dir := filepath.Join(home, kind)
	if err := prepare(dir); err != nil {
		return "", err
	}

	return dir, nil
}

// prepare creates dir if needed and checks it is private.
func prepare(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}

	if info.Mode()&fs.ModeSymlink != 0 {
		return errors.New(dir + " is a symlink; eyedbg keeps dumps only in a real directory it owns")
	}

	if !info.IsDir() {
		return errors.New(dir + " is not a directory")
	}

	return checkPrivate(dir, info)
}

// Name returns a fresh name for a dump of pid: nothing is at dir/name.
// random supplies the name's 4 random bytes (crypto/rand.Reader).
func Name(dir string, pid int, typ string, now time.Time, random io.Reader) (string, error) {
	for range nameTries {
		var b [4]byte
		if _, err := io.ReadFull(random, b[:]); err != nil {
			return "", fmt.Errorf("pick a dump name: %w", err)
		}

		name := strconv.Itoa(pid) + "-" + now.UTC().Format("20060102T150405Z") + "-" + typ + "-" + hex.EncodeToString(b[:]) + ".dmp"
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, fs.ErrNotExist) {
			return name, nil
		}
	}

	return "", fmt.Errorf("pick a dump name in %s: %d names taken", dir, nameTries)
}

// Prune removes eyedbg's dumps from dir — regular files with Name's names,
// nothing else — that are older than maxAge, then the oldest beyond the
// newest keep. protect (the dump about to be analyzed) is never removed. Files that can't
// be removed (open on Windows) are left for the next run. It returns the
// names removed.
func Prune(dir string, now time.Time, maxAge time.Duration, keep int, protect string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	type dump struct {
		name string
		mod  time.Time
	}

	var dumps []dump

	for _, e := range entries {
		if !IsDumpName(e.Name()) {
			continue
		}

		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}

		dumps = append(dumps, dump{e.Name(), info.ModTime()})
	}

	sort.Slice(dumps, func(i, j int) bool {
		if !dumps[i].mod.Equal(dumps[j].mod) {
			return dumps[i].mod.After(dumps[j].mod)
		}

		return dumps[i].name > dumps[j].name
	})

	// By name: names are random, and the caller may hold the path through
	// another spelling of dir (a symlink in it).
	protected := ""
	if protect != "" {
		protected = filepath.Base(protect)
	}

	var removed []string

	for i, d := range dumps {
		if d.name == protected || (i < keep && now.Sub(d.mod) <= maxAge) {
			continue
		}

		if os.Remove(filepath.Join(dir, d.name)) == nil {
			removed = append(removed, d.name)
		}
	}

	return removed
}

// OwnDump resolves path's symlinks once and reports whether the result is
// a dump eyedbg took: a regular file with Name's name directly in dir (the
// private directory, also resolved). It returns that resolved path: the
// one checked, which the caller must use from then on — a path resolved
// again could lead elsewhere; this one, in the 0700 directory, can't be
// swapped by anyone else.
func OwnDump(dir, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}

	if filepath.Dir(resolved) != realDir || !IsDumpName(filepath.Base(resolved)) {
		return "", false
	}

	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	return resolved, true
}
