// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

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

// msbuild properties the launch path evaluates, beyond TargetPath: the
// framework actually built, and what picks the launched app's dialect.
const (
	propTargetPath           = "TargetPath"
	propTargetFrameworks     = "TargetFrameworks"
	propIsTestingPlatformApp = "IsTestingPlatformApplication"
	propUseMTPRunner         = "UseMicrosoftTestingPlatformRunner"
)

// testDialect is a launched test app's own command-line options: which one
// carries FILTER (extra "-- " args go verbatim after it, for every dialect).
type testDialect int

// Dialects [pickDialect] chooses between.
const (
	// dialectMTP is Microsoft.Testing.Platform's own CLI: MSTest, NUnit,
	// MSTest.Sdk, and xUnit v3 switched to the MTP runner.
	dialectMTP testDialect = iota
	// dialectXunitNative is xUnit v3's own CLI (not switched to MTP).
	dialectXunitNative
	dialectTUnit
)

// detectedKind is which marker [detectLaunch] found, needed to pick the
// dialect afterwards (xUnit v3 and TUnit have their own; everything else
// Microsoft.Testing.Platform detects runs on the MTP CLI).
type detectedKind int

const (
	detectOther detectedKind = iota
	detectXunitV3
	detectTUnit
)

// Regular expressions [detectLaunch] and [usesXunitV3] scan project files
// with (comments stripped, [readStripped]).
var (
	xmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

	// xunitV3Ref matches a package that makes a project an xUnit v3 test
	// app: xunit.v3, xunit.v3.core, xunit.v3[.core].mtp-vN.
	xunitV3Ref = regexp.MustCompile(`(?i)<PackageReference\s[^>]*Include\s*=\s*"xunit\.v3(\.core)?(\.mtp-v\d+)?"`)

	// tunitRef matches a PackageReference to TUnit or TUnit.Engine (not
	// TUnit.Assertions alone, which doesn't make a project self-hosting).
	tunitRef = regexp.MustCompile(`(?i)<PackageReference\s[^>]*Include\s*=\s*"TUnit(\.Engine)?"`)

	// testMarkerRe matches one of the elements (or attributes) that mark a
	// project as a Microsoft.Testing.Platform app, set to true.
	testMarkerRe = regexp.MustCompile(`(?i)<(EnableMSTestRunner|EnableNUnitRunner|TestingPlatformDotnetTestSupport|` +
		`IsTestingPlatformApplication|UseMicrosoftTestingPlatformRunner)\b[^>]*>\s*true\s*<`)

	// mstestSdkRe matches the MSTest.Sdk project SDK, as an Sdk attribute
	// (with or without a version) or an Sdk element.
	mstestSdkRe = regexp.MustCompile(`(?i)(Sdk\s*=\s*"MSTest\.Sdk(/[^"]*)?"|<Sdk\b[^>]*\bName\s*=\s*"MSTest\.Sdk")`)

	// useVSTestRe matches <UseVSTest>true, which switches MSTest.Sdk back to
	// VSTest.
	useVSTestRe = regexp.MustCompile(`(?i)<UseVSTest\b[^>]*>\s*true\s*<`)
)

// TestCommand implements session.Tester. Undetected (VSTest) projects run
// through 'dotnet test' in Debug with VSTest's host debugging on, so the
// test host prints its process id and waits for a debugger (VSTEST_DEBUG_NOBP:
// without breaking into it); a Microsoft.Testing.Platform or xUnit v3
// project ([detectLaunch]) is built (or, --no-build, evaluated) and its
// self-hosting test app is launched under the adapter directly (D1, ADR
// 0010's amendment): no runner, no attach. The adapter is checked first: a
// missing one fails before any build.
func (d *Driver) TestCommand(ctx context.Context, spec session.TestSpec) (session.TestCommand, error) {
	launch, err := d.adapterLaunch(ctx)
	if err != nil {
		return session.TestCommand{}, err
	}

	host, err := FindHost()
	if err != nil {
		return session.TestCommand{}, err
	}

	project, err := findProject(spec.Project)
	if err != nil {
		return session.TestCommand{}, err
	}

	reason, kind := detectLaunch(project, spec.Env)
	if reason == "" {
		return testCommand(host, project, spec), nil
	}

	return d.testLaunchCommand(ctx, launch, host, project, reason, kind, spec)
}

