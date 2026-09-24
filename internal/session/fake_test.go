// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// testWait bounds every wait in these tests; none should come close.
const testWait = 20 * time.Second

var (
	agentC = api.Client{ID: "agent", Kind: api.KindAgent}
	humanC = api.Client{ID: "human:t", Kind: api.KindHuman, Name: "t"}
)

// fakeDriver runs programs on the daptest fake adapter, started with opts.
// Program arguments: "hang" keeps it running after its last line,
// "lines=N" sets its length (default 10), "laps=N" runs it N times,
// "throws=L,M" makes lines throw.
type fakeDriver struct {
	opts        daptest.Options
	excFilters  map[api.ExceptionMode][]string
	sideEffects func(string) (string, bool)
	// attach is the program an attach attaches to (its ProcessID is the
	// pid asked for).
	attach daptest.ProgramArgs
	// runner is the fake test runner's mode (see daptest.RunnerCommand),
	// exiting with runnerCode.
	runner     string
	runnerCode int
	// socket, when set, puts the adapter on the connect transport and
	// records the socket paths it was given.
	socket *socketPaths
}

func (fakeDriver) Name() string { return "fake" }

// transport puts l on the connect transport when d.socket is set.
func (d fakeDriver) transport(l Launch) Launch {
	if d.socket == nil {
		return l
	}

	args := l.AdapterArgs
	l.AdapterArgs = nil
	l.SocketArgs = func(path string) []string {
		d.socket.add(path)

		return append(slices.Clone(args), daptest.ConnectArg(path))
	}

	return l
}

// socketPaths are the socket paths a fakeDriver's adapters were given.
type socketPaths struct {
	mu    sync.Mutex
	paths []string
}

func (p *socketPaths) add(path string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.paths = append(p.paths, path)
}

func (p *socketPaths) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Clone(p.paths)
}

func (d fakeDriver) Prepare(_ context.Context, spec LaunchSpec) (Launch, error) {
	path, args, env, err := daptest.CommandWith(d.opts)
	if err != nil {
		return Launch{}, err
	}

	pa := daptest.ProgramArgs{Program: spec.Program, Lines: 10, StopAtEntry: spec.StopOnEntry, Hang: slices.Contains(spec.Args, "hang")}

	if err := parseProgramArgs(&pa, spec.Args); err != nil {
		return Launch{}, err
	}

	return d.transport(Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: pa.Map(), ExceptionFilters: d.excFilters, SideEffects: d.sideEffects,
	}), nil
}

func (d fakeDriver) PrepareAttach(_ context.Context, spec api.AttachSpec) (Launch, error) {
	path, args, env, err := daptest.CommandWith(d.opts)
	if err != nil {
		return Launch{}, err
	}

	pa := d.attach
	pa.ProcessID = spec.PID

	return d.transport(Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Request: RequestAttach, PID: spec.PID,
		Arguments: pa.Map(), AttachHint: "fake attach hint",
	}), nil
}

func (d fakeDriver) TestCommand(_ context.Context, spec TestSpec) (TestCommand, error) {
	path, args, env, err := daptest.RunnerCommand(d.runner, d.runnerCode)
	if err != nil {
		return TestCommand{}, err
	}

	return TestCommand{
		Path: path, Args: args, Env: env, Program: "fake test " + spec.Filter,
		HostPID: func(line string) (int, bool) {
			rest, ok := strings.CutPrefix(line, "Process Id: ")
			if !ok {
				return 0, false
			}

			n, err := strconv.Atoi(strings.Split(rest, ",")[0])

			return n, err == nil
		},
		Failure: func(code int, output string) error {
			return api.NewError(api.CodeBuildFailed, fmt.Sprintf("fake test exited with code %d:\n%s", code, output), "")
		},
	}, nil
}

// parseProgramArgs applies lines=, laps= and throws= arguments.
func parseProgramArgs(pa *daptest.ProgramArgs, args []string) error {
	for _, a := range args {
		name, value, _ := strings.Cut(a, "=")

		var err error

		switch name {
		case "lines":
			pa.Lines, err = strconv.Atoi(value)
		case "laps":
			pa.Laps, err = strconv.Atoi(value)
		case "throws":
			for l := range strings.SplitSeq(value, ",") {
				n, err := strconv.Atoi(l)
				if err != nil {
					return err
				}

				pa.Throws = append(pa.Throws, n)
			}
		}

		if err != nil {
			return err
		}
	}

	return nil
}

// lockedFile is an in-memory recording safe to read while it is written.
type lockedFile struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	closed bool
}

func (f *lockedFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, errors.New("write after close")
	}

	return f.buf.Write(p)
}

func (f *lockedFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.closed = true

	return nil
}

func (f *lockedFile) contents() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	return bytes.Clone(f.buf.Bytes())
}

// stubStore keeps what the manager stores in memory and logs every
// operation as "save ID STATE" or "remove ID".
type stubStore struct {
	lost []api.SessionInfo

	mu         sync.Mutex
	ops        []string
	recordings map[string]*lockedFile
}

