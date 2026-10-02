// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

const (
	topHeader = "PID STAT COMMAND\n"
	topIdle   = topHeader + "10 Ss docker-init\n11 S tail\n"
	topStray  = topIdle + "20 Sl dotnet\n"
)

// fastContainer is a fast-mode container a fake docker describes.
type fastContainer struct {
	paused, restarting, stopped bool
	// unlabelled drops eyedbg's labels (an as-built container).
	unlabelled bool
	// version, dll, workdir are the fast labels ("" version: "1", "" dll:
	// Web.dll, "" workdir: /app).
	version, dll, workdir string
	composeDir            string
	// health is the healthcheck array of the plain inspect, or "null".
	health string
	// top is docker top's answer; topExit and topStderr make it fail; a
	// stray that goes away after strayCalls answers, if > 0, is topStray for
	// that many calls and topIdle after.
	top        string
	topExit    int
	strayCalls int
	// probeFirstFails makes the first probe of the adapter fail (it is not
	// in the container yet); copyFails makes docker cp fail.
	probeFirstFails, copyFails bool
	// image is docker image inspect's answer.
	image string
}

func (c fastContainer) fastJSON() string {
	var (
		version = orDefault(c.version, "1")
		dll     = orDefault(c.dll, "Web.dll")
		workdir = orDefault(c.workdir, "/app")
		fast    = func(v string) any {
			if c.unlabelled {
				return nil
			}

			return v
		}
		override any
	)

	if !c.unlabelled {
		override = "/home/me/.eyedbg/compose/my-app/override.yml"
	}

	var (
		dllEntry any // the entrypoint of a fast container is tail: never the dotnet shape
		compose  any = c.composeDir
	)

	vals := []any{
		testContainerID, "/web-1", testImageID, "linux", !c.stopped, c.paused, c.restarting, dllEntry, false, workdir,
		"my-app", "web", compose, "", fast(version), override, fast(dll), fast(workdir), nil,
	}

	out, err := json.Marshal(vals)
	if err != nil {
		panic(err)
	}

	return string(out)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}

	return s
}

// rules are the fake docker's answers. The order matters: the first rule that
// matches answers.
func (c fastContainer) rules() []containertest.Rule {
	health := orDefault(c.health, "null")
	plain := fmt.Sprintf(`[%q,"/web-1",%q,"tail",true,false,false,true,%s,"my-app","web","","fast"]`, testContainerID, testImageID, health)

	probe := []string{"exec", testContainerID, "/" + testAdapterDir + "/netcoredbg", "--version"}

	rules := []containertest.Rule{
		{Match: []string{"inspect", "--type", "container"}, Has: "dev.izzat.eyedbg.fast.version", Stdout: c.fastJSON()},
		{Match: []string{"inspect", "--type", "container"}, Stdout: plain},
		{Match: []string{"image", "inspect"}, Stdout: orDefault(c.image, "linux/arm64/\n")},
	}

	topMatch := []string{"top", testContainerID, "-o", "pid,stat,comm"}

	switch {
	case c.topExit != 0:
		rules = append(rules, containertest.Rule{Match: topMatch, Exit: c.topExit, Stderr: "Error response from daemon: boom"})
	case c.strayCalls > 0:
		rules = append(rules, containertest.Rule{Match: topMatch, Stdout: topStray, Times: c.strayCalls},
			containertest.Rule{Match: topMatch, Stdout: topIdle})
	default:
		rules = append(rules, containertest.Rule{Match: topMatch, Stdout: orDefault(c.top, topIdle)})
	}

	if c.probeFirstFails {
		rules = append(rules, containertest.Rule{Match: probe, Times: 1, Exit: 126, Stderr: "OCI runtime exec failed: no such file or directory"})
	}

	cp := containertest.Rule{Match: []string{"cp", "-", testContainerID + ":/"}}
	if c.copyFails {
		cp.Exit, cp.Stderr = 1, "Error response from daemon: container rootfs is marked read-only"
	}

	return append(rules, cp, containertest.Rule{Match: probe, Stdout: "NET Core debugger\n"})
}

