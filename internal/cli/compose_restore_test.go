// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// restoreDoc is 'compose restore --json'.
type restoreDoc struct {
	Schema  int              `json:"schema"`
	Group   string           `json:"group"`
	Members []restoredMember `json:"members"`
	Removed bool             `json:"removed"`
	Kept    string           `json:"kept"`
}

func (w *launchWorld) restoreJSON(wantCode int, args ...string) restoreDoc {
	w.t.Helper()

	var doc restoreDoc

	out, _ := w.run(wantCode, append([]string{"compose", "restore", "--json"}, args...)...)
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Schema != 1 {
		w.t.Fatalf("compose restore --json: %v\n%s", err, out)
	}

	return doc
}

// TestComposeRestoreFailure: a recreate that fails leaves the service in fast
// mode, with its files; the session was ended first; nothing is pruned.
func TestComposeRestoreFailure(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer", "consumer")

	w.docker.upErr = func(c upCall) error {
		if slices.Contains(c.services, "producer") {
			return api.NewError(api.CodeAttachFailed, "docker compose up failed: container is unhealthy", "")
		}

		return nil
	}

	doc := w.restoreJSON(0, "consumer")
	if len(doc.Members) != 1 || !doc.Members[0].Restored {
		t.Fatalf("restore consumer = %+v", doc)
	}

	doc = w.restoreJSON(exitCodeOf(api.CodeAttachFailed), "producer")

	if m := doc.Members[0]; m.Restored || m.Error == nil || m.Error.Code != api.CodeAttachFailed || len(m.Sessions) != 1 {
		t.Errorf("producer = %+v", m)
	}

	if doc.Removed {
		t.Error("the project's files were removed although producer is still in fast mode")
	}

	if c := w.docker.byService("producer"); c.fi.Fast == nil {
		t.Error("producer left fast mode although its recreate failed")
	}

	for _, p := range []string{w.override(), w.serviceDir("producer")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("pruned after a failed recreate: %v", err)
		}
	}
}

// TestComposeRestoreLeaseHeld: a session that refuses to end keeps its service
// as it is.
func TestComposeRestoreLeaseHeld(t *testing.T) {
	w := newLaunchWorld(t)

	w.run(0, "--as", "human:bob", "compose", "launch", "--lease-policy", "human-priority", "producer")
	w.enterNamed("consumer")

	before := w.snap()

	doc := w.restoreJSON(0)

	var producer, consumer restoredMember

	for _, m := range doc.Members {
		switch m.Service {
		case "producer":
			producer = m
		case "consumer":
			consumer = m
		}
	}

	if producer.Restored || producer.Error == nil || producer.Error.Code != api.CodeLeaseHeld || !consumer.Restored {
		t.Errorf("producer = %+v, consumer = %+v", producer, consumer)
	}

	after := w.snap()
	if after.ids["producer"] != before.ids["producer"] || after.sessions["producer"] != before.sessions["producer"] {
		t.Errorf("producer changed: %+v -> %+v", before, after)
	}

	if len(w.docker.upCalls()) != 3 || slices.Contains(w.docker.upCalls()[2].services, "producer") {
		t.Errorf("compose up calls = %+v", w.docker.upCalls())
	}
}

// TestComposeRestoreStoppedAndNamed: containers in any state are found (a
// stack that is down is named with -p, and docker compose ps isn't asked), a
// named service that isn't in fast mode is skipped, and there is something to
// say when no project can be found.
func TestComposeRestoreStoppedAndNamed(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer")

	w.docker.byService("producer").fi.Running = false

	psBefore := w.psCalls

	doc := w.restoreJSON(0, "-p", worldProject, "producer", "consumer")
	if w.psCalls != psBefore {
		t.Error("restore -p asked docker compose ps")
	}

	if len(doc.Members) != 2 || !doc.Members[1].Restored || doc.Members[0].Skipped != "not in fast mode" || doc.Members[0].Service != "consumer" {
		t.Errorf("members = %+v", doc.Members)
	}

	if !doc.Removed || doc.Kept != "" {
		t.Errorf("removed = %v, kept = %q: nothing is in fast mode any more, yet eyedbg's files are there", doc.Removed, doc.Kept)
	}

	if _, err := os.Stat(filepath.Dir(w.override())); !os.IsNotExist(err) {
		t.Errorf("eyedbg's project directory after a restore that handled the last fast container: %v", err)
	}

	// Nothing running and no -p: nothing names the project.
	for _, c := range w.docker.ctrs {
		c.fi.Running = false
	}

	_, errOut := w.run(exitError, "compose", "restore")
	expectOutput(t, errOut, "[INVALID_REQUEST]", "name the project with -p NAME")

	_, errOut = w.run(exitError, "compose", "restore", "-p", worldProject, "consumer")
	expectOutput(t, errOut, "nothing is in fast mode")

	_, errOut = w.run(exitError, "compose", "restore", "Bad Name")
	expectOutput(t, errOut, "invalid service")
}

