// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

func topEngine(t *testing.T, r containertest.Rule) (eng container.Engine, calls string) {
	t.Helper()

	calls = filepath.Join(t.TempDir(), "calls")
	r.Match = []string{"top"}

	return containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{r}}), calls
}

func TestTop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		out  string
		want []container.Proc
		bad  bool
	}{
		{
			name: "docker 24 on Docker Desktop",
			out:  "PID                 STAT                COMMAND\n57290               Ss                  docker-init\n57322               S                   tail\n",
			want: []container.Proc{{PID: 57290, Stat: "Ss", Comm: "docker-init"}, {PID: 57322, Stat: "S", Comm: "tail"}},
		},
		{
			name: "during a launch",
			out:  "PID STAT COMMAND\n1 Ss docker-init\n20 Ssl netcoredbg\n22 Sl dotnet\n",
			want: []container.Proc{{PID: 1, Stat: "Ss", Comm: "docker-init"}, {PID: 20, Stat: "Ssl", Comm: "netcoredbg"}, {PID: 22, Stat: "Sl", Comm: "dotnet"}},
		},
		{name: "lowercase header", out: "pid stat comm\n5 S tail\n", want: []container.Proc{{PID: 5, Stat: "S", Comm: "tail"}}},
		{name: "header with CMD", out: "PID STAT CMD\n5 S tail\n", want: []container.Proc{{PID: 5, Stat: "S", Comm: "tail"}}},
		{name: "no processes", out: "PID STAT COMMAND\n", want: []container.Proc{}},
		{name: "crlf", out: "PID STAT COMMAND\r\n5 S tail\r\n", want: []container.Proc{{PID: 5, Stat: "S", Comm: "tail"}}},
		{name: "a name with spaces", out: "PID STAT COMMAND\n5 S my proc\n", want: []container.Proc{{PID: 5, Stat: "S", Comm: "my proc"}}},
		{name: "a zombie", out: "PID STAT COMMAND\n9 Z dotnet\n", want: []container.Proc{{PID: 9, Stat: "Z", Comm: "dotnet"}}},
		{name: "empty answer", out: "", bad: true},
		{name: "no header", out: "5 S tail\n", bad: true},
		{name: "wrong header", out: "USER PID COMMAND\nroot 5 tail\n", bad: true},
		{name: "short header", out: "PID STAT\n5 S\n", bad: true},
		{name: "garbage row", out: "PID STAT COMMAND\nhello\n", bad: true},
		{name: "row without a name", out: "PID STAT COMMAND\n5 S\n", bad: true},
		{name: "non-numeric pid", out: "PID STAT COMMAND\nx S tail\n", bad: true},
		{name: "zero pid", out: "PID STAT COMMAND\n0 S tail\n", bad: true},
		{name: "negative pid", out: "PID STAT COMMAND\n-3 S tail\n", bad: true},
		{name: "blank line between rows", out: "PID STAT COMMAND\n5 S tail\n\n6 S sleep\n", bad: true},
		{name: "control character", out: "PID STAT COMMAND\n5 S ta\x07il\n", bad: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, calls := topEngine(t, containertest.Rule{Stdout: tt.out})
			e.Host = "tcp://h:1"

			got, err := e.Top(t.Context(), "web-1")
			if tt.bad {
				if api.CodeOf(err) != api.CodeAttachFailed {
					t.Errorf("err = %v, want ATTACH_FAILED", err)
				}

				return
			}

			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("got %+v, %v; want %+v", got, err, tt.want)
			}

			// Names only: no args, command or environment column.
			want := []string{"--host=tcp://h:1", "top", "web-1", "-o", "pid,stat,comm"}
			if argv := containertest.ReadCalls(t, calls)[0]; !slices.Equal(argv, want) {
				t.Errorf("argv = %q, want %q", argv, want)
			}
		})
	}
}

func TestTopErrors(t *testing.T) {
	t.Parallel()

	e, _ := topEngine(t, containertest.Rule{Exit: 1, Stderr: "Error response from daemon: container is not running"})
	if _, err := e.Top(t.Context(), "web-1"); api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "not running") {
		t.Errorf("err = %v", err)
	}

	e, calls := topEngine(t, containertest.Rule{Stdout: "PID STAT COMMAND\n"})
	for _, ref := range []string{"-o", "--privileged", "", "a b"} {
		if _, err := e.Top(t.Context(), ref); api.CodeOf(err) != api.CodeInvalidRequest {
			t.Errorf("ref %q: err = %v", ref, err)
		}
	}

	if n := len(containertest.ReadCalls(t, calls)); n != 0 {
		t.Errorf("docker ran %d times for invalid references", n)
	}
}

