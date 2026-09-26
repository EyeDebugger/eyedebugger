// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

func TestTestCommand(t *testing.T) {
	t.Parallel()

	project := filepath.Join(t.TempDir(), "tests.csproj")
	base := []string{"test", project, "-c", "Debug", "--nologo", "--tl:off", "--disable-build-servers"}

	tests := []struct {
		name     string
		spec     session.TestSpec
		wantArgs []string
		wantProg string
	}{
		{name: "plain", wantArgs: base, wantProg: "dotnet test tests.csproj"},
		{
			name:     "filter",
			spec:     session.TestSpec{TestSpec: api.TestSpec{Filter: "Adds"}},
			wantArgs: append(slices.Clone(base), "--filter", "Adds"),
			wantProg: "dotnet test tests.csproj --filter Adds",
		},
		{
			name:     "all",
			spec:     session.TestSpec{LaunchSpec: api.LaunchSpec{NoBuild: true}, TestSpec: api.TestSpec{Filter: "Name~A B", Framework: "net10.0"}},
			wantArgs: append(slices.Clone(base), "--filter", "Name~A B", "--framework", "net10.0", "--no-build"),
			wantProg: "dotnet test tests.csproj --filter Name~A B --framework net10.0 --no-build",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := testCommand("/sdk/dotnet", project, tt.spec)
			if tc.Path != "/sdk/dotnet" || !slices.Equal(tc.Args, tt.wantArgs) || tc.Program != tt.wantProg || tc.Dir != filepath.Dir(project) {
				t.Fatalf("testCommand = %q %q (dir %q, program %q), want args %q, program %q",
					tc.Path, tc.Args, tc.Dir, tc.Program, tt.wantArgs, tt.wantProg)
			}

			for _, want := range []string{"VSTEST_HOST_DEBUG=1", "VSTEST_DEBUG_NOBP=1"} {
				if !slices.Contains(tc.Env, want) {
					t.Errorf("env %q lacks %s", tc.Env, want)
				}
			}
		})
	}
}

