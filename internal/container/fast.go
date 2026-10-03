// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Labels of fast mode (docs/adr/0021): compose's, and eyedbg's own, which
// the override puts on the containers it changes. eyedbg's are read back as
// untrusted input.
const (
	labelConfigFiles = "com.docker.compose.project.config_files"

	labelFastPrefix  = "dev.izzat.eyedbg.fast."
	labelFastVersion = labelFastPrefix + "version"
	labelFastDLL     = labelFastPrefix + "dll"
	labelFastWorkDir = labelFastPrefix + "workdir"
	labelFastProject = labelFastPrefix + "project"
)

// FastVersion is the format of eyedbg's fast-mode labels this code writes
// and understands.
const FastVersion = "1"

// Limits of what fast mode reads and writes.
const (
	// maxDLLName bounds an assembly file name.
	maxDLLName = 255
	// maxAppDirLen bounds a container's working directory.
	maxAppDirLen = 255
	// maxAppDirSegments bounds how deep a working directory may be.
	maxAppDirSegments = 4
	// maxProjectRel bounds a project file's path below the compose directory.
	maxProjectRel = 1024
	// maxFastConfigFiles bounds the compose files a container lists.
	maxFastConfigFiles = maxComposeFiles
)

// fastInspectTemplate selects, from docker's inspect output, the only fields
// fast mode reads of a container: a JSON array of
//
//	id, name, image id, platform (os), running, paused, restarting,
//	the DLL of an exactly-two-element 'dotnet X.dll' entrypoint or null,
//	whether the command is non-empty, the working directory,
//	compose labels project, service, project.working_dir, project.config_files,
//	eyedbg labels fast.version, .override, .dll, .workdir, .project (19 values)
//
// It never names the environment, the arguments (Args), the command's
// values (Cmd appears only inside {{if}}, which yields a boolean), an
// entrypoint's elements outside the shape gate (the gate yields its second
// element only when the entrypoint is exactly "dotnet", then one element),
// or a healthcheck. Every label is read with index, which yields null for
// a missing one; a container without labels at all gets nulls.
const fastInspectTemplate = `[{{json .Id}},{{json .Name}},{{json .Image}},{{json .Platform}},` +
	`{{json .State.Running}},{{json .State.Paused}},{{json .State.Restarting}},` +
	`{{with .Config.Entrypoint}}{{if and (eq (len .) 2) (eq (index . 0) "dotnet")}}{{json (index . 1)}}{{else}}null{{end}}{{else}}null{{end}},` +
	`{{if .Config.Cmd}}true{{else}}false{{end}},{{json .Config.WorkingDir}},` +
	`{{with .Config.Labels}}` +
	`{{json (index . "` + labelProject + `")}},{{json (index . "` + labelService + `")}},` +
	`{{json (index . "` + labelWorkingDir + `")}},{{json (index . "` + labelConfigFiles + `")}},` +
	`{{json (index . "` + labelFastVersion + `")}},{{json (index . "` + labelFastMode + `")}},` +
	`{{json (index . "` + labelFastDLL + `")}},{{json (index . "` + labelFastWorkDir + `")}},{{json (index . "` + labelFastProject + `")}}` +
	`{{else}}null,null,null,null,null,null,null,null,null{{end}}]`

// fastInspectFields is how many values fastInspectTemplate emits.
const fastInspectFields = 19

// FastInfo is what fast mode reads of a container. Every string in it
// passed its grammar (control characters, length, shape); what is empty
// failed or was absent.
type FastInfo struct {
	// ID is the full container id, Name its name without the leading slash,
	// Image its image's id, Platform its os ("linux").
	ID, Name, Image, Platform string
	// Running, Paused and Restarting are the container's state flags.
	Running, Paused, Restarting bool
	// DLL is the assembly of an entrypoint of exactly the shape ["dotnet",
	// "X.dll"] (a leading "./" dropped, X.dll per [ValidDLL]); "" for any
	// other entrypoint. Docker never sends the entrypoint's other shapes.
	DLL string
	// HasCmd is whether the container's command (a compose command: or the
	// image's CMD) is non-empty: its value is never read.
	HasCmd bool
	// WorkDir is the container's working directory when [ValidAppDir]
	// accepts it, else "" with WorkDirErr saying why.
	WorkDir    string
	WorkDirErr error
	// Project and Service are the compose labels, ComposeDir the compose
	// project's working directory on the host (absolute), ConfigFiles the
	// compose files it was created from (each absolute, in order).
	Project, Service, ComposeDir string
	ConfigFiles                  []string
	// Fast is eyedbg's fast-mode labels; nil when the container has none.
	Fast *FastLabels
}

