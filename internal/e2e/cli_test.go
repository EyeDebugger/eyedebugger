// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Exit code classes (internal/cli's are unexported, and this package only
// ever talks to eyedbg as a subprocess, never by importing internal/cli).
const (
	exitOK          = 0
	exitError       = 1
	exitState       = 2
	exitEnvironment = 3
	exitAdapter     = 4
)

// procTimeout bounds every eyedbg invocation from this side, above and
// beyond whatever --timeout the command itself was given: a safety net,
// not a retry.
const procTimeout = 90 * time.Second

// startTimeout is passed to commands that wait for the program (start,
// run-until, continue): generous enough for a real adapter's own start-up.
const startTimeout = "60s"

// harness runs eyedbg against one throwaway daemon: its own runtime
// directory and config directory, stopped and checked in t.Cleanup.
type harness struct {
	t          *testing.T
	eyedbg     string
	runtimeDir string
	configDir  string
	// homeDir is $EYEDBG_HOME: dumps land in its dumps/, never in the
	// developer's ~/.eyedbg.
	homeDir string
	// dataDir is where adapters are found: $EYEDBG_DATA_DIR, else the
	// default resolved from this process's environment (~/.eyedbg/tools),
	// kept although EYEDBG_HOME moves.
	dataDir string
}

// newHarness builds eyedbg/eyedbgd (once for the package, binariesFor) and
// prepares a fresh, private runtime and config directory for one daemon.
// EYEDBG_RUNTIME_DIR uses os.MkdirTemp rather than t.TempDir: the latter
// embeds the (sub)test's full name, which can push the daemon's socket
// path past the AF_UNIX sun_path budget (internal/daemon/paths.go).
func newHarness(t *testing.T) *harness {
	t.Helper()

	eyedbg, _ := binariesFor(t)

	runtimeDir, err := os.MkdirTemp("", "e2e") //nolint:usetesting // t.TempDir embeds the full (sub)test name, which can push the daemon's socket path past the AF_UNIX sun_path budget (internal/daemon/paths.go).
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })

	dataDir, err := adapters.DataDir()
	if err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, eyedbg: eyedbg, runtimeDir: runtimeDir, configDir: t.TempDir(), homeDir: t.TempDir(), dataDir: dataDir}

	t.Cleanup(func() {
		h.run("daemon", "stop", "--force")

		var st struct {
			Running bool `json:"running"`
		}

		h.run("daemon", "status", "--json").decode(&st)

		if st.Running {
			t.Error("daemon still running after 'daemon stop --force'")
		}
	})

	return h
}

// env returns os.Environ() with the given keys overridden ("": removed):
// every eyedbg call gets its own runtime, config and home directory (the
// adapters' data directory stays where it was) and a clean
// client/session/autostart state, but otherwise inherits this process's
// environment unchanged, so a developer's or CI's EYEDBG_DATA_DIR,
// EYEDBG_NETCOREDBG or EYEDBG_PYTHON (real-adapter discovery) still reach
// the autostarted eyedbgd (internal/daemon/start.go: it inherits eyedbg's
// own env).
func (h *harness) env(overrides map[string]string) []string {
	all := map[string]string{
		"EYEDBG_RUNTIME_DIR":  h.runtimeDir,
		"EYEDBG_CONFIG_DIR":   h.configDir,
		"EYEDBG_NO_AUTOSTART": "",
		"EYEDBG_CLIENT":       "",
		"EYEDBG_SESSION":      "",
		"EYEDBG_DAEMON_PATH":  "",
		// The .NET helper e2e tests the default lookup (next to eyedbg).
		"EYEDBG_DOTNET_HELPER": "",
		// The dotnet case tests the default adapter (sharpdbg's passes
		// --adapter).
		"EYEDBG_DOTNET_ADAPTER": "",
		adapters.EnvHome:        h.homeDir,
		adapters.EnvDataDir:     h.dataDir,
	}

	maps.Copy(all, overrides)

	out := make([]string, 0, len(os.Environ())+len(all))
	set := make(map[string]bool, len(all))

	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")

		if nv, ok := all[k]; ok {
			set[k] = true

			if nv != "" {
				out = append(out, k+"="+nv)
			}

			continue
		}

		out = append(out, k+"="+v)
	}

	for k, v := range all {
		if !set[k] && v != "" {
			out = append(out, k+"="+v)
		}
	}

	return out
}