// launchRig is a driver over a fake docker, with netcoredbg "installed" for
// linux/arm64 and linux/amd64.
type launchRig struct {
	d     *Driver
	calls string
}

func newLaunchRig(t *testing.T, c fastContainer) *launchRig {
	t.Helper()

	netcoredbg, sharpdbg := bundled(t)
	adapterDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(adapterDir, "netcoredbg"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &launchRig{calls: filepath.Join(t.TempDir(), "calls")}
	engine := containertest.Engine(t, containertest.Scenario{Calls: r.calls, Rules: c.rules()})

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
		strayChecks: 4, strayEvery: time.Millisecond,
	}}

	return r
}

// docker is the subcommands the fake docker ran, in order ("inspect", "image
// inspect", "top", "cp", "exec").
func (r *launchRig) docker(t *testing.T) []string {
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

func launchSpec() api.ContainerLaunchSpec { return api.ContainerLaunchSpec{Ref: "web-1"} }

func TestPrepareContainerLaunch(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	c := fastContainer{composeDir: src, health: `[5000000000,3000000000,3,false]`}
	r := newLaunchRig(t, c)

	spec := launchSpec()
	spec.Engine = api.ContainerEngine{Context: "desk"}

	launch, err := r.d.PrepareContainerLaunch(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}

	// The adapter is the container's own copy of netcoredbg through docker
	// exec: none of the flags that would change the app's user, directory or
	// environment, and the full container id.
	wantArgs := []string{"--context=desk", "exec", "-i", testContainerID, "/" + testAdapterDir + "/netcoredbg", "--interpreter=vscode"}
	if launch.Adapter == "" || !slices.Equal(launch.AdapterArgs, wantArgs) {
		t.Errorf("adapter = %q %q, want docker %q", launch.Adapter, launch.AdapterArgs, wantArgs)
	}

	for _, banned := range []string{"-e", "--env", "--env-file", "-u", "--user", "-w", "--workdir", "-t", "--tty", "--privileged", "-it"} {
		if slices.Contains(launch.AdapterArgs, banned) {
			t.Errorf("adapter args carry %s", banned)
		}
	}

	checkLaunchRequest(t, launch)
	checkLaunchedContainer(t, launch, src)

	// A running adapter is probed, not copied again.
	if got, want := r.docker(t), []string{"inspect", "image inspect", "inspect", "top", "exec"}; !slices.Equal(got, want) {
		t.Errorf("docker ran %q, want %q", got, want)
	}
}

// checkLaunchSettings checks what a launch says of its app and adapter.
func checkLaunchSettings(t *testing.T, launch session.Launch) {
	t.Helper()

	if launch.Program != "web (compose my-app, container web-1): dotnet /app/Web.dll" {
		t.Errorf("program = %q", launch.Program)
	}

	if launch.AdapterName != "netcoredbg" || launch.AdapterID != "coreclr" || launch.ExitCodeUnknown != "" || launch.SideEffects == nil ||
		launch.StartFailureHint == nil || len(launch.Warnings) != 0 {
		t.Errorf("settings = %+v", launch)
	}
}

// checkLaunchedContainer checks what a launch says of its app and container.
func checkLaunchedContainer(t *testing.T, launch session.Launch, src string) {
	t.Helper()

	checkLaunchSettings(t, launch)

	if launch.PathMap == nil {
		t.Error("no path map: the compose project directory should map to /src")
	}

	resolved, err := filepath.EvalSymlinks(src)
	if err != nil {
		t.Fatal(err)
	}

	got := launch.Container
	if got == nil || got.ID != testContainerID || got.Name != "web-1" || got.PID != 0 || got.UnhealthyAfter != api.Duration(18*time.Second) ||
		!slices.Equal(got.Map, []api.PathMapping{{Remote: "/src", Local: resolved}}) {
		t.Fatalf("container = %+v", got)
	}

	if got.Service != "web" || got.Project != "my-app" || got.Platform != "linux/arm64" {
		t.Errorf("container names = %+v", got)
	}
}

// checkLaunchRequest checks the DAP launch request: `dotnet /app/Web.dll` in
// /app, no arguments and — the point — no env key, so the app inherits the
// container's environment.
func checkLaunchRequest(t *testing.T, launch session.Launch) {
	t.Helper()

	want := map[string]any{
		"name": "eyedbg", "type": "coreclr", "request": "launch", "program": "/app/Web.dll", "args": []string{}, "cwd": "/app",
		"stopAtEntry": false, "justMyCode": true,
	}

	if launch.Request != "launch" || !reflect.DeepEqual(launch.Arguments, want) {
		t.Errorf("launch = %q %v, want %v", launch.Request, launch.Arguments, want)
	}

	if _, has := launch.Arguments["env"]; has {
		t.Error("the launch request carries an env key")
	}

	raw, err := json.Marshal(launch.Arguments)
	if err != nil || !strings.Contains(string(raw), `"args":[]`) || strings.Contains(string(raw), `"env"`) {
		t.Errorf("launch request on the wire = %s, %v: want empty args and no env", raw, err)
	}

	if launch.PID != 0 {
		t.Errorf("pid = %d: a launch has no pid to attach to", launch.PID)
	}
}

func TestPrepareContainerLaunchStopOnEntry(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{})

	spec := launchSpec()
	spec.StopOnEntry = true

	launch, err := r.d.PrepareContainerLaunch(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}

	if launch.Arguments["stopAtEntry"] != true {
		t.Errorf("stopAtEntry = %v", launch.Arguments["stopAtEntry"])
	}
}