func TestUsesMTP(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, dir, content string) {
		t.Helper()

		if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	mtp := `{"test": {"runner": "microsoft.testing.platform"}}`

	tests := []struct {
		name  string
		setup func(t *testing.T, root, project string)
		env   map[string]string
		want  bool
	}{
		{name: "no global.json", setup: func(*testing.T, string, string) {}},
		{name: "project dir", setup: func(t *testing.T, _, project string) { t.Helper(); write(t, project, mtp) }, want: true},
		{name: "parent dir", setup: func(t *testing.T, root, _ string) { t.Helper(); write(t, root, mtp) }, want: true},
		{
			name: "nearest wins",
			setup: func(t *testing.T, root, project string) {
				t.Helper()
				write(t, root, mtp)
				write(t, project, `{"sdk": {"version": "10.0.100"}}`)
			},
		},
		{name: "vstest", setup: func(t *testing.T, _, project string) { t.Helper(); write(t, project, `{"test": {"runner": "VSTest"}}`) }},
		{name: "env", setup: func(*testing.T, string, string) {}, env: map[string]string{envTestRunner: mtpRunner}, want: true},
		{
			name:  "env overrides nothing it doesn't name",
			setup: func(*testing.T, string, string) {},
			env:   map[string]string{envTestRunner: "VSTest"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			project := filepath.Join(root, "tests")

			if err := os.Mkdir(project, 0o700); err != nil {
				t.Fatal(err)
			}

			tt.setup(t, root, project)

			env := tt.env
			if env == nil {
				// Shield the check from the environment's own setting.
				env = map[string]string{envTestRunner: ""}
			}

			if got := usesMTP(project, env); got != tt.want {
				t.Fatalf("usesMTP = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUsesXunitV3(t *testing.T) {
	t.Parallel()

	ref := func(pkg string) string {
		return `<Project><ItemGroup><PackageReference Include="` + pkg + `" Version="3.2.2" /></ItemGroup></Project>`
	}

	tests := []struct {
		name      string
		project   string
		buildRoot string // Directory.Build.props above the project; "" for none
		want      bool
	}{
		{name: "xunit.v3", project: ref("xunit.v3"), want: true},
		{name: "core, any case", project: ref("XUnit.V3.Core"), want: true},
		{name: "mtp flavor", project: ref("xunit.v3.mtp-v2"), want: true},
		{name: "attributes split over lines", project: "<PackageReference\n  Version=\"3.2.2\"\n  Include=\"xunit.v3\" />", want: true},
		{name: "in Directory.Build.props", project: ref("Moq"), buildRoot: ref("xunit.v3"), want: true},
		{name: "xunit v2", project: ref("xunit") + ref("xunit.runner.visualstudio")},
		{name: "only the assertions", project: ref("xunit.v3.assert")},
		{name: "a version, not a reference", project: ref("Moq"), buildRoot: `<PackageVersion Include="xunit.v3" Version="3.2.2" />`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			project := filepath.Join(root, "tests", "tests.csproj")

			if err := os.Mkdir(filepath.Dir(project), 0o700); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(project, []byte(tt.project), 0o600); err != nil {
				t.Fatal(err)
			}

			if tt.buildRoot != "" {
				if err := os.WriteFile(filepath.Join(root, "Directory.Build.props"), []byte(tt.buildRoot), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if got := usesXunitV3(project); got != tt.want {
				t.Fatalf("usesXunitV3 = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShellArg(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{in: "/src/tests/tests.csproj", want: "/src/tests/tests.csproj"},
		{in: `C:\src\tests.csproj`, want: `C:\src\tests.csproj`},
		{in: "/src/Project Lunegit/t.csproj", want: "'/src/Project Lunegit/t.csproj'"},
		{in: "/src/it's/t.csproj", want: `'/src/it'\''s/t.csproj'`},
		{in: "/src/$HOME/t.csproj", want: "'/src/$HOME/t.csproj'"},
		{in: "", want: "''"},
	}

	for _, tt := range tests {
		if got := shellArg(tt.in); got != tt.want {
			t.Errorf("shellArg(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
}

func TestHostPID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		line string
		want int
	}{
		{"Process Id: 96456, Name: dotnet", 96456},
		{"Process Id: 4242, Name: testhost", 4242},
		{"   Process Id: 17, Name: testhost.exe", 17},
		{"Waiting for debugger to attach... Process Id: 88, Name: tests", 88},
		{"Process Id: x, Name: dotnet", 0},
		{"Process Id: 12", 0},
		{"Host debugging is enabled. Please attach debugger to testhost process to continue.", 0},
		{"Process Id: 0, Name: dotnet", 0},
	}

	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			t.Parallel()

			got, ok := hostPID(tt.line)
			if ok != (tt.want != 0) || got != tt.want {
				t.Fatalf("hostPID(%q) = %d, %v; want %d", tt.line, got, ok, tt.want)
			}
		})
	}
}

func TestTestFailure(t *testing.T) {
	t.Parallel()

	tests := []struct {
		output string
		want   api.Code
	}{
		{"/src/A.cs(3,1): error CS1002: ; expected [/src/tests.csproj]", api.CodeBuildFailed},
		{"Build FAILED.", api.CodeBuildFailed},
		{"No test matches the given testcase filter `Nope`", api.CodeNoTestHost},
		{"", api.CodeNoTestHost},
	}

	for _, tt := range tests {
		t.Run(tt.output, func(t *testing.T) {
			t.Parallel()

			err := testFailure("/src/tests.csproj", 1, tt.output)
			if api.CodeOf(err) != tt.want {
				t.Fatalf("testFailure(%q) = %v, want %s", tt.output, err, tt.want)
			}

			if tt.want == api.CodeNoTestHost && !strings.Contains(err.Error(), "code 1") {
				t.Errorf("message %q lacks the exit code", err)
			}
		})
	}
}

// writeFile writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDetectLaunch(t *testing.T) {
	t.Parallel()

	ref := func(pkg string) string {
		return `<Project><ItemGroup><PackageReference Include="` + pkg + `" Version="1.0.0" /></ItemGroup></Project>`
	}

	tests := []struct {
		name       string
		project    string
		buildProps string // Directory.Build.props above the project; "" for none
		env        map[string]string
		wantReason string // substring; "" means detectLaunch found nothing (VSTest)
		wantKind   detectedKind
	}{
		{name: "plain VSTest", project: ref("Microsoft.NET.Test.Sdk")},
		{name: "xunit v2", project: ref("xunit") + ref("xunit.runner.visualstudio")},
		{name: "xunit.v3", project: ref("xunit.v3"), wantReason: "xunit.v3", wantKind: detectXunitV3},
		{name: "global.json/env MTP", project: "<Project></Project>", env: map[string]string{envTestRunner: mtpRunner}, wantReason: mtpRunner, wantKind: detectOther},
		{
			name: "EnableMSTestRunner", wantReason: "EnableMSTestRunner", wantKind: detectOther,
			project: `<Project><PropertyGroup><EnableMSTestRunner>true</EnableMSTestRunner></PropertyGroup></Project>`,
		},
		{
			name: "Condition attribute, case True", wantReason: "EnableMSTestRunner", wantKind: detectOther,
			project: `<Project><PropertyGroup><EnableMSTestRunner Condition="'$(Configuration)'=='Debug'">True</EnableMSTestRunner>` +
				`</PropertyGroup></Project>`,
		},
		{name: "marker set false", project: `<Project><PropertyGroup><EnableMSTestRunner>false</EnableMSTestRunner></PropertyGroup></Project>`},
		{
			name:    "commented-out marker",
			project: `<Project><!-- <EnableMSTestRunner>true</EnableMSTestRunner> --></Project>`,
		},
		{
			name: "marker in Directory.Build.props above the project", project: "<Project></Project>",
			buildProps: `<Project><PropertyGroup><UseMicrosoftTestingPlatformRunner>true</UseMicrosoftTestingPlatformRunner></PropertyGroup></Project>`,
			wantReason: "UseMicrosoftTestingPlatformRunner", wantKind: detectOther,
		},
		{
			name: "MSTest.Sdk attribute with a version", project: `<Project Sdk="MSTest.Sdk/4.4.1"></Project>`,
			wantReason: "MSTest.Sdk", wantKind: detectOther,
		},
		{
			name: "MSTest.Sdk element", project: `<Project><Sdk Name="MSTest.Sdk" Version="4.4.1" /></Project>`,
			wantReason: "MSTest.Sdk", wantKind: detectOther,
		},
		{name: "MSTest.Sdk with UseVSTest true", project: `<Project Sdk="MSTest.Sdk"><PropertyGroup><UseVSTest>true</UseVSTest></PropertyGroup></Project>`},
		{
			name: "MSTest.Sdk, UseVSTest true in Directory.Build.props (F1)", project: `<Project Sdk="MSTest.Sdk"></Project>`,
			buildProps: `<Project><PropertyGroup><UseVSTest>true</UseVSTest></PropertyGroup></Project>`,
		},
		{name: "TUnit.Assertions alone", project: ref("TUnit.Assertions")},
		{name: "TUnit", project: ref("TUnit"), wantReason: "TUnit", wantKind: detectTUnit},
		{name: "TUnit.Engine", project: ref("TUnit.Engine"), wantReason: "TUnit", wantKind: detectTUnit},
		{
			name: "TUnit ref, global.json/env MTP selects the dialect (F2)", project: ref("TUnit"),
			env: map[string]string{envTestRunner: mtpRunner}, wantReason: mtpRunner, wantKind: detectTUnit,
		},
		{
			name: "xunit.v3 ref, global.json/env MTP selects the dialect (F2)", project: ref("xunit.v3"),
			env: map[string]string{envTestRunner: mtpRunner}, wantReason: mtpRunner, wantKind: detectXunitV3,
		},
		{
			name: "TUnit ref, TestingPlatformDotnetTestSupport marker selects the dialect (F2)",
			project: `<Project><ItemGroup><PackageReference Include="TUnit" Version="1.0.0" /></ItemGroup>` +
				`<PropertyGroup><TestingPlatformDotnetTestSupport>true</TestingPlatformDotnetTestSupport></PropertyGroup></Project>`,
			wantReason: "TestingPlatformDotnetTestSupport", wantKind: detectTUnit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			project := filepath.Join(root, "tests.csproj")
			writeFile(t, project, tt.project)

			if tt.buildProps != "" {
				writeFile(t, filepath.Join(root, "Directory.Build.props"), tt.buildProps)
			}

			env := tt.env
			if env == nil {
				env = map[string]string{envTestRunner: ""} // shield from the real environment
			}

			reason, kind := detectLaunch(project, env)

			if tt.wantReason == "" {
				if reason != "" {
					t.Fatalf("detectLaunch = %q, %v; want VSTest", reason, kind)
				}

				return
			}

			if !strings.Contains(reason, tt.wantReason) || kind != tt.wantKind {
				t.Fatalf("detectLaunch = %q, %v; want a reason containing %q, kind %v", reason, kind, tt.wantReason, tt.wantKind)
			}
		})
	}
}

func TestDecideLaunchTarget(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	built := filepath.Join(dir, "tests.dll")
	writeFile(t, built, "")

	notBuilt := filepath.Join(dir, "missing.dll")
	project := filepath.Join(dir, "tests.csproj")

	tests := []struct {
		name     string
		props    map[string]string
		noBuild  bool
		want     string
		wantCode api.Code
	}{
		{name: "ok", props: map[string]string{propTargetPath: built, propIsTestingPlatformApp: "true"}, want: built},
		{name: "case-insensitive true", props: map[string]string{propTargetPath: built, propIsTestingPlatformApp: "True"}, want: built},
		{
			name: "multi-targeting needs --framework", props: map[string]string{propTargetFrameworks: "net10.0;net9.0"},
			wantCode: api.CodeInvalidRequest,
		},
		{name: "no output at all", props: map[string]string{}, wantCode: api.CodeBuildFailed},
		{
			name: "not a testing platform app", props: map[string]string{propTargetPath: built, propIsTestingPlatformApp: "false"},
			wantCode: api.CodeNoTestHost,
		},
		{
			name: "not a testing platform app, property unset", props: map[string]string{propTargetPath: built},
			wantCode: api.CodeNoTestHost,
		},
		{
			name: "no-build, not built", props: map[string]string{propTargetPath: notBuilt, propIsTestingPlatformApp: "true"},
			noBuild: true, wantCode: api.CodeInvalidRequest,
		},
		{
			name: "no-build, already built", props: map[string]string{propTargetPath: built, propIsTestingPlatformApp: "true"},
			noBuild: true, want: built,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := decideLaunchTarget(project, "it references xunit.v3", tt.props, tt.noBuild)

			if tt.wantCode != "" {
				if api.CodeOf(err) != tt.wantCode {
					t.Fatalf("decideLaunchTarget = %v (code %s), want %s", err, api.CodeOf(err), tt.wantCode)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Fatalf("decideLaunchTarget = %q, %v; want %q, no error", got, err, tt.want)
			}
		})
	}
}

func TestPickDialect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		kind  detectedKind
		props map[string]string
		want  testDialect
	}{
		{name: "xUnit v3 native", kind: detectXunitV3, want: dialectXunitNative},
		{name: "xUnit v3 native, property empty", kind: detectXunitV3, props: map[string]string{propUseMTPRunner: ""}, want: dialectXunitNative},
		{name: "xUnit v3 switched to MTP", kind: detectXunitV3, props: map[string]string{propUseMTPRunner: "true"}, want: dialectMTP},
		{name: "xUnit v3 switched to MTP, case", kind: detectXunitV3, props: map[string]string{propUseMTPRunner: "True"}, want: dialectMTP},
		{name: "TUnit", kind: detectTUnit, want: dialectTUnit},
		{name: "TUnit ignores the xUnit property", kind: detectTUnit, props: map[string]string{propUseMTPRunner: "false"}, want: dialectTUnit},
		{name: "MSTest/NUnit/MSTest.Sdk/global.json (detectOther)", kind: detectOther, want: dialectMTP},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := pickDialect(tt.kind, tt.props); got != tt.want {
				t.Errorf("pickDialect(%v, %v) = %v, want %v", tt.kind, tt.props, got, tt.want)
			}
		})
	}
}

func TestTestDialectArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dialect testDialect
		filter  string
		want    []string
	}{
		{name: "xUnit native, no filter", dialect: dialectXunitNative, want: []string{"-noColor"}},
		{name: "xUnit native, filter", dialect: dialectXunitNative, filter: "Adds", want: []string{"-noColor", "-filterVSTest", "Adds"}},
		{name: "MTP, no filter", dialect: dialectMTP, want: nil},
		{name: "MTP, filter", dialect: dialectMTP, filter: "Adds", want: []string{"--filter", "Adds"}},
		{name: "TUnit, no filter", dialect: dialectTUnit, want: nil},
		{name: "TUnit, filter", dialect: dialectTUnit, filter: "Adds", want: []string{"--treenode-filter", "Adds"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := testDialectArgs(tt.dialect, tt.filter); !slices.Equal(got, tt.want) {
				t.Errorf("testDialectArgs(%v, %q) = %v, want %v", tt.dialect, tt.filter, got, tt.want)
			}
		})
	}
}

