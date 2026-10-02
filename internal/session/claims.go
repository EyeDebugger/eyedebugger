// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strconv"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// claimKey names a process of a container.
func claimKey(containerID string, pid int) string { return containerID + "/" + strconv.Itoa(pid) }

// claim records that s debugs process pid of container id: INVALID_REQUEST
// naming the session when another live session already does, so two sessions
// never fight over one debuggee, even when they start at the same moment.
// The claim ends with s (see [Manager.watch]) or with [Manager.release].
func (m *Manager) claim(s *Session, containerID string, pid int) error {
	key := claimKey(containerID, pid)

	for {
		m.mu.Lock()

		holder, taken := m.claims[key]
		if !taken {
			m.claims[key] = s
			s.claimKey = key
			m.mu.Unlock()

			return nil
		}

		m.mu.Unlock()

		// A session locks itself after the manager, never under it.
		if holder.Info().State != api.StateExited {
			return api.NewError(api.CodeInvalidRequest,
				"process "+strconv.Itoa(pid)+" of container "+shortID(containerID)+" is already debugged by session "+holder.ID,
				"'eyedbg stop -s "+holder.ID+"' ends that session (or detaches from the container)")
		}

		// It ended and its claim isn't released yet: take it over.
		m.mu.Lock()
		if m.claims[key] == holder {
			delete(m.claims, key)
		}
		m.mu.Unlock()
	}
}

// release ends s's claim, if it has one.
func (m *Manager) release(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if s.claimKey != "" && m.claims[s.claimKey] == s {
		delete(m.claims, s.claimKey)
	}
}

// shortID is the first 12 characters of a container id.
func shortID(id string) string {
	const short = 12

	if len(id) > short {
		return id[:short]
	}

	return id
}
