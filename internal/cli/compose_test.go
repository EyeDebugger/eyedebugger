// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// composeFake attaches "containers" on the fake adapter: each service's
// program is /src/<service>.txt there, and the default map sends /src to
// local. Service "db" is not .NET (a skip when picked implicitly), "bad" is
// not running, "init" runs docker-init as pid 1.
type composeFake struct {
	fakeDriver

	local string
	seen  *seenSpecs
}

func (composeFake) Name() string { return dotnet.Language }

func (d composeFake) PrepareContainerAttach(_ context.Context, spec api.AttachSpec) (session.Launch, error) {
	cs := spec.Container
	d.seen.add(*cs)

	name := cs.Project + "-" + cs.Service + "-1"

	switch cs.Service {
	case "db":
		if cs.RequireDotnet {
			return session.Launch{}, errors.Join(api.NewError(api.CodeNotDotnet, "pid 1 runs postgres", ""), session.ErrNotCandidate)
		}
	case "bad":
		return session.Launch{}, api.NewError(api.CodeAttachFailed, "container "+name+" is not running", "start it first")
	case "init":
		return session.Launch{}, api.NewError(api.CodeInvalidRequest, "container "+name+" runs docker-init as pid 1", "pass --pid")
	}

	path, args, env, err := daptest.Command()
	if err != nil {
		return session.Launch{}, err
	}

	maps := cs.Map
	if len(maps) == 0 {
		maps = []api.PathMapping{{Remote: "/src", Local: d.local}}
	}

	pm, err := session.NewPathMap(maps)
	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Request: session.RequestAttach, PID: 1,
		Arguments: daptest.ProgramArgs{Program: "/src/" + cs.Service + ".txt", Lines: 10, Hang: true}.Map(), Program: cs.Service + " (container " + name + ")",
		PathMap: pm, Container: &api.ContainerInfo{
			// A distinct full id per container: one app per container.
			ID: cs.Ref + strings.Repeat("0", 64-len(cs.Ref)), Name: name, Service: cs.Service, Project: cs.Project, PID: 1,
			Platform: "linux/arm64", Map: pm.Mappings(), UnhealthyAfter: api.Duration(18 * time.Second),
		},
	}, nil
}

// composeRig is the compose commands' outside world: the projects docker
// compose ps lists, and what it was asked.
type composeRig struct {
	mu       sync.Mutex
	projects map[string][]container.ComposeService
	psErr    error
	asked    []psAsk
}

type psAsk struct {
	host, dockerContext string
	opts                container.ComposeOptions
}

func (r *composeRig) deps() composeDeps {
	return composeDeps{
		now: time.Now,
		ps: func(_ context.Context, host, dockerContext string, o container.ComposeOptions) ([]container.ComposeService, error) {
			r.mu.Lock()
			defer r.mu.Unlock()

			r.asked = append(r.asked, psAsk{host, dockerContext, o})

			return slices.Clone(r.projects[o.ProjectName]), r.psErr
		},
	}
}

func (r *composeRig) lastAsk() psAsk {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.asked) == 0 {
		return psAsk{}
	}

	return r.asked[len(r.asked)-1]
}

func svc(id, service, project, state string) container.ComposeService {
	return container.ComposeService{ID: id, Name: project + "-" + service + "-1", Service: service, Project: project, State: state}
}

