// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

const dapLong = `Speak the Debug Adapter Protocol (DAP) on stdin and stdout, joined to a running debug session,
so an editor or any DAP client can debug it alongside you: VS Code (through the EyeDebugger
extension), nvim-dap, or a script. Use it as a DAP client's debug adapter command; it is not for
typing at a shell.

It connects to the running daemon (it never starts one), authenticates like every command, and
joins the session chosen by -s (else $EYEDBG_SESSION, else the only session) as the client --as
(else $EYEDBG_CLIENT, else agent); an editor usually passes --as human:NAME. From then on it only
copies bytes between stdio and the daemon, which serves DAP for that session: the client attaches
(launch is refused: start the session with 'eyedbg start'), sees the current stop, threads, stack,
variables and output, and gets every later stop.

The client acts under the same rules as the CLI: continue, steps, pause, terminate, setting a
variable and evaluating in the debug console (repl) are execution requests that need the control
lease ('eyedbg lease --help'; refused with LEASE_HELD); watch and hover evaluation are reads that
refuse side effects. Its breakpoints and exception filters are its own ('eyedbg bp ls' shows them
under its client id) and are removed when it disconnects; the lease stays with whoever holds it.
Disconnecting never ends the session; terminate does, like 'eyedbg stop'. Everything the client
does shows in 'eyedbg events'.

Blocks until the DAP client disconnects or closes stdin (then the daemon removes what the client
set and closes the connection), or the daemon goes away. stdout carries DAP only: every error,
also with --json, goes to stderr.

Exit codes: 0 the DAP client ended the connection; 1 the connection to eyedbgd was lost, or a usage
error; 2 no such session, or it has exited (NO_SESSION, SESSION_EXITED); 3 the daemon is too old
for 'eyedbg dap' or refuses the token (VERSION_MISMATCH, UNAUTHORIZED).`

const dapExample = `  eyedbg dap -s s-7f3k --as human:ana           # what an editor runs as its debug adapter

  -- nvim-dap: join the running session as a human
  dap.adapters.eyedbg = { type = 'executable', command = 'eyedbg', args = { 'dap', '--as', 'human:ana' } }
  dap.configurations.python = { { type = 'eyedbg', request = 'attach', name = 'Join eyedbg session' } }`

// dapOpenTimeout bounds connecting to the daemon and switching to DAP.
const dapOpenTimeout = 30 * time.Second

func newDapCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
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

			return runDap(cmd.Context(), info, g.ref(), in, out)
		},
	}
}

// runDap switches a daemon connection to DAP for ref and bridges it to
// in and out until either side ends.
func runDap(ctx context.Context, info version.Info, ref api.SessionRef, in io.Reader, out io.Writer) error {
	openCtx, cancel := context.WithTimeout(ctx, dapOpenTimeout)
	defer cancel()

	cl, err := dialRunning(openCtx, info)
	if err != nil {
		return err
	}

	var res api.FacadeOpenResult
	if err := cl.Call(openCtx, api.MethodFacadeOpen, api.FacadeOpenParams{SessionRef: ref}, &res); err != nil {
		_ = cl.Close()

		return openError(err)
	}

	r, conn := cl.Detach()

	return bridge(ctx, in, out, r, conn)
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