func TestTestLaunchEnv(t *testing.T) {
	t.Parallel()

	base := testLaunchEnv(nil)
	for k, v := range map[string]string{"DOTNET_CLI_TELEMETRY_OPTOUT": "1", "TESTINGPLATFORM_TELEMETRY_OPTOUT": "1", "DOTNET_NOLOGO": "1"} {
		if base[k] != v {
			t.Errorf("testLaunchEnv(nil)[%s] = %q, want %q", k, base[k], v)
		}
	}

	// The user's own --env wins over the daemon's opt-outs.
	overlaid := testLaunchEnv(map[string]string{"DOTNET_NOLOGO": "0", "MY_VAR": "x"})
	if overlaid["DOTNET_NOLOGO"] != "0" || overlaid["MY_VAR"] != "x" || overlaid["DOTNET_CLI_TELEMETRY_OPTOUT"] != "1" {
		t.Errorf("testLaunchEnv overlay = %v", overlaid)
	}
}

func TestLaunchTestProgram(t *testing.T) {
	t.Parallel()

	target := filepath.Join("bin", "Debug", "net10.0", "tests.dll")

	tests := []struct {
		fixed []string
		want  string
	}{
		{fixed: nil, want: "dotnet tests.dll"},
		{fixed: []string{"-noColor"}, want: "dotnet tests.dll -noColor"},
		{fixed: []string{"--filter", "Adds"}, want: "dotnet tests.dll --filter Adds"},
	}

	for _, tt := range tests {
		if got := launchTestProgram(target, tt.fixed); got != tt.want {
			t.Errorf("launchTestProgram(%q, %v) = %q, want %q", target, tt.fixed, got, tt.want)
		}
	}
}

