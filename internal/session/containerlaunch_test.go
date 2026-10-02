// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// launcherDriver launches in "containers" over the fake adapter (it attaches
// to them too, as containerDriver does): the app is /src/prog.txt in the
// container, the container is containerA for any ref but "other", which is
// containerB.
type launcherDriver struct {
	containerDriver

	launched *atomic.Int32
	// err is what PrepareContainerLaunch returns.
	err error
	// noInfo leaves Launch.Container nil; asAttach makes the launch an attach.
	noInfo, asAttach bool
	// hang keeps the app running after its last line.
	hang bool
	// hint is Launch.StartFailureHint; warnings are Launch.Warnings.
	hint     func(context.Context) string
	warnings []string
	// info is the Launch.Container the driver hands out (the last one).
	info *atomic.Pointer[api.ContainerInfo]
}

func newLauncherDriver() launcherDriver {
	return launcherDriver{
		containerDriver: newContainerDriver("/src/prog.txt"), launched: new(atomic.Int32), hang: true,
		info: new(atomic.Pointer[api.ContainerInfo]),
	}
}

func (d launcherDriver) PrepareContainerLaunch(_ context.Context, spec api.ContainerLaunchSpec) (Launch, error) {
	d.launched.Add(1)

	if d.err != nil {
		return Launch{}, d.err
	}

	path, args, env, err := daptest.CommandWith(d.opts)
	if err != nil {
		return Launch{}, err
	}

	pm, err := NewPathMap(spec.Map)
	if err != nil {
		return Launch{}, err
	}

	id, name := containerA, "web-1"
	if spec.Ref == "other" {
		id, name = containerB, "other-1"
	}

	if d.broken.Load() {
		path = filepath.Join(filepath.Dir(path), "no-such-adapter")
	}

	launch := Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake",
		Arguments: daptest.ProgramArgs{Program: "/src/prog.txt", Lines: 5, StopAtEntry: spec.StopOnEntry, Hang: d.hang}.Map(),
		Program:   "web (compose app, container " + name + "): dotnet /app/Web.dll", PathMap: pm,
		StartFailureHint: d.hint, Warnings: d.warnings,
	}

	if d.asAttach {
		launch.Request, launch.PID = RequestAttach, 1
	}

	if !d.noInfo {
		launch.Container = &api.ContainerInfo{
			ID: id, Name: name, Service: "web", Project: "app", Platform: "linux/arm64", Map: pm.Mappings(), UnhealthyAfter: api.Duration(18 * time.Second),
		}
		d.info.Store(launch.Container)
	}

	return launch, nil
}

func launchIn(m *Manager, ref string, p api.StartParams, spec api.ContainerLaunchSpec) (*Session, error) {
	p.Lang = "fake"
	spec.Ref = ref
	p.ContainerLaunch = &spec

	return m.Start(context.Background(), agentC, p)
}

func mustLaunchIn(t *testing.T, m *Manager, p api.StartParams) *Session {
	t.Helper()

	s, err := launchIn(m, "web-1", p, api.ContainerLaunchSpec{})
	if err != nil {
		t.Fatalf("launch in web-1: %v", err)
	}

	return s
}

// hostSource is a host directory holding prog.txt and a path map from /src
// to it.
func hostSource(t *testing.T) (host string, spec api.ContainerLaunchSpec) {
	t.Helper()

	prog := fakeProgram(t)

	return prog, api.ContainerLaunchSpec{Map: []api.PathMapping{{Remote: "/src", Local: filepath.Dir(prog)}}}
}

// TestContainerLaunchSession: a launched session of a container: mode "",
// no host pid, the container marked launched, its pid the app's own once the
// adapter says so; a startup breakpoint, sent before the app ran, is hit.
func TestContainerLaunchSession(t *testing.T) {
	t.Parallel()

	d := newLauncherDriver()
	m := newTestManagerWith(t, nil, d)
	host, spec := hostSource(t)

	s, err := launchIn(m, "web-1", api.StartParams{Group: "app", Breakpoints: []api.BreakpointSpec{{File: host, Line: 3}}}, spec)
	if err != nil {
		t.Fatal(err)
	}

	snap := s.Wait(t.Context(), 0, testWait, api.DumpSpec{})
	expectStopped(t, snap, "breakpoint", 3)

	if snap.Frame == nil || snap.Frame.File != host {
		t.Errorf("frame = %+v, want the host path %s", snap.Frame, host)
	}

	checkLaunchedInfo(t, s, d)
}

