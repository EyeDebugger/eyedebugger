// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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

// dotnetConfig is the build configuration eyedbg always builds and runs
// tests in: Debug, never Release, so symbols and unoptimized code stay
// available to the debugger.
const dotnetConfig = "Debug"

// msbuildNoLogo suppresses the SDK's banner on 'dotnet build'/'dotnet
// msbuild' (distinct from VSTest's own '--nologo').
const msbuildNoLogo = "-nologo"

// dotnetBuild is the dotnet command that builds a project.
const dotnetBuild = "build"

// Names netcoredbg and the CLI know .NET by.
const (
	lang        = "dotnet"
	adapterType = "coreclr"
)

// Language is the language name the .NET driver serves.
const Language = lang

// SharpDbg is the name of the .NET driver's alternative adapter's manifest
// (internal/adapters/manifests/sharpdbg.json; docs/adr/0017).
const SharpDbg = "sharpdbg"

// netcoredbgName is the manifest name serving dotnet natively.
const netcoredbgName = "netcoredbg"

// ExitCodeUnknown is why adapter's exit codes for goos can't be trusted, or
// "" where they can. netcoredbg (verified: 3.2.0-1092, osx-arm64) reports
// every launched or attached program's exit code as 0 on macOS, any arch.
// SharpDbg 0.1.17, on every OS, sends exitCode 0 whenever the exit code
// isn't known yet when the runtime reports the exit (a race: seen in CI on
// launched test apps that exited 1 or 2) and always for an attached program.
// Re-check this on a netcoredbg or SharpDbg manifest bump (docs/DESIGN.md §8).
func ExitCodeUnknown(adapter, goos string) string {
	switch {
	case adapter == netcoredbgName && goos == "darwin":
		return "netcoredbg on macOS reports every program's exit code as 0"
	case adapter == SharpDbg:
		return "SharpDbg 0.1.17 reports an exit code of 0 when it misses the real one"
	}

	return ""
}

// sharpdbgPauseHint is the hint of a pause refused under SharpDbg, whose
// pause (0.1.17) stops the program without a stopped event.
const sharpdbgPauseHint = "set a breakpoint and continue to it, or see where the threads are without stopping them with " +
	"'eyedbg dotnet threads'; where netcoredbg has a build, start with --adapter netcoredbg to pause"

// Driver debugs .NET programs (docs/DESIGN.md §8) with the adapter manifest
// serving dotnet (netcoredbg) or, chosen with --adapter or by default where
// netcoredbg has no build, SharpDbg's.
type Driver struct {
	// m serves dotnet; sharp is the manifest named SharpDbg (nil: none).
	m, sharp *adapters.Manifest
	// pick is the adapter WithAdapter chose ("": the default).
	pick string
	env  driverEnv
}

// driverEnv is what choosing and finding the adapter reads from the
// system; tests replace it.
type driverEnv struct {
	goos, goarch  string
	find          func(m *adapters.Manifest) (adapters.Location, error)
	resolveDotnet func(ctx context.Context, m *adapters.Manifest) (adapters.DotnetRuntime, error)
}

func systemDriverEnv() driverEnv {
	return driverEnv{goos: runtime.GOOS, goarch: runtime.GOARCH, find: adapters.Find, resolveDotnet: adapters.ResolveDotnet}
}

// New returns the .NET driver with the manifests of the default registry
// (bundled and user manifests).
func New() *Driver { return NewFrom(adapters.Default(lang)) }

// NewFrom returns the .NET driver with reg's manifest serving dotnet and
// its manifest named SharpDbg.
func NewFrom(reg *adapters.Registry) *Driver {
	return &Driver{m: reg.Language(lang), sharp: reg.Adapter(SharpDbg), env: systemDriverEnv()}
}

// NewWith returns the .NET driver for adapter manifest m alone (nil: none,
// so every start fails with ADAPTER_NOT_INSTALLED).
func NewWith(m *adapters.Manifest) *Driver { return &Driver{m: m, env: systemDriverEnv()} }

// Name implements session.Driver.
func (*Driver) Name() string { return lang }

