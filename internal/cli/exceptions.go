// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

func newExceptionsCommand(info version.Info, g *globals) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "exceptions [all|uncaught|none]",
		Short: "Show or choose which exceptions stop the program",
		Long: `Choose which thrown exceptions stop the program for you: all (every exception when it is
thrown, even one that is caught), uncaught (one that leaves your code without being caught), or
none. Without an argument, shows every client's choice and the adapter's exception filters that
are on.

Each client has its own choice (--as), and the program stops for the union of them: your "none"
doesn't turn off another client's "all". --force sets the mode for every client instead (so
exactly that mode applies; 'none --force' turns every one off). Needs no lease. 'eyedbg start',
'attach' and 'test' take --exceptions to choose before the program runs.

For dotnet (netcoredbg): all is the adapter's "all" filter, uncaught its "user-unhandled" (Just My
Code); an exception that would end the program always stops it, even with none. For python
(debugpy): all is its "raised" and "uncaught" filters (it stops in every frame the exception
passes through, then once more if nothing catches it); uncaught is "uncaught" and "userUnhandled"
(one stop, where it leaves your code; frame 0 is where it was raised); with none an uncaught
exception ends the program with its traceback in the output. Other adapters (including c, cpp and
rust's lldb-dap) are checked the same way, by the filters their manifest declares; a mode with no
filter for the adapter is UNSUPPORTED_BY_ADAPTER (exit 4), naming the filters it has.

At an exception stop, 'eyedbg status' shows the exception's type, message, first stack lines and
inner exceptions; 'eyedbg eval $exception' has the rest. Returns at once.` + sessionHelp,
		Example: `  eyedbg bp exceptions             # show
  eyedbg bp exceptions all
  eyedbg bp exceptions none --force  # every client's off`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := api.ExceptionsParams{SessionRef: g.ref(), Force: force}

			if len(args) == 1 {
				mode, err := api.ParseExceptionMode(args[0])
				if err != nil {
					return err
				}

				p.Mode = mode
			}

			var res api.ExceptionsResult
			if err := call(cmd, info, daemonCallTimeout, api.MethodBreakpointExceptions, p, &res); err != nil {
				return err
			}

			return writeExceptions(cmd.OutOrStdout(), res, g.json)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "set the mode for every client, not just you")

	return cmd
}

// writeExceptions shows the clients' exception modes and the filters on.
func writeExceptions(w io.Writer, res api.ExceptionsResult, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema               int `json:"schema"`
			api.ExceptionsResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, res})
	}

	if len(res.Modes) == 0 {
		return writeText(w, "exceptions: none\n")
	}

	modes := make([]string, len(res.Modes))
	for i, m := range res.Modes {
		modes[i] = string(m.Mode) + " (" + m.Client + ")"
	}

	s := "exceptions: " + strings.Join(modes, ", ") + "\n"
	if len(res.Filters) > 0 {
		s += "adapter filters: " + strings.Join(res.Filters, ", ") + "\n"
	}

	return writeText(w, s)
}
