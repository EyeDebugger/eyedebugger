// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"strconv"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// launchClaimSuffix ends the key of a container's launch claim, where an
// attach's ends in a process id.
const launchClaimSuffix = "launch"

// claimKey names a process of a container.
func claimKey(containerID string, pid int) string { return containerID + "/" + strconv.Itoa(pid) }

// launchClaimKey names a container's launched app: the whole container.
func launchClaimKey(containerID string) string { return containerID + "/" + launchClaimSuffix }

// claimedBy is one claim a start found in its way.
type claimedBy struct {
	key    string
	holder *Session
}

// claim records that s debugs process pid of container id: INVALID_REQUEST
// naming the session when another live session already does, or when a
// session launched the container's app, so two sessions never fight over one
// debuggee, even when they start at the same moment. The claim ends with s
// (see [Manager.watch]) or with [Manager.release].
func (m *Manager) claim(s *Session, containerID string, pid int) error {
	key := claimKey(containerID, pid)

	return m.take(s, containerID, key, func(k string) bool { return k == key || k == launchClaimKey(containerID) },
		func(c claimedBy) *api.Error {
			if c.key == key {
				return api.NewError(api.CodeInvalidRequest,
					"process "+strconv.Itoa(pid)+" of container "+shortID(containerID)+" is already debugged by session "+c.holder.ID,
					"'eyedbg stop -s "+c.holder.ID+"' ends that session (or detaches from the container)")
			}

			return api.NewError(api.CodeInvalidRequest,
				"container "+shortID(containerID)+" runs an app launched by session "+c.holder.ID+": attaching to it would fight that debugger",
				"'eyedbg stop -s "+c.holder.ID+"' ends that session")
		})
}

// claimLaunch records that s launched the app of container id: INVALID_REQUEST
// naming the session when any live session already debugs the container
// (a launch, or an attach to any of its processes). A container holds either
// one launch claim or any number of attach claims with distinct pids.
func (m *Manager) claimLaunch(s *Session, containerID string) error {
	return m.take(s, containerID, launchClaimKey(containerID), func(string) bool { return true },
		func(c claimedBy) *api.Error {
			return api.NewError(api.CodeInvalidRequest,
				"container "+shortID(containerID)+" is already debugged by session "+c.holder.ID,
				"'eyedbg stop -s "+c.holder.ID+"' ends that session")
		})
}

// take gives s the claim key on container id unless a live session holds a
// claim of that container for which conflicts (given the claim's key) says
// so, then it is refused with refusal. A holder that already ended
// loses its claim to s. The check and the insertion are one step under m.mu.
func (m *Manager) take(s *Session, containerID, key string, conflicts func(key string) bool, refusal func(claimedBy) *api.Error) error {
	prefix := containerID + "/"

	for {
		m.mu.Lock()

		var found []claimedBy

		for k, h := range m.claims {
			if strings.HasPrefix(k, prefix) && conflicts(k) {
				found = append(found, claimedBy{k, h})
			}
		}

		if len(found) == 0 {
			m.claims[key] = s
			s.claimKey = key
			m.mu.Unlock()

			return nil
		}

		m.mu.Unlock()

		// A session locks itself after the manager, never under it.
		for _, c := range found {
			if c.holder.Info().State != api.StateExited {
				return refusal(c)
			}
		}

		// They all ended and their claims aren't released yet: take them over.
		m.mu.Lock()
		for _, c := range found {
			if m.claims[c.key] == c.holder {
				delete(m.claims, c.key)
			}
		}
		m.mu.Unlock()
	}
}

// liveClaimOn is the live session whose claim is on the container ref names
// (its name, or an id prefix of 12 or more characters), or nil: a start
// checks it before its driver runs, so a container that is already debugged
// is refused without inspecting it or copying into it. The claim taken once
// the driver knows the full id decides; this only spares the work.
func (m *Manager) liveClaimOn(ref string) *Session {
	const minIDPrefix = 12

	m.mu.Lock()

	var candidates []*Session

	for _, h := range m.claims {
		if c := h.container; c != nil && (c.Name == ref || (len(ref) >= minIDPrefix && strings.HasPrefix(c.ID, ref))) {
			candidates = append(candidates, h)
		}
	}
	m.mu.Unlock()

	for _, h := range candidates {
		if h.Info().State != api.StateExited {
			return h
		}
	}

	return nil
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
