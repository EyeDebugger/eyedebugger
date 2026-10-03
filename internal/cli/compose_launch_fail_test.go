// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/container"
)

// Failure paths of 'compose launch'. Each one is about what is NOT changed:
// the order of the stages is the safety property.

// changed lists what changed in the stack and in the daemon since snapshot:
// containers recreated, sessions replaced or ended, builds published, apps
// launched.
type snapshot struct {
	ids      map[string]string
	sessions map[string]string
	ups      int
	builds   int
	launches int
}

func (w *launchWorld) snap() snapshot {
	s := snapshot{ids: map[string]string{}, sessions: map[string]string{}, ups: len(w.docker.upCalls()), builds: len(w.publishedProjects()), launches: len(w.launched())}

	for _, c := range w.docker.rows() {
		s.ids[c.Service] = c.ID
	}

	sessions := w.sessions()
	for service := range sessions {
		s.sessions[service] = sessions[service].ID
	}

	return s
}

// unchangedSince fails the test if the stack, the sessions or the apps changed.
func (w *launchWorld) unchangedSince(t *testing.T, before snapshot) {
	t.Helper()

	after := w.snap()
	if after.ups != before.ups || after.launches != before.launches || !mapsEqual(after.ids, before.ids) || !mapsEqual(after.sessions, before.sessions) {
		t.Errorf("something changed:\nbefore %+v\nafter  %+v", before, after)
	}

	if stages := w.stageDirs(); len(stages) != 0 {
		t.Errorf("staging directories left: %v", stages)
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		if b[k] != v {
			return false
		}
	}

	return true
}

// enterNamed enters the named services (their output is not the point).
func (w *launchWorld) enterNamed(names ...string) {
	w.t.Helper()

	w.run(0, append([]string{"compose", "launch"}, names...)...)
}

// TestComposeLaunchBuildFailure: a compile error changes nothing for its
// service: the old app keeps running and keeps its files, no container is
// recreated, and the other services proceed.
func TestComposeLaunchBuildFailure(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer", "consumer")

	before := w.snap()
	producerMarker, consumerMarker := w.marker("producer"), w.marker("consumer")

	w.publishErr["Producer.csproj"] = api.NewError(api.CodeBuildFailed, "dotnet publish failed", "see the build log")

	members := w.launchJSON(0, "producer", "consumer")

	if m := member(t, members, "producer"); m.Error == nil || m.Error.Code != api.CodeBuildFailed || m.Session != nil {
		t.Errorf("producer = %+v", m)
	}

	if m := member(t, members, "consumer"); m.Session == nil {
		t.Errorf("consumer = %+v", m)
	}

	after := w.sessions()
	if after["producer"].ID != before.sessions["producer"] || after["consumer"].ID == before.sessions["consumer"] {
		t.Errorf("sessions: producer %s -> %s, consumer %s -> %s", before.sessions["producer"], after["producer"].ID, before.sessions["consumer"], after["consumer"].ID)
	}

	if w.marker("producer") != producerMarker || w.marker("consumer") == consumerMarker {
		t.Errorf("markers: producer %q (was %q), consumer %q (was %q)", w.marker("producer"), producerMarker, w.marker("consumer"), consumerMarker)
	}

	if len(w.docker.upCalls()) != before.ups {
		t.Error("a build failure recreated a container")
	}

	// Every build failing: nothing at all changes, the exit says BUILD_FAILED.
	w.publishErr["Consumer.csproj"] = api.NewError(api.CodeBuildFailed, "dotnet publish failed", "see the build log")
	before = w.snap()
	producerMarker = w.marker("producer")

	out, errOut := w.run(exitCodeOf(api.CodeBuildFailed), "compose", "launch", "producer", "consumer")
	expectOutput(t, out, "launched 0 of 2 service(s)", "failed", "[BUILD_FAILED]")
	expectOutput(t, errOut, "[BUILD_FAILED]")

	w.unchangedSince(t, before)

	if w.marker("producer") != producerMarker {
		t.Error("the files of a service whose build failed were replaced")
	}

	if strings.Contains(out, "down:") {
		t.Errorf("nothing was stopped, yet:\n%s", out)
	}
}

// exitCodeOf is the exit code a failure with code gives.
func exitCodeOf(code api.Code) int { return exitCode(api.NewError(code, "", "")) }