// FastLabels are eyedbg's own labels on a fast-mode container.
type FastLabels struct {
	// Version is the label format; only [FastVersion]'s others are read:
	// for another, only Version and Override are set.
	Version string
	// Override is the absolute path of the override file the container was
	// created with.
	Override string
	// DLL and WorkDir are what a launch runs: the assembly and the directory
	// it is mounted at, which is also the working directory.
	DLL, WorkDir string
	// Project is the project file's path below the compose directory,
	// "/"-separated, and ProjectPath its absolute resolved path. Both are ""
	// when the file is gone (the caller searches again) or can't be checked.
	Project, ProjectPath string
}

// InspectFast reads container ref (an id or a name) with one docker inspect
// of [fastInspectTemplate]. A missing container is INVALID_REQUEST, other
// docker failures ATTACH_FAILED, a label that fails its grammar (eyedbg's
// are untrusted) INVALID_REQUEST naming it.
func (e Engine) InspectFast(ctx context.Context, ref string) (FastInfo, error) {
	out, err := e.inspectOne(ctx, ref, fastInspectTemplate)
	if err != nil {
		return FastInfo{}, err
	}

	return parseFastInspect(out)
}

// inspectOne runs one docker inspect of container ref with template and
// returns its stdout.
func (e Engine) inspectOne(ctx context.Context, ref, template string) ([]byte, error) {
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}

	res, fail := e.run(ctx, QueryTimeout, nil, e.Args("inspect", "--type", "container", "--format", template, "--", ref))
	if fail != nil {
		if strings.Contains(strings.ToLower(fail.stderr), "no such") {
			return nil, api.NewError(api.CodeInvalidRequest, "no container "+show(ref, 80), "list containers with 'docker ps'")
		}

		return nil, dockerError(fail)
	}

	return res.stdout, nil
}

// rawFast is fastInspectTemplate's values, decoded.
type rawFast struct {
	id, name, image, platform           string
	running, paused, restarting, hasCmd bool
	dll, workDir                        *string
	project, service, composeDir        *string
	configFiles                         *string
	fastVersion, fastOverride           *string
	fastDLL, fastWorkDir, fastProject   *string
}

func (r *rawFast) targets() []any {
	return []any{
		&r.id, &r.name, &r.image, &r.platform, &r.running, &r.paused, &r.restarting, &r.dll, &r.hasCmd, &r.workDir,
		&r.project, &r.service, &r.composeDir, &r.configFiles,
		&r.fastVersion, &r.fastOverride, &r.fastDLL, &r.fastWorkDir, &r.fastProject,
	}
}

// parseFastInspect decodes fastInspectTemplate's output, failing closed on
// any value of the wrong type or shape.
func parseFastInspect(out []byte) (FastInfo, error) {
	bad := func(err error) (FastInfo, error) {
		return FastInfo{}, api.NewError(api.CodeAttachFailed, "unexpected answer from docker inspect: "+err.Error(),
			"eyedbg reads a fixed set of fields; this docker may differ from the ones it was verified with (docker 24 to 26)")
	}

	var fields []json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		return bad(errors.New("not a JSON array"))
	}

	if len(fields) != fastInspectFields {
		return bad(fmt.Errorf("%d values, want %d", len(fields), fastInspectFields))
	}

	var raw rawFast

	for n, d := range raw.targets() {
		if err := json.Unmarshal(fields[n], d); err != nil {
			return bad(fmt.Errorf("value %d: %w", n+1, err))
		}
	}

	info := FastInfo{
		ID: raw.id, Name: strings.TrimPrefix(raw.name, "/"), Image: raw.image, Platform: raw.platform,
		Running: raw.running, Paused: raw.paused, Restarting: raw.restarting, HasCmd: raw.hasCmd,
	}

	if !isFullID(info.ID) || !validImageID(info.Image) || ValidateRef(info.Name) != nil || !grammar(info.Platform, isLowerDigit, isLowerDigit) {
		return bad(errors.New("the container's id, name, image or platform is not in docker's usual form"))
	}

	if err := info.readEntrypoint(&raw); err != nil {
		return FastInfo{}, err
	}

	if err := info.readCompose(&raw); err != nil {
		return FastInfo{}, err
	}

	if err := info.readFast(&raw); err != nil {
		return FastInfo{}, err
	}

	return info, nil
}

