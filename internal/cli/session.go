// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Defaults for commands that wait on the program.
const (
	defaultExecTimeout = 30 * time.Second
	// callSlack is added to a request's own wait for the round trip.
	callSlack = 30 * time.Second
	// startBuildBudget covers building before the program even starts.
	startBuildBudget = 5 * time.Minute
)

// sessionHelp is appended to every command that acts on a session.
const sessionHelp = `
Which session: -s/--session <id>, else $EYEDBG_SESSION, else the only session (an error lists them
if there are several).`

// call connects to the running daemon (never starting one) and runs one
// request. With no daemon there can be no session, so that is NO_SESSION.
func call(cmd *cobra.Command, info version.Info, timeout time.Duration, method string, params, result any) error {
	p, err := daemon.DefaultPaths()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	cl, err := daemon.Dial(ctx, p, info)
	if api.CodeOf(err) == api.CodeDaemonNotRunning {
		return api.NewError(api.CodeNoSession, "there are no debug sessions (the daemon is not running)",
			"start one with 'eyedbg start'")
	}

	if err != nil {
		return err
	}
	defer cl.Close()

	return cl.Call(ctx, method, params, result)
}

// startFlags are the flags of 'eyedbg start'.
type startFlags struct {
	project, program, cwd string
	env, bps              []string
	stopOnEntry, noBuild  bool
	timeout               time.Duration
}

func newStartCommand(info version.Info, g *globals) *cobra.Command {
	var sf startFlags

	cmd := &cobra.Command{
		Use:   "start <lang> [flags] [-- program args...]",
		Short: "Start a program under the debugger",
		Long: `Build (unless --program or --no-build) and start a program under the debugger, creating a new
session. Languages: dotnet (via netcoredbg; install it once with 'eyedbg adapters install
netcoredbg').

For dotnet: --project takes a project file or a directory with exactly one project (default: the
current directory), which is built in Debug; --program takes an already-built .dll (or apphost)
and skips the build. Everything after "--" is passed to the program.

Breakpoints given with --bp are set before the program runs, so they can't be missed; add more
later with 'eyedbg bp add'. With --stop-on-entry or --bp, start waits (up to --timeout) for the
first stop and reports where the program stopped; otherwise it returns as soon as the program
runs. Starts the daemon if needed. Building can take minutes on a cold machine.

Output: the new session id and its state (text), or the session snapshot in --json. The session id
is also the value to pass to -s. Exits 0 once the program is running or stopped, 1 on failure
(BUILD_FAILED with the compiler errors, ADAPTER_NOT_INSTALLED, ...).`,
		Example: `  eyedbg start dotnet --bp Program.cs:12         # build ./, stop at line 12
  eyedbg start dotnet --project src/App/App.csproj --stop-on-entry
  eyedbg start dotnet --program bin/Debug/net10.0/App.dll -- --verbose input.txt`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params, err := sf.params(args, cmd.ArgsLenAtDash())
			if err != nil {
				return err
			}

			return startSession(cmd, info, g, params, sf.timeout)
		},
	}

	f := cmd.Flags()
	f.StringVar(&sf.project, "project", "", "project file or directory to build (default: current directory)")
	f.StringVar(&sf.program, "program", "", "already-built program to run (.dll or apphost); skips the build")
	f.StringVar(&sf.cwd, "cwd", "", "working directory of the program (default: the project's directory)")
	f.StringArrayVar(&sf.env, "env", nil, "environment variable KEY=VALUE for the program (repeatable)")
	f.StringArrayVar(&sf.bps, "bp", nil, "breakpoint FILE:LINE set before the program runs (repeatable)")
	f.BoolVar(&sf.stopOnEntry, "stop-on-entry", false, "stop at the program's entry point")
	f.BoolVar(&sf.noBuild, "no-build", false, "don't build; requires --program")
	f.DurationVar(&sf.timeout, "timeout", defaultExecTimeout, "how long to wait for the first stop with --bp or --stop-on-entry")

	return cmd
}