// TestComposeLaunchLeaseHeld: a service whose session another client holds the
// lease of is excluded, with its session and files intact; the others proceed.
func TestComposeLaunchLeaseHeld(t *testing.T) {
	w := newLaunchWorld(t)

	w.run(0, "--as", "human:bob", "compose", "launch", "--lease-policy", "human-priority", "producer")
	w.enterNamed("consumer")

	before := w.snap()
	producerMarker := w.marker("producer")

	members := w.launchJSON(0, "producer", "consumer")

	if m := member(t, members, "producer"); m.Error == nil || m.Error.Code != api.CodeLeaseHeld {
		t.Errorf("producer = %+v", m)
	}

	if m := member(t, members, "consumer"); m.Session == nil {
		t.Errorf("consumer = %+v", m)
	}

	after := w.sessions()
	if after["producer"].ID != before.sessions["producer"] || after["consumer"].ID == before.sessions["consumer"] {
		t.Errorf("sessions: %v -> %v", before.sessions, after)
	}

	// Mirror never comes before the old app was stopped: its refusal to stop
	// left the files alone.
	if w.marker("producer") != producerMarker {
		t.Errorf("producer's files were replaced while its app still ran: %q -> %q", producerMarker, w.marker("producer"))
	}

	out, _ := w.run(0, "compose", "launch", "producer", "consumer")
	expectOutput(t, out, "[LEASE_HELD]")
}

// TestComposeLaunchNoBuild: --no-build needs something to relaunch.
func TestComposeLaunchNoBuild(t *testing.T) {
	w := newLaunchWorld(t)

	before := w.snap()

	_, errOut := w.run(exitError, "compose", "launch", "--no-build", "producer")
	expectOutput(t, errOut, "[INVALID_REQUEST]", "it isn't in fast mode yet, so there is no build to relaunch")
	w.unchangedSince(t, before)

	// Implicit: skipped, so nothing to launch.
	out, errOut := w.run(exitError, "compose", "launch", "--no-build")
	expectOutput(t, out, "skipped", "no build to relaunch")
	expectOutput(t, errOut, "no service to launch")

	w.enterNamed("producer")

	if err := os.RemoveAll(w.serviceDir("producer")); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(w.serviceDir("producer"), 0o750); err != nil {
		t.Fatal(err)
	}

	before = w.snap()

	_, errOut = w.run(exitError, "compose", "launch", "--no-build", "producer")
	expectOutput(t, errOut, "its build directory is missing or empty")
	w.unchangedSince(t, before)

	if len(w.publishedProjects()) != 1 {
		t.Errorf("--no-build built: %v", w.publishedProjects())
	}
}

// TestComposeLaunchUpFailure: a failed recreate says what compose said, how to
// go back, launches nothing, and prunes nothing.
func TestComposeLaunchUpFailure(t *testing.T) {
	w := newLaunchWorld(t)
	w.docker.upErr = func(upCall) error {
		return api.NewError(api.CodeAttachFailed, "docker compose up failed: bind source missing", "ignored")
	}

	_, errOut := w.run(exitCodeOf(api.CodeAttachFailed), "compose", "launch", "producer", "consumer")
	expectOutput(t, errOut, "[ATTACH_FAILED]", "bind source missing", "eyedbg compose restore consumer producer")

	if len(w.launched()) != 0 || len(w.sessions()) != 0 {
		t.Errorf("launched %v after a failed recreate", w.launchedServices())
	}

	// Nothing was pruned: the builds and the override are where they were.
	for _, p := range []string{w.override(), w.serviceDir("producer"), w.serviceDir("consumer")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("pruned after a failed recreate: %v", err)
		}
	}

	if stages := w.stageDirs(); len(stages) != 0 {
		t.Errorf("staging directories left: %v", stages)
	}

	// The containers are as they were.
	for _, s := range []string{"producer", "consumer"} {
		if c := w.docker.byService(s); c.fi.Fast != nil {
			t.Errorf("%s changed: %+v", s, c.fi)
		}
	}
}

// TestComposeLaunchMirrorFailure: a service whose files couldn't be replaced
// is not launched (its app was stopped, and the output says it is down); a
// build that can't be mirrored fails before the old files change.
func TestComposeLaunchMirrorFailure(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer", "consumer")

	before := w.snap()
	producerMarker := w.marker("producer")

	w.publishHook = nil
	w.publishExtra = func(out string) {
		if err := os.Symlink(os.TempDir(), filepath.Join(out, "link")); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}

	out, errOut := w.run(exitCodeOf(api.CodeBuildFailed), "compose", "launch", "producer")
	expectOutput(t, out, "launched 0 of 1 service(s)", "replace the files of producer", "down: producer")
	expectOutput(t, errOut, "[BUILD_FAILED]")

	if w.marker("producer") != producerMarker {
		t.Errorf("the files changed although the build could not be mirrored: %q", w.marker("producer"))
	}

	if got := w.launchedServices(); len(got) != before.launches || len(w.launched()) != before.launches {
		t.Errorf("a service whose files were not replaced was launched: %v", got)
	}

	if _, ok := w.sessions()["producer"]; ok {
		t.Error("producer's old session is still there: the files may be replaced only after it ended")
	}

	if w.docker.idOf("producer") != before.ids["producer"] {
		t.Error("the container was recreated")
	}
}