// readEntrypoint sets DLL and WorkDir. A DLL that fails its grammar, or a
// working directory that fails [ValidAppDir], is not an error: that service
// can't use fast mode, which its caller says.
func (i *FastInfo) readEntrypoint(raw *rawFast) error {
	if raw.dll != nil {
		if hasControl(*raw.dll) {
			return labelError("entrypoint", *raw.dll)
		}

		if dll, ok := normalizeDLL(*raw.dll); ok {
			i.DLL = dll
		}
	}

	wd := ""
	if raw.workDir != nil {
		wd = *raw.workDir
	}

	if hasControl(wd) {
		return labelError("working directory", wd)
	}

	if err := ValidAppDir(wd); err != nil {
		i.WorkDirErr = err
	} else {
		i.WorkDir = wd
	}

	return nil
}

// readCompose sets the compose labels. They reach argv and paths, so one
// that fails its grammar is an error.
func (i *FastInfo) readCompose(raw *rawFast) error {
	if p := str(raw.project); p != "" {
		if hasControl(p) || api.CheckGroup(p) != nil {
			return labelError(labelProject, p)
		}

		i.Project = p
	}

	if s := str(raw.service); s != "" {
		if hasControl(s) || ValidateService(s) != nil {
			return labelError(labelService, s)
		}

		i.Service = s
	}

	if d := str(raw.composeDir); d != "" {
		if !plainAbsPath(d) {
			return labelError(labelWorkingDir, d)
		}

		i.ComposeDir = d
	}

	files, err := splitConfigFiles(str(raw.configFiles))
	if err != nil {
		return err
	}

	i.ConfigFiles = files

	return nil
}

// splitConfigFiles splits compose's config_files label: absolute paths
// joined by ','. A path holding a comma splits into pieces that aren't all
// absolute, which fails.
func splitConfigFiles(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}

	files := strings.Split(v, ",")
	if len(files) > maxFastConfigFiles {
		return nil, labelError(labelConfigFiles, v)
	}

	for _, f := range files {
		if !plainAbsPath(f) {
			return nil, labelError(labelConfigFiles, v)
		}
	}

	return files, nil
}

// readFast sets Fast from eyedbg's labels. Any of them without the
// override label is incomplete; with version 1, every label but project is
// required and checked.
func (i *FastInfo) readFast(raw *rawFast) error {
	version, override := str(raw.fastVersion), str(raw.fastOverride)
	dll, wd, project := str(raw.fastDLL), str(raw.fastWorkDir), str(raw.fastProject)

	if version == "" && override == "" && dll == "" && wd == "" && project == "" {
		return nil
	}

	if err := checkFastBasics(version, override, dll, wd, project); err != nil {
		return err
	}

	fl := &FastLabels{Version: version, Override: override}
	i.Fast = fl

	if version != FastVersion {
		return nil
	}

	if n, ok := normalizeDLL(dll); !ok || n != dll {
		return labelError(labelFastDLL, dll)
	}

	if ValidAppDir(wd) != nil {
		return labelError(labelFastWorkDir, wd)
	}

	fl.DLL, fl.WorkDir = dll, wd

	return i.readFastProject(fl, project)
}

// checkFastBasics checks what every version of eyedbg's labels must hold:
// plain, short values, the override an absolute path, the version digits.
func checkFastBasics(version, override, dll, wd, project string) error {
	for _, l := range [][2]string{
		{labelFastVersion, version}, {labelFastMode, override}, {labelFastDLL, dll}, {labelFastWorkDir, wd}, {labelFastProject, project},
	} {
		if hasControl(l[1]) || len(l[1]) > maxLabel {
			return labelError(l[0], l[1])
		}
	}

	if override == "" || !plainAbsPath(override) {
		return labelError(labelFastMode, override)
	}

	if !grammar(version, isDigit, isDigit) || len(version) > 3 {
		return labelError(labelFastVersion, version)
	}

	return nil
}

