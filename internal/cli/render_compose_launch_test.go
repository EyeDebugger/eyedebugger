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

// launchedSession is a running launched container session of service in group
// myapp.
func launchedSession(id, service string) *api.SessionInfo {
	s := memberSession(id, service, 0, 18*time.Second)
	s.State, s.Stop, s.StoppedAt = api.StateRunning, nil, nil
	s.Container.Launched = true
	s.Container.Map = []api.PathMapping{{Remote: "/src", Local: "/work/app"}}

	return &s
}

const (
	renderOverride = "/home/me/.eyedbg/compose/myapp/override.yml"
	renderBuildLog = "/home/me/.eyedbg/compose/myapp/build.log"
)

func TestComposeLaunchRendering(t *testing.T) {
	t.Parallel()

	bp := &api.Breakpoint{ID: 1, Owner: "agent", File: "/work/app/Producer/Program.cs", RequestedLine: 6, Line: 6, Verified: true}
	pending := &api.Breakpoint{ID: 1, Owner: "agent", File: "/work/app/Producer/Program.cs", RequestedLine: 6, Line: 6, Message: "module not loaded"}
	boom := api.NewError(api.CodeBuildFailed, "dotnet publish failed: see the build log", "error CS1002 in Program.cs(12)")
	lease := api.NewError(api.CodeLeaseHeld, "the lease is held by human:bob", "ask for it")

	entered := []launchedMember{
		{
			attachedMember: attachedMember{Service: "producer", Container: "myapp-producer-1", Session: launchedSession("s-k3f9", "producer"), Breakpoints: []bpOutcome{{Location: "Producer/Program.cs:6", Breakpoint: bp}}},
			Project:        "Producer/Producer.csproj",
			Fast:           &launchFast{Project: "Producer/Producer.csproj", BuildLog: renderBuildLog, Override: renderOverride, Recreated: true, DurationMs: 2140},
		},
		{
			attachedMember: attachedMember{Service: "consumer", Container: "myapp-consumer-1", Session: launchedSession("s-m2n4", "consumer"), Breakpoints: []bpOutcome{{Location: "Producer/Program.cs:6", Breakpoint: pending}}},
			Project:        "Consumer/Consumer.csproj",
			Fast:           &launchFast{Project: "Consumer/Consumer.csproj", BuildLog: renderBuildLog, Override: renderOverride, Recreated: true, DurationMs: 2140},
		},
		{attachedMember: attachedMember{Service: "db", Container: "myapp-db-1", Skipped: "its entrypoint isn't exec-form 'dotnet X.dll'"}},
	}

	rebuilt := []launchedMember{
		{
			attachedMember: attachedMember{Service: "producer", Container: "myapp-producer-1", Session: launchedSession("s-q8x1", "producer")},
			Project:        "Producer/Producer.csproj",
			Fast:           &launchFast{Project: "Producer/Producer.csproj", BuildLog: renderBuildLog, Override: renderOverride, DurationMs: 3500, Carried: 2, Dropped: 1},
		},
		{
			attachedMember: attachedMember{Service: "consumer", Container: "myapp-consumer-1", Session: launchedSession("s-r7y2", "consumer")},
			Fast:           &launchFast{Project: "Consumer/Consumer.csproj", Override: renderOverride, Carried: 1},
		},
	}

	failed := []launchedMember{
		{attachedMember: attachedMember{Service: "producer", Container: "myapp-producer-1", Error: boom}, Project: "Producer/Producer.csproj", buildFailed: true},
		// Chosen and built, then failed at the lease: not the project's fault.
		{attachedMember: attachedMember{Service: "consumer", Container: "myapp-consumer-1", Error: lease}, Project: "Consumer/Consumer.csproj"},
	}

	partial := []launchedMember{
		{attachedMember: attachedMember{Service: "producer", Container: "myapp-producer-1", Error: boom}},
		rebuilt[1],
	}

	tests := []struct {
		name, golden string
		members      []launchedMember
		view         launchView
		asJSON       bool
	}{
		{"entered", "compose_launch.golden", entered, launchView{Override: renderOverride, BuildLog: renderBuildLog, Recreated: true, Launched: []string{"producer", "consumer"}}, false},
		{"entered, json", "compose_launch_json.golden", entered, launchView{}, true},
		{"rebuilt", "compose_launch_rebuild.golden", rebuilt, launchView{Override: renderOverride, BuildLog: renderBuildLog, Launched: []string{"producer", "consumer"}}, false},
		{"failed", "compose_launch_failed.golden", failed, launchView{Override: renderOverride}, false},
		{"one failed after its app was stopped", "compose_launch_down.golden", partial, launchView{Override: renderOverride, BuildLog: renderBuildLog, Launched: []string{"consumer"}, Down: []string{"producer"}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := writeComposeLaunch(&b, "myapp", tt.members, tt.view, tt.asJSON, renderBase); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}

func TestComposeRestoreRendering(t *testing.T) {
	t.Parallel()

	boom := api.NewError(api.CodeAttachFailed, "docker compose up failed: container myapp-producer-1 is unhealthy", "see 'docker compose ps'")

	members := []restoredMember{
		{Service: "producer", Container: "myapp-producer-1", Restored: true, Sessions: []string{"s-k3f9"}},
		{Service: "consumer", Container: "myapp-consumer-1", Restored: true},
		{Service: "db", Skipped: "not in fast mode"},
		{Service: "api", Container: "myapp-api-1", Error: boom},
	}

	kept := restoreOutcome{Kept: "/home/me/.eyedbg/compose/myapp", Engine: "docker context desk"}

	tests := []struct {
		name, golden string
		members      []restoredMember
		out          restoreOutcome
		asJSON       bool
	}{
		{"mixed", "compose_restore.golden", members, restoreOutcome{}, false},
		{"mixed, json", "compose_restore_json.golden", members, restoreOutcome{}, true},
		{"everything restored", "compose_restore_removed.golden", members[:2], restoreOutcome{Removed: true}, false},
		{"nothing restored", "compose_restore_none.golden", members[2:], restoreOutcome{}, false},
		{"nothing in fast mode: the files are kept", "compose_restore_empty.golden", nil, kept, false},
		{"nothing in fast mode, json", "compose_restore_kept_json.golden", nil, kept, true},
		{"all skipped: the files are kept", "compose_restore_kept.golden", members[2:3], kept, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := writeComposeRestore(&b, "myapp", tt.members, tt.out, tt.asJSON); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), b.Bytes())
		})
	}
}
