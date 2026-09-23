// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// SchemaVersion is the manifest schema this eyedbg reads.
const SchemaVersion = 1

// Adapter runtimes (Adapter.Runtime).
const (
	// RuntimeNative is an executable run directly.
	RuntimeNative = ""
	// RuntimePython is a Python package run on the user's interpreter.
	RuntimePython = "python"
)

// Option types (Option.Type).
const (
	OptionString = "string"
	OptionBool   = "bool"
	OptionInt    = "int"
)

// Manifest sources (Manifest.Source).
const (
	SourceBundled = "bundled"
	SourceUser    = "user"
)

// Manifest describes one debug adapter: how to find, install and run it,
// and, for a language served by the generic driver, how to launch programs
// with it (docs/adapter-manifests.md).
type Manifest struct {
	Schema      int    `json:"schema"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Homepage    string `json:"homepage,omitempty"`
	License     string `json:"license,omitempty"`
	// Version is the release 'eyedbg adapters install' fetches.
	Version  string       `json:"version"`
	Adapter  Adapter      `json:"adapter"`
	Python   *Python      `json:"python,omitempty"`
	Install  *InstallSpec `json:"install,omitempty"`
	Language *Language    `json:"language,omitempty"`
	// Options are the language's --opt options, by name.
	Options map[string]Option `json:"options,omitempty"`
	Launch  *Template         `json:"launch,omitempty"`
	Attach  *Template         `json:"attach,omitempty"`
	// AttachUnsupported is the hint when attaching isn't supported.
	AttachUnsupported string `json:"attachUnsupported,omitempty"`
	// Exceptions maps the exception modes "all" and "uncaught" to the
	// adapter's exception filter ids.
	Exceptions map[string][]string `json:"exceptions,omitempty"`
	EvalGuard  *EvalGuard          `json:"evalGuard,omitempty"`

	// Source is SourceBundled or SourceUser; set by the loader.
	Source string `json:"-"`
	// Path is the file a user manifest was read from.
	Path string `json:"-"`
	// Replaces names the bundled manifest a user manifest replaces.
	Replaces string `json:"-"`
}

// Adapter is how the adapter process runs.
type Adapter struct {
	// ID is the DAP adapterID sent in initialize.
	ID string `json:"id"`
	// Transport is how eyedbg talks to the adapter: "stdio" (or empty).
	Transport string `json:"transport,omitempty"`
	// Runtime is RuntimeNative or RuntimePython.
	Runtime string `json:"runtime,omitempty"`
	// Entry is, for a native adapter, an executable name (".exe" is added
	// on Windows) or an absolute path; for a Python one, a slash path
	// relative to the package root.
	Entry string   `json:"entry"`
	Args  []string `json:"args,omitempty"`
	// Environment is added to the adapter's environment.
	Environment map[string]string `json:"environment,omitempty"`
	// Env names an environment variable pointing at the executable
	// (native only).
	Env string `json:"env,omitempty"`
	// Path also looks Entry up on PATH (native only).
	Path bool `json:"path,omitempty"`
	// VersionArgs make the executable print its version (doctor).
	VersionArgs []string `json:"versionArgs,omitempty"`
	// NotRunningHint is doctor's fix when the executable doesn't run.
	NotRunningHint string `json:"notRunningHint,omitempty"`
}

// Python is how a Python-hosted adapter finds its interpreter and package.
type Python struct {
	// Env names an environment variable holding the interpreter.
	Env string `json:"env,omitempty"`
	// Option names the --opt option holding the interpreter.
	Option string `json:"option,omitempty"`
	// Commands are the interpreters tried on PATH, in order, each a
	// command name and its arguments.
	Commands [][]string `json:"commands"`
	// Venvs are project virtual-environment directory names.
	Venvs []string `json:"venvs,omitempty"`
	// MinVersion ("X.Y") is the oldest Python the downloaded package runs
	// on.
	MinVersion string `json:"minVersion,omitempty"`
	// Module is the import name of the package holding Adapter.Entry.
	Module string `json:"module"`
}

// InstallSpec lists the downloadable builds, by "os/arch" or "*".
type InstallSpec struct {
	Downloads map[string]Download `json:"downloads"`
}

// Download is one build of the adapter.
type Download struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	// Archive is "zip" or "tar.gz".
	Archive string `json:"archive"`
	// Root is the archive's directory that becomes the install directory;
	// empty means the whole archive.
	Root string `json:"root"`
	// Size is the download's size in bytes; 0 means unknown (capped).
	Size int64 `json:"size,omitempty"`
}

// Language is the language a manifest's adapter debugs.
type Language struct {
	Name string `json:"name"`
	// Builtin means a compiled Go driver serves the language; the
	// manifest only carries adapter metadata.
	Builtin    bool     `json:"builtin,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Markers    []string `json:"markers,omitempty"`
}

// Option is a language option set with start --opt NAME=VALUE.
type Option struct {
	// Type is OptionString (or empty), OptionBool or OptionInt.
	Type string `json:"type,omitempty"`
	// Default is the value when not set, as it would be typed.
	Default string `json:"default,omitempty"`
	Help    string `json:"help"`
}

// Template is the body of a launch or attach request, with ${...}
// references (docs/adapter-manifests.md).
type Template struct {
	// Require lists inputs ("program", "opt.NAME") of which at least one
	// must be set.
	Require   []string       `json:"require,omitempty"`
	Arguments map[string]any `json:"arguments"`
}

// EvalGuard is data for the generic check that keeps eval from changing
// the program (drivers/generic).
type EvalGuard struct {
	Quotes               []string `json:"quotes,omitempty"`
	InterpolatedPrefixes []string `json:"interpolatedPrefixes,omitempty"`
	NonCallWords         []string `json:"nonCallWords,omitempty"`
	SafeCalls            []string `json:"safeCalls,omitempty"`
	AssignOps            []string `json:"assignOps,omitempty"`
}

// Kind returns the option's type, OptionString when unset.
func (o Option) Kind() string {
	if o.Type == "" {
		return OptionString
	}

	return o.Type
}

// Parse converts v to the option's type: string, bool or int.
func (o Option) Parse(v string) (any, error) {
	switch o.Kind() {
	case OptionBool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%q is not a bool (true or false)", v)
		}

		return b, nil
	case OptionInt:
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%q is not an integer", v)
		}

		return n, nil
	default:
		return v, nil
	}
}

// LanguageName is the language the manifest serves, "" for none.
func (m *Manifest) LanguageName() string {
	if m.Language == nil {
		return ""
	}

	return m.Language.Name
}

// Builtin reports whether a Go driver serves the manifest's language.
func (m *Manifest) Builtin() bool {
	return m.Language != nil && m.Language.Builtin
}

// DownloadFor returns the build for goos/goarch, else the "*" one.
func (m *Manifest) DownloadFor(goos, goarch string) (Download, bool) {
	if m.Install == nil {
		return Download{}, false
	}

	if d, ok := m.Install.Downloads[goos+"/"+goarch]; ok {
		return d, true
	}

	d, ok := m.Install.Downloads["*"]

	return d, ok
}

// Parse reads and validates one manifest. Unknown fields are errors, so a
// manifest written for a newer schema fails clearly.
func Parse(data []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// Numbers in templates stay as written.
	dec.UseNumber()

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse: data after the manifest object")
	}

	if err := m.validate(); err != nil {
		return nil, err
	}

	return &m, nil
}
