// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// netCoreApp is the shared framework every .NET (Core) program runs on.
const netCoreApp = "Microsoft.NETCore.App"

// DotnetRuntime is how a .NET-hosted adapter runs: its .dll on the dotnet
// host.
type DotnetRuntime struct {
	// Host is the dotnet host (FindDotnetHost).
	Host string
	// Entry is the adapter's .dll.
	Entry string
	// Source is FoundEnv or FoundInstalled.
	Source string
	// Runtime is the newest Microsoft.NETCore.App version the host has
	// that is at least the manifest's minRuntime.
	Runtime string
}

// dotnetEnv is what host lookup and resolution read from the system; tests
// replace it.
type dotnetEnv struct {
	goos        string
	getenv      func(string) string
	lookPath    func(string) (string, error)
	stat        func(string) (fs.FileInfo, error)
	userHomeDir func() (string, error)
	dataDir     func() (string, error)
	// listRuntimes runs 'host --list-runtimes' in dir.
	listRuntimes func(ctx context.Context, host, dir string) (string, error)
}

func systemDotnetEnv() dotnetEnv {
	return dotnetEnv{
		goos: runtime.GOOS, getenv: os.Getenv, lookPath: exec.LookPath, stat: os.Stat,
		userHomeDir: os.UserHomeDir, dataDir: DataDir, listRuntimes: listRuntimes,
	}
}

// FindDotnetHost locates the dotnet host: $DOTNET_HOST_PATH, PATH,
// $DOTNET_ROOT, then ~/.dotnet.
func FindDotnetHost() (string, error) { return systemDotnetEnv().findHost() }

func (e dotnetEnv) findHost() (string, error) {
	name := "dotnet"
	if e.goos == goosWindows {
		name += ".exe"
	}

	if p := e.getenv("DOTNET_HOST_PATH"); p != "" {
		return p, nil
	}

	if p, err := e.lookPath(name); err == nil {
		return p, nil
	}

	var candidates []string
	if root := e.getenv("DOTNET_ROOT"); root != "" {
		candidates = append(candidates, filepath.Join(root, name))
	}

	if home, err := e.userHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".dotnet", name))
	}

	for _, c := range candidates {
		if _, err := e.stat(c); err == nil {
			return c, nil
		}
	}

	return "", api.NewError(api.CodeAdapterMissing, "the dotnet host was not found",
		"install the .NET SDK, or put dotnet on PATH / set DOTNET_ROOT for the daemon (restart it with 'eyedbg daemon stop')")
}

// ResolveDotnet finds how to run .NET adapter m: its .dll (the manifest's
// environment variable, else the installed copy), the dotnet host, and a
// runtime on it new enough for m (checked with 'dotnet --list-runtimes',
// which runs no project code). A missing .dll is ErrNotInstalled; every
// failure is an *api.Error.
func ResolveDotnet(ctx context.Context, m *Manifest) (DotnetRuntime, error) {
	return systemDotnetEnv().resolve(ctx, m)
}

func (e dotnetEnv) resolve(ctx context.Context, m *Manifest) (DotnetRuntime, error) {
	if m.Adapter.Runtime != RuntimeDotnet || m.Dotnet == nil {
		return DotnetRuntime{}, fmt.Errorf("%s is not a dotnet adapter", m.Name)
	}

	entry, source, err := e.entry(m)
	if err != nil {
		return DotnetRuntime{}, err
	}

	host, err := e.findHost()
	if err != nil {
		return DotnetRuntime{}, err
	}

	out, err := e.listRuntimes(ctx, host, filepath.Dir(entry))
	if err != nil {
		return DotnetRuntime{}, api.NewError(api.CodeAdapterMissing,
			fmt.Sprintf("%s --list-runtimes failed: %v", host, err), "repair the .NET install (see 'eyedbg adapters doctor "+m.Name+"')")
	}

	versions := netCoreVersions(out)

	best := newestAtLeast(versions, m.Dotnet.MinRuntime)
	if best == "" {
		found := "none"
		if len(versions) > 0 {
			found = strings.Join(versions, ", ")
		}

		return DotnetRuntime{}, api.NewError(api.CodeAdapterMissing,
			fmt.Sprintf("%s %s needs the .NET %s+ runtime, and %s has %s %s", m.Name, m.Version, m.Dotnet.MinRuntime, host, netCoreApp, found),
			"install the .NET "+m.Dotnet.MinRuntime+" runtime or SDK, or point the daemon at a dotnet that has it (DOTNET_ROOT, then 'eyedbg daemon stop' to restart it)")
	}

	return DotnetRuntime{Host: host, Entry: entry, Source: source, Runtime: best}, nil
}

