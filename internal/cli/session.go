// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
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
if there are several). Who you are: --as agent[:NAME] or human[:NAME], else $EYEDBG_CLIENT, else
agent; two agents sharing a session need different names (e.g. EYEDBG_CLIENT=agent:claude).`

// leaseHelp is appended to every command that changes the program's execution.
const leaseHelp = `

It needs the session's control lease: under the default policy (free) it takes the lease from
whoever holds it; under handoff or human-priority it fails with LEASE_HELD (exit 2) while another
client holds it. See 'eyedbg lease --help'.`

// dumpHelp is appended to every command that prints a stop snapshot.
const dumpHelp = `

When the program is stopped, the output also has what --dump asks for (comma-separated, default
"changed"): changed = the current function's locals that changed since the previous stop, with
their old values (all locals when it is the first stop in this function); locals = every local;
stack = the top 10 frames; none = only the location. What the program printed since it last
resumed is always shown (the last 20 chunks). Variables are cut to --budget tokens, saying so.`

// dumpFlag is the --dump flag of commands that print a stop snapshot.
type dumpFlag struct {
	what []string
}

func addDumpFlag(cmd *cobra.Command) *dumpFlag {
	d := &dumpFlag{}
	cmd.Flags().StringSliceVar(&d.what, "dump", []string{api.DumpChanged},
		"what to show when stopped: changed, locals, stack (comma-separated), or none")

	return d
}

// spec validates the flag and returns the request's DumpSpec.
func (d *dumpFlag) spec(g *globals) (api.DumpSpec, error) {
	for _, w := range d.what {
		switch w {
		case api.DumpChanged, api.DumpLocals, api.DumpStack:
		case api.DumpNone:
			if len(d.what) > 1 {
				return api.DumpSpec{}, errors.New("--dump none can't be combined with other values")
			}

			return api.DumpSpec{Budget: g.budget}, nil
		default:
			return api.DumpSpec{}, fmt.Errorf("invalid --dump value %q: want changed, locals, stack or none", w)
		}
	}

	return api.DumpSpec{Dump: d.what, Budget: g.budget}, nil
}

// errNoDaemon marks the NO_SESSION error of a call that found no daemon.
var errNoDaemon = errors.New("the daemon is not running")

// call connects to the running daemon (never starting one) and runs one
// request. With no daemon there can be no session, so that is NO_SESSION
// (also matching errNoDaemon). A daemon of another protocol version is
// VERSION_MISMATCH: it would misread the request.
func call(cmd *cobra.Command, info version.Info, timeout time.Duration, method string, params, result any) error {
	p, err := daemon.DefaultPaths()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	cl, err := daemon.Dial(ctx, p, info)
	if api.CodeOf(err) == api.CodeDaemonNotRunning {
		return errors.Join(api.NewError(api.CodeNoSession, "there are no debug sessions (the daemon is not running)",
			"start one with 'eyedbg start'"), errNoDaemon)
	}

	if err != nil {
		return err
	}
	defer cl.Close()

	if cl.Hello.ProtocolVersion != api.ProtocolVersion {
		return api.NewError(api.CodeVersionMismatch,
			fmt.Sprintf("eyedbgd %s speaks protocol %d but this eyedbg speaks %d", cl.Hello.Version, cl.Hello.ProtocolVersion, api.ProtocolVersion),
			"stop it with 'eyedbg daemon stop' (--force ends its sessions); 'eyedbg start' then starts a matching one")
	}

	return cl.Call(ctx, method, params, result)
}

// envNoRecord, when "1", turns recording off for new sessions.
const envNoRecord = "EYEDBG_NO_RECORD"

// startFlags are the flags of 'eyedbg start'.
type startFlags struct {
	project, program, cwd, leasePolicy string
	env, bps                           []string
	stopOnEntry, noBuild, noRecord     bool
	timeout                            time.Duration
	dump                               *dumpFlag
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

The starting client (--as) owns the --bp breakpoints and holds the control lease first;
--lease-policy decides whether other clients can take it (free, the default: anyone, by just
running a command; handoff: only when granted; human-priority: handoff, but a human may take it from
an agent). The session's control events (not the program's output) are recorded to a file in the
daemon's runtime directory, shown in 'eyedbg sessions --json'; --no-record or EYEDBG_NO_RECORD=1
turns that off.

Output: the new session id and its state (text), or the session snapshot in --json. The session id
is also the value to pass to -s. Exits 0 once the program is running or stopped, non-zero on
failure (BUILD_FAILED with the compiler errors, ADAPTER_NOT_INSTALLED, ...; see 'eyedbg --help'
for the exit codes).` + dumpHelp + `
`,
		Example: `  eyedbg start dotnet --bp Program.cs:12         # build ./, stop at line 12
  eyedbg start dotnet --project src/App/App.csproj --stop-on-entry
  eyedbg start dotnet --program bin/Debug/net10.0/App.dll -- --verbose input.txt`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params, err := sf.params(args, cmd.ArgsLenAtDash())
			if err != nil {
				return err
			}

			params.Client = g.clientID()

			if params.DumpSpec, err = sf.dump.spec(g); err != nil {
				return err
			}

			return startSession(cmd, info, g, params, sf.timeout)
		},
	}

	sf.register(cmd)

	return cmd
}