// runCompose executes eyedbg with the rig's compose commands, checks the
// exit code and returns stdout and stderr.
func runCompose(t *testing.T, r *composeRig, wantCode int, args ...string) (stdout, stderr string) {
	t.Helper()

	out, errOut, code := execute(t, newEyedbgCommand(testInfo, r.deps()), args)
	if code != wantCode {
		t.Fatalf("eyedbg %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, wantCode, out, errOut)
	}

	return string(out), string(errOut)
}

// sourceFiles creates empty files named in dir and returns dir resolved.
func sourceFiles(t *testing.T, names ...string) string {
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

// sessionIDs maps each session's service to its id, by 'sessions --json'.
func sessionIDs(t *testing.T, r *composeRig) map[string]string {
	t.Helper()

	out, _ := runCompose(t, r, 0, "sessions", "--json")

	var doc struct {
		Sessions []api.SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("sessions --json: %v\n%s", err, out)
	}

	ids := map[string]string{}

	for i := range doc.Sessions {
		if c := doc.Sessions[i].Container; c != nil {
			ids[c.Service] = doc.Sessions[i].ID
		}
	}

	return ids
}

// composeStack is the stack of the end-to-end test: two .NET services, a
// database and an exited container in project myapp, one service in other.
type composeStack struct {
	t              *testing.T
	r              *composeRig
	seen           *seenSpecs
	local, outside string
	ids            map[string]string
}

func (c *composeStack) run(wantCode int, args ...string) (stdout, stderr string) {
	c.t.Helper()

	return runCompose(c.t, c.r, wantCode, args...)
}

// TestComposeEndToEnd drives every compose command through the real command
// tree, an in-process daemon and a fake container driver. Not parallel: it
// sets environment variables.
func TestComposeEndToEnd(t *testing.T) {
	c := &composeStack{t: t, seen: &seenSpecs{}, local: sourceFiles(t, "web.txt"), outside: sourceFiles(t, "x.txt")}

	serveWithDrivers(t, isolate(t), composeFake{local: c.local, seen: c.seen})
	t.Setenv(envDockerHost, "tcp://engine.example:2376")
	t.Setenv(envDockerContext, "")
	t.Setenv(envGroup, "")

	c.r = &composeRig{projects: map[string][]container.ComposeService{
		"myapp": {
			svc("3456789abcde", "old", "myapp", "exited"), svc("23456789abcd", "db", "myapp", "running"),
			svc("123456789abc", "consumer", "myapp", "running"), svc("0123456789ab", "web", "myapp", "running"),
		},
		"other": {svc("456789abcdef", "api", "other", "running")},
	}}

	c.attach()
	c.breakpoints()
	c.waiting()
	c.events()
	c.groups()
	c.stop()
}

// attach: the .NET services, the rest skipped, what was asked of docker; a
// second attach fails per member.
func (c *composeStack) attach() {
	t := c.t
	t.Helper()

	out, _ := c.run(0, "compose", "attach", "-f", "a.yml", "-f", "b.yml", "-p", "myapp", "--project-directory", "/x/y")
	expectOutput(t, out, "group myapp: attached to 2 of 3 service(s)", "consumer", "web", "skipped", "pid 1 runs postgres", "path map: /src="+c.local, "next: eyedbg compose wait -g myapp")

	got := c.r.lastAsk()
	if got.host != "tcp://engine.example:2376" || !slices.Equal(got.opts.Files, []string{"a.yml", "b.yml"}) || got.opts.ProjectName != "myapp" || got.opts.ProjectDirectory != "/x/y" {
		t.Errorf("docker compose ps was asked %+v", got)
	}

	checkSpecs(t, c.seen)

	c.ids = sessionIDs(t, c.r)
	if len(c.ids) != 2 || c.ids["web"] == "" || c.ids["consumer"] == "" {
		t.Fatalf("sessions = %v, want web and consumer", c.ids)
	}

	// One app per container: the output is the per-service table, the exit
	// code the first failure's, and --json stays one document.
	out, errOut := c.run(exitError, "compose", "attach", "-p", "myapp")
	expectOutput(t, out, "attached to 0 of 3 service(s)", "failed", "skipped")
	expectOutput(t, errOut, "[INVALID_REQUEST]")

	out, errOut = c.run(exitError, "compose", "attach", "-p", "myapp", "--json")
	if err := json.Unmarshal([]byte(out), &struct{}{}); err != nil || errOut != "" {
		t.Errorf("--json of a failed attach is not one document: %v\nstdout: %s\nstderr: %s", err, out, errOut)
	}
}

// breakpoints: in every member; one outside every map is skipped.
func (c *composeStack) breakpoints() {
	t := c.t
	t.Helper()

	out, _ := c.run(0, "compose", "bp", "add", filepath.Join(c.local, "web.txt")+":3")
	expectOutput(t, out, "web", "consumer", "web.txt:3 verified")

	out, errOut := c.run(exitError, "compose", "bp", "add", filepath.Join(c.outside, "x.txt")+":3")
	expectOutput(t, out, "skipped: its file is outside this service's path map")
	expectOutput(t, errOut, "outside every service's path map")

	out, _ = c.run(0, "compose", "bp", "ls")
	expectOutput(t, out, "web (", "consumer (", "agent")

	out, _ = c.run(0, "compose", "bp", "ls", "--mine", "--json")
	if !strings.Contains(out, `"breakpoints"`) || !strings.Contains(out, `"schema":1`) {
		t.Errorf("bp ls --json = %s", out)
	}

	out, _ = c.run(0, "compose", "bp", "rm", "all")
	expectOutput(t, out, "removed 1 breakpoint(s)")

	_, errOut = c.run(exitError, "compose", "bp", "rm", "3")
	expectOutput(t, errOut, "takes 'all'")
}

// waiting: nothing stopped yet; then web and consumer stopped, the one
// stopped longest answers.
func (c *composeStack) waiting() {
	t := c.t
	t.Helper()

	out, _ := c.run(0, "compose", "wait", "--timeout", "200ms")
	expectOutput(t, out, "group myapp: nothing stopped within 200ms")

	c.run(0, "pause", "-s", c.ids["web"])
	c.run(0, "pause", "-s", c.ids["consumer"])

	out, _ = c.run(0, "compose", "wait")
	expectOutput(t, out, "group myapp: "+c.ids["web"]+" (web) stopped: pause", "(stopped ", "also stopped:", "consumer ("+c.ids["consumer"]+")", "to act on it: eyedbg vars -s "+c.ids["web"])

	out, _ = c.run(0, "compose", "wait", "--new", "--timeout", "200ms")
	expectOutput(t, out, "no other member stopped within 200ms", "already stopped (before this wait):")

	out, _ = c.run(0, "compose", "wait", "--json")

	var waited api.GroupWaitResult
	if err := json.Unmarshal([]byte(out), &waited); err != nil || waited.Service != "web" || len(waited.Stopped) != 1 || waited.Stopped[0].Service != "consumer" {
		t.Errorf("wait --json = %+v, %v", waited, err)
	}
}

// events: merged, service-prefixed; the cursor reads on.
func (c *composeStack) events() {
	t := c.t
	t.Helper()

	out, _ := c.run(0, "compose", "events", "--kind", "stopped")
	expectOutput(t, out, "web  ", "consumer  ", "stopped: pause", "next: eyedbg compose events -g myapp --since ")

	cursor := regexp.MustCompile(`--since (\S+)`).FindStringSubmatch(out)
	if cursor == nil {
		t.Fatalf("no cursor in\n%s", out)
	}

	out, _ = c.run(0, "compose", "events", "--since", cursor[1], "--kind", "stopped")
	expectOutput(t, out, "(no events)")

	out, _ = c.run(0, "compose", "events", "--since", cursor[1], "--kind", "stopped", "--wait", "--timeout", "200ms")
	expectOutput(t, out, "(no new events within 200ms)")

	out, _ = c.run(0, "compose", "events", "--newest", "--limit", "2", "--kind", "stopped")
	expectOutput(t, out, "stopped: pause")
}

// groups: with several, -g or the environment names one.
func (c *composeStack) groups() {
	t := c.t
	t.Helper()

	c.run(0, "compose", "attach", "-p", "other")

	_, errOut := c.run(exitError, "compose", "wait", "--timeout", "100ms")
	expectOutput(t, errOut, "several groups exist: myapp, other", "[INVALID_REQUEST]")

	out, _ := c.run(0, "compose", "wait", "-g", "other", "--timeout", "100ms")
	expectOutput(t, out, "group other: nothing stopped")

	t.Setenv(envGroup, "other")

	out, _ = c.run(0, "compose", "wait", "--timeout", "100ms")
	expectOutput(t, out, "group other: nothing stopped")

	out, _ = c.run(0, "compose", "stop", "--json")
	if !strings.Contains(out, `"group":"other"`) {
		t.Errorf("stop --json = %s", out)
	}

	t.Setenv(envGroup, "")
}

// stop: every member, the containers keep running; the group is gone.
func (c *composeStack) stop() {
	t := c.t
	t.Helper()

	out, _ := c.run(0, "compose", "stop")
	expectOutput(t, out, "detached; container myapp-web-1 keeps running", "detached; container myapp-consumer-1 keeps running")

	_, errOut := c.run(exitState, "compose", "wait", "--timeout", "100ms")
	expectOutput(t, errOut, "[NO_SESSION]")

	_, errOut = c.run(exitState, "compose", "stop", "-g", "myapp")
	expectOutput(t, errOut, "no group myapp")

	if out, _ := c.run(0, "sessions"); strings.TrimSpace(out) != "" {
		t.Errorf("sessions left: %s", out)
	}
}

// checkSpecs checks what the daemon's driver was asked to attach: the
// containers' ids, implicit members requiring .NET, the engine.
func checkSpecs(t *testing.T, seen *seenSpecs) {
	t.Helper()

	seen.mu.Lock()
	defer seen.mu.Unlock()

	byService := map[string]api.ContainerSpec{}
	for _, s := range seen.specs {
		byService[s.Service] = s
	}

	web := byService["web"]
	if web.Ref != "0123456789ab" || web.Project != "myapp" || !web.RequireDotnet || web.Engine.Host != "tcp://engine.example:2376" || len(web.Map) != 0 {
		t.Errorf("web was attached as %+v", web)
	}

	if db := byService["db"]; db.Ref != "23456789abcd" || !db.RequireDotnet {
		t.Errorf("db was tried as %+v", db)
	}

	if _, ok := byService["old"]; ok {
		t.Error("a container that is not running was tried")
	}
}

// TestComposeAttachOptions: named services (always attempted), --map,
// breakpoints on every member, and what goes wrong before or after docker.
func TestComposeAttachOptions(t *testing.T) {
	c := &composeStack{t: t, seen: &seenSpecs{}, local: sourceFiles(t, "web.txt"), outside: sourceFiles(t, "y.txt")}

	serveWithDrivers(t, isolate(t), composeFake{local: c.local, seen: c.seen})
	t.Setenv(envDockerHost, "")
	t.Setenv(envDockerContext, "ctx1")
	t.Setenv(envGroup, "")

	web, db := svc("0123456789ab", "web", "myapp", "running"), svc("23456789abcd", "db", "myapp", "running")
	c.r = &composeRig{projects: map[string][]container.ComposeService{"": {web, db, svc("9abcdef01234", "bad", "myapp", "running")}}}

	t.Chdir(c.local)

	c.named()
	c.partialFailure()
	c.nothingAttached(db, web)
	c.invalidCommandLines()
	c.dockerSays(web)
}

// named: services are attempted even when not .NET; --map and --bp reach
// every attached member.
func (c *composeStack) named() {
	t := c.t
	t.Helper()

	out, _ := c.run(0, "compose", "attach", "db", "web", "--map", "/src=.", "--bp", filepath.Join(c.local, "web.txt")+":3", "--bp", filepath.Join(c.outside, "y.txt")+":1",
		"--exceptions", "all", "--lease-policy", "handoff", "--no-record")
	expectOutput(t, out, "attached to 2 of 2 service(s)", "breakpoint "+filepath.Join(c.local, "web.txt")+":3:", "verified", "skipped: its file is outside this service's path map")

	if got := c.r.lastAsk(); got.dockerContext != "ctx1" || got.host != "" {
		t.Errorf("ps engine = %+v", got)
	}

	c.seen.mu.Lock()
	for _, s := range c.seen.specs {
		if s.RequireDotnet || len(s.Map) != 1 || s.Map[0].Remote != "/src" || s.Map[0].Local != c.local {
			t.Errorf("named member attached as %+v", s)
		}
	}
	c.seen.mu.Unlock()

	c.run(0, "compose", "stop")
}

// partialFailure: a failing service doesn't keep the others from attaching.
func (c *composeStack) partialFailure() {
	t := c.t
	t.Helper()

	out, _ := c.run(0, "compose", "attach", "web", "bad", "--json")

	var doc struct {
		Schema  int              `json:"schema"`
		Group   string           `json:"group"`
		Members []attachedMember `json:"members"`
	}

	// Members are in service order: bad, web.
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Schema != 1 || doc.Group != "myapp" || len(doc.Members) != 2 ||
		doc.Members[1].Session == nil || doc.Members[0].Error == nil || doc.Members[0].Error.Code != api.CodeAttachFailed {
		t.Errorf("attach --json = %+v, %v", doc, err)
	}

	c.run(0, "compose", "stop")
}

// nothingAttached: the first failure's class; everything skipped.
func (c *composeStack) nothingAttached(db, web container.ComposeService) {
	t := c.t
	t.Helper()

	_, errOut := c.run(exitEnvironment, "compose", "attach", "bad")
	expectOutput(t, errOut, "[ATTACH_FAILED]", "container myapp-bad-1 is not running")

	c.r.projects[""] = []container.ComposeService{db}
	out, errOut := c.run(exitError, "compose", "attach")
	expectOutput(t, out, "skipped", "pid 1 runs postgres")
	expectOutput(t, errOut, "no .NET services running", "[INVALID_REQUEST]")

	c.r.projects[""] = []container.ComposeService{web}
}

// invalidCommandLines fail before docker or the daemon is asked.
func (c *composeStack) invalidCommandLines() {
	t := c.t
	t.Helper()

	before := len(c.r.asked)

	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"compose", "attach", "-p", "Bad Name"}, "invalid project name"},
		{[]string{"compose", "attach", "--map", "nonsense"}, `invalid --map "nonsense"`},
		{[]string{"compose", "attach", "--bp", "nonsense"}, "invalid location"},
		{[]string{"compose", "attach", "--exceptions", "sometimes"}, "unknown exception mode"},
		{[]string{"compose", "attach", "--lease-policy", "whenever"}, "invalid lease policy"},
		{[]string{"compose", "attach", "--privileged"}, "unknown flag"},
		{[]string{"compose", "attach", "-f", ""}, "invalid -f value"},
		{[]string{"compose", "attach", "web;rm"}, "invalid service"},
		{[]string{"compose", "attach", "-web"}, "unknown shorthand flag"},
	} {
		_, errOut := c.run(exitError, tt.args...)
		expectOutput(t, errOut, tt.want)
	}

	if len(c.r.asked) != before {
		t.Errorf("docker compose ps ran for an invalid command line (%d calls, was %d)", len(c.r.asked), before)
	}
}

