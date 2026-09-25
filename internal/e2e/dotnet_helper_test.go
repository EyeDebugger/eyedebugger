// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
)

// The .NET side helper's end-to-end tests (docs/adr/0016): the real helper,
// published next to the eyedbg binariesFor built (the default lookup, not
// $EYEDBG_DOTNET_HELPER), against real .NET processes: testdata/apps/dotnet/breadth
// in its 'wait' and 'hold' modes.

// helperOnce guards the one helper publish and breadth build every test
// shares; both land in binDir, removed by TestMain.
var (
	helperOnce sync.Once
	errHelper  error
	breadthDLL string
)

// requireDotnetSDK skips t without EYEDBG_E2E=1 (or with dotnet left out of
// EYEDBG_E2E_LANGS) and fails it without a dotnet host: never a silent
// skip. It then publishes the helper next to eyedbg and builds the sample,
// once, and returns the dotnet host and eyedbg's harness.
func requireDotnetSDK(t *testing.T) (host string, h *harness) {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 (needs the .NET 10 SDK)")
	}

	requireLang(t, dotnet.Language)

	host, err := dotnet.FindHost()
	if err != nil {
		t.Fatalf("the .NET helper e2e needs the .NET SDK: %v", err)
	}

	h = newHarness(t) // builds eyedbg into binDir first

	helperOnce.Do(func() { errHelper = prepareHelper(host) })

	if errHelper != nil {
		t.Fatal(errHelper)
	}

	return host, h
}

// prepareHelper publishes the helper to binDir/helpers/dotnet (the release
// archive's layout) and builds breadth into binDir/breadth.
func prepareHelper(host string) error {
	src, err := filepath.Abs(filepath.Join("..", "..", "helpers", "dotnet"))
	if err != nil {
		return fmt.Errorf("locate helpers/dotnet: %w", err)
	}

	ctx := context.Background()

	//nolint:gosec // The dotnet host FindHost found; fixed arguments.
	publish := exec.CommandContext(ctx, host, "publish", filepath.Join("src", "EyeDbg.DotnetHelper", "EyeDbg.DotnetHelper.csproj"),
		"-c", "Release", "-p:RestoreLockedMode=true", "-nologo", "-o", filepath.Join(binDir, "helpers", "dotnet"))
	publish.Dir = src // helpers/dotnet/global.json picks the SDK
	publish.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1")

	if out, err := publish.CombinedOutput(); err != nil {
		return fmt.Errorf("dotnet publish the helper: %w\n%s", err, out)
	}

	app := filepath.Join(binDir, "breadth")
	if err := os.CopyFS(app, os.DirFS(filepath.Join("..", "..", "testdata", "apps", "dotnet", "breadth"))); err != nil {
		return fmt.Errorf("copy breadth: %w", err)
	}

	for _, d := range []string{"bin", "obj"} { // a build in the source tree is not the test's
		if err := os.RemoveAll(filepath.Join(app, d)); err != nil {
			return fmt.Errorf("clean breadth: %w", err)
		}
	}

	build := exec.CommandContext(ctx, host, "build", app, "-c", "Debug", "-nologo")
	build.Env = publish.Env

	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("dotnet build breadth: %w\n%s", err, out)
	}

	breadthDLL = filepath.Join(app, "bin", "Debug", "net10.0", "breadth.dll")

	return nil
}

// startWaiting runs 'dotnet breadth.dll wait MARKER' with extra env and
// returns its pid once it said "ready"; it is killed when the test ends.
func startWaiting(t *testing.T, host string, env ...string) int {
	t.Helper()

	return startBreadth(t, host, "wait", env...)
}

// startBreadth runs 'dotnet breadth.dll SCENARIO MARKER' (wait, hold) with
// extra env and returns its pid once it said "ready"; it is killed when the
// test ends.
func startBreadth(t *testing.T, host, scenario string, env ...string) int {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), host, breadthDLL, scenario, filepath.Join(t.TempDir(), "never"))
	cmd.Env = append(os.Environ(), env...)

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
		t.Fatalf("breadth said %q, want ready PID", lines.Text())
	}

	pid, err := strconv.Atoi(strings.TrimPrefix(lines.Text(), "ready "))
	if err != nil {
		t.Fatal(err)
	}

	return pid
}

// countersJSON is 'eyedbg dotnet counters --json's summary.
type countersJSON struct {
	Schema int `json:"schema"`
	Target struct {
		PID     int    `json:"pid"`
		Name    string `json:"name"`
		Session string `json:"session"`
	} `json:"target"`
	Samples   int    `json:"samples"`
	EndReason string `json:"endReason"`
	Counters  []struct {
		Name  string   `json:"name"`
		Kind  string   `json:"kind"`
		Last  *float64 `json:"last"`
		Total *float64 `json:"total"`
	} `json:"counters"`
}

// checkCounters checks a summary of a live .NET process's counters.
func checkCounters(t *testing.T, got countersJSON, pid int) {
	t.Helper()

	if got.Schema != 1 || got.Target.PID != pid || got.Samples < 2 || got.EndReason != "duration" {
		t.Fatalf("summary = %+v, want pid %d, at least 2 samples, endReason duration", got, pid)
	}

	seen := map[string]bool{}

	for _, c := range got.Counters {
		seen[c.Name] = true

		if c.Name == "working-set" && (c.Last == nil || *c.Last <= 0) {
			t.Errorf("working-set = %+v, want a positive last value", c)
		}
	}

	for _, want := range []string{"cpu-usage", "working-set", "gc-heap-size", "exception-count", "threadpool-thread-count"} {
		if !seen[want] {
			t.Errorf("no %s counter in %+v", want, got.Counters)
		}
	}
}