// register adds the flags to cmd.
func (sf *startFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&sf.project, "project", "", "project file or directory to build (default: current directory)")
	f.StringVar(&sf.program, "program", "", "already-built program to run (.dll or apphost); skips the build")
	f.StringVar(&sf.cwd, "cwd", "", "working directory of the program (default: the project's directory)")
	f.StringArrayVar(&sf.env, "env", nil, "environment variable KEY=VALUE for the program (repeatable)")
	f.StringArrayVar(&sf.bps, "bp", nil, "breakpoint FILE:LINE set before the program runs (repeatable)")
	f.BoolVar(&sf.stopOnEntry, "stop-on-entry", false, "stop at the program's entry point")
	f.BoolVar(&sf.noBuild, "no-build", false, "don't build; requires --program")
	f.StringVar(&sf.leasePolicy, "lease-policy", string(api.LeaseFree), "who may take the control lease: free, handoff or human-priority")
	f.BoolVar(&sf.noRecord, "no-record", false, "don't record the session's control events (also EYEDBG_NO_RECORD=1)")
	f.DurationVar(&sf.timeout, "timeout", defaultExecTimeout, "how long to wait for the first stop with --bp or --stop-on-entry")
	sf.dump = addDumpFlag(cmd)
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

	policy, err := api.ParseLeasePolicy(sf.leasePolicy)
	if err != nil {
		return api.StartParams{}, err
	}

	params := api.StartParams{
		Lang: lang,
		LaunchSpec: api.LaunchSpec{
			Project: absPath(project), Program: absPath(sf.program), Cwd: absPath(sf.cwd),
			Args: progArgs, NoBuild: sf.noBuild, StopOnEntry: sf.stopOnEntry,
		},
		Wait:        api.Duration(sf.timeout),
		LeasePolicy: policy,
		NoRecord:    sf.noRecord || os.Getenv(envNoRecord) == "1",
	}

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
exited, lost), who holds its control lease, and program, under a header line. Exited sessions stay
listed, with their exit code, until 'eyedbg stop'. A lost session belonged to a daemon that exited
while it was live: only its metadata and recording are left, and 'eyedbg stop -s ID' forgets it.
With no daemon running, the lost sessions are read from the runtime directory.

