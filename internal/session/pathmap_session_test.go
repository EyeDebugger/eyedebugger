// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// remoteDriver runs the fake program under a container path (remote), with
// a path map: the fake adapter reports frames and matches breakpoints at
// remote, as netcoredbg in a container does.
type remoteDriver struct {
	remote string
	pm     *PathMap
	// noAdapter names an adapter that doesn't exist: a start that gets as
	// far as running it fails with ADAPTER_FAILED.
	noAdapter bool
}

func (remoteDriver) Name() string { return "fake" }

func (d remoteDriver) Prepare(_ context.Context, spec LaunchSpec) (Launch, error) {
	path, args, env, err := daptest.CommandWith(daptest.Options{})
	if err != nil {
		return Launch{}, err
	}

	pa := daptest.ProgramArgs{Program: d.remote, Lines: 10, StopAtEntry: spec.StopOnEntry, Hang: true}

	if d.noAdapter {
		path = filepath.Join(filepath.Dir(path), "no-such-adapter")
	}

	return Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: pa.Map(), PathMap: d.pm,
	}, nil
}

// mappedRig is a host tree for path-map sessions: the map's host directory
// (root, mapped to /src) holding app/main.cs, and a file outside it.
type mappedRig struct {
	root, main, outside string
	pm                  *PathMap
}

