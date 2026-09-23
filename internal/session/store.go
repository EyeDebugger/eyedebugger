// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"io"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Store keeps session metadata and recordings outside the daemon's memory,
// so the sessions of a daemon that crashed show up as lost in the next one
// (docs/DESIGN.md §6, §11).
type Store interface {
	// Save writes a live session's metadata, at start and when it ends.
	Save(info api.SessionInfo) error
	// Remove deletes a session's metadata; its recording stays.
	Remove(id string) error
	// Record opens a new recording for session id and returns where it is.
	Record(id string) (w io.WriteCloser, path string, err error)
	// Lost returns the metadata left behind by earlier daemons.
	Lost() ([]api.SessionInfo, error)
}
