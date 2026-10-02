// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

const (
	containerA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	containerB = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
)

// containerDriver attaches to "containers" over the fake adapter: it checks
// nothing about docker, and describes the container as the ref names it
// (containerA for any ref but "other").
type containerDriver struct {
	fakeDriver

	prepared *atomic.Int32
	// err is what PrepareContainerAttach returns.
	err error
	// noInfo leaves Launch.Container nil.
	noInfo bool
	// broken, while set, points the adapter at a program that doesn't exist:
	// the start fails after the claim was taken.
	broken *atomic.Bool
	// program is what the fake attaches to.
	program string
}

func newContainerDriver(program string) containerDriver {
	return containerDriver{
		fakeDriver: fakeDriver{attach: daptest.ProgramArgs{Program: program, Lines: 5, Hang: true}},
		prepared:   new(atomic.Int32), program: program, broken: new(atomic.Bool),
	}
}

func (d containerDriver) PrepareContainerAttach(ctx context.Context, spec api.AttachSpec) (Launch, error) {
	d.prepared.Add(1)

	if d.err != nil {
		return Launch{}, d.err
	}

	pid := max(spec.Container.PID, 1)

	l, err := d.PrepareAttach(ctx, api.AttachSpec{PID: pid})
	if err != nil {
		return Launch{}, err
	}

	pm, err := NewPathMap(spec.Container.Map)
	if err != nil {
		return Launch{}, err
	}

	id := containerA
	if spec.Container.Ref == "other" {
		id = containerB
	}

	if d.broken.Load() {
		l.Adapter = filepath.Join(filepath.Dir(l.Adapter), "no-such-adapter")
	}

	l.PathMap = pm
	l.Program = "web (compose app, container web-1, pid " + string(rune('0'+pid%10)) + ")"

	if !d.noInfo {
		l.Container = &api.ContainerInfo{
			ID: id, Name: "web-1", Service: "web", Project: "app", PID: pid, Platform: "linux/arm64",
			Map: pm.Mappings(), UnhealthyAfter: api.Duration(18_000_000_000),
		}
	}

	return l, nil
}

func startContainer(m *Manager, ref string, pid int, p api.StartParams) (*Session, error) {
	p.Lang = "fake"
	p.Attach = &api.AttachSpec{Container: &api.ContainerSpec{Ref: ref, PID: pid}}

	return m.Start(context.Background(), agentC, p)
}

func TestContainerAttachRefusesBreakpointsOutsideTheMap(t *testing.T) {
	t.Parallel()

	prog := fakeProgram(t)
	m := newTestManagerWith(t, nil, newContainerDriver(prog))

	// The session has no path map entries: a line breakpoint by host path
	// can't be sent.
	_, err := startContainer(m, "web-1", 424242, api.StartParams{Group: "app", Breakpoints: []api.BreakpointSpec{{File: prog, Line: 3}}})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "outside the container's path map") {
		t.Fatalf("err = %v, want INVALID_REQUEST outside the map", err)
	}
}

func TestContainerAttachInfo(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newContainerDriver(fakeProgram(t)))

	// A pid no host process has: nothing checks it on the host.
	s, err := startContainer(m, "web-1", 424242, api.StartParams{Group: "app"})
	if err != nil {
		t.Fatal(err)
	}

	info := s.Info()

	if info.Mode != api.ModeAttach || info.Group != "app" || info.PID != 0 {
		t.Errorf("info = %+v, want an attach in group app without a host pid", info)
	}

	c := info.Container
	if c == nil || c.ID != containerA || c.Service != "web" || c.PID != 424242 || c.Platform != "linux/arm64" || c.UnhealthyAfter != 18_000_000_000 {
		t.Errorf("container = %+v", c)
	}

	// A process event of the adapter names a container pid: not the host's.
	s.onEvent(&godap.ProcessEvent{Body: godap.ProcessEventBody{SystemProcessId: 77}})

	if pid := s.Info().PID; pid != 0 {
		t.Errorf("pid after a process event = %d, want none for a container session", pid)
	}

	if started := eventsOf(s, api.EventStarted); len(started) != 1 || started[0].Action != api.ModeAttach {
		t.Errorf("started events = %+v", started)
	}
}

