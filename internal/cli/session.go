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
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()

	cl, err := dialRunning(ctx, info)
	if err != nil {
		return err
	}
	defer cl.Close()

	return cl.Call(ctx, method, params, result)
}

// dialRunning connects to the running daemon (never starting one), as
// call does: no daemon is NO_SESSION (also matching errNoDaemon), another
// protocol version is VERSION_MISMATCH.
func dialRunning(ctx context.Context, info version.Info) (*daemon.Client, error) {
	p, err := daemon.DefaultPaths()
	if err != nil {
		return nil, err
	}

	cl, err := daemon.Dial(ctx, p, info)
	if api.CodeOf(err) == api.CodeDaemonNotRunning {
		return nil, errors.Join(api.NewError(api.CodeNoSession, "there are no debug sessions (the daemon is not running)",
			"start one with 'eyedbg start'"), errNoDaemon)
	}

	if err != nil {
		return nil, err
	}

	if cl.Hello.ProtocolVersion != api.ProtocolVersion {
		_ = cl.Close()

		return nil, api.NewError(api.CodeVersionMismatch,
			fmt.Sprintf("eyedbgd %s speaks protocol %d but this eyedbg speaks %d", cl.Hello.Version, cl.Hello.ProtocolVersion, api.ProtocolVersion),
			"stop it with 'eyedbg daemon stop' (--force ends its sessions); 'eyedbg start' then starts a matching one")
	}

	return cl, nil
}

// envNoRecord, when "1", turns recording off for new sessions.
const envNoRecord = "EYEDBG_NO_RECORD"

// sessionFlags are the flags every command that creates a session has
// (start, attach, test).
type sessionFlags struct {
	leasePolicy, exceptions string
	bps                     []string
	noRecord                bool
	timeout                 time.Duration
	dump                    *dumpFlag
}

// register adds the flags to cmd.
func (sf *sessionFlags) register(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringArrayVar(&sf.bps, "bp", nil, `breakpoint FILE:LINE, FILE@"TEXT" or func:NAME set before the program runs (repeatable)`)
	f.StringVar(&sf.exceptions, "exceptions", "", "which exceptions stop the program for you: all, uncaught or none (see 'eyedbg bp exceptions')")
	f.StringVar(&sf.leasePolicy, "lease-policy", string(api.LeaseFree), "who may take the control lease: free, handoff or human-priority")
	f.BoolVar(&sf.noRecord, "no-record", false, "don't record the session's control events (also EYEDBG_NO_RECORD=1)")
	f.DurationVar(&sf.timeout, "timeout", defaultExecTimeout, "how long to wait for the first stop when there are --bp breakpoints (or --stop-on-entry)")
	sf.dump = addDumpFlag(cmd)
}

// params fills in what the flags set of a start request, for client g.
func (sf *sessionFlags) params(g *globals, p *api.StartParams) error {
	policy, err := api.ParseLeasePolicy(sf.leasePolicy)
	if err != nil {
		return err
	}

	if sf.exceptions != "" {
		if p.Exceptions, err = api.ParseExceptionMode(sf.exceptions); err != nil {
			return err
		}
	}

	p.Client, p.LeasePolicy, p.Wait = g.clientID(), policy, api.Duration(sf.timeout)
	p.NoRecord = sf.noRecord || os.Getenv(envNoRecord) == "1"

	for _, b := range sf.bps {
		spec, err := parseLocation(b)
		if err != nil {
			return err
		}

		p.Breakpoints = append(p.Breakpoints, spec)
	}

	p.DumpSpec, err = sf.dump.spec(g)

	return err
}

// startFlags are the flags of 'eyedbg start'.
type startFlags struct {
	sessionFlags

	project, program, cwd string
	env, opts             []string
	stopOnEntry, noBuild  bool
}