// dockerSays: what the compose project turns out to be.
func (c *composeStack) dockerSays(web container.ComposeService) {
	t := c.t
	t.Helper()

	_, errOut := c.run(exitError, "compose", "attach", "nosuch")
	expectOutput(t, errOut, "service nosuch is not running", "running: web")

	c.r.projects[""] = nil
	_, errOut = c.run(exitError, "compose", "attach")
	expectOutput(t, errOut, "no running containers in this compose project")

	c.r.projects[""] = []container.ComposeService{web, svc("0123456789cd", "api", "other", "running")}
	_, errOut = c.run(exitError, "compose", "attach")
	expectOutput(t, errOut, "containers of several compose projects: myapp, other", "-p NAME")

	c.r.psErr = api.NewError(api.CodeAttachFailed, "docker compose ps failed: no engine", "start docker")
	_, errOut = c.run(exitEnvironment, "compose", "attach")
	expectOutput(t, errOut, "[ATTACH_FAILED]", "no engine")
}

// TestComposeNeedsADaemon: the commands that act on sessions never start a
// daemon.
func TestComposeNeedsADaemon(t *testing.T) {
	isolate(t)

	r := &composeRig{}

	for _, args := range [][]string{
		{"compose", "wait"}, {"compose", "events"}, {"compose", "bp", "add", "x.cs:1"}, {"compose", "bp", "ls"}, {"compose", "bp", "rm", "all"}, {"compose", "stop"},
	} {
		_, errOut := runCompose(t, r, exitState, args...)
		expectOutput(t, errOut, "[NO_SESSION]", "the daemon is not running")
	}
}

