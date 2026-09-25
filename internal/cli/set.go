// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

func newSetCommand(info version.Info, g *globals) *cobra.Command {
	var frame int

	cmd := &cobra.Command{
		Use:   "set <variable> <value>",
		Short: "Change a variable of the stopped program",
		Long: `Assign VALUE to VARIABLE in a frame of the stopped thread (default: #0) and print its new value.
VARIABLE is a local or a member path like order.Total or items[0]; VALUE is an expression in the
program's language, evaluated there (e.g. 100, "text", total * 2). Setting a property calls its
setter. The next --dump changed (or 'eyedbg vars --changed') shows the change.

It changes the program, so it is an execution request: it needs the control lease (see below) and
is logged in 'eyedbg events' (the variable's name, not the value). It uses the adapter's
setExpression (netcoredbg) or setVariable; SharpDbg (dotnet --adapter sharpdbg) has neither, so
there set evaluates "VARIABLE = VALUE" instead, and VARIABLE must be a plain name or member path
(names, [N] and ["key"] indexes; anything else is INVALID_REQUEST, exit 1). Another adapter with
neither is UNSUPPORTED_BY_ADAPTER (exit 4); a value the adapter can't assign is ADAPTER_ERROR (exit
4). Needs a stopped program; returns at once.` + leaseHelp + sessionHelp,
		Example: `  eyedbg set total 100
  eyedbg set order.Status '"Paid"'
  eyedbg set i 0 --frame 1 --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res api.SetResult
			if err := call(cmd, info, daemonCallTimeout, api.MethodSet,
				api.SetParams{SessionRef: g.ref(), Variable: args[0], Value: args[1], Frame: frame}, &res); err != nil {
				return err
			}

			return writeSet(cmd.OutOrStdout(), res, g.json)
		},
	}

	cmd.Flags().IntVar(&frame, "frame", 0, "frame number from 'eyedbg stack' (0 = innermost)")

	return cmd
}

// writeSet shows a variable's new value: "total = 100  (int)".
func writeSet(w io.Writer, res api.SetResult, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema        int `json:"schema"`
			api.SetResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, res})
	}

	s := res.Variable + " = " + res.Value
	if res.Type != "" {
		s += "  (" + res.Type + ")"
	}

	return writeText(w, s+"\n")
}
