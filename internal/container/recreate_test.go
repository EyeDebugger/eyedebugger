// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

func absPath(t *testing.T, parts ...string) string {
	t.Helper()

	abs, err := filepath.Abs(filepath.Join(parts...))
	if err != nil {
		t.Fatal(err)
	}

	return abs
}

func upRef(t *testing.T) container.ProjectRef {
	t.Helper()

	dir := absPath(t, t.TempDir())

	return container.ProjectRef{
		Name: "my-app", WorkDir: dir,
		Files: []string{filepath.Join(dir, "compose.yml"), filepath.Join(dir, "compose.override.yml")},
	}
}

func upEngine(t *testing.T, r containertest.Rule) (eng container.Engine, calls string) {
	t.Helper()

	calls = filepath.Join(t.TempDir(), "calls")
	r.Match = []string{"compose"}

	return containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{r}}), calls
}

func upOverride(t *testing.T) string {
	t.Helper()

	return filepath.Join(absPath(t, t.TempDir()), "override.yml")
}

func TestComposeUpEnteringFastMode(t *testing.T) {
	t.Parallel()

	ref, ovr := upRef(t), upOverride(t)
	e, calls := upEngine(t, containertest.Rule{})
	e.Host = "tcp://h:1"

	if err := e.ComposeUp(t.Context(), ref, ovr, []string{"producer", "consumer"}, false); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"--host=tcp://h:1", "compose", "--project-name=my-app", "--project-directory=" + ref.WorkDir,
		"--file=" + ref.Files[0], "--file=" + ref.Files[1], "--file=" + ovr,
		"up", "--detach", "--no-deps", "--force-recreate", "--no-build", "producer", "consumer",
	}
	if argv := containertest.ReadCalls(t, calls)[0]; !slices.Equal(argv, want) {
		t.Errorf("argv = %q\nwant   %q", argv, want)
	}
}

func TestComposeUpRestore(t *testing.T) {
	t.Parallel()

	ref := upRef(t)
	e, calls := upEngine(t, containertest.Rule{})

	if err := e.ComposeUp(t.Context(), ref, "", []string{"web"}, true); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"compose", "--project-name=my-app", "--project-directory=" + ref.WorkDir,
		"--file=" + ref.Files[0], "--file=" + ref.Files[1],
		"up", "--detach", "--no-deps", "--force-recreate", "--no-build", "--wait", "--wait-timeout=180", "web",
	}
	if argv := containertest.ReadCalls(t, calls)[0]; !slices.Equal(argv, want) {
		t.Errorf("argv = %q\nwant   %q", argv, want)
	}
}

func TestComposeUpEnvFiles(t *testing.T) {
	t.Parallel()

	ref, ovr := upRef(t), upOverride(t)
	e, calls := upEngine(t, containertest.Rule{})
	ref.EnvFiles = []string{"rel.env", filepath.Join(ref.WorkDir, "abs.env")}

	if err := e.ComposeUp(t.Context(), ref, ovr, []string{"web"}, false); err != nil {
		t.Fatal(err)
	}

	argv := containertest.ReadCalls(t, calls)[0]
	want := []string{"--env-file=" + absPath(t, "rel.env"), "--env-file=" + filepath.Join(ref.WorkDir, "abs.env")}

	// Absolute, in order, after the compose files.
	i := slices.Index(argv, want[0])
	if i < 0 || !slices.Equal(argv[i:i+2], want) || slices.Index(argv, "--file="+ovr) > i {
		t.Errorf("argv = %q", argv)
	}

	if slices.Contains(argv, "--env-file=rel.env") {
		t.Errorf("a relative env file reached docker: %q", argv)
	}
}

// TestComposeUpOnlyUp: eyedbg runs compose up and nothing else of compose's
// verbs: config would inline env_file values.
func TestComposeUpOnlyUp(t *testing.T) {
	t.Parallel()

	ref, ovr := upRef(t), upOverride(t)
	e, calls := upEngine(t, containertest.Rule{})

	if err := e.ComposeUp(t.Context(), ref, ovr, []string{"web"}, true); err != nil {
		t.Fatal(err)
	}

	for _, a := range containertest.ReadCalls(t, calls)[0] {
		if slices.Contains([]string{"config", "down", "rm", "build", "pull", "run", "exec", "kill", "stop", "restart"}, a) {
			t.Errorf("argv holds %q", a)
		}
	}
}