func TestStrays(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		procs []container.Proc
		want  []int
	}{
		{"idle container", []container.Proc{{PID: 1, Stat: "Ss", Comm: "docker-init"}, {PID: 2, Stat: "S", Comm: "tail"}}, nil},
		{"a running app", []container.Proc{{PID: 1, Stat: "Ss", Comm: "docker-init"}, {PID: 22, Stat: "Sl", Comm: "dotnet"}}, []int{22}},
		{"dotnet-counters is not dotnet", []container.Proc{{PID: 7, Stat: "Sl", Comm: "dotnet-counters"}}, nil},
		{"dotnet-dump is not dotnet", []container.Proc{{PID: 7, Stat: "Sl", Comm: "dotnet-dump"}}, nil},
		{"a longer name starting with dotnet", []container.Proc{{PID: 7, Stat: "S", Comm: "dotnetx"}}, nil},
		{"a path is not the name", []container.Proc{{PID: 7, Stat: "S", Comm: "/usr/share/dotnet/dotnet"}}, nil},
		{"case matters", []container.Proc{{PID: 7, Stat: "S", Comm: "Dotnet"}}, nil},
		{"a zombie is not a stray", []container.Proc{{PID: 9, Stat: "Z", Comm: "dotnet"}, {PID: 10, Stat: "Zl+", Comm: "dotnet"}}, nil},
		{"two apps", []container.Proc{{PID: 3, Stat: "S", Comm: "dotnet"}, {PID: 4, Stat: "R", Comm: "dotnet"}}, []int{3, 4}},
		{"none", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []int
			for _, p := range container.Strays(tt.procs) {
				got = append(got, p.PID)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("strays = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProbeIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     containertest.Rule
		wantCode api.Code
		wantMsg  string
	}{
		{name: "tail is there", rule: containertest.Rule{Stdout: "tail (GNU coreutils) 9.4\n"}},
		{
			name:     "chiseled image",
			rule:     containertest.Rule{Exit: 126, Stderr: `OCI runtime exec failed: exec failed: unable to start container process: exec: "tail": executable file not found in $PATH: unknown`},
			wantCode: api.CodeInvalidRequest, wantMsg: "no 'tail'",
		},
		{name: "busybox style", rule: containertest.Rule{Exit: 127, Stderr: "exec: tail: No such file or directory"}, wantCode: api.CodeInvalidRequest, wantMsg: "no 'tail'"},
		{name: "daemon down", rule: containertest.Rule{Exit: 1, Stderr: "Cannot connect to the Docker daemon at unix:///var/run/docker.sock"}, wantCode: api.CodeAttachFailed, wantMsg: "Cannot connect"},
		{name: "container not running", rule: containertest.Rule{Exit: 1, Stderr: "Error response from daemon: container abc is not running"}, wantCode: api.CodeAttachFailed, wantMsg: "not running"},
		{name: "silent failure", rule: containertest.Rule{Exit: 1}, wantCode: api.CodeAttachFailed, wantMsg: "failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := filepath.Join(t.TempDir(), "calls")
			tt.rule.Match = []string{"exec"}
			e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{tt.rule}})
			e.Context = "remote"

			err := e.ProbeIdle(t.Context(), "web-1")
			if tt.wantCode == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if api.CodeOf(err) != tt.wantCode || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v (%s), want %s containing %q", err, api.CodeOf(err), tt.wantCode, tt.wantMsg)
			}

			// As the container's own user and environment: no -i, -e, -u, -w, -t.
			want := []string{"--context=remote", "exec", "web-1", "tail", "--version"}
			if argv := containertest.ReadCalls(t, calls)[0]; !slices.Equal(argv, want) {
				t.Errorf("argv = %q, want %q", argv, want)
			}
		})
	}

	t.Run("references are checked", func(t *testing.T) {
		t.Parallel()

		calls := filepath.Join(t.TempDir(), "calls")
		e := containertest.Engine(t, containertest.Scenario{Calls: calls})

		if err := e.ProbeIdle(t.Context(), "--privileged"); api.CodeOf(err) != api.CodeInvalidRequest || len(containertest.ReadCalls(t, calls)) != 0 {
			t.Errorf("err = %v", err)
		}
	})
}
