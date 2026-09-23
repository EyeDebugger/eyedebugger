// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Driver turns a language-level launch request into an adapter to run and
// the DAP launch arguments to send it (docs/DESIGN.md §7). Drivers are
// registered explicitly by the daemon's wiring.
type Driver interface {
	// Name is the language name used on the command line ("dotnet").
	Name() string
	// Prepare builds the program if needed and describes how to debug it.
	// Build failures should be returned with enough output to act on.
	Prepare(ctx context.Context, spec LaunchSpec) (Launch, error)
}

// LaunchSpec is what the user asked to debug (see [api.LaunchSpec]).
type LaunchSpec = api.LaunchSpec

// Launch is a driver's answer: the adapter process and the launch request.
type Launch struct {
	// Adapter is the adapter executable; it speaks DAP on stdio.
	Adapter     string
	AdapterArgs []string
	// AdapterEnv is added to the daemon's environment for the adapter.
	AdapterEnv []string
	// AdapterID is sent in the DAP initialize request.
	AdapterID string
	// Arguments is the adapter-specific body of the DAP launch request.
	Arguments map[string]any
	// Program is a human-readable description of what runs, for status.
	Program string
}
