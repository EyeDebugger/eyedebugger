// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// Manifest field limits and patterns (docs/adapter-manifests.md).
const (
	maxDescription = 120
	sha256Hex      = 64
	archiveZip     = "zip"
	archiveTarGz   = "tar.gz"
	// assignOpChars are the characters an evalGuard assignOps entry may use.
	assignOpChars = "=<>!:+-*/%&|^~@"
)

// Patterns are compiled per call: the package keeps no global state and
// manifests are validated rarely.
const (
	namePattern      = `^[a-z][a-z0-9-]{0,31}$`
	versionPattern   = `^[0-9A-Za-z._+-]{1,40}$`
	envPattern       = `^[A-Z][A-Z0-9_]*$`
	envNamePattern   = `^[A-Za-z_][A-Za-z0-9_]*$`
	optionPattern    = `^[A-Za-z][A-Za-z0-9]*$`
	modulePattern    = `^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`
	minVerPattern    = `^[0-9]+\.[0-9]+$`
	sha256Pattern    = `^[0-9a-f]{64}$`
	extensionPattern = `^\.[A-Za-z0-9]+$`
	wordPattern      = `^[A-Za-z_][A-Za-z0-9_]*$`
)

// matches reports whether s matches pattern (one of the constants above).
func matches(pattern, s string) bool {
	return regexp.MustCompile(pattern).MatchString(s)
}

// fieldError is a validation problem in one field.
func fieldError(field, format string, args ...any) error {
	return fmt.Errorf("%s: %s", field, fmt.Sprintf(format, args...))
}

// validate checks m against schema 1; the error names the first bad field.
func (m *Manifest) validate() error {
	checks := []func() error{
		m.validateTop, m.validateAdapter, m.validatePython, m.validateInstall,
		m.validateLanguage, m.validateOptions, m.validateTemplates, m.validateExceptions,
		m.validateEvalGuard,
	}

	for _, check := range checks {
		if err := check(); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manifest) validateTop() error {
	switch {
	case m.Schema != SchemaVersion:
		return fieldError("schema", "%d is not supported (this eyedbg reads schema %d)", m.Schema, SchemaVersion)
	case !matches(namePattern, m.Name):
		return fieldError("name", "%q must be lowercase letters, digits and '-', starting with a letter (at most 32)", m.Name)
	case m.Description == "":
		return fieldError("description", "is required")
	case utf8.RuneCountInString(m.Description) > maxDescription:
		return fieldError("description", "is longer than %d characters", maxDescription)
	case !matches(versionPattern, m.Version):
		return fieldError("version", "%q must be 1-40 letters, digits and ._+-", m.Version)
	}

	if m.Homepage != "" {
		if err := checkHTTPS(m.Homepage); err != nil {
			return fieldError("homepage", "%v", err)
		}
	}

	return nil
}

// checkHTTPS checks that s is an https URL with a host.
func checkHTTPS(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%q is not a URL: %w", s, err)
	}

	if u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%q must be an https URL", s)
	}

	return nil
}

func (m *Manifest) validateAdapter() error {
	a := m.Adapter

	if err := validateTransport(a); err != nil {
		return err
	}

	switch {
	case a.ID == "":
		return fieldError("adapter.id", "is required")
	case a.Runtime != RuntimeNative && a.Runtime != RuntimePython:
		return fieldError("adapter.runtime", "%q is not a runtime (\"\" for an executable, or python)", a.Runtime)
	case a.Entry == "":
		return fieldError("adapter.entry", "is required")
	}

	for name := range a.Environment {
		if !matches(envNamePattern, name) {
			return fieldError("adapter.environment", "%q is not an environment variable name", name)
		}
	}

	if a.Runtime == RuntimePython {
		return m.validatePythonAdapter()
	}

	if err := checkNativeEntry(a.Entry); err != nil {
		return fieldError("adapter.entry", "%v", err)
	}

	if a.Env != "" && !matches(envPattern, a.Env) {
		return fieldError("adapter.env", "%q must be an upper-case environment variable name", a.Env)
	}

	return nil
}

// validateTransport checks adapter.transport and, since transport decides
// what adapter.args may reference, adapter.args too (checkAdapterArgs).
func validateTransport(a Adapter) error {
	switch {
	case a.Transport != "" && a.Transport != TransportStdio && a.Transport != TransportConnect:
		return fieldError("adapter.transport", "%q must be stdio or connect (\"\" for stdio)", a.Transport)
	case a.Transport == TransportConnect && a.Runtime != RuntimeNative:
		return fieldError("adapter.transport", "connect is only for a native adapter")
	}

	return checkAdapterArgs(a)
}