func TestPrepareContainerLaunchPathMap(t *testing.T) {
	t.Parallel()

	t.Run("none without a compose directory", func(t *testing.T) {
		t.Parallel()

		launch, err := newLaunchRig(t, fastContainer{}).d.PrepareContainerLaunch(t.Context(), launchSpec())
		if err != nil {
			t.Fatal(err)
		}

		if launch.PathMap == nil || len(launch.Container.Map) != 0 {
			t.Errorf("map = %+v, want an empty one (line breakpoints refused with a hint)", launch.Container.Map)
		}
	})

	t.Run("the request's map wins", func(t *testing.T) {
		t.Parallel()

		other := t.TempDir()
		r := newLaunchRig(t, fastContainer{composeDir: t.TempDir()})

		spec := launchSpec()
		spec.Map = []api.PathMapping{{Remote: "/code", Local: other}}

		launch, err := r.d.PrepareContainerLaunch(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}

		resolved, _ := filepath.EvalSymlinks(other)
		if want := []api.PathMapping{{Remote: "/code", Local: resolved}}; !slices.Equal(launch.Container.Map, want) {
			t.Errorf("map = %+v, want %+v", launch.Container.Map, want)
		}
	})
}

// TestPrepareContainerLaunchCopiesWhenTheProbeFails: a first launch into a
// container (no adapter there yet) copies it, then probes again.
func TestPrepareContainerLaunchCopiesWhenTheProbeFails(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{probeFirstFails: true})

	if _, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec()); err != nil {
		t.Fatal(err)
	}

	want := []string{"inspect", "image inspect", "inspect", "top", "exec", "cp", "exec"}
	if got := r.docker(t); !slices.Equal(got, want) {
		t.Errorf("docker ran %q, want %q", got, want)
	}
}

func TestPrepareContainerLaunchCopyFails(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{probeFirstFails: true, copyFails: true})

	_, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec())
	if api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "marked read-only") {
		t.Errorf("err = %v, want ATTACH_FAILED naming the read-only root", err)
	}
}

