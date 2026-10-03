// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

const (
	testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testImageID     = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	testAdapterDir  = ".eyedbg-netcoredbg-3.2.0-1092"
)

// inspected is the container a fake docker describes.
type inspected struct {
	path                        string
	running, paused, restarting bool
	init                        bool
	health                      string // the healthcheck's JSON array, or "null"
	workingDir, configFiles     string // compose's project.working_dir and project.config_files labels
	fast                        string
	image                       string // docker image inspect's answer
	missing                     bool   // docker inspect says there is no such container
}

func runningContainer() inspected {
	return inspected{path: "dotnet", running: true, health: "null", image: "linux/arm64/\n"}
}

func (c inspected) rules(adapterOK bool) []containertest.Rule {
	if c.missing {
		return []containertest.Rule{{Match: []string{"inspect"}, Exit: 1, Stderr: "Error: No such container: web-1"}}
	}

	out := fmt.Sprintf(`[%q,"/web-1",%q,%q,%t,%t,%t,%t,%s,"my-app","web",%q,%q,%q]`,
		testContainerID, testImageID, c.path, c.running, c.paused, c.restarting, c.init, c.health, c.workingDir, c.fast, c.configFiles)

	rules := []containertest.Rule{
		{Match: []string{"inspect", "--type", "container"}, Stdout: out},
		{Match: []string{"image", "inspect"}, Stdout: c.image},
		{Match: []string{"cp", "-", testContainerID + ":/"}},
	}

	probe := containertest.Rule{Match: []string{"exec", testContainerID, "/" + testAdapterDir + "/netcoredbg", "--version"}}
	if !adapterOK {
		probe.Exit, probe.Stderr = 126, "exec format error"
	}

	return append(rules, probe)
}

// containerRig is a driver over a fake docker, with netcoredbg "installed"
// for any platform in an adapter directory of its own.
type containerRig struct {
	d     *Driver
	calls string
}

func newContainerRig(t *testing.T, c inspected) *containerRig {
	t.Helper()

	netcoredbg, sharpdbg := bundled(t)
	adapterDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(adapterDir, "netcoredbg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &containerRig{calls: filepath.Join(t.TempDir(), "calls")}

	engine := containertest.Engine(t, containertest.Scenario{Calls: r.calls, Rules: c.rules(true)})

	r.d = &Driver{m: netcoredbg, sharp: sharpdbg, env: driverEnv{
		goos: "darwin", goarch: "arm64",
		engine: func(e api.ContainerEngine) (container.Engine, error) {
			engine.Host, engine.Context = e.Host, e.Context

			return engine, nil
		},
		installedFor: func(_ *adapters.Manifest, platform string) (string, string, error) {
			if platform != "linux/arm64" && platform != "linux/amd64" {
				return "", "", adapters.ErrNotInstalled
			}

			return adapterDir, filepath.Join(adapterDir, "netcoredbg"), nil
		},
	}}

	return r
}

// docker is the subcommands the fake docker ran, in order ("inspect", "image
// inspect", "cp", "exec").
func (r *containerRig) docker(t *testing.T) []string {
	t.Helper()

	var subs []string

	for _, argv := range containertest.ReadCalls(t, r.calls) {
		rest := slices.DeleteFunc(slices.Clone(argv), func(a string) bool { return strings.HasPrefix(a, "--host=") || strings.HasPrefix(a, "--context=") })
		sub := rest[0]

		if sub == "image" {
			sub += " " + rest[1]
		}

		subs = append(subs, sub)
	}

	return subs
}

func attachSpec(cs api.ContainerSpec) api.AttachSpec {
	if cs.Ref == "" {
		cs.Ref = "web-1"
	}

	return api.AttachSpec{Container: &cs}
}

func TestPrepareContainerAttach(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	c := runningContainer()
	c.health = `[5000000000,3000000000,3,false]`
	c.workingDir, c.configFiles = src, writeComposeFile(t, src)

	r := newContainerRig(t, c)

	launch, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{Engine: api.ContainerEngine{Context: "desk"}, RequireDotnet: true}))
	if err != nil {
		t.Fatal(err)
	}

	checkContainerLaunch(t, launch)

	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		t.Fatal(err)
	}

	want := &api.ContainerInfo{
		ID: testContainerID, Name: "web-1", Service: "web", Project: "my-app", PID: 1, Platform: "linux/arm64",
		Map: []api.PathMapping{{Remote: "/src", Local: resolved}}, UnhealthyAfter: api.Duration(18_000_000_000),
	}

	got := launch.Container
	if got == nil || got.ID != want.ID || got.Name != want.Name || got.Service != want.Service || got.Project != want.Project ||
		got.PID != 1 || got.Platform != want.Platform || !slices.Equal(got.Map, want.Map) || got.UnhealthyAfter != want.UnhealthyAfter {
		t.Errorf("container = %+v, want %+v", got, want)
	}

	if launch.PathMap == nil {
		t.Error("no path map: the compose project directory should map to /src")
	}

	if got, wantSubs := r.docker(t), []string{"inspect", "image inspect", "cp", "exec"}; !slices.Equal(got, wantSubs) {
		t.Errorf("docker ran %q, want %q", got, wantSubs)
	}
}