Never starts the daemon or affects a session. Prints nothing when there are none (an empty
"sessions" array in --json) and exits 0. --json adds each session's lease, clients (who used it,
first and last seen) and recording file.`,
		Example: `  eyedbg sessions
  eyedbg sessions --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var list []api.SessionInfo

			err := call(cmd, info, daemonCallTimeout, api.MethodSessionList, nil, &list)
			if errors.Is(err, errNoDaemon) {
				list, err = lostSessions()
			}

			if err != nil {
				return err
			}

			return writeSessions(cmd.OutOrStdout(), list, g.json)
		},
	}
}

func newStatusCommand(info version.Info, g *globals) *cobra.Command {
	var dump *dumpFlag

	cmd := &cobra.Command{
		Use:   statusUse,
		Short: "Show a session's state and where it is stopped",
		Long: `Show a session's state; when stopped, also why (breakpoint, step, entry, pause, exception),
the thread, the current function and file:line, and the source lines around it. When exited, the
exit code.

Read-only: never resumes the program. Returns at once.` + dumpHelp + sessionHelp,
		Example: `  eyedbg status
  eyedbg status --dump locals,stack
  eyedbg status -s s-k3f9 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			spec, err := dump.spec(g)
			if err != nil {
				return err
			}

			var snap api.Snapshot
			if err := call(cmd, info, daemonCallTimeout, api.MethodSessionStatus,
				api.StatusParams{SessionRef: g.ref(), DumpSpec: spec}, &snap); err != nil {
				return err
			}

			return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
		},
	}
	dump = addDumpFlag(cmd)

	return cmd
}

func newStopCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "End a debug session (kills the program)",
		Long: `End a session: the program is terminated if it is still running, the debug adapter exits, and
the session is removed from 'eyedbg sessions'. Use it when done debugging; the daemon exits by
itself once no session is left for its idle timeout. While the program is live this needs the
control lease, like the execution commands (LEASE_HELD, exit 2, if the policy keeps you from
taking it); once it has exited, anyone may stop the session.

A lost session (see 'eyedbg sessions') is forgotten instead: its metadata is deleted, its recording
kept; that also works with no daemon running, given -s ID.

Returns within a few seconds.` + sessionHelp,
		Example: `  eyedbg stop
  eyedbg stop -s s-k3f9`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ref := g.ref()

			var info2 api.SessionInfo

			err := call(cmd, info, daemonCallTimeout, api.MethodSessionStop, ref, &info2)
			if errors.Is(err, errNoDaemon) && ref.SessionID != "" {
				info2, err = forgetLost(ref.SessionID, err)
			}

			if err != nil {
				return err
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Schema  int             `json:"schema"`
					Session api.SessionInfo `json:"session"`
				}{jsonSchemaVersion, info2})
			}

			if info2.State == api.StateLost {
				return writeText(cmd.OutOrStdout(), "session "+info2.ID+" forgotten (it was lost)\n")
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
			"  eyedbg next\n  eyedbg next --dump changed,stack\n  eyedbg next --json",
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
			dump    *dumpFlag
		)

		cmd := &cobra.Command{
			Use:   spec.use,
			Short: spec.short,
			Long: spec.long + `

Blocks until the program stops or exits, at most --timeout (default 30s). Hitting the timeout is not
an error: the program keeps running, the output says so ("timedOut": true in --json), and
'eyedbg wait' or 'eyedbg pause' take it from there.

Output: the resulting state, like 'eyedbg status' (stop reason, function, file:line and source
around it). Exits 2 if the program isn't in the right state (NOT_STOPPED, NOT_RUNNING,
SESSION_EXITED) or another client holds the lease (LEASE_HELD).` + leaseHelp + dumpHelp + sessionHelp,
			Example: spec.example,
			Args:    cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				ds, err := dump.spec(g)
				if err != nil {
					return err
				}

				params := api.ExecParams{SessionRef: g.ref(), DumpSpec: ds, Kind: spec.kind, ThreadID: thread, Wait: api.Duration(timeout)}

				var snap api.Snapshot
				if err := call(cmd, info, timeout+callSlack, api.MethodExec, params, &snap); err != nil {
					return err
				}

				return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
			},
		}

		cmd.Flags().DurationVar(&timeout, "timeout", defaultExecTimeout, "longest time to wait for the program to stop or exit")
		cmd.Flags().IntVar(&thread, "thread", 0, "thread id to act on (default: the thread that stopped)")
		dump = addDumpFlag(cmd)
		cmds = append(cmds, cmd)
	}

	return cmds
}

func newWaitCommand(info version.Info, g *globals) *cobra.Command {
	var (
		timeout time.Duration
		dump    *dumpFlag
	)

	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Wait for a running program to stop or exit",
		Long: `Wait until a running program stops (breakpoint, exception, ...) or exits, at most --timeout.
Use it after an execution command timed out, or after 'eyedbg start' without breakpoints. If the
program is already stopped, returns at once.

Never resumes or pauses the program. Output is the resulting state, like 'eyedbg status';
timing out is not an error ("timedOut": true in --json).` + dumpHelp + sessionHelp,
		Example: `  eyedbg wait
  eyedbg wait --timeout 5m`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds, err := dump.spec(g)
			if err != nil {
				return err
			}

			params := api.WaitParams{SessionRef: g.ref(), DumpSpec: ds, AfterStops: -1, Wait: api.Duration(timeout)}

			var snap api.Snapshot
			if err := call(cmd, info, daemonCallTimeout, api.MethodSessionStatus,
				api.StatusParams{SessionRef: g.ref(), DumpSpec: ds}, &snap); err != nil {
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
	dump = addDumpFlag(cmd)

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

func newVarsCommand(info version.Info, g *globals) *cobra.Command {
	var p api.VarsParams

	cmd := &cobra.Command{
		Use:   "vars",
		Short: "Show the variables of a stack frame",
		Long: `Show the variables in scope in a frame of the stopped thread (default: #0, the current one),
grouped by scope (e.g. Locals). --depth expands objects and collections that many levels (1 = only
the top-level values; objects with members are marked {…}). At most 50 members are shown per
level and values are cut at 200 characters, with the remainder counted.

--expand PATH shows just one variable and its members, e.g. order, order.Items or
order.Items[0].Name. PATH is first looked up as members as the adapter shows them (arrays have
members named [N]); if that fails it is evaluated as an expression (so indexers like list[0] work,
but a getter or indexer runs code in the program). If neither works, the error lists the names
there. --changed shows only the locals of frame #0 that changed since the previous stop,
with their old values (compared by value at the top level: a change inside an object whose value
is only its type name is not seen).

The whole output is cut to --budget tokens (breadth-first: every variable before any member), and
says so; then use --expand for the part you need, or --budget 0.

Needs a stopped program; read-only (properties are not evaluated beyond what the adapter shows).
Returns at once.` + sessionHelp,
		Example: `  eyedbg vars
  eyedbg vars --frame 1 --depth 2
  eyedbg vars --expand order.Items --depth 2
  eyedbg vars --changed`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p.SessionRef, p.Budget = g.ref(), g.budget

			var scopes []api.Scope
			if err := call(cmd, info, daemonCallTimeout, api.MethodVars, p, &scopes); err != nil {
				return err
			}

			return writeVars(cmd.OutOrStdout(), scopes, g.json)
		},
	}

	cmd.Flags().IntVar(&p.Frame, "frame", 0, "frame number from 'eyedbg stack' (0 = innermost)")
	cmd.Flags().IntVar(&p.Depth, "depth", 1, "levels of members to expand (1 = none)")
	cmd.Flags().StringVar(&p.Expand, "expand", "", "show only the variable at this path, e.g. order.Items[0]")
	cmd.Flags().BoolVar(&p.Changed, "changed", false, "show only the locals that changed since the previous stop")
	cmd.MarkFlagsMutuallyExclusive("expand", "changed")

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
			var res api.OutputResult
			if err := call(cmd, info, daemonCallTimeout, api.MethodOutput,
				api.OutputParams{SessionRef: g.ref(), Since: since, Tail: tail}, &res); err != nil {
				return err
			}

			return writeOutput(cmd.OutOrStdout(), cmd.ErrOrStderr(), res, tail > 0, g.json)
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
	var cond string

	cmd := &cobra.Command{
		Use:   "add <file:line>",
		Short: "Add a line breakpoint",
		Long: `Add a breakpoint at FILE:LINE (FILE relative to the current directory, or absolute). The adapter
may move it to the nearest line with code: the output shows the requested and actual line, and
whether it is verified. An unverified breakpoint is pending (e.g. its module isn't loaded yet) and
may still bind later; 'eyedbg bp ls' shows the current state.

--if EXPR stops only when EXPR, an expression in the program's language, is true there (e.g.
'i == 3'). The adapter evaluates it each time the line runs, so it runs code in the program; an
expression that fails to evaluate stops the program. Adding a breakpoint at a line where you
already have one replaces its condition.

The breakpoint is yours (--as): only you remove it, unless 'bp rm --force'. Other clients'
breakpoints at the same line share it: the program stops there if any of them would (a
breakpoint without a condition wins; different conditions make it stop unconditionally, which
'eyedbg bp ls' notes). Needs no lease.

Does not resume the program; returns at once.` + sessionHelp,
		Example: `  eyedbg bp add Program.cs:12
  eyedbg bp add Orders.cs:88 --if 'order.Total > 100'
  eyedbg bp add src/App/Orders.cs:88 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := parseLocation(args[0])
			if err != nil {
				return err
			}

			spec.Condition = cond

			var bp api.Breakpoint
			if err := call(cmd, info, daemonCallTimeout, api.MethodBreakpointAdd,
				api.BreakpointAddParams{SessionRef: g.ref(), BreakpointSpec: spec}, &bp); err != nil {
				return err
			}

			return writeBreakpoints(cmd.OutOrStdout(), []api.Breakpoint{bp}, g.json, workDir())
		},
	}

	cmd.Flags().StringVar(&cond, "if", "", "stop only when this expression is true")

	return cmd
}

