// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

func newAttachCommand(info version.Info, g *globals) *cobra.Command {
	var (
		sf  sessionFlags
		pid int
	)

	cmd := &cobra.Command{
		Use:   "attach <lang> --pid N",
		Short: "Debug a program that is already running",
		Long: `Attach the debugger to a running process, creating a new session, as 'eyedbg start' would for a
program it launches. Languages: dotnet (a .NET program whose runtime has started, via netcoredbg).
Only your own processes: another user's is refused (ATTACH_FAILED, exit 3). The session shows the
process as "pid N (name)", never its command line.

Breakpoints, exceptions, the lease policy and recording work as for 'eyedbg start' (--bp,
--exceptions, --lease-policy, --no-record); with --bp, attach waits (up to --timeout) for the
first stop. When the adapter can't attach it is ATTACH_FAILED with the likely causes: for .NET
the process must be yours, not already under a debugger, not started with
DOTNET_EnableDiagnostics=0, and see the same TMPDIR as eyedbgd.

The program is never killed by eyedbg: 'eyedbg detach' (or 'eyedbg stop') ends the session and
leaves it running. Starts the daemon if needed.

Output: the session's state, like 'eyedbg start'. Exits 1 for no such process.` + dumpHelp,
		Example: `  eyedbg attach dotnet --pid 4242
  eyedbg attach dotnet --pid 4242 --bp 'Worker.cs@"ProcessNext()"'
  eyedbg attach dotnet --pid 4242 --exceptions all --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if pid < 1 {
				return errors.New("attach needs --pid N (a process id from 1)")
			}

			params := api.StartParams{Lang: args[0], Attach: &api.AttachSpec{PID: pid}}
			if err := sf.params(g, &params); err != nil {
				return err
			}

			return startSession(cmd, info, g, params, sf.timeout)
		},
	}

	cmd.Flags().IntVar(&pid, "pid", 0, "id of the process to attach to")
	sf.register(cmd)

	return cmd
}

func newDetachCommand(info version.Info, g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "detach",
		Short: "End an attached session, leaving its program running",
		Long: `Detach from a session made by 'eyedbg attach': the debugger lets go (breakpoints stop applying,
a stopped program resumes), the session ends, and the program keeps running. Sessions made by
'eyedbg start' or 'eyedbg test' can't be detached from (INVALID_REQUEST, exit 1): 'eyedbg stop'
ends them. While the program is live this needs the control lease (LEASE_HELD, exit 2, if the
policy keeps you from taking it).

Output: "session ID detached; pid N keeps running". Returns within a few seconds.` + sessionHelp,
		Example: `  eyedbg detach
  eyedbg detach -s s-k3f9 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res api.SessionInfo
			if err := call(cmd, info, daemonCallTimeout, api.MethodSessionDetach, g.ref(), &res); err != nil {
				return err
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Schema  int             `json:"schema"`
					Session api.SessionInfo `json:"session"`
				}{jsonSchemaVersion, res})
			}

			return writeEnded(cmd.OutOrStdout(), res)
		},
	}
}

// writeEnded says a session ended, or that it was detached from and its
// program keeps running (not when the program had exited by itself).
func writeEnded(w io.Writer, info api.SessionInfo) error {
	if info.Mode != api.ModeAttach || !strings.HasPrefix(info.EndReason, "detached by") {
		return writeText(w, "session "+info.ID+" ended\n")
	}

	return writeText(w, "session "+info.ID+" detached; pid "+strconv.Itoa(info.PID)+" keeps running\n")
}