// startLong is the long help of Start.
const startLong = `Build (unless --program or --no-build) and start a program under the debugger, creating a new
session. Languages: dotnet (via netcoredbg; install it once with 'eyedbg adapters install
netcoredbg'), python (via debugpy: 'eyedbg adapters install python', or your interpreter's
own debugpy), c, cpp and rust (via LLVM's lldb-dap: set EYEDBG_LLDB_DAP if it isn't on PATH), and
go (via Delve: 'eyedbg adapters install delve', or dlv on PATH or EYEDBG_DLV);
'eyedbg adapters ls' lists every language, including ones your own adapter manifests add.
Everything after "--" is passed to the program.

For dotnet: --project takes a project file or a directory with exactly one project (default: the
current directory), which is built in Debug; --program takes an already-built .dll (or apphost)
and skips the build. dotnet takes no --opt options.

For python: --program takes the script, or --opt module=NAME runs a module like 'python -m NAME'
(e.g. pytest). Nothing is built (--project and --no-build are ignored). The working directory
defaults to where you run eyedbg, as with 'python app.py'. The interpreter, which runs both
debugpy and your program, is --opt python=PATH (relative to the current directory), else
EYEDBG_PYTHON (as the daemon was started with), else your active $VIRTUAL_ENV, else a .venv or
venv (with pyvenv.cfg) in the working directory or in the program's directory or its parents (only
directories you own, and a venv others can write is refused), else python3, python or 'py -3' on
PATH; it needs Python 3.10+.
--opt justMyCode=false also stops and steps in library code. Child processes the program
starts run, but are not debugged. Language options are NAME=VALUE (--opt, repeatable); an
unknown one is an error that lists the language's options.

For c, cpp and rust: --program takes an already-built native binary (compile with debug info, e.g.
'cc -g -O0', 'c++ -g -O0' or 'rustc -g'); nothing is built (--project and --no-build are ignored),
and they take no --opt options. The working directory defaults to where you run eyedbg. Rust's
String, Vec and HashMap show raw layouts unless its LLDB formatters are imported (two lines in
~/.lldbinit, from 'rustc --print sysroot'). If the binary was built under a symlinked directory
(e.g. macOS's /tmp or /var), lldb-dap's breakpoints won't bind: build outside a symlinked path, or
resolve it yourself first (e.g. 'cd "$(cd -P dir && pwd)"').

For go: --program takes a package directory or a .go file, built by Delve itself (nothing is built
ahead of time); --cwd also sets where that build runs. --opt mode=debug (the default) builds and
runs it; --opt mode=exec instead takes an already-built binary in --program (build it yourself
first with 'go build -gcflags=all=-N -l' so breakpoints and locals are reliable); --opt mode=test
runs the package's tests in --cwd, not --program's directory. --opt buildFlags='FLAGS' adds extra
'go build' flags (e.g. -tags=integration).

Breakpoints given with --bp are set before the program runs, so they can't be missed; --bp takes
FILE:LINE, FILE@"TEXT" or func:NAME (see 'eyedbg bp add'; conditions, hit counts and logpoints
need 'eyedbg bp add'). --exceptions all|uncaught|none chooses which exceptions stop the program
(see 'eyedbg bp exceptions'). Add more later with 'eyedbg bp add'. With --stop-on-entry or --bp,
start waits (up to --timeout) for the first stop and reports where the program stopped;
otherwise it returns as soon as the program runs. Starts the daemon if needed. Building can take
minutes on a cold machine. To debug a program that already runs, see 'eyedbg attach'; to debug
tests, 'eyedbg test'.

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
`

