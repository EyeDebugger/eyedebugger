// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// EnvConfigDir overrides eyedbg's configuration directory; user manifests
// are in its adapters/ subdirectory.
const EnvConfigDir = "EYEDBG_CONFIG_DIR"

// maxManifestSize is the largest manifest file read.
const maxManifestSize = 1 << 20

// Problem is a manifest that was not loaded, and why.
type Problem struct {
	// Path is the file (or directory) at fault; bundled manifests are
	// "bundled:NAME.json".
	Path string
	Err  error
}

// LoadConfig says where manifests come from.
type LoadConfig struct {
	// UserDir holds user manifests ("" for none).
	UserDir string
	// Builtin are the languages compiled Go drivers serve: only a
	// "builtin" manifest may name them, and a "builtin" manifest must.
	Builtin []string
	// Bundled holds the built-in manifests (*.json at its root; nil for
	// none).
	Bundled fs.FS
}

// Registry is the loaded set of manifests: at most one per adapter name
// and one per language.
type Registry struct {
	adapters []*Manifest
	problems []Problem
}

// UserDir is where user manifests live: $EYEDBG_CONFIG_DIR/adapters, else
// <user config dir>/eyedbg/adapters.
func UserDir() (string, error) {
	if dir := os.Getenv(EnvConfigDir); dir != "" {
		return filepath.Join(dir, "adapters"), nil
	}

	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate the manifest directory (set %s): %w", EnvConfigDir, err)
	}

	return filepath.Join(cfg, "eyedbg", "adapters"), nil
}

// Default loads the bundled manifests and the user's, for the languages
// builtin Go drivers serve.
func Default(builtin ...string) *Registry {
	dir, err := UserDir()
	reg := Load(LoadConfig{UserDir: dir, Builtin: builtin, Bundled: Bundled()})

	if err != nil {
		reg.problems = append(reg.problems, Problem{Err: err})
	}

	return reg
}

// Load reads the manifests cfg names. It never fails: a manifest that
// can't be used is left out and reported by Problems. A user manifest
// replaces the bundled one of the same name or language.
func Load(cfg LoadConfig) *Registry {
	reg := &Registry{}
	bundled := reg.admit(cfg, reg.readBundled(cfg.Bundled))
	user := reg.admit(cfg, reg.readUser(cfg))
	user = reg.dropConflicts(user)

	for _, u := range user {
		var replaced []string

		bundled = slices.DeleteFunc(bundled, func(b *Manifest) bool {
			if b.Name == u.Name || (b.LanguageName() != "" && b.LanguageName() == u.LanguageName()) {
				replaced = append(replaced, b.Name)

				return true
			}

			return false
		})

		u.Replaces = strings.Join(replaced, ", ")
	}

	reg.adapters = slices.Concat(bundled, user)
	sort.Slice(reg.adapters, func(i, j int) bool { return reg.adapters[i].Name < reg.adapters[j].Name })
	sort.SliceStable(reg.problems, func(i, j int) bool { return reg.problems[i].Path < reg.problems[j].Path })

	return reg
}

// Adapters returns every loaded manifest, by name.
func (r *Registry) Adapters() []*Manifest { return slices.Clone(r.adapters) }

// Problems returns the manifests that were left out, by path.
func (r *Registry) Problems() []Problem { return slices.Clone(r.problems) }

// Adapter returns the manifest named name, or nil.
func (r *Registry) Adapter(name string) *Manifest {
	for _, m := range r.adapters {
		if m.Name == name {
			return m
		}
	}

	return nil
}

// Language returns the manifest serving language lang, or nil.
func (r *Registry) Language(lang string) *Manifest {
	for _, m := range r.adapters {
		if lang != "" && m.LanguageName() == lang {
			return m
		}
	}

	return nil
}

// Resolve returns the manifest named s, else the one serving language s,
// or nil.
func (r *Registry) Resolve(s string) *Manifest {
	if m := r.Adapter(s); m != nil {
		return m
	}

	return r.Language(s)
}

// readBundled parses the bundled manifests.
func (r *Registry) readBundled(fsys fs.FS) []*Manifest {
	if fsys == nil {
		return nil
	}

	names, err := fs.Glob(fsys, "*.json")
	if err != nil {
		r.problems = append(r.problems, Problem{Path: "bundled", Err: err})

		return nil
	}

	var out []*Manifest

	for _, name := range names {
		data, err := fs.ReadFile(fsys, name)
		if err == nil {
			var m *Manifest
			if m, err = Parse(data); err == nil {
				m.Source = SourceBundled
				out = append(out, m)

				continue
			}
		}

		r.problems = append(r.problems, Problem{Path: "bundled:" + name, Err: err})
	}

	return out
}

