// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestCheckGroup(t *testing.T) {
	t.Parallel()

	for _, g := range []string{"a", "my-app", "app_2", "0day", strings.Repeat("a", 63)} {
		if err := api.CheckGroup(g); err != nil {
			t.Errorf("CheckGroup(%q) = %v, want ok", g, err)
		}
	}

	bad := []string{"", "-x", "_x", "My", "a b", "a.b", "a/b", "a\nb", "a\x00", "ü", strings.Repeat("a", 64), "--privileged"}
	for _, g := range bad {
		if api.CodeOf(api.CheckGroup(g)) != api.CodeInvalidRequest {
			t.Errorf("CheckGroup(%q) accepted", g)
		}
	}
}

// TestContainerWireShapes pins the JSON a client and a daemon exchange: an
// empty engine and zero values are omitted, so an older reader sees only
// what it knows.
func TestContainerWireShapes(t *testing.T) {
	t.Parallel()

	spec, err := json.Marshal(api.ContainerSpec{Ref: "web-1"})
	if err != nil || string(spec) != `{"ref":"web-1"}` {
		t.Errorf("ContainerSpec = %s, %v", spec, err)
	}

	info, err := json.Marshal(api.SessionInfo{ID: "s-1"})
	if err != nil || strings.Contains(string(info), "container") || strings.Contains(string(info), "group") || strings.Contains(string(info), "stoppedAt") {
		t.Errorf("SessionInfo without a container = %s, %v", info, err)
	}

	att, err := json.Marshal(api.AttachSpec{PID: 3})
	if err != nil || string(att) != `{"pid":3}` {
		t.Errorf("AttachSpec = %s, %v", att, err)
	}
}

// TestContainerLaunchWireShapes pins the container launch's additions the same
// way: zero values are omitted.
func TestContainerLaunchWireShapes(t *testing.T) {
	t.Parallel()

	launch, err := json.Marshal(api.ContainerLaunchSpec{Ref: "web-1", StopOnEntry: true})
	if err != nil || string(launch) != `{"ref":"web-1","stopOnEntry":true}` {
		t.Errorf("ContainerLaunchSpec = %s, %v", launch, err)
	}

	plain, err := json.Marshal(api.ContainerInfo{ID: "i", Name: "n", PID: 1})
	if err != nil || strings.Contains(string(plain), "launched") {
		t.Errorf("an attached ContainerInfo = %s, %v: launched must be omitted", plain, err)
	}

	launched, err := json.Marshal(api.ContainerInfo{ID: "i", Name: "n", Launched: true})
	if err != nil || !strings.Contains(string(launched), `"launched":true`) {
		t.Errorf("a launched ContainerInfo = %s, %v", launched, err)
	}

	start, err := json.Marshal(api.StartParams{Lang: "dotnet"})
	if err != nil || strings.Contains(string(start), "containerLaunch") {
		t.Errorf("StartParams without a container launch = %s, %v", start, err)
	}
}
