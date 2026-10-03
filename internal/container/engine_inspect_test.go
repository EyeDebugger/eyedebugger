// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

const inspectOut = `["` + fullID + `","/web-1","sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210","dotnet",` +
	`true,false,false,null,[5000000000,3000000000,3,false],"my-app","web","/home/me/app","",""]`

func TestInspectViaDocker(t *testing.T) {
	t.Parallel()

	calls := filepath.Join(t.TempDir(), "calls")
	e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{
		{Match: []string{"inspect", "--type", "container", "--format"}, Stdout: inspectOut},
	}})
	e.Host = "tcp://h:1"

	info, err := e.Inspect(t.Context(), "web-1")
	if err != nil {
		t.Fatal(err)
	}

	if info.ID != fullID || info.Name != "web-1" || info.Service != "web" || info.UnhealthyAfter().Seconds() != 18 {
		t.Errorf("info = %+v", info)
	}

	got := containertest.ReadCalls(t, calls)
	if len(got) != 1 {
		t.Fatalf("%d docker calls, want 1: %q", len(got), got)
	}

	argv := got[0]
	if len(argv) != 8 || argv[0] != "--host=tcp://h:1" || !slices.Equal(argv[1:5], []string{"inspect", "--type", "container", "--format"}) {
		t.Fatalf("argv = %q", argv)
	}

	if want := []string{"--", "web-1"}; !slices.Equal(argv[len(argv)-2:], want) {
		t.Errorf("argv ends %q, want %q", argv[len(argv)-2:], want)
	}

	for _, banned := range []string{"Env", "Cmd", "Args", "Entrypoint"} {
		if strings.Contains(argv[5], banned) {
			t.Errorf("the template names %s", banned)
		}
	}
}

// TestInspectSourceDir: the project directory a label names becomes the
// container's SourceDir only when its config_files label names a compose file
// inside it. A hostile image's LABELs can set both labels, so the files must
// really be on this machine, in that directory.
func TestInspectSourceDir(t *testing.T) {
	t.Parallel()

	proj := realDir(t)
	elsewhere := realDir(t)

	writeFile(t, filepath.Join(proj, "compose.yaml"))
	writeFile(t, filepath.Join(elsewhere, "compose.yaml"))

	tests := []struct {
		name, workingDir, files string
		want                    string
	}{
		{"compose made it from files there", proj, filepath.Join(proj, "compose.yaml"), proj},
		{"the label names a directory with no such file", proj, filepath.Join(proj, "missing.yaml"), ""},
		{"the files are in another directory", proj, filepath.Join(elsewhere, "compose.yaml"), ""},
		{"no files label", proj, "", ""},
		{"no directory label", "", filepath.Join(proj, "compose.yaml"), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			out := `["` + fullID + `","/web-1","sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210","dotnet",` +
				`true,false,false,null,null,"my-app","web",` + jsonString(tt.workingDir) + `,"",` + jsonString(tt.files) + `]`

			e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{Match: []string{"inspect"}, Stdout: out}}})

			info, err := e.Inspect(t.Context(), "web-1")
			if err != nil {
				t.Fatal(err)
			}

			if info.SourceDir != tt.want {
				t.Errorf("SourceDir = %q, want %q", info.SourceDir, tt.want)
			}

			if info.WorkingDir != tt.workingDir {
				t.Errorf("WorkingDir = %q, want the label as it is, %q", info.WorkingDir, tt.workingDir)
			}
		})
	}
}

func TestInspectErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     containertest.Rule
		ref      string
		wantCode api.Code
		wantMsg  string
	}{
		{"no such container", containertest.Rule{Match: []string{"inspect"}, Exit: 1, Stderr: "Error: No such container: web"}, "web", api.CodeInvalidRequest, "no container"},
		{"daemon down", containertest.Rule{Match: []string{"inspect"}, Exit: 1, Stderr: "Cannot connect to the Docker daemon"}, "web", api.CodeAttachFailed, "Cannot connect"},
		{"garbage", containertest.Rule{Match: []string{"inspect"}, Stdout: "hello"}, "web", api.CodeAttachFailed, "unexpected answer"},
		{"injected ref", containertest.Rule{Match: []string{"inspect"}, Stdout: inspectOut}, "--privileged", api.CodeInvalidRequest, "invalid container"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := filepath.Join(t.TempDir(), "calls")
			e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{tt.rule}})

			_, err := e.Inspect(t.Context(), tt.ref)
			if api.CodeOf(err) != tt.wantCode || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("err = %v (%s), want %s containing %q", err, api.CodeOf(err), tt.wantCode, tt.wantMsg)
			}

			if tt.name == "injected ref" && len(containertest.ReadCalls(t, calls)) != 0 {
				t.Error("docker ran for an invalid reference")
			}
		})
	}
}