// TestComposeLaunchEntryFromAttached: a service attached as built enters fast
// mode: its breakpoints and exception mode come over, its session is ended
// before the container is recreated, and the old session's end is a detach.
func TestComposeLaunchEntryFromAttached(t *testing.T) {
	w := newLaunchWorld(t)

	producerTxt := filepath.Join(w.dir, "producer.txt")

	w.run(0, "compose", "attach", "producer", "--bp", producerTxt+":4", "--exceptions", "all")

	old := w.sessions()["producer"]
	if old.Container == nil || old.Container.Launched {
		t.Fatalf("producer is not attached: %+v", old)
	}

	var atRecreate struct {
		sessions int
		marker   string
	}

	w.docker.onUp = func(upCall) {
		atRecreate.sessions = len(w.sessions())
		atRecreate.marker = w.marker("producer")
	}

	out, _ := w.run(0, "compose", "launch", "producer", "--bp", producerTxt+":6")
	expectOutput(t, out, "launched 1 of 1 service(s)", "recreated -> launched (1 breakpoint carried)")

	// The old session was gone, and the build in place, before the recreate.
	if atRecreate.sessions != 0 || atRecreate.marker == "" {
		t.Errorf("at the recreate: %+v", atRecreate)
	}

	if strings.Contains(out, "down:") {
		t.Errorf("nothing failed:\n%s", out)
	}

	out, _ = w.run(0, "bp", "ls", "-s", w.sessions()["producer"].ID)
	expectOutput(t, out, "producer.txt:4", "producer.txt:6")

	out, _ = w.run(0, "bp", "exceptions", "-s", w.sessions()["producer"].ID)
	expectOutput(t, out, "all")
}

// TestComposeLaunchCarriesPerService: each service's own breakpoints come back
// to it only (a breakpoint a service's session had is not added to the
// others), and one whose file is gone is dropped and counted.
func TestComposeLaunchCarriesPerService(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer", "consumer")

	extra := filepath.Join(w.dir, "extra.txt")
	if err := os.WriteFile(extra, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	sessions := w.sessions()
	w.run(0, "bp", "add", filepath.Join(w.dir, "producer.txt")+":5", "-s", sessions["producer"].ID)
	w.run(0, "bp", "add", extra+":2", "-s", sessions["producer"].ID)
	w.run(0, "bp", "add", filepath.Join(w.dir, "consumer.txt")+":7", "-s", sessions["consumer"].ID)

	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}

	members := w.launchJSON(0, "producer", "consumer")

	if f := member(t, members, "producer").Fast; f == nil || f.Carried != 1 || f.Dropped != 1 {
		t.Errorf("producer: %+v", f)
	}

	if f := member(t, members, "consumer").Fast; f == nil || f.Carried != 1 || f.Dropped != 0 {
		t.Errorf("consumer: %+v", f)
	}

	after := w.sessions()

	out, _ := w.run(0, "bp", "ls", "-s", after["producer"].ID)
	expectOutput(t, out, "producer.txt:5")

	if strings.Contains(out, "consumer.txt") || strings.Contains(out, "extra.txt") {
		t.Errorf("producer got breakpoints that are not its own or are gone:\n%s", out)
	}

	out, _ = w.run(0, "bp", "ls", "-s", after["consumer"].ID)
	expectOutput(t, out, "consumer.txt:7")

	if strings.Contains(out, "producer.txt") {
		t.Errorf("consumer got producer's breakpoint:\n%s", out)
	}
}

// TestComposeLaunchStaysInItsSelection: only the named services are touched
// and the override keeps the fragment of a service that is already in fast
// mode.
func TestComposeLaunchStaysInItsSelection(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("consumer")

	consumerID := w.docker.idOf("consumer")
	consumer2ID, dbID := w.docker.idOf("consumer2"), w.docker.idOf("db")

	w.enterNamed("producer")

	ups := w.docker.upCalls()
	if len(ups) != 2 || !slices.Equal(ups[1].services, []string{"producer"}) {
		t.Fatalf("compose up calls = %+v", ups)
	}

	if w.docker.idOf("consumer") != consumerID || w.docker.idOf("consumer2") != consumer2ID || w.docker.idOf("db") != dbID {
		t.Error("a service that was not named was recreated")
	}

	if c := w.docker.byService("consumer2"); c.fi.Fast != nil {
		t.Error("consumer2 entered fast mode without being named")
	}

	// consumer's session survives a launch of producer.
	if _, ok := w.sessions()["consumer"]; !ok {
		t.Error("consumer's session ended")
	}

	// And its build stays: only what no container refers to is pruned.
	if w.marker("consumer") == "" {
		t.Error("consumer's build directory was pruned")
	}

	// The override holds both, and the second recreate's files exclude it.
	w.checkOverride(t, "consumer", "producer")

	if slices.Contains(ups[1].ref.Files, w.override()) {
		t.Errorf("the override is among the files: %v", ups[1].ref.Files)
	}
}

