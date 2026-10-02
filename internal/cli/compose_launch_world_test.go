// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// A compose stack for the tests of 'compose launch' and 'compose restore':
// docker, dotnet and netcoredbg are fakes, the daemon is real (in-process),
// and the files eyedbg keeps are real, under a throwaway home.

const worldProject = "myapp"

// fakeCtr is one container of the fake docker.
type fakeCtr struct {
	fi container.FastInfo
	// asBuilt is what the container is when eyedbg's override isn't applied.
	asBuilt container.FastInfo
}

// upCall is one 'docker compose up' the fake docker was asked for.
type upCall struct {
	ref      container.ProjectRef
	override string
	services []string
	wait     bool
}

// fakeStack is the fake docker: a set of containers, the calls made on it in
// order, and the failures to inject.
type fakeStack struct {
	t   *testing.T
	dir string

	mu     sync.Mutex
	ctrs   map[string]*fakeCtr
	nextID int
	log    []string
	ups    []upCall

	// upErr answers a 'compose up' (nil: it works); probeErr and adapterErr
	// are per service; listErr fails FastContainers.
	upErr      func(c upCall) error
	probeErr   map[string]error
	adapterErr map[string]error
	listErr    error
	// onUp runs inside a 'compose up' before it takes effect.
	onUp func(c upCall)
}

func (f *fakeStack) recordf(format string, args ...any) {
	f.log = append(f.log, fmt.Sprintf(format, args...))
}

// upCalls are the 'compose up' calls so far.
func (f *fakeStack) upCalls() []upCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.ups)
}

func (f *fakeStack) newID() string {
	f.nextID++

	return fmt.Sprintf("%064x", f.nextID)
}

// add creates a container for service as built: exec-form dotnet entrypoint
// dll, no command, /app, created from compose.yml and compose.override.yml.
func (f *fakeStack) add(service, dll string) *fakeCtr {
	f.mu.Lock()
	defer f.mu.Unlock()

	fi := container.FastInfo{
		ID: f.newID(), Name: worldProject + "-" + service + "-1", Image: "sha256:" + strings.Repeat("e", 64), Platform: "linux",
		Running: true, DLL: dll, WorkDir: "/app", Project: worldProject, Service: service, ComposeDir: f.dir,
		ConfigFiles: []string{filepath.Join(f.dir, "compose.yml"), filepath.Join(f.dir, "compose.override.yml")},
	}
	c := &fakeCtr{fi: fi, asBuilt: fi}
	f.ctrs[fi.ID] = c

	return c
}

// byService is the container of service.
func (f *fakeStack) byService(service string) *fakeCtr {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.ctrs {
		if c.fi.Service == service {
			return c
		}
	}

	f.t.Fatalf("no container for service %s", service)

	return nil
}

// idOf is the current id of service's container.
func (f *fakeStack) idOf(service string) string { return f.byService(service).fi.ID }

// rows are what 'docker compose ps' shows: the running containers.
func (f *fakeStack) rows() []container.ComposeService {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []container.ComposeService

	for _, c := range f.ctrs {
		state := "exited"
		if c.fi.Running {
			state = "running"
		}

		out = append(out, container.ComposeService{ID: c.fi.ID, Name: c.fi.Name, Service: c.fi.Service, Project: c.fi.Project, State: state})
	}

	return out
}

func (f *fakeStack) InspectFast(_ context.Context, ref string) (container.FastInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c, ok := f.ctrs[ref]
	if !ok {
		return container.FastInfo{}, api.NewError(api.CodeInvalidRequest, "no container "+ref, "")
	}

	f.recordf("inspect %s", c.fi.Service)

	return c.fi, nil
}

func (f *fakeStack) ProbeIdle(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := f.ctrs[ref]
	f.recordf("probe %s", c.fi.Service)

	return f.probeErr[c.fi.Service]
}

