// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"time"
)

// Lease methods (docs/DESIGN.md §3): who may drive a session's execution.
const (
	MethodLeaseStatus  = "lease.status"
	MethodLeaseTake    = "lease.take"
	MethodLeaseRelease = "lease.release"
	MethodLeaseGrant   = "lease.grant"
	MethodLeasePolicy  = "lease.policy"
)

// LeasePolicy decides whether a client may take the control lease from
// another.
type LeasePolicy string

// Lease policies.
const (
	// LeaseFree lets any client take the lease; executing takes it
	// automatically.
	LeaseFree LeasePolicy = "free"
	// LeaseHandoff moves the lease only when its holder releases or grants
	// it.
	LeaseHandoff LeasePolicy = "handoff"
	// LeaseHumanPriority is handoff, except that a human may take the lease
	// from an agent.
	LeaseHumanPriority LeasePolicy = "human-priority"
)

// LeasePolicies returns every policy, the default first.
func LeasePolicies() []LeasePolicy {
	return []LeasePolicy{LeaseFree, LeaseHandoff, LeaseHumanPriority}
}

// ParseLeasePolicy parses a policy name; "" is the default, free. Errors are
// INVALID_REQUEST *Errors.
func ParseLeasePolicy(s string) (LeasePolicy, error) {
	if s == "" {
		return LeaseFree, nil
	}

	for _, p := range LeasePolicies() {
		if string(p) == s {
			return p, nil
		}
	}

	return "", NewError(CodeInvalidRequest,
		fmt.Sprintf("invalid lease policy %q: want free, handoff or human-priority", s), "see 'eyedbg lease --help'")
}

// LeaseInfo is a session's control lease.
type LeaseInfo struct {
	Policy LeasePolicy `json:"policy"`
	// Holder is the client id holding the lease; empty: nobody.
	Holder string     `json:"holder,omitempty"`
	Since  *time.Time `json:"since,omitempty"`
}

// LeaseResult is the result of every lease method.
type LeaseResult struct {
	SessionID string    `json:"sessionId"`
	Lease     LeaseInfo `json:"lease"`
}

// LeaseParams are the params of [MethodLeaseStatus], [MethodLeaseTake] and
// [MethodLeaseRelease]. Force takes the lease whatever the policy.
type LeaseParams struct {
	SessionRef

	Force bool `json:"force,omitempty"`
}

// LeaseGrantParams are the params of [MethodLeaseGrant]: give the lease to
// another client.
type LeaseGrantParams struct {
	SessionRef

	To    string `json:"to"`
	Force bool   `json:"force,omitempty"`
}

// LeasePolicyParams are the params of [MethodLeasePolicy].
type LeasePolicyParams struct {
	SessionRef

	Policy LeasePolicy `json:"policy"`
	Force  bool        `json:"force,omitempty"`
}