// readUser parses the trusted *.json files directly in cfg.UserDir.
func (r *Registry) readUser(cfg LoadConfig) []*Manifest {
	if cfg.UserDir == "" {
		return nil
	}

	entries, err := os.ReadDir(cfg.UserDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err == nil {
		err = trustDir(cfg)
	}

	if err != nil {
		r.problems = append(r.problems, Problem{Path: cfg.UserDir, Err: err})

		return nil
	}

	var out []*Manifest

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}

		p := filepath.Join(cfg.UserDir, e.Name())

		m, err := readUserFile(p)

		switch {
		case errors.Is(err, errNotAFile):
		case err != nil:
			r.problems = append(r.problems, Problem{Path: p, Err: err})
		default:
			out = append(out, m)
		}
	}

	return out
}

// trustDir checks the manifest directory and its parent.
func trustDir(cfg LoadConfig) error {
	if err := checkTrust(cfg.UserDir); err != nil {
		return err
	}

	return checkTrust(filepath.Dir(cfg.UserDir))
}

// errNotAFile marks a *.json entry that is not a regular file (skipped).
var errNotAFile = errors.New("not a regular file")

// readUserFile reads, checks and parses one user manifest. The
// permission check is on the opened file, so what is read is what was
// checked, even through a symlink.
func readUserFile(p string) (*Manifest, error) {
	// Only regular files are opened (a FIFO named *.json would block).
	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	if err := manifestFile(info); err != nil {
		return nil, err
	}

	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	defer f.Close()

	if info, err = f.Stat(); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	if err := manifestFile(info); err != nil {
		return nil, err
	}

	if err := checkInfo(p, info); err != nil {
		return nil, err
	}

	data, err := io.ReadAll(io.LimitReader(f, maxManifestSize+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	if len(data) > maxManifestSize {
		return nil, fmt.Errorf("is larger than %d bytes", maxManifestSize)
	}

	m, err := Parse(data)
	if err != nil {
		return nil, err
	}

	m.Source, m.Path = SourceUser, p

	return m, nil
}

// manifestFile checks that info is a regular file of at most
// maxManifestSize bytes (errNotAFile: skip it).
func manifestFile(info fs.FileInfo) error {
	switch {
	case !info.Mode().IsRegular():
		return errNotAFile
	case info.Size() > maxManifestSize:
		return fmt.Errorf("is larger than %d bytes", maxManifestSize)
	default:
		return nil
	}
}

// admit keeps the manifests whose language fits the Go drivers: a
// builtin manifest needs a Go driver of its language, and only it may
// name a Go driver's language.
func (r *Registry) admit(cfg LoadConfig, ms []*Manifest) []*Manifest {
	return slices.DeleteFunc(ms, func(m *Manifest) bool {
		lang := m.LanguageName()
		goDriver := slices.Contains(cfg.Builtin, lang)

		var err error

		switch {
		case m.Builtin() && !goDriver:
			err = fmt.Errorf("language %q is marked builtin, but no built-in driver serves it", lang)
		case lang != "" && !m.Builtin() && goDriver:
			err = fmt.Errorf("language %q has a built-in driver: its manifest must set language.builtin", lang)
		}

		if err != nil {
			r.problems = append(r.problems, Problem{Path: manifestPath(m), Err: err})
		}

		return err != nil
	})
}

// dropConflicts leaves out every user manifest that shares its name or
// language with another one: which to trust is not eyedbg's call.
func (r *Registry) dropConflicts(ms []*Manifest) []*Manifest {
	names, langs := map[string]int{}, map[string]int{}

	for _, m := range ms {
		names[m.Name]++

		if l := m.LanguageName(); l != "" {
			langs[l]++
		}
	}

	return slices.DeleteFunc(ms, func(m *Manifest) bool {
		var err error

		switch {
		case names[m.Name] > 1:
			err = fmt.Errorf("another user manifest is also named %q; both are ignored", m.Name)
		case m.LanguageName() != "" && langs[m.LanguageName()] > 1:
			err = fmt.Errorf("another user manifest also serves language %q; both are ignored", m.LanguageName())
		}

		if err != nil {
			r.problems = append(r.problems, Problem{Path: manifestPath(m), Err: err})
		}

		return err != nil
	})
}

// manifestPath names where m came from, for a Problem.
func manifestPath(m *Manifest) string {
	if m.Path != "" {
		return m.Path
	}

	return "bundled:" + m.Name + ".json"
}
