// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	godap "github.com/google/go-dap"
)

// The program's only thread, its process id and the variables' references.
const (
	threadID   = 1
	processID  = 4242
	localsRef  = 1000
	objRef     = 2000
	globalsRef = 3000
)

// Program states.
const (
	stateLoaded  = iota // launched, not configured
	stateRunning        // running (only a hung program stays so)
	stateStopped
	stateEnded
)

// typeInt is the type of every variable.
const typeInt = "int"

// Stop reasons.
const (
	reasonBreakpoint = "breakpoint"
	reasonException  = "exception"
	reasonStep       = "step"
)

// exceptionText is the stopped event's text at a throw, as netcoredbg words
// it.
const exceptionText = "Exception thrown: 'Fake.Error' in fake"

// fakeBreakpoint is one line (or function) breakpoint.
type fakeBreakpoint struct {
	line      int
	condition string
	name      string // function breakpoints: the function
}

type bpKey struct {
	path string
	line int
}

// program is the fake debuggee: lines 1..lines of path, laps times.
type program struct {
	emit     func(event string, body any)
	stepHits bool
	path     string
	lines    int
	laps     int
	hang     bool
	attached bool
	throws   []int
	exitCode int

	state int
	line  int  // stopped at (about to run) this line
	lap   int  // from 1
	hung  bool // past the last line, looping forever
	// thrown means the program stopped at line's throw: running on
	// completes the line (the exception is caught).
	thrown bool
	x      string // the local x

	bps     map[string][]fakeBreakpoint
	ids     map[bpKey]int
	fbps    []fakeBreakpoint
	fids    map[string]int
	nextID  int
	filters []string
	// lateVerify leaves breakpoints unverified until the next resume;
	// pending are those to verify then, by id, and verified those it
	// verified (they stay verified).
	lateVerify bool
	pending    map[int]godap.Breakpoint
	verified   map[int]bool
}

func newProgram(emit func(string, any), stepHits, lateVerify bool) *program {
	return &program{
		emit: emit, stepHits: stepHits, lateVerify: lateVerify, state: stateLoaded, x: "0",
		bps: make(map[string][]fakeBreakpoint), ids: make(map[bpKey]int), fids: make(map[string]int),
		pending: make(map[int]godap.Breakpoint), verified: make(map[int]bool),
	}
}

// verifyPending sends a breakpoint changed event verifying each breakpoint
// LateVerify left unverified, in id order.
func (p *program) verifyPending() {
	ids := slices.Sorted(maps.Keys(p.pending))
	for _, id := range ids {
		p.emit("breakpoint", godap.BreakpointEventBody{Reason: "changed", Breakpoint: p.pending[id]})
		p.verified[id] = true
	}

	clear(p.pending)
}

func (p *program) load(args launchArgs, attached bool) {
	p.path, p.lines, p.hang, p.laps, p.throws, p.attached = args.Program, args.Lines, args.Hang, max(args.Laps, 1), args.Throws, attached
	p.exitCode = args.ExitCode
}

// start runs the program from line 1 (stopping there if stopAtEntry). An
// attached program announces no process.
func (p *program) start(stopAtEntry bool) {
	if !p.attached {
		p.emit("process", godap.ProcessEventBody{Name: p.path, SystemProcessId: processID, IsLocalProcess: true, StartMethod: "launch"})
	}

	p.emit("thread", godap.ThreadEventBody{Reason: "started", ThreadId: threadID})
	p.lap = 1

	if stopAtEntry {
		p.stopAt(1, "entry")

		return
	}

	p.runFrom(1)
}

