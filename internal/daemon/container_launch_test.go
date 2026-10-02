// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// launchFake launches in "containers" on the fake adapter: the app is
// /src/prog.txt in the container. Ref "broken" is not in fast mode.
type launchFake struct {
	containerFake

	prepared *atomic.Int32
}

func (d launchFake) PrepareContainerLaunch(_ context.Context, spec api.ContainerLaunchSpec) (session.Launch, error) {
	d.prepared.Add(1)

	if spec.Ref == "broken" {
		return session.Launch{}, api.NewError(api.CodeInvalidRequest, "container broken isn't in eyedbg fast mode", "put it there with 'eyedbg compose launch api'")
	}

	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	pm, err := session.NewPathMap(spec.Map)
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake",
		Arguments: daptest.ProgramArgs{Program: "/src/prog.txt", Lines: 5, StopAtEntry: spec.StopOnEntry, Hang: true}.Map(),
		Program:   "container " + spec.Ref + ": dotnet /app/App.dll", PathMap: pm,
		Container: &api.ContainerInfo{ID: launchContainerID(spec.Ref), Name: spec.Ref, Service: spec.Service, Project: spec.Project, Platform: "linux/arm64", Map: pm.Mappings()},
	}, nil
}

// launchContainerID is a full id that differs for every ref.
func launchContainerID(ref string) string {
	sum := sha256.Sum256([]byte(ref))

	return hex.EncodeToString(sum[:])
}

func startLaunchServer(t *testing.T) (Paths, launchFake) {
	t.Helper()

	d := launchFake{prepared: new(atomic.Int32)}

	return startServerWith(t, PathsIn(shortTempDir(t)), time.Hour, d).paths, d
}

func TestContainerLaunchMembers(t *testing.T) {
	t.Parallel()

	p, _ := startLaunchServer(t)

	var res api.ContainerLaunchResult

	mustCall(t, p, api.MethodContainerLaunch, api.ContainerLaunchParams{
		Lang: "fake", Group: "app",
		Members: []api.ContainerLaunchSpec{
			{Ref: "web-1", Service: "web", Project: "app"},
			{Ref: "broken", Service: "api"},
			{Ref: "worker-22", Service: "worker", StopOnEntry: true},
		},
	}, &res)

	if len(res.Members) != 3 {
		t.Fatalf("%d results, want 3: %+v", len(res.Members), res)
	}

	for i, want := range []string{"web", "api", "worker"} {
		if got := res.Members[i].Service; got != want {
			t.Errorf("result %d is for %q, want %q: results are in the order of the request", i, got, want)
		}
	}

	checkLaunchedMembers(t, res.Members)

	var list []api.SessionInfo
	mustCall(t, p, api.MethodSessionList, nil, &list)

	if len(list) != 2 {
		t.Errorf("%d sessions, want the two launched members", len(list))
	}
}

// checkLaunchedMembers checks the outcomes of TestContainerLaunchMembers'
// three members: launched, failed, launched.
func checkLaunchedMembers(t *testing.T, members []api.ContainerMemberResult) {
	t.Helper()

	web := members[0]
	if web.Session == nil || web.Error != nil || web.Session.Session.Group != "app" || web.Session.Session.Mode != "" || web.Session.Session.PID != 0 ||
		web.Session.Session.Container == nil || !web.Session.Session.Container.Launched || web.Session.Session.Container.Name != "web-1" {
		t.Errorf("web = %+v, want a launched container session in group app without a host pid", web)
	}

	if got := members[1]; api.CodeOf(got.Error) != api.CodeInvalidRequest || got.Session != nil || got.Skipped != "" {
		t.Errorf("broken = %+v, want INVALID_REQUEST", got)
	}

	if got := members[2]; got.Session == nil || got.Session.Session.Container.Name != "worker-22" {
		t.Errorf("worker = %+v", got)
	}
}

// TestContainerLaunchManyMembers: more members than start at once all
// launch, in order.
func TestContainerLaunchManyMembers(t *testing.T) {
	t.Parallel()

	p, _ := startLaunchServer(t)

	const n = 6

	members := make([]api.ContainerLaunchSpec, n)
	for i := range members {
		members[i] = api.ContainerLaunchSpec{Ref: "svc-" + strconv.Itoa(i), Service: "s" + strconv.Itoa(i)}
	}

	var res api.ContainerLaunchResult
	mustCall(t, p, api.MethodContainerLaunch, api.ContainerLaunchParams{Lang: "fake", Members: members}, &res)

	for i, m := range res.Members {
		if m.Service != "s"+strconv.Itoa(i) || m.Session == nil || m.Session.Session.Container.Name != "svc-"+strconv.Itoa(i) {
			t.Errorf("member %d = %+v", i, m)
		}
	}
}

