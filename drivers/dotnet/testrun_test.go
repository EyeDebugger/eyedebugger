// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
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
