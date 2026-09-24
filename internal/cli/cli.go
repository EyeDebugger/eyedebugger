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
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/drivers/generic"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/session"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// globals holds flags shared by every command of a binary (docs/DESIGN.md §4).
type globals struct {
	json    bool
	session string
	budget  int
	as      string
}

// defaultBudget is the token budget for variables in one output
// (docs/DESIGN.md §5).
const defaultBudget = 2000

// Environment variables for commands that act on a session.
const (
	// envSession names the default session.
	envSession = "EYEDBG_SESSION"
	// envClient is who the caller is when --as is not given.
	envClient = "EYEDBG_CLIENT"
)

// ref returns the session chosen by -s, else $EYEDBG_SESSION ("" lets the
// daemon pick the only session), and the client acting on it.
func (g *globals) ref() api.SessionRef {
	id := g.session
	if id == "" {
		id = os.Getenv(envSession)
	}

	return api.SessionRef{SessionID: id, Client: g.clientID()}
}

// clientID is --as, else $EYEDBG_CLIENT; "" is the default client (agent).
// The daemon validates it.
func (g *globals) clientID() string {
	if g.as != "" {
		return g.as
	}

	return os.Getenv(envClient)
}

// Run executes root with args (without the program name) and returns the
// process exit code.
func Run(ctx context.Context, root *cobra.Command, args []string) int {
	// Must run after the caller's SetOut/SetErr (docs/DESIGN.md §4): cobra's
	// InitDefaultCompletionCmd captures the command's output writer once, at
	// the moment it runs, and every completion sub-command's RunE reuses that
	// captured writer forever after. Finalizing any earlier — e.g. in
	// New*Command, before a caller can SetOut — would silently pin completion
	// output to os.Stdout regardless of what the caller configures.
	finalizeCommandTree(root)

	if args == nil {
		args = []string{} // cobra falls back to os.Args[1:] when args is nil
	}

	root.SetArgs(args)

	if err := root.ExecuteContext(ctx); err != nil {
		// Nothing useful can be done if writing the error itself fails.
		if wantsJSON(root, args) {
			_ = writeJSON(root.OutOrStdout(), errorOutput{Schema: jsonSchemaVersion, Error: toErrorDoc(err)})
		} else {
			_, _ = io.WriteString(root.ErrOrStderr(), formatError(root.Name(), err))
		}

		return exitCode(err)
	}

	return 0
}

// wantsJSON reports whether --json was given; errors raised before flags
// are parsed (e.g. an unknown command) only show it in args.
func wantsJSON(root *cobra.Command, args []string) bool {
	if f := root.PersistentFlags().Lookup("json"); f != nil && f.Value.String() == "true" {
		return true
	}

	for _, a := range args {
		if a == "--" {
			return false
		}

		if a == "--json" || a == "--json=true" {
			return true
		}
	}

	return false
}

// Exit codes by error class (docs/DESIGN.md §5), documented in eyedbgLong.
const (
	exitError       = 1 // usage errors, invalid requests, internal errors
	exitState       = 2 // the session is missing or not in the right state
	exitEnvironment = 3 // setup: adapter, build, daemon
	exitAdapter     = 4 // the debug adapter refused or failed a request
)

func exitCode(err error) int {
	switch api.CodeOf(err) {
	case api.CodeNoSession, api.CodeNotStopped, api.CodeNotRunning, api.CodeSessionExited, api.CodeSessionsActive,
		api.CodeLeaseHeld, api.CodeNotOwner:
		return exitState
	case api.CodeAdapterMissing, api.CodeBuildFailed, api.CodeDaemonNotRunning, api.CodeDaemonStart,
		api.CodeVersionMismatch, api.CodeUnauthorized, api.CodeAttachFailed, api.CodeNoTestHost:
		return exitEnvironment
	case api.CodeAdapterFailed, api.CodeUnsupported:
		return exitAdapter
	case api.CodeInvalidRequest, api.CodeUnknownMethod, api.CodeInternal, api.CodeSideEffects,
		api.CodeAnchorNotFound, api.CodeAnchorAmbiguous:
		return exitError
	default:
		return exitError
	}
}

// errorOutput is the --json form of an error.
type errorOutput struct {
	Schema int        `json:"schema"`
	Error  *api.Error `json:"error"`
}

// toErrorDoc returns err as an *api.Error; errors without a code (bad
// flags or arguments, local failures) get ERROR.
func toErrorDoc(err error) *api.Error {
	if e, ok := errors.AsType[*api.Error](err); ok {
		return e
	}

	return api.NewError("ERROR", err.Error(), "")
}

