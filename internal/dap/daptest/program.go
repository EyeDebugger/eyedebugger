// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

import (
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	godap "github.com/google/go-dap"
)

// The program's only thread, its process id and the locals' reference.
const (
	threadID  = 1
	processID = 4242
	localsRef = 1000
)

// Program states.
const (
	stateLoaded  = iota // launched, not configured
	stateRunning        // running (only a hung program stays so)
	stateStopped
	stateEnded
)

// fakeBreakpoint is one line breakpoint.
type fakeBreakpoint struct {
	line      int
	condition string
}

type bpKey struct {
	path string
	line int
}

// program is the fake debuggee: lines 1..lines of path.
type program struct {
	emit  func(event string, body any)
	path  string
	lines int
	hang  bool

	state int
	line  int  // stopped at (about to run) this line
	hung  bool // past the last line, looping forever

	bps    map[string][]fakeBreakpoint
	ids    map[bpKey]int
	nextID int
}

func newProgram(emit func(string, any)) *program {
	return &program{emit: emit, state: stateLoaded, bps: make(map[string][]fakeBreakpoint), ids: make(map[bpKey]int)}
}

func (p *program) load(path string, lines int, hang bool) {
	p.path, p.lines, p.hang = path, lines, hang
}

// start runs the program from line 1 (stopping there if stopAtEntry).
func (p *program) start(stopAtEntry bool) {
	p.emit("process", godap.ProcessEventBody{Name: p.path, SystemProcessId: processID, IsLocalProcess: true, StartMethod: "launch"})
	p.emit("thread", godap.ThreadEventBody{Reason: "started", ThreadId: threadID})

	if stopAtEntry {
		p.stopAt(1, "entry")

		return
	}

	p.runFrom(1)
}

// runFrom runs lines from l on, stopping at the first breakpoint that holds.
func (p *program) runFrom(l int) {
	p.state = stateRunning

	for ; l <= p.lines; l++ {
		if p.hits(l) {
			p.stopAt(l, "breakpoint")

			return
		}

		p.run(l)
	}

	p.finish()
}

func (p *program) cont() {
	if p.state != stateStopped {
		return
	}

	if p.hung {
		p.state = stateRunning

		return
	}

	p.run(p.line)
	p.runFrom(p.line + 1)
}

func (p *program) step() {
	if p.state != stateStopped {
		return
	}

	if p.hung {
		p.stopAt(p.line, "step")

		return
	}

	p.run(p.line)

	if p.line+1 > p.lines {
		p.finish()

		return
	}

	p.stopAt(p.line+1, "step")
}

func (p *program) pause() {
	if p.state == stateRunning {
		p.stopAt(p.line, "pause")
	}
}

// run runs line l: it prints "line L".
func (p *program) run(l int) {
	p.line = l
	p.emit("output", godap.OutputEventBody{Category: "stdout", Output: fmt.Sprintf("line %d\n", l)})
}

func (p *program) stopAt(l int, reason string) {
	p.line, p.state = l, stateStopped
	p.emit("stopped", godap.StoppedEventBody{Reason: reason, ThreadId: threadID, AllThreadsStopped: true})
}

// finish ends the program after its last line, or hangs there.
func (p *program) finish() {
	if p.hang {
		p.line, p.hung, p.state = p.lines, true, stateRunning

		return
	}

	p.state = stateEnded
	p.emit("exited", godap.ExitedEventBody{ExitCode: 0})
	p.emit("terminated", nil)
}

// hits reports whether a breakpoint of the program file at l holds.
func (p *program) hits(l int) bool {
	for _, b := range p.bps[p.path] {
		if b.line == l {
			return holds(b.condition, l)
		}
	}

	return false
}

// holds evaluates a condition at line l: "false" never holds, "line == N"
// holds at N, anything else always holds (like a condition that fails to
// evaluate, which stops the program).
func holds(cond string, l int) bool {
	if cond == "false" {
		return false
	}

	if rest, ok := strings.CutPrefix(cond, "line == "); ok {
		n, err := strconv.Atoi(rest)

		return err != nil || n == l
	}

	return true
}

// setBreakpoints replaces path's breakpoints. Two on one line are an error.
func (p *program) setBreakpoints(path string, req []godap.SourceBreakpoint) ([]godap.Breakpoint, error) {
	list := make([]fakeBreakpoint, 0, len(req))
	out := make([]godap.Breakpoint, 0, len(req))

	for _, sb := range req {
		if slices.ContainsFunc(list, func(b fakeBreakpoint) bool { return b.line == sb.Line }) {
			return nil, fmt.Errorf("two breakpoints on line %d of %s in one request", sb.Line, filepath.Base(path))
		}

		list = append(list, fakeBreakpoint{line: sb.Line, condition: sb.Condition})

		key := bpKey{path, sb.Line}
		if p.ids[key] == 0 {
			p.nextID++
			p.ids[key] = p.nextID
		}

		b := godap.Breakpoint{Id: p.ids[key], Verified: true, Line: sb.Line, Source: &godap.Source{Path: path}}
		if path == p.path && (sb.Line < 1 || sb.Line > p.lines) {
			b.Verified, b.Message = false, "no code at this line"
		}

		out = append(out, b)
	}

	p.bps[path] = list

	return out, nil
}

func (p *program) frame() godap.StackFrame {
	return godap.StackFrame{
		Id: 1, Name: "main", Line: p.line, Column: 1,
		Source: &godap.Source{Name: filepath.Base(p.path), Path: p.path},
	}
}

func (p *program) locals() []godap.Variable {
	return []godap.Variable{{Name: "line", Value: strconv.Itoa(p.line), Type: "int"}}
}

// evaluate knows "line" and "$bps".
func (p *program) evaluate(expr string) (string, bool) {
	switch expr {
	case "line":
		return strconv.Itoa(p.line), true
	case "$bps":
		list := slices.Clone(p.bps[p.path])
		slices.SortFunc(list, func(a, b fakeBreakpoint) int { return a.line - b.line })

		parts := make([]string, 0, len(list))
		for _, b := range list {
			s := strconv.Itoa(b.line)
			if b.condition != "" {
				s += " if " + b.condition
			}

			parts = append(parts, s)
		}

		return strings.Join(parts, ","), true
	default:
		return "", false
	}
}