// checkLaunchedInfo checks the info of TestContainerLaunchSession's session,
// whose adapter's process event came before its stop.
func checkLaunchedInfo(t *testing.T, s *Session, d launcherDriver) {
	t.Helper()

	info := s.Info()
	if info.Mode != "" || info.PID != 0 || info.Group != "app" {
		t.Errorf("info = %+v, want a launched session in group app without a host pid", info)
	}

	c := info.Container
	if c == nil || !c.Launched || c.ID != containerA || c.Service != "web" || c.UnhealthyAfter != api.Duration(18*time.Second) {
		t.Fatalf("container = %+v, want a launched one", c)
	}

	// The app's pid, as the container numbers it, not a host pid.
	if c.PID != 4242 {
		t.Errorf("container pid = %d, want the process event's 4242", c.PID)
	}

	if got := d.info.Load(); got == nil || got.Launched || got.PID != 0 {
		t.Errorf("the driver's own container info was changed: %+v", got)
	}

	if started := eventsOf(s, api.EventStarted); len(started) != 1 || started[0].Action != "" {
		t.Errorf("started events = %+v, want one with no action (a launch)", started)
	}
}

// TestContainerLaunchStopEntry: stopOnEntry reaches the driver.
func TestContainerLaunchStopEntry(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())

	s, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{StopOnEntry: true})
	if err != nil {
		t.Fatal(err)
	}

	expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "entry", 1)
}

// TestContainerLaunchStopKillsDetachRefused: stop ends the app (disconnect
// with terminateDebuggee true), frees the container at once, and detach is
// refused: the app dies with its session.
func TestContainerLaunchStopKillsDetachRefused(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())
	s := mustLaunchIn(t, m, api.StartParams{})

	_, err := m.Detach(t.Context(), agentC, s.ID)
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "'eyedbg stop'") {
		t.Fatalf("detach err = %v, want INVALID_REQUEST pointing at stop", err)
	}

	if st := s.Info().State; st == api.StateExited {
		t.Fatal("a refused detach ended the session")
	}

	if _, err := m.Stop(t.Context(), agentC, s.ID); err != nil {
		t.Fatal(err)
	}

	if out := outputText(s); !strings.Contains(out, "terminateDebuggee=true") {
		t.Errorf("stop didn't terminate the debuggee; adapter output:\n%s", out)
	}

	if n := claimCount(m); n != 0 {
		t.Errorf("%d claims after stop, want none", n)
	}
}

func claimCount(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.claims)
}

func TestContainerLaunchNeedsAContainerLauncher(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newContainerDriver(fakeProgram(t))) // attaches, can't launch

	_, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "can't launch inside a container") {
		t.Errorf("err = %v, want INVALID_REQUEST: can't launch inside a container", err)
	}
}

