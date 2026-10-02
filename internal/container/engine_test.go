// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

// TestMain lets the test binary double as the fake docker.
func TestMain(m *testing.M) {
	containertest.MaybeRun()

	os.Exit(m.Run())
}

const fullID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestEngineArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		e    container.Engine
		want []string
	}{
		{"default", container.Engine{}, []string{"inspect", "x"}},
		{"context", container.Engine{Context: "desktop-linux"}, []string{"--context=desktop-linux", "inspect", "x"}},
		{"host", container.Engine{Host: "tcp://h:2376"}, []string{"--host=tcp://h:2376", "inspect", "x"}},
		{"host wins", container.Engine{Host: "tcp://h:2376", Context: "c"}, []string{"--host=tcp://h:2376", "inspect", "x"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.e.Args("inspect", "x"); !slices.Equal(got, tt.want) {
				t.Errorf("Args = %q, want %q", got, tt.want)
			}
		})
	}

	// Each value stays one argument, whatever it holds.
	e := container.Engine{Host: "tcp://h --privileged"}
	if got := e.Args("ps"); len(got) != 2 || got[0] != "--host=tcp://h --privileged" {
		t.Errorf("Args with spaces = %q", got)
	}

	got := container.Engine{Context: "c"}.ExecArgs(fullID, "/.eyedbg-x-1/x", "--interpreter=vscode")
	want := []string{"--context=c", "exec", "-i", fullID, "/.eyedbg-x-1/x", "--interpreter=vscode"}

	if !slices.Equal(got, want) {
		t.Errorf("ExecArgs = %q, want %q", got, want)
	}
}

func TestNewEngine(t *testing.T) { //nolint:paralleltest // Sets environment variables.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(container.EnvDocker, self)

	e, err := container.NewEngine("tcp://h:1", "")
	if err != nil || e.Docker != self || e.Host != "tcp://h:1" {
		t.Fatalf("NewEngine = %+v, %v", e, err)
	}

	for _, bad := range [][2]string{{"--host=x", ""}, {"", "-bad"}, {"a b://", ""}} {
		if _, err := container.NewEngine(bad[0], bad[1]); api.CodeOf(err) != api.CodeInvalidRequest {
			t.Errorf("NewEngine(%q, %q) err = %v, want INVALID_REQUEST", bad[0], bad[1], err)
		}
	}

	for _, v := range []string{"docker", filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		t.Setenv(container.EnvDocker, v)

		if _, err := container.NewEngine("", ""); api.CodeOf(err) != api.CodeAttachFailed {
			t.Errorf("NewEngine with %s=%q err = %v, want ATTACH_FAILED", container.EnvDocker, v, err)
		}
	}
}

func TestRunFailures(t *testing.T) {
	t.Parallel()

	t.Run("exit status and stderr", func(t *testing.T) {
		t.Parallel()

		e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{
			Match: []string{"inspect"}, Exit: 1, Stderr: "Error response\x1b[31m from\ndaemon\tx\r\n  y\n",
		}}})

		_, err := e.Inspect(t.Context(), "web")
		if api.CodeOf(err) != api.CodeAttachFailed {
			t.Fatalf("err = %v, want ATTACH_FAILED", err)
		}

		msg := err.Error()
		if !strings.Contains(msg, "Error response [31m from daemon x y") || strings.ContainsAny(msg, "\x1b\n\r\t") {
			t.Errorf("message %q: stderr not stripped of control characters", msg)
		}

		if !strings.Contains(msg, "exit status 1") {
			t.Errorf("message %q lacks the exit status", msg)
		}
	})

	t.Run("stderr is bounded", func(t *testing.T) {
		t.Parallel()

		e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{
			Match: []string{"inspect"}, Exit: 1, Stderr: strings.Repeat("x", 100_000) + " END",
		}}})

		_, err := e.Inspect(t.Context(), "web")
		if err == nil || len(err.Error()) > 1000 || !strings.HasSuffix(err.Error(), " END") {
			t.Errorf("err = %q (%d bytes): want a short message ending with the stderr's end", err, len(err.Error()))
		}
	})

	t.Run("stdout is bounded", func(t *testing.T) {
		t.Parallel()

		e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{
			Match: []string{"inspect"}, Flood: 2 << 20,
		}}})

		_, err := e.Inspect(t.Context(), "web")
		if api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "wrote more than") {
			t.Errorf("err = %v, want ATTACH_FAILED for too much output", err)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		t.Parallel()

		e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{Match: []string{"inspect"}, Hang: true}}})

		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()

		_, err := e.Inspect(ctx, "web")
		if api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "did not finish in time") {
			t.Errorf("err = %v, want ATTACH_FAILED for a docker that hangs", err)
		}
	})

	t.Run("docker missing", func(t *testing.T) {
		t.Parallel()

		e := container.Engine{Docker: filepath.Join(t.TempDir(), "no-such-docker")}

		_, err := e.Inspect(t.Context(), "web")
		if api.CodeOf(err) != api.CodeAttachFailed {
			t.Errorf("err = %v, want ATTACH_FAILED", err)
		}
	})
}
