// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Parallel()

	vcs := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123abcd"},
			{Key: "vcs.time", Value: "2026-09-01T10:00:00Z"},
		},
	}

	tests := []struct {
		name                        string
		ldVersion, ldCommit, ldDate string
		bi                          *debug.BuildInfo
		want                        Info
	}{
		{
			name:      "link-time values win",
			ldVersion: "v1.2.3", ldCommit: "feedface", ldDate: "2026-09-02T00:00:00Z",
			bi:   vcs,
			want: Info{Version: "v1.2.3", Commit: "feedface", Date: "2026-09-02T00:00:00Z"},
		},
		{
			name: "local checkout build uses vcs settings",
			bi:   vcs,
			want: Info{Version: "dev", Commit: "0123abcd", Date: "2026-09-01T10:00:00Z"},
		},
		{
			name: "go install module@version uses module version",
			bi:   &debug.BuildInfo{Main: debug.Module{Version: "v0.3.0"}},
			want: Info{Version: "v0.3.0", Commit: "unknown", Date: "unknown"},
		},
		{
			name: "no build info",
			want: Info{Version: "dev", Commit: "unknown", Date: "unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := resolve(tt.ldVersion, tt.ldCommit, tt.ldDate, tt.bi)
			if got != tt.want {
				t.Errorf("resolve() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
