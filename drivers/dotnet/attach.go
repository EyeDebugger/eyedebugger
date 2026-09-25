// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// attachHint lists why attaching to a .NET process fails. Both adapters
// attach through the runtime's debugger transport (pipes in TMPDIR), not
// ptrace.
const attachHint = "the process must be a .NET (Core) program of yours whose runtime has started, " +
	"not already under a debugger, not run with DOTNET_EnableDiagnostics=0, " +
	"and with the same TMPDIR as eyedbgd"

// PrepareAttach implements session.Attacher: the adapter attaches to spec's
// process.
func (d *Driver) PrepareAttach(ctx context.Context, spec api.AttachSpec) (session.Launch, error) {
	launch, err := d.adapterLaunch(ctx)
	if err != nil {
		return session.Launch{}, err
	}

	launch.Request = session.RequestAttach
	launch.PID = spec.PID
	launch.Arguments = attachArguments(spec.PID)

	return launch, nil
}

// attachArguments builds the attach request (netcoredbg's; SharpDbg takes
// the same); it reads only processId (justMyCode stays on).
func attachArguments(pid int) map[string]any {
	return map[string]any{
		"name":       "eyedbg",
		"type":       adapterType,
		"request":    "attach",
		"processId":  pid,
		"justMyCode": true,
	}
}