// params turns the flags and arguments (<lang> [-- program args]) into a
// start request, resolving paths against the working directory.
func (sf *startFlags) params(args []string, dash int) (api.StartParams, error) {
	lang := args[0]
	progArgs := args[1:]

	if dash >= 0 {
		if dash > 1 {
			return api.StartParams{}, fmt.Errorf("unexpected argument %q before \"--\"", args[1])
		}

		progArgs = args[dash:]
	} else if len(progArgs) > 0 {
		return api.StartParams{}, fmt.Errorf("unexpected argument %q (put program arguments after \"--\")", progArgs[0])
	}

	// The daemon's working directory is not the caller's: resolve the
	// default project here.
	project := sf.project
	if project == "" && sf.program == "" {
		project = "."
	}

	params := api.StartParams{
		Lang: lang,
		LaunchSpec: api.LaunchSpec{
			Project: absPath(project), Program: absPath(sf.program), Cwd: absPath(sf.cwd),
			Args: progArgs, NoBuild: sf.noBuild, StopOnEntry: sf.stopOnEntry,
		},
		Wait: api.Duration(sf.timeout),
	}

	var err error
	if params.Env, err = parseEnv(sf.env); err != nil {
		return api.StartParams{}, err
	}

	for _, b := range sf.bps {
		spec, err := parseLocation(b)
		if err != nil {
			return api.StartParams{}, err
		}

		params.Breakpoints = append(params.Breakpoints, spec)
	}

	return params, nil
}

func startSession(cmd *cobra.Command, info version.Info, g *globals, params api.StartParams, timeout time.Duration) error {
	p, err := daemon.DefaultPaths()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), startBuildBudget+timeout+callSlack)
	defer cancel()

	cl, _, err := daemon.Connect(ctx, p, info, daemon.OptionsFromEnv())
	if err != nil {
		return err
	}
	defer cl.Close()

	var snap api.Snapshot
	if err := cl.Call(ctx, api.MethodSessionStart, params, &snap); err != nil {
		return err
	}

	return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
}

func newSessionsCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "sessions",
		Short: "List debug sessions",
		Long: `List every debug session of the daemon: id, language, state (starting, running, stopped,
exited) and program. Exited sessions stay listed, with their exit code, until 'eyedbg stop'.

Never starts the daemon or affects a session. Prints nothing when there are none (an empty
"sessions" array in --json) and exits 0.`,
		Example: `  eyedbg sessions
  eyedbg sessions --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var list []api.SessionInfo

			err := call(cmd, info, daemonCallTimeout, api.MethodSessionList, nil, &list)
			if api.CodeOf(err) == api.CodeNoSession {
				list, err = nil, nil
			}

			if err != nil {
				return err
			}

			return writeSessions(cmd.OutOrStdout(), list, g.json)
		},
	}
}

func newStatusCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show a session's state and where it is stopped",
		Long: `Show a session's state; when stopped, also why (breakpoint, step, entry, pause, exception),
the thread, the current function and file:line, and the source lines around it. When exited, the
exit code.

Read-only: never resumes the program. Returns at once.` + sessionHelp,
		Example: `  eyedbg status
  eyedbg status -s s-k3f9 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var snap api.Snapshot
			if err := call(cmd, info, daemonCallTimeout, api.MethodSessionStatus, g.ref(), &snap); err != nil {
				return err
			}

			return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
		},
	}
}

func newStopCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "End a debug session (kills the program)",
		Long: `End a session: the program is terminated if it is still running, the debug adapter exits, and
the session is removed from 'eyedbg sessions'. Use it when done debugging; the daemon exits by
itself once no session is left for its idle timeout.

Returns within a few seconds.` + sessionHelp,
		Example: `  eyedbg stop
  eyedbg stop -s s-k3f9`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var info2 api.SessionInfo
			if err := call(cmd, info, daemonCallTimeout, api.MethodSessionStop, g.ref(), &info2); err != nil {
				return err
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Schema  int             `json:"schema"`
					Session api.SessionInfo `json:"session"`
				}{jsonSchemaVersion, info2})
			}

			return writeText(cmd.OutOrStdout(), "session "+info2.ID+" ended\n")
		},
	}
}

// execSpec describes one of the execution commands.
type execSpec struct {
	use, kind, short, long, example string
}

