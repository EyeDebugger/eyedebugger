// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
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