func (f *fakeStack) FastContainers(_ context.Context, project string) ([]container.FastContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.recordf("list %s", project)

	if f.listErr != nil {
		return nil, f.listErr
	}

	var out []container.FastContainer

	for _, c := range f.ctrs {
		if c.fi.Project == project && c.fi.Fast != nil {
			out = append(out, container.FastContainer{ID: c.fi.ID, Service: c.fi.Service})
		}
	}

	slices.SortFunc(out, func(a, b container.FastContainer) int { return strings.Compare(a.Service+a.ID, b.Service+b.ID) })

	return out, nil
}

// overrideFile is the override as eyedbg wrote it: the header, then JSON.
type overrideFile struct {
	Services map[string]struct {
		Entrypoint []string          `json:"entrypoint"`
		Command    []string          `json:"command"`
		Init       bool              `json:"init"`
		Labels     map[string]string `json:"labels"`
		Volumes    []struct {
			Source   string `json:"source"`
			Target   string `json:"target"`
			ReadOnly bool   `json:"read_only"` //nolint:tagliatelle // Compose's own key.
		} `json:"volumes"`
	} `json:"services"`
}

// readOverride parses the override file at path.
func readOverride(t *testing.T, path string) overrideFile {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the override: %v", err)
	}

	header, body, _ := bytes.Cut(raw, []byte("\n"))
	if string(header) != container.OverrideHeader {
		t.Fatalf("override header = %q", header)
	}

	var doc overrideFile
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("override is not JSON: %v\n%s", err, body)
	}

	return doc
}