// TestComposeLaunchOptions: --stop-on-entry and --exceptions reach the launch,
// --dotnet-project picks the project, --env-file reaches the recreate.
func TestComposeLaunchOptions(t *testing.T) {
	w := newLaunchWorld(t)

	envFile := filepath.Join(w.dir, "prod.env")

	out, _ := w.run(0, "compose", "launch", "producer", "--stop-on-entry", "--exceptions", "all", "--env-file", envFile,
		"--dotnet-project", "producer=Producer/Producer.csproj", "-f", filepath.Join(w.dir, "compose.yml"))
	expectOutput(t, out, "launched 1 of 1")

	if l := w.launched(); len(l) != 1 || !l[0].spec.StopOnEntry {
		t.Errorf("launches = %+v", l)
	}

	if ups := w.docker.upCalls(); len(ups) != 1 || !slices.Equal(ups[0].ref.EnvFiles, []string{envFile}) {
		t.Errorf("compose up = %+v", ups)
	}

	if p := w.publishes[0].spec.Project; p != filepath.Join(w.dir, "Producer", "Producer.csproj") {
		t.Errorf("published %s", p)
	}

	out, _ = w.run(0, "status", "-s", w.sessions()["producer"].ID)
	expectOutput(t, out, "stopped", "entry")

	out, _ = w.run(0, "bp", "exceptions", "-s", w.sessions()["producer"].ID)
	expectOutput(t, out, "all")
}

// TestComposeLaunchRejectsBeforeChanging: a command line that can't work is
// refused before docker, the build or any session is touched.
func TestComposeLaunchRejectsBeforeChanging(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer")

	missing := filepath.Join(w.dir, "nosuch.txt")
	outside := filepath.Join(t.TempDir(), "x.txt")

	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"bad service name", []string{"Bad Name"}, "invalid service"},
		{"unknown flag --map", []string{"--map", "/a=/b"}, "unknown flag"},
		{"bad dotnet-project", []string{"--dotnet-project", "producer"}, "want SERVICE=PATH"},
		{"dotnet-project of an unselected service", []string{"producer", "--dotnet-project", "consumer=Consumer/Consumer.csproj"}, "doesn't launch"},
		{"dotnet-project outside the compose directory", []string{"producer", "--dotnet-project", "producer=" + outside}, "isn't a project file"},
		{"dotnet-project that isn't there", []string{"producer", "--dotnet-project", "producer=Nope/Nope.csproj"}, "doesn't exist"},
		{"breakpoint outside the map", []string{"producer", "--bp", outside + ":1"}, "outside the container's path map"},
		{"breakpoint in a file that isn't there", []string{"producer", "--bp", missing + ":1"}, "no source file"},
		{"bad lease policy", []string{"producer", "--lease-policy", "whenever"}, "invalid lease policy"},
		{"bad exception mode", []string{"producer", "--exceptions", "most"}, "unknown exception mode"},
		{"env file with a control character", []string{"producer", "--env-file", "a\nb.env"}, "env-file"},
		{"a service that isn't running", []string{"nosuch"}, "is not running"},
	}

	for _, tt := range tests {
		before := w.snap()

		_, errOut := w.run(exitError, append([]string{"compose", "launch"}, tt.args...)...)
		expectOutput(t, errOut, tt.want)
		w.unchangedSince(t, before)
	}

	if n := len(w.publishedProjects()); n != 1 {
		t.Errorf("a refused command line built something: %v", w.publishedProjects())
	}
}

// TestComposeLaunchLocked: one run per project at a time.
func TestComposeLaunchLocked(t *testing.T) {
	w := newLaunchWorld(t)

	root, err := artifacts.Dir(artifacts.Compose)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := artifacts.ProjectDir(root, worldProject)
	if err != nil {
		t.Fatal(err)
	}

	unlock, err := artifacts.Lock(dir)
	if err != nil {
		t.Fatal(err)
	}

	before := w.snap()

	_, errOut := w.run(exitError, "compose", "launch", "producer")
	expectOutput(t, errOut, "[INVALID_REQUEST]", "another eyedbg compose run is working on project myapp")

	_, errOut = w.run(exitError, "compose", "restore", "producer")
	expectOutput(t, errOut, "another eyedbg compose run is working on project myapp")

	w.unchangedSince(t, before)

	// The lock is the other run's: a refused run doesn't remove it.
	if _, err := os.Stat(filepath.Join(dir, "lock")); err != nil {
		t.Errorf("the other run's lock is gone: %v", err)
	}

	unlock()

	w.run(0, "compose", "launch", "producer")
}

