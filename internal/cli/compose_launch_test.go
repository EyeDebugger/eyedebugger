// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

// TestComposeLaunchCycle: the whole life of a stack in fast mode, through the
// real commands, an in-process daemon and fakes for docker and dotnet: enter
// (recreate), rebuild (same container), relaunch without a build, a launch of
// everything, then restore one service and the rest.
func TestComposeLaunchCycle(t *testing.T) {
	w := newLaunchWorld(t)

	w.enter(t)
	w.rebuild(t)
	w.noBuild(t)
	w.everything(t)
	w.restore(t)
}

// enter: three services enter fast mode at once, one build per project, one
// recreate naming them all, a startup breakpoint in every launch.
func (w *launchWorld) enter(t *testing.T) {
	t.Helper()

	producerTxt := filepath.Join(w.dir, "producer.txt")
	oldIDs := map[string]string{"producer": w.docker.idOf("producer"), "consumer": w.docker.idOf("consumer"), "consumer2": w.docker.idOf("consumer2")}

	out, _ := w.run(0, "compose", "launch", "--bp", producerTxt+":3")
	expectOutput(t, out, "group myapp: launched 3 of 4 service(s)", "producer", "consumer2", "built Producer/Producer.csproj in", "recreated -> launched",
		"db", "skipped", "path map: /src="+w.dir, "breakpoint "+producerTxt+":3:", "override: "+w.override(), "build log: ",
		"recreated with this shell's compose environment", "app runs only while its eyedbg session runs it",
		"undo with: eyedbg compose restore consumer consumer2 producer", "next: eyedbg compose wait -g myapp")

	w.checkPublishes(t)
	w.checkEntryRecreate(t)

	w.checkOverride(t, "consumer", "consumer2", "producer")
	w.checkEntryLaunches(t, oldIDs)
	w.checkEntrySessions(t)

	out, _ = w.run(0, "compose", "bp", "ls")
	expectOutput(t, out, "producer.txt:3")
}

// checkEntryLaunches: each launch followed the mirror and the override, in the
// recreated container (not the old one), with the path map.
func (w *launchWorld) checkEntryLaunches(t *testing.T, oldIDs map[string]string) {
	t.Helper()

	launches := w.launched()
	if len(launches) != 3 {
		t.Fatalf("%d launches", len(launches))
	}

	for i := range launches {
		l := &launches[i]
		if l.marker == "" || !l.override || l.spec.Ref == oldIDs[l.spec.Service] || l.spec.Ref != w.docker.idOf(l.spec.Service) ||
			l.spec.Project != worldProject || len(l.spec.Map) != 1 || l.spec.Map[0] != (api.PathMapping{Remote: "/src", Local: w.dir}) {
			t.Errorf("launch %+v (old id %s)", l, oldIDs[l.spec.Service])
		}
	}
}

// checkEntrySessions: shared builds share a marker, the staging directory is
// gone, and every service has a launched session of the group.
func (w *launchWorld) checkEntrySessions(t *testing.T) {
	t.Helper()

	if w.marker("consumer") == "" || w.marker("consumer") != w.marker("consumer2") || w.marker("producer") == w.marker("consumer") {
		t.Errorf("markers: consumer %q consumer2 %q producer %q", w.marker("consumer"), w.marker("consumer2"), w.marker("producer"))
	}

	if stages := w.stageDirs(); len(stages) != 0 {
		t.Errorf("staging directories left: %v", stages)
	}

	sessions := w.sessions()
	for service := range sessions {
		if s := sessions[service]; s.Group != worldProject || s.Container == nil || !s.Container.Launched || s.Container.Service != service {
			t.Errorf("session of %s = %+v", service, s)
		}
	}

	if len(sessions) != 3 {
		t.Errorf("%d sessions, want 3", len(sessions))
	}
}

// checkPublishes: one build per distinct project (consumer and consumer2 share
// one), into the run's staging directory, with eyedbg's own artifacts path.
func (w *launchWorld) checkPublishes(t *testing.T) {
	t.Helper()

	if got := w.publishedProjects(); !slices.Equal(got, []string{"Consumer.csproj", "Producer.csproj"}) {
		t.Errorf("published %v", got)
	}

	for i := range w.publishes {
		spec := w.publishes[i].spec
		if spec.Host != "dotnet-fake" || spec.Root != w.dir || !strings.HasPrefix(spec.Out, filepath.Join(w.home, "compose", worldProject, "stage-")) ||
			spec.Artifacts != filepath.Join(w.home, "compose", worldProject, "artifacts") {
			t.Errorf("publish spec = %+v", spec)
		}
	}
}

