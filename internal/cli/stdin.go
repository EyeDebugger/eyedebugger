// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// EnvCancelOnStdinEOF is the opt-in env var (main.go) that makes eyedbg
// treat stdin EOF, or a read error on it, like Ctrl-C: it cancels the root
// context the same way os.Interrupt does. Off by default — an agent's
// shell commonly gives stdin at EOF (/dev/null), which would cancel every
// command at once — a tool that can't send Ctrl-C (the VS Code extension,
// on Windows) sets it and closes stdin to cancel instead. It never applies
// to 'eyedbg dap': that command already ends on its own stdin's EOF (it is
// the DAP stream, not a control channel), so wrapping it too would race a
// second reader against the DAP framing (F5).
const EnvCancelOnStdinEOF = "EYEDBG_CANCEL_ON_STDIN_EOF"

// CancelOnEOF returns a context derived from parent that is canceled the
// same way parent's own cancellation is (context.Canceled) once r's Read
// returns EOF or any other error. It takes a reader rather than os.Stdin
// itself so it can be tested with an io.Pipe, not a global.
//
// The draining goroutine blocks on r.Read until that happens or parent
// ends first; either way it exits on its own and never keeps the process
// alive or delays its exit (a process exit closes the reader out from
// under a still-blocked Read on a real stdin pipe).
func CancelOnEOF(parent context.Context, r io.Reader) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	go func() {
		buf := make([]byte, 512)

		for {
			if _, err := r.Read(buf); err != nil {
				cancel()

				return
			}

			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()

	return ctx, cancel
}

// WrapCancelOnEOF reads EnvCancelOnStdinEOF and always clears it (main.go)
// so an autostarted daemon, the adapters it launches and the debuggee never
// inherit it (they all build their environment from this process's, F5),
// then returns ctx wrapped by [CancelOnEOF] on root's stdin — unless the
// var wasn't "1", or args resolve to 'eyedbg dap', which already ends on
// its own stdin's EOF and would otherwise race a second reader against the
// DAP framing. root.InOrStdin() rather than os.Stdin directly, so a caller
// that set root.SetIn (a test) is honored the same way the command itself
// is.
func WrapCancelOnEOF(ctx context.Context, root *cobra.Command, args []string) (context.Context, context.CancelFunc) {
	enabled := os.Getenv(EnvCancelOnStdinEOF) == "1"

	_ = os.Unsetenv(EnvCancelOnStdinEOF)

	if !enabled || isDapInvocation(root, args) {
		return ctx, func() {}
	}

	return CancelOnEOF(ctx, root.InOrStdin())
}

// isDapInvocation reports whether args resolve to root's own 'dap'
// sub-command (not a deeper one of the same name, if root ever grows one).
func isDapInvocation(root *cobra.Command, args []string) bool {
	cmd, _, err := root.Find(args)

	return err == nil && cmd != nil && cmd.Name() == dapCommandName && cmd.Parent() == root
}
