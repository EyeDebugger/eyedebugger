// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// testLong is the long help of Test.
const testLong = `Run a project's tests under the debugger, creating a new session. For dotnet: 'dotnet test' (VSTest)
in Debug, on --project (a test project file or a directory holding exactly one; default: the
current directory), with FILTER passed as --filter (e.g. Adds, FullyQualifiedName~Orders,
Category=Fast); --framework picks one target framework of a multi-targeting project (only the
first test host is debugged otherwise); --no-build skips the build.

'dotnet test' starts a test host that waits for a debugger; eyedbg attaches to it, sets --bp
breakpoints and lets the tests run. The session exists (state starting) from the moment the run
starts, so 'eyedbg stop' can end a long build; everything 'dotnet test' prints is the session's
output. With --bp, test waits (up to --timeout, on top of building) for the first stop. The
session ends when 'dotnet test' exits, with its exit code (0: all passed, 1: a test failed).
'eyedbg stop' kills the run; detaching is refused.

Microsoft.Testing.Platform projects (global.json "test": {"runner": ...}) and xUnit v3 projects
(which run their tests outside the test host) are refused with NO_TEST_HOST (exit 3): debug the
test app with 'eyedbg start' instead, e.g. 'eyedbg start dotnet --project tests -- -method
"*Adds"'. A build error is
BUILD_FAILED, a run that ends without a test host (e.g. no test matches the filter) NO_TEST_HOST,
both with the end of the output.

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
		Use:   "test <lang> [filter]",
		Short: "Debug a test run",
		Long:  testLong,
		Example: `  eyedbg test dotnet Adds --bp 'CalculatorTests.cs@"Assert.Equal"'
  eyedbg test dotnet --project tests/Api.Tests --exceptions all
  eyedbg test dotnet 'FullyQualifiedName~Orders' --framework net10.0 --no-build
  eyedbg test dotnet Adds --adapter sharpdbg --bp CalculatorTests.cs:13`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if project == "" {
				project = "."
			}

			params := api.StartParams{
				Lang:       args[0],
				LaunchSpec: api.LaunchSpec{Project: absPath(project), NoBuild: noBuild},
				Test:       &api.TestSpec{Framework: framework},
			}

			if len(args) > 1 {
				params.Test.Filter = args[1]
			}

			var err error
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