func TestBuildTestLaunch(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	project := filepath.Join(root, "src", "tests.csproj")
	target := filepath.Join(root, "src", "bin", "Debug", "net10.0", "tests.dll")

	tests := []struct {
		name     string
		dialect  testDialect
		spec     session.TestSpec
		wantArgs []string // after target, the whole "args" argv
		wantProg string
	}{
		{
			name:     "MTP, no filter, no extra args",
			dialect:  dialectMTP,
			wantArgs: nil,
			wantProg: "dotnet tests.dll",
		},
		{
			name:     "MTP, filter only",
			dialect:  dialectMTP,
			spec:     session.TestSpec{TestSpec: api.TestSpec{Filter: "Adds"}},
			wantArgs: []string{"--filter", "Adds"},
			wantProg: "dotnet tests.dll --filter Adds",
		},
		{
			name:     "MTP, filter then '--' args (D5 order: fixed, filter, then extra verbatim)",
			dialect:  dialectMTP,
			spec:     session.TestSpec{TestSpec: api.TestSpec{Filter: "Adds"}, LaunchSpec: api.LaunchSpec{Args: []string{"-method", "*Adds"}}},
			wantArgs: []string{"--filter", "Adds", "-method", "*Adds"},
			wantProg: "dotnet tests.dll --filter Adds",
		},
		{
			name:     "xUnit native, no filter, extra args",
			dialect:  dialectXunitNative,
			spec:     session.TestSpec{LaunchSpec: api.LaunchSpec{Args: []string{"-v"}}},
			wantArgs: []string{"-noColor", "-v"},
			wantProg: "dotnet tests.dll -noColor",
		},
		{
			name:     "TUnit, filter only",
			dialect:  dialectTUnit,
			spec:     session.TestSpec{TestSpec: api.TestSpec{Filter: "Foo"}},
			wantArgs: []string{"--treenode-filter", "Foo"},
			wantProg: "dotnet tests.dll --treenode-filter Foo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tc := buildTestLaunch(session.Launch{AdapterID: "netcoredbg"}, "/sdk/dotnet", project, target, tt.dialect, tt.spec)
			checkTestLaunch(t, tc, project, target, tt.wantArgs, tt.wantProg)
		})
	}
}

