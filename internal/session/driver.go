// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"errors"
	"io"

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

// PrepareOptions are how a start wants its program prepared (see
// [OptionsPreparer]).
type PrepareOptions struct {
	// Output, when set, receives the build's output as it is produced (a
	// streamed build); it is not written to once PrepareWith returns. Nil
	// prepares as [Driver.Prepare] does.
	Output io.Writer
	// Terminal asks for a launch whose adapter runs the program in the
	// editor's terminal (its runInTerminal request). A driver whose
	// adapter can't returns an UNSUPPORTED_BY_ADAPTER error.
	Terminal bool
}

// OptionsPreparer is a [Driver] that can prepare a launch with
// [PrepareOptions]. Its Prepare is PrepareWith with zero options.
type OptionsPreparer interface {
	// PrepareWith is [Driver.Prepare] with opts.
	PrepareWith(ctx context.Context, spec LaunchSpec, opts PrepareOptions) (Launch, error)
}

// LaunchSpec is what the user asked to debug (see [api.LaunchSpec]).
type LaunchSpec = api.LaunchSpec

// Launch is a driver's answer: the adapter process and the launch request.
type Launch struct {
	// Adapter is the adapter executable; it speaks DAP on stdio, or over a
	// socket with SocketArgs.
	Adapter     string
	AdapterArgs []string
	// SocketArgs, when set, selects the connect transport: the session
	// listens on a Unix socket in a fresh private directory, starts the
	// adapter with SocketArgs(its path) as its arguments (AdapterArgs is
	// then unused), and speaks DAP on the one connection the adapter dials
	// in. The adapter's stdout goes where its stderr does.
	SocketArgs func(socket string) []string
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

	// AdapterName names the adapter for status (its manifest's name).
	AdapterName string
	// PauseUnsupported, when set, makes the session refuse pause
	// (UNSUPPORTED_BY_ADAPTER) with it as the hint: for an adapter whose
	// pause stops the program without reporting the stop, which would
	// leave the session running as far as it knows.
	PauseUnsupported string
	// SetByEval makes set, on an adapter with neither setExpression nor
	// setVariable, evaluate "VARIABLE = VALUE" in the repl context instead
	// of refusing: for an adapter whose evaluator runs assignments.
	SetByEval bool
	// ExitCodeUnknown, when set, is why this launch's adapter can't be
	// trusted to report the program's real exit code (e.g. netcoredbg on
	// macOS, which reports 0 for every program): the session then records
	// no exit code for it, for launch, attach and test runs alike, and
	// says why in its end reason.
	ExitCodeUnknown string
	// PathMap, when set, is how the adapter's source paths map to the
	// host's (a debuggee in a container): the session translates every
	// source path crossing its DAP connection, refuses line breakpoints
	// outside the map and reads source excerpts only inside its host
	// directories (see [PathMap]). Nil: paths are the host's.
	PathMap *PathMap
	// Container, when set, says the attach is to a process in a container
	// (see [ContainerAttacher]), or the launch is of one (see
	// [ContainerLauncher]): the session reports it, has no host pid of its
	// own, and takes a claim on the container's process, or on the whole
	// container for a launch.
	Container *api.ContainerInfo
	// StartFailureHint, when set, is called after a start of this launch
	// failed and its adapter was killed, with a context that ends within a few
	// seconds: what it returns (empty: nothing) is added to the error's hint.
	// For a launch in a container, whose adapter's kill reaches only the
	// docker client on the host: it says what may still run in the
	// container.
	StartFailureHint func(ctx context.Context) string
	// Warnings are things the driver noticed and did not fail the start for;
	// the manager logs each.
	Warnings []string
}

// Request kinds for [Launch.Request].
const (
	RequestLaunch = "launch"
	RequestAttach = "attach"
)

// AdapterSelector is a [Driver] that has more than one adapter: the
// session uses the driver WithAdapter returns for [api.StartParams.Adapter]
// (docs/DESIGN.md §7's AdapterFor). A driver without it has only its own,
// and a start naming an adapter is refused.
type AdapterSelector interface {
	// WithAdapter returns the driver bound to the adapter named name, or
	// an *api.Error if it has none by that name.
	WithAdapter(name string) (Driver, error)
}

// Attacher is a [Driver] that can attach to a running process.
type Attacher interface {
	// PrepareAttach describes how to attach to spec's process.
	PrepareAttach(ctx context.Context, spec api.AttachSpec) (Launch, error)
}

// ContainerAttacher is a [Driver] that can attach to a process inside a
// container (docs/adr/0020): its adapter is whatever it can run there, over
// docker or the like, so the session sees only a [Launch] (with Container and
// usually PathMap set).
type ContainerAttacher interface {
	// PrepareContainerAttach describes how to attach to spec.Container's
	// process (spec.PID, 0 meaning 1). It returns ErrNotCandidate, joined
	// with the reason as an *api.Error, for a container that is not a
	// candidate when spec.Container.RequireDotnet says it was picked
	// implicitly.
	PrepareContainerAttach(ctx context.Context, spec api.AttachSpec) (Launch, error)
}

// ContainerLauncher is a [Driver] that can launch a program inside a
// container (docs/adr/0021): its adapter is whatever it can run there, over
// docker or the like, so the session sees only a [Launch] (with Container and
// usually PathMap set). What runs is the driver's to read from the container;
// the spec names only the container.
type ContainerLauncher interface {
	// PrepareContainerLaunch describes how to launch spec's container's
	// program under the adapter. Launch.Request must be a launch.
	PrepareContainerLaunch(ctx context.Context, spec api.ContainerLaunchSpec) (Launch, error)
}

// ErrNotCandidate marks (in an error's chain) a container that an implicit
// pick should skip rather than fail on: the daemon reports its member as
// skipped, with the error's message as the reason.
var ErrNotCandidate = errors.New("not a candidate")

// TestSpec is a test run to debug: the project (in LaunchSpec's Project),
// its environment, and which tests.
type TestSpec struct {
	api.LaunchSpec
	api.TestSpec
}

// TestCommand is how a [Tester] runs tests: either a command that starts a
// test host, prints its process id and waits for a debugger to attach, or,
// with Launch set, a self-hosting test app the session launches under the
// adapter itself.
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
	// Launch, when set, is a self-hosting test app to launch under the
	// adapter directly: no runner, no attach. The session ends when it
	// exits, with its exit code. Path, Args, Env, Dir, HostPID and Failure
	// are then unused.
	Launch *Launch
}

// Tester is a [Driver] that can debug test runs. A Tester must also be an
// [Attacher]: the session attaches to the test host.
type Tester interface {
	// TestCommand describes how to run spec's tests.
	TestCommand(ctx context.Context, spec TestSpec) (TestCommand, error)
}
