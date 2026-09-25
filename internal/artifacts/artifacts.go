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

// Kinds: the subdirectories of eyedbg's home that hold artifacts.
const (
	// Dumps holds dumps (eyedbg dotnet dump, heap, threads).
	Dumps = "dumps"
	// Traces holds traces (eyedbg dotnet trace) and the scratch files of
	// their summaries.
	Traces = "traces"
)

// Retention of eyedbg's own artifacts (Prune), the same for every kind.
const (
	// MaxAge is how long an artifact is kept.
	MaxAge = 7 * 24 * time.Hour
	// Keep is how many artifacts of a kind are kept at most: a heap dump
	// of a small app is hundreds of MB, a trace up to 512 MiB.
	Keep = 10
	// ScratchMaxAge is how long a summary's scratch file may stay: a
	// summary is bounded by its --timeout (at most an hour), so an older
	// one was left by a summary that was killed.
	ScratchMaxAge = time.Hour
)

// The parts of every name: <pid>-<UTC time>-<type>-<8 hex><extension>.
const (
	namePrefix  = `^[0-9]+-[0-9]{8}T[0-9]{6}Z-(`
	nameRandom  = `)-[0-9a-f]{8}`
	scratchExt  = ".etlx"
	scratchNext = ".new" // TraceLog writes <scratch>.new, then renames it
)

// namePattern is the pattern of kind's artifact names, or "" for an
// unknown kind (nothing matches).
func namePattern(kind string) string {
	switch kind {
	case Dumps:
		return namePrefix + `heap|mini|triage|full` + nameRandom + `\.dmp$`
	case Traces:
		return namePrefix + `cpu|gc` + nameRandom + `\.nettrace$`
	default:
		return ""
	}
}

// scratchPattern is the pattern of kind's scratch files, or "" for a kind
// without any: a trace summary's ETLX file (types: a trace's profile, or
// "file" for a trace given as a file) and TraceLog's .new sibling of it.
func scratchPattern(kind string) string {
	if kind != Traces {
		return ""
	}

	return namePrefix + `cpu|gc|file` + nameRandom + `\.etlx(\.new)?$`
}

// extension is the file extension of kind's artifacts.
func extension(kind string) string {
	switch kind {
	case Dumps:
		return ".dmp"
	case Traces:
		return ".nettrace"
	default:
		return ""
	}
}

// nameTries bounds picking a fresh name.
const nameTries = 5

// compile compiles one of this file's patterns; "" (an unknown kind) is nil.
func compile(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}

	return regexp.MustCompile(pattern)
}

// match reports whether name matches re; nil matches nothing.
func match(re *regexp.Regexp, name string) bool {
	return re != nil && re.MatchString(name)
}

// matches reports whether name matches pattern; "" matches nothing.
func matches(pattern, name string) bool {
	return match(compile(pattern), name)
}

// IsName reports whether name is one Name gives for kind.
func IsName(kind, name string) bool {
	return matches(namePattern(kind), name)
}

// TypeOf is the type (a dump type, a trace profile) a name of Name's form
// for kind records, or "".
func TypeOf(kind, name string) string {
	re := compile(namePattern(kind))
	if re == nil {
		return ""
	}

	m := re.FindStringSubmatch(name)
	if m == nil {
		return ""
	}

	return m[1]
}

// Dir returns the private directory for kind (Dumps, Traces), creating it if
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
		return errors.New(dir + " is a symlink; eyedbg keeps dumps and traces only in a real directory it owns")
	}

	if !info.IsDir() {
		return errors.New(dir + " is not a directory")
	}

	return checkPrivate(dir, info)
}

// Name returns a fresh name for kind's artifact of pid and type typ (a
// dump type, a trace profile): nothing is at dir/name. random supplies the
// name's 4 random bytes (crypto/rand.Reader).
func Name(dir, kind string, pid int, typ string, now time.Time, random io.Reader) (string, error) {
	return freshName(dir, namePattern(kind), extension(kind), pid, typ, now, random, nil)
}

// ScratchName returns a fresh name for the scratch file of a trace
// summary in dir (the Traces directory): typ is the trace's profile, or
// "file" for a trace given as a file. Nothing is at dir/name, nor at
// dir/name.new, which TraceLog writes first.
func ScratchName(dir string, pid int, typ string, now time.Time, random io.Reader) (string, error) {
	return freshName(dir, scratchPattern(Traces), scratchExt, pid, typ, now, random, []string{scratchNext})
}