// TestComposeLaunchChecks: what a container must be to enter fast mode, and
// that a refusal changes nothing; a service named fails, one picked without a
// name is skipped, and an adapter that isn't installed fails either way.
func TestComposeLaunchChecks(t *testing.T) {
	tests := []struct {
		name string
		edit func(w *launchWorld)
		code api.Code
		want string
		// failsImplicit: the same refusal is a failure, not a skip, without names.
		failsImplicit bool
	}{
		{"a command", func(w *launchWorld) { w.docker.byService("producer").fi.HasCmd = true }, api.CodeInvalidRequest, "passes arguments to its app", false},
		{"another entrypoint (an IDE's worker)", func(w *launchWorld) { w.docker.byService("producer").fi.DLL = "" }, api.CodeInvalidRequest, "isn't exec-form", false},
		{"a working directory fast mode can't use", func(w *launchWorld) {
			w.docker.byService("producer").fi.WorkDirErr = errors.New("the container's working directory /usr/app can't hold the build")
		}, api.CodeInvalidRequest, "working directory /usr/app", false},
		{"no compose files recorded", func(w *launchWorld) { w.docker.byService("producer").fi.ConfigFiles = nil }, api.CodeInvalidRequest, "compose labels don't check out", false},
		{"fast mode of another version", func(w *launchWorld) {
			w.docker.byService("producer").fi.Fast = &container.FastLabels{Version: "2", Override: "/x/override.yml"}
		}, api.CodeInvalidRequest, "another eyedbg", false},
		{"no tail in the image", func(w *launchWorld) {
			w.docker.probeErr["producer"] = api.NewError(api.CodeInvalidRequest, "the image has no 'tail'", "")
		}, api.CodeInvalidRequest, "no 'tail'", false},
		{"docker failing on the probe", func(w *launchWorld) {
			w.docker.probeErr["producer"] = api.NewError(api.CodeAttachFailed, "docker exec failed", "")
		}, api.CodeAttachFailed, "docker exec failed", true},
		{"netcoredbg not installed", func(w *launchWorld) {
			w.docker.adapterErr["producer"] = api.NewError(api.CodeAdapterMissing, "netcoredbg for linux/arm64 is not installed", "run 'eyedbg adapters install'")
		}, api.CodeAdapterMissing, "not installed", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newLaunchWorld(t)
			tt.edit(w)

			before := w.snap()

			_, errOut := w.run(exitCodeOf(tt.code), "compose", "launch", "producer")
			expectOutput(t, errOut, "["+string(tt.code)+"]", tt.want)
			w.unchangedSince(t, before)

			if n := len(w.publishedProjects()); n != 0 {
				t.Errorf("a refused service was built: %v", w.publishedProjects())
			}

			// Without names the others go on; the refusal is a skip or a failure.
			out, _ := w.run(0, "compose", "launch")
			expectOutput(t, out, "launched 2 of 4 service(s)", "producer")

			verb := "skipped"
			if tt.failsImplicit {
				verb = "failed"
			}

			if row := rowOf(out, "producer"); len(row) < 3 || row[1] != "-" || row[2] != verb {
				t.Errorf("producer's row = %q, want %s:\n%s", row, verb, out)
			}
		})
	}
}

// TestComposeLaunchTwoContainers: fast mode runs one container per service:
// 'compose up SERVICE' would recreate every replica, so none is launched.
func TestComposeLaunchTwoContainers(t *testing.T) {
	w := newLaunchWorld(t)

	twin := w.docker.add("producer", "Producer.dll")
	twin.fi.Name = worldProject + "-producer-2"

	out, _ := w.run(0, "compose", "launch")
	expectOutput(t, out, "launched 2 of 5 service(s)", "more than one container")

	if got := w.launchedServices(); len(got) != 2 || slices.Contains(got, "producer") {
		t.Errorf("launched %v", got)
	}

	for _, u := range w.docker.upCalls() {
		if slices.Contains(u.services, "producer") {
			t.Errorf("compose up names the scaled service: %v", u.services)
		}
	}
}

// TestComposeLaunchSharedComposeDir: the services of a run share one compose
// directory (the build maps /src to it): one that disagrees is refused.
func TestComposeLaunchSharedComposeDir(t *testing.T) {
	w := newLaunchWorld(t)

	// Another directory that really holds the files its container names: only
	// the sharing is wrong.
	other, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(other, "compose.yml")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	producer := w.docker.byService("producer")
	producer.fi.ComposeDir, producer.fi.ConfigFiles = other, []string{file}

	out, _ := w.run(0, "compose", "launch", "consumer", "producer")
	expectOutput(t, out, "launched 1 of 2 service(s)", "its compose project directory is")

	if got := w.launchedServices(); !slices.Equal(got, []string{"consumer"}) {
		t.Errorf("launched %v", got)
	}
}