// readFastProject sets the project labels of fl: project must be a valid
// relative project path, and resolve inside the compose directory; a file
// that is gone leaves them empty.
func (i *FastInfo) readFastProject(fl *FastLabels, project string) error {
	if project == "" {
		return nil
	}

	if !ValidProjectRel(project) {
		return labelError(labelFastProject, project)
	}

	if i.ComposeDir == "" {
		return nil
	}

	abs, rel, err := ResolveProject(i.ComposeDir, project)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil || rel != project:
		return labelError(labelFastProject, project)
	}

	fl.Project, fl.ProjectPath = rel, abs

	return nil
}

// str is *s, "" for nil.
func str(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// labelError is the error for a label (or entrypoint, or working directory)
// whose value fails its grammar. The value is untrusted: shown cut and
// without control characters.
func labelError(what, value string) error {
	return api.NewError(api.CodeInvalidRequest, "the container's "+what+" is not valid: "+show(value, 80),
		"eyedbg reads its own fast-mode labels as untrusted input; recreate the container with 'docker compose up -d --force-recreate SERVICE' or 'eyedbg compose restore SERVICE'")
}

// hasControl reports whether s holds a control character or isn't UTF-8.
func hasControl(s string) bool {
	return !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl)
}

// plainAbsPath reports whether p is an absolute host path of plain text
// within the bound for a label.
func plainAbsPath(p string) bool {
	return p != "" && len(p) <= maxLabel && !hasControl(p) && filepath.IsAbs(p)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// ValidDLL reports whether name is an assembly file name fast mode runs:
// letters, digits, '_', '.' and '-' (at most 255 bytes), ending in ".dll"
// with something before it. It holds no separator, so it can't leave the
// directory it is joined to.
func ValidDLL(name string) bool {
	base, ok := strings.CutSuffix(name, ".dll")

	return ok && base != "" && len(name) <= maxDLLName && grammar(base, isNameChar, isNameChar)
}

// normalizeDLL is name without one leading "./", and whether the result is
// a [ValidDLL].
func normalizeDLL(name string) (string, bool) {
	name = strings.TrimPrefix(name, "./")

	return name, ValidDLL(name)
}

// reservedTopDir reports whether s is a system directory of a container,
// which an app directory's first segment may not be: a mount would hide it.
func reservedTopDir(s string) bool {
	switch s {
	case "bin", "boot", "dev", "etc", "lib", "lib32", "lib64", "libx32", "proc", "run", "sbin", "sys", "tmp", "usr", "var":
		return true
	default:
		return false
	}
}

// ValidAppDir checks a container's working directory, where the host-built
// app is mounted: absolute, as path.Clean writes it, at most 255 bytes,
// one to four segments of letters, digits, '_', '.' and '-' (neither "." nor
// ".."), the first not a system directory ([reservedTopDir]) and not one
// of eyedbg's own (".eyedbg-"). The error names the directory and why.
func ValidAppDir(dir string) error {
	refuse := func(why string) error {
		return api.NewError(api.CodeInvalidRequest, "the container's working directory "+show(dir, 80)+" can't hold the build: "+why,
			"fast mode mounts the build at the working directory: one to four plain segments such as /app, not a system directory")
	}

	switch {
	case dir == "":
		return refuse("it has none")
	case dir == "/":
		return refuse("it is the root directory")
	case len(dir) > maxAppDirLen || !utf8.ValidString(dir):
		return refuse("it is too long or not text")
	case !strings.HasPrefix(dir, "/") || path.Clean(dir) != dir:
		return refuse("it is not an absolute, clean path")
	}

	segments := strings.Split(dir[1:], "/")
	if len(segments) > maxAppDirSegments {
		return refuse(fmt.Sprintf("it is more than %d levels deep", maxAppDirSegments))
	}

	for _, s := range segments {
		if s == "" || s == "." || s == ".." || !grammar(s, isNameChar, isNameChar) {
			return refuse("a segment is not letters, digits, '_', '.' or '-'")
		}
	}

	if reservedTopDir(segments[0]) || strings.HasPrefix(segments[0], ".eyedbg-") {
		return refuse(show(segments[0], 40) + " is a system directory or eyedbg's own")
	}

	return nil
}

// ProjectExts are the project file extensions eyedbg builds.
func ProjectExts() []string { return []string{".csproj", ".fsproj", ".vbproj"} }

// IsProjectFile reports whether name ends in a project file extension, with
// something before it.
func IsProjectFile(name string) bool {
	return slices.ContainsFunc(ProjectExts(), func(ext string) bool { return len(name) > len(ext) && strings.HasSuffix(name, ext) })
}

// ValidProjectRel checks a project file's path below the compose
// directory: "/"-separated and as path.Clean writes it, relative, no ".."
// or empty segment, no backslash, plain text, at most 1024 bytes, ending in
// a project file extension.
func ValidProjectRel(rel string) bool {
	if rel == "" || len(rel) > maxProjectRel || hasControl(rel) || strings.ContainsRune(rel, '\\') ||
		strings.HasPrefix(rel, "/") || path.Clean(rel) != rel || !IsProjectFile(path.Base(rel)) {
		return false
	}

	return filepath.IsLocal(filepath.FromSlash(rel))
}

// ErrOutsideRoot is [ResolveProject]'s error for a path that leads out of
// the root.
var ErrOutsideRoot = errors.New("outside the compose directory")

// ResolveProject resolves the project file p, which is absolute or relative
// to root, to the real file: every symlink followed, then the result must lie
// inside realpath(root) (compared exactly, case included), be a regular
// file and have a project file extension. It returns the resolved absolute
// path and its path below the resolved root ("/"-separated, a
// [ValidProjectRel]). A path that doesn't exist wraps [fs.ErrNotExist]; one
// that leaves root wraps [ErrOutsideRoot].
func ResolveProject(root, p string) (abs, rel string, err error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", root, err)
	}

	if !filepath.IsAbs(p) {
		p = filepath.Join(realRoot, filepath.FromSlash(p))
	}

	abs, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", p, err)
	}

	if !within(realRoot, abs) {
		return "", "", fmt.Errorf("%s: %w", abs, ErrOutsideRoot)
	}

	info, err := os.Lstat(abs)
	if err != nil {
		return "", "", fmt.Errorf("stat %s: %w", abs, err)
	}

	if !info.Mode().IsRegular() || !IsProjectFile(info.Name()) {
		return "", "", fmt.Errorf("%s is not a project file: %w", abs, ErrOutsideRoot)
	}

	r, err := filepath.Rel(realRoot, abs)
	if err != nil {
		return "", "", fmt.Errorf("relate %s to %s: %w", abs, realRoot, err)
	}

	rel = filepath.ToSlash(r)
	if !ValidProjectRel(rel) {
		return "", "", fmt.Errorf("%s: not a path eyedbg records: %w", rel, ErrOutsideRoot)
	}

	return abs, rel, nil
}

