// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
)

// The docker e2e tests (docs/adr/0020 and 0021, Confirmation): the real eyedbg
// and eyedbgd against the compose fixture testdata/apps/dotnet/compose, built
// and run by a real docker engine. TestComposeAttach debugs the services as
// built; TestComposeLaunch (compose_launch_test.go) runs fast mode. Each test
// has its own throwaway compose project (eyedbg-e2e-<8 hex>), data dir, home
// and daemon; only that project is ever removed.

// envE2EDocker opts in to the docker e2e tests, on top of EYEDBG_E2E=1: unset,
// they skip naming it; set, a missing docker CLI, compose plugin or engine
// fails them, never skips. They need network access (base images, NuGet,
// netcoredbg's release download) and start only the .NET services, without
// Kafka (docs/adr/0020, D14).
const envE2EDocker = "EYEDBG_E2E_DOCKER"

const (
	// dockerBuildLimit bounds 'up --build': a cold run pulls the SDK image and
	// restores NuGet packages.
	dockerBuildLimit = 25 * time.Minute
	// dockerLimit bounds every other docker call.
	dockerLimit = 3 * time.Minute
	// composeLimit bounds an eyedbg compose call that builds or recreates:
	// the Debug publish, the recreate and restore's --wait (180 s).
	composeLimit = 10 * time.Minute
	// stopTimeout is the --timeout of the waits for a stop.
	stopTimeout = "60s"
)

// buildEnv keeps the host's build servers from outliving the test: eyedbg
// compose launch runs 'dotnet publish' with this environment.
var buildEnv = map[string]string{
	"UseSharedCompilation":          "false",
	"MSBUILDDISABLENODEREUSE":       "1",
	"DOTNET_CLI_USE_MSBUILD_SERVER": "0",
	"DOTNET_CLI_TELEMETRY_OPTOUT":   "1",
	"DOTNET_NOLOGO":                 "1",
}

// debugOverride makes the fixture's images Debug builds (the Visual Studio
// Dockerfile template's BUILD_CONFIGURATION argument), so line breakpoints bind
// when eyedbg attaches to them.
const debugOverride = `services:
  producer:
    build:
      args:
        BUILD_CONFIGURATION: Debug
  consumer:
    build:
      args:
        BUILD_CONFIGURATION: Debug
`

// composeStack is one fixture copy, built and running as its own compose
// project, with its own eyedbg harness.
type composeStack struct {
	t       *testing.T
	h       *harness
	project string
	// dir is the fixture copy, with symlinks resolved: Docker Desktop paths go
	// through /private/var on macOS.
	dir string
	// files are the compose files, in -f order.
	files []string
}

// requireDocker skips t unless EYEDBG_E2E=1 and EYEDBG_E2E_DOCKER=1, then
// requires docker, its compose plugin and a Linux engine (failing, never
// skipping), and returns the engine's platform, e.g. linux/arm64.
func requireDocker(t *testing.T) string {
	t.Helper()

	if os.Getenv(envE2E) != "1" {
		t.Skip("set " + envE2E + "=1 and " + envE2EDocker + "=1 (needs docker with the compose plugin and network access)")
	}

	if os.Getenv(envE2EDocker) != "1" {
		t.Skip("set " + envE2EDocker + "=1 (needs docker with the compose plugin and a Linux engine, and network access for the base images and NuGet)")
	}

	requireLang(t, dotnet.Language)

	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("%s=1 needs the docker CLI on PATH: %v", envE2EDocker, err)
	}

	if out, err := dockerRun(dockerLimit, "compose", "version", "--short"); err != nil {
		t.Fatalf("%s=1 needs the docker compose plugin: %v (%s)", envE2EDocker, err, out)
	}

	out, err := dockerRun(dockerLimit, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		t.Fatalf("%s=1 needs a running docker engine: %v", envE2EDocker, err)
	}

	platform := strings.TrimSpace(out)
	if !strings.HasPrefix(platform, "linux/") {
		t.Fatalf("%s=1 needs a Linux container engine, docker reports %q", envE2EDocker, platform)
	}

	return platform
}