// checkAdapterArgs checks adapter.args' only allowed reference, ${socket}
// (the connect transport's socket path): required, and the only reference,
// on connect; refused (it needs transport connect) otherwise.
func checkAdapterArgs(a Adapter) error {
	if a.Transport != TransportConnect {
		for _, arg := range a.Args {
			if hasVarRef(arg, VarSocket) {
				return fieldError("adapter.args", "%q needs transport connect", arg)
			}
		}

		return nil
	}

	kinds := map[string]varKind{VarSocket: kindString}
	hasSocket := false

	for _, arg := range a.Args {
		if err := checkString(arg, kinds); err != nil {
			return fieldError("adapter.args", "%v", err)
		}

		if hasVarRef(arg, VarSocket) {
			hasSocket = true
		}
	}

	if !hasSocket {
		return fieldError("adapter.args", "connect needs ${socket} in at least one argument")
	}

	return nil
}

// validatePythonAdapter checks the adapter fields a Python adapter has
// (and those it must not have).
func (m *Manifest) validatePythonAdapter() error {
	a := m.Adapter

	switch {
	case a.Env != "" || a.Path || len(a.VersionArgs) > 0:
		return fieldError("adapter", "env, path and versionArgs are only for native adapters (python has its own)")
	case m.Python == nil:
		return fieldError("python", "is required for a python adapter")
	}

	if err := checkRelative(a.Entry); err != nil {
		return fieldError("adapter.entry", "%v", err)
	}

	return nil
}