// TestComposeRestoreOptions: --env-file reaches the recreate, which waits for
// health; no compose files recorded fails that service only.
func TestComposeRestoreOptions(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer", "consumer")

	w.docker.byService("consumer").fi.ConfigFiles = nil

	doc := w.restoreJSON(0, "--env-file", "prod.env")

	for _, m := range doc.Members {
		switch m.Service {
		case "producer":
			if !m.Restored {
				t.Errorf("producer = %+v", m)
			}
		case "consumer":
			if m.Restored || m.Error == nil || m.Error.Code != api.CodeInvalidRequest {
				t.Errorf("consumer = %+v", m)
			}
		}
	}

	ups := w.docker.upCalls()
	if last := ups[len(ups)-1]; !last.wait || !slices.Equal(last.ref.EnvFiles, []string{"prod.env"}) || !slices.Equal(last.services, []string{"producer"}) {
		t.Errorf("restore's compose up = %+v", last)
	}
}

// TestComposeRestoreKeepsFilesWhenNoFastContainerIsSeen: 'restore -p NAME' asks
// no compose ps, so on the wrong engine (another DOCKER_CONTEXT, a remote host)
// the project has no fast-mode container to see, though the real ones still
// mount eyedbg's directory for it. It must stay, with a note saying so.
func TestComposeRestoreKeepsFilesWhenNoFastContainerIsSeen(t *testing.T) {
	for _, args := range [][]string{{}, {"consumer"}} {
		w := newLaunchWorld(t)
		t.Setenv(envDockerContext, "elsewhere")

		dir := filepath.Dir(w.override())
		build := filepath.Join(dir, "services", "producer", "App.dll")

		if err := os.MkdirAll(filepath.Dir(build), 0o700); err != nil {
			t.Fatal(err)
		}

		for _, f := range []string{build, w.override()} {
			if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		cmd := append([]string{"compose", "restore", "-p", worldProject}, args...)
		out, errOut := w.run(exitError, cmd...)
		expectOutput(t, out, "no fast-mode container of project "+worldProject+" was found on docker context elsewhere", "were kept: "+dir)
		expectOutput(t, errOut, "nothing is in fast mode")

		for _, f := range []string{build, w.override()} {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("restore %v on an engine without the containers removed %s: %v", args, f, err)
			}
		}

		doc := w.restoreJSON(exitError, append([]string{"-p", worldProject}, args...)...)
		if doc.Removed || doc.Kept != dir {
			t.Errorf("restore %v --json: removed = %v, kept = %q; want kept = %q", args, doc.Removed, doc.Kept, dir)
		}
	}
}

// TestComposeRestoreRefusesUncorroboratedLabels: restore recreates from the
// directory and files a container's labels name, which an image's own LABELs
// can set on a container made by plain 'docker run': a service whose files
// aren't in its directory, or whose directory isn't the one the flags name,
// fails alone and its container is left as it is.
func TestComposeRestoreRefusesUncorroboratedLabels(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer", "consumer")

	consumer := w.docker.byService("consumer")
	consumer.fi.ComposeDir = realDirWith(t, "id_rsa")

	doc := w.restoreJSON(0)

	for _, m := range doc.Members {
		switch m.Service {
		case "producer":
			if !m.Restored {
				t.Errorf("producer = %+v", m)
			}
		case "consumer":
			if m.Restored || m.Error == nil || m.Error.Code != api.CodeInvalidRequest || !strings.Contains(m.Error.Message, "compose labels don't check out") {
				t.Errorf("consumer = %+v", m)
			}
		}
	}

	for _, up := range w.docker.upCalls() {
		if up.ref.WorkDir == consumer.fi.ComposeDir {
			t.Errorf("a compose up ran in the directory a label named: %+v", up)
		}
	}
}

func TestComposeRestoreProjectDirectoryFlag(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer")

	doc := w.restoreJSON(exitCodeOf(api.CodeInvalidRequest), "--project-directory", realDirWith(t))
	if len(doc.Members) != 1 || doc.Members[0].Restored || doc.Members[0].Error == nil ||
		!strings.Contains(doc.Members[0].Error.Message, "the command line names") {
		t.Errorf("members = %+v", doc.Members)
	}

	if ups := w.docker.upCalls(); len(ups) != 1 {
		t.Errorf("compose up ran after the refusal: %+v", ups[1:])
	}

	doc = w.restoreJSON(0, "--project-directory", w.dir)
	if len(doc.Members) != 1 || !doc.Members[0].Restored {
		t.Errorf("with the right directory: %+v", doc.Members)
	}
}