// testLaunchCommand builds (or, spec.NoBuild, evaluates) project and returns
// the launch that runs its self-hosting test app under the adapter.
func (d *Driver) testLaunchCommand(
	ctx context.Context, launch session.Launch, host, project, reason string, kind detectedKind, spec session.TestSpec,
) (session.TestCommand, error) {
	props, err := evaluateTestBuild(ctx, host, project, spec.Framework, spec.NoBuild)
	if err != nil {
		return session.TestCommand{}, err
	}

	target, err := decideLaunchTarget(project, reason, props, spec.NoBuild)
	if err != nil {
		return session.TestCommand{}, err
	}

	return buildTestLaunch(launch, host, project, target, pickDialect(kind, props), spec), nil
}

// buildTestLaunch is the pure part of testLaunchCommand, once target (the
// built self-hosting test app) and its dialect are known: launch.Arguments'
// argv is the dialect's fixed arguments (FILTER's own option among them),
// then spec's own "-- " arguments verbatim (D5); cwd is project's directory;
// env is the daemon's opt-outs overlaid by spec's own (D7). Program names
// the fixed and filter arguments only — never the "-- " ones (D14).
func buildTestLaunch(launch session.Launch, host, project, target string, dialect testDialect, spec session.TestSpec) session.TestCommand {
	fixed := testDialectArgs(dialect, spec.Filter)

	launch.Arguments = launchArguments(host, target, filepath.Dir(project), session.LaunchSpec{
		Args: append(slices.Clone(fixed), spec.Args...),
		Env:  testLaunchEnv(spec.Env),
	})

	return session.TestCommand{Program: launchTestProgram(target, fixed), Launch: &launch}
}