// dockerRun runs docker with args under limit and returns its stdout; an
// error carries stderr.
func dockerRun(limit time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...)

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// newComposeStack prepares the stack: a fresh harness, a copy of the fixture
// (plus override, when not empty, as a second compose file), netcoredbg for
// the engine's platform in a data directory of its own, then builds the two
// .NET services and waits for them. Its project is removed when t ends.
func newComposeStack(t *testing.T, override string) *composeStack {
	t.Helper()

	platform := requireDocker(t)
	h := newHarness(t)

	// A data directory of its own, so the linux netcoredbg installed below
	// never lands in the developer's.
	h.dataDir = t.TempDir()

	home, err := filepath.EvalSymlinks(h.homeDir)
	if err != nil {
		t.Fatal(err)
	}

	h.homeDir = home

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	copyFixture(t, dir)

	s := &composeStack{t: t, h: h, project: "eyedbg-e2e-" + randomHex(t), dir: dir, files: []string{filepath.Join(dir, "compose.yml")}}

	if override != "" {
		file := filepath.Join(dir, "override.yml")
		if err := os.WriteFile(file, []byte(override), 0o600); err != nil {
			t.Fatal(err)
		}

		s.files = append(s.files, file)
	}

	h.runLimited(dockerLimit, nil, "adapters", "install", "netcoredbg", "--platform", platform).wantCode(exitOK)

	t.Cleanup(s.down)

	if out, err := dockerRun(dockerBuildLimit, s.composeArgs("up", "-d", "--build", "--wait", "--no-deps", "producer", "consumer")...); err != nil {
		t.Fatalf("up: %v\n%s", err, out)
	}

	return s
}

// copyFixture copies testdata/apps/dotnet/compose into dir, leaving out any
// build of the source tree.
func copyFixture(t *testing.T, dir string) {
	t.Helper()

	if err := os.CopyFS(dir, os.DirFS(filepath.Join("..", "..", "testdata", "apps", "dotnet", "compose"))); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"Producer", "Consumer"} {
		for _, sub := range []string{"bin", "obj"} {
			if err := os.RemoveAll(filepath.Join(dir, d, sub)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// randomHex returns 8 hex digits.
func randomHex(t *testing.T) string {
	t.Helper()

	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}

	return hex.EncodeToString(b)
}

// composeArgs is the 'docker compose' argv for this project and verb.
func (s *composeStack) composeArgs(verb ...string) []string {
	args := []string{"compose", "-p", s.project}
	for _, f := range s.files {
		args = append(args, "-f", f)
	}

	return append(args, verb...)
}

// down removes this project and its locally built images and volumes, and
// nothing else.
func (s *composeStack) down() {
	if out, err := dockerRun(dockerLimit, s.composeArgs("down", "--rmi", "local", "--volumes", "--remove-orphans")...); err != nil {
		s.t.Errorf("removing project %s: %v\n%s", s.project, err, out)
	}
}

// docker runs docker, failing the test on error, and returns stdout.
func (s *composeStack) docker(args ...string) string {
	s.t.Helper()

	out, err := dockerRun(dockerLimit, args...)
	if err != nil {
		s.t.Fatal(err)
	}

	return out
}

// containerID returns the full id of the project's one container of service.
func (s *composeStack) containerID(service string) string {
	s.t.Helper()

	out := s.docker("ps", "-a", "--no-trunc", "--filter", "label=com.docker.compose.project="+s.project,
		"--filter", "label=com.docker.compose.service="+service, "--format", "{{.ID}}")

	ids := strings.Fields(out)
	if len(ids) != 1 {
		s.t.Fatalf("service %s has %d containers (%q), want 1", service, len(ids), ids)
	}

	return ids[0]
}

// inspect returns docker inspect's --format result for the container.
func (s *composeStack) inspect(id, format string) string {
	s.t.Helper()

	return strings.TrimSpace(s.docker("inspect", "--type", "container", "--format", format, id))
}

// requireRunning fails unless every container is running.
func (s *composeStack) requireRunning(ids ...string) {
	s.t.Helper()

	for _, id := range ids {
		if got := s.inspect(id, "{{.State.Running}}"); got != "true" {
			s.t.Fatalf("container %s running = %q, want true", id[:12], got)
		}
	}
}

// logs returns what the container wrote to docker's log (stdout and stderr).
func (s *composeStack) logs(id string) string {
	s.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), dockerLimit)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "logs", id).CombinedOutput()
	if err != nil {
		s.t.Fatalf("docker logs %s: %v\n%s", id[:12], err, out)
	}

	return string(out)
}

// dotnetRunning reports whether the container has a process named dotnet.
func (s *composeStack) dotnetRunning(id string) bool {
	s.t.Helper()

	for line := range strings.SplitSeq(s.docker("top", id, "-o", "pid,stat,comm"), "\n") {
		if fields := strings.Fields(line); len(fields) >= 3 && fields[len(fields)-1] == "dotnet" {
			return true
		}
	}

	return false
}

