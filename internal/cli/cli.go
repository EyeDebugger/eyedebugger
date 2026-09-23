// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package cli builds the command trees of the EyeDebugger binaries (eyedbg,
// eyedbgd). It owns flag parsing and output rendering; the logic behind each
// command lives in other internal packages.
//
// Every command is documented in place: Short, Long and Example are a tested
// artifact (docs/DESIGN.md §4, docs/CONVENTIONS.md § Help text), not an
// afterthought. Run `eyedbg <command> --help` or `eyedbg help <command>` for
// any command's own help; agents should treat that output as authoritative.
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

const eyedbgLong = `eyedbg is the CLI for EyeDebugger, an AI-native, CLI-first debugger built for
coding agents (and humans) driving the same live debug session.

The CLI is stateless: every invocation talks to a per-user daemon (eyedbgd) over local IPC, which
auto-starts on first use. There is no MCP server by default — this CLI, with its complete built-in
help, is the agent interface (docs/DESIGN.md §10).

Run 'eyedbg <command> --help' or 'eyedbg help <command>' for a command's own help: what it does,
when to use it, whether it blocks (and for how long), its effect on the debuggee, its output shape,
and its exit codes (docs/DESIGN.md §4).`

const eyedbgExample = `  eyedbg version           # build info for this binary
  eyedbg version --json    # machine-readable build info ("schema": 1)`

// NewEyedbgCommand returns the root command of the eyedbg CLI.
func NewEyedbgCommand(info version.Info) *cobra.Command {
	root, g := newRoot("eyedbg", "AI-native, CLI-first debugger", eyedbgLong, eyedbgExample, info)
	root.AddCommand(newVersionCommand("eyedbg", info, g))
	finalizeCommandTree(root)

	return root
}

const eyedbgdLong = `eyedbgd is the EyeDebugger daemon: the per-user process that owns live debug
sessions, clients, the control lease, breakpoint ownership, the event log and stop snapshots
(docs/DESIGN.md §6). It is not meant to be run by hand in normal use — eyedbg auto-starts it on
first use and talks to it over local IPC.

This binary is not implemented yet (docs/DESIGN.md §13, milestone 1).`

const eyedbgdExample = `  eyedbgd version           # build info for this binary`

// NewDaemonCommand returns the root command of the eyedbgd daemon.
func NewDaemonCommand(info version.Info) *cobra.Command {
	root, g := newRoot("eyedbgd", "EyeDebugger per-user daemon", eyedbgdLong, eyedbgdExample, info)
	root.RunE = notImplemented("the daemon (docs/DESIGN.md §13, milestone 1)")
	root.AddCommand(newVersionCommand("eyedbgd", info, g))
	// eyedbgd is never invoked interactively by a shell (docs/DESIGN.md §6), so shell
	// completion has no audience here; disabling it keeps eyedbgd --help short.
	root.CompletionOptions.DisableDefaultCmd = true
	finalizeCommandTree(root)

	return root
}

// finalizeCommandTree adds cobra's built-in "help" and "completion" commands
// up front instead of leaving them to be created lazily inside Execute, and
// gives each of them its own Example. Without this, a New*Command tree walked
// without ever calling Execute (as the tests below do) would miss commands
// that every real 'eyedbg --help' shows (docs/DESIGN.md §4). Call it once, as
// the last step of building a root command, after every other AddCommand.
func finalizeCommandTree(root *cobra.Command) {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	for _, sub := range root.Commands() {
		switch sub.Name() {
		case "help":
			sub.Example = fmt.Sprintf("  %[1]s help version   # same as: %[1]s version --help\n  %[1]s help           # same as: %[1]s --help", root.Name())
		case "completion":
			sub.Example = fmt.Sprintf("  %[1]s completion bash   # print the bash completion script (see the shell sub-commands' own help to install it)", root.Name())

			for _, shell := range sub.Commands() {
				shell.Example = completionExample(root.Name(), shell.Name())
			}
		}
	}
}

// completionExample returns a one-line install/use example for one of
// cobra's built-in "completion" shell sub-commands, or "" for a shell this
// scaffold doesn't know about yet (which TestAllCommandsHaveHelp then flags).
func completionExample(rootName, shellName string) string {
	switch shellName {
	case "bash":
		return fmt.Sprintf("  %[1]s completion bash > /etc/bash_completion.d/%[1]s", rootName)
	case "zsh":
		return fmt.Sprintf(`  %[1]s completion zsh > "${fpath[1]}/_%[1]s"`, rootName)
	case "fish":
		return fmt.Sprintf("  %[1]s completion fish > ~/.config/fish/completions/%[1]s.fish", rootName)
	case "powershell":
		return fmt.Sprintf("  %[1]s completion powershell | Out-String | Invoke-Expression", rootName)
	default:
		return ""
	}
}

func newRoot(name, short, long, example string, info version.Info) (*cobra.Command, *globals) {
	g := &globals{}
	root := &cobra.Command{
		Use:           name,
		Short:         short,
		Long:          long,
		Example:       example,
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
