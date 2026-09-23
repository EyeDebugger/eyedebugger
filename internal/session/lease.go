// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"errors"
	"fmt"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// leaseOp is what a client asks of the lease.
type leaseOp int

const (
	// opExec is an execution request: allowed ones take the lease.
	opExec leaseOp = iota
	// opTake is 'lease take'.
	opTake
	// opAdmin is 'lease grant' and 'lease policy': only the holder's.
	opAdmin
)

// lease is a session's control lease (docs/DESIGN.md §3): who may change
// the program's execution. The zero holder means nobody holds it.
type lease struct {
	policy api.LeasePolicy
	holder api.Client
	since  time.Time
}

// allows reports whether c may do op. Nobody holding, c holding, or force
// always allow it. Otherwise grant and policy are refused, and exec and take
// depend on the policy: free allows them (exec takes the lease),
// human-priority only for a human when an agent holds it, handoff never.
func (l *lease) allows(op leaseOp, c api.Client, force bool) bool {
	if force || l.holder.ID == "" || l.holder.ID == c.ID {
		return true
	}

	if op == opAdmin {
		return false
	}

	switch l.policy {
	case api.LeaseFree:
		return true
	case api.LeaseHandoff:
		return false
	case api.LeaseHumanPriority:
		return c.IsHuman() && !l.holder.IsHuman()
	default:
		return false
	}
}

// info returns the lease as the API shows it.
func (l *lease) info() api.LeaseInfo {
	info := api.LeaseInfo{Policy: l.policy, Holder: l.holder.ID}

	if l.holder.ID != "" {
		since := l.since
		info.Since = &since
	}

	return info
}

// Lease returns the session's lease.
func (s *Session) Lease() api.LeaseInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lease.info()
}

// TakeLease gives the lease to c, if the policy allows it or force.
func (s *Session) TakeLease(c api.Client, force bool) (api.LeaseInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lease.holder.ID == c.ID {
		return s.lease.info(), nil
	}

	action := "take"

	if !s.lease.allows(opTake, c, false) {
		if !force {
			return api.LeaseInfo{}, s.leaseHeldLocked(c)
		}

		action = "force"
	}

	s.setHolderLocked(c, c.ID, action)

	return s.lease.info(), nil
}

// ReleaseLease lets go of the lease if c holds it; otherwise it does
// nothing.
func (s *Session) ReleaseLease(c api.Client) api.LeaseInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lease.holder.ID == c.ID {
		s.setHolderLocked(api.Client{}, c.ID, "release")
	}

	return s.lease.info()
}

// GrantLease gives the lease to the client to, on behalf of c: the holder,
// anyone while nobody holds it, or anyone with force.
func (s *Session) GrantLease(c api.Client, to string, force bool) (api.LeaseInfo, error) {
	if to == "" {
		return api.LeaseInfo{}, api.NewError(api.CodeInvalidRequest, "lease grant needs the client to give it to",
			"e.g. 'eyedbg lease grant human:NAME'")
	}

	target, err := api.ParseClient(to)
	if err != nil {
		// The same message, but the hint is about the argument, not the
		// caller's own identity.
		msg := err.Error()
		if e, ok := errors.AsType[*api.Error](err); ok {
			msg = e.Message
		}

		return api.LeaseInfo{}, api.NewError(api.CodeInvalidRequest, msg, "e.g. 'eyedbg lease grant human:NAME'")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.lease.allows(opAdmin, c, force) {
		return api.LeaseInfo{}, s.leaseHeldLocked(c)
	}

	if s.lease.holder.ID != target.ID {
		s.setHolderLocked(target, c.ID, "grant")
	}

	return s.lease.info(), nil
}

// SetLeasePolicy changes the lease policy on behalf of c: the holder,
// anyone while nobody holds it, or anyone with force.
func (s *Session) SetLeasePolicy(c api.Client, policy api.LeasePolicy, force bool) (api.LeaseInfo, error) {
	policy, err := api.ParseLeasePolicy(string(policy))
	if err != nil {
		return api.LeaseInfo{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.lease.allows(opAdmin, c, force) {
		return api.LeaseInfo{}, s.leaseHeldLocked(c)
	}

	if s.lease.policy != policy {
		s.lease.policy = policy
		info := s.lease.info()
		s.log.append(api.Event{Kind: api.EventLease, Client: c.ID, Action: "policy", Lease: &info, Previous: s.lease.holder.ID})
	}

	return s.lease.info(), nil
}

// acquireLocked lets c execute: it takes the lease for c when c doesn't hold
// it and the policy allows (action auto), or returns LEASE_HELD.
func (s *Session) acquireLocked(c api.Client) error {
	if s.lease.holder.ID == c.ID {
		return nil
	}

	if !s.lease.allows(opExec, c, false) {
		return s.leaseHeldLocked(c)
	}

	s.setHolderLocked(c, c.ID, "auto")

	return nil
}

// setHolderLocked moves the lease to holder (zero: nobody) and logs it.
func (s *Session) setHolderLocked(holder api.Client, actor, action string) {
	previous := s.lease.holder.ID
	s.lease.holder, s.lease.since = holder, time.Now()

	info := s.lease.info()
	s.log.append(api.Event{Kind: api.EventLease, Client: actor, Action: action, Lease: &info, Previous: previous})
}

// leaseHeldLocked is the error for c when another client holds the lease.
// It never suggests --force.
func (s *Session) leaseHeldLocked(c api.Client) error {
	holder := s.lease.holder.ID

	return api.NewError(api.CodeLeaseHeld,
		fmt.Sprintf("the lease of session %s is held by %s (policy %s)", s.ID, holder, s.lease.policy),
		fmt.Sprintf("ask %s to run 'eyedbg lease grant %s'; 'eyedbg events --wait --kind lease' waits for a change", holder, c.ID))
}