func TestContainerInfoIsACopy(t *testing.T) {
	t.Parallel()

	root := realTempDir(t)
	m := newTestManagerWith(t, nil, newContainerDriver(fakeProgram(t)))

	s, err := m.Start(t.Context(), agentC, api.StartParams{Lang: "fake", Attach: &api.AttachSpec{
		Container: &api.ContainerSpec{Ref: "web-1", Map: []api.PathMapping{{Remote: "/src", Local: root}}},
	}})
	if err != nil {
		t.Fatal(err)
	}

	first := s.Info()
	if len(first.Container.Map) != 1 || first.Container.Map[0].Remote != "/src" {
		t.Fatalf("map = %+v", first.Container.Map)
	}

	first.Container.Map[0].Remote = "/changed"
	first.Container.Name = "changed"

	if again := s.Info().Container; again.Map[0].Remote != "/src" || again.Name != "web-1" {
		t.Errorf("a reader changed the session's container info: %+v", again)
	}
}

func TestContainerAttachNeedsAContainerAttacher(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, fakeDriver{})

	_, err := startContainer(m, "web-1", 1, api.StartParams{})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "can't attach inside a container") {
		t.Errorf("err = %v, want INVALID_REQUEST: can't attach inside a container", err)
	}
}

func TestContainerAttachChecksBeforeTheDriver(t *testing.T) {
	t.Parallel()

	d := newContainerDriver(fakeProgram(t))
	m := newTestManagerWith(t, nil, d)

	tests := []struct {
		name    string
		p       api.StartParams
		spec    api.ContainerSpec
		hostPID int
		want    string
	}{
		{"launch options", api.StartParams{LaunchSpec: api.LaunchSpec{Cwd: "/x"}}, api.ContainerSpec{Ref: "web-1"}, 0, "attaching takes no launch options"},
		{"bad group", api.StartParams{Group: "Not A Group"}, api.ContainerSpec{Ref: "web-1"}, 0, "invalid group name"},
		{"relative map local", api.StartParams{}, api.ContainerSpec{Ref: "web-1", Map: []api.PathMapping{{Remote: "/src", Local: "rel"}}}, 0, "invalid path map"},
		{"a host pid", api.StartParams{}, api.ContainerSpec{Ref: "web-1"}, 5, "names its process in the container spec"},
		{"relative map remote", api.StartParams{}, api.ContainerSpec{Ref: "web-1", Map: []api.PathMapping{{Remote: "src", Local: t.TempDir()}}}, 0, "invalid path map"},
	}

	for _, tt := range tests {
		tt.p.Lang, tt.p.Attach = "fake", &api.AttachSpec{PID: tt.hostPID, Container: &tt.spec}

		_, err := m.Start(t.Context(), agentC, tt.p)
		if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want INVALID_REQUEST containing %q", tt.name, err, tt.want)
		}
	}

	if n := d.prepared.Load(); n != 0 {
		t.Errorf("the driver ran %d times for requests that fail first", n)
	}
}

func TestContainerAttachDriverProblems(t *testing.T) {
	t.Parallel()

	t.Run("driver error", func(t *testing.T) {
		t.Parallel()

		d := newContainerDriver(fakeProgram(t))
		d.err = api.NewError(api.CodeAttachFailed, "container web-1 is not running", "")
		m := newTestManagerWith(t, nil, d)

		_, err := startContainer(m, "web-1", 1, api.StartParams{})
		expectCode(t, err, api.CodeAttachFailed)

		if n := len(m.List()); n != 0 {
			t.Errorf("%d sessions after a failed start", n)
		}
	})

	t.Run("a failed start releases its claim", func(t *testing.T) {
		t.Parallel()

		d := newContainerDriver(fakeProgram(t))
		m := newTestManagerWith(t, nil, d)

		d.broken.Store(true)

		if _, err := startContainer(m, "web-1", 1, api.StartParams{}); err == nil {
			t.Fatal("a start with a missing adapter succeeded")
		}

		d.broken.Store(false)

		if _, err := startContainer(m, "web-1", 1, api.StartParams{}); err != nil {
			t.Errorf("an attach after a failed start: %v", err)
		}
	})

	t.Run("driver names no container", func(t *testing.T) {
		t.Parallel()

		d := newContainerDriver(fakeProgram(t))
		d.noInfo = true
		m := newTestManagerWith(t, nil, d)

		_, err := startContainer(m, "web-1", 1, api.StartParams{})
		expectCode(t, err, api.CodeInternal)
	})

	t.Run("not a candidate passes through", func(t *testing.T) {
		t.Parallel()

		d := newContainerDriver(fakeProgram(t))
		d.err = errors.Join(api.NewError(api.CodeNotDotnet, "pid 1 runs nginx", ""), ErrNotCandidate)
		m := newTestManagerWith(t, nil, d)

		_, err := startContainer(m, "web-1", 1, api.StartParams{})
		if !errors.Is(err, ErrNotCandidate) || api.CodeOf(err) != api.CodeNotDotnet {
			t.Errorf("err = %v, want NOT_DOTNET marked ErrNotCandidate", err)
		}
	})
}

