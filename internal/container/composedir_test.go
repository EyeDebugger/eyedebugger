// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/container"
)

// realDir is a new directory with its symlinks resolved (a macOS temp
// directory is behind one).
func realDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

func writeFile(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(b)
}

// TestCorroborateComposeDir: a project directory label is believed only with
// a compose file of the container's files label inside it. Both labels may
// come from an image (docker run copies its LABELs onto the container), so a
// directory and a file that merely exist are not enough: the file must look
// like compose's, be a regular file, and be inside, after symlinks.
func TestCorroborateComposeDir(t *testing.T) {
	t.Parallel()

	root := realDir(t)
	proj := filepath.Join(root, "proj")
	home := filepath.Join(root, "home")

	writeFile(t, filepath.Join(proj, "compose.yaml"))
	writeFile(t, filepath.Join(proj, "docker", "stack.YML"))
	writeFile(t, filepath.Join(proj, "notes.txt"))
	writeFile(t, filepath.Join(home, ".bashrc"))
	writeFile(t, filepath.Join(home, "id_rsa"))
	writeFile(t, filepath.Join(root, "outside.yaml"))

	if err := os.MkdirAll(filepath.Join(proj, "dir.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Where links can't be made, the file isn't there: refused either way.
	_ = os.Symlink(filepath.Join(root, "outside.yaml"), filepath.Join(proj, "link.yaml"))

	tests := []struct {
		name  string
		dir   string
		files []string
		ok    bool
	}{
		{"compose's own files", proj, []string{filepath.Join(proj, "compose.yaml")}, true},
		{"one of several", proj, []string{filepath.Join(root, "other.yaml"), filepath.Join(proj, "compose.yaml")}, true},
		{"a file in a subdirectory, in capitals", proj, []string{filepath.Join(proj, "docker", "stack.YML")}, true},
		{"the directory by a trailing separator", proj + string(filepath.Separator), []string{filepath.Join(proj, "compose.yaml")}, true},

		// The hostile image: a LABEL names a directory (the user's home, say).
		{"a directory with no matching file", home, []string{filepath.Join(proj, "compose.yaml")}, false},
		{"a file there that isn't a compose file (.bashrc)", home, []string{filepath.Join(home, ".bashrc")}, false},
		{"a file there that isn't a compose file (id_rsa)", home, []string{filepath.Join(home, "id_rsa")}, false},
		{"a file there that isn't a compose file (.txt)", proj, []string{filepath.Join(proj, "notes.txt")}, false},
		{"a file that is not there", proj, []string{filepath.Join(proj, "gone.yaml")}, false},
		{"a directory named like a compose file", proj, []string{filepath.Join(proj, "dir.yaml")}, false},
		{"a file outside the directory", proj, []string{filepath.Join(root, "outside.yaml")}, false},
		{"an extension and nothing else", proj, []string{filepath.Join(proj, ".yaml")}, false},
		{"no files", proj, nil, false},
		{"no directory", "", []string{filepath.Join(proj, "compose.yaml")}, false},
		{"a relative directory", "proj", []string{filepath.Join(proj, "compose.yaml")}, false},
		{"a directory that is not there", filepath.Join(root, "nowhere"), []string{filepath.Join(proj, "compose.yaml")}, false},
		{"the file system root", filepath.VolumeName(root) + string(filepath.Separator), []string{filepath.Join(root, "outside.yaml")}, false},
		{"a file that is a link out of the directory", proj, []string{filepath.Join(proj, "link.yaml")}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := container.CorroborateComposeDir(tt.dir, tt.files)
			if (err == nil) != tt.ok {
				t.Fatalf("CorroborateComposeDir(%q, %q) = %q, %v; want success %v", tt.dir, tt.files, got, err, tt.ok)
			}

			if err == nil && got != proj {
				t.Errorf("dir = %q, want %q", got, proj)
			}
		})
	}
}

// A directory given through a symlink is returned resolved, so the path map
// and the build see the real one.
func TestCorroborateComposeDirResolvesLinks(t *testing.T) {
	t.Parallel()

	root := realDir(t)
	proj := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(proj, "compose.yaml"))

	link := filepath.Join(root, "link")
	if err := os.Symlink(proj, link); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	got, err := container.CorroborateComposeDir(link, []string{filepath.Join(link, "compose.yaml")})
	if err != nil || got != proj {
		t.Errorf("got %q, %v; want %q", got, err, proj)
	}
}