// checkTestLaunch asserts tc, from buildTestLaunch(…, project, target, …):
// Program, argv (target then wantArgs), cwd, and the daemon's own three
// opt-outs among the launch env.
func checkTestLaunch(t *testing.T, tc session.TestCommand, project, target string, wantArgs []string, wantProg string) {
	t.Helper()

	if tc.Launch == nil || tc.Launch.AdapterID != "netcoredbg" {
		t.Fatalf("Launch = %+v, want AdapterID preserved", tc.Launch)
	}

	if tc.Program != wantProg {
		t.Errorf("Program = %q, want %q", tc.Program, wantProg)
	}

	args, _ := tc.Launch.Arguments["args"].([]string)
	if want := append([]string{target}, wantArgs...); !slices.Equal(args, want) {
		t.Errorf("args = %v, want %v", args, want)
	}

	if cwd, _ := tc.Launch.Arguments["cwd"].(string); cwd != filepath.Dir(project) {
		t.Errorf("cwd = %q, want %q", cwd, filepath.Dir(project))
	}

	env, _ := tc.Launch.Arguments["env"].(map[string]string)
	for k, v := range map[string]string{
		"DOTNET_CLI_TELEMETRY_OPTOUT":      "1",
		"TESTINGPLATFORM_TELEMETRY_OPTOUT": "1",
		"DOTNET_NOLOGO":                    "1",
	} {
		if env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, env[k], v)
		}
	}
}

