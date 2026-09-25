// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
)

// TestAttach attaches to a running program, changes it, detaches, and
// checks it ran on with the change, through each adapter.
func TestAttach(t *testing.T) {
	forEachAdapter(t, attach)
}

func attach(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "breadth")
	src := filepath.Join(dir, "Program.cs")
	dll := buildApp(t, dir, "breadth")
	marker := filepath.Join(t.TempDir(), "go")

	host, err := dotnet.FindHost()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), host, dll, "wait", marker)

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	lines := bufio.NewScanner(out)
	if !lines.Scan() || !strings.HasPrefix(lines.Text(), "ready ") {
		t.Fatalf("program said %q, want ready PID", lines.Text())
	}

	pid, err := strconv.Atoi(strings.TrimPrefix(lines.Text(), "ready "))
	if err != nil {
		t.Fatal(err)
	}

	m := newManager(t)

	sess, err := m.Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, Attach: &api.AttachSpec{PID: pid},
		Breakpoints: []api.BreakpointSpec{{File: src, Anchor: "ticks++;"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", lineOf(t, src, "wait-tick"))

	if info := sess.Info(); info.Mode != api.ModeAttach || info.PID != pid || info.Adapter != adapter {
		t.Errorf("info = %+v, want attached to %d with %s", info, pid, adapter)
	}

	t.Logf("ticks when stopped: %s", e2eEval(t, sess, "ticks"))

	if _, err := sess.Set(t.Context(), agent, api.SetParams{Variable: "ticks", Value: "1000"}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := sess.RemoveBreakpoint(t.Context(), agent, 0, false); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Detach(t.Context(), agent, sess.ID); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	checkDetachedRun(t, cmd, lines)
}

// checkDetachedRun: the detached program finishes with the changed ticks.
func checkDetachedRun(t *testing.T, cmd *exec.Cmd, lines *bufio.Scanner) {
	t.Helper()

	done := ""
	for lines.Scan() {
		if strings.HasPrefix(lines.Text(), "done after ") {
			done = lines.Text()
		}
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("program after detach: %v", err)
	}

	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(done, "done after "), " ticks"))
	if err != nil || n < 1000 {
		t.Fatalf("program printed %q, want done after >= 1000 ticks", done)
	}
}
