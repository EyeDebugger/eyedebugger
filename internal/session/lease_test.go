// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func mustClient(t *testing.T, id string) api.Client {
	t.Helper()

	c, err := api.ParseClient(id)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

// TestLeaseRules checks every policy, holder, requester kind, operation and
// force against the table in docs/DESIGN.md §3.
func TestLeaseRules(t *testing.T) {
	t.Parallel()

	// takeFromOther says whether a requester may take the lease (exec or
	// take) from another client, by policy.
	takeFromOther := map[api.LeasePolicy]func(requesterHuman, holderHuman bool) bool{
		api.LeaseFree:          func(bool, bool) bool { return true },
		api.LeaseHandoff:       func(bool, bool) bool { return false },
		api.LeaseHumanPriority: func(r, h bool) bool { return r && !h },
	}

	policies := api.LeasePolicies()
	holders := []string{"", "self", "agent:other", "human:other"}
	requesters := []string{"agent:me", "human:me"}
	ops := []leaseOp{opExec, opTake, opAdmin}

	// Every combination, by index: policy, holder, requester, op, force.
	n := len(policies) * len(holders) * len(requesters) * len(ops) * 2
	for i := range n {
		policy := policies[i%len(policies)]
		holder := holders[i/len(policies)%len(holders)]
		c := mustClient(t, requesters[i/(len(policies)*len(holders))%len(requesters)])
		op := ops[i/(len(policies)*len(holders)*len(requesters))%len(ops)]
		force := i/(len(policies)*len(holders)*len(requesters)*len(ops)) == 1

		l := lease{policy: policy}

		switch holder {
		case "":
		case "self":
			l.holder = c
		default:
			l.holder = mustClient(t, holder)
		}

		want := true
		if !force && holder != "" && holder != "self" {
			want = op != opAdmin && takeFromOther[policy](c.IsHuman(), l.holder.IsHuman())
		}

		if got := l.allows(op, c, force); got != want {
			t.Errorf("%s, holder %q, %s, op %d, force %v: allows = %v, want %v", policy, holder, c.ID, op, force, got, want)
		}
	}
}

func TestLeaseInfo(t *testing.T) {
	t.Parallel()

	l := lease{policy: api.LeaseHandoff}
	if info := l.info(); info.Holder != "" || info.Since != nil || info.Policy != api.LeaseHandoff {
		t.Errorf("nobody holding: info = %+v", info)
	}

	l.holder = mustClient(t, "human:x")
	if info := l.info(); info.Holder != "human:x" || info.Since == nil {
		t.Errorf("held: info = %+v", info)
	}
}

// requestClients returns the clients of pending lease requests, in order,
// comma-separated and ended by "|".
func requestClients(l api.LeaseInfo) string {
	out := make([]string, 0, len(l.Requests))
	for _, r := range l.Requests {
		out = append(out, r.Client)
	}

	return strings.Join(out, ",") + "|"
}

func TestRequestLease(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy api.LeasePolicy
		// holder takes the lease before the request; "" leaves it free.
		holder    api.Client
		requester api.Client
		message   string
		wantCode  api.Code
		wantEvent bool
	}{
		{name: "free", policy: api.LeaseFree, holder: agentC, requester: humanC, message: "let me step", wantEvent: true},
		{name: "handoff", policy: api.LeaseHandoff, holder: agentC, requester: humanC, message: "x", wantEvent: true},
		{name: "human-priority", policy: api.LeaseHumanPriority, holder: humanC, requester: agentC, wantEvent: true},
		{name: "by the holder", policy: api.LeaseHandoff, holder: agentC, requester: agentC},
		{name: "nobody holds it", policy: api.LeaseHandoff, requester: humanC},
		{
			name: "200 characters", policy: api.LeaseFree, holder: agentC, requester: humanC,
			message: strings.Repeat("é", api.MaxLeaseRequestMessage), wantEvent: true,
		},
		{
			name: "201 characters", policy: api.LeaseFree, holder: agentC, requester: humanC,
			message: strings.Repeat("é", api.MaxLeaseRequestMessage+1), wantCode: api.CodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := start(t, newTestManager(t, nil), agentC,
				api.StartParams{LeasePolicy: tt.policy, LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

			s.ReleaseLease(agentC)

			if tt.holder.ID != "" {
				if _, err := s.TakeLease(tt.holder, true); err != nil {
					t.Fatal(err)
				}
			}

			before := s.log.latest()

			l, err := s.RequestLease(tt.requester, tt.message)
			if tt.wantCode != "" {
				expectCode(t, err, tt.wantCode)
			} else if err != nil {
				t.Fatal(err)
			}

			events := s.log.query(api.EventsParams{Since: before, Kinds: []api.EventKind{api.EventLease}}).Events
			if !tt.wantEvent {
				if len(events) != 0 || len(s.Lease().Requests) != 0 {
					t.Errorf("events %+v, lease %+v; want no request", events, s.Lease())
				}

				return
			}

			checkRequested(t, l, events, tt.holder.ID, api.LeaseRequest{Client: tt.requester.ID, Message: tt.message})
		})
	}
}

