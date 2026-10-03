// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
)

// fastLabelPrefix starts every label eyedbg puts on a container it recreated
// for fast mode (docs/adr/0021).
const fastLabelPrefix = "dev.izzat.eyedbg.fast."

// TestComposeLaunch is ADR 0021's docker e2e: fast mode against the fixture's
// Release images. It needs the .NET SDK on this machine, the host's 'dotnet
// publish' builds the Debug build.
func TestComposeLaunch(t *testing.T) {
	requireDocker(t)

	if _, err := dotnet.FindHost(); err != nil {
		t.Fatalf("%s=1 needs the .NET SDK for fast mode's host build: %v", envE2EDocker, err)
	}

	s := newComposeStack(t, "") // the fixture's own Release images

	producerFile := filepath.Join(s.dir, "Producer", "Program.cs")
	startup := markerLine(t, producerFile, "startup")

	asBuilt := map[string]string{"producer": s.containerID("producer"), "consumer": s.containerID("consumer")}

	s.checkAsBuilt(asBuilt)

	producer := s.enterFastMode(producerFile, startup, asBuilt)
	s.checkStartupStop(producerFile, startup, producer)
	s.checkOutputInEyedbgOnly(producer)

	producer = s.rebuild(producerFile, startup, producer)

	s.stopLeavesIdle(producer)
	s.relaunchWithoutBuild(producerFile, startup, producer)

	s.checkRestore(producer.container)
	s.requireNoBuildInCopy()
}

// launched is a producer session in fast mode.
type launched struct {
	session   string
	container string
}

// checkAsBuilt checks the stack as docker compose built it (a Release build,
// the environment and env_file values in the app), attaches to it, and lets
// go: attached sessions end with 'compose stop', the services keep running.
func (s *composeStack) checkAsBuilt(ids map[string]string) {
	s.t.Helper()

	logs := s.logs(ids["producer"])
	if !strings.Contains(logs, "build=release") || !strings.Contains(logs, "env=compose/file") {
		s.t.Fatalf("the as-built producer's log has no 'build=release' and 'env=compose/file':\n%s", logs)
	}

	s.attachAll(ids)

	var stopped struct {
		Members []struct {
			Service string `json:"service"`
		} `json:"members"`
	}

	s.eyedbgCompose(&stopped, "stop", "-g", s.project)

	if len(stopped.Members) != 2 {
		s.t.Fatalf("compose stop: %d members, want 2", len(stopped.Members))
	}

	s.requireRunning(ids["producer"], ids["consumer"])

	if left := s.sessions(); len(left) != 0 {
		s.t.Fatalf("sessions after compose stop: %+v, want none", left)
	}
}

// launch runs 'compose launch' with this project's discovery flags and args.
func (s *composeStack) launch(args ...string) groupResult {
	s.t.Helper()

	var res groupResult

	s.eyedbgCompose(&res, append(append([]string{"launch"}, s.discovery()...), args...)...)

	if res.Schema != 1 || res.Group != s.project {
		s.t.Fatalf("compose launch: schema %d group %q, want 1 and %s", res.Schema, res.Group, s.project)
	}

	return res
}

// requireLaunched checks one member of a launch result: a launched session of
// the container with the labels fast mode puts on it. It returns the member.
func (s *composeStack) requireLaunched(res groupResult, service string) memberResult {
	s.t.Helper()

	m := res.member(s.t, service)
	info := m.Session
	c := info.Container

	if c == nil || !c.Launched || c.Service != service || c.Project != s.project || info.Group != s.project {
		s.t.Fatalf("%s: container %+v group %q, want a launched session of %s", service, c, info.Group, s.project)
	}

	// A launched session is not an attach: no mode, no host pid.
	if info.Mode != "" || info.PID != 0 {
		s.t.Fatalf("%s: mode %q pid %d, want a launched session with no host pid", service, info.Mode, info.PID)
	}

	if m.Fast == nil || c.ID != s.containerID(service) {
		s.t.Fatalf("%s: fast %+v container %s, want fast mode's result for %s", service, m.Fast, c.ID, s.containerID(service))
	}

	override := filepath.Join(s.h.homeDir, "compose", s.project, "override.yml")
	if m.Fast.Override != override {
		s.t.Fatalf("%s: override %q, want %q", service, m.Fast.Override, override)
	}

	if _, ok := s.labels(c.ID)[fastLabelPrefix+"override"]; !ok {
		s.t.Fatalf("%s: container %s has no %soverride label", service, c.ID[:12], fastLabelPrefix)
	}

	return m
}

// labels returns the container's labels.
func (s *composeStack) labels(id string) map[string]string {
	s.t.Helper()

	var labels map[string]string

	decodeJSON(s.t, []byte(s.inspect(id, "{{json .Config.Labels}}")), &labels)

	return labels
}

