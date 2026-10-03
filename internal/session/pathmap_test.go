// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// realTempDir is t.TempDir with its symlinks resolved (macOS: /var is a
// symlink into /private), as a map stores its host directories.
func realTempDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// mkdirs creates dirs (relative to root, slash-separated) and returns
// root.
func mkdirs(t *testing.T, root string, dirs ...string) string {
	t.Helper()

	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func mustPathMap(t *testing.T, pairs ...api.PathMapping) *PathMap {
	t.Helper()

	m, err := NewPathMap(pairs)
	if err != nil {
		t.Fatalf("NewPathMap(%+v): %v", pairs, err)
	}

	return m
}

func TestNewPathMap(t *testing.T) {
	t.Parallel()

	root := mkdirs(t, realTempDir(t), "a", "b", "a/inner")

	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "a"), link); err != nil {
		link = "" // no symlinks (Windows without the privilege)
	}

	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	many := make([]api.PathMapping, maxPathMappings+1)

	for i := range many {
		dir := mkdirs(t, root, "many/"+strings.Repeat("d", i+1))
		many[i] = api.PathMapping{Remote: "/r" + strings.Repeat("d", i+1), Local: filepath.Join(dir, "many", strings.Repeat("d", i+1))}
	}

	tests := []struct {
		name  string
		pairs []api.PathMapping
		ok    bool
	}{
		{"none", nil, true},
		{"one", []api.PathMapping{{Remote: "/src", Local: a}}, true},
		{"root remote", []api.PathMapping{{Remote: "/", Local: a}}, true},
		{"nested roots", []api.PathMapping{{Remote: "/src", Local: a}, {Remote: "/src/lib", Local: b}}, true},
		{"sixteen", many[:maxPathMappings], true},
		{"seventeen", many, false},
		{"relative remote", []api.PathMapping{{Remote: "src", Local: a}}, false},
		{"empty remote", []api.PathMapping{{Remote: "", Local: a}}, false},
		{"unclean remote", []api.PathMapping{{Remote: "/src/", Local: a}}, false},
		{"dot-dot remote", []api.PathMapping{{Remote: "/src/../etc", Local: a}}, false},
		{"double slash remote", []api.PathMapping{{Remote: "//src", Local: a}}, false},
		{"backslash remote", []api.PathMapping{{Remote: `/src\x`, Local: a}}, false},
		{"windows remote", []api.PathMapping{{Remote: `C:\src`, Local: a}}, false},
		{"NUL in remote", []api.PathMapping{{Remote: "/src\x00", Local: a}}, false},
		{"newline in remote", []api.PathMapping{{Remote: "/src\nx", Local: a}}, false},
		{"invalid UTF-8 remote", []api.PathMapping{{Remote: "/src\xff", Local: a}}, false},
		{"long remote", []api.PathMapping{{Remote: "/" + strings.Repeat("x", maxRemotePath), Local: a}}, false},
		{"relative local", []api.PathMapping{{Remote: "/src", Local: "a"}}, false},
		{"missing local", []api.PathMapping{{Remote: "/src", Local: filepath.Join(root, "gone")}}, false},
		{"file local", []api.PathMapping{{Remote: "/src", Local: file}}, false},
		{"duplicate remote", []api.PathMapping{{Remote: "/src", Local: a}, {Remote: "/src", Local: b}}, false},
		{"duplicate local", []api.PathMapping{{Remote: "/src", Local: a}, {Remote: "/app", Local: a}}, false},
		{"duplicate local, unclean", []api.PathMapping{{Remote: "/src", Local: a}, {Remote: "/app", Local: a + string(filepath.Separator) + "."}}, false},
	}

	if link != "" {
		tests = append(tests, struct {
			name  string
			pairs []api.PathMapping
			ok    bool
		}{"duplicate local through a symlink", []api.PathMapping{{Remote: "/src", Local: a}, {Remote: "/app", Local: link}}, false})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, err := NewPathMap(tt.pairs)
			if tt.ok {
				if err != nil || m == nil {
					t.Fatalf("NewPathMap = %v, %v; want a map", m, err)
				}

				return
			}

			if m != nil || api.CodeOf(err) != api.CodeInvalidRequest {
				t.Fatalf("NewPathMap = %v, %v; want INVALID_REQUEST", m, err)
			}
		})
	}
}