// TestComposeLaunchProjectSearch: the project is found by assembly name; two
// matches or none name --dotnet-project.
func TestComposeLaunchProjectSearch(t *testing.T) {
	w := newLaunchWorld(t)

	if err := os.MkdirAll(filepath.Join(w.dir, "Other"), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(w.dir, "Other", "Producer.csproj"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	before := w.snap()

	_, errOut := w.run(exitError, "compose", "launch", "producer")
	expectOutput(t, errOut, "[INVALID_REQUEST]", "2 projects build Producer.dll", "--dotnet-project SERVICE=PATH")
	w.unchangedSince(t, before)

	out, _ := w.run(0, "compose", "launch", "producer", "--dotnet-project", "producer=Other/Producer.csproj")
	expectOutput(t, out, "launched 1 of 1")
}

// rowOf is the words of service's line in a launch table ("" row when none).
func rowOf(out, service string) []string {
	for line := range strings.Lines(out) {
		if f := strings.Fields(line); len(f) > 0 && f[0] == service {
			return f
		}
	}

	return nil
}

// TestComposeLaunchFirstBuildFails: a compile error on the first launch leaves
// the log to read (the project directory stays although no service is in fast
// mode), and nothing else; restore tidies it.
func TestComposeLaunchFirstBuildFails(t *testing.T) {
	w := newLaunchWorld(t)
	w.publishErr["Producer.csproj"] = api.NewError(api.CodeBuildFailed, "dotnet publish failed", "see the build log")

	before := w.snap()

	_, errOut := w.run(exitCodeOf(api.CodeBuildFailed), "compose", "launch", "producer")
	expectOutput(t, errOut, "[BUILD_FAILED]")
	w.unchangedSince(t, before)

	log, err := os.ReadFile(filepath.Join(w.home, "compose", worldProject, "build.log"))
	if err != nil || !strings.Contains(string(log), "CS1002") {
		t.Errorf("the build log is gone or empty: %q, %v", log, err)
	}

	if _, err := os.Stat(w.serviceDir("producer")); !os.IsNotExist(err) {
		t.Errorf("a service directory exists for a service that never built: %v", err)
	}

	w.run(exitError, "compose", "restore")

	if w.projectDirExists() {
		t.Error("restore left eyedbg's project directory although nothing is in fast mode")
	}
}

// TestComposeLaunchBuildLogNotRegular: eyedbg writes only files of its own: a
// build.log that is a symlink is not written through.
func TestComposeLaunchBuildLogNotRegular(t *testing.T) {
	w := newLaunchWorld(t)

	root, err := artifacts.Dir(artifacts.Compose)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := artifacts.ProjectDir(root, worldProject)
	if err != nil {
		t.Fatal(err)
	}

	victim := filepath.Join(t.TempDir(), "precious.txt")
	if err := os.WriteFile(victim, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(victim, filepath.Join(dir, "build.log")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	before := w.snap()

	_, errOut := w.run(exitError, "compose", "launch", "producer")
	expectOutput(t, errOut, "[INVALID_REQUEST]", "is not a regular file")
	w.unchangedSince(t, before)

	if got, err := os.ReadFile(victim); err != nil || string(got) != "keep me" {
		t.Errorf("the file the link pointed to was written: %q, %v", got, err)
	}
}

// TestComposeLaunchPruneNeedsTheList: when docker's list of fast-mode
// containers can't be read, nothing of eyedbg's is deleted.
func TestComposeLaunchPruneNeedsTheList(t *testing.T) {
	w := newLaunchWorld(t)
	w.enterNamed("producer")

	w.docker.listErr = api.NewError(api.CodeAttachFailed, "docker ps failed", "")

	// The recreate-free relaunch reads nothing; the prune can't list: the
	// directories stay.
	w.run(0, "compose", "launch", "producer")

	for _, p := range []string{w.override(), w.serviceDir("producer")} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("deleted although the list could not be read: %v", err)
		}
	}

	// Restore can't list either: it fails and removes nothing.
	_, errOut := w.run(exitCodeOf(api.CodeAttachFailed), "compose", "restore")
	expectOutput(t, errOut, "docker ps failed")

	if _, err := os.Stat(w.serviceDir("producer")); err != nil {
		t.Errorf("deleted although the list could not be read: %v", err)
	}
}

// TestComposeRestoreWithoutDaemon: no daemon means no sessions to end, not a
// failure; and what eyedbg keeps for the project is removed.
func TestComposeRestoreWithoutDaemon(t *testing.T) {
	w := newWorld(t, false)

	root, err := artifacts.Dir(artifacts.Compose)
	if err != nil {
		t.Fatal(err)
	}

	dir, err := artifacts.ProjectDir(root, worldProject)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "services", "producer"), 0o750); err != nil {
		t.Fatal(err)
	}

	w.docker.enterFast("producer", w.override(), "Producer/Producer.csproj")

	out, _ := w.run(0, "compose", "restore")
	expectOutput(t, out, "restored 1 of 1 service(s)", "producer")

	if strings.Contains(out, "ended") {
		t.Errorf("no daemon, yet sessions were ended:\n%s", out)
	}

	if w.projectDirExists() {
		t.Error("eyedbg's project directory is still there")
	}
}

// TestComposeLaunchReentersOutdatedFast: a container in fast mode with another
// override (a moved home, an older file) is recreated like an as-built one,
// from its own files without the old override.
func TestComposeLaunchReentersOutdatedFast(t *testing.T) {
	w := newLaunchWorld(t)

	old := filepath.Join(w.dir, "old-override.yml")
	w.docker.enterFast("producer", old, "Producer/Producer.csproj")

	out, _ := w.run(0, "compose", "launch", "producer")
	expectOutput(t, out, "recreated -> launched")

	ups := w.docker.upCalls()
	if len(ups) != 1 || slices.Contains(ups[0].ref.Files, old) || len(ups[0].ref.Files) != 2 || ups[0].override != w.override() {
		t.Errorf("compose up = %+v", ups)
	}
}

