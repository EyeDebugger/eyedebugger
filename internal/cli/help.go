// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// installHelp makes help honor --json and replaces cobra's help command with
// one that can print the whole tree (--all).
func installHelp(root *cobra.Command, g *globals) {
	textHelp := root.HelpFunc()

	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if !g.json {
			textHelp(cmd, args)

			return
		}

		// A help func can't return an error; a failed write has nowhere
		// better to go.
		_ = writeJSON(cmd.OutOrStdout(), helpOutput{Schema: jsonSchemaVersion, Command: newHelpDoc(cmd, false)})
	})

	var all bool

	help := &cobra.Command{
		Use:   "help [command...] [--all]",
		Short: "Show help for any command, or for all of them",
		Long: fmt.Sprintf(`Show a command's help: what it does, when to use it, whether it blocks, its effect on the
program, its output and exit codes. Same as '%[1]s <command> --help'.

--all prints the help of the command and of every command under it, in one read (for the root:
the whole CLI). --json prints the help as JSON instead: {"schema", "command": {"path", "usage",
"short", "long", "example", "flags", "inheritedFlags", "commands"}}, where each flag has "name",
"shorthand", "type", "default" and "usage"; with --all, "commands" holds the full help of every
subcommand, recursively, and inherited flags are listed once, at the top.

Exits 1 for an unknown command.`, root.Name()),
		Example: fmt.Sprintf(`  %[1]s help version         # same as: %[1]s version --help
  %[1]s help --all           # the whole CLI in one read
  %[1]s help --all --json    # the same, as JSON`, root.Name()),
		RunE: func(cmd *cobra.Command, args []string) error {
			target, rest, err := cmd.Root().Find(args)
			if err != nil || len(rest) > 0 {
				return fmt.Errorf("unknown help topic %q (see '%s help')", strings.Join(args, " "), cmd.Root().Name())
			}

			if !all {
				return target.Help()
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), helpOutput{Schema: jsonSchemaVersion, Command: newHelpDoc(target, true)})
			}

			return writeHelpTree(cmd, target, textHelp)
		},
	}
	help.Flags().BoolVar(&all, "all", false, "also show every command under it (the whole CLI for the root)")

	root.SetHelpCommand(help)
}

// writeHelpTree prints the text help of cmd and every command under it.
func writeHelpTree(out, cmd *cobra.Command, textHelp func(*cobra.Command, []string)) error {
	rule := strings.Repeat("=", 80) + "\n"

	var walk func(c *cobra.Command) error

	walk = func(c *cobra.Command) error {
		if err := writeText(out.OutOrStdout(), rule+c.CommandPath()+"\n"+rule); err != nil {
			return err
		}

		textHelp(c, nil)

		for _, sub := range c.Commands() {
			if sub.IsAvailableCommand() || sub.Name() == "help" {
				if err := walk(sub); err != nil {
					return err
				}
			}
		}

		return nil
	}

	return walk(cmd)
}

type helpOutput struct {
	Schema  int     `json:"schema"`
	Command helpDoc `json:"command"`
}

// helpDoc is the --json form of a command's help.
type helpDoc struct {
	Path           string    `json:"path"`
	Usage          string    `json:"usage,omitempty"`
	Short          string    `json:"short"`
	Long           string    `json:"long,omitempty"`
	Example        string    `json:"example,omitempty"`
	Flags          []flagDoc `json:"flags,omitempty"`
	InheritedFlags []flagDoc `json:"inheritedFlags,omitempty"`
	// Commands are the subcommands: path and short only, or everything with
	// --all.
	Commands []helpDoc `json:"commands,omitempty"`
}

type flagDoc struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default,omitempty"`
	Usage     string `json:"usage"`
}

// newHelpDoc describes cmd; full also describes every subcommand in full
// (listing inherited flags only at cmd itself).
func newHelpDoc(cmd *cobra.Command, full bool) helpDoc {
	return helpDocAt(cmd, full, true)
}

func helpDocAt(cmd *cobra.Command, full, top bool) helpDoc {
	d := helpDoc{
		Path: cmd.CommandPath(), Usage: cmd.UseLine(), Short: cmd.Short, Long: cmd.Long, Example: cmd.Example,
		Flags: flagDocs(cmd.LocalFlags()),
	}

	if top {
		d.InheritedFlags = flagDocs(cmd.InheritedFlags())
	}

	for _, sub := range cmd.Commands() {
		if !sub.IsAvailableCommand() && sub.Name() != "help" {
			continue
		}

		if full {
			d.Commands = append(d.Commands, helpDocAt(sub, true, false))
		} else {
			d.Commands = append(d.Commands, helpDoc{Path: sub.CommandPath(), Short: sub.Short})
		}
	}

	return d
}

func flagDocs(fs *pflag.FlagSet) []flagDoc {
	var out []flagDoc

	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}

		d := flagDoc{Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Usage: f.Usage}
		if f.DefValue != "" && f.DefValue != "[]" && (f.Value.Type() != "bool" || f.DefValue != "false") {
			d.Default = f.DefValue
		}

		out = append(out, d)
	})

	return out
}