const eyedbgLong = `eyedbg is the CLI for EyeDebugger, an AI-native, CLI-first debugger built for
coding agents (and humans) driving the same live debug session.

The CLI is stateless: every invocation talks to a per-user daemon (eyedbgd) over local IPC, which
auto-starts on first use and exits by itself when idle (see 'eyedbg daemon --help'). There is no MCP server by default — this CLI, with its complete built-in
help, is the agent interface (docs/DESIGN.md §10).

A typical loop: start a program with breakpoints (dotnet or python, or a language your own
adapter manifest adds: 'eyedbg adapters ls'; or attach to a running one, or debug a test run with
'eyedbg test'), inspect (status, stack, vars, eval, output), move (next, step-in,
step-out, continue, pause, wait), change it if needed (set), and stop (or detach) when done.
Every execution command prints where the program ended up, so no extra call is needed to see it.
Breakpoints (lines, text anchors, functions, hit counts, logpoints) and exception stops are
managed with 'eyedbg bp'. Add --json to any command for machine-readable output
with a "schema" field; errors print a stable [CODE] and a hint.

Run 'eyedbg <command> --help' or 'eyedbg help <command>' for a command's own help: what it does,
when to use it, whether it blocks (and for how long), its effect on the debuggee, its output shape,
and its exit codes (docs/DESIGN.md §4). 'eyedbg help --all' prints every command's help in one
read; add --json for the same as structured data.

Agents: 'eyedbg skill install' installs a usage guide (SKILL.md) for your agent.

Output is budgeted: variables are cut to --budget tokens (default 2000) and every cut says so.

Several clients can share a session (docs/DESIGN.md §3): each request says who sends it (--as
agent[:NAME] or human[:NAME], else $EYEDBG_CLIENT, else agent). Breakpoints belong to the client
that added them; the control lease decides who may run, step or pause the program ('eyedbg lease
--help'); 'eyedbg events' shows what every client did.

Exit codes: 0 success (a wait that times out is a success that says so); 1 usage or internal
error (INVALID_REQUEST, SIDE_EFFECTS, ANCHOR_NOT_FOUND, ANCHOR_AMBIGUOUS); 2 no such session, it is
in the wrong state, or another client holds it (NO_SESSION, NOT_STOPPED, NOT_RUNNING,
SESSION_EXITED, SESSIONS_ACTIVE, LEASE_HELD, NOT_OWNER); 3 setup problem (ADAPTER_NOT_INSTALLED,
BUILD_FAILED, ATTACH_FAILED, NO_TEST_HOST, DAEMON_*, VERSION_MISMATCH, UNAUTHORIZED); 4 the debug
adapter refused a request or can't do it (ADAPTER_ERROR, e.g. an expression that doesn't
evaluate; UNSUPPORTED_BY_ADAPTER). Errors print "eyedbg: message [CODE]" and a hint on
stderr; with --json, {"schema": 1, "error": {"code", "message", "hint"}} on stdout.`

const eyedbgExample = `  eyedbg adapters install netcoredbg            # once per machine
  eyedbg start dotnet --bp Program.cs:12         # build, run, stop at line 12
  eyedbg start python --program app.py --bp app.py:7
  eyedbg vars                                    # locals of the current frame
  eyedbg next                                    # step over, show where it stopped
  eyedbg eval 'total * 2'
  eyedbg bp add Program.cs:20 --log 'i={i}'      # print instead of stopping
  eyedbg continue                                # to the next breakpoint or exit
  eyedbg stop                                    # end the session`

// NewEyedbgCommand returns the root command of the eyedbg CLI.
func NewEyedbgCommand(info version.Info) *cobra.Command {
	root, g := newRoot("eyedbg", "AI-native, CLI-first debugger", eyedbgLong, eyedbgExample, info)
	root.PersistentFlags().StringVarP(&g.session, "session", "s", "",
		"session id to act on (default: $"+envSession+", else the only session)")
	root.PersistentFlags().IntVar(&g.budget, "budget", defaultBudget,
		"most tokens (about 4 characters each) of variables to print; 0 for no limit")
	root.PersistentFlags().StringVar(&g.as, "as", "",
		"who you are: agent[:NAME] or human[:NAME] (default: $"+envClient+", else agent)")

	root.AddCommand(
		newStartCommand(info, g),
		newSessionsCommand(info, g),
		newStatusCommand(info, g),
		newWaitCommand(info, g),
		newStackCommand(info, g),
		newVarsCommand(info, g),
		newEvalCommand(info, g),
		newOutputCommand(info, g),
		newBreakpointCommand(info, g),
		newRunUntilCommand(info, g),
		newSetCommand(info, g),
		newAttachCommand(info, g),
		newDetachCommand(info, g),
		newTestCommand(info, g),
		newLeaseCommand(info, g),
		newEventsCommand(info, g),
		newStopCommand(info, g),
		newAdaptersCommand(g),
		newDaemonCommand(info, g),
		newSkillCommand(g),
		newVersionCommand("eyedbg", info, g),
	)
	root.AddCommand(newExecCommands(info, g)...)

	return root
}

