// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Touch records that c used the session: a client seen for the first time
// is added (and logged); a known one gets its last-seen time updated.
func (s *Session) Touch(c api.Client) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	for i := range s.clients {
		if s.clients[i].ID == c.ID {
			s.clients[i].LastSeen = now

			return
		}
	}

	s.clients = append(s.clients, api.ClientInfo{Client: c, FirstSeen: now, LastSeen: now})
	s.log.append(api.Event{Kind: api.EventClient, Client: c.ID})
}
