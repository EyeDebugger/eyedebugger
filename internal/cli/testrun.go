// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// testLong is the long help of Test.
const testLong = `Run a project's tests under the debugger, creating a new session. For dotnet, most projects run
through 'dotnet test' (VSTest) in Debug, on --project (a test project file or a directory holding
exactly one; default: the current directory), with FILTER passed as --filter (e.g. Adds,
FullyQualifiedName~Orders, Category=Fast); --framework picks one target framework of a
multi-targeting project (only the first test host is debugged otherwise); --no-build skips the
build. 'dotnet test' starts a test host that waits for a debugger; eyedbg attaches to it, sets
--bp breakpoints and lets the tests run. The session exists (state starting) from the moment the
run starts, so 'eyedbg stop' can end a long build; everything 'dotnet test' prints is the
session's output.

Microsoft.Testing.Platform (MTP) projects and xUnit v3 projects run their tests inside the test
app itself, not a separate host eyedbg could attach to: eyedbg builds the project (as above) and
launches that app directly under the adapter instead, like 'eyedbg start' (no runner, no attach).
eyedbg recognizes one by a reference to xunit.v3, a global.json or DOTNET_TEST_RUNNER selecting
Microsoft.Testing.Platform, an EnableMSTestRunner, EnableNUnitRunner,
TestingPlatformDotnetTestSupport, IsTestingPlatformApplication or UseMicrosoftTestingPlatformRunner
set to true (in the project or a Directory.Build.props/.targets above it), the MSTest.Sdk (unless
UseVSTest is true), or a TUnit reference; an unrecognized MTP project can be marked with
<IsTestingPlatformApplication>true</IsTestingPlatformApplication>. --framework is required if a
launched project targets more than one framework (INVALID_REQUEST names them); --no-build then
requires it already built.

FILTER goes to the launched app's own filter option: --filter for Microsoft.Testing.Platform
(MSTest, NUnit, MSTest.Sdk, and xUnit v3 switched to the MTP runner); -filterVSTest for xUnit v3's
own CLI (native mode, xUnit v3 4.0 and later only — earlier versions have no VSTest-syntax filter
and exit with an "unknown option" error, use '--' instead); --treenode-filter for TUnit.
Everything after '--' is passed to the launched test app verbatim (argv, never a shell, e.g.
'eyedbg test dotnet --project tests -- -method "*Adds"'); a VSTest run refuses '--' arguments
(INVALID_REQUEST) since its tests run in a separate host eyedbg only attaches to.

The launch path's build runs before the session exists (as 'eyedbg start'), so 'eyedbg stop'
can't cancel it — it's bounded by the same 5-minute start timeout. With --bp, test waits (up to
--timeout, on top of building) for the first stop. The session ends when the test run finishes: a
VSTest session ends when 'dotnet test' exits, with its exit code (0: all passed, 1: a test
failed); a launched session ends when the test app itself exits, with its own exit code
(Microsoft.Testing.Platform frameworks: 0 pass, 2 a test failed, 8 no test matched; xUnit v3
native: 0 pass or no test matched, 1 a test failed; see the framework's own output for others),
except where the adapter's exit codes can't be trusted (see --adapter), when it ends without one
and the output is the only way to tell. A
build error is BUILD_FAILED; a VSTest run that ends without a test host (e.g. no test matches the
filter) is NO_TEST_HOST, both with the end of the output. 'eyedbg stop' kills the run; detaching
is refused.

Only dotnet has test runs. For python, run pytest as a module under 'eyedbg start':
'eyedbg start python --opt module=pytest --bp tests/test_x.py:8 -- -x tests/test_x.py'.

Breakpoints, exceptions, the lease policy and recording work as for 'eyedbg start'.` + adapterHelp + dumpHelp

func newTestCommand(info version.Info, g *globals) *cobra.Command {
	var (
		sf                 sessionFlags
		project, framework string
		env                []string
		noBuild            bool
	)

	cmd := &cobra.Command{
		Use:   "test <lang> [filter] [flags] [-- test app args...]",
		Short: "Debug a test run",
		Long:  testLong,
		Example: `  eyedbg test dotnet Adds --bp 'CalculatorTests.cs@"Assert.Equal"'
  eyedbg test dotnet --project tests/Api.Tests --exceptions all
  eyedbg test dotnet 'FullyQualifiedName~Orders' --framework net10.0 --no-build
  eyedbg test dotnet Adds --adapter sharpdbg --bp CalculatorTests.cs:13
  eyedbg test dotnet --project tests/Unit -- -method '*Adds'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filter, testArgs, err := testCommandArgs(args, cmd.ArgsLenAtDash())
			if err != nil {
				return err
			}

			if project == "" {
				project = "."
			}

			params := api.StartParams{
				Lang:       args[0],
				LaunchSpec: api.LaunchSpec{Project: absPath(project), NoBuild: noBuild, Args: testArgs},
				Test:       &api.TestSpec{Framework: framework, Filter: filter},
			}

			if params.Env, err = parseEnv(env); err != nil {
				return err
			}

			if err := sf.params(g, &params); err != nil {
				return err
			}

			return startSession(cmd, info, g, params, sf.timeout)
		},
	}

	f := cmd.Flags()
	f.StringVar(&project, "project", "", "test project file or directory (default: current directory)")
	f.StringVar(&framework, "framework", "", "target framework to test, e.g. net10.0")
	f.StringArrayVar(&env, "env", nil, "environment variable KEY=VALUE for the run (repeatable)")
	f.BoolVar(&noBuild, "no-build", false, "don't build the project first")
	sf.register(cmd)

	return cmd
}

// testCommandArgs splits args (<lang> [filter] [-- test app args...]) into
// the test filter and the launched test app's own arguments; dash is
// cmd.ArgsLenAtDash() (-1 without "--"). At most lang and filter may precede
// "--" (or, without one, follow lang) — a third is an error naming it (put
// test app arguments after "--" instead). Args after "--" are refused for a
// VSTest run at the session layer, not here (the driver decides which run
// this is).
func testCommandArgs(args []string, dash int) (filter string, testArgs []string, err error) {
	if dash < 0 {
		rest := args[1:]

		if len(rest) > 1 {
			return "", nil, fmt.Errorf("unexpected argument %q (put test app arguments after \"--\")", rest[1])
		}

		if len(rest) == 1 {
			filter = rest[0]
		}

		return filter, nil, nil
	}

	if dash > 2 {
		return "", nil, fmt.Errorf("unexpected argument %q (put test app arguments after \"--\")", args[2])
	}

	if dash == 2 {
		filter = args[1]
	}

	return filter, args[dash:], nil
}