// requireNoDotnet polls, for a bounded time, until the container has no
// dotnet process: the app is gone a moment after its session ends.
func (s *composeStack) requireNoDotnet(id string) {
	s.t.Helper()

	deadline := time.After(15 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)

	defer tick.Stop()

	for s.dotnetRunning(id) {
		select {
		case <-deadline:
			s.t.Fatalf("container %s still runs dotnet after its session ended: %s", id[:12], s.docker("top", id, "-o", "pid,stat,comm"))
		case <-tick.C:
		}
	}
}

// discovery returns the -f and -p flags that name this project for eyedbg's
// compose attach, launch and restore.
func (s *composeStack) discovery() []string {
	var args []string
	for _, f := range s.files {
		args = append(args, "-f", f)
	}

	return append(args, "-p", s.project)
}

// eyedbgCompose runs 'eyedbg --json compose ARGS' (bounded by composeLimit,
// with buildEnv) and decodes its JSON into v.
func (s *composeStack) eyedbgCompose(v any, args ...string) {
	s.t.Helper()

	s.h.runLimited(composeLimit, buildEnv, append([]string{"--json", "compose"}, args...)...).wantCode(exitOK).decode(v)
}

// memberResult is one member of 'compose attach|launch --json'.
type memberResult struct {
	Service     string           `json:"service"`
	Container   string           `json:"container"`
	Session     *api.SessionInfo `json:"session"`
	Error       *api.Error       `json:"error"`
	Skipped     string           `json:"skipped"`
	Breakpoints []struct {
		Location   string         `json:"location"`
		Breakpoint api.Breakpoint `json:"breakpoint"`
	} `json:"breakpoints"`
	Fast *struct {
		Project    string `json:"project"`
		BuildLog   string `json:"buildLog"`
		Override   string `json:"override"`
		Recreated  bool   `json:"recreated"`
		DurationMs int    `json:"durationMs"`
		Carried    int    `json:"carried"`
	} `json:"fast"`
}

// groupResult is 'compose attach|launch --json'.
type groupResult struct {
	Schema  int            `json:"schema"`
	Group   string         `json:"group"`
	Members []memberResult `json:"members"`
}

// member returns the result for service, failing the test when it is missing
// or didn't start a session.
func (g groupResult) member(t *testing.T, service string) memberResult {
	t.Helper()

	for _, m := range g.Members {
		if m.Service != service {
			continue
		}

		if m.Session == nil || m.Error != nil {
			t.Fatalf("%s: session %v, error %+v, skipped %q", service, m.Session, m.Error, m.Skipped)
		}

		return m
	}

	t.Fatalf("no member %s among %d", service, len(g.Members))

	return memberResult{}
}

// waitEnvelope is 'compose wait --json'.
type waitEnvelope struct {
	Schema              int `json:"schema"`
	api.GroupWaitResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
}

// groupEventsEnvelope is 'compose events --json'.
type groupEventsEnvelope struct {
	Schema                int `json:"schema"`
	api.GroupEventsResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
}

// waitStop waits for the group's next stop and requires it to be service's,
// at file:line, with that line in the source excerpt.
func (s *composeStack) waitStop(service, file string, line int, excerpt string) api.Snapshot {
	s.t.Helper()

	var res waitEnvelope

	s.eyedbgCompose(&res, "wait", "-g", s.project, "--timeout", stopTimeout)

	if res.Session == nil || res.Service != service {
		s.t.Fatalf("compose wait: session %+v service %q, want %s stopped (timedOut %v)", res.Session, res.Service, service, res.TimedOut)
	}

	snap := *res.Session

	if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.File != file || snap.Frame.Line != line {
		s.t.Fatalf("compose wait: state %s frame %+v, want stopped at %s:%d", snap.Session.State, snap.Frame, file, line)
	}

	for _, l := range snap.Source {
		if l.Current && strings.Contains(l.Text, excerpt) {
			return snap
		}
	}

	s.t.Fatalf("compose wait: source excerpt %+v has no current line containing %q", snap.Source, excerpt)

	return snap
}

// sessions returns the daemon's sessions.
func (s *composeStack) sessions() []api.SessionInfo {
	s.t.Helper()

	var res struct {
		Sessions []api.SessionInfo `json:"sessions"`
	}

	s.h.run("--json", "sessions").wantCode(exitOK).decode(&res)

	return res.Sessions
}