func TestBuildTestLaunchEnvOverlay(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	project := filepath.Join(root, "src", "tests.csproj")
	target := filepath.Join(root, "src", "bin", "Debug", "net10.0", "tests.dll")

	spec := session.TestSpec{LaunchSpec: api.LaunchSpec{Env: map[string]string{"DOTNET_NOLOGO": "0", "MY_VAR": "x"}}}

	tc := buildTestLaunch(session.Launch{}, "/sdk/dotnet", project, target, dialectMTP, spec)

	env, _ := tc.Launch.Arguments["env"].(map[string]string)
	if env["DOTNET_NOLOGO"] != "0" || env["MY_VAR"] != "x" || env["DOTNET_CLI_TELEMETRY_OPTOUT"] != "1" {
		t.Errorf("env overlay = %v", env)
	}
}

func TestParseBuildProperties(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		out         string
		checkResult bool
		wantOK      bool
		want        map[string]string
	}{
		{
			name:        "build, success",
			out:         `{"Properties":{"TargetPath":"/p/tests.dll"},"TargetResults":{"Build":{"Result":"Success"}}}`,
			checkResult: true, wantOK: true, want: map[string]string{"TargetPath": "/p/tests.dll"},
		},
		{
			name:        "build, failed",
			out:         `{"Properties":{"TargetPath":""},"TargetResults":{"Build":{"Result":"Failed"}}}`,
			checkResult: true, wantOK: false,
		},
		{
			name:        "msbuild eval, no TargetResults to check",
			out:         `{"Properties":{"TargetPath":"/p/tests.dll"}}`,
			checkResult: false, wantOK: true, want: map[string]string{"TargetPath": "/p/tests.dll"},
		},
		{name: "not JSON", out: "warning: something\n", checkResult: true, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			props, ok := parseBuildProperties([]byte(tt.out), tt.checkResult)
			if ok != tt.wantOK {
				t.Fatalf("parseBuildProperties(%q, %v) ok = %v, want %v", tt.out, tt.checkResult, ok, tt.wantOK)
			}

			if ok && !maps.Equal(props, tt.want) {
				t.Fatalf("parseBuildProperties(%q, %v) = %v, want %v", tt.out, tt.checkResult, props, tt.want)
			}
		})
	}
}
