// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Session files in the runtime directory's sessions/ (docs/DESIGN.md §6,
// §11): <id>.json is a live session's metadata, removed when the session is
// stopped or the daemon exits cleanly, so the metadata left there at start
// belongs to sessions a crashed daemon lost; <id>.jsonl is its recording.
const (
	metadataExt  = ".json"
	recordingExt = ".jsonl"
	// metadataVersion is the version of the metadata format; files of
	// other versions are ignored.
	metadataVersion = 1
	// RecordingMaxAge is how long recordings of ended sessions are kept.
	RecordingMaxAge = 7 * 24 * time.Hour
	// maxIDLength bounds the session ids the store accepts.
	maxIDLength = 32
)

// metadata is the on-disk form of a session's metadata (version 1).
type metadata struct {
	Version   int              `json:"version"`
	ID        string           `json:"id"`
	Lang      string           `json:"lang"`
	Program   string           `json:"program"`
	CreatedAt time.Time        `json:"createdAt"`
	DaemonPID int              `json:"daemonPid"`
	State     api.SessionState `json:"state"`
	ExitCode  *int             `json:"exitCode,omitempty"`
	EndReason string           `json:"endReason,omitempty"`
	Recording string           `json:"recording,omitempty"`
}

// FileStore keeps session metadata and recordings as files in one
// directory (session.Store). It is safe for concurrent use on different
// sessions. It only ever touches <id>.json and <id>.jsonl files there, for
// ids of lowercase letters, digits and dashes.
type FileStore struct {
	dir string
	pid int
}

// NewFileStore returns a store in dir, writing pid (the daemon's) into the
// metadata it saves.
func NewFileStore(dir string, pid int) *FileStore {
	return &FileStore{dir: dir, pid: pid}
}

// path returns the file of session id with ext, refusing ids that could
// name anything else.
func (f *FileStore) path(id, ext string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid session id %q", id)
	}

	return filepath.Join(f.dir, id+ext), nil
}

func validID(id string) bool {
	if id == "" || len(id) > maxIDLength {
		return false
	}

	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}

	return true
}

// Save writes info as id's metadata, atomically and readable only by the
// user.
func (f *FileStore) Save(info api.SessionInfo) error {
	path, err := f.path(info.ID, metadataExt)
	if err != nil {
		return err
	}

	b, err := json.Marshal(metadata{
		Version: metadataVersion, ID: info.ID, Lang: info.Lang, Program: info.Program, CreatedAt: info.CreatedAt,
		DaemonPID: f.pid, State: info.State, ExitCode: info.ExitCode, EndReason: info.EndReason, Recording: info.Recording,
	})
	if err != nil {
		return fmt.Errorf("encode session metadata: %w", err)
	}

	if err := writeFileAtomic(path, append(b, '\n')); err != nil {
		return fmt.Errorf("save session metadata: %w", err)
	}

	return nil
}

// Remove deletes id's metadata, if any; the recording stays.
func (f *FileStore) Remove(id string) error {
	path, err := f.path(id, metadataExt)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session metadata: %w", err)
	}

	return nil
}

// Record creates id's recording, readable only by the user. It never
// replaces an existing recording.
func (f *FileStore) Record(id string) (io.WriteCloser, string, error) {
	path, err := f.path(id, recordingExt)
	if err != nil {
		return nil, "", err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("create recording: %w", err)
	}

	return file, path, nil
}

// Lost returns the metadata in the directory: at a daemon's start, the
// sessions earlier daemons lost. Unreadable files and files of another
// format version are skipped.
func (f *FileStore) Lost() ([]api.SessionInfo, error) {
	entries, err := os.ReadDir(f.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read sessions directory: %w", err)
	}

	var out []api.SessionInfo

	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), metadataExt)
		if !ok || !validID(id) || !e.Type().IsRegular() {
			continue
		}

		if info, ok := f.read(id); ok {
			out = append(out, info)
		}
	}

	return out, nil
}

// read reads id's metadata; false if it can't be used.
func (f *FileStore) read(id string) (api.SessionInfo, bool) {
	b, err := os.ReadFile(filepath.Join(f.dir, id+metadataExt))
	if err != nil {
		return api.SessionInfo{}, false
	}

	var m metadata
	if err := json.Unmarshal(b, &m); err != nil || m.Version != metadataVersion || m.ID != id {
		return api.SessionInfo{}, false
	}

	return api.SessionInfo{
		ID: m.ID, Lang: m.Lang, Program: m.Program, State: m.State, CreatedAt: m.CreatedAt,
		ExitCode: m.ExitCode, EndReason: m.EndReason, Recording: m.Recording,
	}, true
}

// Prune deletes the recordings of ended sessions (no metadata left) last
// written more than maxAge before now. Nothing else is touched.
func (f *FileStore) Prune(now time.Time, maxAge time.Duration) error {
	entries, err := os.ReadDir(f.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("read sessions directory: %w", err)
	}

	var errs []error

	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), recordingExt)
		if !ok || !validID(id) || !e.Type().IsRegular() {
			continue
		}

		if _, err := os.Lstat(filepath.Join(f.dir, id+metadataExt)); !errors.Is(err, os.ErrNotExist) {
			continue // a lost session's (kept until it is forgotten), or unknown
		}

		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) <= maxAge {
			continue
		}

		if err := os.Remove(filepath.Join(f.dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("prune recordings: %w", err)
	}

	return nil
}