const eyedbgdLong = `eyedbgd is the EyeDebugger daemon: the per-user process that owns live debug
sessions, clients, the control lease, breakpoint ownership, the event log and stop snapshots
(docs/DESIGN.md §6).

You normally never run it by hand: eyedbg starts it detached on first use, and it exits by itself
once it has had no sessions for --idle-timeout. Run it directly to debug the daemon itself: it
stays in the foreground and logs to stderr until interrupted (Ctrl-C), stopped with
'eyedbg daemon stop', or idle.

Only one daemon runs per user (per EYEDBG_RUNTIME_DIR): a second one exits 3 at once, or 0 without
a word when eyedbg started it (it lost a start race; eyedbg uses the winner). On start it writes a
fresh token and listens on a Unix socket, both in a directory only you can access. Its sessions/
subdirectory keeps each live session's metadata, so a daemon that crashed leaves its sessions listed
as lost, and each session's recording (control events only, no program output; pruned 7 days after
the session ended). Exits 0 when it stops normally, 1 on a start-up error.`

const eyedbgdExample = `  eyedbgd                       # run in the foreground, logging to stderr
  eyedbgd --idle-timeout 0      # never exit when idle
  eyedbgd version               # build info for this binary`

// NewDaemonCommand returns the root command of the eyedbgd daemon.
func NewDaemonCommand(info version.Info) *cobra.Command {
	root, g := newRoot("eyedbgd", "EyeDebugger per-user daemon", eyedbgdLong, eyedbgdExample, info)

	idle := daemon.DefaultIdleTimeout
	root.Flags().DurationVar(&idle, "idle-timeout", idle,
		"exit after this long with no debug sessions (e.g. 30m, 2h); 0 never exits when idle")

	root.RunE = func(cmd *cobra.Command, _ []string) error {
		p, err := daemon.DefaultPaths()
		if err != nil {
			return err
		}

		logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))

		err = daemon.Serve(cmd.Context(), daemon.Config{
			Paths: p, IdleTimeout: idle, Info: info, Logger: logger,
			Drivers: daemonDrivers(cmd.Context(), loadRegistry(), logger),
		})
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			// An auto-started daemon that lost the start race: the client
			// uses the winner, and the shared log stays clean.
			if os.Getenv(daemon.EnvAutostarted) == "1" {
				return nil
			}

			return api.NewError(api.CodeDaemonStart, err.Error()+" ("+p.Dir+")",
				"use 'eyedbg daemon status' to inspect it or 'eyedbg daemon stop' to stop it")
		}

		return err
	}
	root.AddCommand(newVersionCommand("eyedbgd", info, g))
	// eyedbgd is never invoked interactively by a shell (docs/DESIGN.md §6), so shell
	// completion has no audience here; disabling it keeps eyedbgd --help short.
	root.CompletionOptions.DisableDefaultCmd = true

	return root
}

// daemonDrivers are the daemon's languages: the Go drivers, each given the
// manifest serving its language, and a generic driver for every other
// language a manifest serves. Manifests that failed to load are logged.
func daemonDrivers(ctx context.Context, reg *adapters.Registry, logger *slog.Logger) []session.Driver {
	for _, p := range reg.Problems() {
		logger.WarnContext(ctx, "adapter manifest ignored", slog.String("path", p.Path), slog.Any("error", p.Err))
	}

	drivers := []session.Driver{dotnet.NewWith(reg.Language(dotnet.Language))}

	return append(drivers, generic.Drivers(reg)...)
}

// formatError renders err for stderr: the message, its stable code and a
// hint when it is an *api.Error (docs/DESIGN.md §5).
func formatError(name string, err error) string {
	apiErr, ok := errors.AsType[*api.Error](err)
	if !ok {
		return fmt.Sprintf("%s: %v\n", name, err)
	}

	msg := fmt.Sprintf("%s: %s [%s]\n", name, apiErr.Message, apiErr.Code)
	if apiErr.Hint != "" {
		msg += "hint: " + apiErr.Hint + "\n"
	}

	return msg
}

// finalizeCommandTree adds cobra's built-in "help" and "completion" commands
// up front instead of leaving them to be created lazily inside Execute, and
// gives each of them its own Example. It is idempotent (cobra's own
// InitDefaultHelpCmd/InitDefaultCompletionCmd are no-ops once the commands
// already exist), so Run calls it unconditionally, right before Execute. Test
// code that only walks or inspects a tree without ever executing it (never
// producing output) may also call it directly, e.g. via rootFactories.
func finalizeCommandTree(root *cobra.Command) {
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	for _, sub := range root.Commands() {
		if sub.Name() != "completion" {
			continue
		}

		sub.Example = fmt.Sprintf("  %[1]s completion bash   # print the bash completion script (see the shell sub-commands' own help to install it)", root.Name())

		for _, shell := range sub.Commands() {
			shell.Example = completionExample(root.Name(), shell.Name())
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
	root.PersistentFlags().BoolVar(&g.json, "json", false, "print machine-readable JSON output (errors too, on stdout)")
	installHelp(root, g)

	return root, g
}
