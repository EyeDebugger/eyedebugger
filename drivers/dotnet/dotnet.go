// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// buildOutputLines is how much of a failed build's output is reported.
const buildOutputLines = 40

// Names netcoredbg and the CLI know .NET by.
const (
	lang        = "dotnet"
	adapterType = "coreclr"
)

// Language is the language name the .NET driver serves.
const Language = lang

// Driver debugs .NET programs with netcoredbg (docs/DESIGN.md §8), as
// described by the adapter manifest serving dotnet.
type Driver struct {
	m *adapters.Manifest
}

// New returns the .NET driver with the manifest the default registry
// (bundled and user manifests) has for dotnet.
func New() *Driver { return NewWith(adapters.Default(lang).Language(lang)) }

// NewWith returns the .NET driver for adapter manifest m (nil: none, so
// every start fails with ADAPTER_NOT_INSTALLED).
func NewWith(m *adapters.Manifest) *Driver { return &Driver{m: m} }

// Name implements session.Driver.
func (*Driver) Name() string { return lang }

// Prepare implements session.Driver: it builds the project (unless a program
// is given) and launches the result under netcoredbg via the dotnet host.
func (d *Driver) Prepare(ctx context.Context, spec session.LaunchSpec) (session.Launch, error) {
	if len(spec.Options) > 0 {
		return session.Launch{}, api.NewError(api.CodeInvalidRequest, "dotnet takes no --opt options", "see 'eyedbg help start'")
	}

	launch, err := d.netcoredbg()
	if err != nil {
		return session.Launch{}, err
	}

	host, err := FindHost()
	if err != nil {
		return session.Launch{}, err
	}

	program := spec.Program
	cwd := spec.Cwd

	if program == "" {
		if spec.NoBuild {
			return session.Launch{}, api.NewError(api.CodeInvalidRequest, "--no-build needs --program",
				"pass the built .dll with --program, or drop --no-build")
		}

		project, err := findProject(spec.Project)
		if err != nil {
			return session.Launch{}, err
		}

		program, err = build(ctx, host, project)
		if err != nil {
			return session.Launch{}, err
		}

		if cwd == "" {
			cwd = filepath.Dir(project)
		}
	}

	if cwd == "" {
		cwd = filepath.Dir(program)
	}

	launch.Arguments = launchArguments(host, program, cwd, spec)
	launch.Program = program

	return launch, nil
}

// netcoredbg returns the Launch fields every netcoredbg session shares,
// from the manifest.
func (d *Driver) netcoredbg() (session.Launch, error) {
	m := d.m

	switch {
	case m == nil:
		return session.Launch{}, api.NewError(api.CodeAdapterMissing, "no adapter manifest serves dotnet",
			"see 'eyedbg adapters ls': a user manifest may have replaced netcoredbg's or failed to load")
	case m.Adapter.Runtime != adapters.RuntimeNative:
		return session.Launch{}, api.NewError(api.CodeAdapterMissing,
			"the dotnet adapter manifest "+m.Name+" is not a native executable", "see 'eyedbg adapters ls'")
	}

	dbg, err := adapters.Find(m)
	if errors.Is(err, adapters.ErrNotInstalled) {
		return session.Launch{}, api.NewError(api.CodeAdapterMissing, m.Name+" is not installed", installHint(m))
	}

	if err != nil {
		return session.Launch{}, err
	}

	return session.Launch{
		Adapter:     dbg.Path,
		AdapterArgs: slices.Clone(m.Adapter.Args),
		AdapterEnv:  adapters.Environ(m),
		AdapterID:   m.Adapter.ID,
		// netcoredbg's exception filters (docs/adr/0010).
		ExceptionFilters: map[api.ExceptionMode][]string{
			api.ExceptionsAll:      {"all"},
			api.ExceptionsUncaught: {"user-unhandled"},
		},
		SideEffects: SideEffects,
		AttachHint:  attachHint,
	}, nil
}

// installHint says how to get m's adapter: "run 'eyedbg adapters install
// netcoredbg', or set EYEDBG_NETCOREDBG to an existing netcoredbg".
func installHint(m *adapters.Manifest) string {
	var ways []string

	if m.Install != nil {
		ways = append(ways, "run 'eyedbg adapters install "+m.Name+"'")
	}

	if m.Adapter.Env != "" {
		ways = append(ways, "set "+m.Adapter.Env+" to an existing "+m.Adapter.Entry)
	}

	if len(ways) == 0 {
		return "install " + m.Adapter.Entry + " (see 'eyedbg adapters ls')"
	}

	return strings.Join(ways, ", or ")
}

