// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
)

// Bounds of fast mode's host build and project search.
const (
	// publishTimeout bounds one publish.
	publishTimeout = 15 * time.Minute
	// publishLogTail is how much of the end of the build log is read for an
	// error.
	publishLogTail = 64 << 10
	// maxProjectCandidates is how many matching projects an error lists.
	maxProjectCandidates = 10
	// pathMapTarget is where PDBs say the sources are: the Visual Studio
	// template's WORKDIR, so the default path map just works.
	pathMapTarget = "/src/"
)

// unreadableLog stands in for a build log's tail that can't be read.
const unreadableLog = "(the build log can't be read)"

// skippedProjectDir reports whether the project search doesn't enter the
// directory name: dot-directories, bin, obj and node_modules.
func skippedProjectDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "bin" || name == "obj" || name == "node_modules"
}

// FindContainerProject finds the project that builds the assembly dll (an
// "X.dll", per container.ValidDLL): the unique X.csproj, X.fsproj or
// X.vbproj, a regular file, under realpath(root). The walk follows no
// symlink, skips dot-directories, bin, obj and node_modules, skips
// directories it can't read, and fails after limit entries. No match and
// several are INVALID_REQUEST naming --dotnet-project (and listing up to
// ten candidates). It returns the project's absolute path below the
// resolved root.
//
// rw are the host paths of the read-write bind mounts of the stack's
// containers (container.Engine.RWBindSources): a container can write
// anything under one, a project file included, and building a project is
// running its code on this machine. So a project at or under one of them is
// never returned: it is not counted among the matches, and when it is the
// only match the error says so. When root is itself at or under one, no
// project is found at all. Mounts are compared as the same directory
// (os.SameFile on each ancestor, so symlinks and letter case don't matter),
// else by resolved path when a source can't be read here. A read-only mount
// is not in rw: its container can't write it.
func FindContainerProject(root, dll string, limit int, rw []string) (string, error) {
	if !container.ValidDLL(dll) {
		return "", api.NewError(api.CodeInvalidRequest, "invalid assembly name "+quote(dll), "")
	}

	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", api.NewError(api.CodeInvalidRequest, "can't search "+quote(root)+" for the project of "+dll+": "+err.Error(),
			"pass --dotnet-project SERVICE=PATH")
	}

	mounts := newMountSet(rw)

	if src := mounts.covering(realRoot, ""); src != "" {
		return "", api.NewError(api.CodeInvalidRequest, "the compose directory "+quote(realRoot)+" is at or under "+quote(src)+", a read-write bind mount of one of the stack's containers: "+
			"a container could have written any project file there, so none is searched for the project of "+dll,
			"pass --dotnet-project SERVICE=PATH to name the project yourself")
	}

	search := projectSearch{name: strings.TrimSuffix(dll, ".dll"), root: realRoot, limit: limit, mounts: mounts}

	if err := filepath.WalkDir(realRoot, search.visit); err != nil {
		return "", api.NewError(api.CodeInvalidRequest, fmt.Sprintf("searched %d entries of %s for the project of %s without finishing", limit, quote(realRoot), dll),
			"pass --dotnet-project SERVICE=PATH")
	}

	if len(search.found) == 0 && len(search.written) > 0 {
		return "", writtenError(realRoot, dll, search.written)
	}

	return oneProject(search.found, realRoot, dll)
}

// projectSearch is one walk for a project: the matches outside the mounts a
// container can write (found), and those inside (written).
type projectSearch struct {
	name, root     string
	limit, seen    int
	mounts         mountSet
	found, written []string
}

// visit is the walk's function: it counts entries, skips what is not
// searched and sorts the matches.
func (ps *projectSearch) visit(p string, d fs.DirEntry, err error) error {
	ps.seen++
	if ps.seen > ps.limit {
		return errSearchLimit
	}

	switch {
	case err != nil && d != nil && d.IsDir():
		return filepath.SkipDir // unreadable: a project there couldn't be built either
	case err != nil:
		return nil //nolint:nilerr // An entry that can't be read can't be a project that could be built either.
	case d.IsDir():
		if p != ps.root && skippedProjectDir(d.Name()) {
			return filepath.SkipDir
		}
	case d.Type().IsRegular() && isProjectOf(d.Name(), ps.name):
		if ps.mounts.covering(p, ps.root) != "" {
			ps.written = append(ps.written, p)
		} else {
			ps.found = append(ps.found, p)
		}
	}

	return nil
}