func newMappedRig(t *testing.T) mappedRig {
	t.Helper()

	dir := mkdirs(t, realTempDir(t), "host/app", "other")
	r := mappedRig{
		root:    filepath.Join(dir, "host"),
		main:    filepath.Join(dir, "host", "app", "main.cs"),
		outside: filepath.Join(dir, "other", "main.cs"),
	}

	var text strings.Builder
	for i := range 10 {
		text.WriteString("line " + strconv.Itoa(i+1) + "\n")
	}

	for _, f := range []string{r.main, r.outside} {
		if err := os.WriteFile(f, []byte(text.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	r.pm = mustPathMap(t, api.PathMapping{Remote: "/src", Local: r.root})

	return r
}

// startRemote starts a session whose fake program is at remote, stopped at
// entry, with start breakpoints bps.
func (r mappedRig) startRemote(t *testing.T, remote string, bps ...api.BreakpointSpec) (*Session, error) {
	t.Helper()

	return r.startWith(t, remoteDriver{remote: remote, pm: r.pm}, bps...)
}

func (r mappedRig) startWith(t *testing.T, drv remoteDriver, bps ...api.BreakpointSpec) (*Session, error) {
	t.Helper()

	m := newTestManagerWith(t, nil, drv)

	return m.Start(t.Context(), agentC, api.StartParams{
		Lang: "fake", LaunchSpec: api.LaunchSpec{Program: r.main, StopOnEntry: true}, Breakpoints: bps,
	})
}

func (r mappedRig) mustStart(t *testing.T, remote string, bps ...api.BreakpointSpec) *Session {
	t.Helper()

	s, err := r.startRemote(t, remote, bps...)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	expectStopped(t, s.Wait(t.Context(), 0, testWait, api.DumpSpec{}), "entry", 1)

	return s
}

// A breakpoint on the host file reaches the adapter at the container path
// and hits; the stop's frame, stack and source excerpt are the host's, and
// the stop is attributed to the breakpoint whose condition held.
func TestPathMapSessionRoundTrip(t *testing.T) {
	t.Parallel()

	r := newMappedRig(t)
	s := r.mustStart(t, "/src/app/main.cs", api.BreakpointSpec{File: r.main, Line: 5})

	b := addBP(t, s, agentC, r.main, 3, "")
	if !b.Verified || b.File != r.main {
		t.Errorf("breakpoint = %+v, want verified at %s", b, r.main)
	}

	// The human's differing condition makes the stop a filtered, attributed
	// one: the filter matches the frame's (mapped) file to the breakpoints'.
	addBP(t, s, humanC, r.main, 3, "false")

	if got := adapterBPs(t, s); got != "3,5" {
		t.Errorf("the adapter holds %q at /src/app/main.cs, want 3,5", got)
	}

	if _, err := s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{Function: "f7"}); err != nil {
		t.Errorf("function breakpoint: %v", err)
	}

	snap, err := s.Resume(t.Context(), agentC, ExecContinue, 0, testWait, api.DumpSpec{Dump: []string{api.DumpStack}})
	if err != nil {
		t.Fatal(err)
	}

	expectStopped(t, snap, "breakpoint", 3)
	checkHostStop(t, snap, r.main, b.ID)

	frames, err := s.Stack(t.Context(), 0, 5)
	if err != nil || len(frames) == 0 || frames[0].File != r.main {
		t.Errorf("Stack = %+v, %v; want frames at %s", frames, err, r.main)
	}
}

// checkHostStop checks that a stop at line 3 of file names the host file in
// its frame and stack, has file's excerpt, and is attributed to breakpoint
// id.
func checkHostStop(t *testing.T, snap api.Snapshot, file string, id int) {
	t.Helper()

	if snap.Frame.File != file {
		t.Errorf("frame file = %q, want %q", snap.Frame.File, file)
	}

	if len(snap.Stack) == 0 || snap.Stack[0].File != file {
		t.Errorf("stack = %+v, want frames at %s", snap.Stack, file)
	}

	if i := slices.IndexFunc(snap.Source, func(l api.SourceLine) bool { return l.Current }); i < 0 || snap.Source[i].Text != "line 3" {
		t.Errorf("source = %+v, want line 3 current", snap.Source)
	}

	if st := snap.Session.Stop; st == nil || !slices.ContainsFunc(st.Breakpoints, func(sb api.StopBreakpoint) bool { return sb.ID == id }) {
		t.Errorf("stop = %+v, want it attributed to breakpoint %d", st, id)
	}
}

// Line breakpoints outside the map are refused every way in: bp add,
// run-until, a start's breakpoints; nothing reaches the adapter.
func TestPathMapSessionRefusesOutside(t *testing.T) {
	t.Parallel()

	r := newMappedRig(t)

	// Refused before the adapter runs: this one doesn't exist.
	_, err := r.startWith(t, remoteDriver{remote: "/src/app/main.cs", pm: r.pm, noAdapter: true}, api.BreakpointSpec{File: r.outside, Line: 2})
	expectCode(t, err, api.CodeInvalidRequest)

	s := r.mustStart(t, "/src/app/main.cs")

	tests := []struct {
		name string
		do   func() error
	}{
		{"bp add", func() error {
			_, err := s.AddBreakpoint(t.Context(), agentC, api.BreakpointSpec{File: r.outside, Line: 2})

			return err
		}},
		{"run-until", func() error {
			_, err := s.RunUntil(t.Context(), agentC, api.BreakpointSpec{File: r.outside, Line: 2}, 0, testWait, api.DumpSpec{})

			return err
		}},
	}

	for _, tt := range tests {
		err := tt.do()
		expectCode(t, err, api.CodeInvalidRequest)

		e, _ := errors.AsType[*api.Error](err)
		if e == nil || !strings.Contains(e.Message, "outside the container's path map") || !strings.Contains(e.Hint, "/src="+r.root) {
			t.Errorf("%s: err = %+v, want the map in its hint", tt.name, e)
		}
	}

	if bps := s.Breakpoints(""); len(bps) != 0 {
		t.Errorf("breakpoints = %+v, want none", bps)
	}

	if got := adapterBPs(t, s); got != "" {
		t.Errorf("the adapter holds %q, want none", got)
	}

	// An editor's list: the outside entry is refused alone.
	got, err := s.ReplaceBreakpoints(t.Context(), humanC, r.outside, []api.BreakpointSpec{{Line: 2}}, nil)
	if err != nil || len(got) != 1 || got[0].ID != 0 || !strings.Contains(got[0].Message, "outside the container's path map") {
		t.Errorf("ReplaceBreakpoints outside = %+v, %v; want the entry refused", got, err)
	}

	got, err = s.ReplaceBreakpoints(t.Context(), humanC, r.main, []api.BreakpointSpec{{Line: 4}}, nil)
	if err != nil || len(got) != 1 || got[0].ID == 0 {
		t.Errorf("ReplaceBreakpoints inside = %+v, %v; want it placed", got, err)
	}
}

// A frame the map doesn't take keeps its container path and gets no source
// excerpt, even though a host file exists at that very path; a mapped frame
// whose host file is a symlink out of the map gets none either.
func TestPathMapSessionReadsOnlyInside(t *testing.T) {
	t.Parallel()

	r := newMappedRig(t)

	link := filepath.Join(r.root, "app", "link.cs")
	hasLink := os.Symlink(r.outside, link) == nil

	tests := []struct {
		name, remote, wantFile string
		skip                   bool
	}{
		{"unmapped path naming a host file", r.outside, r.outside, false},
		{"mapped path through a symlink out", "/src/app/link.cs", link, !hasLink},
		{"mapped path", "/src/app/main.cs", r.main, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if tt.skip {
				t.Skip("no symlinks here")
			}

			s := r.mustStart(t, tt.remote)
			snap := s.Snapshot(t.Context(), api.DumpSpec{})

			if snap.Frame == nil || snap.Frame.File != tt.wantFile {
				t.Fatalf("frame = %+v, want it at %s", snap.Frame, tt.wantFile)
			}

			if read := snap.Source != nil; read != (tt.wantFile == r.main) {
				t.Errorf("source = %+v; want an excerpt only for the mapped file", snap.Source)
			}
		})
	}
}

// A frame the map takes, inside the map's directory, still gets no source
// excerpt when its file isn't a source file by name: a hostile adapter may
// name anything under a mapped directory (a container's labels pick the
// default map's), and snapshots go into an agent's context. A link named like
// source to such a file is refused too, since the file read is the link's
// target; the endings are matched in any case.
func TestPathMapSessionReadsOnlySourceFiles(t *testing.T) {
	t.Parallel()

	r := newMappedRig(t)
	app := filepath.Join(r.root, "app")

	data := []byte(strings.Repeat("a line\n", 10))

	tests := []struct {
		name string
		file string
		want bool
	}{
		{"source", "main.cs", true},
		{"source in capitals", "Upper.CS", true},
		{"generated source", "Page.razor.g.cs", true},
		{"private key", "id_rsa", false},
		{"pem", "server.pem", false},
		{"env file", ".env", false},
		{"json settings", "appsettings.json", false},
		{"ending only", ".cs", false},
	}

	for _, tt := range tests {
		if err := os.WriteFile(filepath.Join(app, tt.file), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	link := filepath.Join(app, "link.cs")
	hasLink := os.Symlink(filepath.Join(app, "id_rsa"), link) == nil

	if hasLink {
		tests = append(tests, struct {
			name string
			file string
			want bool
		}{"source-named link to a private key", "link.cs", false})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := r.mustStart(t, "/src/app/"+tt.file)
			snap := s.Snapshot(t.Context(), api.DumpSpec{})

			if snap.Frame == nil || snap.Frame.File != filepath.Join(app, tt.file) {
				t.Fatalf("frame = %+v, want it at %s", snap.Frame, filepath.Join(app, tt.file))
			}

			if read := snap.Source != nil; read != tt.want {
				t.Errorf("source = %+v; want an excerpt: %v", snap.Source, tt.want)
			}
		})
	}
}
