// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// seenSpecs records the container specs a containerFake was asked to attach
// to.
type seenSpecs struct {
	mu    sync.Mutex
	specs []api.ContainerSpec
}

func (s *seenSpecs) add(spec api.ContainerSpec) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.specs = append(s.specs, spec)
}

func (s *seenSpecs) last() api.ContainerSpec {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.specs) == 0 {
		return api.ContainerSpec{}
	}

	return s.specs[len(s.specs)-1]
}

// containerFake attaches "containers" on the fake adapter, whose program is
// /src/prog.txt there. Ref "broken" fails.
type containerFake struct {
	fakeDriver

	seen *seenSpecs
}

func (d containerFake) PrepareContainerAttach(_ context.Context, spec api.AttachSpec) (session.Launch, error) {
	cs := spec.Container
	d.seen.add(*cs)

	if cs.Ref == "broken" {
		return session.Launch{}, api.NewError(api.CodeAttachFailed, "container broken is not running", "start it first")
	}

	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	pm, err := session.NewPathMap(cs.Map)
	if err != nil {
		return session.Launch{}, err
	}

	pid := max(cs.PID, 1)

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Request: session.RequestAttach, PID: pid,
		Arguments: daptest.ProgramArgs{Program: "/src/prog.txt", Lines: 10, Hang: true}.Map(), Program: "web (container " + cs.Ref + ")", PathMap: pm,
		Container: &api.ContainerInfo{
			ID: strings.Repeat("a", 64), Name: cs.Ref, Service: "web", PID: pid, Platform: "linux/arm64", Map: pm.Mappings(),
		},
	}, nil
}