func newStartCommand(info version.Info, g *globals) *cobra.Command {
	var sf startFlags

	cmd := &cobra.Command{
		Use:   "start <lang> [flags] [-- program args...]",
		Short: "Start a program under the debugger",
		Long:  startLong,
		Example: `  eyedbg start dotnet --bp Program.cs:12         # build ./, stop at line 12
  eyedbg start dotnet --bp 'Program.cs@"return total"' --exceptions all
  eyedbg start dotnet --project src/App/App.csproj --stop-on-entry
  eyedbg start dotnet --program bin/Debug/net10.0/App.dll -- --verbose input.txt
  eyedbg start python --program app.py --bp app.py:12 -- --verbose
  eyedbg start python --opt module=pytest --bp tests/test_x.py:8 -- -x tests/test_x.py
  eyedbg start python --program app.py --opt python=.venv/bin/python --opt justMyCode=false
  eyedbg start c --program ./bin/app --bp main.c:12
  eyedbg start go --program . --cwd . --bp main.go:12`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params, err := sf.params(args, cmd.ArgsLenAtDash())
			if err != nil {
				return err
			}

			if err := sf.sessionFlags.params(g, &params); err != nil {
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
	f.StringVar(&sf.cwd, "cwd", "", "working directory of the program (default: dotnet the project's directory, python/c/cpp/rust/go the current one)")
	f.StringArrayVar(&sf.env, "env", nil, "environment variable KEY=VALUE for the program (repeatable)")
	f.BoolVar(&sf.stopOnEntry, "stop-on-entry", false, "stop at the program's entry point")
	f.BoolVar(&sf.noBuild, "no-build", false, "don't build; requires --program")
	f.StringArrayVar(&sf.opts, "opt", nil, "language option NAME=VALUE, e.g. module=pytest for python (repeatable; see 'eyedbg adapters ls')")
	sf.sessionFlags.register(cmd)
}

// params turns the launch flags and arguments (<lang> [-- program args])
// into a start request, resolving paths against the working directory.
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
			Args: progArgs, NoBuild: sf.noBuild, StopOnEntry: sf.stopOnEntry, ClientDir: workDir(),
			VirtualEnv: absPath(os.Getenv("VIRTUAL_ENV")),
		},
	}

	var err error
	if params.Env, err = parseEnv(sf.env); err != nil {
		return api.StartParams{}, err
	}

	if params.Options, err = parseOpts(sf.opts); err != nil {
		return api.StartParams{}, err
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
exited, lost), who holds its control lease, which clients have an editor connected ('eyedbg dap';
CONNECTED, "-" for none), and program, under a header line. Exited sessions stay
listed, with their exit code, until 'eyedbg stop'. A lost session belonged to a daemon that exited
while it was live: only its metadata and recording are left, and 'eyedbg stop -s ID' forgets it.
With no daemon running, the lost sessions are read from the runtime directory.

Never starts the daemon or affects a session. Prints nothing when there are none (an empty
"sessions" array in --json) and exits 0. --json adds each session's lease (with pending
requests), clients (who used it, first and last seen, "connected": open editor connections) and
recording file.`,
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
the thread, the current function and file:line, and the source lines around it. At an exception
stop, also the exception: its type and message, the first 5 lines of its stack trace (eval
'$exception.StackTrace' for the rest) and its inner exceptions, when the adapter can tell. When
exited, the exit code.

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
		Short: "End a debug session (kills the program; detaches from an attached one)",
		Long: `End a session: the program is terminated if it is still running, the debug adapter exits, and