// freshName picks <pid>-<UTC time>-<typ>-<8 hex><ext> with nothing at it
// in dir, nor at it plus any of suffixes. The name must match pattern: a
// name Prune doesn't know would never be removed.
func freshName(dir, pattern, ext string, pid int, typ string, now time.Time, random io.Reader, suffixes []string) (string, error) {
	for range nameTries {
		var b [4]byte
		if _, err := io.ReadFull(random, b[:]); err != nil {
			return "", fmt.Errorf("pick a file name: %w", err)
		}

		name := strconv.Itoa(pid) + "-" + now.UTC().Format("20060102T150405Z") + "-" + typ + "-" + hex.EncodeToString(b[:]) + ext
		if !matches(pattern, name) {
			return "", fmt.Errorf("pick a file name: %q isn't a name eyedbg keeps", name)
		}

		if free(dir, name, suffixes) {
			return name, nil
		}
	}

	return "", fmt.Errorf("pick a file name in %s: %d names taken", dir, nameTries)
}

// free reports whether nothing is at dir/name, nor at it plus a suffix.
func free(dir, name string, suffixes []string) bool {
	names := []string{name}
	for _, s := range suffixes {
		names = append(names, name+s)
	}

	for _, n := range names {
		if _, err := os.Lstat(filepath.Join(dir, n)); !errors.Is(err, fs.ErrNotExist) {
			return false
		}
	}

	return true
}

// Prune removes kind's artifacts from dir — regular files with Name's names
// for kind, nothing else — that are older than maxAge, then the oldest
// beyond the newest keep; and kind's scratch files (ScratchName's, and
// their .new siblings) older than ScratchMaxAge, which are never counted.
// protect (an artifact just made, or about to be analyzed) is never
// removed. Files that can't be removed (open on Windows) are left for the
// next run. It returns the names removed.
func Prune(dir, kind string, now time.Time, maxAge time.Duration, keep int, protect string) []string {
	artifacts, scratch := list(dir, kind)

	sort.Slice(artifacts, func(i, j int) bool {
		if !artifacts[i].mod.Equal(artifacts[j].mod) {
			return artifacts[i].mod.After(artifacts[j].mod)
		}

		return artifacts[i].name > artifacts[j].name
	})

	// By name: names are random, and the caller may hold the path through
	// another spelling of dir (a symlink in it).
	protected := ""
	if protect != "" {
		protected = filepath.Base(protect)
	}

	var removed []string

	remove := func(name string) {
		if os.Remove(filepath.Join(dir, name)) == nil {
			removed = append(removed, name)
		}
	}

	for i, a := range artifacts {
		if a.name != protected && (i >= keep || now.Sub(a.mod) > maxAge) {
			remove(a.name)
		}
	}

	// A running summary's scratch file is younger than an hour: its
	// --timeout is at most that.
	for _, s := range scratch {
		if s.name != protected && now.Sub(s.mod) > ScratchMaxAge {
			remove(s.name)
		}
	}

	return removed
}

// entry is a file Prune may remove.
type entry struct {
	name string
	mod  time.Time
}

// list returns kind's artifacts and scratch files in dir: regular files
// with their names, nothing else.
func list(dir, kind string) (artifacts, scratch []entry) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}

	names, scratchNames := compile(namePattern(kind)), compile(scratchPattern(kind))

	for _, e := range entries {
		isName, isScratch := match(names, e.Name()), match(scratchNames, e.Name())
		if !isName && !isScratch {
			continue
		}

		info, err := os.Lstat(filepath.Join(dir, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}

		if isName {
			artifacts = append(artifacts, entry{e.Name(), info.ModTime()})
		} else {
			scratch = append(scratch, entry{e.Name(), info.ModTime()})
		}
	}

	return artifacts, scratch
}

// Own resolves path's symlinks once and reports whether the result is an
// artifact of kind eyedbg made: a regular file with Name's name for kind
// directly in dir (the private directory, also resolved). It returns that
// resolved path: the one checked, which the caller must use from then on —
// a path resolved again could lead elsewhere; this one, in the 0700
// directory, can't be swapped by anyone else.
func Own(dir, kind, path string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}

	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", false
	}

	if filepath.Dir(resolved) != realDir || !IsName(kind, filepath.Base(resolved)) {
		return "", false
	}

	info, err := os.Lstat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	return resolved, true
}
