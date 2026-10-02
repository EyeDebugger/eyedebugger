// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"cmp"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Path map limits.
const (
	maxPathMappings = 16
	maxRemotePath   = 1024
)

// PathMap maps source paths between the host and a debuggee that names its
// sources by another file system's paths (a container: docs/adr/0020, D5).
// Each pair maps a remote directory (an absolute, clean POSIX path) to a
// host directory (absolute, symlinks resolved). The longest root on a
// path-segment boundary wins; a path no root takes doesn't map. A PathMap
// is immutable, and its methods are safe for concurrent use; only
// [PathMap.Readable] touches the file system.
type PathMap struct {
	pairs []pathPair // as given
	// byLocal and byRemote are pairs sorted by the length of that side,
	// longest first.
	byLocal, byRemote []pathPair
}

type pathPair struct{ remote, local string }

// NewPathMap checks pairs and returns their map: at most 16 pairs, each
// Remote an absolute, clean POSIX path (no backslash, no control
// characters, at most 1024 bytes), each Local an absolute path to an
// existing host directory, stored with symlinks resolved. A Remote or a
// Local given twice (a Local by another name of the same directory, or on
// Windows in another case) is ambiguous and refused. No pairs is an empty
// map, under which nothing maps. Errors are INVALID_REQUEST.
func NewPathMap(pairs []api.PathMapping) (*PathMap, error) {
	if len(pairs) > maxPathMappings {
		return nil, mapError(fmt.Sprintf("%d path mappings: at most %d are allowed", len(pairs), maxPathMappings))
	}

	m := &PathMap{pairs: make([]pathPair, 0, len(pairs))}
	dirs := make([]os.FileInfo, 0, len(pairs))

	for _, in := range pairs {
		if err := checkRemoteRoot(in.Remote); err != nil {
			return nil, err
		}

		local, dir, err := checkLocalRoot(in.Local)
		if err != nil {
			return nil, err
		}

		for i, p := range m.pairs {
			switch {
			case p.remote == in.Remote:
				return nil, mapError(fmt.Sprintf("container path %q is mapped twice", in.Remote))
			case sameLocal(p.local, local) || os.SameFile(dirs[i], dir):
				return nil, mapError(fmt.Sprintf("host directory %q is mapped twice", local))
			}
		}

		m.pairs = append(m.pairs, pathPair{remote: in.Remote, local: local})
		dirs = append(dirs, dir)
	}

	m.byLocal, m.byRemote = slices.Clone(m.pairs), slices.Clone(m.pairs)
	slices.SortStableFunc(m.byLocal, func(a, b pathPair) int { return cmp.Compare(len(b.local), len(a.local)) })
	slices.SortStableFunc(m.byRemote, func(a, b pathPair) int { return cmp.Compare(len(b.remote), len(a.remote)) })

	return m, nil
}

// checkRemoteRoot checks a pair's container directory.
func checkRemoteRoot(remote string) error {
	switch {
	case len(remote) > maxRemotePath:
		return mapError(fmt.Sprintf("container path of %d bytes: at most %d are allowed", len(remote), maxRemotePath))
	case !strings.HasPrefix(remote, "/") || strings.ContainsRune(remote, '\\') || !plainText(remote):
		return mapError(fmt.Sprintf("container path %q: want an absolute POSIX path (from /, no backslashes or control characters)", remote))
	case remote != path.Clean(remote):
		return mapError(fmt.Sprintf("container path %q: want it clean, as %q", remote, path.Clean(remote)))
	}

	return nil
}

// checkLocalRoot checks a pair's host directory and returns it with its
// symlinks resolved, and its file info.
func checkLocalRoot(local string) (resolved string, info os.FileInfo, err error) {
	if !filepath.IsAbs(local) {
		return "", nil, mapError(fmt.Sprintf("host directory %q: want an absolute path", local))
	}

	resolved, err = filepath.EvalSymlinks(filepath.Clean(local))
	if err != nil {
		return "", nil, mapError(fmt.Sprintf("host directory %q: not found", local))
	}

	info, err = os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", nil, mapError(fmt.Sprintf("host directory %q: not a directory", local))
	}

	return resolved, info, nil
}

// sameLocal reports whether two host directories are the same by name (in
// any case on Windows).
func sameLocal(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}

	return a == b
}

func mapError(msg string) error {
	return api.NewError(api.CodeInvalidRequest, "invalid path map: "+msg,
		"each --map is REMOTE=LOCAL: an absolute container directory and the host directory with the same sources, each at most once")
}

// Mappings returns the map's pairs as given, with the host directories
// resolved.
func (m *PathMap) Mappings() []api.PathMapping {
	out := make([]api.PathMapping, len(m.pairs))
	for i, p := range m.pairs {
		out[i] = api.PathMapping{Remote: p.remote, Local: p.local}
	}

	return out
}

// String lists the pairs as REMOTE=LOCAL, for hints.
func (m *PathMap) String() string {
	if len(m.pairs) == 0 {
		return "(empty)"
	}

	parts := make([]string, len(m.pairs))
	for i, p := range m.pairs {
		parts[i] = p.remote + "=" + p.local
	}

	return strings.Join(parts, ", ")
}