the session is removed from 'eyedbg sessions'. A session made by 'eyedbg attach' is detached from
instead: that program keeps running (like 'eyedbg detach'). A test run ('eyedbg test') is ended:
'dotnet test' and its test host are killed. Use it when done debugging; the daemon exits by
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

			return writeEnded(cmd.OutOrStdout(), info2)
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
grouped by scope (e.g. Locals; for python Locals and Globals, where special __dunder__ and
function variables are hidden and class variables grouped). --depth expands objects and collections that many levels (1 = only
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
	var p api.EvalParams

	cmd := &cobra.Command{
		Use:   "eval <expression>",
		Short: "Evaluate an expression in a stack frame",
		Long: `Evaluate an expression in the context of a frame of the stopped thread (default: #0) and print
its value and type. For dotnet, netcoredbg evaluates C# expressions: operators, member and index
access, method calls and casts; lambdas and LINQ with lambdas are not supported. At an exception
stop, $exception is the exception (e.g. '$exception.StackTrace'). For python, debugpy evaluates
Python expressions (a string's value is its repr, e.g. 'ab'). For c, cpp and rust, lldb-dap
evaluates expressions with LLDB's own parser (variables, casts, member and index access, most
operators). For go, Delve evaluates Go expressions; a bare call like 'price(1)' does not actually
run the function (Delve needs a 'call ' prefix, e.g. 'call price(1)', to do that), but is still
refused as a side effect since eyedbg can't tell the two apart from the text alone.
--depth N also shows the result's members, N-1 levels deep, cut to --budget tokens.

Side effects: an expression that visibly changes the program is refused with SIDE_EFFECTS (exit 1)
unless you pass --allow-side-effects: for dotnet a method call, new, an assignment, ++ or --, an
interpolated string; for python a call (except read-only builtins such as len, str, repr, type,
isinstance), = or :=, an f-string; for c and cpp an assignment, ++, --, or any call (there is no
safe-call list: even 'strlen(x)' runs code in the target); rust the same, without ++ or --; for go
an assignment (= or :=) or a call other than a built-in (len, cap, min, max, real, imag, complex)
or a type conversion (int, string, ...). That
flag makes the eval an execution request: it needs the
control lease (see below) and is logged in 'eyedbg events'. The check is best-effort, a scan of
the expression's text: a property getter, indexer or operator still runs code in the program
without it (no adapter can evaluate without running code), so a getter with side effects is
not caught, and for dotnet a delegate called through a parenthesized name, '(f)(1)', passes as a
cast.

An evaluation still running when another client resumes the program is canceled (ADAPTER_ERROR).
Needs a stopped program; returns at once.` + leaseHelp + sessionHelp,
		Example: `  eyedbg eval total
  eyedbg eval 'order.Items.Count * 2' --frame 1
  eyedbg eval order --depth 2
  eyedbg eval 'Orders.Price(2)' --allow-side-effects`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p.SessionRef, p.Expression, p.Budget = g.ref(), args[0], g.budget

			var res api.EvalResult
			if err := call(cmd, info, daemonCallTimeout, api.MethodEval, p, &res); err != nil {
				return err
			}

			return writeEval(cmd.OutOrStdout(), res, g.json)
		},
	}

	cmd.Flags().IntVar(&p.Frame, "frame", 0, "frame number from 'eyedbg stack' (0 = innermost)")
	cmd.Flags().IntVar(&p.Depth, "depth", 1, "levels of the result's members to show (1 = none)")
	cmd.Flags().BoolVar(&p.AllowSideEffects, "allow-side-effects", false, "evaluate even if it calls methods or assigns (takes the lease)")

	return cmd
}