// launchArguments builds netcoredbg's launch request. netcoredbg starts
// "program" as a process: a .dll runs through the dotnet host, an apphost
// executable runs directly.
func launchArguments(host, program, cwd string, spec session.LaunchSpec) map[string]any {
	exe, args := host, append([]string{program}, spec.Args...)
	if !strings.EqualFold(filepath.Ext(program), ".dll") {
		exe, args = program, spec.Args
	}

	launchArgs := map[string]any{
		"name":        "eyedbg",
		"type":        adapterType,
		"request":     "launch",
		"program":     exe,
		"args":        args,
		"cwd":         cwd,
		"stopAtEntry": spec.StopOnEntry,
		"justMyCode":  true,
	}

	if len(spec.Env) > 0 {
		launchArgs["env"] = spec.Env
	}

	return launchArgs
}

// findProject resolves path (a project file or a directory holding exactly
// one) to a project file.
func findProject(path string) (string, error) {
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get working directory: %w", err)
		}

		path = wd
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", api.NewError(api.CodeInvalidRequest, "project "+path+" not found", "")
	}

	if !info.IsDir() {
		return path, nil
	}

	var found []string

	for _, pattern := range []string{"*.csproj", "*.fsproj", "*.vbproj"} {
		m, _ := filepath.Glob(filepath.Join(path, pattern))
		found = append(found, m...)
	}

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", api.NewError(api.CodeInvalidRequest, "no .NET project in "+path,
			"pass --project <file.csproj> or --program <app.dll>")
	default:
		return "", api.NewError(api.CodeInvalidRequest,
			fmt.Sprintf("%d projects in %s: %s", len(found), path, strings.Join(found, ", ")), "pick one with --project")
	}
}

// build runs dotnet build in Debug and returns the built program's path.
func build(ctx context.Context, host, project string) (string, error) {
	cmd := exec.CommandContext(ctx, host, "build", project, "-c", "Debug", "-nologo",
		"-getProperty:TargetPath", "-getTargetResult:Build")
	cmd.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1")

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	var result struct {
		Properties struct {
			TargetPath string `json:"TargetPath"` //nolint:tagliatelle // MSBuild's property name.
		} `json:"Properties"` //nolint:tagliatelle // MSBuild's JSON shape.
		TargetResults struct {
			Build struct {
				Result string `json:"Result"` //nolint:tagliatelle // MSBuild's JSON shape.
			} `json:"Build"` //nolint:tagliatelle // MSBuild's JSON shape.
		} `json:"TargetResults"` //nolint:tagliatelle // MSBuild's JSON shape.
	}

	jsonErr := json.Unmarshal(stdout.Bytes(), &result)
	if runErr != nil || jsonErr != nil || result.TargetResults.Build.Result != "Success" || result.Properties.TargetPath == "" {
		return "", api.NewError(api.CodeBuildFailed, "dotnet build "+project+" failed:\n"+
			tail(withoutResultJSON(stdout.String())+stderr.String(), buildOutputLines), "fix the build errors above, then start again")
	}

	return result.Properties.TargetPath, nil
}

// FindHost locates the dotnet host: $DOTNET_HOST_PATH, PATH, $DOTNET_ROOT,
// then ~/.dotnet.
func FindHost() (string, error) {
	name := "dotnet"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}

	if p := os.Getenv("DOTNET_HOST_PATH"); p != "" {
		return p, nil
	}

	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}

	var candidates []string
	if root := os.Getenv("DOTNET_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, name))
	}

	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".dotnet", name))
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil { //nolint:gosec // Candidates come from the user's environment and home.
			return c, nil
		}
	}

	return "", api.NewError(api.CodeAdapterMissing, "the dotnet host was not found",
		"install the .NET SDK, or put dotnet on PATH / set DOTNET_ROOT for the daemon (restart it with 'eyedbg daemon stop')")
}

// withoutResultJSON drops the leading -getProperty/-getTargetResult JSON
// document from dotnet build's output, keeping the diagnostics after it.
func withoutResultJSON(out string) string {
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		return out
	}

	if _, rest, ok := strings.Cut(out, "\n}\n"); ok {
		return rest
	}

	return out
}

// tail returns the last n non-empty lines of s.
func tail(s string, n int) string {
	var lines []string

	for l := range strings.SplitSeq(strings.TrimRight(s, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return strings.Join(lines, "\n")
}
