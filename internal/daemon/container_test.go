// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// containerFake attaches to "containers" on the fake adapter: the program is
// /src/prog.txt in the container. Ref "broken" fails to attach, ref "nginx"
// is not a .NET container.
type containerFake struct{ fakeDriver }

func (containerFake) PrepareContainerAttach(_ context.Context, spec api.AttachSpec) (session.Launch, error) {
	cs := spec.Container

	switch cs.Ref {
	case "broken":
		return session.Launch{}, api.NewError(api.CodeAttachFailed, "container broken is not running", "start it")
	case "nginx":
		return session.Launch{}, errors.Join(api.NewError(api.CodeNotDotnet, "pid 1 runs nginx", ""), session.ErrNotCandidate)
	}

	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	pm, err := session.NewPathMap(cs.Map)
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Request: session.RequestAttach, PID: max(cs.PID, 1),
		Arguments: daptest.ProgramArgs{Program: "/src/prog.txt", Lines: 5, Hang: true}.Map(),
		Program:   "container " + cs.Ref, PathMap: pm,
		Container: &api.ContainerInfo{ID: containerID(cs.Ref), Name: cs.Ref, PID: max(cs.PID, 1), Platform: "linux/arm64", Map: pm.Mappings()},
	}, nil
}

// containerID is a full id derived from ref.
func containerID(ref string) string {
	return strings.Repeat(strconv.FormatInt(int64(len(ref))%16, 16), 64)
}

func startContainerServer(t *testing.T) Paths {
	t.Helper()

	return startServerWith(t, PathsIn(shortTempDir(t)), time.Hour, containerFake{}).paths
}

func TestContainerAttachMembers(t *testing.T) {
	t.Parallel()

	p := startContainerServer(t)

	var res api.ContainerAttachResult

	mustCall(t, p, api.MethodContainerAttach, api.ContainerAttachParams{
		Lang: "fake", Group: "app",
		Members: []api.ContainerSpec{
			{Ref: "web-1", Service: "web", RequireDotnet: true},
			{Ref: "broken", Service: "api"},
			{Ref: "nginx", Service: "proxy", RequireDotnet: true},
			{Ref: "nginx", Service: "named"}, // named: not skipped, the same error
			{Ref: "worker-2", Service: "worker", PID: 9},
		},
	}, &res)

	if len(res.Members) != 5 {
		t.Fatalf("%d results, want 5: %+v", len(res.Members), res)
	}

	for i, want := range []string{"web", "api", "proxy", "named", "worker"} {
		if got := res.Members[i].Service; got != want {
			t.Errorf("result %d is for %q, want %q: results are in the order of the request", i, got, want)
		}
	}

	checkMemberResults(t, res.Members)

	var list []api.SessionInfo
	mustCall(t, p, api.MethodSessionList, nil, &list)

	if len(list) != 2 || list[0].Group != "app" || list[1].Group != "app" {
		t.Errorf("sessions = %+v, want the two attached members in group app", list)
	}
}

// checkMemberResults checks the outcomes of TestContainerAttachMembers' five
// members: attached, failed, skipped, failed (named), attached.
func checkMemberResults(t *testing.T, members []api.ContainerMemberResult) {
	t.Helper()

	checkWebMember(t, members[0])

	if api.CodeOf(members[1].Error) != api.CodeAttachFailed || members[1].Session != nil {
		t.Errorf("broken member = %+v, want ATTACH_FAILED", members[1])
	}

	if got := members[2]; got.Skipped != "pid 1 runs nginx" || got.Error != nil || got.Session != nil {
		t.Errorf("implicit nginx = %+v, want skipped", got)
	}

	if got := members[3]; api.CodeOf(got.Error) != api.CodeNotDotnet || got.Skipped != "" {
		t.Errorf("named nginx = %+v, want a NOT_DOTNET error, not skipped", got)
	}

	if got := members[4]; got.Session == nil || got.Session.Session.Container.PID != 9 {
		t.Errorf("worker = %+v", got)
	}
}

func checkWebMember(t *testing.T, web api.ContainerMemberResult) {
	t.Helper()

	if web.Session == nil || web.Error != nil || web.Skipped != "" {
		t.Fatalf("web = %+v, want attached", web)
	}

	info := web.Session.Session
	if info.Group != "app" || info.Container == nil || info.Container.Name != "web-1" || info.PID != 0 {
		t.Errorf("web session = %+v, want group app, container web-1, no host pid", info)
	}
}

