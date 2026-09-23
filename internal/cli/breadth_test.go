// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestBreadthCLI drives M5's commands through an in-process daemon. Not
// parallel: it sets environment variables.
func TestBreadthCLI(t *testing.T) {
	serveInProcess(t, isolate(t))

	prog := filepath.Join(t.TempDir(), "prog.txt")
	if err := os.WriteFile(prog, []byte("a\nb\nc\ntotal += price\ne\nf\ng\nh\ni\nj\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	expectOutput(t, run(t, 0, "start", "fake", "--program", prog, "--stop-on-entry", "--exceptions", "all", "--timeout", "20s"), "stopped: entry")

	expectOutput(t, run(t, 0, "bp", "add", "func:f6"), "1  agent  func:f6  verified")
	expectOutput(t, run(t, 0, "bp", "add", prog+`@"total  +=  price"`, "--hit", ">=1"), `2  agent  `, `prog.txt:4 @"total  +=  price" hit >=1  verified`)
	expectOutput(t, run(t, 0, "bp", "add", prog+":2", "--log", "at {line}"), `log "at {line}"  verified`)
	expectOutput(t, run(t, exitError, "bp", "add", prog+`@"nowhere"`), "[ANCHOR_NOT_FOUND]")
	expectOutput(t, run(t, exitError, "bp", "add", "func:f6", "--hit", "2"), "[INVALID_REQUEST]", "line breakpoints only")

	expectOutput(t, run(t, 0, "bp", "exceptions"), "exceptions: all (agent)", "adapter filters: all")
	expectOutput(t, run(t, 0, "--as", "human:t", "bp", "exceptions", "none", "--force"), "exceptions: none")

	expectOutput(t, run(t, 0, "continue", "--timeout", "20s"), "stopped: breakpoint", "prog.txt:4", "> 4 | total += price", "at 2")
	expectOutput(t, run(t, 0, "bp", "ls"), "hits 1")

	expectOutput(t, run(t, exitError, "eval", "Foo()"), "[SIDE_EFFECTS]", "--allow-side-effects")
	expectOutput(t, run(t, 0, "eval", "$context", "--allow-side-effects"), "repl")
	expectOutput(t, run(t, 0, "eval", "obj", "--depth", "2"), "{Obj}", "  a: int = 1")
	expectOutput(t, run(t, 0, "set", "x", "7"), "x = 7  (int)")

	expectOutput(t, run(t, 0, "run-until", prog+`@"f"`, "--timeout", "20s"), "stopped: breakpoint", "prog.txt:6")
	expectOutput(t, run(t, 0, "events", "--since", "0"),
		"agent: exceptions all", "human:t: exceptions none (for every client)", "agent: eval $context (side effects allowed)",
		"agent: set x", "output (logpoint): at 2")

	expectOutput(t, run(t, exitError, "detach"), "[INVALID_REQUEST]", "'eyedbg stop' ends it")
	expectOutput(t, run(t, 0, "stop"), " ended")

	pid := strconv.Itoa(os.Getpid())
	expectOutput(t, run(t, 0, "attach", "fake", "--pid", pid), "session ", "running")
	expectOutput(t, run(t, 0, "sessions"), "pid "+pid)
	expectOutput(t, run(t, 0, "detach"), "detached; pid "+pid+" keeps running")
	expectOutput(t, run(t, exitError, "attach", "fake"), "needs --pid")
}