// writtenError is the refusal of a project found only where a container can
// write.
func writtenError(root, dll string, written []string) error {
	listed := make([]string, 0, maxProjectCandidates)

	for _, p := range written[:min(len(written), maxProjectCandidates)] {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}

		listed = append(listed, quote(rel))
	}

	return api.NewError(api.CodeInvalidRequest,
		"the only project for "+dll+" is in a directory a container can write (a read-write bind mount): "+strings.Join(listed, ", "),
		"a container could have planted it; if it is yours, pass --dotnet-project SERVICE=PATH")
}

// mountSet is the read-write bind mount sources of a stack, as they can be
// compared.
type mountSet struct {
	paths []string
	infos []os.FileInfo
}

// newMountSet reads each source: its file info when it exists here (os.Stat
// follows a symlink; an entry without one is compared by its path alone).
func newMountSet(sources []string) mountSet {
	var m mountSet

	for _, src := range sources {
		info, err := os.Stat(src)
		if err != nil {
			info = nil
		}

		m.paths = append(m.paths, filepath.Clean(src))
		m.infos = append(m.infos, info)
	}

	return m
}

// covering returns the source that p is at or under, "" when none: p and each
// of its parents up to (not above) stop are compared with every source. A
// stop of "" goes up to the file system root.
func (m mountSet) covering(p, stop string) string {
	if len(m.paths) == 0 {
		return ""
	}

	for cur := p; ; cur = filepath.Dir(cur) {
		for i, src := range m.paths {
			if cur == src || (m.infos[i] != nil && sameFile(cur, m.infos[i])) {
				return src
			}
		}

		if cur == stop || filepath.Dir(cur) == cur {
			return ""
		}
	}
}

// sameFile reports whether the file at p is the one info describes.
func sameFile(p string, info os.FileInfo) bool {
	cur, err := os.Stat(p)

	return err == nil && os.SameFile(cur, info)
}

// errSearchLimit ends a project search that read too many entries.
var errSearchLimit = errors.New("too many entries")

// isProjectOf reports whether file is name's project file.
func isProjectOf(file, name string) bool {
	for _, ext := range container.ProjectExts() {
		if file == name+ext {
			return true
		}
	}

	return false
}

// oneProject is the only element of found, or the error for none or
// several.
func oneProject(found []string, root, dll string) (string, error) {
	const hint = "pass --dotnet-project SERVICE=PATH"

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", api.NewError(api.CodeInvalidRequest, "no project file for "+dll+" (a "+strings.TrimSuffix(dll, ".dll")+".csproj) found under "+quote(root), hint)
	default:
		listed := found
		more := ""

		if len(listed) > maxProjectCandidates {
			listed, more = listed[:maxProjectCandidates], fmt.Sprintf(" (and %d more)", len(found)-maxProjectCandidates)
		}

		quoted := make([]string, len(listed))
		for i, p := range listed {
			quoted[i] = quote(p)
		}

		return "", api.NewError(api.CodeInvalidRequest, fmt.Sprintf("%d projects build %s: %s%s", len(found), dll, strings.Join(quoted, ", "), more), hint)
	}
}

// quote is s for a message: cut, without control characters.
func quote(s string) string {
	const maxShown = 120

	if len(s) > maxShown {
		s = s[:maxShown] + "..."
	}

	return `"` + strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(s, "?")) + `"`
}