func newRunUntilCommand(info version.Info, g *globals) *cobra.Command {
	var (
		timeout time.Duration
		thread  int
		cond    string
		dump    *dumpFlag
	)

	cmd := &cobra.Command{
		Use:   "run-until <file:line>",
		Short: "Continue to a line, optionally until a condition holds there",
		Long: `Continue the stopped program until it reaches FILE:LINE; with --if EXPR, until it reaches it
with EXPR true (an expression in the program's language, e.g. 'i == 3' or 'order.Total > 100';
evaluating it runs code in the program). Replaces "bp add, continue, bp rm" with one call.

It works through a temporary breakpoint, removed once the program stops or exits. The program may
stop elsewhere first (another breakpoint, an exception, a pause) or exit: the output says where it
is. If you already have a breakpoint at that line, that one is used and --if is ignored; another
client's breakpoint there is not (your temporary one shares the line with it).

Blocks until the program stops or exits, at most --timeout (default 30s). On a timeout the program
keeps running and the temporary breakpoint stays (marked in 'eyedbg bp ls') so 'eyedbg wait' can
still catch it; the next execution command, whoever sends it, removes it. Exits 2 if the program
isn't stopped or another client holds the lease (LEASE_HELD).` +
			leaseHelp + dumpHelp + sessionHelp,
		Example: `  eyedbg run-until Program.cs:20
  eyedbg run-until Orders.cs:88 --if 'order.Id == 42' --timeout 2m
  eyedbg run-until Program.cs:20 --dump locals`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := parseLocation(args[0])
			if err != nil {
				return err
			}

			spec.Condition = cond

			ds, err := dump.spec(g)
			if err != nil {
				return err
			}

			params := api.RunUntilParams{
				SessionRef: g.ref(), BreakpointSpec: spec, DumpSpec: ds, ThreadID: thread, Wait: api.Duration(timeout),
			}

			var snap api.Snapshot
			if err := call(cmd, info, timeout+callSlack, api.MethodRunUntil, params, &snap); err != nil {
				return err
			}

			return writeSnapshot(cmd.OutOrStdout(), snap, g.json, workDir())
		},
	}

	cmd.Flags().StringVar(&cond, "if", "", "stop there only when this expression is true")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultExecTimeout, "longest time to wait for the program to stop or exit")
	cmd.Flags().IntVar(&thread, "thread", 0, "thread id to resume (default: the thread that stopped)")
	dump = addDumpFlag(cmd)

	return cmd
}

