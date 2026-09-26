// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// TestDebugTests debugs a 'dotnet test' run: a breakpoint in the passing
// test stops it, and each run ends with dotnet test's exit code, through
// each adapter.
func TestDebugTests(t *testing.T) {
	forEachAdapter(t, debugTests)
}

func debugTests(t *testing.T, adapter string) {
	t.Helper()

	dir := copyApp(t, "tests")
	src := filepath.Join(dir, "CalculatorTests.cs")
	m := newManager(t)

	sess, err := m.Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir}, Test: &api.TestSpec{Filter: "Adds"},
		Breakpoints: []api.BreakpointSpec{{File: src, Anchor: "Assert.Equal(5, sum);"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", lineOf(t, src, "adds-assert"))

	if got := e2eEval(t, sess, "a + b"); got != "5" {
		t.Errorf("a + b = %s, want 5", got)
	}

	if got := sess.Info().Adapter; got != adapter {
		t.Errorf("test session adapter = %q, want %s", got, adapter)
	}

	snap := e2eResume(t, sess, session.ExecContinue)
	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != 0 {
		t.Fatalf("after continue: %+v, want the run exited 0", snap.Session)
	}

	if out := joinOutput(sess.Output(0, 0).Lines); !strings.Contains(out, "Passed!") {
		t.Errorf("output lacks Passed!:\n%s", out)
	}

	sess, err = m.Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, NoBuild: true}, Test: &api.TestSpec{Filter: "Fails"},
	})
	if err != nil {
		t.Fatal(err)
	}

	snap = sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{})
	if snap.Session.State != api.StateExited || snap.Session.ExitCode == nil || *snap.Session.ExitCode != 1 {
		t.Fatalf("failing run: %+v, want exited 1", snap.Session)
	}
}

// launchedTestCase is a self-hosting test app (Microsoft.Testing.Platform or
// xUnit v3) that S2 launches directly under the adapter instead of attaching
// to a VSTest host.
type launchedTestCase struct {
	name string
	// app is the testdata/apps/dotnet directory to copy.
	app string
	// addsAnchor and failAnchor are the Adds and Fails tests' assert
	// lines, for a FILE@"TEXT" breakpoint and the a+b eval (Adds only).
	addsAnchor string
	// buildProps, non-empty, is written as a Directory.Build.props into
	// the app's copy before building: switches xUnit v3 from its own
	// native CLI to the Microsoft.Testing.Platform one.
	buildProps string
	// failCode is the launched app's own exit code for a run selecting
	// only Fails (D8a: the app's exit code, where the adapter reports it
	// truthfully).
	failCode int
	// failArgs, non-empty, selects Fails through '-- ' arguments (D5)
	// instead of FILTER, exercising the launched app's own argument
	// passthrough; empty uses Test.Filter = "Fails" instead.
	failArgs []string
}

// mtpBuildProps switches a copy of the xunit3 sample to the
// Microsoft.Testing.Platform runner (also exercises Directory.Build.props
// detection, D3).
const mtpBuildProps = `<Project>
  <PropertyGroup>
    <UseMicrosoftTestingPlatformRunner>true</UseMicrosoftTestingPlatformRunner>
  </PropertyGroup>
</Project>
`

// launchedTestCases covers xUnit v3's own CLI, xUnit v3 switched to the MTP
// runner and MSTest's MTP runner (D13): every dialect [pickDialect] chooses
// between.
var launchedTestCases = []launchedTestCase{
	{
		name:       "xunit3 native",
		app:        "xunit3",
		addsAnchor: "Assert.Equal(5, sum);",
		failCode:   1,
		failArgs:   []string{"-method", "*Fails"},
	},
	{
		name:       "xunit3 mtp",
		app:        "xunit3",
		addsAnchor: "Assert.Equal(5, sum);",
		buildProps: mtpBuildProps,
		failCode:   2,
	},
	{
		name:       "mstest",
		app:        "mstest",
		addsAnchor: "Assert.AreEqual(5, sum);",
		failCode:   2,
	},
}

// TestDebugLaunchedTests debugs a launched (Microsoft.Testing.Platform or
// xUnit v3) test app directly under the adapter (S2, ADR 0010's amendment):
// a breakpoint in the passing test stops it, and each run ends with the
// test app's own exit code, through each adapter.
func TestDebugLaunchedTests(t *testing.T) {
	for _, tc := range launchedTestCases {
		t.Run(tc.name, func(t *testing.T) {
			forEachAdapter(t, func(t *testing.T, adapter string) {
				t.Helper()

				debugLaunchedTests(t, adapter, tc)
			})
		})
	}
}

func debugLaunchedTests(t *testing.T, adapter string, tc launchedTestCase) {
	t.Helper()

	dir := copyApp(t, tc.app)

	if tc.buildProps != "" {
		if err := os.WriteFile(filepath.Join(dir, "Directory.Build.props"), []byte(tc.buildProps), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	src := filepath.Join(dir, "CalculatorTests.cs")
	m := newManager(t)

	sess, err := m.Start(t.Context(), agent, api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir}, Test: &api.TestSpec{Filter: "Adds"},
		Breakpoints: []api.BreakpointSpec{{File: src, Anchor: tc.addsAnchor}},
	})
	if err != nil {
		t.Fatal(err)
	}

	expectStop(t, sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{}), "breakpoint", lineOf(t, src, "adds-assert"))

	if got := e2eEval(t, sess, "a + b"); got != "5" {
		t.Errorf("a + b = %s, want 5", got)
	}

	if got := sess.Info().Mode; got != api.ModeTest {
		t.Errorf("launched test session mode = %q, want %s", got, api.ModeTest)
	}

	if got := sess.Info().Adapter; got != adapter {
		t.Errorf("launched test session adapter = %q, want %s", got, adapter)
	}

	snap := e2eResume(t, sess, session.ExecContinue)
	if snap.Session.State != api.StateExited {
		t.Fatalf("after continue: %+v, want the launched app exited", snap.Session)
	}

	expectExitCode(t, snap.Session, 0)

	failParams := api.StartParams{
		Lang: "dotnet", Adapter: adapter, LaunchSpec: api.LaunchSpec{Project: dir, NoBuild: true}, Test: &api.TestSpec{},
	}

	if len(tc.failArgs) > 0 {
		failParams.Args = tc.failArgs
	} else {
		failParams.Test.Filter = "Fails"
	}

	sess, err = m.Start(t.Context(), agent, failParams)
	if err != nil {
		t.Fatal(err)
	}

	snap = sess.Wait(t.Context(), 0, e2eWait, api.DumpSpec{})
	if snap.Session.State != api.StateExited {
		t.Fatalf("failing run: %+v, want exited\noutput:\n%s", snap.Session, joinOutput(sess.Output(0, 0).Lines))
	}

	expectExitCode(t, snap.Session, tc.failCode)

	// Where the exit code can't be trusted, the output naming the failing
	// test is the only way to tell (D8a; the "read the test output" end
	// reason promise).
	if dotnet.ExitCodeUnknown(snap.Session.Adapter, runtime.GOOS) != "" {
		if out := joinOutput(sess.Output(0, 0).Lines); !strings.Contains(out, "Fails") {
			t.Errorf("output lacks the failing test's name (%q):\n%s", "Fails", out)
		}
	}
}