// runFrom runs lines from l on, stopping at the first breakpoint that holds
// or throw that a filter catches.
func (p *program) runFrom(l int) {
	p.state = stateRunning

	for {
		for ; l <= p.lines; l++ {
			if p.hits(l) {
				p.stopAt(l, reasonBreakpoint)

				return
			}

			if p.exec(l) {
				return
			}
		}

		if p.lap >= p.laps {
			break
		}

		p.lap, l = p.lap+1, 1
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

	if p.exec(p.line) {
		return
	}

	p.runFrom(p.line + 1)
}

func (p *program) step() {
	if p.state != stateStopped {
		return
	}

	if p.hung {
		p.stopAt(p.line, reasonStep)

		return
	}

	if p.exec(p.line) {
		return
	}

	next := p.line + 1
	if next > p.lines {
		if p.lap >= p.laps {
			p.finish()

			return
		}

		p.lap, next = p.lap+1, 1
	}

	reason := reasonStep
	if p.stepHits && p.hits(next) {
		reason = reasonBreakpoint
	}

	p.stopAt(next, reason)
}

// pause stops a running program with reason pause, or, asSignal, as a
// SIGSTOP signal stop (reason exception).
func (p *program) pause(asSignal bool) {
	if p.state != stateRunning {
		return
	}

	if !asSignal {
		p.stopAt(p.line, "pause")

		return
	}

	p.state, p.thrown = stateStopped, false
	p.emit("stopped", godap.StoppedEventBody{Reason: reasonException, Description: "signal SIGSTOP", ThreadId: threadID, AllThreadsStopped: true})
}

// exec runs line l, or stops at its throw when the "all" filter is on; it
// reports whether it stopped.
func (p *program) exec(l int) bool {
	if !p.thrown && slices.Contains(p.throws, l) && slices.Contains(p.filters, filterAll) {
		p.thrown = true
		p.line, p.state = l, stateStopped
		p.emit("stopped", godap.StoppedEventBody{Reason: reasonException, ThreadId: threadID, AllThreadsStopped: true, Text: exceptionText})

		return true
	}

	p.thrown = false
	p.run(l)

	return false
}

// run runs line l: it prints "line L".
func (p *program) run(l int) {
	p.line = l
	p.emit("output", godap.OutputEventBody{Category: "stdout", Output: fmt.Sprintf("line %d\n", l)})
}

func (p *program) stopAt(l int, reason string) {
	p.line, p.state, p.thrown = l, stateStopped, false
	p.emit("stopped", godap.StoppedEventBody{Reason: reason, ThreadId: threadID, AllThreadsStopped: true})
}

// finish ends the program after its last line, or hangs there.
func (p *program) finish() {
	if p.hang {
		p.line, p.hung, p.state = p.lines, true, stateRunning

		return
	}

	p.state = stateEnded
	p.emit("exited", godap.ExitedEventBody{ExitCode: p.exitCode})
	p.emit("terminated", nil)
}

// hits reports whether a breakpoint of the program file, or a function
// breakpoint, at l holds.
func (p *program) hits(l int) bool {
	for _, b := range slices.Concat(p.bps[p.path], p.fbps) {
		if b.line == l && p.holds(b.condition, l) {
			return true
		}
	}

	return false
}

// holds evaluates a condition at line l: "false" never holds, "line == N"
// and "lap == N" hold at N, anything else always holds (like a condition
// that fails to evaluate, which stops the program).
func (p *program) holds(cond string, l int) bool {
	if cond == "false" {
		return false
	}

	for _, v := range []struct {
		prefix string
		value  int
	}{{"line == ", l}, {"lap == ", p.lap}} {
		if rest, ok := strings.CutPrefix(cond, v.prefix); ok {
			n, err := strconv.Atoi(rest)

			return err != nil || n == v.value
		}
	}

	return true
}

// setBreakpoints replaces path's breakpoints. Two on one line are an error.
func (p *program) setBreakpoints(path string, req []godap.SourceBreakpoint) ([]godap.Breakpoint, error) {
	maps.DeleteFunc(p.pending, func(_ int, b godap.Breakpoint) bool { return b.Source.Path == path })

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

		if p.lateVerify && b.Verified && !p.verified[b.Id] {
			p.pending[b.Id] = b
			b.Verified, b.Message = false, "pending"
		}

		out = append(out, b)
	}

	p.bps[path] = list

	return out, nil
}

// setFunctionBreakpoints replaces the function breakpoints. Function fN is
// line N; a name twice in one request is an error (netcoredbg would merge
// them into one breakpoint).
func (p *program) setFunctionBreakpoints(req []godap.FunctionBreakpoint) ([]godap.Breakpoint, error) {
	list := make([]fakeBreakpoint, 0, len(req))
	out := make([]godap.Breakpoint, 0, len(req))

	for _, fb := range req {
		if slices.ContainsFunc(list, func(b fakeBreakpoint) bool { return b.name == fb.Name }) {
			return nil, fmt.Errorf("function %s twice in one request", fb.Name)
		}

		if p.fids[fb.Name] == 0 {
			p.nextID++
			p.fids[fb.Name] = p.nextID
		}

		b := godap.Breakpoint{Id: p.fids[fb.Name], Verified: true}

		line, err := strconv.Atoi(strings.TrimPrefix(fb.Name, "f"))
		if !strings.HasPrefix(fb.Name, "f") || err != nil || line < 1 || line > p.lines {
			b.Verified, b.Message, line = false, "no function "+fb.Name, 0
		}

		list = append(list, fakeBreakpoint{line: line, condition: fb.Condition, name: fb.Name})
		out = append(out, b)
	}

	p.fbps = list

	return out, nil
}

