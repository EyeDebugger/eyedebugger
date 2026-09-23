// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
)

const (
	installTimeout = 5 * time.Minute
	probeTimeout   = 10 * time.Second

	adapterNetcoredbg = "netcoredbg"
	toolDotnet        = "dotnet"
)

func newAdaptersCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adapters",
		Short: "Install and check debug adapters",
		Long: `Install and check the debug adapters eyedbg drives (docs/DESIGN.md §7, §8). For dotnet that is
netcoredbg (Samsung, MIT), pinned to one release and verified by SHA-256 before use. Microsoft's
vsdbg is never used: its license restricts it to Microsoft's IDEs.

Adapters are installed per user under the cache directory (override with EYEDBG_DATA_DIR); set
EYEDBG_NETCOREDBG to use your own netcoredbg build instead.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`,
		Example: `  eyedbg adapters install netcoredbg
  eyedbg adapters doctor`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newAdaptersInstallCommand(g), newAdaptersDoctorCommand(g))

	return cmd
}

func newAdaptersInstallCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "install <adapter>",
		Short: "Download and install an adapter",
		Long: `Download the pinned release of an adapter for this platform, verify its SHA-256 against the
digest built into eyedbg, and extract it. Adapters: netcoredbg (` + adapters.NetcoredbgVersion + `; prebuilt for
linux x64/arm64, macOS arm64 and Windows x64).

Needs network access to github.com; blocks until done (typically seconds, at most 5m). Idempotent:
an installed adapter is left as is. Prints the installed path ("path" in --json). Exits 1 on
download, checksum or extraction failure; nothing half-installed is left behind.`,
		Example: `  eyedbg adapters install netcoredbg`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != adapterNetcoredbg {
				return fmt.Errorf("unknown adapter %q (available: netcoredbg)", args[0])
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), installTimeout)
			defer cancel()

			path, err := adapters.InstallNetcoredbg(ctx, http.DefaultClient)
			if err != nil {
				return err
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Schema  int    `json:"schema"`
					Adapter string `json:"adapter"`
					Version string `json:"version"`
					Path    string `json:"path"`
				}{jsonSchemaVersion, adapterNetcoredbg, adapters.NetcoredbgVersion, path})
			}

			return writeText(cmd.OutOrStdout(), fmt.Sprintf("%s %s installed at %s\n", adapterNetcoredbg, adapters.NetcoredbgVersion, path))
		},
	}
}

func newAdaptersDoctorCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that adapters and toolchains are usable",
		Long: `Check everything debugging needs on this machine and say what to fix: for dotnet, that
netcoredbg is found (and where from: EYEDBG_NETCOREDBG, installed, or PATH) and runs, and that the
dotnet host is found and reports its version.

Runs each tool once with --version (at most 10s each); changes nothing. Output: one line per check,
"ok" or "problem" with a fix ("checks" in --json). Exits 1 if any check has a problem.`,
		Example: `  eyedbg adapters doctor
  eyedbg adapters doctor --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := runDoctor(cmd.Context())

			if err := writeDoctor(cmd, checks, g.json); err != nil {
				return err
			}

			for _, c := range checks {
				if !c.OK {
					return errDoctorProblems
				}
			}

			return nil
		},
	}
}

var errDoctorProblems = errors.New("some checks have problems (see above)")

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func runDoctor(ctx context.Context) []doctorCheck {
	var checks []doctorCheck

	loc, err := adapters.FindNetcoredbg()

	switch {
	case errors.Is(err, adapters.ErrNotInstalled):
		checks = append(checks, doctorCheck{
			Name: adapterNetcoredbg, Detail: "not found",
			Fix: "run 'eyedbg adapters install netcoredbg' or set " + adapters.EnvNetcoredbg,
		})
	case err != nil:
		checks = append(checks, doctorCheck{Name: adapterNetcoredbg, Detail: err.Error(), Fix: "fix or unset " + adapters.EnvNetcoredbg})
	default:
		out, err := probe(ctx, loc.Path, "--version")
		if err != nil {
			checks = append(checks, doctorCheck{
				Name: adapterNetcoredbg, Detail: loc.Path + " does not run: " + err.Error(),
				Fix: "reinstall it, or check that the .NET runtime it needs is present",
			})
		} else {
			checks = append(checks, doctorCheck{
				Name: adapterNetcoredbg, OK: true,
				Detail: fmt.Sprintf("%s (%s, %s)", firstLine(out), loc.Source, loc.Path),
			})
		}
	}

	host, err := dotnet.FindHost()
	if err != nil {
		checks = append(checks, doctorCheck{
			Name: toolDotnet, Detail: "the dotnet host was not found",
			Fix: "install the .NET SDK and put dotnet on PATH or set DOTNET_ROOT (the daemon uses the environment of the eyedbg that started it)",
		})
	} else if out, err := probe(ctx, host, "--version"); err != nil {
		checks = append(checks, doctorCheck{Name: toolDotnet, Detail: host + " does not run: " + err.Error(), Fix: "repair the .NET SDK install"})
	} else {
		checks = append(checks, doctorCheck{Name: toolDotnet, OK: true, Detail: fmt.Sprintf("SDK %s (%s)", firstLine(out), host)})
	}

	return checks
}

func probe(ctx context.Context, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, exe, args...).CombinedOutput()

	return string(out), err
}

func writeDoctor(cmd *cobra.Command, checks []doctorCheck, asJSON bool) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), struct {
			Schema int           `json:"schema"`
			Checks []doctorCheck `json:"checks"`
		}{jsonSchemaVersion, checks})
	}

	var b strings.Builder

	for _, c := range checks {
		if c.OK {
			fmt.Fprintf(&b, "ok       %s: %s\n", c.Name, c.Detail)
		} else {
			fmt.Fprintf(&b, "problem  %s: %s\n         fix: %s\n", c.Name, c.Detail, c.Fix)
		}
	}

	return writeText(cmd.OutOrStdout(), b.String())
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(line)
}