// checkRequested: the lease (held by holder) has want pending, and events
// are the one request event.
func checkRequested(t *testing.T, l api.LeaseInfo, events []api.Event, holder string, want api.LeaseRequest) {
	t.Helper()

	if len(l.Requests) != 1 || l.Requests[0].Client != want.Client || l.Requests[0].Message != want.Message ||
		l.Requests[0].At.IsZero() || l.Holder != holder {
		t.Errorf("lease = %+v, want holder %s and request %+v", l, holder, want)
	}

	if len(events) != 1 || events[0].Action != "request" || events[0].Client != want.Client ||
		events[0].Text != want.Message || events[0].Lease == nil || len(events[0].Lease.Requests) != 1 {
		t.Errorf("events = %+v, want one request event with the message", events)
	}
}

func TestLeaseRequestsQueue(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LeasePolicy: api.LeaseHandoff, LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

	request := func(id, message string) api.LeaseInfo {
		t.Helper()

		l, err := s.RequestLease(mustClient(t, id), message)
		if err != nil {
			t.Fatal(err)
		}

		return l
	}

	request("human:a", "one")
	request("human:b", "")

	// A repeat replaces the client's request and moves it last.
	l := request("human:a", "two")
	if got := requestClients(l); got != "human:b,human:a|" || l.Requests[1].Message != "two" {
		t.Errorf("after a repeat: %+v", l.Requests)
	}

	// The 17th client drops the oldest.
	for i := range maxLeaseRequests - 1 {
		l = request(fmt.Sprintf("agent:n%d", i), "")
	}

	if len(l.Requests) != maxLeaseRequests || l.Requests[0].Client != "human:a" {
		t.Errorf("at the cap: %d requests, first %+v; want %d, human:b dropped", len(l.Requests), l.Requests[0], maxLeaseRequests)
	}
}

func TestLeaseChangesClearRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy api.LeasePolicy
		change func(t *testing.T, s *Session) error
		action string
	}{
		{
			name: "take", policy: api.LeaseFree, action: "take",
			change: func(_ *testing.T, s *Session) error { _, err := s.TakeLease(agentB, false); return err },
		},
		{
			name: "force", policy: api.LeaseHandoff, action: "force",
			change: func(_ *testing.T, s *Session) error { _, err := s.TakeLease(agentB, true); return err },
		},
		{
			name: "auto", policy: api.LeaseFree, action: "auto",
			change: func(t *testing.T, s *Session) error {
				t.Helper()
				resume(t, s, agentB, ExecNext)

				return nil
			},
		},
		{
			name: "grant", policy: api.LeaseHandoff, action: "grant",
			change: func(_ *testing.T, s *Session) error { _, err := s.GrantLease(agentC, humanC.ID, false); return err },
		},
		{
			name: "release", policy: api.LeaseHandoff, action: "release",
			change: func(_ *testing.T, s *Session) error { s.ReleaseLease(agentC); return nil },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := start(t, newTestManager(t, nil), agentC,
				api.StartParams{LeasePolicy: tt.policy, LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

			if _, err := s.RequestLease(humanC, "please"); err != nil {
				t.Fatal(err)
			}

			if err := tt.change(t, s); err != nil {
				t.Fatal(err)
			}

			if l := s.Lease(); len(l.Requests) != 0 {
				t.Errorf("requests after %s: %+v", tt.name, l.Requests)
			}

			leases := eventsOf(s, api.EventLease)
			if last := leases[len(leases)-1]; last.Action != tt.action || len(last.Lease.Requests) != 0 {
				t.Errorf("last lease event = %+v, want %s without requests", last, tt.action)
			}
		})
	}
}

func TestRequestLeaseOnExitedSession(t *testing.T) {
	t.Parallel()

	s := start(t, newTestManager(t, nil), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true, Args: []string{"lines=1"}}})
	if snap := resume(t, s, agentC, ExecContinue); snap.Session.State != api.StateExited {
		t.Fatalf("state = %s, want exited", snap.Session.State)
	}

	_, err := s.RequestLease(humanC, "")
	expectCode(t, err, api.CodeSessionExited)
}

func TestRecordableStripsLeaseRequests(t *testing.T) {
	t.Parallel()

	info := api.LeaseInfo{Policy: api.LeaseHandoff, Holder: "agent", Requests: []api.LeaseRequest{{Client: "human:t", Message: "secret"}}}
	e := api.Event{Kind: api.EventLease, Client: "human:t", Action: "request", Lease: &info, Text: "secret"}

	got, ok := recordable(e)
	if !ok || got.Text != "" || got.Lease == nil || got.Lease.Requests != nil || got.Lease.Holder != "agent" {
		t.Errorf("recordable = %+v, %v; want no text and no requests", got, ok)
	}

	if len(info.Requests) != 1 {
		t.Errorf("recordable changed the logged event's lease: %+v", info)
	}
}