// The map stores host directories with symlinks resolved, so breakpoint
// files (resolved too) map through a symlinked directory's real path.
func TestPathMapResolvesLocal(t *testing.T) {
	t.Parallel()

	root := mkdirs(t, realTempDir(t), "real")

	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Join(root, "real"), link); err != nil {
		t.Skipf("symlink: %v", err)
	}

	m := mustPathMap(t, api.PathMapping{Remote: "/src", Local: link})

	if got := m.Mappings(); !slices.Equal(got, []api.PathMapping{{Remote: "/src", Local: filepath.Join(root, "real")}}) {
		t.Errorf("Mappings = %+v", got)
	}

	if got, ok := m.ToRemote(filepath.Join(root, "real", "a.cs")); !ok || got != "/src/a.cs" {
		t.Errorf("ToRemote = %q, %v", got, ok)
	}
}

func TestPathMapToLocal(t *testing.T) {
	t.Parallel()

	root := mkdirs(t, realTempDir(t), "app", "lib", "all")
	app, lib := filepath.Join(root, "app"), filepath.Join(root, "lib")
	m := mustPathMap(t, api.PathMapping{Remote: "/src", Local: app}, api.PathMapping{Remote: "/src/lib", Local: lib})
	all := mustPathMap(t, api.PathMapping{Remote: "/", Local: filepath.Join(root, "all")})
	empty := mustPathMap(t)

	tests := []struct {
		name   string
		m      *PathMap
		remote string
		want   string // "" for unmapped
	}{
		{"file", m, "/src/a/b.cs", filepath.Join(app, "a", "b.cs")},
		{"root itself", m, "/src", app},
		{"root with a slash", m, "/src/", app},
		{"double slash", m, "/src//a.cs", filepath.Join(app, "a.cs")},
		{"dot segment", m, "/src/./a.cs", filepath.Join(app, "a.cs")},
		{"nested root wins", m, "/src/lib/x.cs", filepath.Join(lib, "x.cs")},
		{"nested root itself", m, "/src/lib", lib},
		{"prefix without a boundary", m, "/srcx/a.cs", ""},
		{"nested prefix without a boundary", m, "/src/libx/a.cs", filepath.Join(app, "libx", "a.cs")},
		{"outside", m, "/usr/share/dotnet/x.cs", ""},
		{"dot-dot out", m, "/src/../etc/passwd", ""},
		{"dot-dot inside", m, "/src/a/../b.cs", ""},
		{"dot-dot out of a nested root", m, "/src/lib/../a.cs", ""},
		{"relative", m, "src/a.cs", ""},
		{"empty", m, "", ""},
		{"backslash", m, `/src/a\b.cs`, ""},
		{"backslash dot-dot", m, `/src/a\..\..\x`, ""},
		{"NUL", m, "/src/a\x00.cs", ""},
		{"newline", m, "/src/a\n.cs", ""},
		{"invalid UTF-8", m, "/src/a\xff.cs", ""},
		{"windows path", m, `C:\src\a.cs`, ""},
		{"root remote maps all", all, "/etc/passwd", filepath.Join(root, "all", "etc", "passwd")},
		{"root remote, root", all, "/", filepath.Join(root, "all")},
		{"root remote, dot-dot", all, "/../etc/passwd", ""},
		{"empty map", empty, "/src/a.cs", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tt.m.ToLocal(tt.remote)
			if ok != (tt.want != "") || got != tt.want {
				t.Errorf("ToLocal(%q) = %q, %v; want %q", tt.remote, got, ok, tt.want)
			}
		})
	}
}