// result is one eyedbg invocation's outcome.
type result struct {
	t      *testing.T
	stdout []byte
	stderr []byte
	code   int
}

// runEnv runs eyedbg with the base environment plus overrides.
func (h *harness) runEnv(overrides map[string]string, args ...string) result {
	h.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), procTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, h.eyedbg, args...) //nolint:gosec // The path is what binariesFor just built; args are test-controlled.
	cmd.Env = h.env(overrides)

	var out, errOut bytes.Buffer

	cmd.Stdout, cmd.Stderr = &out, &errOut

	code := 0

	var exitErr *exec.ExitError

	if err := cmd.Run(); errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	} else if err != nil {
		h.t.Fatalf("run eyedbg %v: %v\nstderr: %s", args, err, errOut.String())
	}

	return result{t: h.t, stdout: out.Bytes(), stderr: errOut.Bytes(), code: code}
}

// run runs eyedbg with just the base environment.
func (h *harness) run(args ...string) result { return h.runEnv(nil, args...) }

// wantCode fails the test if the exit code isn't want.
func (r result) wantCode(want int) result {
	r.t.Helper()

	if r.code != want {
		r.t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", r.code, want, r.stdout, r.stderr)
	}

	return r
}

// wantStderr fails the test unless stderr contains sub.
func (r result) wantStderr(sub string) {
	r.t.Helper()

	if !strings.Contains(string(r.stderr), sub) {
		r.t.Fatalf("stderr %q, want it to contain %q", r.stderr, sub)
	}
}

// wantStdout fails the test unless stdout contains sub.
func (r result) wantStdout(sub string) {
	r.t.Helper()

	if !strings.Contains(string(r.stdout), sub) {
		r.t.Fatalf("stdout %q, want it to contain %q", r.stdout, sub)
	}
}

// decode unmarshals stdout into v (a --json result), failing the test on
// error.
func (r result) decode(v any) {
	r.t.Helper()

	if err := json.Unmarshal(r.stdout, v); err != nil {
		r.t.Fatalf("decode %T: %v\nstdout: %s", v, err, r.stdout)
	}
}

// snapshotEnvelope is 'eyedbg --json status|start|run-until|continue's
// output: api.Snapshot's own fields, promoted, plus "schema"
// (internal/cli/render.go's writeSnapshot).
type snapshotEnvelope struct {
	Schema       int `json:"schema"`
	api.Snapshot     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
}

// evalEnvelope is 'eyedbg --json eval's output.
type evalEnvelope struct {
	Schema         int `json:"schema"`
	api.EvalResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
}

// eventsEnvelope is 'eyedbg --json events's output.
type eventsEnvelope struct {
	Schema           int `json:"schema"`
	api.EventsResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
}

// loc is FILE:LINE, the location syntax 'eyedbg start --bp',
// 'eyedbg run-until' and 'eyedbg bp add' all share.
func loc(file string, line int) string { return fmt.Sprintf("%s:%d", file, line) }

// TestCLI drives eyedbg and eyedbgd as real binaries through a 13-step
// script (docs/CONVENTIONS.md § Testing), once per language: the always-on
// fake case, and python/dotnet behind EYEDBG_E2E=1.
func TestCLI(t *testing.T) {
	t.Parallel()

	for _, lc := range langCases(t) {
		t.Run(lc.name, func(t *testing.T) {
			lc.require(t)
			t.Parallel()
			runScript(t, lc)
		})
	}
}