// TestContainerAttachManyMembers: more members than start at once all
// attach, in order.
func TestContainerAttachManyMembers(t *testing.T) {
	t.Parallel()

	p := startContainerServer(t)

	const n = 5

	members := make([]api.ContainerSpec, n)
	for i := range members {
		members[i] = api.ContainerSpec{Ref: "svc-" + strconv.Itoa(i), Service: "s" + strconv.Itoa(i), PID: i + 1}
	}

	var res api.ContainerAttachResult
	mustCall(t, p, api.MethodContainerAttach, api.ContainerAttachParams{Lang: "fake", Members: members}, &res)

	for i, m := range res.Members {
		if m.Service != "s"+strconv.Itoa(i) || m.Session == nil || m.Session.Session.Container.PID != i+1 {
			t.Errorf("member %d = %+v", i, m)
		}
	}
}

func TestContainerAttachWaitsForOneMember(t *testing.T) {
	t.Parallel()

	p := startContainerServer(t)
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	host := filepath.Join(local, "prog.txt")
	if err := os.WriteFile(host, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var res api.ContainerAttachResult

	mustCall(t, p, api.MethodContainerAttach, api.ContainerAttachParams{
		Lang: "fake", Wait: api.Duration(20 * time.Second), Breakpoints: []api.BreakpointSpec{{File: host, Line: 3}},
		Members: []api.ContainerSpec{{Ref: "web-1", Map: []api.PathMapping{{Remote: "/src", Local: local}}}},
	}, &res)

	snap := res.Members[0].Session
	if snap == nil || snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Line != 3 || snap.Frame.File != host {
		t.Fatalf("result = %+v, want stopped at the breakpoint with the host path", res.Members[0])
	}
}

func TestContainerAttachChecksTheWholeRequest(t *testing.T) {
	t.Parallel()

	p := startContainerServer(t)
	good := api.ContainerSpec{Ref: "web-1"}

	tests := map[string]api.ContainerAttachParams{
		"no members":        {Lang: "fake"},
		"too many members":  {Lang: "fake", Members: make([]api.ContainerSpec, 65)},
		"flag as a ref":     {Lang: "fake", Members: []api.ContainerSpec{good, {Ref: "--privileged"}}},
		"bad host":          {Lang: "fake", Members: []api.ContainerSpec{{Ref: "web-1", Engine: api.ContainerEngine{Host: "x y://"}}}},
		"bad group":         {Lang: "fake", Group: "Bad Group", Members: []api.ContainerSpec{good}},
		"bad service":       {Lang: "fake", Members: []api.ContainerSpec{{Ref: "web-1", Service: "a b"}}},
		"unknown language":  {Lang: "cobol", Members: []api.ContainerSpec{good}},
		"unknown lease":     {Lang: "fake", LeasePolicy: "whenever", Members: []api.ContainerSpec{good}},
		"unknown exception": {Lang: "fake", Exceptions: "some", Members: []api.ContainerSpec{good}},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var res api.ContainerAttachResult

			err := callAs(t, p, api.MethodContainerAttach, params, &res)
			if name == "unknown language" || name == "unknown lease" || name == "unknown exception" {
				// These fail per member, with the same code.
				if err != nil || res.Members[0].Error == nil || res.Members[0].Error.Code != api.CodeInvalidRequest {
					t.Errorf("err = %v, result %+v, want a per-member INVALID_REQUEST", err, res)
				}

				return
			}

			if api.CodeOf(err) != api.CodeInvalidRequest {
				t.Errorf("err = %v, want INVALID_REQUEST for the whole request", err)
			}
		})
	}

	var list []api.SessionInfo
	mustCall(t, p, api.MethodSessionList, nil, &list)

	if len(list) != 0 {
		t.Errorf("%d sessions started by requests that were wrong", len(list))
	}
}

// TestSessionStartRefusesContainers: session.start must never carry a
// container, whatever this daemon would do with it.
func TestSessionStartRefusesContainers(t *testing.T) {
	t.Parallel()

	p := startContainerServer(t)

	err := callAs(t, p, api.MethodSessionStart, api.StartParams{Lang: "fake", Attach: &api.AttachSpec{Container: &api.ContainerSpec{Ref: "web-1"}}}, nil)
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), api.MethodContainerAttach) {
		t.Errorf("err = %v, want INVALID_REQUEST naming container.attach", err)
	}
}