// PublishSpec is one host publish for a container.
type PublishSpec struct {
	// Host is the dotnet host executable.
	Host string
	// Project is the project file, Root the compose project's directory
	// (both absolute, existing): Project must lie inside Root. Root is what
	// PathMap maps to /src/.
	Project, Root string
	// Out is the directory the output goes to (-o) and Artifacts the
	// --artifacts-path directory (both absolute): they keep the user's bin
	// and obj untouched.
	Out, Artifacts string
	// DLL is the assembly the project must produce (X.dll): Out must hold it
	// and X.pdb afterwards.
	DLL string
	// Log receives the build's stdout and stderr (a file: no pipe a lingering
	// build server could hold open).
	Log *os.File

	// env is extra environment for the build: the seam a test fakes dotnet
	// at (the test binary re-executes itself as Host).
	env []string
	// timeout, when set, replaces publishTimeout.
	timeout time.Duration
}

// PublishForContainer builds the project for a container: 'dotnet publish
// PROJECT -c Debug -o OUT --artifacts-path ARTIFACTS -p:UseAppHost=false
// -p:DebugType=portable -p:PathMap=ROOT/=/src/ -nologo -tl:off' in the
// caller's environment (as every build here), with Root as the working
// directory (where global.json is looked for), bounded by ctx and 15
// minutes. The paths are refused, not escaped, when they hold a character
// MSBuild or Roslyn would split or unescape; Project must lie inside Root
// (both resolved). A failed build is BUILD_FAILED with the log's last
// lines and its path; success needs OUT/DLL and OUT/<name>.pdb.
func PublishForContainer(ctx context.Context, spec PublishSpec) error {
	argv, root, err := spec.publishArgs()
	if err != nil {
		return err
	}

	timeout := publishTimeout
	if spec.timeout > 0 {
		timeout = spec.timeout
	}

	ctxRun, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := buildCommand(ctxRun, spec.Host, argv)
	cmd.Env = append(cmd.Env, spec.env...)
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = spec.Log, spec.Log

	if err := cmd.Run(); err != nil {
		return spec.publishError(ctx, ctxRun, err)
	}

	return spec.checkOutputs()
}

// publishArgs checks the spec and returns the argv (after the host) and the
// resolved root.
func (s PublishSpec) publishArgs() (argv []string, root string, err error) {
	if s.Host == "" || s.Log == nil {
		return nil, "", api.NewError(api.CodeInternal, "publish without a dotnet host or a log file", "")
	}

	if !container.ValidDLL(s.DLL) {
		return nil, "", api.NewError(api.CodeInvalidRequest, "invalid assembly name "+quote(s.DLL), "")
	}

	root, project, err := s.resolve()
	if err != nil {
		return nil, "", err
	}

	for _, p := range [][2]string{{"the compose directory", root}, {"the project", project}, {"the output directory", s.Out}, {"the artifacts directory", s.Artifacts}} {
		if err := checkMSBuildPath(p[0], p[1]); err != nil {
			return nil, "", err
		}
	}

	if !filepath.IsAbs(s.Out) || !filepath.IsAbs(s.Artifacts) {
		return nil, "", api.NewError(api.CodeInternal, "publish output paths must be absolute", "")
	}

	return []string{
		"publish", project, "-c", dotnetConfig, "-o", s.Out, "--artifacts-path", s.Artifacts,
		"-p:UseAppHost=false", "-p:DebugType=portable",
		"-p:PathMap=" + root + string(filepath.Separator) + "=" + pathMapTarget,
		msbuildNoLogo, "-tl:off",
	}, root, nil
}