// TestComposeLaunchGroupsByFiles: services created from different compose
// files are recreated by separate 'compose up' calls, each with its own files.
func TestComposeLaunchGroupsByFiles(t *testing.T) {
	w := newLaunchWorld(t)

	only := []string{filepath.Join(w.dir, "compose.yml")}

	for _, c := range []*fakeCtr{w.docker.byService("producer")} {
		c.fi.ConfigFiles, c.asBuilt.ConfigFiles = only, only
	}

	w.run(0, "compose", "launch", "producer", "consumer")

	ups := w.docker.upCalls()
	if len(ups) != 2 {
		t.Fatalf("%d compose up calls, want 2: %+v", len(ups), ups)
	}

	byService := map[string][]string{}
	for i := range ups {
		for _, s := range ups[i].services {
			byService[s] = ups[i].ref.Files
		}
	}

	if !slices.Equal(byService["producer"], only) || len(byService["consumer"]) != 2 {
		t.Errorf("files by service: %v", byService)
	}
}

// TestComposeLaunchLaunchFails: a launch the daemon refuses leaves the
// service down, and the output says so and how to try again.
func TestComposeLaunchLaunchFails(t *testing.T) {
	w := newLaunchWorld(t)
	w.launchErr["producer"] = api.NewError(api.CodeAttachFailed, "container myapp-producer-1 already runs a .NET process", "docker restart myapp-producer-1")

	out, _ := w.run(0, "compose", "launch", "producer", "consumer")
	expectOutput(t, out, "launched 1 of 2 service(s)", "already runs a .NET process", "down: producer", "'eyedbg compose launch producer' tries again")
	expectOutput(t, out, "undo with: eyedbg compose restore consumer")

	if strings.Contains(out, "restore consumer producer") {
		t.Errorf("the failed service is offered for restore as if it ran:\n%s", out)
	}
}

// TestComposeLaunchRecreatedNotRunning: a container that doesn't come up
// running in fast mode is a failure, not a launch.
func TestComposeLaunchRecreatedNotRunning(t *testing.T) {
	w := newLaunchWorld(t)

	w.docker.afterUp = func() {
		for _, c := range w.docker.ctrs {
			if c.fi.Service == "producer" && c.fi.Fast != nil {
				c.fi.Running = false
			}
		}
	}

	out, errOut := w.run(exitCodeOf(api.CodeAttachFailed), "compose", "launch", "producer")
	expectOutput(t, out, "launched 0 of 1", "isn't running in fast mode after the recreate", "down: producer")
	expectOutput(t, errOut, "[ATTACH_FAILED]")

	if len(w.launched()) != 0 {
		t.Errorf("launched %v", w.launchedServices())
	}
}