// checkEntryRecreate: one recreate, for the three services only, from the
// containers' own files, with the override last and no --wait.
func (w *launchWorld) checkEntryRecreate(t *testing.T) {
	t.Helper()

	ups := w.docker.upCalls()
	if len(ups) != 1 {
		t.Fatalf("%d compose up calls, want 1", len(ups))
	}

	up := ups[0]
	if !slices.Equal(up.services, []string{"consumer", "consumer2", "producer"}) || up.wait || up.override != w.override() ||
		up.ref.Name != worldProject || up.ref.WorkDir != w.dir ||
		!slices.Equal(up.ref.Files, []string{filepath.Join(w.dir, "compose.yml"), filepath.Join(w.dir, "compose.override.yml")}) {
		t.Errorf("compose up = %+v", up)
	}
}

// checkOverride checks the override file: a fragment for exactly these
// services, with only the idle entrypoint, the mount and the labels.
func (w *launchWorld) checkOverride(t *testing.T, services ...string) {
	t.Helper()

	doc := readOverride(t, w.override())
	if got := sortedMapKeys(doc.Services); !slices.Equal(got, services) {
		t.Fatalf("the override has fragments for %v, want %v", got, services)
	}

	for name := range doc.Services {
		w.checkFragment(t, name, doc)
	}
}

// checkFragment checks one service's fragment of the override: only the idle
// entrypoint, no command, init, the read-only mount and eyedbg's five labels.
func (w *launchWorld) checkFragment(t *testing.T, name string, doc overrideFile) {
	t.Helper()

	frag := doc.Services[name]
	dll := strings.ToUpper(name[:1]) + strings.TrimRight(name[1:], "2") + ".dll"
	project := strings.TrimSuffix(dll, ".dll") + "/" + strings.TrimSuffix(dll, ".dll") + ".csproj"

	if !slices.Equal(frag.Entrypoint, []string{"tail", "-f", "/dev/null"}) || len(frag.Command) != 0 || !frag.Init {
		t.Errorf("fragment of %s idles wrongly: %+v", name, frag)
	}

	if len(frag.Volumes) != 1 || frag.Volumes[0].Source != w.serviceDir(name) || frag.Volumes[0].Target != "/app" || !frag.Volumes[0].ReadOnly {
		t.Errorf("fragment of %s mounts wrongly: %+v", name, frag.Volumes)
	}

	labels := map[string]string{
		"dev.izzat.eyedbg.fast.version": "1", "dev.izzat.eyedbg.fast.override": w.override(), "dev.izzat.eyedbg.fast.dll": dll,
		"dev.izzat.eyedbg.fast.workdir": "/app", "dev.izzat.eyedbg.fast.project": project,
	}
	if len(frag.Labels) != len(labels) {
		t.Errorf("fragment of %s has labels %v", name, frag.Labels)
	}

	for k, v := range labels {
		if frag.Labels[k] != v {
			t.Errorf("fragment of %s: label %s = %q, want %q", name, k, frag.Labels[k], v)
		}
	}
}

// rebuild: re-running for one service builds it while its old app runs,
// stops that session, replaces its files and relaunches it in the SAME
// container; the breakpoint comes along; nothing else is touched.
func (w *launchWorld) rebuild(t *testing.T) {
	t.Helper()

	before := w.sessions()
	producerID := w.docker.idOf("producer")
	consumerMarker, producerMarker := w.marker("consumer"), w.marker("producer")
	launches := len(w.launched())

	// The build runs while the old app is still running, and its files are
	// still the old build's.
	var building struct {
		running bool
		marker  string
		session string
	}

	w.publishHook = func(dotnet.PublishSpec) {
		s, ok := w.sessions()["producer"]
		building.running, building.marker, building.session = ok && s.State != api.StateExited, w.marker("producer"), s.ID
	}

	out, _ := w.run(0, "compose", "launch", "producer")

	if !building.running || building.marker != producerMarker || building.session != before["producer"].ID {
		t.Errorf("while building: %+v, want the old app running with the old files (%s, %s)", building, producerMarker, before["producer"].ID)
	}

	expectOutput(t, out, "launched 1 of 1 service(s)", "built Producer/Producer.csproj in", "launched (1 breakpoint carried)")

	if strings.Contains(out, "recreated") {
		t.Errorf("a rebuild says it recreated:\n%s", out)
	}

	if n := len(w.docker.upCalls()); n != 1 {
		t.Errorf("a rebuild ran compose up (%d calls)", n)
	}

	if got := w.docker.idOf("producer"); got != producerID {
		t.Errorf("the container was recreated: %s -> %s", producerID, got)
	}

	after := w.sessions()
	if after["producer"].ID == before["producer"].ID || after["consumer"].ID != before["consumer"].ID {
		t.Errorf("sessions: producer %s -> %s, consumer %s -> %s", before["producer"].ID, after["producer"].ID, before["consumer"].ID, after["consumer"].ID)
	}

	if w.marker("producer") == producerMarker || w.marker("consumer") != consumerMarker {
		t.Errorf("markers: producer %q (was %q), consumer %q (was %q)", w.marker("producer"), producerMarker, w.marker("consumer"), consumerMarker)
	}

	if l := w.launched(); len(l) != launches+1 || l[launches].marker != w.marker("producer") || l[launches].spec.Ref != producerID {
		t.Errorf("launches = %+v", l)
	}

	out, _ = w.run(0, "compose", "bp", "ls", "--mine")
	expectOutput(t, out, "producer.txt:3")
}

