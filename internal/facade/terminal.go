// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// CommandRunInTerminal is both an event and a request (docs/adr/0019): the
// event (body [TerminalEventBody]) asks the launching editor to run a
// program in a terminal, for its own launch with console
// integratedTerminal; the request (arguments [TerminalArguments], empty
// body) is its answer.
const CommandRunInTerminal = "eyedbg/runInTerminal"

// Launch argument console values.
const (
	consoleInternal = "internalConsole"
	consoleTerminal = "integratedTerminal"
)

// Bounds of a terminal request (the validation table, docs/adr/0019).
const (
	maxTerminalArgs     = 1000
	maxTerminalEnv      = 1000
	maxTerminalBytes    = 256 << 10
	maxTerminalTitle    = 200
	maxTerminalErrorLen = 1000
	defaultTermTitle    = "eyedbg"
)

// goosWindows is Windows' GOOS.
const goosWindows = "windows"

// envName is a terminal environment variable's name.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// TerminalEventBody is the eyedbg/runInTerminal event's body: run Args
// (Args[0] the program, no shell) in Cwd with Env added (a null value
// unsets a variable), in a terminal named Title; answer with ID.
type TerminalEventBody struct {
	ID    int                `json:"id"`
	Title string             `json:"title"`
	Cwd   string             `json:"cwd"`
	Args  []string           `json:"args"`
	Env   map[string]*string `json:"env"`
}

// TerminalEvent is the eyedbg/runInTerminal event.
type TerminalEvent struct {
	godap.Event

	Body TerminalEventBody `json:"body"`
}

// TerminalArguments are the eyedbg/runInTerminal request's arguments: the
// event's ID and either the process id of the program it started or why
// it couldn't.
type TerminalArguments struct {
	ID        int     `json:"id"`
	ProcessID *int64  `json:"processId,omitempty"`
	Error     *string `json:"error,omitempty"`
}

// TerminalRequest is the eyedbg/runInTerminal request.
type TerminalRequest struct {
	godap.Request

	Arguments TerminalArguments `json:"arguments"`
}

// TerminalResponse is the eyedbg/runInTerminal response (no body).
type TerminalResponse struct {
	godap.Response
}

// terminalState is a launch connection's terminal exchange: at most one
// eyedbg/runInTerminal event waits for its answer at a time.
type terminalState struct {
	mu      sync.Mutex
	lastID  int
	pending *terminalPending
}

// terminalPending is an event waiting for its answer.
type terminalPending struct {
	id     int
	answer chan terminalAnswer // capacity 1, sent to once
}

// terminalAnswer is the editor's answer: a process id, or why not.
type terminalAnswer struct {
	pid int
	err string
}

// open allocates the next id and makes it the pending one; false if one
// is pending already.
func (t *terminalState) open() (*terminalPending, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pending != nil {
		return nil, false
	}

	t.lastID++
	t.pending = &terminalPending{id: t.lastID, answer: make(chan terminalAnswer, 1)}

	return t.pending, true
}

// close ends p's wait, if it is still pending.
func (t *terminalState) close(p *terminalPending) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pending == p {
		t.pending = nil
	}
}

// deliver hands a to the pending event with id, which it ends; false if
// no event with id is pending (another id, or already answered).
func (t *terminalState) deliver(id int, a terminalAnswer) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.pending == nil || t.pending.id != id {
		return false
	}

	t.pending.answer <- a
	t.pending = nil

	return true
}

// runInTerminal is a terminal launch's Terminal hook (session.LaunchHooks),
// on its own goroutine: it checks the adapter's request against the
// validation table, asks the launching editor with an eyedbg/runInTerminal
// event built from the checked values, and waits for its answer. Nothing
// of the request is logged.
func (c *connection) runInTerminal(ctx context.Context, args godap.RunInTerminalRequestArguments) (int, error) {
	body, rule := checkTerminal(args, runtime.GOOS)
	if rule != "" {
		c.logger.WarnContext(ctx, "facade terminal refused", slog.String("rule", rule))

		return 0, api.NewError(api.CodeAdapterFailed, "the debug adapter asked to run a command eyedbg won't run in a terminal: "+rule, session.TerminalHint)
	}

	p, ok := c.launch.term.open()
	if !ok {
		return 0, api.NewError(api.CodeAdapterFailed, "the debug adapter asked for a second terminal", session.TerminalHint)
	}
	defer c.launch.term.close(p)

	body.ID = p.id
	if !c.sendTerminal(&TerminalEvent{Event: event(CommandRunInTerminal), Body: body}) {
		return 0, errors.New("the editor disconnected")
	}

	select {
	case a := <-p.answer:
		if a.err != "" {
			return 0, api.NewError(api.CodeAdapterFailed, "the editor didn't start the program's terminal: "+a.err, session.TerminalHint)
		}

		return a.pid, nil
	case <-ctx.Done():
		// The session bounds the wait: its cause is then ErrTerminalTimeout.
		if cause := context.Cause(ctx); errors.Is(cause, session.ErrTerminalTimeout) {
			return 0, api.NewError(api.CodeAdapterFailed, "the editor didn't start the program's terminal: "+cause.Error(), session.TerminalHint)
		}

		return 0, ctx.Err() //nolint:wrapcheck // The start's own end.
	}
}

