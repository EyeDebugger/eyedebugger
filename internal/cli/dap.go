// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/daemon"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

const dapLong = `Speak the Debug Adapter Protocol (DAP) on stdin and stdout, joined to a running debug session
or launching a new one, so an editor or any DAP client can debug it alongside you: VS Code (through
the EyeDebugger extension), nvim-dap, or a script. Use it as a DAP client's debug adapter command;
it is not for typing at a shell.

Without --launch it connects to the running daemon (it never starts one), authenticates like every
command, and joins the session chosen by -s (else $EYEDBG_SESSION, else the only session) as the
client --as (else $EYEDBG_CLIENT, else agent); an editor usually passes --as human:NAME. From then
on it only copies bytes between stdio and the daemon, which serves DAP for that session: the client
attaches (launch is refused there: use --launch), sees the output so far, the current stop,
threads, stack and variables, and gets every later stop.

With --launch the client's DAP launch starts the session, as 'eyedbg start' would, as the client
--as, who holds its lease; the daemon is started if needed (like 'eyedbg start'), and -s is refused
($EYEDBG_SESSION is ignored). Launch arguments: lang (required: dotnet, python, or a language of
your own manifest, see 'eyedbg adapters ls'), program, project, cwd (relative paths resolve against
this command's directory; neither program nor project builds the project in it), args (list),
env and opts (objects of strings: 'eyedbg start --env' and '--opt'), stopOnEntry, noBuild (true or
false), leasePolicy (free, handoff or human-priority), exceptions (all, uncaught or none) and
adapter (dotnet: netcoredbg or sharpdbg, else $EYEDBG_DOTNET_ADAPTER); other keys are ignored, and a
key that differs from one of these only in case is refused. The build's output streams to the
client's debug console. Once the adapter is ready the client gets an eyedbg/session event with the
session's id, a capabilities event, and initialized; its breakpoints and exception filters are in
place before the program runs. Stop (terminate) ends the session and forgets it, as 'eyedbg stop'
does; a Restart is a new launch, so the program really restarts. When this connection ends, the
session it launched is forgotten if its program has exited, and keeps running otherwise.

The client acts under the same rules as the CLI: continue, steps, pause, terminate, setting a
variable and evaluating in the debug console (repl) are execution requests that need the control
lease ('eyedbg lease --help'; refused with LEASE_HELD); watch and hover evaluation are reads that
refuse side effects. Its breakpoints and exception filters are its own ('eyedbg bp ls' shows them
under its client id, marked (editor)); its list never removes what the same client set with 'eyedbg
bp add'. Other clients' line breakpoints appear in the editor, marked at column 1 with whose they
are and their condition in the hover: removing one there only hides it, editing one (a condition)
makes a breakpoint of your own. When the client's last connection closes, its editor breakpoints
and exception mode are removed and its lease is released (a Restart keeps them for 10 s).
Disconnecting never ends the session; terminate does, like 'eyedbg stop'. Everything the client
does shows in 'eyedbg events' and 'eyedbg sessions' shows it connected. Custom eyedbg/* requests
and events carry the lease, clients and breakpoints for editor extensions (docs/adr/0014).

Blocks until the DAP client disconnects or closes stdin, or the daemon goes away. stdout carries
DAP only: every error, also with --json, goes to stderr.

Exit codes: 0 the DAP client ended the connection; 1 the connection to eyedbgd was lost, or a usage
error (--launch with -s); 2 no such session, or it has exited (NO_SESSION, SESSION_EXITED); 3 the
daemon is too old for 'eyedbg dap' or refuses the token (VERSION_MISMATCH, UNAUTHORIZED), or with
--launch it could not be started (DAEMON_START_FAILED, DAEMON_NOT_RUNNING with
EYEDBG_NO_AUTOSTART=1). A launch that fails (a build error, say) is the launch request's error on
the DAP connection, not an exit code.`

const dapExample = `  eyedbg dap -s s-7f3k --as human:ana           # what an editor runs as its debug adapter

  -- nvim-dap: join the running session as a human
  dap.adapters.eyedbg = { type = 'executable', command = 'eyedbg', args = { 'dap', '--as', 'human:ana' } }
  dap.configurations.python = { { type = 'eyedbg', request = 'attach', name = 'Join eyedbg session' } }

  eyedbg dap --launch --as human:ana             # an editor's adapter that launches the session

  -- nvim-dap: launch the current file through eyedbg
  dap.adapters['eyedbg-launch'] = { type = 'executable', command = 'eyedbg', args = { 'dap', '--launch', '--as', 'human:ana' } }
  dap.configurations.python = { { type = 'eyedbg-launch', request = 'launch', name = 'Debug with eyedbg',
    lang = 'python', program = '${file}', stopOnEntry = false } }`

// launchFacadeVersion is the first facade version with launch connections.
const launchFacadeVersion = 3