// TestPrepareContainerLaunchRefusals: everything that can be refused is
// refused before the adapter is probed or copied — and before docker top.
func TestPrepareContainerLaunchRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    fastContainer
		code api.Code
		want string
		hint string
	}{
		{"not in fast mode", fastContainer{unlabelled: true}, api.CodeInvalidRequest, "isn't in eyedbg fast mode", "eyedbg compose launch web"},
		{"a label format this eyedbg doesn't know", fastContainer{version: "2"}, api.CodeInvalidRequest, "isn't in eyedbg fast mode", "eyedbg compose launch web"},
		{"stopped", fastContainer{stopped: true}, api.CodeAttachFailed, "is not running", "docker start web-1"},
		{"paused", fastContainer{paused: true}, api.CodeAttachFailed, "is paused", "docker unpause web-1"},
		{"restarting", fastContainer{restarting: true}, api.CodeAttachFailed, "is restarting", ""},
		{"a dll label with a slash", fastContainer{dll: "sub/Web.dll"}, api.CodeInvalidRequest, "dll", ""},
		{"a dll label that is no assembly", fastContainer{dll: "Web.txt"}, api.CodeInvalidRequest, "dll", ""},
		{"a working directory under /usr", fastContainer{workdir: "/usr/x"}, api.CodeInvalidRequest, "workdir", ""},
		{"a relative working directory", fastContainer{workdir: "app"}, api.CodeInvalidRequest, "workdir", ""},
		{"a dotted working directory", fastContainer{workdir: "/app/../etc"}, api.CodeInvalidRequest, "workdir", ""},
		{"a windows image", fastContainer{image: "windows/amd64/\n"}, api.CodeAttachFailed, "Linux containers", ""},
		{"an unsupported architecture", fastContainer{image: "linux/riscv64/\n"}, api.CodeAttachFailed, "linux/amd64 and linux/arm64 only", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := newLaunchRig(t, tt.c)

			_, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec())

			e, ok := errors.AsType[*api.Error](err)
			if !ok || e.Code != tt.code || !strings.Contains(strings.ToLower(e.Message), strings.ToLower(tt.want)) || !strings.Contains(e.Hint, tt.hint) {
				t.Fatalf("err = %v (%+v), want %s containing %q, hint containing %q", err, e, tt.code, tt.want, tt.hint)
			}

			for _, sub := range r.docker(t) {
				if sub == "top" || sub == "cp" || sub == "exec" {
					t.Errorf("docker ran %q: a refused launch must not look at processes or copy anything (%q)", sub, r.docker(t))
				}
			}
		})
	}
}

func TestPrepareContainerLaunchNoSharpDbg(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{})

	d, err := r.d.WithAdapter(SharpDbg)
	if err != nil {
		t.Fatal(err)
	}

	cl, ok := d.(*Driver)
	if !ok {
		t.Fatalf("WithAdapter returned %T", d)
	}

	_, err = cl.PrepareContainerLaunch(t.Context(), launchSpec())
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "sharpdbg can't debug inside a container") {
		t.Errorf("err = %v, want INVALID_REQUEST", err)
	}

	if subs := r.docker(t); len(subs) != 0 {
		t.Errorf("docker ran %q before refusing the adapter", subs)
	}
}

func TestPrepareContainerLaunchAdapterNotInstalled(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{image: "linux/amd64/\n"})
	r.d.env.installedFor = func(*adapters.Manifest, string) (string, string, error) { return "", "", adapters.ErrNotInstalled }

	_, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec())

	e, ok := errors.AsType[*api.Error](err)
	if !ok || e.Code != api.CodeAdapterMissing || !strings.Contains(e.Hint, "eyedbg adapters install netcoredbg --platform linux/amd64") {
		t.Fatalf("err = %v, want ADAPTER_NOT_INSTALLED with the --platform hint", err)
	}

	for _, sub := range r.docker(t) {
		if sub == "top" || sub == "cp" || sub == "exec" {
			t.Errorf("docker ran %q before the adapter was known to be installed", sub)
		}
	}
}