// sendTerminal sends ev to the editor, unless it is leaving; false if it
// wasn't sent.
func (c *connection) sendTerminal(ev *TerminalEvent) bool {
	c.gate.Lock()
	defer c.gate.Unlock()

	if c.view.leaving {
		return false
	}

	if err := c.srv.Send(ev); err != nil {
		c.close()

		return false
	}

	return true
}

// terminalAnswered handles the editor's eyedbg/runInTerminal request, on
// the reader: only a launch connection's, for the pending event's id,
// once. An answer with neither a process id in range nor an error (or
// both) is refused and fails the terminal all the same.
func (c *connection) terminalAnswered(ctx context.Context, r *TerminalRequest) {
	if c.launch == nil || !c.launch.started {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "no terminal was asked for on this connection", ""), false)

		return
	}

	a, valid := terminalAnswerOf(r.Arguments)

	if !c.launch.term.deliver(r.Arguments.ID, a) {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "no terminal with that id waits for an answer", ""), false)

		return
	}

	if !valid {
		c.fail(ctx, r, api.NewError(api.CodeInvalidRequest, "answer with a processId from 1 to 2147483647, or an error", ""), false)

		return
	}

	c.respond(ctx, r, &TerminalResponse{})
}

// terminalAnswerOf reads an answer: exactly one of a process id in range
// and an error; anything else is an invalid answer, a failure.
func terminalAnswerOf(a TerminalArguments) (terminalAnswer, bool) {
	switch {
	case a.Error != nil && a.ProcessID == nil:
		msg := sanitizeTerminalError(*a.Error)
		if msg == "" {
			msg = "no reason given"
		}

		return terminalAnswer{err: msg}, true
	case a.Error == nil && a.ProcessID != nil && *a.ProcessID >= 1 && *a.ProcessID <= math.MaxInt32:
		return terminalAnswer{pid: int(*a.ProcessID)}, true
	default:
		return terminalAnswer{err: "its answer was invalid"}, false
	}
}

// sanitizeTerminalError is the editor's error text for a message: control
// characters as spaces, at most maxTerminalErrorLen runes.
func sanitizeTerminalError(s string) string {
	var b strings.Builder

	n := 0

	for _, r := range s {
		if n == maxTerminalErrorLen {
			break
		}

		if isTerminalControl(r) {
			r = ' '
		}

		b.WriteRune(r)
		n++
	}

	return strings.TrimSpace(b.String())
}

// isTerminalControl reports whether r is a character no terminal request
// may hold: C0, DEL, C1, and the line and paragraph separators.
func isTerminalControl(r rune) bool {
	return r <= 0x1F || (r >= 0x7F && r <= 0x9F) || r == '\u2028' || r == '\u2029'
}

// checkTerminal checks an adapter's runInTerminal request against the
// validation table (docs/adr/0019) for goos, and returns the event body
// built from its checked values, or the first rule it breaks. It refuses,
// never cuts or rewrites (an empty title becomes "eyedbg").
func checkTerminal(a godap.RunInTerminalRequestArguments, goos string) (body TerminalEventBody, rule string) {
	checks := []func() string{
		func() string { return checkKind(a) },
		func() string { return checkCount(a) },
		func() string { return checkChars(a) },
		func() string { return checkPaths(a, goos) },
		func() string { return checkWindowsArgs(a, goos) },
		func() string { return checkEnv(a) },
		func() string { return checkTitle(a) },
	}

	for _, check := range checks {
		if rule = check(); rule != "" {
			return TerminalEventBody{}, rule
		}
	}

	env := make(map[string]*string, len(a.Env))

	for k, v := range a.Env {
		if s, ok := v.(string); ok {
			env[k] = &s
		} else {
			env[k] = nil
		}
	}

	title := a.Title
	if title == "" {
		title = defaultTermTitle
	}

	return TerminalEventBody{Title: title, Cwd: a.Cwd, Args: append([]string(nil), a.Args...), Env: env}, ""
}

