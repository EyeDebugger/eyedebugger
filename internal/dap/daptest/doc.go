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
// The program is lines 1..N of a file. "Stopped at L" means about to run L;
// running L prints "line L\n" to stdout. continue runs to the next line with a
// breakpoint whose condition holds ("", "true", "false" or "line == N"; any
// other condition holds) or to the end; next, stepIn and stepOut run one line
// (reason step). At the end the program exits with code 0 and the adapter
// sends terminated, unless it hangs: then it keeps running until paused
// (reason pause). initialized is sent once the launch request arrived (the
// session sends launch without waiting for its answer), and continued before
// the continue or step response, as netcoredbg does. setBreakpoints rejects
// two breakpoints on one line in one request (netcoredbg keeps only one per
// line) and keeps ids stable per file and line. evaluate knows two
// expressions: "line" (the current line) and "$bps" (the program file's
// breakpoints, e.g. "3,5 if false,9"). threads, stackTrace, scopes and
// variables describe one thread with one frame whose only local is line.
//
// It imports only the standard library and go-dap.
package daptest