func (f *fakeStack) ComposeUp(_ context.Context, ref container.ProjectRef, override string, services []string, wait bool) error {
	f.mu.Lock()
	hook := f.onUp
	f.mu.Unlock()

	call := upCall{ref: ref, override: override, services: slices.Clone(services), wait: wait}
	if hook != nil {
		hook(call)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.ups = append(f.ups, call)
	f.recordf("up %s", strings.Join(services, ","))

	if err := ref.Validate(); err != nil {
		f.t.Errorf("ComposeUp with an invalid project: %v", err)
	}

	if override != "" && slices.Contains(ref.Files, override) {
		f.t.Errorf("the override is among the compose files: %v", ref.Files)
	}

	if f.upErr != nil {
		if err := f.upErr(call); err != nil {
			return err
		}
	}

	var doc overrideFile
	if override != "" {
		doc = readOverride(f.t, override)
	}

	for _, name := range services {
		c := f.findLocked(name)
		if c == nil {
			f.t.Errorf("compose up names %s, which has no container", name)

			continue
		}

		delete(f.ctrs, c.fi.ID)

		next := f.recreated(c, call, doc)
		f.ctrs[next.fi.ID] = &next
	}

	return nil
}

// recreated is the container 'compose up' makes of c: a new id, and either as
// built or, with an override, idle with the build mounted and eyedbg's labels.
func (f *fakeStack) recreated(c *fakeCtr, call upCall, doc overrideFile) fakeCtr {
	next := c.asBuilt
	next.ID = f.newID()
	next.Running = true

	if call.override == "" {
		next.ConfigFiles = slices.Clone(call.ref.Files)

		return fakeCtr{fi: next, asBuilt: c.asBuilt}
	}

	name := c.fi.Service

	frag, ok := doc.Services[name]
	if !ok || len(frag.Volumes) != 1 {
		f.t.Errorf("the override has no fragment for %s", name)

		return fakeCtr{fi: next, asBuilt: c.asBuilt}
	}

	// What the mount source holds is what the app runs: a recreate after the
	// mirror.
	if _, err := os.Stat(filepath.Join(frag.Volumes[0].Source, "marker.txt")); err != nil {
		f.t.Errorf("the build of %s isn't in %s when the container is recreated: %v", name, frag.Volumes[0].Source, err)
	}

	next.DLL = ""
	next.ConfigFiles = append(slices.Clone(call.ref.Files), call.override)
	next.Fast = &container.FastLabels{
		Version: frag.Labels["dev.izzat.eyedbg.fast.version"], Override: frag.Labels["dev.izzat.eyedbg.fast.override"],
		DLL: frag.Labels["dev.izzat.eyedbg.fast.dll"], WorkDir: frag.Labels["dev.izzat.eyedbg.fast.workdir"],
		Project: frag.Labels["dev.izzat.eyedbg.fast.project"],
	}
	next.Fast.ProjectPath = filepath.Join(f.dir, filepath.FromSlash(next.Fast.Project))

	return fakeCtr{fi: next, asBuilt: c.asBuilt}
}

func (f *fakeStack) findLocked(service string) *fakeCtr {
	for _, c := range f.ctrs {
		if c.fi.Service == service {
			return c
		}
	}

	return nil
}

// publishCall is one 'dotnet publish' of the fake.
type publishCall struct {
	spec dotnet.PublishSpec
	// log is what the fake wrote to the build log's file.
	at time.Time
}

// launchWorld is a compose stack with everything outside eyedbg faked.
type launchWorld struct {
	t *testing.T

	// dir is the compose directory (resolved); home eyedbg's home.
	dir, home string
	docker    *fakeStack

	mu sync.Mutex
	// builds counts publishes: each leaves "build-N" in marker.txt.
	builds    int
	publishes []publishCall
	// publishErr fails a publish (nil: it works), by project file name.
	publishErr map[string]error
	// publishHook runs inside a publish; publishExtra after it wrote the
	// build, with the output directory.
	publishHook  func(spec dotnet.PublishSpec)
	publishExtra func(out string)
	// launches are the container.launch members the driver prepared, in
	// order, with what the service's directory held at that moment.
	launches []launchRecord
	// launchErr fails a launch of a service.
	launchErr map[string]error
	hostErr   error
	// psCalls counts 'docker compose ps' runs.
	psCalls int
	// engine, when set, is the docker the commands use instead of the fake
	// stack: a fake docker CLI, to see the real argv.
	engine stackDocker
}

// launchRecord is one launch the fake driver prepared.
type launchRecord struct {
	spec   api.ContainerLaunchSpec
	marker string
	// override: the override file existed when the app was launched.
	override bool
}

// newLaunchWorld builds the stack: services producer and consumer (each its
// own project), consumer2 (sharing consumer's project) and db (not .NET). Not
// parallel: it sets environment variables for the daemon.
func newLaunchWorld(t *testing.T) *launchWorld {
	t.Helper()

	p := isolate(t)
	t.Setenv(envDockerHost, "")
	t.Setenv(envDockerContext, "")
	t.Setenv(envGroup, "")

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"Producer/Producer.csproj", "Consumer/Consumer.csproj", "producer.txt", "consumer.txt", "consumer2.txt", "compose.yml"} {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	w := &launchWorld{t: t, dir: dir, home: os.Getenv(adapters.EnvHome), publishErr: map[string]error{}, launchErr: map[string]error{}}
	w.docker = &fakeStack{t: t, dir: dir, ctrs: map[string]*fakeCtr{}, probeErr: map[string]error{}, adapterErr: map[string]error{}}

	w.docker.add("consumer", "Consumer.dll")
	w.docker.add("consumer2", "Consumer.dll")
	w.docker.add("db", "")
	w.docker.add("producer", "Producer.dll")

	// A database is not a dotnet exec-form container.
	db := w.docker.byService("db")
	db.fi.DLL, db.asBuilt.DLL = "", ""

	serveWithDrivers(t, p, &launchDriver{w: w})

	return w
}

func (w *launchWorld) deps() composeDeps {
	return composeDeps{
		now: time.Now,
		ps: func(context.Context, string, string, container.ComposeOptions) ([]container.ComposeService, error) {
			w.mu.Lock()
			w.psCalls++
			w.mu.Unlock()

			// docker compose ps lists the running containers only.
			return slices.DeleteFunc(w.docker.rows(), func(r container.ComposeService) bool { return r.State != "running" }), nil
		},
		stack: stackDeps{
			docker: func(string, string) (stackDocker, error) {
				if w.engine != nil {
					return w.engine, nil
				}

				return w.docker, nil
			},
			ensureAdapter: func(_ context.Context, _, _ string, fi container.FastInfo) error {
				w.docker.mu.Lock()
				defer w.docker.mu.Unlock()

				w.docker.recordf("adapter %s", fi.Service)

				return w.docker.adapterErr[fi.Service]
			},
			publish:    w.publish,
			dotnetHost: func() (string, error) { return "dotnet-fake", w.hostErr },
			root:       func() (string, error) { return artifacts.Dir(artifacts.Compose) },
		},
	}
}

// publish is the fake 'dotnet publish': a build with a new marker.
func (w *launchWorld) publish(_ context.Context, spec dotnet.PublishSpec) error {
	w.mu.Lock()
	hook := w.publishHook
	w.mu.Unlock()

	if hook != nil {
		hook(spec)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.builds++
	w.publishes = append(w.publishes, publishCall{spec: spec, at: time.Now()})
	w.docker.mu.Lock()
	w.docker.recordf("publish %s", filepath.Base(spec.Project))
	w.docker.mu.Unlock()

	if err := w.publishErr[filepath.Base(spec.Project)]; err != nil {
		if spec.Log != nil {
			_, _ = spec.Log.WriteString("error CS1002: ; expected\n")
		}

		return err
	}

	if err := os.MkdirAll(spec.Out, 0o700); err != nil {
		return fmt.Errorf("fake publish: %w", err)
	}

	name := strings.TrimSuffix(spec.DLL, ".dll")

	for file, content := range map[string]string{spec.DLL: "dll", name + ".pdb": "pdb", "marker.txt": "build-" + strconv.Itoa(w.builds)} {
		if err := os.WriteFile(filepath.Join(spec.Out, file), []byte(content), 0o600); err != nil {
			return fmt.Errorf("fake publish: %w", err)
		}
	}

	if w.publishExtra != nil {
		w.publishExtra(spec.Out)
	}

	return nil
}

func (w *launchWorld) publishedProjects() []string {
	w.mu.Lock()
	defer w.mu.Unlock()

	var out []string
	for i := range w.publishes {
		out = append(out, filepath.Base(w.publishes[i].spec.Project))
	}

	return out
}

// override is the project's override path.
func (w *launchWorld) override() string {
	return filepath.Join(w.home, artifacts.Compose, worldProject, container.OverrideName)
}

// serviceDir is where a service's build is mounted from.
func (w *launchWorld) serviceDir(service string) string {
	return filepath.Join(w.home, artifacts.Compose, worldProject, "services", service)
}

// marker is the marker of the build in service's directory ("" when none).
func (w *launchWorld) marker(service string) string {
	raw, err := os.ReadFile(filepath.Join(w.serviceDir(service), "marker.txt"))
	if err != nil {
		return ""
	}

	return string(raw)
}

// projectDirExists reports whether eyedbg's directory for the project exists.
func (w *launchWorld) projectDirExists() bool {
	_, err := os.Stat(filepath.Join(w.home, artifacts.Compose, worldProject))

	return err == nil
}

// stageDirs are the staging directories left in the project's directory.
func (w *launchWorld) stageDirs() []string {
	entries, _ := os.ReadDir(filepath.Join(w.home, artifacts.Compose, worldProject))

	var out []string

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "stage-") {
			out = append(out, e.Name())
		}
	}

	return out
}