// checkNativeEntry allows an executable name or an absolute path.
func checkNativeEntry(entry string) error {
	if filepath.IsAbs(entry) {
		return nil
	}

	if strings.ContainsAny(entry, `/\`) || entry == "." || entry == ".." {
		return fmt.Errorf("%q must be an executable name or an absolute path", entry)
	}

	return nil
}

// checkRelative allows a clean slash path inside a directory.
func checkRelative(p string) error {
	if p == "" || strings.ContainsAny(p, `\:`) || path.IsAbs(p) || path.Clean(p) != p ||
		p == ".." || strings.HasPrefix(p, "../") {
		return fmt.Errorf("%q must be a relative slash path without '..'", p)
	}

	return nil
}

func (m *Manifest) validatePython() error {
	py := m.Python
	if py == nil {
		return nil
	}

	if m.Adapter.Runtime != RuntimePython {
		return fieldError("python", "is only for adapter.runtime python")
	}

	if err := checkCommands(py.Commands); err != nil {
		return fieldError("python.commands", "%v", err)
	}

	switch {
	case py.Env != "" && !matches(envPattern, py.Env):
		return fieldError("python.env", "%q must be an upper-case environment variable name", py.Env)
	case py.MinVersion != "" && !matches(minVerPattern, py.MinVersion):
		return fieldError("python.minVersion", "%q must be X.Y", py.MinVersion)
	case !matches(modulePattern, py.Module):
		return fieldError("python.module", "%q must be a Python import name", py.Module)
	}

	return m.validatePythonLookup()
}

// validatePythonLookup checks the interpreter option and venv names.
func (m *Manifest) validatePythonLookup() error {
	py := m.Python

	if py.Option != "" {
		if o, ok := m.Options[py.Option]; !ok || o.Kind() != OptionString {
			return fieldError("python.option", "%q must name a declared string option", py.Option)
		}
	}

	for _, v := range py.Venvs {
		if strings.ContainsAny(v, `/\`) || v == "" || v == "." || v == ".." {
			return fieldError("python.venvs", "%q must be a directory name", v)
		}
	}

	return nil
}

// checkCommands checks interpreter commands: each a name (or absolute
// path) and its arguments.
func checkCommands(cmds [][]string) error {
	if len(cmds) == 0 {
		return errors.New("at least one command is required")
	}

	for _, c := range cmds {
		if len(c) == 0 {
			return errors.New("a command is empty")
		}

		if err := checkNativeEntry(c[0]); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manifest) validateInstall() error {
	if m.Install == nil {
		return nil
	}

	if len(m.Install.Downloads) == 0 {
		return fieldError("install.downloads", "at least one download is required")
	}

	for key, d := range m.Install.Downloads {
		field := "install.downloads[" + key + "]"

		if !validPlatform(key) {
			return fieldError(field, "the key must be \"*\" or OS/ARCH with OS linux, darwin or windows and ARCH amd64 or arm64")
		}

		if err := checkDownload(d); err != nil {
			return fieldError(field, "%v", err)
		}
	}

	return nil
}

// validPlatform reports whether key is "*" or a supported "os/arch".
func validPlatform(key string) bool {
	if key == "*" {
		return true
	}

	goos, goarch, ok := strings.Cut(key, "/")

	return ok && slices.Contains([]string{"linux", "darwin", goosWindows}, goos) &&
		slices.Contains([]string{"amd64", "arm64"}, goarch)
}

func checkDownload(d Download) error {
	if err := checkHTTPS(d.URL); err != nil {
		return fmt.Errorf("url: %w", err)
	}

	switch {
	case len(d.SHA256) != sha256Hex || !matches(sha256Pattern, d.SHA256):
		return fmt.Errorf("sha256 %q must be 64 lowercase hex digits", d.SHA256)
	case d.Archive != archiveZip && d.Archive != archiveTarGz:
		return fmt.Errorf("archive %q must be zip or tar.gz", d.Archive)
	case d.Size < 0:
		return fmt.Errorf("size %d is negative", d.Size)
	}

	if d.Root != "" {
		if err := checkRelative(d.Root); err != nil {
			return fmt.Errorf("root: %w", err)
		}
	}

	return nil
}

func (m *Manifest) validateLanguage() error {
	l := m.Language
	generic := m.Launch != nil || m.Attach != nil || len(m.Options) > 0 || m.Exceptions != nil || m.EvalGuard != nil

	switch {
	case l == nil && generic:
		return fieldError("language", "is required with options, launch, attach, exceptions or evalGuard")
	case l == nil:
		return nil
	case !matches(namePattern, l.Name):
		return fieldError("language.name", "%q must be lowercase letters, digits and '-', starting with a letter", l.Name)
	case l.Builtin && generic:
		return fieldError("language.builtin", "a built-in language's driver is Go code: drop options, launch, attach, exceptions and evalGuard")
	case !l.Builtin && m.Launch == nil:
		return fieldError("launch", "is required for a language without a built-in driver")
	}

	return validateDetection(l)
}

// validateDetection checks the language's file extensions and markers.
func validateDetection(l *Language) error {
	for _, e := range l.Extensions {
		if !matches(extensionPattern, e) {
			return fieldError("language.extensions", "%q must be like \".py\"", e)
		}
	}

	for _, g := range l.Markers {
		if _, err := path.Match(g, ""); err != nil || g == "" || strings.ContainsAny(g, `/\`) {
			return fieldError("language.markers", "%q must be a file name or glob", g)
		}
	}

	return nil
}

func (m *Manifest) validateOptions() error {
	for name, o := range m.Options {
		field := "options." + name

		switch {
		case !matches(optionPattern, name):
			return fieldError("options", "%q must be letters and digits, starting with a letter", name)
		case o.Kind() != OptionString && o.Kind() != OptionBool && o.Kind() != OptionInt:
			return fieldError(field+".type", "%q must be string, bool or int", o.Type)
		case o.Help == "":
			return fieldError(field+".help", "is required")
		}

		if o.Default != "" {
			if _, err := o.Parse(o.Default); err != nil {
				return fieldError(field+".default", "%v", err)
			}
		}
	}

	return nil
}

func (m *Manifest) validateTemplates() error {
	if m.Launch != nil {
		if err := m.validateTemplate("launch", m.Launch, false); err != nil {
			return err
		}

		if len(m.Launch.Require) == 0 {
			return fieldError("launch.require", "name at least one input (program or opt.NAME)")
		}
	}

	if m.Attach != nil {
		if len(m.Attach.Require) > 0 {
			return fieldError("attach.require", "is only for launch")
		}

		return m.validateTemplate("attach", m.Attach, true)
	}

	return nil
}

func (m *Manifest) validateTemplate(field string, t *Template, attach bool) error {
	if len(t.Arguments) == 0 {
		return fieldError(field+".arguments", "is required")
	}

	kinds := m.varKinds(attach)

	for _, r := range t.Require {
		if r != varProgram && (!strings.HasPrefix(r, optPrefix) || kinds[r] == kindNone) {
			return fieldError(field+".require", "%q must be program or a declared opt.NAME", r)
		}
	}

	if err := checkTemplate(t.Arguments, kinds); err != nil {
		return fieldError(field+".arguments", "%v", err)
	}

	return nil
}

func (m *Manifest) validateExceptions() error {
	for mode, filters := range m.Exceptions {
		if mode != "all" && mode != "uncaught" {
			return fieldError("exceptions", "%q must be all or uncaught (none sets no filters)", mode)
		}

		if len(filters) == 0 || slices.Contains(filters, "") {
			return fieldError("exceptions."+mode, "must list at least one filter id")
		}
	}

	return nil
}

func (m *Manifest) validateEvalGuard() error {
	g := m.EvalGuard
	if g == nil {
		return nil
	}

	for _, q := range g.Quotes {
		if utf8.RuneCountInString(q) != 1 {
			return fieldError("evalGuard.quotes", "%q must be one character", q)
		}
	}

	words := map[string][]string{
		"interpolatedPrefixes": g.InterpolatedPrefixes, "nonCallWords": g.NonCallWords, "safeCalls": g.SafeCalls,
	}
	for field, list := range words {
		for _, w := range list {
			if !matches(wordPattern, w) {
				return fieldError("evalGuard."+field, "%q must be a word", w)
			}
		}
	}

	for _, op := range g.AssignOps {
		if op == "" || strings.Trim(op, assignOpChars) != "" {
			return fieldError("evalGuard.assignOps", "%q must be made of %s", op, assignOpChars)
		}
	}

	return nil
}