// checkContainerLaunch checks the parts of a container attach's Launch that
// don't depend on the container.
func checkContainerLaunch(t *testing.T, launch session.Launch) {
	t.Helper()

	wantArgs := []string{"--context=desk", "exec", "-i", testContainerID, "/" + testAdapterDir + "/netcoredbg", "--interpreter=vscode"}
	if launch.Adapter == "" || !slices.Equal(launch.AdapterArgs, wantArgs) {
		t.Errorf("adapter = %q %q, want docker %q", launch.Adapter, launch.AdapterArgs, wantArgs)
	}

	if launch.Request != session.RequestAttach || launch.PID != 1 || launch.Arguments["processId"] != 1 || launch.Arguments["justMyCode"] != true {
		t.Errorf("attach = %q pid %d args %v", launch.Request, launch.PID, launch.Arguments)
	}

	if launch.Program != "web (compose my-app, container web-1, pid 1)" {
		t.Errorf("program = %q", launch.Program)
	}

	if launch.AdapterName != "netcoredbg" || launch.AdapterID != "coreclr" || launch.ExitCodeUnknown != "" || launch.SideEffects == nil ||
		!strings.Contains(launch.AttachHint, "--pid") {
		t.Errorf("settings = %+v", launch)
	}
}

func TestPrepareContainerAttachPidAndMap(t *testing.T) {
	t.Parallel()

	local := t.TempDir()

	r := newContainerRig(t, runningContainer())

	launch, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{
		PID: 7, Map: []api.PathMapping{{Remote: "/app/src", Local: local}},
	}))
	if err != nil {
		t.Fatal(err)
	}

	if launch.PID != 7 || launch.Container.PID != 7 || launch.Arguments["processId"] != 7 || !strings.HasSuffix(launch.Program, "pid 7)") {
		t.Errorf("pid: launch %d, container %d, args %v, program %q", launch.PID, launch.Container.PID, launch.Arguments, launch.Program)
	}

	if len(launch.Container.Map) != 1 || launch.Container.Map[0].Remote != "/app/src" {
		t.Errorf("map = %+v: --map replaces the default", launch.Container.Map)
	}

	// A running container with neither a map nor a compose directory: an
	// empty map (line breakpoints are refused later), no UnhealthyAfter.
	bare, err := newContainerRig(t, runningContainer()).d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
	if err != nil || bare.PathMap == nil || len(bare.Container.Map) != 0 || bare.Container.UnhealthyAfter != 0 {
		t.Errorf("bare container: %+v, %v", bare.Container, err)
	}
}

