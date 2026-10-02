// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// containerSession is the stopped session of a service in a compose
// project: no host pid, frames in host paths.
func containerSession() api.SessionInfo {
	s := stoppedSnapshot().Session
	stoppedAt := renderTime.Add(-45 * time.Second)

	s.PID, s.Group = 0, "myapp"
	s.Program = "web (compose myapp, container web-1, pid 1)"
	s.StoppedAt = &stoppedAt
	s.Container = &api.ContainerInfo{
		ID: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Name: "web-1", Service: "web", Project: "myapp",
		PID: 1, Platform: "linux/arm64", Map: []api.PathMapping{{Remote: "/src", Local: "/work/app"}}, UnhealthyAfter: api.Duration(18_000_000_000),
	}

	return s
}

func TestContainerRendering(t *testing.T) {
	t.Parallel()

	snap := stoppedSnapshot()
	snap.Session = containerSession()

	bare := containerSession()
	bare.Group, bare.Container.Service, bare.Container.Project = "", "", ""
	bare.Program = "container web-1 (pid 7)"
	bare.State, bare.Stop, bare.StoppedAt = api.StateRunning, nil, nil

	ended := containerSession()
	ended.Mode, ended.State, ended.EndReason = api.ModeAttach, api.StateExited, "detached by agent (the program keeps running)"
	ended.Stop, ended.StoppedAt = nil, nil

	hostGroup := stoppedSnapshot().Session
	hostGroup.Group = "myapp"

	tests := []struct {
		name, golden string
		render       func(*bytes.Buffer) error
	}{
		{"snapshot", "snapshot_container.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, snap, false, renderBase) }},
		{"snapshot json", "snapshot_container_json.golden", func(b *bytes.Buffer) error { return writeSnapshot(b, snap, true, renderBase) }},
		{"sessions", "sessions_container.golden", func(b *bytes.Buffer) error {
			return writeSessions(b, []api.SessionInfo{stoppedSnapshot().Session, containerSession(), bare}, false)
		}},
		{"running without compose names", "snapshot_container_bare.golden", func(b *bytes.Buffer) error {
			return writeSnapshot(b, api.Snapshot{Session: bare}, false, renderBase)
		}},
		{"detach", "detach_container.golden", func(b *bytes.Buffer) error { return writeEnded(b, ended) }},
		{"host process in a group", "snapshot_group.golden", func(b *bytes.Buffer) error {
			return writeSnapshot(b, api.Snapshot{Session: hostGroup}, false, renderBase)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := tt.render(&b); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}