// entry finds m's .dll: the environment variable's, else the installed
// one.
func (e dotnetEnv) entry(m *Manifest) (dll, source string, err error) {
	if m.Adapter.Env != "" {
		if p := e.getenv(m.Adapter.Env); p != "" {
			if err := e.checkEntry(p); err != nil {
				return "", "", api.NewError(api.CodeAdapterMissing, fmt.Sprintf("%s=%s: %v", m.Adapter.Env, p, err),
					"fix or unset "+m.Adapter.Env+" (the daemon reads it when it starts: 'eyedbg daemon stop' to restart it)")
			}

			return p, FoundEnv, nil
		}
	}

	if data, err := e.dataDir(); err == nil {
		p := installedEntry(m, filepath.Join(data, m.Name, m.Version))
		if info, err := e.stat(p); err == nil && info.Mode().IsRegular() {
			return p, FoundInstalled, nil
		}
	}

	return "", "", notInstalled(api.NewError(api.CodeAdapterMissing, m.Name+" is not installed", dotnetInstallHint(m)))
}

// checkEntry checks that p is an existing .dll file.
func (e dotnetEnv) checkEntry(p string) error {
	if !strings.EqualFold(filepath.Ext(p), ".dll") {
		return errors.New("not a .dll")
	}

	info, err := e.stat(p)
	if err != nil {
		return fmt.Errorf("not found: %w", err)
	}

	if !info.Mode().IsRegular() {
		return errors.New("not a file")
	}

	return nil
}

// dotnetInstallHint says how to get .NET adapter m.
func dotnetInstallHint(m *Manifest) string {
	var ways []string

	if m.Install != nil {
		ways = append(ways, fmt.Sprintf("run 'eyedbg adapters install %s' (downloads %s %s once)", m.Name, m.Name, m.Version))
	}

	if m.Adapter.Env != "" {
		ways = append(ways, "set "+m.Adapter.Env+" to its "+path.Base(m.Adapter.Entry)+" for the daemon")
	}

	if len(ways) == 0 {
		return "see 'eyedbg adapters ls'"
	}

	return strings.Join(ways, ", or ")
}

// netCoreVersions are the Microsoft.NETCore.App versions in 'dotnet
// --list-runtimes' output (lines like "Microsoft.NETCore.App 10.0.10
// [/usr/share/dotnet/shared/Microsoft.NETCore.App]"), in order, without
// duplicates.
func netCoreVersions(out string) []string {
	var versions []string

	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != netCoreApp {
			continue
		}

		if _, ok := majorMinor(fields[1]); ok && !slices.Contains(versions, fields[1]) {
			versions = append(versions, fields[1])
		}
	}

	return versions
}

// newestAtLeast is the newest of versions (X.Y.Z, maybe with a
// pre-release suffix) at least minimum ("X.Y"), "" for none. A pre-release
// counts: the host rolls forward to one when no release fits.
func newestAtLeast(versions []string, minimum string) string {
	best := ""

	for _, v := range versions {
		if versionAtLeast(v, minimum) && (best == "" || compareVersions(v, best) > 0) {
			best = v
		}
	}

	return best
}

// compareVersions orders X.Y.Z[-suffix] versions by their numbers, a
// release after its pre-releases.
func compareVersions(a, b string) int {
	an, apre := splitVersion(a)
	bn, bpre := splitVersion(b)

	if c := slices.Compare(an, bn); c != 0 {
		return c
	}

	switch {
	case apre == bpre:
		return 0
	case apre == "":
		return 1
	case bpre == "":
		return -1
	default:
		return strings.Compare(apre, bpre)
	}
}

// splitVersion splits X.Y.Z[-suffix] into its numbers (missing or bad ones
// are 0) and its suffix.
func splitVersion(v string) (nums []int, pre string) {
	core, pre, _ := strings.Cut(v, "-")
	nums = make([]int, 3)

	for i, p := range strings.SplitN(core, ".", 3) {
		n := 0

		for _, r := range p {
			if r < '0' || r > '9' {
				break
			}

			n = n*10 + int(r-'0')
		}

		nums[i] = n
	}

	return nums, pre
}

// listRuntimes runs 'host --list-runtimes' in dir: a host command that
// reads no project and runs no program code.
func listRuntimes(ctx context.Context, host, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, host, "--list-runtimes")
	cmd.Env = append(os.Environ(), "DOTNET_CLI_TELEMETRY_OPTOUT=1", "DOTNET_NOLOGO=1")

	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		cmd.Dir = dir
	}

	var stdout, stderr bytes.Buffer

	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, lastLine(stderr.String()))
	}

	return stdout.String(), nil
}
