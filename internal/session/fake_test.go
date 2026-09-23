// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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

// fakeDriver runs programs on the daptest fake adapter. Program arguments:
// "hang" keeps it running after its last line, "lines=N" sets its length
// (default 10).
type fakeDriver struct{}

func (fakeDriver) Name() string { return "fake" }

func (fakeDriver) Prepare(_ context.Context, spec LaunchSpec) (Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return Launch{}, err
	}

	lines := 10

	for _, a := range spec.Args {
		if n, ok := strings.CutPrefix(a, "lines="); ok {
			if lines, err = strconv.Atoi(n); err != nil {
				return Launch{}, err
			}
		}
	}

	return Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: daptest.Arguments(spec.Program, lines, spec.StopOnEntry, slices.Contains(spec.Args, "hang")),
	}, nil
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

	ctx, cancel := context.WithCancel(context.Background())
	m := NewManager(ctx, Config{Drivers: []Driver{fakeDriver{}}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard, Store: store})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	return m
}

// fakeProgram is the path of a fake program (it need not exist).
func fakeProgram(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "prog.txt")
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

	res, err := s.Eval(t.Context(), "$bps", 0)
	if err != nil {
		t.Fatalf("eval $bps: %v", err)
	}

	return res.Value
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