// enterFastMode launches both services with a breakpoint at the producer's
// startup marker: the containers are recreated (a new id each), and the
// producer's session is returned.
func (s *composeStack) enterFastMode(file string, line int, asBuilt map[string]string) launched {
	s.t.Helper()

	res := s.launch("--bp", loc(file, line))

	var producer launched

	for service, old := range asBuilt {
		m := s.requireLaunched(res, service)

		if m.Session.Container.ID == old || !m.Fast.Recreated {
			s.t.Fatalf("%s: container %s (was %s), recreated %v, want a recreated container", service, m.Session.Container.ID[:12], old[:12], m.Fast.Recreated)
		}

		if service == "producer" {
			producer = launched{session: m.Session.ID, container: m.Session.Container.ID}
		}
	}

	return producer
}

// checkStartupStop checks the producer stopped at its startup line (before
// any app code ran), then that the app got the container's environment and is
// the Debug build.
func (s *composeStack) checkStartupStop(file string, line int, p launched) {
	s.t.Helper()

	snap := s.waitStop("producer", file, line, "marker: startup")
	if snap.Session.ID != p.session {
		s.t.Fatalf("compose wait answered %s, want the producer's session %s", snap.Session.ID, p.session)
	}

	// The marker line assigns startupEnv: step over it.
	s.h.run("next", "-s", p.session).wantCode(exitOK)

	if got := s.eval(p.session, "startupEnv"); got != `"compose/file"` {
		s.t.Fatalf("startupEnv = %s, want \"compose/file\" (environment: and env_file: values)", got)
	}

	if got := s.eval(p.session, "Program.Build"); got != `"debug"` {
		s.t.Fatalf("Program.Build = %s, want \"debug\"", got)
	}

	s.h.run("continue", "-s", p.session, "--timeout", "1s").wantCode(exitOK)
}

// eval evaluates expr in the session's top frame and returns its value as the
// adapter printed it.
func (s *composeStack) eval(session, expr string) string {
	s.t.Helper()

	var res evalEnvelope

	s.h.run("--json", "eval", "-s", session, expr).wantCode(exitOK).decode(&res)

	return res.Value
}

// requireOutput waits, for a bounded time, until a 'compose events --kind
// output' event of service contains text, following the cursor with the
// command's own --wait.
func (s *composeStack) requireOutput(service, text string) {
	s.t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	cursor := ""

	for {
		args := []string{"events", "-g", s.project, "--kind", "output", "--limit", "1000"}
		if cursor != "" {
			args = append(args, "--since", cursor, "--wait", "--timeout", "5s")
		}

		var res groupEventsEnvelope

		s.eyedbgCompose(&res, args...)

		for i := range res.Events {
			if e := &res.Events[i]; e.Service == service && e.Event.Kind == api.EventOutput && strings.Contains(e.Event.Text, text) {
				return
			}
		}

		if time.Now().After(deadline) {
			s.t.Fatalf("no %s output containing %q in 60 s", service, text)
		}

		cursor = res.Cursor
	}
}

// checkOutputInEyedbgOnly checks the app's output is in eyedbg's merged
// events and not in docker's log, and that the app runs in the container.
func (s *composeStack) checkOutputInEyedbgOnly(p launched) {
	s.t.Helper()

	s.requireOutput("producer", "tag=v1 build=debug env=compose/file")
	s.requireOutput("producer", "produced item")

	if logs := strings.TrimSpace(s.logs(p.container)); logs != "" {
		s.t.Fatalf("docker logs of the fast-mode producer is not empty:\n%s", logs)
	}

	if !s.dotnetRunning(p.container) {
		s.t.Fatal("no dotnet process in the fast-mode producer")
	}
}

// rebuild edits the producer's Tag, launches the service again, and checks the
// rebuild happened in the same container, with the breakpoint carried over.
func (s *composeStack) rebuild(file string, line int, old launched) launched {
	s.t.Helper()

	src, err := os.ReadFile(file)
	if err != nil {
		s.t.Fatal(err)
	}

	edited := strings.Replace(string(src), `Tag = "v1"`, `Tag = "v2"`, 1)
	if edited == string(src) {
		s.t.Fatalf("%s has no 'Tag = \"v1\"' to edit", file)
	}

	if err := os.WriteFile(file, []byte(edited), 0o600); err != nil { //nolint:gosec // The fixture copy's own source file.
		s.t.Fatal(err)
	}

	m := s.requireLaunched(s.launch("producer"), "producer")

	if m.Session.Container.ID != old.container || m.Fast.Recreated || m.Fast.Carried != 1 {
		s.t.Fatalf("rebuild: container %s (was %s) recreated %v carried %d, want the same container, not recreated, 1 breakpoint carried",
			m.Session.Container.ID[:12], old.container[:12], m.Fast.Recreated, m.Fast.Carried)
	}

	p := launched{session: m.Session.ID, container: old.container}

	s.requireBreakpointAt(p.session, file, line)

	snap := s.waitStop("producer", file, line, "marker: startup")
	if snap.Session.ID != p.session {
		s.t.Fatalf("compose wait answered %s, want the rebuilt producer's session %s", snap.Session.ID, p.session)
	}

	if got := s.eval(p.session, "Program.Tag"); got != `"v2"` {
		s.t.Fatalf("Program.Tag = %s after the rebuild, want \"v2\"", got)
	}

	return p
}

