// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package cli builds the command trees of the EyeDebugger binaries (eyedbg,
// eyedbgd). It owns flag parsing and output rendering; the logic behind each
// command lives in other internal packages.
//
// Only this package and cmd/ may import the CLI framework.
package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/version"
)

// errNotImplemented marks commands that exist in the scaffold but have no
// behavior yet.
var errNotImplemented = errors.New("not implemented yet")

// globals holds flags shared by every command of a binary (docs/DESIGN.md §4).
type globals struct {
	json bool
}

// Run executes root with args (without the program name) and returns the
// process exit code.
func Run(ctx context.Context, root *cobra.Command, args []string) int {
	if args == nil {
		args = []string{} // cobra falls back to os.Args[1:] when args is nil
	}

	root.SetArgs(args)

	if err := root.ExecuteContext(ctx); err != nil {
		// Nothing useful can be done if writing the error itself fails.
		_, _ = fmt.Fprintf(root.ErrOrStderr(), "%s: %v\n", root.Name(), err)

		return 1
	}

	return 0
}

// NewEyedbgCommand returns the root command of the eyedbg CLI.
func NewEyedbgCommand(info version.Info) *cobra.Command {
	root, g := newRoot("eyedbg", "AI-native, CLI-first debugger", info)
	root.AddCommand(newVersionCommand(info, g))

	return root
}

// NewDaemonCommand returns the root command of the eyedbgd daemon.
func NewDaemonCommand(info version.Info) *cobra.Command {
	root, g := newRoot("eyedbgd", "EyeDebugger per-user daemon", info)
	root.RunE = notImplemented("the daemon (docs/DESIGN.md §13, milestone 1)")
	root.AddCommand(newVersionCommand(info, g))

	return root
}

func newRoot(name, short string, info version.Info) (*cobra.Command, *globals) {
	g := &globals{}
	root := &cobra.Command{
		Use:           name,
		Short:         short,
		Version:       info.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "print machine-readable JSON output")

	return root, g
}

func notImplemented(what string) func(*cobra.Command, []string) error {
	return func(*cobra.Command, []string) error {
		return fmt.Errorf("%s: %w", what, errNotImplemented)
	}
}