// requireNoBuildInCopy fails if the fixture copy holds a bin or obj directory:
// eyedbg builds under its own home.
func (s *composeStack) requireNoBuildInCopy() {
	s.t.Helper()

	err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && (d.Name() == "bin" || d.Name() == "obj") {
			return fmt.Errorf("the build left %s in the project", path)
		}

		return nil
	})
	if err != nil {
		s.t.Fatal(err)
	}
}

// TestComposeAttach is ADR 0020's docker e2e: eyedbg attaches to the two
// services of a compose stack as built (Debug images, so line breakpoints
// bind), stops each at a breakpoint in a host file with a source excerpt, and
// lets go without touching them.
func TestComposeAttach(t *testing.T) {
	s := newComposeStack(t, debugOverride)

	producerFile := filepath.Join(s.dir, "Producer", "Program.cs")
	consumerFile := filepath.Join(s.dir, "Consumer", "Program.cs")
	produce := markerLine(t, producerFile, "produce")
	tick := markerLine(t, consumerFile, "tick")
	ids := map[string]string{"producer": s.containerID("producer"), "consumer": s.containerID("consumer")}

	sessionIDs := s.attachAll(ids)

	s.stopInProducer(producerFile, produce, sessionIDs["producer"])
	s.stopInConsumer(consumerFile, tick, sessionIDs["consumer"])
	s.checkEvents(sessionIDs)

	s.eyedbgCompose(&struct{}{}, "bp", "rm", "all", "-g", s.project)
	s.h.run("continue", "-s", sessionIDs["consumer"], "--timeout", "1s").wantCode(exitOK)

	var stopped struct {
		Members []struct {
			Service   string `json:"service"`
			SessionID string `json:"sessionId"`
		} `json:"members"`
	}

	s.eyedbgCompose(&stopped, "stop", "-g", s.project)

	if len(stopped.Members) != 2 {
		t.Fatalf("compose stop: %d members, want 2", len(stopped.Members))
	}

	s.requireRunning(ids["producer"], ids["consumer"])

	if left := s.sessions(); len(left) != 0 {
		t.Fatalf("sessions after compose stop: %+v, want none", left)
	}

	s.attachOne(ids["producer"], producerFile, produce)
	s.requireNoBuildInCopy()
}

// attachAll runs 'compose attach' and checks what it reports; it returns the
// sessions' ids by service.
func (s *composeStack) attachAll(ids map[string]string) map[string]string {
	s.t.Helper()

	var res groupResult

	s.eyedbgCompose(&res, append([]string{"attach"}, s.discovery()...)...)

	if res.Schema != 1 || res.Group != s.project || len(res.Members) != 2 {
		s.t.Fatalf("compose attach: schema %d group %q members %d, want 1, %s, 2", res.Schema, res.Group, len(res.Members), s.project)
	}

	sessionIDs := make(map[string]string, len(ids))

	for service, id := range ids {
		info := res.member(s.t, service).Session
		c := info.Container

		if info.Mode != api.ModeAttach || info.Group != s.project || info.State != api.StateRunning || c == nil {
			s.t.Fatalf("%s: mode %q group %q state %s container %+v, want an attached running member of %s", service, info.Mode, info.Group, info.State, c, s.project)
		}

		if c.ID != id || c.Service != service || c.Project != s.project || c.Launched {
			s.t.Fatalf("%s: container %+v, want id %s of this project, attached", service, c, id)
		}

		if !slices.Equal(c.Map, []api.PathMapping{{Remote: "/src", Local: s.dir}}) {
			s.t.Fatalf("%s: path map %+v, want /src=%s", service, c.Map, s.dir)
		}

		sessionIDs[service] = info.ID
	}

	s.requireUnhealthyAfter(res)

	return sessionIDs
}

// requireUnhealthyAfter checks what a stop would cost each service: the
// producer's healthcheck (5 s x 3 retries + 3 s timeout) turns unhealthy after
// 18 s, the consumer has none.
func (s *composeStack) requireUnhealthyAfter(res groupResult) {
	s.t.Helper()

	if got := res.member(s.t, "producer").Session.Container.UnhealthyAfter; got != api.Duration(18*time.Second) {
		s.t.Fatalf("producer unhealthyAfter = %v, want 18s", time.Duration(got))
	}

	if got := res.member(s.t, "consumer").Session.Container.UnhealthyAfter; got != 0 {
		s.t.Fatalf("consumer unhealthyAfter = %v, want none", time.Duration(got))
	}
}

