// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/skill"
)

func newSkillCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Print or install eyedbg's agent usage guide",
		Long: `Print or install SKILL.md (docs/DESIGN.md §10), the agent-facing guide to eyedbg: when to reach
for it, the debug loop, keeping output small, sharing a session, and per-language notes. It ships
embedded in this binary, so it is always in sync with the eyedbg that prints it.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`,
		Example: `  eyedbg skill print
  eyedbg skill install`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newSkillPrintCommand(g), newSkillInstallCommand(g))

	return cmd
}

// skillPrintOutput is the --json form of 'skill print'.
type skillPrintOutput struct {
	Schema  int    `json:"schema"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

func newSkillPrintCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "print",
		Short: "Print the embedded SKILL.md",
		Long: `Print SKILL.md byte for byte, frontmatter included, so 'eyedbg skill print > SKILL.md' works for
agents that install skills themselves. --json wraps it as {"schema", "name", "content"}.

Never blocks and has no effect on any debug session or the filesystem. Exits 0, or 1 on a write
error.`,
		Example: `  eyedbg skill print
  eyedbg skill print > SKILL.md`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.json {
				return writeJSON(cmd.OutOrStdout(), skillPrintOutput{Schema: jsonSchemaVersion, Name: skill.Name, Content: skill.Markdown()})
			}

			return writeText(cmd.OutOrStdout(), skill.Markdown())
		},
	}
}

// skillInstallOutput is the --json form of 'skill install'.
type skillInstallOutput struct {
	Schema int    `json:"schema"`
	Path   string `json:"path"`
	Status string `json:"status"`
}

func newSkillInstallCommand(g *globals) *cobra.Command {
	var (
		dir   string
		force bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install SKILL.md into an agent's skills directory",
		Long: `Write the embedded SKILL.md to ROOT/eyedbg/SKILL.md, creating directories as needed. ROOT is a
skills root: by default Claude Code's personal one (~/.claude/skills, or %USERPROFILE%\.claude\skills
on Windows); --dir names a different one, e.g. --dir .claude/skills for a project, or
--dir ~/.codex/skills for another agent. A leading ~ (or ~\ on Windows) in --dir is expanded to your
home directory ourselves, since the shell does not always do it (bash never does after "=", cmd.exe
never does at all).

Idempotent: an identical existing file is left alone. A different one is left alone and refused
(INVALID_REQUEST) unless --force replaces it. Prints the path written and one of "installed",
"unchanged" or "replaced" ("path" and "status" in --json), then a reminder to restart the agent (or
start a new session) so it loads the change.

Never touches a debug session. Exits 0, or 1 if the file differs and --force was not given, or on a
write error.`,
		Example: `  eyedbg skill install
  eyedbg skill install --force
  eyedbg skill install --dir ~/.codex/skills`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := resolveSkillRoot(dir)
			if err != nil {
				return err
			}

			result, err := skill.Install(root, force)
			if err != nil {
				return skillInstallError(err)
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), skillInstallOutput{Schema: jsonSchemaVersion, Path: result.Path, Status: string(result.Status)})
			}

			return writeText(cmd.OutOrStdout(), fmt.Sprintf("%s %s\nrestart your agent (or start a new session) to load it\n", result.Status, result.Path))
		},
	}

	cmd.Flags().StringVar(&dir, "dir", "", "skills root to install into (default: Claude Code's personal skills directory)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing SKILL.md that differs")

	return cmd
}

// resolveSkillRoot returns dir as an absolute path, or [skill.DefaultRoot]
// when dir is "". A leading "~" is expanded to the user's home directory
// first: neither bash (after "=") nor cmd.exe (ever) expands it for us, so
// without this a literal "~" directory would be created in the cwd.
func resolveSkillRoot(dir string) (string, error) {
	if dir == "" {
		return skill.DefaultRoot()
	}

	dir, err := expandHome(dir)
	if err != nil {
		return "", err
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}

	return abs, nil
}

// expandHome expands a leading "~", "~/" or "~\" (Windows) to the user's
// home directory. "~name" (another user's home) has no portable stdlib
// lookup and is left as a literal path, same as an unexpanded "~" alone
// would resolve to a directory literally named "~".
func expandHome(dir string) (string, error) {
	if dir != "~" && !strings.HasPrefix(dir, "~/") && !strings.HasPrefix(dir, `~\`) {
		return dir, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %s: %w", dir, err)
	}

	return filepath.Join(home, dir[1:]), nil
}

// skillInstallError maps a [skill.DiffersError] to INVALID_REQUEST with the
// --force hint; any other error passes through.
func skillInstallError(err error) error {
	if _, ok := errors.AsType[*skill.DiffersError](err); ok {
		return api.NewError(api.CodeInvalidRequest, err.Error(), "use --force to overwrite it")
	}

	return err
}