// WithAdapter implements session.AdapterSelector: the driver bound to the
// adapter named name, the manifest serving dotnet or SharpDbg's.
func (d *Driver) WithAdapter(name string) (session.Driver, error) {
	if d.manifest(name) == nil {
		names := d.adapterNames()
		if len(names) == 0 {
			names = []string{"none"}
		}

		return nil, api.NewError(api.CodeInvalidRequest,
			"dotnet has no adapter "+name+" (its adapters: "+strings.Join(names, ", ")+")",
			"fix --adapter, or EYEDBG_DOTNET_ADAPTER if it chose it (see 'eyedbg adapters ls')")
	}

	c := *d
	c.pick = name

	return &c, nil
}

// manifest is the driver's manifest named name, or nil.
func (d *Driver) manifest(name string) *adapters.Manifest {
	for _, m := range []*adapters.Manifest{d.m, d.sharp} {
		if m != nil && m.Name == name {
			return m
		}
	}

	return nil
}

// adapterNames are the names --adapter takes.
func (d *Driver) adapterNames() []string {
	var names []string

	for _, m := range []*adapters.Manifest{d.m, d.sharp} {
		if m != nil && !slices.Contains(names, m.Name) {
			names = append(names, m.Name)
		}
	}

	return names
}

// Prepare implements session.Driver: it builds the project (unless a program
// is given) and launches the result under the adapter via the dotnet host.
func (d *Driver) Prepare(ctx context.Context, spec session.LaunchSpec) (session.Launch, error) {
	return d.PrepareWith(ctx, spec, session.PrepareOptions{})
}