// TestContainerLaunchOneAppPerContainer: a container named twice in one
// request, and one already launched, give exactly one session.
func TestContainerLaunchOneAppPerContainer(t *testing.T) {
	t.Parallel()

	p, _ := startLaunchServer(t)

	var res api.ContainerLaunchResult

	mustCall(t, p, api.MethodContainerLaunch, api.ContainerLaunchParams{
		Lang: "fake", Members: []api.ContainerLaunchSpec{{Ref: "web-1", Service: "a"}, {Ref: "web-1", Service: "b"}},
	}, &res)

	launched, refused := 0, 0

	for _, m := range res.Members {
		switch {
		case m.Session != nil:
			launched++
		case m.Error != nil && strings.Contains(m.Error.Message, "already debugged"):
			refused++
		}
	}

	if launched != 1 || refused != 1 {
		t.Fatalf("%d launched, %d refused: %+v, want exactly one of each", launched, refused, res.Members)
	}

	// Stopping the app frees the container.
	var sid string

	for _, m := range res.Members {
		if m.Session != nil {
			sid = m.Session.Session.ID
		}
	}

	var stopped api.SessionInfo
	mustCall(t, p, api.MethodSessionStop, api.SessionRef{SessionID: sid}, &stopped)

	var again api.ContainerLaunchResult
	mustCall(t, p, api.MethodContainerLaunch, api.ContainerLaunchParams{Lang: "fake", Members: []api.ContainerLaunchSpec{{Ref: "web-1"}}}, &again)

	if again.Members[0].Session == nil {
		t.Errorf("a launch after stop: %+v, want the container free", again.Members[0])
	}
}

// TestContainerLaunchBreakpointsAndDetach: the request's breakpoints apply to
// every member (here one) and are mapped; a launched member can't be
// detached from.
func TestContainerLaunchBreakpointsAndDetach(t *testing.T) {
	t.Parallel()

	p, _ := startLaunchServer(t)

	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	host := filepath.Join(local, "prog.txt")
	if err := os.WriteFile(host, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var res api.ContainerLaunchResult

	mustCall(t, p, api.MethodContainerLaunch, api.ContainerLaunchParams{
		Lang: "fake", Breakpoints: []api.BreakpointSpec{{File: host, Line: 3}},
		Members: []api.ContainerLaunchSpec{{Ref: "web-1", Map: []api.PathMapping{{Remote: "/src", Local: local}}}},
	}, &res)

	got := res.Members[0]
	if got.Session == nil {
		t.Fatalf("launch: %+v", got)
	}

	var snap api.Snapshot

	mustCall(t, p, api.MethodWait, api.WaitParams{SessionRef: api.SessionRef{SessionID: got.Session.Session.ID}, Wait: api.Duration(20 * time.Second)}, &snap)

	if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Line != 3 || snap.Frame.File != host {
		t.Fatalf("snapshot = %+v, want stopped at the startup breakpoint with the host path", snap)
	}

	err = callAs(t, p, api.MethodSessionDetach, api.SessionRef{SessionID: snap.Session.ID}, nil)
	if api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("detach err = %v, want INVALID_REQUEST: the app dies with its session", err)
	}
}

func TestContainerLaunchChecksTheWholeRequest(t *testing.T) {
	t.Parallel()

	p, d := startLaunchServer(t)
	good := api.ContainerLaunchSpec{Ref: "web-1"}

	tests := map[string]api.ContainerLaunchParams{
		"no members":       {Lang: "fake"},
		"too many members": {Lang: "fake", Members: make([]api.ContainerLaunchSpec, 65)},
		"flag as a ref":    {Lang: "fake", Members: []api.ContainerLaunchSpec{good, {Ref: "--privileged"}}},
		"bad host":         {Lang: "fake", Members: []api.ContainerLaunchSpec{{Ref: "web-1", Engine: api.ContainerEngine{Host: "x y://"}}}},
		"bad context":      {Lang: "fake", Members: []api.ContainerLaunchSpec{{Ref: "web-1", Engine: api.ContainerEngine{Context: "-x"}}}},
		"bad group":        {Lang: "fake", Group: "Bad Group", Members: []api.ContainerLaunchSpec{good}},
		"bad service":      {Lang: "fake", Members: []api.ContainerLaunchSpec{{Ref: "web-1", Service: "a b"}}},
		"bad project":      {Lang: "fake", Members: []api.ContainerLaunchSpec{{Ref: "web-1", Project: "A B"}}},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := callAs(t, p, api.MethodContainerLaunch, params, &api.ContainerLaunchResult{})
			if api.CodeOf(err) != api.CodeInvalidRequest {
				t.Errorf("err = %v, want INVALID_REQUEST for the whole request", err)
			}
		})
	}

	var list []api.SessionInfo
	mustCall(t, p, api.MethodSessionList, nil, &list)

	if len(list) != 0 || d.prepared.Load() != 0 {
		t.Errorf("%d sessions, %d driver runs: requests that were wrong must start nothing", len(list), d.prepared.Load())
	}
}

