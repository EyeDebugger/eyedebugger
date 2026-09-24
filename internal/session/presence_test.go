// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// presenceSession starts a handoff session held by the agent, with an
// exception filter for mode all, and returns it with its program.
func presenceSession(t *testing.T) (s *Session, file string) {
	t.Helper()

	drv := fakeDriver{excFilters: map[api.ExceptionMode][]string{api.ExceptionsAll: {"all"}}}
	file = writeProgram(t, 10)
	s = start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{
		LeasePolicy: api.LeaseHandoff, LaunchSpec: api.LaunchSpec{Program: file, StopOnEntry: true},
	})

	return s, s.Program
}

// connected returns client id's open connections as the session shows them.
func connected(s *Session, id string) int {
	for _, c := range s.Info().Clients {
		if c.ID == id {
			return c.Connected
		}
	}

	return -1
}

// humanBreakpoints gives the human an editor breakpoint at 3, a CLI one at
// 5 and a run-until temporary at 9; the agent gets one at 4.
func humanBreakpoints(t *testing.T, s *Session, file string) {
	t.Helper()

	replaceLines(t, s, humanC, file, lines("3"))
	addBP(t, s, humanC, file, 5, "")
	addBP(t, s, agentC, file, 4, "")

	s.mu.Lock()
	temp := s.addNewLocked(humanC.ID, api.BreakpointSpec{File: file, Line: 9})
	temp.Temporary = true
	s.mu.Unlock()
}

// leftLines renders the session's breakpoints as "owner:line", sorted.
func leftLines(s *Session) []string {
	bps := s.Breakpoints("")
	out := make([]string, 0, len(bps))

	for i := range bps {
		out = append(out, bps[i].Owner+":"+strconv.Itoa(bps[i].Line))
	}

	slices.Sort(out)

	return out
}

func TestPresenceLeave(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup runs while the human is connected, before it leaves.
		setup      func(t *testing.T, s *Session, p *Presence)
		wantEvents []string
		wantHolder string
		wantModes  int
	}{
		{
			name: "holder leaves",
			setup: func(t *testing.T, s *Session, p *Presence) {
				t.Helper()

				if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
					t.Fatal(err)
				}

				setExceptions(t, s, humanC, api.ExceptionsAll, false)
				p.ExceptionsSet(api.ExceptionsAll)
			},
			wantEvents: []string{"client:disconnected", "breakpoint:removed", "exceptions:none", "lease:release"},
		},
		{
			name: "requester leaves",
			setup: func(t *testing.T, s *Session, _ *Presence) {
				t.Helper()

				if _, err := s.RequestLease(humanC, "please"); err != nil {
					t.Fatal(err)
				}
			},
			wantEvents: []string{"client:disconnected", "breakpoint:removed"},
			wantHolder: agentC.ID,
		},
		{
			name: "mode set with the CLI stays",
			setup: func(t *testing.T, s *Session, _ *Presence) {
				t.Helper()

				setExceptions(t, s, humanC, api.ExceptionsAll, false)
			},
			wantEvents: []string{"client:disconnected", "breakpoint:removed"},
			wantHolder: agentC.ID, wantModes: 1,
		},
		{
			name: "editor set none last",
			setup: func(t *testing.T, s *Session, p *Presence) {
				t.Helper()

				setExceptions(t, s, humanC, api.ExceptionsAll, false)
				p.ExceptionsSet(api.ExceptionsAll)
				p.ExceptionsSet(api.ExceptionsNone)
			},
			wantEvents: []string{"client:disconnected", "breakpoint:removed"},
			wantHolder: agentC.ID, wantModes: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, file := presenceSession(t)
			before := s.log.latest()

			p := s.Connect(humanC)
			if n := connected(s, humanC.ID); n != 1 {
				t.Fatalf("connected = %d, want 1", n)
			}

			humanBreakpoints(t, s, file)
			tt.setup(t, s, p)

			// First seen, then connected.
			if got := kindsAndActions(s.log.query(api.EventsParams{Since: before, Limit: 2}).Events); !slices.Equal(got, []string{"client", "client:connected"}) {
				t.Errorf("first events = %v, want client, client:connected", got)
			}

			mark := s.log.latest()
			p.Leave(t.Context(), 0)
			p.Leave(t.Context(), 0) // idempotent

			if got := kindsAndActions(s.log.query(api.EventsParams{Since: mark}).Events); !slices.Equal(got, tt.wantEvents) {
				t.Errorf("events = %v, want %v", got, tt.wantEvents)
			}

			checkLeft(t, s, tt.wantHolder, tt.wantModes)
		})
	}
}