// shellArg is s as a hint shows it for pasting into a shell: single-quoted
// unless it's only letters, digits and punctuation no shell interprets.
func shellArg(s string) string {
	plain := s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(`-_./:\@+=,`, r)
	})
	if plain {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// testCommand builds the VSTest run of project's tests with host.
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

// testBaseArgs are the arguments every VSTest run gets.
func testBaseArgs(project string) []string {
	return []string{"test", project, "-c", dotnetConfig, "--nologo", "--tl:off", "--disable-build-servers"}
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

// upwardFiles is project, then Directory.Build.props and .targets in every
// directory from project's own up to the filesystem root.
func upwardFiles(project string) []string {
	files := []string{project}

	for dir := filepath.Dir(project); ; dir = filepath.Dir(dir) {
		files = append(files, filepath.Join(dir, "Directory.Build.props"), filepath.Join(dir, "Directory.Build.targets"))

		if filepath.Dir(dir) == dir {
			break
		}
	}

	return files
}

// readStripped reads path with its XML comments removed, so a commented-out
// marker isn't seen; ok is false for a missing or unreadable file.
func readStripped(path string) (data []byte, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	return xmlCommentRe.ReplaceAll(data, nil), true
}

// usesXunitV3 reports whether project references xUnit v3, itself or
// through a Directory.Build.props or .targets above it. Its VSTest adapter
// runs the tests in a child of the test host, which the session never
// attaches to (so a VSTest run refuses it; the launch path debugs it
// directly instead).
func usesXunitV3(project string) bool {
	return matchesUpward(project, xunitV3Ref)
}

// usesTUnit reports whether project references TUnit or TUnit.Engine,
// itself or through a Directory.Build.props or .targets above it.
func usesTUnit(project string) bool {
	return matchesUpward(project, tunitRef)
}

// usesVSTest reports whether project, or a Directory.Build.props or
// .targets above it, sets <UseVSTest>true, which switches MSTest.Sdk back
// to VSTest. Checked across every upward file (not only the one that sets
// the MSTest.Sdk marker): the usual place for a repo-wide property is a
// shared Directory.Build.props, not the project file itself.
func usesVSTest(project string) bool {
	return matchesUpward(project, useVSTestRe)
}

// matchesUpward reports whether re matches project or a Directory.Build.props
// or .targets above it.
func matchesUpward(project string, re *regexp.Regexp) bool {
	for _, f := range upwardFiles(project) {
		if data, ok := readStripped(f); ok && re.Match(data) {
			return true
		}
	}

	return false
}

// testKind is which dialect project needs ([pickDialect]), from what it
// references — independent of which marker below made detectLaunch launch
// it directly (D3/D4/D6): a project can reference xUnit v3 or TUnit while a
// global.json, DOTNET_TEST_RUNNER or an explicit MTP marker is what
// triggers the launch.
func testKind(project string) detectedKind {
	switch {
	case usesXunitV3(project):
		return detectXunitV3
	case usesTUnit(project):
		return detectTUnit
	default:
		return detectOther
	}
}

// detectLaunch decides, before any build, whether project's tests should be
// launched directly under the adapter (a self-hosting Microsoft.Testing.Platform
// or xUnit v3 app) instead of run through 'dotnet test': reason is "" for a
// VSTest project (the usual path), else why, for NO_TEST_HOST's message if
// the build doesn't confirm it. kind says which dialect it needs
// ([pickDialect]); the build afterwards confirms IsTestingPlatformApplication
// and, for xUnit v3, whether it was switched to the MTP runner.
func detectLaunch(project string, env map[string]string) (reason string, kind detectedKind) {
	kind = testKind(project)

	if usesMTP(filepath.Dir(project), env) {
		return "global.json or " + envTestRunner + " selects " + mtpRunner, kind
	}

	if usesXunitV3(project) {
		return "it references xunit.v3", kind
	}

	vstest := usesVSTest(project)

	for _, f := range upwardFiles(project) {
		data, ok := readStripped(f)
		if !ok {
			continue
		}

		if m := testMarkerRe.FindSubmatch(data); m != nil {
			return "it sets " + string(m[1]), kind
		}

		if mstestSdkRe.Match(data) && !vstest {
			return "it uses MSTest.Sdk", kind
		}

		if tunitRef.Match(data) {
			return "it references TUnit", kind
		}
	}

	return "", detectOther
}

// evaluateTestBuild builds project for framework f (""; the project's
// default) in Debug, or, noBuild, evaluates its properties without
// building, and returns TargetPath, TargetFrameworks,
// IsTestingPlatformApplication and UseMicrosoftTestingPlatformRunner.
func evaluateTestBuild(ctx context.Context, host, project, f string, noBuild bool) (map[string]string, error) {
	var args []string

	if noBuild {
		args = []string{"msbuild", project, msbuildNoLogo, "-p:Configuration=" + dotnetConfig}
		if f != "" {
			args = append(args, "-p:TargetFramework="+f)
		}
	} else {
		args = []string{"build", project, "-c", dotnetConfig, msbuildNoLogo}
		if f != "" {
			args = append(args, "-f", f)
		}

		args = append(args, "-getTargetResult:Build")
	}

	for _, p := range []string{propTargetPath, propTargetFrameworks, propIsTestingPlatformApp, propUseMTPRunner} {
		args = append(args, "-getProperty:"+p)
	}

	return buildQuery(ctx, host, project, args, !noBuild)
}

// buildQuery runs host with args — a 'dotnet build' or 'dotnet msbuild'
// invocation ending in one or more -getProperty: entries — and returns the
// evaluated properties; checkResult also requires -getTargetResult:Build's
// Result to be Success. require lists properties the query also treats as
// failed if empty (a plain build's TargetPath must be; the test launch
// path's own multi-targeting check needs to see an empty one instead, so it
// passes none). Shared by build (Prepare's exact query) and evaluateTestBuild.
func buildQuery(ctx context.Context, host, project string, args []string, checkResult bool, require ...string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, host, args...)
	cmd.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1")

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()

	props, ok := parseBuildProperties(stdout.Bytes(), checkResult)

	for _, p := range require {
		if props[p] == "" {
			ok = false
		}
	}

	if runErr != nil || !ok {
		return nil, api.NewError(api.CodeBuildFailed, "dotnet "+args[0]+" "+project+" failed:\n"+
			tail(withoutResultJSON(stdout.String())+stderr.String(), buildOutputLines), "fix the build errors above, then start again")
	}

	return props, nil
}

// parseBuildProperties reads -getProperty's JSON; checkResult also requires
// -getTargetResult:Build's Result to be Success ('dotnet msbuild
// -getProperty' alone has no TargetResults to check).
func parseBuildProperties(out []byte, checkResult bool) (map[string]string, bool) {
	var result struct {
		Properties    map[string]string `json:"Properties"` //nolint:tagliatelle // MSBuild's JSON shape.
		TargetResults struct {
			Build struct {
				Result string `json:"Result"` //nolint:tagliatelle // MSBuild's JSON shape.
			} `json:"Build"` //nolint:tagliatelle // MSBuild's JSON shape.
		} `json:"TargetResults"` //nolint:tagliatelle // MSBuild's JSON shape.
	}

	if err := json.Unmarshal(out, &result); err != nil {
		return nil, false
	}

	if checkResult && result.TargetResults.Build.Result != "Success" {
		return nil, false
	}

	return result.Properties, true
}

// decideLaunchTarget turns project's evaluated properties into the program
// to launch, or the error to report: multi-targeting needs --framework
// (D10), --no-build needs it already built (D11), and it must self-host
// (IsTestingPlatformApplication), or reason was a false positive.
func decideLaunchTarget(project, reason string, props map[string]string, noBuild bool) (string, error) {
	target := props[propTargetPath]

	if target == "" {
		if fws := props[propTargetFrameworks]; fws != "" {
			return "", api.NewError(api.CodeInvalidRequest,
				filepath.Base(project)+" targets more than one framework ("+fws+")", "pick one with --framework")
		}

		return "", api.NewError(api.CodeBuildFailed, "dotnet build "+project+" produced no output",
			"fix the build errors above, then start again")
	}

	if noBuild {
		if _, err := os.Stat(target); err != nil {
			return "", api.NewError(api.CodeInvalidRequest, filepath.Base(project)+" is not built: "+target+" does not exist", "drop --no-build")
		}
	}

	if !strings.EqualFold(props[propIsTestingPlatformApp], "true") {
		return "", api.NewError(api.CodeNoTestHost,
			filepath.Base(project)+" doesn't look like a Microsoft.Testing.Platform app, although "+reason+
				" ("+propIsTestingPlatformApp+" isn't true)",
			"mark it with <IsTestingPlatformApplication>true</IsTestingPlatformApplication>, or debug it directly: "+
				"eyedbg start dotnet --project "+shellArg(project))
	}

	return target, nil
}

// pickDialect is the launched app's command-line dialect: TUnit and xUnit
// v3 (not switched to the MTP runner) have their own; everything else
// detectLaunch found runs on the Microsoft.Testing.Platform CLI.
func pickDialect(kind detectedKind, props map[string]string) testDialect {
	switch {
	case kind == detectTUnit:
		return dialectTUnit
	case kind == detectXunitV3 && !strings.EqualFold(props[propUseMTPRunner], "true"):
		return dialectXunitNative
	default:
		return dialectMTP
	}
}

// testDialectArgs is the fixed arguments and, non-empty, FILTER's own
// option, for dialect (D4, D6); extra "-- " arguments go after these,
// verbatim (D5).
func testDialectArgs(dialect testDialect, filter string) []string {
	var args []string

	switch dialect {
	case dialectXunitNative:
		args = append(args, "-noColor")

		if filter != "" {
			args = append(args, "-filterVSTest", filter)
		}
	case dialectTUnit:
		if filter != "" {
			args = append(args, "--treenode-filter", filter)
		}
	case dialectMTP:
		if filter != "" {
			args = append(args, "--filter", filter)
		}
	}

	return args
}

// testLaunchEnv is the launched test app's environment (D7): the daemon's
// opt-outs, overlaid by the user's own --env (which wins).
func testLaunchEnv(user map[string]string) map[string]string {
	env := map[string]string{
		"DOTNET_CLI_TELEMETRY_OPTOUT":      "1",
		"TESTINGPLATFORM_TELEMETRY_OPTOUT": "1",
		"DOTNET_NOLOGO":                    "1",
	}

	maps.Copy(env, user)

	return env
}

// launchTestProgram describes a launched test run for status (D14): "dotnet
// <app>.dll" plus its fixed and filter arguments; "-- " arguments are never
// shown (as a plain launch's program arguments aren't).
func launchTestProgram(target string, fixed []string) string {
	shown := append([]string{"dotnet", filepath.Base(target)}, fixed...)

	return strings.Join(shown, " ")
}

// testFailure is the error for a VSTest 'dotnet test' that exited before a
// test host waited for the debugger.
func testFailure(project string, code int, output string) error {
	// An MSBuild error line, e.g. "Foo.cs(3,1): error CS1002: ; expected".
	compileError := regexp.MustCompile(`: error [A-Z]+\d+:`)

	if compileError.MatchString(output) || strings.Contains(output, "Build FAILED") {
		return api.NewError(api.CodeBuildFailed, "dotnet test "+project+" failed to build:\n"+tail(output, buildOutputLines),
			"fix the build errors above, then run the tests again")
	}

	return api.NewError(api.CodeNoTestHost,
		fmt.Sprintf("dotnet test exited with code %d before a test host waited for the debugger:\n%s", code, tail(output, buildOutputLines)),
		"is it a VSTest project (Microsoft.NET.Test.Sdk) with tests matching the filter? eyedbg launches "+
			"Microsoft.Testing.Platform and xUnit v3 projects itself (see 'eyedbg help test'); an unrecognized one can be "+
			"marked with <IsTestingPlatformApplication>true</IsTestingPlatformApplication>")
}