// TestContainerLaunchPerMemberCodes: what is wrong per member fails that
// member only, with the same code.
func TestContainerLaunchPerMemberCodes(t *testing.T) {
	t.Parallel()

	p, _ := startLaunchServer(t)

	tests := map[string]api.ContainerLaunchParams{
		"unknown language":  {Lang: "cobol"},
		"unknown lease":     {Lang: "fake", LeasePolicy: "whenever"},
		"unknown exception": {Lang: "fake", Exceptions: "some"},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			params.Members = []api.ContainerLaunchSpec{{Ref: "web-1"}}

			var res api.ContainerLaunchResult

			if err := callAs(t, p, api.MethodContainerLaunch, params, &res); err != nil || res.Members[0].Error == nil || res.Members[0].Error.Code != api.CodeInvalidRequest {
				t.Errorf("err = %v, result %+v, want a per-member INVALID_REQUEST", err, res)
			}
		})
	}
}

// TestSessionStartRefusesContainerLaunch: session.start must never carry a
// container launch, whatever this daemon would do with it.
func TestSessionStartRefusesContainerLaunch(t *testing.T) {
	t.Parallel()

	p, d := startLaunchServer(t)

	err := callAs(t, p, api.MethodSessionStart, api.StartParams{Lang: "fake", ContainerLaunch: &api.ContainerLaunchSpec{Ref: "web-1"}}, nil)
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), api.MethodContainerLaunch) {
		t.Errorf("err = %v, want INVALID_REQUEST naming container.launch", err)
	}

	if d.prepared.Load() != 0 {
		t.Error("the driver ran for a session.start that carried a container launch")
	}
}

// TestStartMembersBoundsConcurrency: at most maxConcurrentAttaches members
// start at once, however many there are, and every member answers in its own
// place; a member that never got a slot before the request ended answers with
// that. synctest makes "no fifth one is running" a fact, not a hope: once
// every goroutine is blocked, exactly four are inside start.
func TestStartMembersBoundsConcurrency(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		const members = 10

		var (
			inside  atomic.Int32
			mostIn  atomic.Int32
			release = make(chan struct{})
		)

		done := make(chan []api.ContainerMemberResult, 1)

		go func() {
			done <- startMembers(t.Context(), members,
				func(i int) (string, string) { return "s" + strconv.Itoa(i), "c" + strconv.Itoa(i) },
				func(i int) api.ContainerMemberResult {
					n := inside.Add(1)
					mostIn.Store(max(mostIn.Load(), n))
					<-release
					inside.Add(-1)

					return api.ContainerMemberResult{Service: "s" + strconv.Itoa(i)}
				})
		}()

		synctest.Wait() // everything that can start has, and is blocked

		if n := inside.Load(); n != maxConcurrentAttaches {
			t.Fatalf("%d members inside start with everything blocked, want exactly %d", n, maxConcurrentAttaches)
		}

		close(release)

		results := <-done
		if len(results) != members || mostIn.Load() != maxConcurrentAttaches {
			t.Fatalf("%d results, at most %d at once, want %d and %d", len(results), mostIn.Load(), members, maxConcurrentAttaches)
		}

		for i, r := range results {
			if r.Service != "s"+strconv.Itoa(i) {
				t.Errorf("result %d is for %q: results are in member order", i, r.Service)
			}
		}
	})
}

// TestStartMembersAnswersForMembersThatNeverStarted: with the request over,
// members still waiting for a slot answer with the context's error, in place.
func TestStartMembersAnswersForMembersThatNeverStarted(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		release := make(chan struct{})
		done := make(chan []api.ContainerMemberResult, 1)

		go func() {
			done <- startMembers(ctx, 6,
				func(i int) (string, string) { return "s" + strconv.Itoa(i), "c" + strconv.Itoa(i) },
				func(int) api.ContainerMemberResult {
					<-release

					return api.ContainerMemberResult{}
				})
		}()

		synctest.Wait()
		cancel()
		synctest.Wait()
		close(release)

		results := <-done

		failed := 0

		for i, r := range results {
			if r.Error != nil {
				failed++

				if r.Service != "s"+strconv.Itoa(i) || r.Container != "c"+strconv.Itoa(i) {
					t.Errorf("result %d names %q/%q, want its own member", i, r.Service, r.Container)
				}
			}
		}

		if failed != 6-maxConcurrentAttaches {
			t.Errorf("%d members answered with the cancellation, want %d (those without a slot)", failed, 6-maxConcurrentAttaches)
		}
	})
}