// checkLeft checks the session after the human left: the agent's, the
// human's CLI and temporary breakpoints stay, the lease is holder's
// without requests, wantModes exception modes are left, and a release by
// the human says it disconnected.
func checkLeft(t *testing.T, s *Session, holder string, wantModes int) {
	t.Helper()

	if got := leftLines(s); !slices.Equal(got, []string{"agent:4", "human:t:5", "human:t:9"}) {
		t.Errorf("breakpoints = %v, want the agent's, the CLI one and the temporary", got)
	}

	if l := s.Lease(); l.Holder != holder || len(l.Requests) != 0 {
		t.Errorf("lease = %+v, want holder %q and no requests", l, holder)
	}

	if res, _ := s.Exceptions(t.Context(), agentC, api.ExceptionsParams{}); len(res.Modes) != wantModes {
		t.Errorf("exception modes = %+v, want %d", res.Modes, wantModes)
	}

	if leases := eventsOf(s, api.EventLease); holder == "" {
		if last := leases[len(leases)-1]; last.Reason != "disconnected" || last.Client != humanC.ID || last.Previous != humanC.ID {
			t.Errorf("release event = %+v, want by human:t, reason disconnected", last)
		}
	}

	if n := connected(s, humanC.ID); n != 0 {
		t.Errorf("connected after leaving = %d", n)
	}
}

func TestPresenceLastConnectionOnly(t *testing.T) {
	t.Parallel()

	s, file := presenceSession(t)
	first, second := s.Connect(humanC), s.Connect(humanC)

	if n := connected(s, humanC.ID); n != 2 {
		t.Fatalf("connected = %d, want 2", n)
	}

	if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
		t.Fatal(err)
	}

	humanBreakpoints(t, s, file)

	mark := s.log.latest()
	first.Leave(t.Context(), 0)

	if got := kindsAndActions(s.log.query(api.EventsParams{Since: mark}).Events); !slices.Equal(got, []string{"client:disconnected"}) {
		t.Errorf("first leave: events %v, want only client:disconnected", got)
	}

	if l := s.Lease(); l.Holder != humanC.ID || len(leftLines(s)) != 4 || connected(s, humanC.ID) != 1 {
		t.Errorf("first leave: lease %+v, breakpoints %v", l, leftLines(s))
	}

	second.Leave(t.Context(), 0)

	if l := s.Lease(); l.Holder != "" || len(leftLines(s)) != 3 {
		t.Errorf("last leave: lease %+v, breakpoints %v", l, leftLines(s))
	}

	// The agent was never connected: another client's leaving never
	// releases its lease.
	if _, err := s.TakeLease(agentC, false); err != nil {
		t.Fatal(err)
	}

	p := s.Connect(humanC)
	p.Leave(t.Context(), 0)

	if l := s.Lease(); l.Holder != agentC.ID {
		t.Errorf("after the human left: lease %+v, want the agent's", l)
	}
}