// writeComposeFile makes dir/compose.yaml and returns its path.
func writeComposeFile(t *testing.T, dir string) string {
	t.Helper()

	file := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(file, []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return file
}

// The default map needs the working_dir label to be an existing directory that
// holds a compose file the files label names: an image can set both labels on
// a container started with plain 'docker run', so the label alone (a directory
// the image author picked, such as the user's home) maps nothing.
func TestPrepareContainerAttachDefaultMapNeedsCorroboration(t *testing.T) {
	t.Parallel()

	good := t.TempDir()
	goodFile := writeComposeFile(t, good)

	hostile := t.TempDir() // the directory a hostile image's label picks
	if err := os.WriteFile(filepath.Join(hostile, "id_rsa"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, workingDir, configFiles string
		mapped                        bool
	}{
		{"compose's labels agree", good, goodFile, true},
		{"the hostile label: a directory with no compose file in it", hostile, "", false},
		{"the hostile label with a file list that isn't there", hostile, filepath.Join(hostile, "compose.yaml"), false},
		{"the hostile label with a file list that isn't compose's", hostile, filepath.Join(hostile, "id_rsa"), false},
		{"the hostile label with another directory's compose file", hostile, goodFile, false},
		{"a directory that is gone", filepath.Join(t.TempDir(), "gone"), goodFile, false},
		{"a relative directory", "relative/dir", goodFile, false},
		{"the root", string(filepath.Separator), goodFile, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := runningContainer()
			c.workingDir, c.configFiles = tt.workingDir, tt.configFiles

			launch, err := newContainerRig(t, c).d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
			if err != nil {
				t.Fatal(err)
			}

			if mapped := len(launch.Container.Map) != 0; mapped != tt.mapped {
				t.Errorf("map %+v; want a default map: %v", launch.Container.Map, tt.mapped)
			}

			if !tt.mapped && launch.PathMap == nil {
				t.Error("an empty map is still a map: line breakpoints must be refused with its hint, not sent unmapped")
			}
		})
	}
}

// An explicit --map is the user's own and needs no corroboration.
func TestPrepareContainerAttachExplicitMapIgnoresLabels(t *testing.T) {
	t.Parallel()

	local := t.TempDir()

	c := runningContainer()
	c.workingDir = t.TempDir() // a label no compose file backs

	launch, err := newContainerRig(t, c).d.PrepareContainerAttach(t.Context(),
		attachSpec(api.ContainerSpec{Map: []api.PathMapping{{Remote: "/app", Local: local}}}))
	if err != nil {
		t.Fatal(err)
	}

	if len(launch.Container.Map) != 1 || launch.Container.Map[0].Remote != "/app" {
		t.Errorf("map = %+v, want the one asked for", launch.Container.Map)
	}
}

func TestPrepareContainerAttachRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		c         func(*inspected)
		spec      api.ContainerSpec
		wantCode  api.Code
		wantMsg   string
		wantCalls []string // docker subcommands that ran: nothing may be copied
		skip      bool     // the error is marked session.ErrNotCandidate
	}{
		{"not running", func(c *inspected) { c.running = false }, api.ContainerSpec{}, api.CodeAttachFailed, "is not running", []string{"inspect"}, false},
		{"paused", func(c *inspected) { c.paused = true }, api.ContainerSpec{}, api.CodeAttachFailed, "is paused", []string{"inspect"}, false},
		{"restarting", func(c *inspected) { c.restarting = true }, api.ContainerSpec{}, api.CodeAttachFailed, "is restarting", []string{"inspect"}, false},
		{"no such container", func(c *inspected) { c.missing = true }, api.ContainerSpec{}, api.CodeInvalidRequest, "no container", []string{"inspect"}, false},
		{
			"pid 1 is not dotnet, implicit", func(c *inspected) { c.path = "/usr/sbin/nginx" },
			api.ContainerSpec{RequireDotnet: true},
			api.CodeNotDotnet, "pid 1 runs nginx",
			[]string{"inspect"},
			true,
		},
		{
			"fast mode, implicit", func(c *inspected) { c.fast = "/x/override.yml" },
			api.ContainerSpec{RequireDotnet: true},
			api.CodeInvalidRequest, "runs in eyedbg fast mode: its app runs only under 'eyedbg compose launch'",
			[]string{"inspect"},
			true,
		},
		{
			"fast mode, named", func(c *inspected) { c.fast = "/x/override.yml" },
			api.ContainerSpec{},
			api.CodeInvalidRequest, "runs in eyedbg fast mode",
			[]string{"inspect"},
			false,
		},
		{
			"init: true without --pid", func(c *inspected) { c.init = true },
			api.ContainerSpec{},
			api.CodeInvalidRequest, "docker-init",
			[]string{"inspect"},
			false,
		},
		{
			"init: true without --pid, implicit", func(c *inspected) { c.init = true },
			api.ContainerSpec{RequireDotnet: true},
			api.CodeInvalidRequest, "docker-init",
			[]string{"inspect"},
			false,
		},
		{
			"arm/v7 image", func(c *inspected) { c.image = "linux/arm/v7\n" },
			api.ContainerSpec{},
			api.CodeAttachFailed, "linux/arm/v7 image: netcoredbg is built for linux/amd64 and linux/arm64 only",
			[]string{"inspect", "image inspect"},
			false,
		},
		{
			"windows container", func(c *inspected) { c.image = "windows/amd64/\n" },
			api.ContainerSpec{},
			api.CodeAttachFailed, "eyedbg debugs Linux containers",
			[]string{"inspect", "image inspect"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := runningContainer()
			tt.c(&c)

			r := newContainerRig(t, c)

			_, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(tt.spec))
			if api.CodeOf(err) != tt.wantCode || err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("err = %v (%s), want %s containing %q", err, api.CodeOf(err), tt.wantCode, tt.wantMsg)
			}

			if errors.Is(err, session.ErrNotCandidate) != tt.skip {
				t.Errorf("ErrNotCandidate = %v, want %v", !tt.skip, tt.skip)
			}

			if got := r.docker(t); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("docker ran %q, want %q (nothing is copied for a refusal)", got, tt.wantCalls)
			}
		})
	}
}