// TestContainerLaunchChecksBeforeTheDriver: what is wrong without looking at
// the container never reaches the driver.
func TestContainerLaunchChecksBeforeTheDriver(t *testing.T) {
	t.Parallel()

	d := newLauncherDriver()
	m := newTestManagerWith(t, nil, d)

	tests := []struct {
		name string
		p    api.StartParams
		spec api.ContainerLaunchSpec
		want string
	}{
		{"program", api.StartParams{LaunchSpec: api.LaunchSpec{Program: "/x"}}, api.ContainerLaunchSpec{}, "--program"},
		{"project", api.StartParams{LaunchSpec: api.LaunchSpec{Project: "/x"}}, api.ContainerLaunchSpec{}, "--project"},
		{"args", api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"x"}}}, api.ContainerLaunchSpec{}, "program arguments"},
		{"cwd", api.StartParams{LaunchSpec: api.LaunchSpec{Cwd: "/x"}}, api.ContainerLaunchSpec{}, "--cwd"},
		{"env", api.StartParams{LaunchSpec: api.LaunchSpec{Env: map[string]string{"A": "b"}}}, api.ContainerLaunchSpec{}, "--env"},
		{"no-build", api.StartParams{LaunchSpec: api.LaunchSpec{NoBuild: true}}, api.ContainerLaunchSpec{}, "--no-build"},
		{"host stop on entry", api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}}, api.ContainerLaunchSpec{}, "--stop-on-entry"},
		{"options", api.StartParams{LaunchSpec: api.LaunchSpec{Options: map[string]string{"a": "b"}}}, api.ContainerLaunchSpec{}, "--opt"},
		{"bad group", api.StartParams{Group: "Not A Group"}, api.ContainerLaunchSpec{}, "invalid group name"},
		{"relative map local", api.StartParams{}, api.ContainerLaunchSpec{Map: []api.PathMapping{{Remote: "/src", Local: "rel"}}}, "invalid path map"},
		{"attach too", api.StartParams{Attach: &api.AttachSpec{PID: 3}}, api.ContainerLaunchSpec{}, "not several"},
	}

	for _, tt := range tests {
		_, err := launchIn(m, "web-1", tt.p, tt.spec)
		if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want INVALID_REQUEST containing %q", tt.name, err, tt.want)
		}
	}

	if n := d.launched.Load(); n != 0 {
		t.Errorf("the driver ran %d times for requests that fail first", n)
	}
}

// TestContainerLaunchBreakpointOutsideTheMap: a start breakpoint the map
// can't carry fails the start before the adapter runs, claim freed.
func TestContainerLaunchBreakpointOutsideTheMap(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())

	// No map entries: a line breakpoint by host path can't be sent.
	_, err := launchIn(m, "web-1", api.StartParams{Breakpoints: []api.BreakpointSpec{{File: fakeProgram(t), Line: 3}}}, api.ContainerLaunchSpec{})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "outside the container's path map") {
		t.Fatalf("err = %v, want INVALID_REQUEST outside the map", err)
	}

	if n := claimCount(m); n != 0 {
		t.Errorf("%d claims after a refused start", n)
	}
}

func TestContainerLaunchDriverProblems(t *testing.T) {
	t.Parallel()

	t.Run("driver error", func(t *testing.T) {
		t.Parallel()

		d := newLauncherDriver()
		d.err = api.NewError(api.CodeInvalidRequest, "container web-1 isn't in eyedbg fast mode", "")
		m := newTestManagerWith(t, nil, d)

		_, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{})
		expectCode(t, err, api.CodeInvalidRequest)

		if n, c := len(m.List()), claimCount(m); n != 0 || c != 0 {
			t.Errorf("%d sessions and %d claims after a failed start", n, c)
		}
	})

	t.Run("no container", func(t *testing.T) {
		t.Parallel()

		d := newLauncherDriver()
		d.noInfo = true
		m := newTestManagerWith(t, nil, d)

		_, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{})
		expectCode(t, err, api.CodeInternal)
	})

	t.Run("an attach", func(t *testing.T) {
		t.Parallel()

		d := newLauncherDriver()
		d.asAttach = true
		m := newTestManagerWith(t, nil, d)

		_, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{})
		expectCode(t, err, api.CodeInternal)
	})
}

// TestContainerLaunchClaims: one app per container.
func TestContainerLaunchClaims(t *testing.T) {
	t.Parallel()

	d := newLauncherDriver()
	m := newTestManagerWith(t, nil, d)

	first := mustLaunchIn(t, m, api.StartParams{})

	// The same container again, by name or by id prefix: refused naming the
	// first session, before the driver even looks at it.
	for _, ref := range []string{"web-1", containerA[:12], containerA} {
		_, err := launchIn(m, ref, api.StartParams{}, api.ContainerLaunchSpec{})
		if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), first.ID) {
			t.Errorf("a second launch by %q: %v, want INVALID_REQUEST naming %s", ref, err, first.ID)
		}
	}

	if n := d.launched.Load(); n != 1 {
		t.Errorf("the driver ran %d times, want 1: a claimed container is refused before it", n)
	}

	// Another container is free.
	if _, err := launchIn(m, "other", api.StartParams{}, api.ContainerLaunchSpec{}); err != nil {
		t.Errorf("another container: %v", err)
	}

	// Once the first session ended, the container is free again.
	if _, err := m.Stop(t.Context(), agentC, first.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{}); err != nil {
		t.Errorf("a launch after the first ended: %v", err)
	}
}