func TestDotnetHelperPsAndCounters(t *testing.T) {
	t.Parallel()

	host, h := requireDotnetSDK(t)
	pid := startWaiting(t, host)

	var ps struct {
		Schema    int `json:"schema"`
		Processes []struct {
			PID  int    `json:"pid"`
			Name string `json:"name"`
		} `json:"processes"`
	}

	h.run("dotnet", "ps", "--json").wantCode(0).decode(&ps)

	found := false

	for _, p := range ps.Processes {
		found = found || (p.PID == pid && p.Name != "")
	}

	if ps.Schema != 1 || !found {
		t.Errorf("dotnet ps = %+v, want pid %d with its name", ps, pid)
	}

	var got countersJSON

	h.run("dotnet", "counters", "--pid", strconv.Itoa(pid), "--duration", "3s", "--json").wantCode(0).decode(&got)
	checkCounters(t, got, pid)

	h.run("dotnet", "counters", "--pid", strconv.Itoa(pid), "--duration", "2s").wantCode(0).
		wantStdout(fmt.Sprintf("pid %d (", pid))
}

func TestDotnetHelperWatch(t *testing.T) {
	t.Parallel()

	host, h := requireDotnetSDK(t)
	pid := startWaiting(t, host)

	r := h.run("dotnet", "counters", "--pid", strconv.Itoa(pid), "--watch", "--duration", "3s", "--json").wantCode(0)

	lines := strings.Split(strings.TrimSpace(string(r.stdout)), "\n")
	if len(lines) < 2 {
		t.Fatalf("watch printed %d lines, want at least 2:\n%s", len(lines), r.stdout)
	}

	for _, l := range lines {
		var s struct {
			Schema    int               `json:"schema"`
			ElapsedMs int64             `json:"elapsedMs"`
			Counters  []json.RawMessage `json:"counters"`
		}

		if err := json.Unmarshal([]byte(l), &s); err != nil || s.Schema != 1 || s.ElapsedMs <= 0 || len(s.Counters) == 0 {
			t.Errorf("watch line %q: %+v, %v", l, s, err)
		}
	}
}

func TestDotnetHelperTargetErrors(t *testing.T) {
	t.Parallel()

	host, h := requireDotnetSDK(t)

	// This test process is Go: no .NET diagnostics endpoint (on Windows the
	// helper's pipe pre-check answers at once).
	began := time.Now()

	h.run("dotnet", "counters", "--pid", strconv.Itoa(os.Getpid())).wantCode(2).wantStderr("[NOT_DOTNET]")

	if d := time.Since(began); d > 4*time.Second {
		t.Errorf("NOT_DOTNET took %v; a missing endpoint should be told at once", d)
	}

	disabled := startWaiting(t, host, "DOTNET_EnableDiagnostics=0")
	h.run("dotnet", "counters", "--pid", strconv.Itoa(disabled)).wantCode(2).wantStderr("[DIAGNOSTICS_DISABLED]")

	h.run("dotnet", "counters", "--pid", "999999999").wantCode(1).wantStderr("[INVALID_REQUEST]")
}

// TestDotnetHelperSession samples a session's program: refused while it is
// stopped at a breakpoint, a timeout by --pid then, and fine once it runs
// again (the stopped runtime recovered).
func TestDotnetHelperSession(t *testing.T) {
	t.Parallel()

	_, h := requireDotnetSDK(t)
	requireDotnetE2E(t)

	src := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(breadthDLL)))), "Program.cs")
	marker := filepath.Join(t.TempDir(), "never")

	var start snapshotEnvelope

	h.run("start", "dotnet", "--program", breadthDLL, "--bp", src+`@"ticks++;"`, "--timeout", startTimeout, "--json",
		"--", "wait", marker).wantCode(0).decode(&start)

	id, pid := start.Session.ID, start.Session.PID
	if start.Session.State != "stopped" || pid <= 0 {
		t.Fatalf("start = %+v, want stopped at the breakpoint with a pid", start.Session)
	}

	h.run("dotnet", "counters").wantCode(2).wantStderr("is stopped (breakpoint)")

	began := time.Now()

	h.run("dotnet", "counters", "--pid", strconv.Itoa(pid)).wantCode(2).wantStderr("[DIAGNOSTICS_TIMEOUT]")

	if d := time.Since(began); d > 30*time.Second {
		t.Errorf("DIAGNOSTICS_TIMEOUT took %v, want about 5s", d)
	}

	h.run("bp", "rm", "all").wantCode(0)
	h.run("continue", "--timeout", "1s").wantCode(0)

	var got countersJSON

	h.run("dotnet", "counters", "--duration", "3s", "--json").wantCode(0).decode(&got)
	checkCounters(t, got, pid)

	if got.Target.Session != id {
		t.Errorf("target = %+v, want session %s", got.Target, id)
	}

	h.run("stop").wantCode(0)
}