// checkKind is rules 1 and 2: the integrated terminal, no shell.
func checkKind(a godap.RunInTerminalRequestArguments) string {
	switch {
	case a.Kind != "" && a.Kind != "integrated":
		return "rule 1: only the integrated terminal"
	case a.ArgsCanBeInterpretedByShell:
		return "rule 2: its arguments would go through a shell"
	default:
		return ""
	}
}

// checkCount is rule 3: 1 to 1000 arguments.
func checkCount(a godap.RunInTerminalRequestArguments) string {
	if len(a.Args) < 1 || len(a.Args) > maxTerminalArgs {
		return "rule 3: it needs 1 to 1000 arguments"
	}

	return ""
}

// checkChars is rules 4 and 5: no control characters in any string, and
// at most 256 KiB of them all.
func checkChars(a godap.RunInTerminalRequestArguments) string {
	strs := make([]string, 0, 2+len(a.Args)+2*len(a.Env))
	strs = append(strs, a.Title, a.Cwd)
	strs = append(strs, a.Args...)

	for k, v := range a.Env {
		strs = append(strs, k)

		if s, ok := v.(string); ok {
			strs = append(strs, s)
		}
	}

	size := 0

	for _, s := range strs {
		if strings.ContainsFunc(s, isTerminalControl) {
			return "rule 4: it holds a control character"
		}

		size += len(s)
	}

	if size > maxTerminalBytes {
		return "rule 5: it is larger than 256 KiB"
	}

	return ""
}

// checkPaths is rule 6: the program and the working directory are local
// absolute paths.
func checkPaths(a godap.RunInTerminalRequestArguments, goos string) string {
	if !localAbsolute(a.Args[0], goos) || !localAbsolute(a.Cwd, goos) {
		return "rule 6: its program and working directory must be local absolute paths"
	}

	return ""
}

// localAbsolute reports whether p is a local absolute path on goos: never
// two leading separators (UNC, \\?\, \\.\), on Windows a drive root (C:\ or
// C:/), elsewhere a leading /.
func localAbsolute(p, goos string) bool {
	isSep := func(b byte) bool { return b == '/' || b == '\\' }

	if len(p) >= 2 && isSep(p[0]) && isSep(p[1]) {
		return false
	}

	if goos != goosWindows {
		return strings.HasPrefix(p, "/")
	}

	return len(p) >= 3 && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z') && p[1] == ':' && isSep(p[2])
}

// checkWindowsArgs is rule 7: on Windows, the program is an .exe, and no
// argument is one node-pty passes unquoted while it holds a space (rule 4
// already refused tabs), which the program would drop or split: a single
// space, or one enclosed in quotes (node-pty's argsToCommandLine).
func checkWindowsArgs(a godap.RunInTerminalRequestArguments, goos string) string {
	if goos != goosWindows {
		return ""
	}

	if !strings.HasSuffix(strings.ToLower(a.Args[0]), ".exe") {
		return "rule 7: on Windows its program must be an .exe"
	}

	for _, arg := range a.Args {
		if arg == " " {
			return "rule 7: on Windows no argument may be a single space"
		}

		if len(arg) > 1 && arg[0] == '"' && arg[len(arg)-1] == '"' && strings.Contains(arg, " ") {
			return `rule 7: on Windows no argument may start and end with " while holding a space`
		}
	}

	return ""
}

// checkEnv is rule 8: at most 1000 variables, plain names, string or null
// values.
func checkEnv(a godap.RunInTerminalRequestArguments) string {
	if len(a.Env) > maxTerminalEnv {
		return "rule 8: it sets more than 1000 environment variables"
	}

	for k, v := range a.Env {
		if !envName.MatchString(k) {
			return "rule 8: an environment variable's name isn't a plain name"
		}

		if _, ok := v.(string); !ok && v != nil {
			return "rule 8: an environment variable's value isn't a string or null"
		}
	}

	return ""
}

// checkTitle is rule 9: a title of at most 200 characters.
func checkTitle(a godap.RunInTerminalRequestArguments) string {
	if utf8.RuneCountInString(a.Title) > maxTerminalTitle {
		return "rule 9: its title is longer than 200 characters"
	}

	return ""
}