// within reports whether p is inside root (both resolved): root's exact
// text, then a separator. A case difference is a different file on a
// case-sensitive directory, so it is refused.
func within(root, p string) bool {
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}

	return strings.HasPrefix(p, prefix)
}

// FastContainer is a container, in any state, of a compose project that
// carries eyedbg's fast-mode override label.
type FastContainer struct {
	// ID is the full container id, Service its compose service.
	ID, Service string
}

// FastContainers lists the containers of compose project (a name), in any
// state, that carry eyedbg's fast-mode override label, with one docker ps -a:
// full id and service, sorted by service then id. Every line must be a full id
// and a service name: anything else fails, so the caller never acts on a
// partial answer.
func (e Engine) FastContainers(ctx context.Context, project string) ([]FastContainer, error) {
	if err := api.CheckGroup(project); err != nil {
		return nil, err
	}

	argv := e.Args("ps", "--all", "--no-trunc",
		"--filter=label="+labelProject+"="+project, "--filter=label="+labelFastMode,
		`--format={{.ID}} {{.Label "`+labelService+`"}}`)

	res, fail := e.run(ctx, QueryTimeout, nil, argv)
	if fail != nil {
		return nil, dockerError(fail)
	}

	out := strings.TrimRight(string(res.stdout), "\r\n")
	if out == "" {
		return nil, nil
	}

	var found []FastContainer

	for line := range strings.SplitSeq(out, "\n") {
		id, service, ok := strings.Cut(strings.TrimSuffix(line, "\r"), " ")
		if !ok || !isFullID(id) || ValidateService(service) != nil {
			return nil, api.NewError(api.CodeAttachFailed, "unexpected answer from docker ps: "+show(line, 100),
				"eyedbg reads a fixed set of fields; this docker may differ from the ones it was verified with (docker 24 to 26)")
		}

		found = append(found, FastContainer{ID: id, Service: service})
	}

	slices.SortFunc(found, func(a, b FastContainer) int {
		return cmp.Or(strings.Compare(a.Service, b.Service), strings.Compare(a.ID, b.ID))
	})

	return found, nil
}