// checkBreakpoint refuses a line breakpoint whose file has no container
// path in m (INVALID_REQUEST): the adapter could never bind it. Function
// breakpoints pass, and every breakpoint passes a nil map (no map).
func (m *PathMap) checkBreakpoint(spec api.BreakpointSpec) error {
	if m == nil || spec.Function != "" {
		return nil
	}

	if _, ok := m.ToRemote(spec.File); ok {
		return nil
	}

	return api.NewError(api.CodeInvalidRequest, spec.File+" is outside the container's path map",
		"the map is "+m.String()+": a breakpoint's file must be under one of its host directories (--map REMOTE=LOCAL adds one)")
}

// ToRemote maps a host path to the container: under the longest host
// directory that holds it, to the same relative path under that pair's
// container directory. A path outside every host directory, or one whose
// container name couldn't be mapped back (control characters; on a
// non-Windows host a backslash, an ordinary character there; on Windows a
// colon or a reserved name), doesn't map.
func (m *PathMap) ToRemote(host string) (string, bool) {
	for _, p := range m.byLocal {
		rel, err := filepath.Rel(p.local, host)
		if err != nil || escapes(rel) {
			continue
		}

		if rel == "." {
			return p.remote, true
		}

		slash := filepath.ToSlash(rel)
		if _, ok := localRel(slash); !ok {
			return "", false
		}

		return path.Join(p.remote, slash), true
	}

	return "", false
}

// ToLocal maps a container path to the host: under the longest container
// directory that holds it (on a segment boundary: /srcx is not under
// /src), to the same relative path under that pair's host directory. Only
// an absolute path maps, and none with control characters or a ".."
// segment (the container resolves ".." after symlinks, which a host can't
// mirror); the rest may not hold a backslash (on Windows, nor a colon or a
// reserved name), and the result must lie inside the host directory. Once
// the longest root matched, no shorter one is tried.
func (m *PathMap) ToLocal(remote string) (string, bool) {
	if !strings.HasPrefix(remote, "/") || !plainText(remote) || hasDotDot(remote) {
		return "", false
	}

	c := path.Clean(remote)

	for _, p := range m.byRemote {
		rel, ok := under(p.remote, c)
		if !ok {
			continue
		}

		if rel == "" {
			return p.local, true
		}

		osRel, ok := localRel(rel)
		if !ok {
			return "", false
		}

		out := filepath.Join(p.local, osRel)
		if !within(p.local, out) {
			return "", false
		}

		return out, true
	}

	return "", false
}

// Readable reports whether local, with its symlinks resolved, lies inside
// one of the map's host directories: the only host paths a session with a
// map reads.
func (m *PathMap) Readable(local string) bool {
	_, _, ok := m.resolve(local)

	return ok
}

// resolve returns local with its symlinks resolved, below the longest host
// directory that holds it: that directory and the relative path.
func (m *PathMap) resolve(local string) (root, rel string, ok bool) {
	resolved, err := filepath.EvalSymlinks(local)
	if err != nil {
		return "", "", false
	}

	for _, p := range m.byLocal {
		if within(p.local, resolved) {
			rel = strings.TrimPrefix(strings.TrimPrefix(resolved, p.local), string(filepath.Separator))
			if rel == "" {
				rel = "."
			}

			return p.local, rel, true
		}
	}

	return "", "", false
}

// open opens local for reading when it is [PathMap.Readable]: its resolved
// path, through the host directory that holds it, following no symlink out
// of that directory (so one swapped in after the check can't lead out).
func (m *PathMap) open(local string) (*os.File, error) {
	root, rel, ok := m.resolve(local)
	if !ok {
		return nil, fmt.Errorf("open %s: outside the path map: %w", local, os.ErrPermission)
	}

	f, err := os.OpenInRoot(root, rel)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", local, err)
	}

	return f, nil
}

// under returns c's path below root (a clean absolute POSIX path; "/"
// holds everything), "" for root itself.
func under(root, c string) (string, bool) {
	switch {
	case root == "/":
		return c[1:], true
	case c == root:
		return "", true
	case strings.HasPrefix(c, root+"/"):
		return c[len(root)+1:], true
	default:
		return "", false
	}
}

// localRel turns rel, a relative slash-separated path, into a host path
// that stays below the directory it is joined to, or reports false: no
// empty, "." or ".." segment, no backslash or control character, and what
// filepath.Localize refuses (on Windows a colon and reserved names).
func localRel(rel string) (string, bool) {
	if strings.ContainsRune(rel, '\\') || !plainText(rel) {
		return "", false
	}

	if runtime.GOOS == "windows" && strings.ContainsRune(rel, ':') {
		return "", false
	}

	for seg := range strings.SplitSeq(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", false
		}
	}

	out, err := filepath.Localize(rel)
	if err != nil || !filepath.IsLocal(out) {
		return "", false
	}

	return out, true
}

// escapes reports whether rel (from filepath.Rel) leaves its base.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(rel) || filepath.VolumeName(rel) != ""
}

// within reports whether p is root or below it, comparing names exactly
// (both sides come from the same resolution, so a case difference means a
// different file on a case-sensitive directory, and is refused).
func within(root, p string) bool {
	if p == root {
		return true
	}

	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}

	return strings.HasPrefix(p, prefix)
}

// hasDotDot reports whether a slash-separated path has a ".." segment.
func hasDotDot(p string) bool {
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return true
		}
	}

	return false
}

// plainText reports whether s is valid UTF-8 without control characters
// (NUL included).
func plainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}

	return !strings.ContainsFunc(s, unicode.IsControl)
}