func TestPrepareContainerAttachInitWithPid(t *testing.T) {
	t.Parallel()

	c := runningContainer()
	c.init = true

	launch, err := newContainerRig(t, c).d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{PID: 7}))
	if err != nil || launch.PID != 7 {
		t.Errorf("init: true with --pid 7: pid %d, %v", launch.PID, err)
	}

	// A named service whose pid 1 isn't dotnet is attached to anyway.
	c = runningContainer()
	c.path = "/bin/sh"

	if _, err := newContainerRig(t, c).d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{})); err != nil {
		t.Errorf("a named container whose pid 1 is sh: %v", err)
	}
}

func TestPrepareContainerAttachAdapters(t *testing.T) {
	t.Parallel()

	t.Run("not installed for the container's platform", func(t *testing.T) {
		t.Parallel()

		c := runningContainer()
		c.image = "linux/amd64/\n"
		r := newContainerRig(t, c)
		r.d.env.installedFor = func(*adapters.Manifest, string) (string, string, error) {
			return "", "", fmt.Errorf("netcoredbg for linux/amd64: %w", adapters.ErrNotInstalled)
		}

		_, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))

		var e *api.Error
		if !errors.As(err, &e) || e.Code != api.CodeAdapterMissing || !strings.Contains(e.Hint, "eyedbg adapters install netcoredbg --platform linux/amd64") {
			t.Fatalf("err = %v, want ADAPTER_NOT_INSTALLED with the install hint", err)
		}

		if got := r.docker(t); !slices.Equal(got, []string{"inspect", "image inspect"}) {
			t.Errorf("docker ran %q: nothing may be copied", got)
		}
	})

	t.Run("probe fails", func(t *testing.T) {
		t.Parallel()

		r := newContainerRig(t, runningContainer())
		r.d.env.engine = func(api.ContainerEngine) (container.Engine, error) {
			return containertest.Engine(t, containertest.Scenario{Calls: r.calls, Rules: runningContainer().rules(false)}), nil
		}

		_, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
		if api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "doesn't run in the container") {
			t.Errorf("err = %v, want ATTACH_FAILED", err)
		}
	})

	t.Run("sharpdbg is refused before docker runs", func(t *testing.T) {
		t.Parallel()

		r := newContainerRig(t, runningContainer())
		d := bind(t, r.d, "sharpdbg")

		_, err := d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
		if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "only netcoredbg is copied") {
			t.Errorf("err = %v, want INVALID_REQUEST", err)
		}

		if got := r.docker(t); len(got) != 0 {
			t.Errorf("docker ran %q for a refused adapter", got)
		}
	})

	t.Run("netcoredbg is the adapter even where it is not the host default", func(t *testing.T) {
		t.Parallel()

		r := newContainerRig(t, runningContainer())
		r.d.env.goos, r.d.env.goarch = "darwin", "amd64" // no netcoredbg build: sharpdbg is the host default

		d := bind(t, r.d, "netcoredbg")

		launch, err := d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
		if err != nil || launch.AdapterName != "netcoredbg" {
			t.Errorf("adapter = %q, %v", launch.AdapterName, err)
		}
	})

	t.Run("no netcoredbg manifest", func(t *testing.T) {
		t.Parallel()

		r := newContainerRig(t, runningContainer())
		r.d.m, r.d.sharp = nil, nil

		_, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
		expectCode(t, err, api.CodeAdapterMissing)
	})

	t.Run("a user manifest under another name is not netcoredbg", func(t *testing.T) {
		t.Parallel()

		r := newContainerRig(t, runningContainer())
		m := *r.d.m
		m.Name = "mydbg"
		r.d.m = &m

		_, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
		expectCode(t, err, api.CodeAdapterMissing)
	})
}

func TestPrepareContainerAttachEngineErrors(t *testing.T) {
	t.Parallel()

	r := newContainerRig(t, runningContainer())
	r.d.env.engine = func(api.ContainerEngine) (container.Engine, error) {
		return container.Engine{}, api.NewError(api.CodeAttachFailed, "docker was not found", "")
	}

	_, err := r.d.PrepareContainerAttach(t.Context(), attachSpec(api.ContainerSpec{}))
	expectCode(t, err, api.CodeAttachFailed)

	if _, err := r.d.PrepareContainerAttach(t.Context(), api.AttachSpec{PID: 5}); api.CodeOf(err) != api.CodeInternal {
		t.Errorf("an attach without a container: %v, want INTERNAL", err)
	}
}

func expectCode(t *testing.T, err error, code api.Code) {
	t.Helper()

	if got := api.CodeOf(err); got != code {
		t.Fatalf("err = %v (code %q), want %s", err, got, code)
	}
}