// PrepareWith implements session.OptionsPreparer: Prepare, with the build's
// output streamed to opts.Output when it is set (see buildStreamed).
func (d *Driver) PrepareWith(ctx context.Context, spec session.LaunchSpec, opts session.PrepareOptions) (session.Launch, error) {
	if len(spec.Options) > 0 {
		return session.Launch{}, api.NewError(api.CodeInvalidRequest, "dotnet takes no --opt options", "see 'eyedbg help start'")
	}

	launch, err := d.adapterLaunch(ctx)
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

		program, err = buildProject(ctx, host, project, opts.Output)
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

// Adapter returns the name of the adapter a session of d would use (the
// chosen one, else the default here), or why none can run.
func (d *Driver) Adapter(ctx context.Context) (string, error) {
	launch, err := d.adapterLaunch(ctx)

	return launch.AdapterName, err
}

// adapterLaunch returns the Launch fields every session with the chosen
// adapter shares: the one WithAdapter picked, else SharpDbg where it is the
// default (autoDefault), else the manifest serving dotnet.
func (d *Driver) adapterLaunch(ctx context.Context) (session.Launch, error) {
	if d.pick != "" {
		return d.launchWith(ctx, d.manifest(d.pick))
	}

	if launch, ok, err := d.autoDefault(ctx); ok {
		return launch, err
	}

	if d.m == nil {
		return session.Launch{}, api.NewError(api.CodeAdapterMissing, "no adapter manifest serves dotnet",
			"see 'eyedbg adapters ls': a user manifest may have replaced netcoredbg's or failed to load")
	}

	return d.launchWith(ctx, d.m)
}

// autoDefault chooses SharpDbg when it is the only adapter that can run
// here: netcoredbg (the manifest serving dotnet, native) has no build for
// this platform and is not otherwise found (EYEDBG_NETCOREDBG, installed,
// PATH), SharpDbg is installed (nothing is ever downloaded unasked), and CI
// has not withheld the platform (sharpdbgWithheld). ok reports whether it
// chose SharpDbg: then err is SharpDbg's failure, if any (it is installed,
// but can't run).
func (d *Driver) autoDefault(ctx context.Context) (launch session.Launch, ok bool, err error) {
	if d.m == nil || d.sharp == nil || d.m == d.sharp || d.m.Adapter.Runtime != adapters.RuntimeNative {
		return session.Launch{}, false, nil
	}

	if _, has := d.m.DownloadFor(d.env.goos, d.env.goarch); has || d.withheld() != "" {
		return session.Launch{}, false, nil
	}

	if _, err := d.env.find(d.m); !errors.Is(err, adapters.ErrNotInstalled) {
		return session.Launch{}, false, nil // found, or broken: netcoredbg's own path says so
	}

	launch, err = d.launchWith(ctx, d.sharp)
	if errors.Is(err, adapters.ErrNotInstalled) {
		return session.Launch{}, false, nil
	}

	return launch, true, err
}

// withheld is why this platform doesn't default to an installed SharpDbg
// ("" when it does).
func (d *Driver) withheld() string {
	return SharpDbgWithheld(d.env.goos + "/" + d.env.goarch)
}

// SharpDbgWithheld says why sessions on platform ("os/arch") don't default
// to an installed SharpDbg although netcoredbg has no build there: CI's
// SharpDbg e2e fails there (--adapter sharpdbg still chooses it, unverified).
// It is "" where CI passes (darwin/amd64; docs/adr/0017).
func SharpDbgWithheld(platform string) string {
	withheld := map[string]string{
		"windows/arm64": "CI's SharpDbg e2e on windows-11-arm fails intermittently " +
			"(start misses the first breakpoint: the program runs on, or to exit)",
	}

	return withheld[platform]
}

// launchWith returns the Launch fields every session with adapter m shares:
// its process (a native executable, or a .dll on the dotnet host), and the
// .NET settings, with SharpDbg's own (docs/adr/0017).
func (d *Driver) launchWith(ctx context.Context, m *adapters.Manifest) (session.Launch, error) {
	var launch session.Launch

	switch m.Adapter.Runtime {
	case adapters.RuntimeNative:
		dbg, err := d.env.find(m)
		if errors.Is(err, adapters.ErrNotInstalled) {
			return session.Launch{}, api.NewError(api.CodeAdapterMissing, m.Name+" is not installed", d.installHint(m))
		}

		if err != nil {
			return session.Launch{}, err
		}

		launch.Adapter, launch.AdapterArgs = dbg.Path, slices.Clone(m.Adapter.Args)
	case adapters.RuntimeDotnet:
		rt, err := d.env.resolveDotnet(ctx, m)
		if err != nil {
			return session.Launch{}, err
		}

		launch.Adapter, launch.AdapterArgs = rt.Host, append([]string{rt.Entry}, m.Adapter.Args...)
	default:
		return session.Launch{}, api.NewError(api.CodeAdapterMissing,
			"the dotnet adapter manifest "+m.Name+" runs on "+m.Adapter.Runtime+", which the .NET driver can't start",
			"see 'eyedbg adapters ls'")
	}

	launch.AdapterEnv = adapters.Environ(m)
	launch.AdapterID = m.Adapter.ID
	launch.AdapterName = m.Name
	// Both adapters' exception filters (docs/adr/0010, 0017).
	launch.ExceptionFilters = map[api.ExceptionMode][]string{
		api.ExceptionsAll:      {"all"},
		api.ExceptionsUncaught: {"user-unhandled"},
	}
	launch.SideEffects = SideEffects
	launch.AttachHint = attachHint
	launch.ExitCodeUnknown = ExitCodeUnknown(m.Name, d.env.goos)

	if m.Name == SharpDbg {
		launch.PauseUnsupported = sharpdbgPauseHint
		launch.SetByEval = true
	}

	return launch, nil
}

// installHint says how to get native adapter m: "run 'eyedbg adapters
// install netcoredbg', or set EYEDBG_NETCOREDBG to an existing
// netcoredbg"; where it has no build, SharpDbg first.
func (d *Driver) installHint(m *adapters.Manifest) string {
	hint := installHint(m)
	if _, has := m.DownloadFor(d.env.goos, d.env.goarch); has || m != d.m || d.sharp == nil || d.sharp == m {
		return hint
	}

	platform := d.env.goos + "/" + d.env.goarch
	if m.Adapter.Env != "" {
		hint = "set " + m.Adapter.Env + " to your own " + m.Adapter.Entry + " build"
	}

	if reason := d.withheld(); reason != "" {
		return m.Name + " has no build for " + platform + ": run 'eyedbg adapters install " + d.sharp.Name + "' and start with --adapter " +
			d.sharp.Name + " (not verified here: " + reason + "), or " + hint
	}

	return m.Name + " has no build for " + platform + ": run 'eyedbg adapters install " + d.sharp.Name +
		"' (sessions here then use it unless --adapter or EYEDBG_DOTNET_ADAPTER picks " + m.Name + "), or " + hint
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

// launchArguments builds the launch request (netcoredbg's; SharpDbg takes
// the same). The adapter starts "program" as a process: a .dll runs through
// the dotnet host, an apphost executable runs directly.
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
	args := []string{dotnetBuild, project, "-c", dotnetConfig, msbuildNoLogo, "-getProperty:" + propTargetPath, "-getTargetResult:Build"}

	props, err := buildQuery(ctx, host, project, args, true, propTargetPath)
	if err != nil {
		return "", err
	}

	return props[propTargetPath], nil
}

// buildProject builds project with its output streamed to out, or, out
// nil, kept for a failure's error (build).
func buildProject(ctx context.Context, host, project string, out io.Writer) (string, error) {
	if out != nil {
		return buildStreamed(ctx, host, project, out)
	}

	return build(ctx, host, project)
}

// buildStreamed builds project in Debug as build does, with the build's
// output (stdout and stderr, as the build interleaves them) streamed to out
// instead of kept: 'dotnet build -getProperty' prints no build log, so the
// build runs plainly, then 'dotnet msbuild -getProperty' names what it built
// (as build's query does). A failed build's error doesn't repeat its
// output, which out has had. out is not written to once buildStreamed
// returns.
func buildStreamed(ctx context.Context, host, project string, out io.Writer) (string, error) {
	cmd := buildCommand(ctx, host, streamedBuildArgs(project))
	// One writer for both: os/exec then copies them from one goroutine, so
	// out sees one write at a time, in the order the build made them.
	cmd.Stdout, cmd.Stderr = out, out

	if err := cmd.Run(); err != nil {
		return "", streamedBuildErr(ctx, project, err)
	}

	return queryTargetPath(ctx, host, project)
}

// queryTargetPath asks msbuild for the path of project's Debug build. Asked
// for one property, msbuild prints its bare value (verified: SDK 10.0.302),
// not the JSON document of several; JSON is still read if it comes. An
// empty or multi-line answer is BUILD_FAILED, as an empty TargetPath is for
// build.
func queryTargetPath(ctx context.Context, host, project string) (string, error) {
	args := targetPathArgs(project)
	cmd := buildCommand(ctx, host, args)

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	runErr := cmd.Run()
	path, ok := parseTargetPath(stdout.Bytes())

	if runErr != nil || !ok {
		return "", api.NewError(api.CodeBuildFailed, "dotnet "+args[0]+" "+project+" failed:\n"+
			tail(stdout.String()+stderr.String(), buildOutputLines), "fix the build errors above, then start again")
	}

	return path, nil
}

// parseTargetPath reads queryTargetPath's answer: one line, the path (or
// the JSON document holding it); ok is false for none, or more than one
// line.
func parseTargetPath(out []byte) (string, bool) {
	path := strings.TrimSpace(string(out))

	if props, ok := parseBuildProperties(out, false); ok {
		path = props[propTargetPath]
	}

	return path, path != "" && !strings.ContainsAny(path, "\r\n")
}

// streamedBuildArgs is buildStreamed's build: Debug, no banner, and the
// classic logger, whose lines read as plain text outside a terminal.
func streamedBuildArgs(project string) []string {
	return []string{dotnetBuild, project, "-c", dotnetConfig, msbuildNoLogo, "-tl:off"}
}

// targetPathArgs asks msbuild, without building, for the path of project's
// Debug build.
func targetPathArgs(project string) []string {
	return []string{"msbuild", project, msbuildNoLogo, "-getProperty:" + propTargetPath, "-p:Configuration=" + dotnetConfig}
}

// streamedBuildErr is the error of buildStreamed's build that failed with
// err: the context's end, BUILD_FAILED pointing at the output already
// streamed when the build ran and failed, else why it didn't run.
func streamedBuildErr(ctx context.Context, project string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("dotnet build %s: %w", project, ctxErr)
	}

	if _, exited := errors.AsType[*exec.ExitError](err); exited {
		return api.NewError(api.CodeBuildFailed, "dotnet build "+project+" failed (its output is above)",
			"fix the build errors above, then start again")
	}

	return api.NewError(api.CodeBuildFailed, "dotnet build "+project+" failed: "+err.Error(), "check the .NET SDK ('dotnet --info')")
}

// FindHost locates the dotnet host: $DOTNET_HOST_PATH, PATH, $DOTNET_ROOT,
// then ~/.dotnet (adapters.FindDotnetHost).
func FindHost() (string, error) { return adapters.FindDotnetHost() }

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