// TestComposeGroupResolution: -g, then $EYEDBG_GROUP, then the only group.
func TestComposeGroupResolution(t *testing.T) {
	t.Setenv(envGroup, "")

	if g, err := explicitGroup(""); g != "" || err != nil {
		t.Errorf("explicitGroup(\"\") = %q, %v", g, err)
	}

	t.Setenv(envGroup, "fromenv")

	if g, err := explicitGroup(""); g != "fromenv" || err != nil {
		t.Errorf("env: %q, %v", g, err)
	}

	if g, err := explicitGroup("flag"); g != "flag" || err != nil {
		t.Errorf("flag beats the environment: %q, %v", g, err)
	}

	if _, err := explicitGroup("Not Valid"); api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("an invalid group: %v", err)
	}
}

func TestOnlyGroup(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		list     []api.SessionInfo
		want     string
		wantCode api.Code
		wantMsg  string
	}{
		{"none", nil, "", api.CodeNoSession, "no compose group"},
		{"host sessions have none", []api.SessionInfo{{ID: "s-1"}}, "", api.CodeNoSession, "no compose group"},
		{"lost ones don't count", []api.SessionInfo{{ID: "s-1", Group: "a", State: api.StateLost}}, "", api.CodeNoSession, "no compose group"},
		{"one", []api.SessionInfo{{ID: "s-1", Group: "a"}, {ID: "s-2", Group: "a"}, {ID: "s-3"}}, "a", "", ""},
		{"exited ones count", []api.SessionInfo{{ID: "s-1", Group: "a", State: api.StateExited}}, "a", "", ""},
		{"several", []api.SessionInfo{{ID: "s-1", Group: "b"}, {ID: "s-2", Group: "a"}}, "", api.CodeInvalidRequest, "several groups exist: a, b"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := onlyGroup(tt.list)
			if tt.wantCode == "" {
				if err != nil || got != tt.want {
					t.Errorf("onlyGroup = %q, %v; want %q", got, err, tt.want)
				}

				return
			}

			if api.CodeOf(err) != tt.wantCode || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("onlyGroup error = %v, want %s containing %q", err, tt.wantCode, tt.wantMsg)
			}
		})
	}
}