// stopInProducer sets a breakpoint in every member and checks the stop in the
// producer: the frame is the host file, the excerpt shows the marker, the
// local is there.
func (s *composeStack) stopInProducer(file string, line int, sessionID string) {
	s.t.Helper()

	var added struct {
		Members []struct {
			Service    string         `json:"service"`
			SessionID  string         `json:"sessionId"`
			Breakpoint api.Breakpoint `json:"breakpoint"`
		} `json:"members"`
	}

	s.eyedbgCompose(&added, "bp", "add", loc(file, line), "-g", s.project)

	if len(added.Members) != 2 {
		s.t.Fatalf("compose bp add: %d members, want 2", len(added.Members))
	}

	snap := s.waitStop("producer", file, line, "marker: produce")
	if snap.Session.ID != sessionID {
		s.t.Fatalf("compose wait answered %s, want the producer's session %s", snap.Session.ID, sessionID)
	}

	s.h.run("vars", "-s", sessionID).wantCode(exitOK).wantStdout("item")

	// Clear every breakpoint (the producer would hit this one every half
	// second) before it runs on.
	s.eyedbgCompose(&struct{}{}, "bp", "rm", "all", "-g", s.project)
	s.h.run("continue", "-s", sessionID, "--timeout", "1s").wantCode(exitOK)
}

// stopInConsumer does the same in the consumer, a service running as a
// non-root user.
func (s *composeStack) stopInConsumer(file string, line int, sessionID string) {
	s.t.Helper()

	if user := s.inspect(s.containerID("consumer"), "{{.Config.User}}"); user == "" || user == "0" || user == "root" {
		s.t.Fatalf("the consumer's user is %q, want a non-root user", user)
	}

	s.eyedbgCompose(&struct{}{}, "bp", "add", loc(file, line), "-g", s.project)

	snap := s.waitStop("consumer", file, line, "marker: tick")
	if snap.Session.ID != sessionID {
		s.t.Fatalf("compose wait answered %s, want the consumer's session %s", snap.Session.ID, sessionID)
	}
}

// checkEvents requires both members' stops in the merged event log, with their
// session ids, and a cursor naming both.
func (s *composeStack) checkEvents(sessionIDs map[string]string) {
	s.t.Helper()

	var res groupEventsEnvelope

	s.eyedbgCompose(&res, "events", "-g", s.project, "--kind", "stopped")

	seen := map[string]string{}

	for i := range res.Events {
		if e := &res.Events[i]; e.Event.Kind == api.EventStopped {
			seen[e.Service] = e.SessionID
		}
	}

	for service, id := range sessionIDs {
		if seen[service] != id {
			s.t.Errorf("compose events: %s stopped in session %q, want %s (events %+v)", service, seen[service], id, res.Events)
		}

		if !strings.Contains(res.Cursor, id+":") {
			s.t.Errorf("compose events cursor %q does not name %s", res.Cursor, id)
		}
	}
}

// attachOne attaches to the producer's container with 'attach dotnet
// --container', stops it at the marker, and detaches, leaving the app running.
func (s *composeStack) attachOne(id, file string, line int) {
	s.t.Helper()

	var snap snapshotEnvelope

	s.h.run("--json", "attach", "dotnet", "--container", id, "--bp", loc(file, line), "--timeout", stopTimeout).wantCode(exitOK).decode(&snap)

	c := snap.Session.Container
	if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.File != file || snap.Frame.Line != line {
		s.t.Fatalf("attach --container: state %s frame %+v, want stopped at %s:%d", snap.Session.State, snap.Frame, file, line)
	}

	if c == nil || c.ID != id || c.Project != s.project || snap.Session.Mode != api.ModeAttach || snap.Session.PID != 0 {
		s.t.Fatalf("attach --container: container %+v mode %q pid %d, want an attached session of %s with no host pid", c, snap.Session.Mode, snap.Session.PID, id)
	}

	s.h.run("bp", "rm", "all", "-s", snap.Session.ID).wantCode(exitOK)
	s.h.run("continue", "-s", snap.Session.ID, "--timeout", "1s").wantCode(exitOK)
	s.h.run("detach", "-s", snap.Session.ID).wantCode(exitOK).wantStdout("keeps running")

	s.requireRunning(id)

	if !s.dotnetRunning(id) {
		s.t.Fatal("the producer's app is gone after detach")
	}
}

// decodeJSON decodes data into v, failing the test on error.
func decodeJSON(t *testing.T, data []byte, v any) {
	t.Helper()

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode %T: %v\n%s", v, err, data)
	}
}