// resolve returns the real Root and Project, checking Project is a project
// file inside Root and Root isn't a file system root.
func (s PublishSpec) resolve() (root, project string, err error) {
	if !filepath.IsAbs(s.Root) || !filepath.IsAbs(s.Project) {
		return "", "", api.NewError(api.CodeInternal, "publish paths must be absolute", "")
	}

	root, err = filepath.EvalSymlinks(s.Root)
	if err != nil {
		return "", "", api.NewError(api.CodeInvalidRequest, "the compose directory "+quote(s.Root)+" can't be resolved: "+err.Error(), "")
	}

	project, err = filepath.EvalSymlinks(s.Project)
	if err != nil {
		return "", "", api.NewError(api.CodeInvalidRequest, "the project "+quote(s.Project)+" can't be resolved: "+err.Error(), "")
	}

	if filepath.Dir(root) == root {
		return "", "", api.NewError(api.CodeInvalidRequest, "the compose directory "+quote(root)+" is a file system root", "run from the project's own directory")
	}

	if !strings.HasPrefix(project, root+string(filepath.Separator)) {
		return "", "", api.NewError(api.CodeInvalidRequest, "the project "+quote(project)+" is not inside the compose directory "+quote(root),
			"fast mode builds only projects below the compose project's directory")
	}

	if info, serr := os.Stat(project); serr != nil || !info.Mode().IsRegular() || !container.IsProjectFile(info.Name()) {
		return "", "", api.NewError(api.CodeInvalidRequest, quote(project)+" is not a project file", "")
	}

	return root, project, nil
}

// checkMSBuildPath refuses a path MSBuild or Roslyn would misread: one
// holding ',' or ';' (a property list), '%' (an escape), '=' (a path map
// pair), '"' or a control character, or not UTF-8. Refused, never escaped.
func checkMSBuildPath(what, p string) error {
	if !utf8.ValidString(p) {
		return api.NewError(api.CodeInvalidRequest, what+" "+quote(p)+" is not valid UTF-8", "")
	}

	for _, r := range p {
		if strings.ContainsRune(`,;%="`, r) || unicode.IsControl(r) {
			return api.NewError(api.CodeInvalidRequest, fmt.Sprintf("%s %s holds a character MSBuild would split on (%q)", what, quote(p), r),
				"rename or move the directory so its path has no , ; % = \" or control characters")
		}
	}

	return nil
}

// publishError is the error of a publish that didn't finish: the caller's
// context ending, the time limit, or the build failing.
func (s PublishSpec) publishError(parent, run context.Context, err error) error {
	if cerr := parent.Err(); cerr != nil {
		return fmt.Errorf("dotnet publish %s: %w", s.Project, cerr)
	}

	if run.Err() != nil {
		return api.NewError(api.CodeBuildFailed, "dotnet publish "+quote(s.Project)+" did not finish in time", "the build's output so far is in "+quote(s.Log.Name()))
	}

	if _, exited := errors.AsType[*exec.ExitError](err); exited {
		return api.NewError(api.CodeBuildFailed, "dotnet publish "+quote(s.Project)+" failed:\n"+s.logTail(),
			"fix the build errors above, then run it again; the whole build log is "+quote(s.Log.Name()))
	}

	return api.NewError(api.CodeBuildFailed, "dotnet publish "+quote(s.Project)+" failed: "+err.Error(), "check the .NET SDK ('dotnet --info')")
}

// logTail is the last lines of the build log, control characters (but
// newlines) turned into spaces.
func (s PublishSpec) logTail() string {
	f, err := os.Open(s.Log.Name())
	if err != nil {
		return unreadableLog
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.Size() > publishLogTail {
		if _, err := f.Seek(-publishLogTail, io.SeekEnd); err != nil {
			return unreadableLog
		}
	}

	data, err := io.ReadAll(io.LimitReader(f, publishLogTail))
	if err != nil {
		return unreadableLog
	}

	clean := strings.Map(func(r rune) rune {
		if r != '\n' && unicode.IsControl(r) {
			return ' '
		}

		return r
	}, strings.ToValidUTF8(string(data), "?"))

	return tail(clean, buildOutputLines)
}

// checkOutputs requires OUT/DLL and OUT/<name>.pdb, regular files.
func (s PublishSpec) checkOutputs() error {
	for _, name := range []string{s.DLL, strings.TrimSuffix(s.DLL, ".dll") + ".pdb"} {
		info, err := os.Lstat(filepath.Join(s.Out, name))
		if err != nil || !info.Mode().IsRegular() {
			return api.NewError(api.CodeBuildFailed, "dotnet publish "+quote(s.Project)+" succeeded but produced no "+name,
				"the project's assembly name must match the container's entrypoint DLL ("+s.DLL+"); the build log is "+quote(s.Log.Name()))
		}
	}

	return nil
}
