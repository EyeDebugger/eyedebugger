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

	// Request is the DAP request that starts debugging: "launch" (or
	// empty) or "attach".
	Request string
	// PID is the process an attach request attaches to.
	PID int
	// ExceptionFilters maps each exception mode to the adapter's exception
	// filter ids; a mode missing from it uses its own name as the id.
	ExceptionFilters map[api.ExceptionMode][]string
	// SideEffects finds what in expr would visibly change the program (a
	// method call, an assignment): eval refuses such an expression without
	// allowSideEffects. Nil checks nothing.
	SideEffects func(expr string) (reason string, found bool)
	// AttachHint is the hint of an ATTACH_FAILED error.
	AttachHint string
}

// Request kinds for [Launch.Request].
const (
	RequestLaunch = "launch"
	RequestAttach = "attach"
)

// Attacher is a [Driver] that can attach to a running process.
type Attacher interface {
	// PrepareAttach describes how to attach to spec's process.
	PrepareAttach(ctx context.Context, spec api.AttachSpec) (Launch, error)
}

// TestSpec is a test run to debug: the project (in LaunchSpec's Project),
// its environment, and which tests.
type TestSpec struct {
	api.LaunchSpec
	api.TestSpec
}

// TestCommand is how a [Tester] runs tests: a command that starts a test
// host, prints its process id and waits for a debugger to attach.
type TestCommand struct {
	Path string
	Args []string
	// Env is added to the daemon's environment (and the spec's Env after
	// it).
	Env []string
	Dir string
	// Program describes the run for status, e.g. "dotnet test tests.csproj
	// --filter Adds".
	Program string
	// HostPID finds the test host's process id in one line of the command's
	// standard output.
	HostPID func(line string) (pid int, ok bool)
	// Failure is the error for a command that exited with code before a
	// test host appeared; output is the end of what it printed.
	Failure func(code int, output string) error
}

// Tester is a [Driver] that can debug test runs. A Tester must also be an
// [Attacher]: the session attaches to the test host.
type Tester interface {
	// TestCommand describes how to run spec's tests.
	TestCommand(ctx context.Context, spec TestSpec) (TestCommand, error)
}
