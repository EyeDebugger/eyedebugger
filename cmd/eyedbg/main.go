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
	code := cli.Run(ctx, cli.NewEyedbgCommand(version.Get()), os.Args[1:])

	stop()
	os.Exit(code)
}
