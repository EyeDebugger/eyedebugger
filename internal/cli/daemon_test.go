// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestDaemonStatusOutput(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	exitAt := started.Add(30 * time.Minute)
	now := started.Add(3*time.Minute + 2*time.Second)
	st := &api.StatusResult{
		PID:         4242,
		Version:     "v0.1.0",
		Commit:      "abc1234",
		StartedAt:   started,
		Dir:         "/run/user/1000/eyedbg",
		Socket:      "/run/user/1000/eyedbg/eyedbgd.sock",
		IdleTimeout: api.Duration(30 * time.Minute),
		IdleExitAt:  &exitAt,
	}
	noIdle := *st
	noIdle.IdleTimeout = 0
	noIdle.IdleExitAt = nil

	tests := []struct {
		name   string
		view   statusView
		json   bool
		golden string
	}{
		{"not running", statusView{}, false, "daemon_status_stopped.golden"},
		{"not running json", statusView{}, true, "daemon_status_stopped_json.golden"},
		{"running", newStatusView(st, false, now), false, "daemon_status.golden"},
		{"running json", newStatusView(st, false, now), true, "daemon_status_json.golden"},
		{"started", newStatusView(st, true, now), false, "daemon_start.golden"},
		{"idle disabled", newStatusView(&noIdle, false, now), false, "daemon_status_no_idle.golden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			if err := writeDaemonStatus(&out, tt.view, tt.json); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), out.Bytes())
		})
	}
}

func TestDaemonStopOutput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		out    stopOutput
		json   bool
		golden string
	}{
		{"stopped", stopOutput{Stopped: true, WasRunning: true}, false, "daemon_stop.golden"},
		{"stopped json", stopOutput{Stopped: true, WasRunning: true}, true, "daemon_stop_json.golden"},
		{"not running", stopOutput{}, false, "daemon_stop_not_running.golden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			if err := writeStop(&out, tt.out, tt.json); err != nil {
				t.Fatal(err)
			}

			assertGolden(t, filepath.Join("testdata", tt.golden), out.Bytes())
		})
	}
}

func TestDaemonLogsOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	if err := writeLogs(&out, "/x/eyedbgd.log", nil, true); err != nil {
		t.Fatal(err)
	}

	if got, want := out.String(), `{"schema":1,"path":"/x/eyedbgd.log","lines":[]}`+"\n"; got != want {
		t.Errorf("empty logs --json = %q, want %q", got, want)
	}
}

func TestFormatError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"plain", errors.New("boom"), "eyedbg: boom\n"},
		{
			"api error with hint",
			api.NewError(api.CodeSessionsActive, "2 debug session(s) are active", "use --force"),
			"eyedbg: 2 debug session(s) are active [SESSIONS_ACTIVE]\nhint: use --force\n",
		},
		{
			"joined api error hides the low-level cause",
			errors.Join(api.NewError(api.CodeDaemonNotRunning, "not running", ""), errors.New("dial unix: no such file")),
			"eyedbg: not running [DAEMON_NOT_RUNNING]\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := formatError("eyedbg", tt.err); got != tt.want {
				t.Errorf("formatError = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDaemonStatusNotRunning runs the real command against an empty runtime
// directory. Not parallel: it sets EYEDBG_RUNTIME_DIR.
func TestDaemonStatusNotRunning(t *testing.T) {
	t.Setenv("EYEDBG_RUNTIME_DIR", t.TempDir())

	for _, args := range [][]string{{"daemon", "status"}, {"daemon", "stop"}} {
		stdout, stderr, code := execute(t, NewEyedbgCommand(testInfo), args)
		if code != 0 || string(stdout) != "eyedbgd is not running\n" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want exit 0 and \"not running\"", args, code, stdout, stderr)
		}
	}
}