// TestAttachContainerEndToEnd drives 'attach --container' through the real
// command and an in-process daemon. Not parallel: it sets environment
// variables.
func TestAttachContainerEndToEnd(t *testing.T) {
	seen := &seenSpecs{}
	serveWithDrivers(t, isolate(t), containerFake{seen: seen})

	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	prog := filepath.Join(local, "prog.txt")
	if err := os.WriteFile(prog, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Chdir(local)
	t.Setenv(envDockerHost, "tcp://engine.example:2376")
	t.Setenv(envDockerContext, "ctx1")

	out := run(t, 0, "attach", "fake", "--container", "web-1", "--map", "/src=.", "--group", "myapp", "--bp", prog+":3", "--timeout", "20s")
	expectOutput(t, out, "(fake; service web, container web-1, group myapp) stopped: breakpoint", "prog.txt:3")

	got := seen.last()
	if got.Ref != "web-1" || got.Engine.Host != "tcp://engine.example:2376" || got.Engine.Context != "ctx1" ||
		len(got.Map) != 1 || got.Map[0].Remote != "/src" || got.Map[0].Local != local || got.PID != 0 {
		t.Errorf("the daemon was asked to attach to %+v", got)
	}

	var snap struct {
		Session struct {
			PID       int    `json:"pid"`
			Group     string `json:"group"`
			Container struct {
				Name string `json:"name"`
				PID  int    `json:"pid"`
			} `json:"container"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(run(t, 0, "status", "--json")), &snap); err != nil {
		t.Fatal(err)
	}

	if snap.Session.PID != 0 || snap.Session.Group != "myapp" || snap.Session.Container.Name != "web-1" || snap.Session.Container.PID != 1 {
		t.Errorf("status --json session = %+v", snap.Session)
	}

	expectOutput(t, run(t, 0, "sessions"), "web (container web-1)")
	expectOutput(t, run(t, 0, "detach"), "detached; container web-1 keeps running")
}

func TestAttachContainerOptions(t *testing.T) {
	seen := &seenSpecs{}
	serveWithDrivers(t, isolate(t), containerFake{seen: seen})

	t.Setenv(envDockerHost, "")
	t.Setenv(envDockerContext, "")

	expectOutput(t, run(t, 0, "attach", "fake", "--container", "web-1", "--pid", "7"), "container web-1")

	if got := seen.last(); got.PID != 7 || got.Engine.Host != "" || got.Engine.Context != "" {
		t.Errorf("spec = %+v, want pid 7 and the default engine", got)
	}

	expectOutput(t, run(t, 0, "sessions"), "web (container web-1)")
	expectOutput(t, run(t, 0, "stop"), "detached; container web-1 keeps running")

	// What goes wrong, with its class.
	expectOutput(t, run(t, exitEnvironment, "attach", "fake", "--container", "broken"), "[ATTACH_FAILED]", "container broken is not running")
	expectOutput(t, run(t, exitError, "attach", "fake", "--container", "--privileged"), "[INVALID_REQUEST]", "invalid container")
	expectOutput(t, run(t, exitError, "attach", "fake", "--container", "web-1", "--pid", "0"), "--pid is a process id from 1")
	expectOutput(t, run(t, exitError, "attach", "fake", "--map", "/src=.", "--pid", "3"), "--map and --group go with --container")
	expectOutput(t, run(t, exitError, "attach", "fake", "--group", "g", "--pid", "3"), "--map and --group go with --container")
	expectOutput(t, run(t, exitError, "attach", "fake", "--container", "web-1", "--map", "nonsense"), `invalid --map "nonsense"`)
	expectOutput(t, run(t, exitError, "attach", "fake", "--container", "web-1", "--map", "/src="), `invalid --map "/src="`)
	expectOutput(t, run(t, exitError, "attach", "fake", "--container", "web-1", "--group", "Not Valid"), "[INVALID_REQUEST]", "invalid group name")
	expectOutput(t, run(t, exitError, "attach", "fake", "--container", "web-1", "--map", "src=/tmp"), "[INVALID_REQUEST]", "invalid path map")

	if n := seen.last(); n.Ref == "--privileged" {
		t.Error("the driver ran for an invalid container reference")
	}
}

func TestParseMaps(t *testing.T) {
	t.Parallel()

	tmp := filepath.Clean(os.TempDir())

	got, err := parseMaps([]string{"/src=" + tmp, "/app=a=b"})
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 || got[0] != (api.PathMapping{Remote: "/src", Local: tmp}) || got[1].Remote != "/app" || !strings.HasSuffix(got[1].Local, "a=b") || !filepath.IsAbs(got[1].Local) {
		t.Errorf("parseMaps = %+v: REMOTE is up to the first '=', LOCAL is made absolute", got)
	}

	for _, bad := range []string{"", "x", "=y", "/src=", "="} {
		if _, err := parseMaps([]string{bad}); err == nil {
			t.Errorf("parseMaps(%q) accepted", bad)
		}
	}

	if none, err := parseMaps(nil); err != nil || none != nil {
		t.Errorf("parseMaps(nil) = %v, %v", none, err)
	}
}

func TestContainerCallError(t *testing.T) {
	t.Parallel()

	err := containerCallError(api.NewError(api.CodeUnknownMethod, "unknown method container.attach", ""))
	if api.CodeOf(err) != api.CodeVersionMismatch || !strings.Contains(err.Error(), "too old") {
		t.Errorf("err = %v, want VERSION_MISMATCH", err)
	}

	other := api.NewError(api.CodeAttachFailed, "x", "")
	if got := containerCallError(other); !errors.Is(got, other) {
		t.Errorf("another error changed: %v", got)
	}
}

func TestSoleMember(t *testing.T) {
	t.Parallel()

	snap := api.Snapshot{Session: api.SessionInfo{ID: "s-1"}}
	boom := api.NewError(api.CodeAttachFailed, "no", "")

	if got, err := soleMember(api.ContainerAttachResult{Members: []api.ContainerMemberResult{{Session: &snap}}}); err != nil || got.Session.ID != "s-1" {
		t.Errorf("attached: %+v, %v", got, err)
	}

	if _, err := soleMember(api.ContainerAttachResult{Members: []api.ContainerMemberResult{{Error: boom}}}); !errors.Is(err, boom) {
		t.Errorf("failed: %v, want the member's error", err)
	}

	if _, err := soleMember(api.ContainerAttachResult{Members: []api.ContainerMemberResult{{Container: "web-1", Skipped: "pid 1 runs nginx"}}}); api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("skipped: %v, want INVALID_REQUEST", err)
	}

	for _, n := range []int{0, 2} {
		res := api.ContainerAttachResult{Members: make([]api.ContainerMemberResult, n)}
		if _, err := soleMember(res); api.CodeOf(err) != api.CodeInternal {
			t.Errorf("%d members: %v, want INTERNAL", n, err)
		}
	}
}

// TestDotnetRefusesContainerSessions: 'eyedbg dotnet' inspects host processes
// only, so a container session is refused (its pid isn't one), and 'dotnet
// ps' never maps a container session to a host pid.
func TestDotnetRefusesContainerSessions(t *testing.T) {
	t.Parallel()

	snap := api.Snapshot{Session: containerSession()}
	snap.Session.State, snap.Session.Stop = api.StateRunning, nil

	_, err := resolveSessionTarget(t.Context(), snap, fakeLookup(testProcs), false)
	requireCode(t, err, api.CodeInvalidRequest, "session s-k3f9 debugs a program in container web-1; 'eyedbg dotnet' inspects host processes only")

	rows := psRows(t.Context(), []int{11}, fakeLookup(testProcs), []api.SessionInfo{snap.Session})
	if len(rows) != 1 || rows[0].Session != "" {
		t.Errorf("psRows = %+v, want pid 11 with no session", rows)
	}
}