func TestImagePlatform(t *testing.T) {
	t.Parallel()

	const image = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

	tests := []struct {
		out, goos, arch, variant string
		bad                      bool
	}{
		{out: "linux/arm64/\n", goos: "linux", arch: "arm64"},
		{out: "linux/arm/v7\n", goos: "linux", arch: "arm", variant: "v7"},
		{out: "windows/amd64/\n", goos: "windows", arch: "amd64"},
		{out: "linux/amd64\n", bad: true},
		{out: "Linux/amd64/\n", bad: true},
		{out: "linux/amd64/\nextra", bad: true},
		{out: "", bad: true},
	}

	for _, tt := range tests {
		t.Run(strings.TrimSpace(tt.out), func(t *testing.T) {
			t.Parallel()

			e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{{Match: []string{"image", "inspect", "--format"}, Stdout: tt.out}}})

			goos, arch, variant, err := e.ImagePlatform(t.Context(), image)
			if tt.bad {
				if err == nil {
					t.Errorf("ImagePlatform(%q) accepted", tt.out)
				}

				return
			}

			if err != nil || goos != tt.goos || arch != tt.arch || variant != tt.variant {
				t.Errorf("ImagePlatform(%q) = %q %q %q, %v", tt.out, goos, arch, variant, err)
			}
		})
	}

	e := containertest.Engine(t, containertest.Scenario{})
	if _, _, _, err := e.ImagePlatform(t.Context(), "--format"); err == nil {
		t.Error("ImagePlatform accepted a flag as the image")
	}
}

func TestComposePSViaDocker(t *testing.T) {
	t.Parallel()

	row := `{"Command":"\"dotnet Web.dll\"","ID":"0123456789ab","Name":"my-app-web-1","Project":"my-app","Service":"web","State":"running"}`
	calls := filepath.Join(t.TempDir(), "calls")
	e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{
		{Match: []string{"compose", "--file=a.yml", "--project-name=my-app", "ps", "--format", "json"}, Stdout: row + "\n"},
	}})
	e.Context = "colima"

	list, err := e.ComposePS(t.Context(), container.ComposeOptions{Files: []string{"a.yml"}, ProjectName: "my-app"})
	if err != nil {
		t.Fatal(err)
	}

	if len(list) != 1 || list[0].Service != "web" || list[0].Project != "my-app" || list[0].State != "running" {
		t.Errorf("list = %+v", list)
	}

	want := []string{"--context=colima", "compose", "--file=a.yml", "--project-name=my-app", "ps", "--format", "json"}
	if got := containertest.ReadCalls(t, calls); len(got) != 1 || !slices.Equal(got[0], want) {
		t.Errorf("docker was run as %q, want one call %q", got, want)
	}
}

func TestComposePSErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		rule     containertest.Rule
		opts     container.ComposeOptions
		wantCode api.Code
		wantMsg  string
	}{
		{
			"no compose file",
			containertest.Rule{Match: []string{"compose"}, Exit: 1, Stderr: "no configuration file provided: not found"},
			container.ComposeOptions{},
			api.CodeInvalidRequest, "no compose file",
		},
		{
			"engine down",
			containertest.Rule{Match: []string{"compose"}, Exit: 1, Stderr: "Cannot connect to the Docker daemon"},
			container.ComposeOptions{},
			api.CodeAttachFailed, "Cannot connect",
		},
		{"garbage", containertest.Rule{Match: []string{"compose"}, Stdout: "hello"}, container.ComposeOptions{}, api.CodeAttachFailed, "unexpected answer"},
		{
			"flag as project",
			containertest.Rule{Match: []string{"compose"}, Stdout: "[]"},
			container.ComposeOptions{ProjectName: "--privileged"},
			api.CodeInvalidRequest, "invalid project name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := filepath.Join(t.TempDir(), "calls")
			e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{tt.rule}})

			_, err := e.ComposePS(t.Context(), tt.opts)
			if api.CodeOf(err) != tt.wantCode || err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("ComposePS error = %v, want %s containing %q", err, tt.wantCode, tt.wantMsg)
			}

			if tt.wantCode == api.CodeInvalidRequest && strings.Contains(tt.name, "flag") {
				if got := containertest.ReadCalls(t, calls); len(got) != 0 {
					t.Errorf("docker ran for an invalid project: %q", got)
				}
			}
		})
	}
}
