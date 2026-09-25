// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// jsonSchemaVersion is the version of every --json output shape
// (docs/DESIGN.md §5). Bump it only for incompatible changes.
const jsonSchemaVersion = 1

// versionOutput is the --json wire shape of the version command. It is kept
// separate from version.Info so the output contract changes only on purpose.
type versionOutput struct {
	Schema    int    `json:"schema"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	// Protocol is the native API version it speaks (api.ProtocolVersion).
	Protocol int `json:"protocol"`
	// Features are optional capabilities of this build: "dap" (eyedbg dap
	// and the daemon's DAP facade).
	Features []string `json:"features"`
}

func newVersionCommand(binName string, info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Long: fmt.Sprintf(`Print %[1]s's build information: version, commit, build date, Go
toolchain version and platform.

Never blocks and has no effect on any debug session. Exits 0 on success, 1 on a usage error (e.g.
extra arguments) or a write error. Default output is text for LLM/human reading; --json prints the
machine-readable form with a stable "schema" field (docs/DESIGN.md §5) that only changes on
purpose, plus the native protocol version it speaks and its optional features ("dap": 'eyedbg
dap' and the daemon's DAP facade; "presence": connected editors in sessions; "lease.request":
'eyedbg lease request'; "dap.collab": the facade's eyedbg/* messages, other clients' breakpoints
in the editor and output replay; "dotnet.helper": 'eyedbg dotnet ps' and 'eyedbg dotnet counters',
run through the .NET side helper — which may still be missing at run time, HELPER_NOT_FOUND;
"dotnet.dump": 'eyedbg dotnet dump', 'eyedbg dotnet heap' and 'eyedbg dotnet threads', through the
same helper; "dotnet.trace": 'eyedbg dotnet trace', through the same helper).`, binName),
		Example: fmt.Sprintf("  %[1]s version\n  %[1]s version --json", binName),
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeVersion(cmd.OutOrStdout(), cmd.Root().Name(), info, g.json)
		},
	}
}

func writeVersion(w io.Writer, name string, info version.Info, asJSON bool) error {
	if asJSON {
		out := versionOutput{
			Schema:    jsonSchemaVersion,
			Name:      name,
			Version:   info.Version,
			Commit:    info.Commit,
			Date:      info.Date,
			GoVersion: info.GoVersion,
			Platform:  info.Platform,
			Protocol:  api.ProtocolVersion,
			Features:  []string{"dap", "presence", "lease.request", "dap.collab", "dotnet.helper", "dotnet.dump", "dotnet.trace"},
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			return fmt.Errorf("write version: %w", err)
		}

		return nil
	}

	_, err := fmt.Fprintf(w, "%s %s\ncommit: %s\nbuilt:  %s\ngo:     %s %s\n",
		name, info.Version, info.Commit, info.Date, info.GoVersion, info.Platform)
	if err != nil {
		return fmt.Errorf("write version: %w", err)
	}

	return nil
}