func TestComposeUpRefusals(t *testing.T) {
	t.Parallel()

	ref := upRef(t)
	ovr := filepath.Join(absPath(t, t.TempDir()), "override.yml")

	tests := []struct {
		name     string
		mod      func(r *container.ProjectRef)
		override string
		services []string
	}{
		{name: "empty project", mod: func(r *container.ProjectRef) { r.Name = "" }},
		{name: "uppercase project", mod: func(r *container.ProjectRef) { r.Name = "MyApp" }},
		{name: "flag as project", mod: func(r *container.ProjectRef) { r.Name = "--help" }},
		{name: "project with a comma", mod: func(r *container.ProjectRef) { r.Name = "a,b" }},
		{name: "relative work dir", mod: func(r *container.ProjectRef) { r.WorkDir = "rel" }},
		{name: "control char in work dir", mod: func(r *container.ProjectRef) { r.WorkDir = "/a\nb" }},
		{name: "empty work dir", mod: func(r *container.ProjectRef) { r.WorkDir = "" }},
		{name: "no files", mod: func(r *container.ProjectRef) { r.Files = nil }},
		{name: "relative file", mod: func(r *container.ProjectRef) { r.Files = []string{"compose.yml"} }},
		{name: "control char in file", mod: func(r *container.ProjectRef) { r.Files = []string{"/a/b\x00.yml"} }},
		{name: "too many files", mod: func(r *container.ProjectRef) {
			r.Files = slices.Repeat([]string{ref.Files[0]}, 17)
		}},
		{name: "control char in env file", mod: func(r *container.ProjectRef) { r.EnvFiles = []string{"a\nb.env"} }},
		{name: "empty env file", mod: func(r *container.ProjectRef) { r.EnvFiles = []string{""} }},
		{name: "relative override", override: "override.yml"},
		{name: "control char in override", override: "/a/o\x07.yml"},
		{name: "override already in the files", override: ref.Files[0]},
		{name: "no services", services: []string{}},
		{name: "flag as service", services: []string{"--force-recreate"}},
		{name: "dash service", services: []string{"-x"}},
		{name: "service with a space", services: []string{"we b"}},
		{name: "service with a slash", services: []string{"../web"}},
		{name: "one bad service among good ones", services: []string{"web", "--rm"}},
		{name: "too many services", services: slices.Repeat([]string{"web"}, 65)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e, calls := upEngine(t, containertest.Rule{})

			r := ref
			if tt.mod != nil {
				tt.mod(&r)
			}

			override := ovr
			if tt.override != "" {
				override = tt.override
			}

			services := []string{"web"}
			if tt.services != nil {
				services = tt.services
			}

			err := e.ComposeUp(t.Context(), r, override, services, false)
			if api.CodeOf(err) != api.CodeInvalidRequest {
				t.Errorf("err = %v (%s), want INVALID_REQUEST", err, api.CodeOf(err))
			}

			if n := len(containertest.ReadCalls(t, calls)); n != 0 {
				t.Errorf("docker ran %d times", n)
			}
		})
	}
}

func TestComposeUpFailure(t *testing.T) {
	t.Parallel()

	ref := upRef(t)

	t.Run("stderr tail without control characters, stdout never", func(t *testing.T) {
		t.Parallel()

		e, _ := upEngine(t, containertest.Rule{
			Exit: 1, Stdout: "SECRET-STDOUT-CANARY\n",
			Stderr: "line one\x1b[31m red\x07\nError response from daemon: invalid mount config for type \"bind\": bind source path does not exist\n",
		})

		err := e.ComposeUp(t.Context(), ref, "", []string{"web"}, true)
		if api.CodeOf(err) != api.CodeAttachFailed {
			t.Fatalf("err = %v (%s)", err, api.CodeOf(err))
		}

		msg := err.Error()
		if !strings.Contains(msg, "bind source path does not exist") || !strings.Contains(msg, "exit status 1") {
			t.Errorf("message = %q", msg)
		}

		if strings.Contains(msg, "SECRET-STDOUT-CANARY") {
			t.Error("compose's stdout reached the error")
		}

		for _, r := range msg {
			if r < ' ' || r == 0x7f {
				t.Errorf("control character %q in %q", r, msg)

				break
			}
		}

		if ae, ok := errors.AsType[*api.Error](err); !ok || !strings.Contains(ae.Hint, "eyedbg compose restore") {
			t.Errorf("hint of %v", err)
		}
	})

	t.Run("a long stderr is cut to its tail", func(t *testing.T) {
		t.Parallel()

		e, _ := upEngine(t, containertest.Rule{Exit: 1, Stderr: strings.Repeat("a", 20000) + "THE-END"})

		err := e.ComposeUp(t.Context(), ref, "", []string{"web"}, false)
		if err == nil || !strings.HasSuffix(strings.TrimSpace(err.Error()), "THE-END") || len(err.Error()) > 4400 {
			t.Errorf("err (%d bytes) = %.100v", len(err.Error()), err)
		}
	})
}