// TestContainerLaunchClaimByFullID: a ref the early check can't match to a
// claim (another name for the container) is still refused, by the full id the
// driver returns.
func TestContainerLaunchClaimByFullID(t *testing.T) {
	t.Parallel()

	d := newLauncherDriver()
	m := newTestManagerWith(t, nil, d)

	first := mustLaunchIn(t, m, api.StartParams{})

	// The fake driver answers containerA for any ref but "other".
	_, err := launchIn(m, "alias", api.StartParams{}, api.ContainerLaunchSpec{})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), first.ID) {
		t.Errorf("launch by another name: %v, want INVALID_REQUEST naming %s", err, first.ID)
	}

	if n := d.launched.Load(); n != 2 {
		t.Errorf("the driver ran %d times, want 2: the full id is known only after it", n)
	}

	if n := len(m.List()); n != 1 {
		t.Errorf("%d sessions, want the first alone", n)
	}
}

// TestContainerLaunchRace: starts at the same moment don't both get the
// container, however they name it.
func TestContainerLaunchRace(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())

	const racers = 8

	var (
		wg   sync.WaitGroup
		ok   atomic.Int32
		busy atomic.Int32
	)

	// The container by its name, its id and a name the early check can't match.
	refs := []string{"web-1", containerA, "alias"}

	for i := range racers {
		ref := refs[i%len(refs)]

		wg.Go(func() {
			_, err := launchIn(m, ref, api.StartParams{}, api.ContainerLaunchSpec{})

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
		t.Errorf("%d launched, %d refused, want 1 and %d", ok.Load(), busy.Load(), racers-1)
	}

	if n := len(m.List()); n != 1 {
		t.Errorf("%d sessions, want exactly one", n)
	}
}

// TestContainerLaunchClaimFreedAfterAFailedStart: the claim is released by
// the failed start itself (a failed start's session has exited, which would
// let a relaunch take the claim over: look at the claim, not at a relaunch).
func TestContainerLaunchClaimFreedAfterAFailedStart(t *testing.T) {
	t.Parallel()

	d := newLauncherDriver()
	m := newTestManagerWith(t, nil, d)

	d.broken.Store(true)

	if _, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{}); err == nil {
		t.Fatal("a start with a missing adapter succeeded")
	}

	if n := claimCount(m); n != 0 {
		t.Errorf("%d claims after a failed start, want none", n)
	}

	d.broken.Store(false)

	if _, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{}); err != nil {
		t.Errorf("a launch after a failed start: %v", err)
	}
}

// TestContainerLaunchClaimFreedByExit: a session that ended on its own (the
// app exited) frees its container: the manager's watch releases the claim, and
// a launch right after the exit never sees a stale one.
func TestContainerLaunchClaimFreedByExit(t *testing.T) {
	t.Parallel()

	d := newLauncherDriver()
	d.hang = false // the app runs its five lines and exits

	live := make(chan int, 64)
	ctx, cancel := context.WithCancel(context.Background())

	m := NewManager(ctx, Config{Drivers: []Driver{d}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard, OnLive: func(n int) {
		select {
		case live <- n:
		default:
		}
	}})
	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	s, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{})
	if err != nil {
		t.Fatal(err)
	}

	// The watch reports the live count after it released the claim.
	for n := range live {
		if n == 0 && s.Info().State == api.StateExited {
			break
		}
	}

	if n := claimCount(m); n != 0 {
		t.Errorf("%d claims after the app exited, want none", n)
	}

	if _, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{}); err != nil {
		t.Errorf("a launch after the first ended: %v", err)
	}
}