func (w *launchWorld) launched() []launchRecord {
	w.mu.Lock()
	defer w.mu.Unlock()

	return slices.Clone(w.launches)
}

// launchedServices are the services of the launches so far, in order.
func (w *launchWorld) launchedServices() []string {
	launches := w.launched()

	var out []string
	for i := range launches {
		out = append(out, launches[i].spec.Service)
	}

	return out
}

// run executes eyedbg with args and checks the exit code.
func (w *launchWorld) run(wantCode int, args ...string) (stdout, stderr string) {
	w.t.Helper()

	out, errOut, code := execute(w.t, newEyedbgCommand(testInfo, w.deps()), args)
	if code != wantCode {
		w.t.Fatalf("eyedbg %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, wantCode, out, errOut)
	}

	return string(out), string(errOut)
}

// sessions are the daemon's sessions by service.
func (w *launchWorld) sessions() map[string]api.SessionInfo {
	w.t.Helper()

	out, _ := w.run(0, "sessions", "--json")

	var doc struct {
		Sessions []api.SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		w.t.Fatalf("sessions --json: %v\n%s", err, out)
	}

	byService := map[string]api.SessionInfo{}

	for i := range doc.Sessions {
		if s := &doc.Sessions[i]; s.Container != nil && s.State != api.StateExited {
			byService[s.Container.Service] = *s
		}
	}

	return byService
}

// launchJSON runs 'compose launch --json' with args and decodes it.
func (w *launchWorld) launchJSON(wantCode int, args ...string) []launchedMember {
	w.t.Helper()

	out, _ := w.run(wantCode, append([]string{"compose", "launch", "--json"}, args...)...)

	var doc struct {
		Schema  int              `json:"schema"`
		Members []launchedMember `json:"members"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Schema != 1 {
		w.t.Fatalf("compose launch --json: %v\n%s", err, out)
	}

	return doc.Members
}

// member is the member of service in a launch result.
func member(t *testing.T, members []launchedMember, service string) launchedMember {
	t.Helper()

	for _, m := range members {
		if m.Service == service {
			return m
		}
	}

	t.Fatalf("no member for %s in %+v", service, members)

	return launchedMember{}
}

// launchDriver is the daemon's fake .NET driver for containers: it launches
// and attaches on the fake adapter, whose program is /src/<service>.txt.
type launchDriver struct {
	fakeDriver

	w *launchWorld
}

func (*launchDriver) Name() string { return dotnet.Language }

// container is the fake container with the full id id.
func (d *launchDriver) container(id string) (*fakeCtr, error) {
	d.w.docker.mu.Lock()
	defer d.w.docker.mu.Unlock()

	if c, ok := d.w.docker.ctrs[id]; ok {
		return c, nil
	}

	return nil, api.NewError(api.CodeInvalidRequest, "no container "+id, "")
}

func (d *launchDriver) PrepareContainerLaunch(_ context.Context, spec api.ContainerLaunchSpec) (session.Launch, error) {
	c, err := d.container(spec.Ref)
	if err != nil {
		return session.Launch{}, err
	}

	d.w.mu.Lock()
	failure := d.w.launchErr[spec.Service]

	_, overrideErr := os.Stat(d.w.override())
	rec := launchRecord{spec: spec, marker: d.w.marker(spec.Service), override: overrideErr == nil}
	d.w.launches = append(d.w.launches, rec)
	d.w.mu.Unlock()

	if failure != nil {
		return session.Launch{}, failure
	}

	return d.launchOn(c, session.RequestLaunch, spec.Service, spec.Map, spec.StopOnEntry, 0)
}

func (d *launchDriver) PrepareContainerAttach(_ context.Context, spec api.AttachSpec) (session.Launch, error) {
	cs := spec.Container

	c, err := d.container(cs.Ref)
	if err != nil {
		return session.Launch{}, err
	}

	return d.launchOn(c, session.RequestAttach, c.fi.Service, cs.Map, false, 1)
}

func (d *launchDriver) launchOn(c *fakeCtr, request, service string, maps []api.PathMapping, stopOnEntry bool, pid int) (session.Launch, error) {
	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	if len(maps) == 0 {
		maps = []api.PathMapping{{Remote: "/src", Local: d.w.dir}}
	}

	pm, err := session.NewPathMap(maps)
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Request: request, PID: pid,
		Arguments: daptest.ProgramArgs{Program: "/src/" + service + ".txt", Lines: 10, StopAtEntry: stopOnEntry, Hang: true}.Map(),
		Program:   service + " (container " + c.fi.Name + ")", PathMap: pm,
		Container: &api.ContainerInfo{
			ID: c.fi.ID, Name: c.fi.Name, Service: service, Project: c.fi.Project, PID: pid, Platform: "linux/arm64",
			Map: pm.Mappings(), UnhealthyAfter: api.Duration(18 * time.Second),
		},
	}, nil
}