func TestSelectServices(t *testing.T) {
	t.Parallel()

	web, web2 := svc("0123456789ab", "web", "myapp", "running"), svc("0123456789ac", "web", "myapp", "running")
	web2.Name = "myapp-web-2"

	db, gone := svc("23456789abcd", "db", "myapp", "running"), svc("3456789abcde", "gone", "myapp", "exited")
	api1 := svc("456789abcdef", "api", "other", "running")

	many := make([]container.ComposeService, maxComposeMembers+1)
	for i := range many {
		many[i] = svc("0123456789ab", "web", "myapp", "running")
	}

	tests := []struct {
		name         string
		rows         []container.ComposeService
		names        []string
		wantServices []string
		wantProject  string
		wantMsg      string
	}{
		{"all running, sorted by service then name", []container.ComposeService{web2, db, gone, web}, nil, []string{"db", "web", "web"}, "myapp", ""},
		{"named", []container.ComposeService{db, web}, []string{"web"}, []string{"web"}, "myapp", ""},
		{"scaled service: every container", []container.ComposeService{web2, web}, []string{"web"}, []string{"web", "web"}, "myapp", ""},
		{"named but not running", []container.ComposeService{web, gone}, []string{"gone"}, nil, "", "service gone is not running"},
		{"named but unknown", []container.ComposeService{web, db}, []string{"nosuch"}, nil, "", "running: db, web"},
		{"none running", []container.ComposeService{gone}, nil, nil, "", "no running containers"},
		{"nothing at all", nil, nil, nil, "", "no running containers"},
		{"two projects", []container.ComposeService{web, api1}, nil, nil, "", "several compose projects: myapp, other"},
		{"two projects, one named", []container.ComposeService{web, api1}, []string{"api"}, nil, "", "several compose projects"},
		{"too many", many, nil, nil, "", "at most 64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, project, err := selectServices(tt.rows, tt.names)
			if tt.wantMsg != "" {
				if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error()+" "+hintOf(err), tt.wantMsg) {
					t.Errorf("selectServices error = %v, want INVALID_REQUEST containing %q", err, tt.wantMsg)
				}

				return
			}

			if err != nil || project != tt.wantProject {
				t.Fatalf("selectServices = %v, %q, %v", got, project, err)
			}

			var services []string
			for _, g := range got {
				services = append(services, g.Service)
			}

			if !slices.Equal(services, tt.wantServices) {
				t.Errorf("services = %v, want %v", services, tt.wantServices)
			}
		})
	}
}