// setFilters sets the exception filters: "all" and "user-unhandled".
func (p *program) setFilters(filters []string) error {
	for _, f := range filters {
		if f != filterAll && f != filterUserUnhandled {
			return fmt.Errorf("unknown exception filter %q", f)
		}
	}

	p.filters = slices.Clone(filters)

	return nil
}

func (p *program) frame() godap.StackFrame {
	return godap.StackFrame{
		Id: 1, Name: "main", Line: p.line, Column: 1,
		Source: &godap.Source{Name: filepath.Base(p.path), Path: p.path},
	}
}

func (p *program) locals() []godap.Variable {
	return []godap.Variable{
		{Name: "line", Value: strconv.Itoa(p.line), Type: typeInt},
		{Name: "x", Value: p.x, Type: typeInt},
	}
}

func (p *program) variables(ref int) ([]godap.Variable, bool) {
	switch ref {
	case localsRef:
		return p.locals(), true
	case objRef:
		return []godap.Variable{{Name: "a", Value: "1", Type: typeInt}, {Name: "b", Value: "2", Type: typeInt}}, true
	case globalsRef:
		return []godap.Variable{{Name: "g", Value: "1", Type: typeInt}}, true
	default:
		return nil, false
	}
}

// set assigns value to x, the only variable that can change.
func (p *program) set(name, value string) error {
	if name != "x" {
		return fmt.Errorf("cannot set %s", name)
	}

	if _, err := strconv.Atoi(value); err != nil {
		return fmt.Errorf("cannot assign %q to x", value)
	}

	p.x = value

	return nil
}

// exceptionInfo describes the exception the program stopped at.
func (p *program) exceptionInfo() (godap.ExceptionInfoResponseBody, bool) {
	if p.state != stateStopped || !p.thrown {
		return godap.ExceptionInfoResponseBody{}, false
	}

	msg := fmt.Sprintf("fake failure at line %d", p.line)

	return godap.ExceptionInfoResponseBody{
		ExceptionId: "Fake.Error", Description: msg, BreakMode: "always",
		Details: &godap.ExceptionDetails{
			Message: msg, TypeName: "Error", FullTypeName: "Fake.Error",
			StackTrace:     fmt.Sprintf("   at main() in %s:line %d", p.path, p.line),
			InnerException: []godap.ExceptionDetails{{Message: "inner cause", FullTypeName: "Fake.Inner"}},
		},
	}, true
}

// evaluate knows "line", "lap", "x", "obj", "$bps", "$fbps", "$filters",
// and the conditions "true", "false", "line == N" and "lap == N".
func (p *program) evaluate(expr string) (value string, ref int, ok bool) {
	switch expr {
	case "line":
		return strconv.Itoa(p.line), 0, true
	case "lap":
		return strconv.Itoa(p.lap), 0, true
	case "x":
		return p.x, 0, true
	case "obj":
		return "{Obj}", objRef, true
	case "$bps":
		return describe(p.bps[p.path], func(b fakeBreakpoint) string { return strconv.Itoa(b.line) }), 0, true
	case "$fbps":
		return describe(p.fbps, func(b fakeBreakpoint) string { return b.name }), 0, true
	case "$filters":
		return strings.Join(p.filters, ","), 0, true
	case "true", "false":
		return expr, 0, true
	}

	if strings.HasPrefix(expr, "line == ") || strings.HasPrefix(expr, "lap == ") {
		return strconv.FormatBool(p.holds(expr, p.line)), 0, true
	}

	return "", 0, false
}

// describe lists breakpoints sorted by line, e.g. "3,5 if false".
func describe(bps []fakeBreakpoint, name func(fakeBreakpoint) string) string {
	list := slices.Clone(bps)
	slices.SortStableFunc(list, func(a, b fakeBreakpoint) int { return a.line - b.line })

	parts := make([]string, 0, len(list))
	for _, b := range list {
		s := name(b)
		if b.condition != "" {
			s += " if " + b.condition
		}

		parts = append(parts, s)
	}

	return strings.Join(parts, ",")
}