// noBuild: --no-build relaunches from the last build: no publish, no mirror,
// no recreate.
func (w *launchWorld) noBuild(t *testing.T) {
	t.Helper()

	w.publishHook = nil

	publishes, marker, before := len(w.publishedProjects()), w.marker("producer"), w.sessions()["producer"].ID

	out, _ := w.run(0, "compose", "launch", "--no-build", "producer")
	expectOutput(t, out, "launched 1 of 1 service(s)", "from the last build -> launched")

	if len(w.publishedProjects()) != publishes || w.marker("producer") != marker || len(w.docker.upCalls()) != 1 {
		t.Errorf("--no-build built or recreated: %v, marker %q, %d ups", w.publishedProjects(), w.marker("producer"), len(w.docker.upCalls()))
	}

	if w.sessions()["producer"].ID == before {
		t.Error("the app was not relaunched")
	}
}

// everything: with no names, every service in fast mode is rebuilt, the
// other (the database) is skipped, and nothing is recreated.
func (w *launchWorld) everything(t *testing.T) {
	t.Helper()

	before := w.sessions()

	out, _ := w.run(0, "compose", "launch")
	expectOutput(t, out, "launched 3 of 4 service(s)", "db", "skipped", "exec-form")

	if len(w.docker.upCalls()) != 1 {
		t.Errorf("compose up ran again: %d calls", len(w.docker.upCalls()))
	}

	after := w.sessions()
	for service := range after {
		if after[service].ID == before[service].ID {
			t.Errorf("%s was not relaunched", service)
		}
	}
}

// restore: one service goes back as built (its session ended, its fragment
// and directory gone, the others kept), then the rest (everything of
// eyedbg's is removed), then nothing is left to restore.
func (w *launchWorld) restore(t *testing.T) {
	t.Helper()

	w.restoreOne(t)
	w.restoreRest(t)
}

func (w *launchWorld) restoreOne(t *testing.T) {
	t.Helper()

	producer := w.sessions()["producer"].ID

	out, _ := w.run(0, "compose", "restore", "producer")
	expectOutput(t, out, "group myapp: restored 1 of 1 service(s)", "producer", "recreated as built; ended 1 session ("+producer+")",
		"recreated as built, with this shell's compose environment")

	ups := w.docker.upCalls()
	if len(ups) != 2 {
		t.Fatalf("%d compose up calls, want 2", len(ups))
	}

	up := ups[1]
	if !slices.Equal(up.services, []string{"producer"}) || !up.wait || up.override != "" ||
		!slices.Equal(up.ref.Files, []string{filepath.Join(w.dir, "compose.yml"), filepath.Join(w.dir, "compose.override.yml")}) {
		t.Errorf("restore's compose up = %+v", up)
	}

	if c := w.docker.byService("producer"); c.fi.Fast != nil || c.fi.DLL != "Producer.dll" {
		t.Errorf("producer is not as built: %+v", c.fi)
	}

	if _, ok := w.sessions()["producer"]; ok {
		t.Error("producer still has a session")
	}

	w.checkOverride(t, "consumer", "consumer2")

	if _, err := os.Stat(w.serviceDir("producer")); !os.IsNotExist(err) {
		t.Errorf("producer's build directory is still there: %v", err)
	}

	if w.marker("consumer") == "" {
		t.Error("consumer's build was removed")
	}
}

func (w *launchWorld) restoreRest(t *testing.T) {
	t.Helper()

	out, _ := w.run(0, "compose", "restore")
	expectOutput(t, out, "restored 2 of 2 service(s)", "consumer", "consumer2", "eyedbg's files for this project were removed")

	if w.projectDirExists() {
		t.Error("eyedbg's project directory is still there")
	}

	if len(w.sessions()) != 0 {
		t.Errorf("sessions left: %v", w.sessions())
	}

	_, errOut := w.run(exitError, "compose", "restore")
	expectOutput(t, errOut, "[INVALID_REQUEST]", "nothing is in fast mode")

	for _, c := range w.docker.ctrs {
		if c.fi.Fast != nil {
			t.Errorf("%s is still in fast mode", c.fi.Service)
		}
	}

	if len(w.docker.upCalls()) != 3 {
		t.Errorf("%d compose up calls, want 3", len(w.docker.upCalls()))
	}
}

