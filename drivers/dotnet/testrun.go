// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// Microsoft.Testing.Platform, the test runner 'dotnet test' can use instead
// of VSTest.
const (
	mtpRunner     = "Microsoft.Testing.Platform"
	envTestRunner = "DOTNET_TEST_RUNNER"
)

// hostPIDPrefix starts the line vstest.console prints when VSTEST_HOST_DEBUG
// is set: "Process Id: 4242, Name: dotnet" (not localized).
const hostPIDPrefix = "Process Id: "

// TestCommand implements session.Tester: 'dotnet test' in Debug with
// VSTest's host debugging on, so the test host prints its process id and
// waits for a debugger (VSTEST_DEBUG_NOBP: without breaking into it).
func (*Driver) TestCommand(_ context.Context, spec session.TestSpec) (session.TestCommand, error) {
	host, err := FindHost()
	if err != nil {
		return session.TestCommand{}, err
	}

	project, err := findProject(spec.Project)
	if err != nil {
		return session.TestCommand{}, err
	}

	if usesMTP(filepath.Dir(project), spec.Env) {
		return session.TestCommand{}, api.NewError(api.CodeNoTestHost,
			"eyedbg test supports VSTest runs only, and "+filepath.Base(project)+" runs on "+mtpRunner,
			"debug the test app directly: eyedbg start dotnet --project "+project+" -- <test options>")
	}

	return testCommand(host, project, spec), nil
}

// testCommand builds the run of project's tests with host.
func testCommand(host, project string, spec session.TestSpec) session.TestCommand {
	args := testArgs(project, spec)
	shown := append([]string{lang, "test", filepath.Base(project)}, args[len(testBaseArgs(project)):]...)

	return session.TestCommand{
		Path:    host,
		Args:    args,
		Env:     []string{"VSTEST_HOST_DEBUG=1", "VSTEST_DEBUG_NOBP=1", "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1"},
		Dir:     filepath.Dir(project),
		Program: strings.Join(shown, " "),
		HostPID: hostPID,
		Failure: func(code int, output string) error { return testFailure(project, code, output) },
	}
}

// testBaseArgs are the arguments every run gets.
func testBaseArgs(project string) []string {
	return []string{"test", project, "-c", "Debug", "--nologo", "--tl:off", "--disable-build-servers"}
}

// testArgs adds spec's choices to the base arguments. Each value is its own
// argument: nothing goes through a shell.
func testArgs(project string, spec session.TestSpec) []string {
	args := testBaseArgs(project)

	if spec.Filter != "" {
		args = append(args, "--filter", spec.Filter)
	}

	if spec.Framework != "" {
		args = append(args, "--framework", spec.Framework)
	}

	if spec.NoBuild {
		args = append(args, "--no-build")
	}

	return args
}

// hostPID finds the test host's process id in a line of output.
func hostPID(line string) (int, bool) {
	_, rest, ok := strings.Cut(line, hostPIDPrefix)
	if !ok {
		return 0, false
	}

	digits, rest, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasPrefix(rest, " Name: ") {
		return 0, false
	}

	pid, err := strconv.Atoi(digits)
	if err != nil || pid < 1 {
		return 0, false
	}

	return pid, true
}

// usesMTP reports whether 'dotnet test' in dir runs Microsoft.Testing.Platform:
// the nearest global.json says so, or DOTNET_TEST_RUNNER (in env, else the
// daemon's environment) names it.
func usesMTP(dir string, env map[string]string) bool {
	runner, ok := env[envTestRunner]
	if !ok {
		runner = os.Getenv(envTestRunner)
	}

	if strings.EqualFold(runner, mtpRunner) {
		return true
	}

	for {
		data, err := os.ReadFile(filepath.Join(dir, "global.json"))
		if err == nil {
			var cfg struct {
				Test struct {
					Runner string `json:"runner"`
				} `json:"test"`
			}

			// The SDK reads global.json leniently; an unreadable one runs
			// VSTest as far as this check can tell.
			_ = json.Unmarshal(data, &cfg)

			return strings.EqualFold(cfg.Test.Runner, mtpRunner)
		}

		if !errors.Is(err, fs.ErrNotExist) {
			return false
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}

		dir = parent
	}
}

// testFailure is the error for a 'dotnet test' that exited before a test
// host waited for the debugger.
func testFailure(project string, code int, output string) error {
	// An MSBuild error line, e.g. "Foo.cs(3,1): error CS1002: ; expected".
	compileError := regexp.MustCompile(`: error [A-Z]+\d+:`)

	if compileError.MatchString(output) || strings.Contains(output, "Build FAILED") {
		return api.NewError(api.CodeBuildFailed, "dotnet test "+project+" failed to build:\n"+tail(output, buildOutputLines),
			"fix the build errors above, then run the tests again")
	}

	return api.NewError(api.CodeNoTestHost,
		fmt.Sprintf("dotnet test exited with code %d before a test host waited for the debugger:\n%s", code, tail(output, buildOutputLines)),
		"is it a VSTest project (Microsoft.NET.Test.Sdk) with tests matching the filter? "+
			"For a Microsoft.Testing.Platform project use 'eyedbg start dotnet --project <tests>'")
}