func TestContainerClaims(t *testing.T) {
	t.Parallel()

	d := newContainerDriver(fakeProgram(t))
	m := newTestManagerWith(t, nil, d)

	first, err := startContainer(m, "web-1", 1, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}

	// The same process again: refused, naming the first session.
	_, err = startContainer(m, "web-1", 1, api.StartParams{})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), first.ID) {
		t.Errorf("a second attach: %v, want INVALID_REQUEST naming %s", err, first.ID)
	}

	// Another process of it, and another container's pid 1: fine.
	if _, err := startContainer(m, "web-1", 2, api.StartParams{}); err != nil {
		t.Errorf("another pid: %v", err)
	}

	if _, err := startContainer(m, "other", 1, api.StartParams{}); err != nil {
		t.Errorf("another container: %v", err)
	}

	// Once the first session ended, its claim goes with it.
	if err := first.Detach(t.Context(), agentC); err != nil {
		t.Fatal(err)
	}

	if _, err := startContainer(m, "web-1", 1, api.StartParams{}); err != nil {
		t.Errorf("an attach after the first ended: %v", err)
	}
}

// TestContainerClaimsRace: starts at the same moment don't both get the
// process.
func TestContainerClaimsRace(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newContainerDriver(fakeProgram(t)))

	const racers = 6

	var (
		wg   sync.WaitGroup
		ok   atomic.Int32
		busy atomic.Int32
	)

	for range racers {
		wg.Go(func() {
			_, err := startContainer(m, "web-1", 1, api.StartParams{})

			switch {
			case err == nil:
				ok.Add(1)
			case api.CodeOf(err) == api.CodeInvalidRequest && strings.Contains(err.Error(), "already debugged"):
				busy.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}

	wg.Wait()

	if ok.Load() != 1 || busy.Load() != racers-1 {
		t.Errorf("%d attached, %d refused, want 1 and %d", ok.Load(), busy.Load(), racers-1)
	}
}

func TestStoppedAt(t *testing.T) {
	t.Parallel()

	m := newTestManager(t, nil)
	s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Program: fakeProgram(t), StopOnEntry: true}})

	snap := s.Wait(t.Context(), 0, testWait, api.DumpSpec{})
	if snap.Session.State != api.StateStopped || snap.Session.StoppedAt == nil || snap.Session.StoppedAt.IsZero() {
		t.Fatalf("stopped session: %+v, want StoppedAt set", snap.Session)
	}

	first := *snap.Session.StoppedAt

	snap = resume(t, s, agentC, ExecNext)
	if snap.Session.State != api.StateStopped || snap.Session.StoppedAt == nil || snap.Session.StoppedAt.Before(first) {
		t.Errorf("after a step: %+v, want a StoppedAt not before %v", snap.Session, first)
	}
}

func TestStoppedAtOnlyWhileStopped(t *testing.T) {
	t.Parallel()

	s := newSession(t.Context(), "s-t", "fake", "", Launch{}, slog.New(slog.DiscardHandler), agentC, api.LeaseFree)

	if s.Info().StoppedAt != nil {
		t.Error("a session that never stopped has StoppedAt")
	}

	s.mu.Lock()
	s.applyStopLocked(api.StopInfo{Reason: "breakpoint"})
	s.mu.Unlock()

	if at := s.Info().StoppedAt; at == nil || at.IsZero() {
		t.Error("a stopped session has no StoppedAt")
	}

	s.mu.Lock()
	s.setStateLocked(api.StateRunning)
	s.mu.Unlock()

	if s.Info().StoppedAt != nil {
		t.Error("a running session has StoppedAt")
	}
}
