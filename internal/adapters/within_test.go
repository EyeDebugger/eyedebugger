// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"path/filepath"
	"testing"
)

func TestWithinDir(t *testing.T) {
	t.Parallel()

	home := filepath.Join(string(filepath.Separator), "home", "me")

	tests := []struct {
		name string
		root string
		p    string
		fold bool
		want bool
	}{
		{"root itself", home, home, false, true},
		{"inside", home, filepath.Join(home, "work", "app"), false, true},
		{"parent", home, filepath.Dir(home), false, false},
		{"sibling with common prefix", home, home + "2", false, false},
		{"filesystem root", home, string(filepath.Separator), false, false},
		{"dot-dot name inside", home, filepath.Join(home, "..x"), false, true},
		{"case differs, folded", home, filepath.Join(filepath.Join(string(filepath.Separator), "HOME", "Me"), "w"), true, true},
		{"case differs, not folded", home, filepath.Join(string(filepath.Separator), "HOME", "Me"), false, false},
		{"empty root", "", home, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := withinDir(tt.root, tt.p, tt.fold); got != tt.want {
				t.Errorf("withinDir(%q, %q) = %v, want %v", tt.root, tt.p, got, tt.want)
			}
		})
	}
}