func newOutputCommand(info version.Info, g *globals) *cobra.Command {
	var since, tail int

	cmd := &cobra.Command{
		Use:   "output",
		Short: "Show what the program printed",
		Long: `Show the program's output (stdout, stderr and debugger console messages), as captured by the
debugger, and what logpoints printed (category "logpoint" in --json; see 'eyedbg bp add --log').
In a test session ('eyedbg test') it is what 'dotnet test' printed. The last 2000 chunks are kept per session. Each chunk has a sequence number: pass the
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
		Short: "Add, list and remove breakpoints; choose exception stops",
		Long: `Manage a session's breakpoints: at a line (FILE:LINE), at the line holding some text
(FILE@"TEXT"), or at a function (func:NAME); with conditions, hit counts or log messages
(logpoints). 'eyedbg bp exceptions' chooses which exceptions stop the program. Breakpoints can be
added while the program runs or is stopped; to be sure one is hit early in the program, pass it to
'eyedbg start --bp' instead.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`,
		Example: `  eyedbg bp add Program.cs:12
  eyedbg bp add 'Program.cs@"total += price"' --log 'total={total}'
  eyedbg bp exceptions all
  eyedbg bp ls
  eyedbg bp rm 1`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newBreakpointAddCommand(info, g), newBreakpointListCommand(info, g), newBreakpointRemoveCommand(info, g),
		newExceptionsCommand(info, g))

	return cmd
}

// bpAddLong is the long help of BreakpointAdd.
const bpAddLong = `Add a breakpoint. LOCATION is one of:
  FILE:LINE     a line (FILE relative to the current directory, or absolute: a bare file name
                is looked for in the current directory only, and a file that isn't there is
                INVALID_REQUEST, exit 1);
  FILE@"TEXT"   the one line of FILE that holds TEXT (case-sensitive; runs of whitespace match
                one space), e.g. 'Program.cs@"total += price"' (quote it for the shell). It is
                found once, now: several matching lines is ANCHOR_AMBIGUOUS and none is
                ANCHOR_NOT_FOUND with the closest lines (both exit 1). If the file changes later,
                the breakpoint stays on that line (the program still runs the code it was built
                from) and 'eyedbg bp ls' notes it; add it again to find the text anew;
  func:NAME     a function, by name or a dotted suffix of its full name (Price, Orders.Price or
                Ns.Orders.Price), bound when its module loads. Needs an adapter that has them
                (netcoredbg, debugpy, lldb-dap and Delve do; UNSUPPORTED_BY_ADAPTER, exit 4,
                otherwise). For python NAME is the bare function name (price, not Orders.price),
                and debugpy verifies any name, so a misspelled one is "verified" and never stops.
                For c, cpp and rust NAME is the bare function name (price, not the mangled
                symbol). For go NAME is package-qualified (main.price, not price).
The adapter may move a line breakpoint to the nearest line with code: the output shows the
requested and actual line, and whether it is verified. An unverified breakpoint is pending (e.g.
its module isn't loaded yet) and may still bind later; 'eyedbg bp ls' shows the current state.

--if EXPR stops only when EXPR, an expression in the program's language, is true there (e.g.
'i == 3'). It is evaluated each time the line runs, so it runs code in the program; an expression
that fails to evaluate stops the program.

--hit counts the times the line is reached with its --if true, and stops only at some: N (only
the Nth time), >=N (the Nth time and after) or %N (every Nth time). --log MESSAGE makes it a
logpoint: it never stops, and prints MESSAGE to the output (category "logpoint"; see 'eyedbg
output') each time, with every {EXPR} replaced by its value ({{ and }} print braces); --hit then
chooses which times log. Both are for FILE:LINE and FILE@"TEXT" breakpoints. eyedbg does the
counting and logging itself (netcoredbg can't): each time the line is reached the program pauses
briefly, and eyedbg continues it. A step (or a pause) that reaches such a line stops there, even
when it wouldn't stop the program otherwise. 'eyedbg bp ls' shows the counts. Don't put one on
the first line of a function that has a func: breakpoint: eyedbg can't tell that function
breakpoint's stops from the line's, so its --hit or --log decides them too.

Adding a breakpoint where you already have one replaces its condition, hit count and log message
(and restarts its count). The breakpoint is yours (--as): only you remove it, unless 'bp rm
--force'. Other clients' breakpoints at the same line share it: the program stops there if any
of them would (a breakpoint without a condition wins; different conditions make it stop
unconditionally, which 'eyedbg bp ls' notes, except while some breakpoint has --hit or --log:
then each one's condition is checked). Needs no lease.

Does not resume the program; returns at once.` + sessionHelp

func newBreakpointAddCommand(info version.Info, g *globals) *cobra.Command {
	var cond, hit, logMsg string

	cmd := &cobra.Command{
		Use:   "add <location>",
		Short: "Add a breakpoint, logpoint or function breakpoint",
		Long:  bpAddLong,
		Example: `  eyedbg bp add Program.cs:12
  eyedbg bp add Orders.cs:88 --if 'order.Total > 100'
  eyedbg bp add 'Program.cs@"total += price"' --hit '>=3'
  eyedbg bp add Program.cs:20 --log 'i={i} total={total}'
  eyedbg bp add func:Orders.Price --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			spec, err := parseLocation(args[0])
			if err != nil {
				return err
			}

			spec.Condition, spec.HitCondition, spec.LogMessage = cond, hit, logMsg

			var bp api.Breakpoint
			if err := call(cmd, info, daemonCallTimeout, api.MethodBreakpointAdd,
				api.BreakpointAddParams{SessionRef: g.ref(), BreakpointSpec: spec}, &bp); err != nil {
				return err
			}

			return writeBreakpoints(cmd.OutOrStdout(), []api.Breakpoint{bp}, g.json, workDir())
		},
	}

	cmd.Flags().StringVar(&cond, "if", "", "stop only when this expression is true")
	cmd.Flags().StringVar(&hit, "hit", "", "stop only at some hits: N, >=N or %N")
	cmd.Flags().StringVar(&logMsg, "log", "", "print this message (with {EXPR} values) instead of stopping")

	return cmd
}

// runUntilLong is the long help of RunUntil.
const runUntilLong = `Continue the stopped program until it reaches FILE:LINE (or FILE@"TEXT": the line holding TEXT,
see 'eyedbg bp add'); with --if EXPR, until it reaches it with EXPR true (an expression in the
program's language, e.g. 'i == 3' or 'order.Total > 100'; evaluating it runs code in the
program). Replaces "bp add, continue, bp rm" with one call.

It works through a temporary breakpoint, removed once the program stops or exits. The program may
stop elsewhere first (another breakpoint, an exception, a pause) or exit: the output says where it
is. If you already have a breakpoint at that line, that one is used and --if is ignored (unless it
is a logpoint or has --hit: then a temporary one is added beside it); another client's breakpoint
there is not (your temporary one shares the line with it).

Blocks until the program stops or exits, at most --timeout (default 30s). On a timeout the program
keeps running and the temporary breakpoint stays (marked in 'eyedbg bp ls') so 'eyedbg wait' can
still catch it; the next execution command, whoever sends it, removes it. Exits 2 if the program
isn't stopped or another client holds the lease (LEASE_HELD).` +
	leaseHelp + dumpHelp + sessionHelp

func newRunUntilCommand(info version.Info, g *globals) *cobra.Command {
	var (
		timeout time.Duration
		thread  int
		cond    string
		dump    *dumpFlag
	)

	cmd := &cobra.Command{
		Use:   "run-until <location>",
		Short: "Continue to a line, optionally until a condition holds there",
		Long:  runUntilLong,
		Example: `  eyedbg run-until Program.cs:20
  eyedbg run-until 'Program.cs@"return total"'
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
added it), where (file:line, with the anchor text it was found by; or func:NAME), the requested
line if the adapter moved it, condition, hit count and log message, whether each is verified, and
how many hits it counted (breakpoints with --hit or --log). A note says when a breakpoint shares
its line with another client's breakpoint whose condition differs (it then stops
unconditionally), or when its file changed after the session started (the program still runs the
code it was built from, so the line may not match the file any more).

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

// parseLocation parses FILE:LINE, FILE@"TEXT" (an anchor: the line holding
// TEXT) or func:NAME, resolving FILE against the working directory.
func parseLocation(s string) (api.BreakpointSpec, error) {
	if name, ok := strings.CutPrefix(s, "func:"); ok && name != "" && strings.Trim(name, "0123456789") != "" {
		return api.BreakpointSpec{Function: name}, nil
	}

	if at := strings.Index(s, `@"`); at > 0 {
		text, ok := strings.CutSuffix(s[at+2:], `"`)
		if !ok || strings.TrimSpace(text) == "" {
			return api.BreakpointSpec{}, fmt.Errorf(`invalid anchor in %q: want FILE@"TEXT" with TEXT from one line`, s)
		}

		return api.BreakpointSpec{File: absPath(s[:at]), Anchor: text}, nil
	}

	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return api.BreakpointSpec{}, fmt.Errorf(`invalid location %q: want FILE:LINE, FILE@"TEXT" or func:NAME`, s)
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

// parseOpts parses --opt NAME=VALUE flags; VALUE may be empty, a NAME
// only once.
func parseOpts(kvs []string) (map[string]string, error) {
	if len(kvs) == 0 {
		return nil, nil //nolint:nilnil // No options is a valid, empty result.
	}

	opts := make(map[string]string, len(kvs))

	for _, kv := range kvs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --opt %q: want NAME=VALUE", kv)
		}

		if _, dup := opts[k]; dup {
			return nil, fmt.Errorf("--opt %s is given twice", k)
		}

		opts[k] = v
	}

	return opts, nil
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

	// The daemon resolves symlinks in the paths it is given (as adapters
	// report them), so paths are shown relative to the resolved directory.
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		return resolved
	}

	return wd
}