// A non-zombie process named exactly dotnet is an earlier app: looked for
// again (a stopped app may still be exiting), then refused with the way out,
// before the adapter is probed or copied.
func TestStrayGuardRefusesAfterRetries(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{top: topStray})

	_, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec())

	e, ok := errors.AsType[*api.Error](err)
	if !ok || e.Code != api.CodeAttachFailed || !strings.Contains(e.Message, "already runs a .NET process") || !strings.Contains(e.Hint, "docker restart web-1") {
		t.Fatalf("err = %v, want ATTACH_FAILED with the docker restart way out", err)
	}

	tops, others := countDocker(r.docker(t))
	if tops != 4 {
		t.Errorf("docker top ran %d times, want 4 checks", tops)
	}

	if len(others) != 0 {
		t.Errorf("docker also ran %q: a refused launch must not copy or probe", others)
	}
}

func TestStrayGuardAnAppThatWasExiting(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{strayCalls: 2})

	if _, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec()); err != nil {
		t.Fatalf("a stray that went away: %v", err)
	}

	if tops, _ := countDocker(r.docker(t)); tops != 3 {
		t.Errorf("docker top ran %d times, want 3 (two strays, then clear)", tops)
	}
}

// TestStrayGuardNotStrays: a zombie dotnet, dotnet-counters and a process
// merely containing the name are not an earlier app.
func TestStrayGuardNotStrays(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{top: topIdle + "30 Z dotnet\n31 Sl dotnet-counters\n32 S mydotnet\n"})

	if _, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec()); err != nil {
		t.Fatalf("err = %v, want none: none of these is an earlier app", err)
	}

	if tops, _ := countDocker(r.docker(t)); tops != 1 {
		t.Errorf("docker top ran %d times, want 1", tops)
	}
}

func TestStrayGuardDockerTopFails(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{topExit: 1})

	launch, err := r.d.PrepareContainerLaunch(t.Context(), launchSpec())
	if err != nil {
		t.Fatalf("a failing docker top must not stop the launch: %v", err)
	}

	if len(launch.Warnings) != 1 || !strings.Contains(launch.Warnings[0], "docker top failed") {
		t.Errorf("warnings = %q, want one saying docker top failed", launch.Warnings)
	}
}

func TestStrayGuardTheCallerGivesUp(t *testing.T) {
	t.Parallel()

	r := newLaunchRig(t, fastContainer{top: topStray})

	engine, err := r.d.env.engine(api.ContainerEngine{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// A canceled request is not "docker top failed, go ahead".
	warning, err := r.d.strayGuard(ctx, engine, container.FastInfo{ID: testContainerID, Name: "web-1"})
	if !errors.Is(err, context.Canceled) || warning != "" {
		t.Errorf("guard = %q, %v, want the cancellation and no warning", warning, err)
	}
}

// countDocker splits docker's calls into top calls and the others that touch
// the adapter (cp, exec).
func countDocker(subs []string) (tops int, others []string) {
	for _, s := range subs {
		switch s {
		case "top":
			tops++
		case "cp", "exec":
			others = append(others, s)
		}
	}

	return tops, others
}

// TestStrayHintOfAFailedStart: what is added to a failed launch's error.
func TestStrayHintOfAFailedStart(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		c    fastContainer
		want string // "" for none
	}{
		{"a stray", fastContainer{top: topStray}, "docker restart web-1"},
		{"none", fastContainer{top: topIdle}, ""},
		{"docker top fails", fastContainer{topExit: 1}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			engine, err := newLaunchRig(t, tt.c).d.env.engine(api.ContainerEngine{})
			if err != nil {
				t.Fatal(err)
			}

			got := strayHint(t.Context(), engine, testContainerID, "web-1")
			if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
				t.Errorf("hint = %q, want one containing %q", got, tt.want)
			}
		})
	}
}