// execSpecs lists the execution commands.
func execSpecs() []execSpec {
	return []execSpec{
		{
			"continue", "continue", "Resume until the next stop or exit",
			`Resume the stopped program until it hits a breakpoint, stops for another reason, or exits.`,
			"  eyedbg continue\n  eyedbg continue --timeout 2m",
		},
		{
			"next", "next", "Step over the current line",
			`Run the current line, stepping over calls, and stop at the next line of the same function (or
its caller when the function returns).`,
			"  eyedbg next\n  eyedbg next --json",
		},
		{
			"step-in", "stepIn", "Step into the call on the current line",
			`Step into the function called on the current line; like next when there is no call to enter
(Just My Code is on: framework code is stepped over).`,
			"  eyedbg step-in",
		},
		{
			"step-out", "stepOut", "Run until the current function returns",
			`Run until the current function returns, and stop in its caller just after the call.`,
			"  eyedbg step-out",
		},
		{
			"pause", "pause", "Pause the running program",
			`Pause a running program wherever it is (e.g. to find where it hangs), then show where.`,
			"  eyedbg pause\n  eyedbg stack   # then see the whole stack",
		},
	}
}

func newExecCommands(info version.Info, g *globals) []*cobra.Command {
	specs := execSpecs()

	cmds := make([]*cobra.Command, 0, len(specs))

	for _, spec := range specs {
		var (
			timeout time.Duration
			thread  int
		)

		cmd := &cobra.Command{
			Use:   spec.use,
			Short: spec.short,
			Long: spec.long + `

Blocks until the program stops or exits, at most --timeout (default 30s). Hitting the timeout is not
an error: the program keeps running, the output says so ("timedOut": true in --json), and
'eyedbg wait' or 'eyedbg pause' take it from there.

Output: the resulting state, like 'eyedbg status' (stop reason, function, file:line and source
around it). Exits 1 if the program isn't in the right state (NOT_STOPPED, NOT_RUNNING,
SESSION_EXITED).` + sessionHelp,
			Example: spec.example,
			Args:    cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				params := api.ExecParams{SessionRef: g.ref(), Kind: spec.kind, ThreadID: thread, Wait: api.Duration(timeout)}

				var snap api.Snapshot
				if err := call(cmd, info, timeout+callSlack, api.MethodExec, params, &snap); err != nil {
					return err
				}

				return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
			},
		}

		cmd.Flags().DurationVar(&timeout, "timeout", defaultExecTimeout, "longest time to wait for the program to stop or exit")
		cmd.Flags().IntVar(&thread, "thread", 0, "thread id to act on (default: the thread that stopped)")
		cmds = append(cmds, cmd)
	}

	return cmds
}

func newWaitCommand(info version.Info, g *globals) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Wait for a running program to stop or exit",
		Long: `Wait until a running program stops (breakpoint, exception, ...) or exits, at most --timeout.
Use it after an execution command timed out, or after 'eyedbg start' without breakpoints. If the
program is already stopped, returns at once.

Never resumes or pauses the program. Output is the resulting state, like 'eyedbg status';
timing out is not an error ("timedOut": true in --json).` + sessionHelp,
		Example: `  eyedbg wait
  eyedbg wait --timeout 5m`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := api.WaitParams{SessionRef: g.ref(), AfterStops: -1, Wait: api.Duration(timeout)}

			var snap api.Snapshot
			if err := call(cmd, info, daemonCallTimeout, api.MethodSessionStatus, g.ref(), &snap); err != nil {
				return err
			}

			if snap.Session.State == api.StateStopped || snap.Session.State == api.StateExited {
				return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
			}

			if err := call(cmd, info, timeout+callSlack, api.MethodWait, params, &snap); err != nil {
				return err
			}

			return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", defaultExecTimeout, "longest time to wait")

	return cmd
}