func newBreakpointListCommand(info version.Info, g *globals) *cobra.Command {
	var mine bool

	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List breakpoints",
		Long: `List the session's breakpoints, every client's (--mine: only yours): id, owner (the client that
added it), file:line (and the requested line if the adapter moved it), condition, and whether each
is verified. A note says when a breakpoint shares its line with another client's breakpoint whose
condition differs (it then stops unconditionally).

Read-only; returns at once. Prints nothing when there are none.` + sessionHelp,
		Example: `  eyedbg bp ls
  eyedbg bp ls --mine
  eyedbg bp ls --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var bps []api.Breakpoint
			if err := call(cmd, info, daemonCallTimeout, api.MethodBreakpointLs,
				api.BreakpointListParams{SessionRef: g.ref(), Mine: mine}, &bps); err != nil {
				return err
			}

			return writeBreakpoints(cmd.OutOrStdout(), bps, g.json, workDir())
		},
	}

	cmd.Flags().BoolVar(&mine, "mine", false, "list only your breakpoints (see --as)")

	return cmd
}

func newBreakpointRemoveCommand(info version.Info, g *globals) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "rm <id|all>",
		Short: "Remove a breakpoint, or all of yours",
		Long: `Remove the breakpoint with the given id (from 'eyedbg bp ls'), or with "all" every breakpoint of
yours (--as), including your run-until breakpoints. Another client's breakpoint is refused with
NOT_OWNER (exit 2); --force removes it anyway, and with "all" removes everyone's. Removing all never
fails for finding none: it says how many were removed and how many of other clients were kept.

Does not resume the program and needs no lease; returns at once. Exits 1 if there is no such
breakpoint.` + sessionHelp,
		Example: `  eyedbg bp rm 2
  eyedbg bp rm all
  eyedbg bp rm all --force   # every client's`,
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
				api.BreakpointRemoveParams{SessionRef: g.ref(), ID: id, Force: force}, &res); err != nil {
				return err
			}

			return writeRemoved(cmd.OutOrStdout(), res, g.json)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "remove other clients' breakpoints too")

	return cmd
}

// lostSessions lists the sessions a crashed daemon left, read from the
// runtime directory (no daemon runs).
func lostSessions() ([]api.SessionInfo, error) {
	p, err := daemon.DefaultPaths()
	if err != nil {
		return nil, err
	}

	list, err := daemon.NewFileStore(p.Sessions, 0).Lost()
	if err != nil {
		return nil, err
	}

	for i := range list {
		list[i].State = api.StateLost
	}

	return list, nil
}

// forgetLost forgets lost session id with no daemon running; if there is no
// such lost session it returns notFound, the error that led here.
func forgetLost(id string, notFound error) (api.SessionInfo, error) {
	list, err := lostSessions()
	if err != nil {
		return api.SessionInfo{}, err
	}

	for i := range list {
		if list[i].ID != id {
			continue
		}

		p, err := daemon.DefaultPaths()
		if err != nil {
			return api.SessionInfo{}, err
		}

		if err := daemon.NewFileStore(p.Sessions, 0).Remove(id); err != nil {
			return api.SessionInfo{}, err
		}

		return list[i], nil
	}

	return api.SessionInfo{}, notFound
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