// runScript is the 13-step script, one helper per step (or step group) to
// keep each piece's own branching simple.
func runScript(t *testing.T, lc langCase) {
	t.Helper()

	h := newHarness(t)

	if lc.lang == "fakelang" {
		fakeManifestFiles(t, h.configDir)
	}

	stepNoDaemon(t, h, lc)
	stepStart(t, h, lc)
	stepStatus(t, h, lc)
	stepRunUntil(t, h, lc)
	stepVars(t, h)
	stepEval(t, h, lc)
	stepSideEffect(t, h, lc)
	stepLease(t, h)
	stepAttach(t, h, lc)
	stepLogpoint(t, h, lc)
	stepEvents(t, h)
	stepStop(t, h)
	stepDaemonStop(t, h)
}

// stepNoDaemon is step 1: with no daemon running and autostart disabled, a
// command needing an existing session gets NO_SESSION (exit 2) — there
// cannot be a session without a daemon, so internal/cli/session.go's call()
// deliberately folds "no daemon" into "no session" — while a command that
// creates one gets DAEMON_NOT_RUNNING (exit 3) instead, since there it is
// the daemon itself, not a session, that is missing (docs/DESIGN.md §5).
func stepNoDaemon(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	noAutostart := map[string]string{"EYEDBG_NO_AUTOSTART": "1"}

	h.runEnv(noAutostart, "status").
		wantCode(exitState).wantStderr("[" + string(api.CodeNoSession) + "]")

	var errEnv struct {
		Schema int `json:"schema"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}

	h.runEnv(noAutostart, "--json", "status").wantCode(exitState).decode(&errEnv)

	if errEnv.Schema != 1 || errEnv.Error.Code != string(api.CodeNoSession) {
		t.Fatalf("--json status with no daemon = %+v, want schema 1 and %s", errEnv, api.CodeNoSession)
	}

	startArgs := append([]string{"start", lc.lang}, lc.startArgs(t)...)
	startArgs = append(startArgs, "--timeout", startTimeout)

	h.runEnv(noAutostart, startArgs...).
		wantCode(exitEnvironment).wantStderr("[" + string(api.CodeDaemonNotRunning) + "]")
}

// stepStart is step 2: start, stopped at the anchor; proves autostart.
func stepStart(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	startArgs := append([]string{"start", lc.lang}, lc.startArgs(t)...)
	startArgs = append(startArgs, "--bp", loc(lc.file, lc.anchor), "--lease-policy", "handoff", "--timeout", startTimeout, "--json")

	var snap snapshotEnvelope

	h.run(startArgs...).wantCode(exitOK).decode(&snap)

	if snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Line != lc.anchor {
		t.Fatalf("start: state=%s frame=%+v, want stopped at line %d", snap.Session.State, snap.Frame, lc.anchor)
	}

	var daemonSt struct {
		Running bool `json:"running"`
	}

	h.run("daemon", "status", "--json").wantCode(exitOK).decode(&daemonSt)

	if !daemonSt.Running {
		t.Fatal("daemon status --json after start: running = false, want true (autostart)")
	}
}

// stepStatus is step 3: status agrees.
func stepStatus(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	var snap snapshotEnvelope

	h.run("--json", "status").wantCode(exitOK).decode(&snap)

	if snap.Schema != 1 || snap.Session.State != api.StateStopped || snap.Frame == nil || snap.Frame.Line != lc.anchor {
		t.Fatalf("status: %+v, want schema 1, stopped at line %d", snap, lc.anchor)
	}
}

// stepRunUntil is step 4: run until the target.
func stepRunUntil(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	var snap snapshotEnvelope

	h.run("run-until", loc(lc.file, lc.target), "--timeout", startTimeout, "--json").wantCode(exitOK).decode(&snap)

	if snap.Reached == nil || !*snap.Reached || snap.Frame == nil || snap.Frame.Line != lc.target {
		t.Fatalf("run-until: reached=%v frame=%+v, want reached at line %d", snap.Reached, snap.Frame, lc.target)
	}
}

// stepVars is step 5: changed locals.
func stepVars(t *testing.T, h *harness) {
	t.Helper()
	h.run("vars", "--changed").wantCode(exitOK)
}

// stepEval is step 6: a side-effect-free eval.
func stepEval(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	var ev evalEnvelope

	h.run("--json", "eval", lc.okExpr).wantCode(exitOK).decode(&ev)

	if ev.Value != lc.okValue {
		t.Fatalf("eval %q = %q, want %q", lc.okExpr, ev.Value, lc.okValue)
	}
}

// stepSideEffect is step 7: an eval with a visible side effect is refused.
func stepSideEffect(t *testing.T, h *harness, lc langCase) {
	t.Helper()
	h.run("eval", lc.sideEffectExpr).wantCode(exitError).wantStderr("[" + string(api.CodeSideEffects) + "]")
}

// stepLease is step 8: another client can't take over the control lease
// (handoff policy). A round trip through 'eyedbg lease grant' then puts a
// "lease" event in the log for step 11: a refused take, unlike a grant,
// logs nothing (internal/session/lease.go), so the refusal alone never
// would.
func stepLease(t *testing.T, h *harness) {
	t.Helper()

	h.run("--as", "human:e2e", "continue").wantCode(exitState).wantStderr("[" + string(api.CodeLeaseHeld) + "]")

	h.run("lease", "grant", "human:e2e").wantCode(exitOK)
	h.run("--as", "human:e2e", "lease", "grant", "agent").wantCode(exitOK)
}

// stepAttach is step 9: attach is unsupported here (fake, python have no
// attach template); M5's e2e covers dotnet's real attach.
func stepAttach(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	if lc.attachSupported {
		return
	}

	h.run("attach", lc.lang, "--pid", strconv.Itoa(os.Getpid())).
		wantCode(exitAdapter).wantStderr("[" + string(api.CodeUnsupported) + "]")
}

// stepLogpoint is step 10: a logpoint at the anchor, then run to exit; its
// messages are in the output.
func stepLogpoint(t *testing.T, h *harness, lc langCase) {
	t.Helper()

	h.run("bp", "add", loc(lc.file, lc.anchor), "--log", "v={"+lc.logExpr+"}").wantCode(exitOK)

	var snap snapshotEnvelope

	h.run("continue", "--timeout", startTimeout, "--json").wantCode(exitOK).decode(&snap)

	if snap.Session.State != api.StateExited {
		t.Fatalf("continue: state=%s, want exited", snap.Session.State)
	}

	h.run("output").wantCode(exitOK).wantStdout("v=")
}

// stepEvents is step 11: the event log has a lease, an exec and a
// breakpoint event.
func stepEvents(t *testing.T, h *harness) {
	t.Helper()

	var events eventsEnvelope

	h.run("--json", "events", "--since", "0").wantCode(exitOK).decode(&events)

	kinds := make(map[api.EventKind]bool, len(events.Events))
	for i := range events.Events {
		kinds[events.Events[i].Kind] = true
	}

	for _, want := range []api.EventKind{api.EventLease, api.EventExec, api.EventBreakpoint} {
		if !kinds[want] {
			t.Errorf("events --since 0: no %q event among %d", want, len(events.Events))
		}
	}
}

// stepStop is step 12: stop, then stop again.
func stepStop(t *testing.T, h *harness) {
	t.Helper()

	h.run("stop").wantCode(exitOK)
	h.run("stop").wantCode(exitState).wantStderr("[" + string(api.CodeNoSession) + "]")
}

// stepDaemonStop is step 13: stop the daemon.
func stepDaemonStop(t *testing.T, h *harness) {
	t.Helper()
	h.run("daemon", "stop").wantCode(exitOK).wantStdout("eyedbgd stopped")
}