// TestContainerClaimTakesOverAnEndedHolder: a claim whose session ended but
// was not released yet (the manager's watch runs on its own goroutine) blocks
// neither a launch nor an attach; a live holder still does.
func TestContainerClaimTakesOverAnEndedHolder(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())
	probe := func(id string) *Session {
		return newSession(m.ctx, id, "fake", "", Launch{}, slog.New(slog.DiscardHandler), agentC, api.LeaseFree)
	}

	holder := probe("s-held")
	if err := m.claimLaunch(holder, containerA); err != nil {
		t.Fatal(err)
	}

	if err := m.claimLaunch(probe("s-next"), containerA); err == nil {
		t.Fatal("a live holder's launch claim was taken")
	}

	if err := m.claim(probe("s-att"), containerA, 1); err == nil {
		t.Fatal("a live launch holder's container was attached")
	}

	holder.mu.Lock()
	holder.setStateLocked(api.StateExited)
	holder.mu.Unlock()

	next := probe("s-next")
	if err := m.claimLaunch(next, containerA); err != nil {
		t.Fatalf("launch over an ended holder: %v", err)
	}

	m.release(holder) // late: must not drop the new holder's claim

	if m.claims[launchClaimKey(containerA)] != next {
		t.Error("the ended session's late release dropped its successor's claim")
	}

	next.mu.Lock()
	next.setStateLocked(api.StateExited)
	next.mu.Unlock()

	if err := m.claim(probe("s-att"), containerA, 1); err != nil {
		t.Errorf("attach over an ended launch holder: %v", err)
	}
}

// A container holds either one launch or any number of attaches with
// distinct pids, never both.
func TestContainerClaimsLaunchThenAttach(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())
	launched := mustLaunchIn(t, m, api.StartParams{})

	for _, pid := range []int{1, 7} {
		_, err := startContainer(m, "web-1", pid, api.StartParams{})
		if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), launched.ID) || !strings.Contains(err.Error(), "launched") {
			t.Errorf("attach to pid %d: %v, want INVALID_REQUEST naming the launch %s", pid, err, launched.ID)
		}
	}

	// Another container is free for an attach.
	if _, err := startContainer(m, "other", 1, api.StartParams{}); err != nil {
		t.Errorf("attach elsewhere: %v", err)
	}
}

func TestContainerClaimsAttachThenLaunch(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())

	first, err := startContainer(m, "web-1", 1, api.StartParams{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := startContainer(m, "web-1", 2, api.StartParams{}); err != nil { // distinct pids: fine
		t.Fatal(err)
	}

	// Any attach of the container, whichever pid, keeps a launch out: by a
	// name or id the early check matches, and by one only the full id catches.
	for _, ref := range []string{"web-1", containerA, "alias"} {
		_, err = launchIn(m, ref, api.StartParams{}, api.ContainerLaunchSpec{})
		if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "already debugged") {
			t.Errorf("launch by %q over attaches: %v, want INVALID_REQUEST", ref, err)
		}
	}

	// Both attaches have to go before the launch can take the container.
	if err := first.Detach(t.Context(), agentC); err != nil {
		t.Fatal(err)
	}

	if _, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{}); err == nil {
		t.Error("a launch with one attach still live was accepted")
	}
}

// TestContainerLaunchFailureHint: a failed start asks the driver what may
// still run in the container and says it in the error's hint, within a bounded
// time; a start that succeeds never asks.
func TestContainerLaunchFailureHint(t *testing.T) {
	t.Parallel()

	var (
		asked    atomic.Int32
		deadline atomic.Bool
	)

	d := newLauncherDriver()
	d.hint = func(ctx context.Context) string {
		asked.Add(1)

		if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= failureHintTimeout {
			deadline.Store(true)
		}

		return "a dotnet process may still run in container web-1: docker restart web-1"
	}

	m := newTestManagerWith(t, nil, d)

	// A good start doesn't ask.
	good := mustLaunchIn(t, m, api.StartParams{})

	if asked.Load() != 0 {
		t.Fatal("the hint was asked for after a successful start")
	}

	if _, err := m.Stop(t.Context(), agentC, good.ID); err != nil {
		t.Fatal(err)
	}

	d.broken.Store(true)

	_, err := launchIn(m, "web-1", api.StartParams{}, api.ContainerLaunchSpec{})

	e, ok := errors.AsType[*api.Error](err)
	if !ok || !strings.Contains(e.Hint, "docker restart web-1") || !strings.Contains(e.Hint, "check 'eyedbg adapters doctor'") {
		t.Fatalf("err = %v (%+v), want the adapter failure's hint plus the driver's", err, e)
	}

	if asked.Load() != 1 || !deadline.Load() {
		t.Errorf("hint asked %d times, with a bounded context: %v; want once, bounded", asked.Load(), deadline.Load())
	}
}

