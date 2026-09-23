// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestFileStore(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := NewFileStore(dir, 42)
	code := 3
	info := api.SessionInfo{
		ID: "s-ab12", Lang: "dotnet", Program: "/p/app.dll", State: api.StateExited,
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), ExitCode: &code, EndReason: "the program terminated",
		Recording: filepath.Join(dir, "s-ab12.jsonl"),
	}

	if err := st.Save(info); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(filepath.Join(dir, "s-ab12.json")); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("metadata mode = %v, %v; want 0600", fi.Mode(), err)
		}
	}

	// Files that aren't usable metadata are skipped.
	for name, body := range map[string]string{
		"s-v2.json":    `{"version":2,"id":"s-v2"}`,
		"s-bad.json":   `not json`,
		"s-other.json": `{"version":1,"id":"s-else"}`,
		"notes.txt":    `{}`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	lost, err := st.Lost()
	if err != nil || len(lost) != 1 || lost[0].ID != "s-ab12" || *lost[0].ExitCode != 3 || lost[0].Recording != info.Recording {
		t.Fatalf("Lost = %+v, %v; want only s-ab12 as saved", lost, err)
	}

	for range 2 { // idempotent
		if err := st.Remove("s-ab12"); err != nil {
			t.Fatal(err)
		}
	}

	if lost, _ := st.Lost(); len(lost) != 0 {
		t.Errorf("Lost after Remove = %+v", lost)
	}
}

func TestFileStoreRejectsOddIDs(t *testing.T) {
	t.Parallel()

	st := NewFileStore(t.TempDir(), 1)

	for _, id := range []string{"", "../x", "a/b", `a\b`, "S-UP", "s-1.json", string(make([]byte, 40))} {
		if err := st.Save(api.SessionInfo{ID: id}); err == nil {
			t.Errorf("Save(%q) succeeded", id)
		}

		if err := st.Remove(id); err == nil {
			t.Errorf("Remove(%q) succeeded", id)
		}

		if _, _, err := st.Record(id); err == nil {
			t.Errorf("Record(%q) succeeded", id)
		}
	}
}

func TestFileStoreRecordNeverReplaces(t *testing.T) {
	t.Parallel()

	st := NewFileStore(t.TempDir(), 1)

	w, path, err := st.Record("s-r1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.Write([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()

	if _, _, err := st.Record("s-r1"); err == nil {
		t.Error("a second Record of one id succeeded")
	}

	if b, err := os.ReadFile(path); err != nil || string(b) != "{}\n" {
		t.Errorf("recording = %q, %v; want it untouched", b, err)
	}
}

func TestPruneRecordings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	st := NewFileStore(dir, 1)
	now := time.Now()
	old := now.Add(-RecordingMaxAge - time.Hour)

	files := map[string]time.Time{
		"s-old.jsonl":  old, // ended long ago: pruned
		"s-new.jsonl":  now, // recent: kept
		"s-lost.jsonl": old, // a lost session's (metadata below): kept
		"s-lost.json":  old,
		"No_ID.jsonl":  old, // not a session id: kept
		"other.txt":    old,
	}

	for name, mtime := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.Prune(now, RecordingMaxAge); err != nil {
		t.Fatal(err)
	}

	for name := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		if gone := errors.Is(err, os.ErrNotExist); gone != (name == "s-old.jsonl") {
			t.Errorf("%s: gone = %v", name, gone)
		}
	}
}
