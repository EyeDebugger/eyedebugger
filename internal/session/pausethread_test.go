// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

// TestPauseThreadBeforeFirstStop: a pause with no thread given, before any
// stop, asks the adapter for threads and pauses the one it lists (D2, F3):
// netcoredbg-like adapters refuse threadId 0.
func TestPauseThreadBeforeFirstStop(t *testing.T) {
	t.Parallel()

	drv := fakeDriver{opts: daptest.Options{StrictPause: true}}
	s := start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"hang"}}})

	waitRunning(t, s)

	snap := resume(t, s, agentC, ExecPause)
	expectStopped(t, snap, "pause", 10)

	if got := evalValue(t, s, "$threadsRequests"); got != "1" {
		t.Errorf("$threadsRequests = %s, want 1 (one threads request before the pause)", got)
	}
}

// TestPauseThreadStaleFallsBackToFirst: when the last stop's thread is no
// longer among the adapter's threads, pause uses the first one it lists
// (D2).
func TestPauseThreadStaleFallsBackToFirst(t *testing.T) {
	t.Parallel()

	drv := fakeDriver{opts: daptest.Options{StrictPause: true, Threads: []godap.Thread{{Id: 42, Name: "other"}}}}
	s := start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"hang"}}})

	waitRunning(t, s)

	// A first pause reports the fake's fixed thread (1), which is not
	// among Threads (42): the second pause's fallback must be exercised,
	// not that now-stale id.
	expectStopped(t, resume(t, s, agentC, ExecPause), "pause", 10)

	// Exec, not Resume: continuing a hung program never stops again on its
	// own, so waiting for a stop here would run to Resume's own timeout.
	if _, err := s.Exec(t.Context(), agentC, ExecContinue, 0); err != nil {
		t.Fatalf("continue: %v", err)
	}

	waitRunning(t, s)

	snap := resume(t, s, agentC, ExecPause)
	expectStopped(t, snap, "pause", 10)
}

// TestPauseThreadEmptyList: an empty threads answer falls back to today's
// choice (the last stop's thread, 0 before any stop), which a strict
// adapter refuses: ADAPTER_ERROR surfaces exactly as before this feature
// existed.
func TestPauseThreadEmptyList(t *testing.T) {
	t.Parallel()

	drv := fakeDriver{opts: daptest.Options{StrictPause: true, EmptyThreads: true}}
	s := start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"hang"}}})

	waitRunning(t, s)

	_, err := s.Resume(t.Context(), agentC, ExecPause, 0, testWait, api.DumpSpec{})
	expectCode(t, err, api.CodeAdapterFailed)
}

// TestPauseThreadExplicit: an explicit --thread never triggers a threads
// request: the caller's choice is sent as is.
func TestPauseThreadExplicit(t *testing.T) {
	t.Parallel()

	drv := fakeDriver{opts: daptest.Options{StrictPause: true}}
	s := start(t, newTestManagerWith(t, nil, drv), agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"hang"}}})

	waitRunning(t, s)

	snap, err := s.Resume(t.Context(), agentC, ExecPause, 1, testWait, api.DumpSpec{})
	if err != nil {
		t.Fatalf("pause --thread 1: %v", err)
	}

	expectStopped(t, snap, "pause", 10)

	if got := evalValue(t, s, "$threadsRequests"); got != "0" {
		t.Errorf("$threadsRequests = %s, want 0 (an explicit thread needs no threads request)", got)
	}
}

// TestPauseThreadUnsupported: a driver's PauseUnsupported refuses pause
// (TestPauseUnsupported) before the capability/state pre-check that would
// otherwise trigger a threads request (D2's pauseReady gate) — there is no
// stopped state to evaluate $threadsRequests from, so this is the same
// side-effect-free assertion TestPauseUnsupported already makes.
func TestPauseThreadUnsupported(t *testing.T) {
	t.Parallel()

	m := newTestManagerWith(t, nil, fakeDriver{knobs: Launch{AdapterName: "b", PauseUnsupported: pauseHint}})
	s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{Args: []string{"hang"}}})

	waitRunning(t, s)

	if _, err := s.Exec(t.Context(), humanC, ExecPause, 0); api.CodeOf(err) != api.CodeUnsupported {
		t.Fatalf("pause with PauseUnsupported: err = %v, want UNSUPPORTED", err)
	}

	if info := s.Info(); info.State != api.StateRunning {
		t.Errorf("after the refused pause: state = %s, want running", info.State)
	}
}