// requireBreakpointAt checks the session lists a breakpoint at file:line.
func (s *composeStack) requireBreakpointAt(session, file string, line int) {
	s.t.Helper()

	var res struct {
		Breakpoints []api.Breakpoint `json:"breakpoints"`
	}

	s.h.run("--json", "bp", "ls", "-s", session).wantCode(exitOK).decode(&res)

	if !slices.ContainsFunc(res.Breakpoints, func(b api.Breakpoint) bool { return b.File == file && b.RequestedLine == line }) {
		s.t.Fatalf("session %s breakpoints %+v, want one at %s:%d (carried over)", session, res.Breakpoints, file, line)
	}
}

// stopLeavesIdle ends the producer's session with 'eyedbg stop': the app is
// killed and the container keeps running, idle.
func (s *composeStack) stopLeavesIdle(p launched) {
	s.t.Helper()

	s.h.run("stop", "-s", p.session).wantCode(exitOK).wantStdout("idles")

	s.requireRunning(p.container)
	s.requireNoDotnet(p.container)
}

// relaunchWithoutBuild launches the producer from the last build (--no-build):
// the rebuilt one, in the same container.
func (s *composeStack) relaunchWithoutBuild(file string, line int, old launched) {
	s.t.Helper()

	m := s.requireLaunched(s.launch("--no-build", "--bp", loc(file, line), "producer"), "producer")
	if m.Session.Container.ID != old.container || m.Fast.Recreated {
		s.t.Fatalf("--no-build: container %s (was %s) recreated %v, want the same container", m.Session.Container.ID[:12], old.container[:12], m.Fast.Recreated)
	}

	s.waitStop("producer", file, line, "marker: startup")

	if got := s.eval(m.Session.ID, "Program.Tag"); got != `"v2"` {
		s.t.Fatalf("Program.Tag = %s after --no-build, want \"v2\" (the last build)", got)
	}
}

// checkRestore ends every session, restores both services, and checks they
// run as built again with none of eyedbg's labels and files left.
func (s *composeStack) checkRestore(producer string) {
	s.t.Helper()

	s.eyedbgCompose(&struct{}{}, "stop", "-g", s.project)

	if left := s.sessions(); len(left) != 0 {
		s.t.Fatalf("sessions after compose stop: %+v, want none", left)
	}

	var res struct {
		Members []struct {
			Service  string `json:"service"`
			Restored bool   `json:"restored"`
		} `json:"members"`
		Removed bool `json:"removed"`
	}

	s.eyedbgCompose(&res, append([]string{"restore"}, s.discovery()...)...)

	if len(res.Members) != 2 || !res.Members[0].Restored || !res.Members[1].Restored || !res.Removed {
		s.t.Fatalf("compose restore: %+v removed %v, want both services restored and eyedbg's files removed", res.Members, res.Removed)
	}

	for service, dll := range map[string]string{"producer": "Producer.dll", "consumer": "Consumer.dll"} {
		id := s.containerID(service)
		s.requireRunning(id)

		for k := range s.labels(id) {
			if strings.HasPrefix(k, fastLabelPrefix) {
				s.t.Fatalf("%s still has the label %s after restore", service, k)
			}
		}

		if got := s.inspect(id, "{{json .Config.Entrypoint}}"); got != `["dotnet","`+dll+`"]` {
			s.t.Fatalf("%s entrypoint = %s after restore, want the image's own", service, got)
		}

		if id == producer {
			s.t.Fatalf("%s kept container %s: restore should recreate it", service, id[:12])
		}
	}

	if logs := s.logs(s.containerID("producer")); !strings.Contains(logs, "build=release") {
		s.t.Fatalf("the restored producer's log has no 'build=release':\n%s", logs)
	}

	if _, err := os.Stat(filepath.Join(s.h.homeDir, "compose", s.project)); !os.IsNotExist(err) {
		s.t.Fatalf("eyedbg's files for the project: stat error %v, want them removed", err)
	}

	if left := s.sessions(); len(left) != 0 {
		s.t.Fatalf("sessions after restore: %+v, want none", left)
	}
}