func TestPathMapToRemote(t *testing.T) {
	t.Parallel()

	root := mkdirs(t, realTempDir(t), "app", "app/lib", "appx")
	app, lib := filepath.Join(root, "app"), filepath.Join(root, "app", "lib")
	m := mustPathMap(t, api.PathMapping{Remote: "/src", Local: app}, api.PathMapping{Remote: "/lib", Local: lib})

	tests := []struct {
		name string
		host string
		want string // "" for unmapped
	}{
		{"file", filepath.Join(app, "a", "b.cs"), "/src/a/b.cs"},
		{"root itself", app, "/src"},
		{"nested root wins", filepath.Join(lib, "x.cs"), "/lib/x.cs"},
		{"prefix without a boundary", filepath.Join(root, "appx", "a.cs"), ""},
		{"parent", root, ""},
		{"dot-dot out", app + string(filepath.Separator) + filepath.Join("..", "appx", "a.cs"), ""},
		{"relative", filepath.Join("app", "a.cs"), ""},
		{"newline in name", app + string(filepath.Separator) + "a\n.cs", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := m.ToRemote(tt.host)
			if ok != (tt.want != "") || got != tt.want {
				t.Errorf("ToRemote(%q) = %q, %v; want %q", tt.host, got, ok, tt.want)
			}
		})
	}
}

// Readable follows symlinks: a link inside a root to a file outside every
// root is not readable, nor is anything outside, nor a missing file; open
// reads exactly the readable files.
func TestPathMapReadable(t *testing.T) {
	t.Parallel()

	root := mkdirs(t, realTempDir(t), "app/sub", "secret")
	app := filepath.Join(root, "app")
	m := mustPathMap(t, api.PathMapping{Remote: "/src", Local: app})

	write := func(p, text string) string {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}

		return p
	}

	inside := write(filepath.Join(app, "sub", "a.cs"), "inside")
	outside := write(filepath.Join(root, "secret", "key.txt"), "secret")

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"inside", inside, true},
		{"outside", outside, false},
		{"missing", filepath.Join(app, "gone.cs"), false},
		{"dot-dot out", filepath.Join(app, "sub") + string(filepath.Separator) + filepath.Join("..", "..", "secret", "key.txt"), false},
		{"relative", filepath.Join("sub", "a.cs"), false},
		{"inside, not source", filepath.Join(app, "sub", "id_rsa"), false},
		{"inside, source in capitals", filepath.Join(app, "sub", "B.CSHTML"), true},
	}

	write(filepath.Join(app, "sub", "id_rsa"), "inside")
	write(filepath.Join(app, "sub", "B.CSHTML"), "inside")

	links := []struct {
		name, at, to string
		want         bool
	}{
		{"file link out", filepath.Join(app, "out.txt"), outside, false},
		{"dir link out", filepath.Join(app, "outdir"), filepath.Join(root, "secret"), false},
		{"file link in", filepath.Join(app, "in.cs"), inside, true},
	}

	for _, l := range links {
		if err := os.Symlink(l.to, l.at); err != nil {
			t.Logf("symlink: %v (skipping the link cases)", err)

			break
		}

		p := l.at
		if l.name == "dir link out" {
			p = filepath.Join(l.at, "key.txt")
		}

		tests = append(tests, struct {
			name string
			path string
			want bool
		}{l.name, p, l.want})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := m.Readable(tt.path); got != tt.want {
				t.Errorf("Readable(%q) = %v, want %v", tt.path, got, tt.want)
			}

			f, err := m.open(tt.path)
			if (err == nil) != tt.want {
				t.Fatalf("open(%q) = %v, want success %v", tt.path, err, tt.want)
			}

			if err != nil {
				return
			}

			defer f.Close()

			if data, _ := io.ReadAll(f); string(data) != "inside" {
				t.Errorf("open(%q) read %q", tt.path, data)
			}
		})
	}
}