// dapOpenTimeout bounds connecting to the daemon and switching to DAP.
const dapOpenTimeout = 30 * time.Second

func newDapCommand(info version.Info, g *globals) *cobra.Command {
	var launch bool

	cmd := &cobra.Command{
		Use:     "dap",
		Short:   "Speak DAP to a debug session over stdio (for editors)",
		Long:    dapLong,
		Example: dapExample,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, out := cmd.InOrStdin(), cmd.OutOrStdout()
			// stdout is the DAP stream from here on: Run writes any error,
			// text or JSON, to stderr.
			cmd.Root().SetOut(cmd.ErrOrStderr())

			if !launch {
				return runDap(cmd.Context(), info, api.FacadeOpenParams{SessionRef: g.ref()}, in, out)
			}

			if g.session != "" {
				return errors.New("--launch starts a session: it can't join one (drop -s)")
			}

			return runDap(cmd.Context(), info, api.FacadeOpenParams{
				SessionRef: api.SessionRef{Client: g.clientID()}, Launch: dapLaunch(),
			}, in, out)
		},
	}

	cmd.Flags().BoolVar(&launch, "launch", false, "start the session with the DAP client's launch request instead of joining one (starts the daemon if needed)")

	return cmd
}

// dapLaunch is what a launch connection starts sessions with besides its
// launch arguments: what 'eyedbg start' adds from the environment.
func dapLaunch() *api.FacadeLaunch {
	return &api.FacadeLaunch{
		ClientDir: workDir(), VirtualEnv: absPath(os.Getenv("VIRTUAL_ENV")),
		NoRecord: os.Getenv(envNoRecord) == "1", DotnetAdapter: os.Getenv(envDotnetAdapter),
	}
}

// runDap switches a daemon connection to DAP with p and bridges it to in
// and out until either side ends. A launch connection starts the daemon
// if needed, as 'eyedbg start' does; joining one never does.
func runDap(ctx context.Context, info version.Info, p api.FacadeOpenParams, in io.Reader, out io.Writer) error {
	openCtx, cancel := context.WithTimeout(ctx, dapOpenTimeout)
	defer cancel()

	cl, err := dapConnect(openCtx, info, p.Launch != nil)
	if err != nil {
		return err
	}

	var res api.FacadeOpenResult
	if err := cl.Call(openCtx, api.MethodFacadeOpen, p, &res); err != nil {
		_ = cl.Close()

		return openError(err)
	}

	if p.Launch != nil && (res.SessionID != "" || res.FacadeVersion < launchFacadeVersion) {
		_ = cl.Close()

		return api.NewError(api.CodeVersionMismatch, "the running eyedbgd is too old for 'eyedbg dap --launch'",
			"stop it with 'eyedbg daemon stop' (--force ends its sessions); the next command starts a current one")
	}

	r, conn := cl.Detach()

	return bridge(ctx, in, out, r, conn)
}

// dapConnect connects to the daemon: the running one (see dialRunning), or
// for a launch one started if needed.
func dapConnect(ctx context.Context, info version.Info, launch bool) (*daemon.Client, error) {
	if !launch {
		return dialRunning(ctx, info)
	}

	p, err := daemon.DefaultPaths()
	if err != nil {
		return nil, err
	}

	cl, _, err := daemon.Connect(ctx, p, info, daemon.OptionsFromEnv())
	if err != nil {
		return nil, err
	}

	return cl, nil
}

// openError is facade.open's error as the command reports it: a daemon
// that doesn't know the method predates 'eyedbg dap'.
func openError(err error) error {
	if api.CodeOf(err) == api.CodeUnknownMethod {
		return api.NewError(api.CodeVersionMismatch, "the running eyedbgd is too old for 'eyedbg dap'",
			"stop it with 'eyedbg daemon stop' (--force ends its sessions); the next command starts a current one")
	}

	return err
}

// bridge copies in to conn and conn (through r, its reader) to out. When
// in ends, conn's write side is closed and the daemon's remaining output
// still reaches out; it returns once conn's read side ends: nil at a clean
// end, an error if the connection broke. The copy from in may outlive it,
// blocked in a read of in that can't be interrupted; it ends at the next
// read or write once conn is closed.
func bridge(ctx context.Context, in io.Reader, out io.Writer, r io.Reader, conn net.Conn) error {
	defer conn.Close()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	go func() {
		if _, err := io.Copy(conn, in); err == nil {
			closeWrite(conn)
		}
	}()

	_, err := io.Copy(out, r)

	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("dap: %w", ctx.Err())
	case err != nil && errors.Is(err, net.ErrClosed):
		return nil
	case err != nil:
		return fmt.Errorf("connection to eyedbgd lost: %w", err)
	default:
		return nil
	}
}

// closeWrite tells the daemon the DAP client is done: it shuts down the
// write side (the daemon still sends what is left), else closes conn.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
		return
	}

	_ = conn.Close()
}
