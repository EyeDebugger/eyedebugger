// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package daptest is a fake DAP adapter for tests: a deterministic program of
// numbered lines that runs, steps, stops at breakpoints and pauses, with none
// of a real debugger's timing.
//
// Tests run it as a real adapter process by re-executing their own test
// binary: the package's TestMain calls [MaybeRun] first, and a driver launches
// [Command] with [Arguments]. In the child, MaybeRun serves DAP on stdin and
// stdout and returns true, and TestMain returns without running tests (a
// TestMain that returns exits 0). [Command] passes -test.run=^$, so a TestMain
// that forgets MaybeRun runs no tests either and the adapter just exits.
//
// The program is lines 1..N of a file, run Laps times ([ProgramArgs]).
// "Stopped at L" means about to run L; running L prints "line L\n" to
// stdout. continue runs to the next line with a breakpoint whose condition
// holds ("", "true", "false", "line == N" or "lap == N"; any other condition
// holds) or to the end; next, stepIn and stepOut run one line (reason step,
// or breakpoint with [Options.StepHitsBreakpoints] when the line has one).
// At the end the program exits with code 0 and the adapter sends
// terminated, unless it hangs: then it keeps running until paused (reason
// pause). initialized is sent once the launch (or attach) request arrived
// (the session sends it without waiting for its answer), and continued
// before the continue or step response, as netcoredbg does.
//
// setBreakpoints rejects two breakpoints on one line in one request
// (netcoredbg keeps only one per line) and keeps ids stable per file and
// line. setFunctionBreakpoints maps function fN to line N and rejects a name
// twice in one request. setExceptionBreakpoints knows the filters all and
// user-unhandled; with all, the lines in Throws stop with reason exception
// (a caught Fake.Error, described by exceptionInfo). setExpression and
// setVariable change the local x. An attach request loads the program like
// launch but sends no process event, fails configurationDone when
// FailAttach is set, and makes disconnect print "fake: disconnect
// terminateDebuggee=true|false|unset" first. Capabilities default to
// [DefaultCaps]; [CommandWith] passes other [Options].
//
// evaluate knows "line", "lap", "x", "obj" (with members a and b), the
// conditions above (as "true" or "false"), "$context" (the request's
// context), "$bps" (the program file's breakpoints, e.g. "3,5 if
// false,9"), "$fbps" (the function breakpoints) and "$filters" (the
// exception filters). threads, stackTrace, scopes and variables describe
// one thread with one frame whose locals are line and x; with
// [Options.GlobalsScope] the Locals scope has presentationHint "locals"
// and a second, cheap Globals scope holds g.
//
// [RunnerCommand] starts the test binary as a fake 'dotnet test' run
// instead (TestMain calls [MaybeRunRunner]): it prints a test host's
// "Process Id: N, Name: testhost" line and, in mode [RunnerExit], waits
// until a fake adapter attached to that pid has exited.
//
// It imports only the standard library and go-dap.
package daptest