// TestCarriedSpec: what of a session's breakpoints comes over to the next one.
func TestCarriedSpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   api.Breakpoint
		want api.BreakpointSpec
		ok   bool
	}{
		{"line: the requested line, not where the adapter put it", api.Breakpoint{File: "/a/b.cs", RequestedLine: 10, Line: 12}, api.BreakpointSpec{File: "/a/b.cs", Line: 10}, true},
		{"no requested line: the line", api.Breakpoint{File: "/a/b.cs", Line: 12}, api.BreakpointSpec{File: "/a/b.cs", Line: 12}, true},
		{"condition, hit condition, log message, anchor", api.Breakpoint{
			File: "/a/b.cs", RequestedLine: 3, Condition: "x > 1", HitCondition: ">=2", LogMessage: "x={x}", Anchor: "var x",
		}, api.BreakpointSpec{File: "/a/b.cs", Line: 3, Condition: "x > 1", HitCondition: ">=2", LogMessage: "x={x}", Anchor: "var x"}, true},
		{"function", api.Breakpoint{Function: "App.Run", Condition: "n > 0"}, api.BreakpointSpec{Function: "App.Run", Condition: "n > 0"}, true},
		{"temporary (run-until)", api.Breakpoint{File: "/a/b.cs", RequestedLine: 3, Temporary: true}, api.BreakpointSpec{}, false},
		{"the editor's", api.Breakpoint{File: "/a/b.cs", RequestedLine: 3, Editor: true}, api.BreakpointSpec{}, false},
		{"no line at all", api.Breakpoint{File: "/a/b.cs"}, api.BreakpointSpec{}, false},
		{"no file", api.Breakpoint{RequestedLine: 3}, api.BreakpointSpec{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := carriedSpec(&tt.in)
			if ok != tt.ok || got != tt.want {
				t.Errorf("carriedSpec = %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestCarryModes: the exception mode that stops at the most is carried.
func TestCarryModes(t *testing.T) {
	t.Parallel()

	var c carry

	for _, m := range []api.ExceptionMode{api.ExceptionsNone, api.ExceptionsUncaught, api.ExceptionsAll, api.ExceptionsUncaught, ""} {
		c.addMode(m)
	}

	if c.mode != api.ExceptionsAll {
		t.Errorf("mode = %q, want all", c.mode)
	}

	var d carry

	d.addMode(api.ExceptionsNone)
	d.addMode(api.ExceptionsUncaught)

	if d.mode != api.ExceptionsUncaught {
		t.Errorf("mode = %q, want uncaught", d.mode)
	}
}

// TestComposeLaunchHomeWithComma: a home path with a comma can't be recorded in
// compose's comma-separated file list: refused before anything is created.
func TestComposeLaunchHomeWithComma(t *testing.T) {
	w := newLaunchWorld(t)

	home := filepath.Join(t.TempDir(), "a,b")
	t.Setenv(adapters.EnvHome, home)

	before := w.snap()

	_, errOut := w.run(exitError, "compose", "launch", "producer")
	expectOutput(t, errOut, "[INVALID_REQUEST]", "comma in its path")
	w.unchangedSince(t, before)

	if _, err := os.Stat(filepath.Join(home, "compose", worldProject)); !os.IsNotExist(err) {
		t.Errorf("a project directory was created: %v", err)
	}
}

// TestComposeLaunchRefusesUncorroboratedLabels: an image's own LABELs are
// copied onto a container started by plain 'docker run', so the compose labels
// of a container are believed only when its compose files are in the project
// directory they name. A hostile label naming another directory (the user's
// home, say) must not be built in, recreated from or mapped: the service is
// refused, and nothing changes.
func TestComposeLaunchRefusesUncorroboratedLabels(t *testing.T) {
	tests := []struct {
		name string
		edit func(w *launchWorld, c *fakeCtr)
	}{
		{"a directory with no compose file in it", func(w *launchWorld, c *fakeCtr) {
			c.fi.ComposeDir = realDirWith(w.t, "id_rsa")
		}},
		{"a file list naming a file that isn't compose's", func(w *launchWorld, c *fakeCtr) {
			c.fi.ComposeDir = realDirWith(w.t, ".bashrc")
			c.fi.ConfigFiles = []string{filepath.Join(c.fi.ComposeDir, ".bashrc")}
		}},
		{"a file list naming a compose file elsewhere", func(w *launchWorld, c *fakeCtr) {
			c.fi.ComposeDir = realDirWith(w.t, "x")
			c.fi.ConfigFiles = []string{filepath.Join(w.dir, "compose.yml")}
		}},
		{"a directory that is not on this machine", func(w *launchWorld, c *fakeCtr) {
			c.fi.ComposeDir = filepath.Join(realDirWith(w.t, "x"), "gone")
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newLaunchWorld(t)
			tt.edit(w, w.docker.byService("producer"))

			before := w.snap()

			_, errOut := w.run(exitCodeOf(api.CodeInvalidRequest), "compose", "launch", "producer")
			expectOutput(t, errOut, "[INVALID_REQUEST]", "compose labels don't check out")
			w.unchangedSince(t, before)

			if n := len(w.publishedProjects()); n != 0 {
				t.Errorf("a refused service was built: %v", w.publishedProjects())
			}

			if ups := w.docker.upCalls(); len(ups) != 0 {
				t.Errorf("a refused service was recreated: %+v", ups)
			}
		})
	}
}

// realDirWith is a new directory (symlinks resolved) holding empty files.
func realDirWith(t *testing.T, names ...string) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// TestComposeLaunchProjectDirectoryFlags: when --project-directory (or the
// first -f file's directory) says where the project is, the container's label
// must name that directory; the labels agreeing with the flags launch.
func TestComposeLaunchProjectDirectoryFlags(t *testing.T) {
	tests := []struct {
		name  string
		flags func(w *launchWorld) []string
		ok    bool
	}{
		{"--project-directory agrees", func(w *launchWorld) []string { return []string{"--project-directory", w.dir} }, true},
		{"-f agrees: its directory is the project's", func(w *launchWorld) []string { return []string{"-f", filepath.Join(w.dir, "compose.yml")} }, true},
		{"no flags", func(*launchWorld) []string { return nil }, true},
		{"--project-directory names another directory", func(*launchWorld) []string { return []string{"--project-directory", realDirWith(t)} }, false},
		{"-f in another directory", func(*launchWorld) []string { return []string{"-f", filepath.Join(realDirWith(t), "compose.yml")} }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newLaunchWorld(t)
			args := append([]string{"compose", "launch", "producer"}, tt.flags(w)...)

			if tt.ok {
				out, _ := w.run(0, args...)
				expectOutput(t, out, "launched 1 of 1 service(s)")

				return
			}

			before := w.snap()

			_, errOut := w.run(exitCodeOf(api.CodeInvalidRequest), args...)
			expectOutput(t, errOut, "[INVALID_REQUEST]", "the command line names")
			w.unchangedSince(t, before)

			if n := len(w.publishedProjects()); n != 0 {
				t.Errorf("a refused service was built: %v", w.publishedProjects())
			}
		})
	}
}