// TestContainerLaunchWarnings: what the driver noticed is logged, not failed.
func TestContainerLaunchWarnings(t *testing.T) {
	t.Parallel()

	var logs lockedLog

	d := newLauncherDriver()
	d.warnings = []string{"docker top failed: boom"}

	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, Config{Drivers: []Driver{d}, Logger: slog.New(slog.NewTextHandler(&logs, nil)), Stderr: io.Discard})
	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	mustLaunchIn(t, m, api.StartParams{})

	if got := logs.String(); !strings.Contains(got, "docker top failed: boom") || !strings.Contains(got, "level=WARN") {
		t.Errorf("log = %q, want the warning at WARN", got)
	}
}

// lockedLog is a log sink safe for the manager's goroutines.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

// TestContainerLaunchGroupMember: the launched session is a group member like
// any other.
func TestContainerLaunchGroupMember(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())
	s := mustLaunchIn(t, m, api.StartParams{Group: "app"})

	if got := s.service(); got != "web" {
		t.Errorf("service = %q, want web", got)
	}

	if g := s.Info().Group; g != "app" {
		t.Errorf("group = %q", g)
	}
}

// TestManagerEndsReleaseTheirClaims: stop, detach and stop-all release the
// claim themselves, not only through the watch that follows an exit (sessions
// here have no watch, so nothing else could).
func TestManagerEndsReleaseTheirClaims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mode string
		end  func(m *Manager, s *Session) error
	}{
		{"stop", "", func(m *Manager, s *Session) error { _, err := m.Stop(t.Context(), agentC, s.ID); return err }},
		{"detach", api.ModeAttach, func(m *Manager, s *Session) error { _, err := m.Detach(t.Context(), agentC, s.ID); return err }},
		{"stop all", "", func(m *Manager, _ *Session) error { m.StopAll(t.Context()); return nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManagerWith(t, nil, newLauncherDriver())
			s := newSession(m.ctx, "s-probe", "fake", tt.mode, Launch{}, slog.New(slog.DiscardHandler), agentC, api.LeaseFree)
			m.add(s)

			if err := m.claimLaunch(s, containerA); err != nil {
				t.Fatal(err)
			}

			if err := tt.end(m, s); err != nil {
				t.Fatal(err)
			}

			if n := claimCount(m); n != 0 {
				t.Errorf("%d claims after %s, want none", n, tt.name)
			}
		})
	}
}

// TestLiveClaimOn: the early check names the live holder of a container by
// its name or an id prefix of 12 or more characters, and nobody else.
func TestLiveClaimOn(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, newLauncherDriver())
	holder := newSession(m.ctx, "s-held", "fake", "", Launch{
		Container: &api.ContainerInfo{ID: containerA, Name: "web-1", Launched: true},
	}, slog.New(slog.DiscardHandler), agentC, api.LeaseFree)

	if err := m.claimLaunch(holder, containerA); err != nil {
		t.Fatal(err)
	}

	for _, ref := range []string{"web-1", containerA, containerA[:12], containerA[:40]} {
		if got := m.liveClaimOn(ref); got != holder {
			t.Errorf("liveClaimOn(%q) = %v, want the holder", ref, got)
		}
	}

	// Another name, a too-short prefix and another container's id: not it.
	for _, ref := range []string{"web-2", containerA[:11], containerB, "alias"} {
		if got := m.liveClaimOn(ref); got != nil {
			t.Errorf("liveClaimOn(%q) = session %s, want none", ref, got.ID)
		}
	}

	// An ended holder whose claim isn't released yet is not in the way.
	holder.mu.Lock()
	holder.setStateLocked(api.StateExited)
	holder.mu.Unlock()

	if got := m.liveClaimOn("web-1"); got != nil {
		t.Errorf("liveClaimOn after the holder ended = session %s, want none", got.ID)
	}
}
