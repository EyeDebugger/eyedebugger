// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// buildDaemon compiles cmd/eyedbgd with testInfo's version and commit, so the
// spawned process is exactly what auto-start runs in production.
func buildDaemon(t *testing.T) string {
	t.Helper()

	if testing.Short() {
		t.Skip("builds and spawns eyedbgd")
	}

	exe := filepath.Join(t.TempDir(), "eyedbgd")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}

	const pkg = "github.com/eyedebugger/eyedebugger/internal/version"

	ldflags := "-X " + pkg + ".version=" + testInfo.Version + " -X " + pkg + ".commit=" + testInfo.Commit

	out, err := exec.CommandContext(t.Context(), "go", "build", "-o", exe, "-ldflags", ldflags, "../../cmd/eyedbgd").CombinedOutput()
	if err != nil {
		t.Fatalf("build eyedbgd: %v\n%s", err, out)
	}

	return exe
}

// stopDaemon stops whatever daemon owns p, if any.
func stopDaemon(t *testing.T, p Paths) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cl, err := Dial(ctx, p, testInfo)
	if err != nil {
		return
	}

	_ = cl.Call(ctx, api.MethodDaemonStop, api.StopParams{Force: true}, nil)
	_ = cl.Close()

	if err := WaitStopped(ctx, p, 10*time.Second); err != nil {
		t.Errorf("daemon did not stop: %v", err)
	}
}

func TestConnectAutostart(t *testing.T) {
	t.Parallel()

	exe := buildDaemon(t)
	p := PathsIn(shortTempDir(t))
	opts := StartOptions{Executable: exe}

	t.Cleanup(func() { stopDaemon(t, p) })

	cl, started, err := Connect(t.Context(), p, testInfo, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = cl.Close()

	if !started {
		t.Error("first Connect: started = false, want true")
	}

	cl2, started, err := Connect(t.Context(), p, testInfo, opts)
	if err != nil {
		t.Fatal(err)
	}
	_ = cl2.Close()

	if started || cl2.Hello.PID != cl.Hello.PID {
		t.Errorf("second Connect: started = %v, pid %d -> %d; want the same daemon reused", started, cl.Hello.PID, cl2.Hello.PID)
	}
}

func TestConcurrentConnectsShareOneDaemon(t *testing.T) {
	t.Parallel()

	exe := buildDaemon(t)
	p := PathsIn(shortTempDir(t))

	t.Cleanup(func() { stopDaemon(t, p) })

	const n = 5

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		pids = make(map[int]bool)
	)

	for range n {
		wg.Go(func() {
			cl, _, err := Connect(t.Context(), p, testInfo, StartOptions{Executable: exe})
			if err != nil {
				t.Error(err)

				return
			}
			_ = cl.Close()

			mu.Lock()
			pids[cl.Hello.PID] = true
			mu.Unlock()
		})
	}

	wg.Wait()

	if len(pids) != 1 {
		t.Errorf("%d concurrent Connects reached daemons %v, want exactly one", n, pids)
	}

	// The daemons that lost the race exited quietly (every Connect waited
	// for its own spawn to exit).
	if log, err := os.ReadFile(p.Log); err != nil || strings.Contains(string(log), "already running") {
		t.Errorf("daemon log (%v):\n%s\nwant no start-race errors", err, log)
	}
}

func TestConnectReplacesOtherVersion(t *testing.T) {
	t.Parallel()

	exe := buildDaemon(t)
	p := PathsIn(shortTempDir(t))

	t.Cleanup(func() { stopDaemon(t, p) })

	cl, _, err := Connect(t.Context(), p, testInfo, StartOptions{Executable: exe})
	if err != nil {
		t.Fatal(err)
	}
	_ = cl.Close()

	// A newer client: the idle old daemon is stopped and a new one started.
	// The only eyedbgd available is the old build, so the result is a
	// mismatch against a fresh process, not a silent reuse of the old one.
	newer := version.Info{Version: "v9.9.9", Commit: "fffffff"}

	_, _, err = Connect(t.Context(), p, newer, StartOptions{Executable: exe})
	if got := api.CodeOf(err); got != api.CodeVersionMismatch {
		t.Fatalf("code = %q (err = %v), want %q", got, err, api.CodeVersionMismatch)
	}

	cl2, err := Dial(t.Context(), p, testInfo)
	if err != nil {
		t.Fatal(err)
	}
	_ = cl2.Close()

	if cl2.Hello.PID == cl.Hello.PID {
		t.Errorf("old daemon (pid %d) still running; want it replaced", cl.Hello.PID)
	}
}