func hintOf(err error) string {
	if e, ok := errors.AsType[*api.Error](err); ok {
		return e.Hint
	}

	return ""
}

func TestOutsideMap(t *testing.T) {
	t.Parallel()

	outside := api.NewError(api.CodeInvalidRequest, "/x/y.cs"+api.MessageOutsidePathMap, "the map is /src=/work")

	for _, tt := range []struct {
		err  error
		want bool
	}{
		{outside, true},
		{errors.Join(errors.New("x"), outside), true},
		{api.NewError(api.CodeAttachFailed, "/x/y.cs"+api.MessageOutsidePathMap, ""), false},
		{api.NewError(api.CodeInvalidRequest, "no such file", ""), false},
		{errors.New("plain"), false},
		{nil, false},
	} {
		if got := outsideMap(tt.err); got != tt.want {
			t.Errorf("outsideMap(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestAttachFailure(t *testing.T) {
	t.Parallel()

	boom := api.NewError(api.CodeAdapterMissing, "netcoredbg for linux/arm64 is not installed", "install it")
	sess := &api.SessionInfo{ID: "s-1"}

	if err := attachFailure([]attachedMember{{Session: sess}, {Error: boom}}); err != nil {
		t.Errorf("one attached: %v", err)
	}

	err := attachFailure([]attachedMember{{Skipped: "pid 1 runs nginx"}, {Error: boom}})
	if api.CodeOf(err) != api.CodeAdapterMissing || !isReported(err) || exitCode(err) != exitEnvironment {
		t.Errorf("first failure: %v (reported %v)", err, isReported(err))
	}

	err = attachFailure([]attachedMember{{Skipped: "pid 1 runs nginx"}})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "no .NET services running") {
		t.Errorf("all skipped: %v", err)
	}
}

func TestEveryMemberFailed(t *testing.T) {
	t.Parallel()

	boom := api.NewError(api.CodeLeaseHeld, "held", "")
	errs := []*api.Error{boom, nil}

	if err := everyMemberFailed(2, func(i int) *api.Error { return errs[i] }); err != nil {
		t.Errorf("one answered: %v", err)
	}

	if err := everyMemberFailed(1, func(i int) *api.Error { return errs[i] }); api.CodeOf(err) != api.CodeLeaseHeld || !isReported(err) {
		t.Errorf("all failed: %v", err)
	}

	if err := everyMemberFailed(0, func(int) *api.Error { return nil }); err != nil {
		t.Errorf("no members: %v", err)
	}

	if err := firstFailure(nil, boom, api.NewError(api.CodeNoSession, "x", "")); !errors.Is(err, boom) {
		t.Errorf("firstFailure = %v", err)
	}
}

func TestGroupCallError(t *testing.T) {
	t.Parallel()

	err := groupCallError(api.NewError(api.CodeUnknownMethod, "unknown method group.wait", ""))
	if api.CodeOf(err) != api.CodeVersionMismatch || !strings.Contains(err.Error(), "too old") || !strings.Contains(hintOf(err), "eyedbg daemon stop") {
		t.Errorf("err = %v, want VERSION_MISMATCH", err)
	}

	other := api.NewError(api.CodeNoSession, "x", "")
	if got := groupCallError(other); !errors.Is(got, other) {
		t.Errorf("another error changed: %v", got)
	}
}

// TestComposeHelp: every compose command documents what the contract asks
// of its help, and the root help points at it.
func TestComposeHelp(t *testing.T) {
	t.Parallel()

	root := newEyedbgCommand(testInfo, composeDeps{})
	if !strings.Contains(root.Long, "eyedbg help compose") {
		t.Error("the root help doesn't point at 'eyedbg help compose'")
	}

	compose, _, err := root.Find([]string{"compose"})
	if err != nil || compose.Name() != "compose" {
		t.Fatalf("no compose command: %v", err)
	}

	for _, want := range []string{"attach", "wait", "events", "bp", "stop"} {
		if sub, _, err := compose.Find([]string{want}); err != nil || sub.Name() != want {
			t.Errorf("no compose %s: %v", want, err)
		}
	}

	if sub, _, err := compose.Find([]string{"detach"}); err == nil && sub.Name() == "detach" {
		t.Error("compose detach exists: it is compose stop")
	}

	for _, path := range [][]string{{"compose", "events"}, {"compose", "wait"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			t.Fatal(err)
		}

		if !strings.Contains(cmd.Long, envGroup) {
			t.Errorf("%v help doesn't document $%s", path, envGroup)
		}
	}

	events, _, _ := root.Find([]string{"compose", "events"})
	for _, want := range []string{"may repeat", "--newest"} {
		if !strings.Contains(events.Long, want) {
			t.Errorf("compose events help lacks %q", want)
		}
	}
}