func newStubStore(lost ...api.SessionInfo) *stubStore {
	return &stubStore{lost: lost, recordings: make(map[string]*lockedFile)}
}

func (st *stubStore) Save(info api.SessionInfo) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.ops = append(st.ops, "save "+info.ID+" "+string(info.State))

	return nil
}

func (st *stubStore) Remove(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.ops = append(st.ops, "remove "+id)

	return nil
}

func (st *stubStore) Record(id string) (io.WriteCloser, string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	f := &lockedFile{}
	st.recordings[id] = f

	return f, "mem:" + id, nil
}

func (st *stubStore) Lost() ([]api.SessionInfo, error) { return st.lost, nil }

// opsFor returns the operations on id, in order.
func (st *stubStore) opsFor(id string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()

	var out []string

	for _, op := range st.ops {
		if strings.HasPrefix(op, "save "+id+" ") || op == "remove "+id {
			out = append(out, op)
		}
	}

	return out
}

func (st *stubStore) recording(id string) *lockedFile {
	st.mu.Lock()
	defer st.mu.Unlock()

	return st.recordings[id]
}

// newTestManager returns a manager of fake sessions, stopped at cleanup.
func newTestManager(t *testing.T, store Store) *Manager {
	t.Helper()

	return newTestManagerWith(t, store, fakeDriver{})
}

// newTestManagerWith is newTestManager with drv as the fake driver.
func newTestManagerWith(t *testing.T, store Store, drv Driver) *Manager {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, Config{Drivers: []Driver{drv}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard, Store: store})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	return m
}

// fakeProgram is the path of an empty fake program: it exists because
// breakpoints need their file to, and has symlinks resolved as their file
// does.
func fakeProgram(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "prog.txt")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// start starts a fake session for c; with stop on entry it waits for that
// stop.
func start(t *testing.T, m *Manager, c api.Client, p api.StartParams) *Session {
	t.Helper()

	p.Lang = "fake"
	if p.Program == "" {
		p.Program = fakeProgram(t)
	}

	s, err := m.Start(t.Context(), c, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if p.StopOnEntry {
		expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "entry", 1)
	}

	return s
}

func expectStopped(t *testing.T, snap api.Snapshot, reason string, line int) {
	t.Helper()

	st := snap.Session
	if st.State != api.StateStopped || st.Stop == nil || st.Stop.Reason != reason || snap.Frame == nil || snap.Frame.Line != line {
		t.Fatalf("snapshot = %+v (frame %+v), want stopped (%s) at line %d", st, snap.Frame, reason, line)
	}
}

func expectCode(t *testing.T, err error, code api.Code) {
	t.Helper()

	if got := api.CodeOf(err); got != code {
		t.Fatalf("err = %v (code %q), want %s", err, got, code)
	}
}

// resume runs an execution request and fails the test on error.
func resume(t *testing.T, s *Session, c api.Client, kind string) api.Snapshot {
	t.Helper()

	snap, err := s.Resume(t.Context(), c, kind, 0, testWait, api.DumpSpec{})
	if err != nil {
		t.Fatalf("%s %s: %v", c.ID, kind, err)
	}

	return snap
}

// adapterBPs returns the breakpoint lines the fake adapter holds for the
// program file, e.g. "3,5 if false".
func adapterBPs(t *testing.T, s *Session) string {
	t.Helper()

	return evalValue(t, s, "$bps")
}

// evalValue evaluates expr in frame 0 of the stopped program.
func evalValue(t *testing.T, s *Session, expr string) string {
	t.Helper()

	res, err := s.Eval(t.Context(), agentC, api.EvalParams{Expression: expr})
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}

	return res.Value
}

// writeProgram writes a fake program file of n lines ("line N" each, or
// text[N-1] where given and not empty) and returns its path.
func writeProgram(t *testing.T, n int, text ...string) string {
	t.Helper()

	var b strings.Builder

	for i := range n {
		if i < len(text) && text[i] != "" {
			b.WriteString(text[i])
		} else {
			b.WriteString("line " + strconv.Itoa(i+1))
		}

		b.WriteByte('\n')
	}

	path := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func addBP(t *testing.T, s *Session, c api.Client, file string, line int, cond string) api.Breakpoint {
	t.Helper()

	b, err := s.AddBreakpoint(t.Context(), c, api.BreakpointSpec{File: file, Line: line, Condition: cond})
	if err != nil {
		t.Fatalf("%s: add breakpoint at %d: %v", c.ID, line, err)
	}

	return b
}

// eventsOf returns every event of the given kinds (all if none), from seq 1.
func eventsOf(s *Session, kinds ...api.EventKind) []api.Event {
	return s.log.query(api.EventsParams{Limit: api.MaxEventsLimit, Kinds: kinds}).Events
}

// kindsAndActions renders events as "kind" or "kind:action" for comparing
// sequences.
func kindsAndActions(events []api.Event) []string {
	out := make([]string, 0, len(events))

	for i := range events {
		e := &events[i]
		if e.Action != "" {
			out = append(out, string(e.Kind)+":"+e.Action)
		} else {
			out = append(out, string(e.Kind))
		}
	}

	return out
}