func newStackCommand(info version.Info, g *globals) *cobra.Command {
	var levels, thread int

	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Show the call stack of the stopped thread",
		Long: `Show the call stack of the thread that stopped (or --thread), innermost frame first as #0. The
frame numbers are what 'eyedbg vars --frame N' and 'eyedbg eval --frame N' take.

Needs a stopped program (NOT_STOPPED otherwise); read-only; returns at once. Output: one line per
frame, "#N function file:line".` + sessionHelp,
		Example: `  eyedbg stack
  eyedbg stack --frames 50 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var frames []api.Frame
			if err := call(cmd, info, daemonCallTimeout, api.MethodStack,
				api.StackParams{SessionRef: g.ref(), ThreadID: thread, Levels: levels}, &frames); err != nil {
				return err
			}

			return writeStack(cmd.OutOrStdout(), frames, g.json, workDir())
		},
	}

	cmd.Flags().IntVar(&levels, "frames", 20, "maximum number of frames; 0 for all")
	cmd.Flags().IntVar(&thread, "thread", 0, "thread id (default: the thread that stopped)")

	return cmd
}

//nolint:dupl // Command definitions share a shape; their help, flags and output differ.
func newVarsCommand(info version.Info, g *globals) *cobra.Command {
	var frame, depth int

	cmd := &cobra.Command{
		Use:   "vars",
		Short: "Show the variables of a stack frame",
		Long: `Show the variables in scope in a frame of the stopped thread (default: #0, the current one),
grouped by scope (e.g. Locals). --depth expands objects and collections that many levels (1 = only
the top-level values; objects with members are marked {…}). At most 50 members are shown per
level and values are cut at 200 characters, with the remainder counted.

Needs a stopped program; read-only (properties are not evaluated beyond what the adapter shows).
Returns at once.` + sessionHelp,
		Example: `  eyedbg vars
  eyedbg vars --frame 1 --depth 2`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var scopes []api.Scope
			if err := call(cmd, info, daemonCallTimeout, api.MethodVars,
				api.VarsParams{SessionRef: g.ref(), Frame: frame, Depth: depth}, &scopes); err != nil {
				return err
			}

			return writeVars(cmd.OutOrStdout(), scopes, g.json)
		},
	}

	cmd.Flags().IntVar(&frame, "frame", 0, "frame number from 'eyedbg stack' (0 = innermost)")
	cmd.Flags().IntVar(&depth, "depth", 1, "levels of members to expand (1 = none)")

	return cmd
}

func newEvalCommand(info version.Info, g *globals) *cobra.Command {
	var frame int

	cmd := &cobra.Command{
		Use:   "eval <expression>",
		Short: "Evaluate an expression in a stack frame",
		Long: `Evaluate an expression in the context of a frame of the stopped thread (default: #0) and print
its value and type. For dotnet, netcoredbg evaluates C# expressions: operators, member and index
access, method calls and casts; lambdas and LINQ with lambdas are not supported.

Side effects: calling a method or property getter runs code in the program and can change its
state. Needs a stopped program; returns at once.` + sessionHelp,
		Example: `  eyedbg eval total
  eyedbg eval 'order.Items.Count * 2' --frame 1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var res api.EvalResult
			if err := call(cmd, info, daemonCallTimeout, api.MethodEval,
				api.EvalParams{SessionRef: g.ref(), Expression: args[0], Frame: frame}, &res); err != nil {
				return err
			}

			return writeEval(cmd.OutOrStdout(), res, g.json)
		},
	}

	cmd.Flags().IntVar(&frame, "frame", 0, "frame number from 'eyedbg stack' (0 = innermost)")

	return cmd
}

