// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"
	"time"
)

// Client kinds (docs/DESIGN.md §3).
const (
	KindAgent = "agent"
	KindHuman = "human"
	// DefaultClientID is who a request that names no client comes from. An
	// unidentified caller never gets a human's privileges.
	DefaultClientID = KindAgent
)

// maxClientName bounds the NAME part of a client id.
const maxClientName = 64

// Client identifies who sends a request: its kind, and an optional name
// telling several clients of one kind apart.
type Client struct {
	// ID is the canonical form: "agent", "agent:claude", "human:ijat".
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}

// IsHuman reports whether c is a human client.
func (c Client) IsHuman() bool { return c.Kind == KindHuman }

// ParseClient parses KIND[:NAME], where KIND is agent or human (lowercase)
// and NAME is 1 to 64 of A-Z a-z 0-9 . _ @ + -. An empty string is the
// default client, agent. Errors are INVALID_REQUEST *Errors.
func ParseClient(s string) (Client, error) {
	if s == "" {
		return Client{ID: DefaultClientID, Kind: KindAgent}, nil
	}

	kind, name, hasName := strings.Cut(s, ":")
	if (kind != KindAgent && kind != KindHuman) || (hasName && !validClientName(name)) {
		return Client{}, NewError(CodeInvalidRequest,
			fmt.Sprintf("invalid client %q: want agent, human, agent:NAME or human:NAME "+
				"(NAME: 1 to %d of letters, digits and . _ @ + -)", s, maxClientName),
			"set it with --as or EYEDBG_CLIENT")
	}

	return Client{ID: s, Kind: kind, Name: name}, nil
}

func validClientName(name string) bool {
	if name == "" || len(name) > maxClientName {
		return false
	}

	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("._@+-", r):
		default:
			return false
		}
	}

	return true
}

// ClientInfo is a client that has used a session, with when it was first
// and last seen there.
type ClientInfo struct {
	Client

	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}
