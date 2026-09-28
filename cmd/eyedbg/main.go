// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Command eyedbg is documented in docs/DESIGN.md.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/eyedebugger/eyedebugger/internal/cli"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)

	cancelStdin := func() {}
	if os.Getenv(cli.EnvCancelOnStdinEOF) == "1" {
		ctx, cancelStdin = cli.CancelOnEOF(ctx, os.Stdin)
	}

	code := cli.Run(ctx, cli.NewEyedbgCommand(version.Get()), os.Args[1:])

	cancelStdin()
	stop()
	os.Exit(code)
}