//nolint:dupl // Command definitions share a shape; their help, flags and output differ.
func newOutputCommand(info version.Info, g *globals) *cobra.Command {
	var since, tail int

	cmd := &cobra.Command{
		Use:   "output",
		Short: "Show what the program printed",
		Long: `Show the program's output (stdout, stderr and debugger console messages), as captured by the
debugger. The last 2000 chunks are kept per session. Each chunk has a sequence number: pass the
highest one you have seen to --since to get only newer output ("seq" in --json).

Read-only; works in any state, including after the program exited.` + sessionHelp,
		Example: `  eyedbg output
  eyedbg output --tail 20
  eyedbg output --since 42 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var lines []api.OutputLine
			if err := call(cmd, info, daemonCallTimeout, api.MethodOutput,
				api.OutputParams{SessionRef: g.ref(), Since: since, Tail: tail}, &lines); err != nil {
				return err
			}

			return writeOutput(cmd.OutOrStdout(), lines, g.json)
		},
	}

	cmd.Flags().IntVar(&since, "since", 0, "only output with a sequence number above this")
	cmd.Flags().IntVar(&tail, "tail", 0, "only the last N chunks; 0 for all")

	return cmd
}

func newBreakpointCommand(info version.Info, g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bp",
		Short: "Add, list and remove breakpoints",
		Long: `Manage line breakpoints of a session. Breakpoints can be added while the program runs or is
stopped; to be sure one is hit early in the program, pass it to 'eyedbg start --bp' instead.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`,
		Example: `  eyedbg bp add Program.cs:12
  eyedbg bp ls
  eyedbg bp rm 1`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newBreakpointAddCommand(info, g), newBreakpointListCommand(info, g), newBreakpointRemoveCommand(info, g))

	return cmd
}

func newBreakpointAddCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "add <file:line>",
		Short: "Add a line breakpoint",
		Long: `Add a breakpoint at FILE:LINE (FILE relative to the current directory, or absolute). The adapter
may move it to the nearest line with code: the output shows the requested and actual line, and
whether it is verified. An unverified breakpoint is pending (e.g. its module isn't loaded yet) and
may still bind later; 'eyedbg bp ls' shows the current state.

Does not resume the program; returns at once.` + sessionHelp,
		Example: `  eyedbg bp add Program.cs:12
  eyedbg bp add src/App/Orders.cs:88 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := parseLocation(args[0])
			if err != nil {
				return err
			}

			var bp api.Breakpoint
			if err := call(cmd, info, daemonCallTimeout, api.MethodBreakpointAdd,
				api.BreakpointAddParams{SessionRef: g.ref(), BreakpointSpec: spec}, &bp); err != nil {
				return err
			}

			return writeBreakpoints(cmd.OutOrStdout(), []api.Breakpoint{bp}, g.json, workDir())
		},
	}
}

func newBreakpointListCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List breakpoints",
		Long: `List the session's breakpoints: id, file:line (and the requested line if the adapter moved it),
and whether each is verified.

Read-only; returns at once. Prints nothing when there are none.` + sessionHelp,
		Example: `  eyedbg bp ls
  eyedbg bp ls --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var bps []api.Breakpoint
			if err := call(cmd, info, daemonCallTimeout, api.MethodBreakpointLs, g.ref(), &bps); err != nil {
				return err
			}

			return writeBreakpoints(cmd.OutOrStdout(), bps, g.json, workDir())
		},
	}
}

func newBreakpointRemoveCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <id|all>",
		Short: "Remove a breakpoint, or all of them",
		Long: `Remove the breakpoint with the given id (from 'eyedbg bp ls'), or every breakpoint with "all".

Does not resume the program; returns at once. Exits 1 if there is no such breakpoint.` + sessionHelp,
		Example: `  eyedbg bp rm 2
  eyedbg bp rm all`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := 0
			if args[0] != "all" {
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 1 {
					return fmt.Errorf("invalid breakpoint id %q (use a number from 'eyedbg bp ls', or all)", args[0])
				}

				id = n
			}

			var res api.BreakpointRemoveResult
			if err := call(cmd, info, daemonCallTimeout, api.MethodBreakpointRm,
				api.BreakpointRemoveParams{SessionRef: g.ref(), ID: id}, &res); err != nil {
				return err
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Schema  int `json:"schema"`
					Removed int `json:"removed"`
				}{jsonSchemaVersion, res.Removed})
			}

			return writeText(cmd.OutOrStdout(), fmt.Sprintf("removed %d breakpoint(s)\n", res.Removed))
		},
	}
}

// parseLocation parses FILE:LINE, resolving FILE against the working
// directory.
func parseLocation(s string) (api.BreakpointSpec, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return api.BreakpointSpec{}, fmt.Errorf("invalid location %q: want FILE:LINE", s)
	}

	line, err := strconv.Atoi(s[i+1:])
	if err != nil || line < 1 {
		return api.BreakpointSpec{}, fmt.Errorf("invalid line in %q: want FILE:LINE with LINE from 1", s)
	}

	return api.BreakpointSpec{File: absPath(s[:i]), Line: line}, nil
}

func parseEnv(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil //nolint:nilnil // No variables is a valid, empty result.
	}

	env := make(map[string]string, len(kvs))

	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --env %q: want KEY=VALUE", kv)
		}

		env[k] = v
	}

	return env, nil
}

// absPath makes p absolute against the working directory ("" stays "").
func absPath(p string) string {
	if p == "" {
		return ""
	}

	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}

	return abs
}

// workDir is the directory file paths are shown relative to.
func workDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}

	return wd
}