// TestPresenceReconnect: a connection of the client opened after its last
// one closed cancels the cleanup — whether it runs at once (the split
// phases) or after the restart grace.
func TestPresenceReconnect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		leave func(t *testing.T, s *Session, p *Presence)
	}{
		{
			name: "between the phases",
			leave: func(t *testing.T, s *Session, p *Presence) {
				t.Helper()

				gen, ok := p.decrement(t.Context())
				if !ok {
					t.Fatal("decrement: the client didn't leave")
				}

				next := s.Connect(humanC)
				s.leave(t.Context(), humanC, gen)

				// The reconnection's own leaving cleans up; the stale
				// cleanup after it does nothing more.
				next.Leave(t.Context(), 0)
				s.leave(t.Context(), humanC, gen)
			},
		},
		{
			name: "within the grace",
			leave: func(t *testing.T, s *Session, p *Presence) {
				t.Helper()

				p.Leave(t.Context(), time.Hour)
				next := s.Connect(humanC)
				next.Leave(t.Context(), 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, file := presenceSession(t)
			p := s.Connect(humanC)

			if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
				t.Fatal(err)
			}

			humanBreakpoints(t, s, file)
			mark := s.log.latest()

			tt.leave(t, s, p)

			want := []string{"client:disconnected", "client:connected", "client:disconnected", "breakpoint:removed", "lease:release"}
			if got := kindsAndActions(s.log.query(api.EventsParams{Since: mark}).Events); !slices.Equal(got, want) {
				t.Errorf("events = %v, want %v (one cleanup, by the last connection)", got, want)
			}
		})
	}
}

// TestPresenceGraceKeeps: within the grace nothing is removed or released.
func TestPresenceGraceKeeps(t *testing.T) {
	t.Parallel()

	s, file := presenceSession(t)
	p := s.Connect(humanC)

	if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
		t.Fatal(err)
	}

	humanBreakpoints(t, s, file)
	p.Leave(t.Context(), time.Hour)
	s.Connect(humanC)

	if l := s.Lease(); l.Holder != humanC.ID || len(leftLines(s)) != 4 {
		t.Errorf("after a restart: lease %+v, breakpoints %v; want both kept", l, leftLines(s))
	}
}

// logsAfter reports whether fn logged an event.
func logsAfter(t *testing.T, s *Session, fn func()) bool {
	t.Helper()

	mark := s.log.latest()
	fn()

	return s.log.latest() != mark
}

func TestPresenceWithoutCleanup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// end prepares the leave and returns its ctx.
		end        func(t *testing.T, s *Session) context.Context
		wantEvents []string
	}{
		{
			name: "daemon stopping",
			end: func(t *testing.T, _ *Session) context.Context {
				t.Helper()

				ctx, cancel := context.WithCancel(t.Context())
				cancel()

				return ctx
			},
			wantEvents: []string{"client:disconnected"},
		},
		{
			name: "session ended",
			end: func(t *testing.T, s *Session) context.Context {
				t.Helper()

				if err := s.Terminate(t.Context(), humanC); err != nil {
					t.Fatal(err)
				}

				return t.Context()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s, file := presenceSession(t)
			p := s.Connect(humanC)

			if _, err := s.GrantLease(agentC, humanC.ID, false); err != nil {
				t.Fatal(err)
			}

			humanBreakpoints(t, s, file)
			ctx := tt.end(t, s)
			mark := s.log.latest()

			p.Leave(ctx, 0)

			if got := kindsAndActions(s.log.query(api.EventsParams{Since: mark}).Events); !slices.Equal(got, tt.wantEvents) {
				t.Errorf("events = %v, want %v", got, tt.wantEvents)
			}

			if l := s.Lease(); l.Holder != humanC.ID {
				t.Errorf("lease = %+v, want kept", l)
			}

			if n := connected(s, humanC.ID); n != 0 {
				t.Errorf("connected = %d, want 0", n)
			}

			// After the end, presence logs nothing.
			if tt.name == "session ended" && logsAfter(t, s, func() { s.Connect(humanC).Leave(t.Context(), 0) }) {
				t.Errorf("presence events after the session ended")
			}
		})
	}
}
