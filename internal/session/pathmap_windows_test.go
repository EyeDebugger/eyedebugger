// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package session

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// On a Windows host, backslashes, colons (drives, alternate data streams)
// and reserved device names in a container path never map, and host paths
// compare in any case.
func TestPathMapWindows(t *testing.T) {
	t.Parallel()

	root := mkdirs(t, realTempDir(t), "proj")
	proj := filepath.Join(root, "proj")
	m := mustPathMap(t, api.PathMapping{Remote: "/src", Local: proj})

	locals := []struct {
		name, remote, want string // want "" for unmapped
	}{
		{"file", "/src/a/b.cs", filepath.Join(proj, "a", "b.cs")},
		{"backslash dot-dot", `/src/a\..\..\x`, ""},
		{"backslash", `/src/a\b.cs`, ""},
		{"drive-relative", "/src/C:x", ""},
		{"absolute drive", "/src/C:/Windows/win.ini", ""},
		{"stream", "/src/a.cs:secret", ""},
		{"device", "/src/CON", ""},
		{"device in a directory", "/src/a/nul", ""},
		{"UNC-like", "/src//server/share", filepath.Join(proj, "server", "share")},
	}

	for _, tt := range locals {
		t.Run("ToLocal "+tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := m.ToLocal(tt.remote)
			if ok != (tt.want != "") || got != tt.want {
				t.Errorf("ToLocal(%q) = %q, %v; want %q", tt.remote, got, ok, tt.want)
			}
		})
	}

	remotes := []struct {
		name, host, want string
	}{
		{"file", filepath.Join(proj, "a", "b.cs"), "/src/a/b.cs"},
		{"lower-case path", strings.ToLower(filepath.Join(proj, "b.cs")), "/src/b.cs"},
		{"upper-case path", strings.ToUpper(filepath.Join(proj, "c.cs")), "/src/C.CS"},
		{"another volume", `Q:\proj\a.cs`, ""},
		{"UNC", `\\server\share\proj\a.cs`, ""},
		{"drive-relative", "C:a.cs", ""},
	}

	for _, tt := range remotes {
		t.Run("ToRemote "+tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := m.ToRemote(tt.host)
			if ok != (tt.want != "") || got != tt.want {
				t.Errorf("ToRemote(%q) = %q, %v; want %q", tt.host, got, ok, tt.want)
			}
		})
	}
}

// A host directory given twice in different cases is one directory:
// ambiguous, refused.
func TestPathMapWindowsDuplicateCase(t *testing.T) {
	t.Parallel()

	root := mkdirs(t, realTempDir(t), "proj")
	proj := filepath.Join(root, "proj")

	tests := []struct {
		name  string
		other string
	}{
		{"lower case", strings.ToLower(proj)},
		{"upper case", strings.ToUpper(proj)},
		{"lower-case drive", strings.ToLower(proj[:1]) + proj[1:]},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewPathMap([]api.PathMapping{{Remote: "/src", Local: proj}, {Remote: "/app", Local: tt.other}})
			if api.CodeOf(err) != api.CodeInvalidRequest {
				t.Errorf("NewPathMap with %q twice = %v, want INVALID_REQUEST", tt.other, err)
			}
		})
	}
}