// TestComposeLaunchHelp: the commands' flags and the promises their help makes.
func TestComposeLaunchHelp(t *testing.T) {
	t.Parallel()

	root := newEyedbgCommand(testInfo, defaultComposeDeps())

	launch, _, err := root.Find([]string{"compose", "launch"})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{"file", "project-name", "project-directory", "dotnet-project", "env-file", "bp", "exceptions", "lease-policy", "no-record", "no-build", "stop-on-entry"} {
		if launch.Flags().Lookup(f) == nil {
			t.Errorf("compose launch has no --%s", f)
		}
	}

	// The path map is fixed by the build; the override is eyedbg's own.
	for _, f := range []string{"map", "out", "adapter", "timeout"} {
		if launch.Flags().Lookup(f) != nil {
			t.Errorf("compose launch has --%s", f)
		}
	}

	for _, want := range []string{
		"RECREATES", "SAME container", "DOWN", "docker compose logs", "read-only", "command: or image CMD", "service_healthy",
		"A compile error therefore changes nothing", "compose restore", "--no-build", "chiseled", "never reads", "no --map",
		"read-write bind mount", "driver_opts type none", "isn't a path on this\nmachine", "--dotnet-project SERVICE=PATH", "untrusted input", "Not supported: compose files outside the project directory",
	} {
		if !strings.Contains(launch.Long, want) {
			t.Errorf("compose launch's help lacks %q", want)
		}
	}

	restore, _, err := root.Find([]string{"compose", "restore"})
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{"file", "project-name", "project-directory", "env-file"} {
		if restore.Flags().Lookup(f) == nil {
			t.Errorf("compose restore has no --%s", f)
		}
	}

	for _, want := range []string{"force-recreate", "--wait", "Dependents", "not in fast mode", "nothing is launched"} {
		if !strings.Contains(restore.Long, want) {
			t.Errorf("compose restore's help lacks %q", want)
		}
	}
}

// TestComposeLaunchRealEngine: through the real docker wrapper over a fake
// docker CLI, what reaches docker is exactly what the contract says, in this
// order: inspect, the probe, the list of fast containers, then one compose up
// for the named service only.
func TestComposeLaunchRealEngine(t *testing.T) {
	w := newLaunchWorld(t)

	producer := w.docker.byService("producer").fi
	configFiles := strings.Join(producer.ConfigFiles, ",")

	inspect := `["` + producer.ID + `","/` + producer.Name + `","` + producer.Image + `","linux",true,false,false,"Producer.dll",false,"/app","` + worldProject +
		`","producer","` + w.dir + `","` + configFiles + `",null,null,null,null,null]`

	calls := filepath.Join(t.TempDir(), "calls")
	w.engine = containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{
		{Match: []string{"inspect", "--type", "container"}, Stdout: inspect},
		{Match: []string{"exec"}, Stdout: "tail (GNU coreutils) 9.4\n"},
		{Match: []string{"ps", "--all"}},
		{Match: []string{"compose"}, Exit: 1, Stderr: "service producer: bind source missing"},
	}})

	envFile := filepath.Join(w.dir, "prod.env")

	_, errOut := w.run(exitCodeOf(api.CodeAttachFailed), "compose", "launch", "producer", "--env-file", envFile)
	expectOutput(t, errOut, "[ATTACH_FAILED]", "bind source missing", "eyedbg compose restore producer")

	var subs []string

	argvs := containertest.ReadCalls(t, calls)
	for _, argv := range argvs {
		subs = append(subs, argv[0])
	}

	// The project is searched for after the read-write bind mounts of the
	// stack's containers were asked for (a ps; no container answers here). A
	// failed recreate prunes nothing and reads nothing back.
	if want := []string{"inspect", "exec", "ps", "ps", "compose"}; !slices.Equal(subs, want) {
		t.Fatalf("docker ran %v, want %v", subs, want)
	}

	if got, want := argvs[1], []string{"exec", producer.ID, "tail", "--version"}; !slices.Equal(got, want) {
		t.Errorf("probe = %q, want %q", got, want)
	}

	want := []string{
		"compose", "--project-name=" + worldProject, "--project-directory=" + w.dir,
		"--file=" + producer.ConfigFiles[0], "--file=" + producer.ConfigFiles[1], "--file=" + w.override(), "--env-file=" + envFile,
		"up", "--detach", "--no-deps", "--force-recreate", "--no-build", "producer",
	}
	if got := argvs[4]; !slices.Equal(got, want) {
		t.Errorf("compose up = %q\nwant         %q", got, want)
	}
}
