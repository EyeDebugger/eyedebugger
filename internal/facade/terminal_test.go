// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// terminalVector is one row of testdata/terminal_requests.json.
type terminalVector struct {
	Name      string         `json:"name"`
	Platform  string         `json:"platform"`
	Valid     bool           `json:"valid"`
	Rule      int            `json:"rule"`
	Arguments map[string]any `json:"arguments"`
	Repeat    *struct {
		Field      string `json:"field"`
		Count      int    `json:"count"`
		Item       string `json:"item"`
		ItemRepeat int    `json:"itemRepeat"`
	} `json:"repeat"`
}

// arguments are the vector's arguments with its repeat applied, as JSON.
func (v *terminalVector) arguments(t *testing.T) json.RawMessage {
	t.Helper()

	args := maps.Clone(v.Arguments)

	if r := v.Repeat; r != nil {
		item := strings.Repeat(r.Item, max(r.ItemRepeat, 1))

		switch r.Field {
		case "title":
			args["title"] = strings.Repeat(item, r.Count)
		case "args":
			list, _ := args["args"].([]any)
			for range r.Count {
				list = append(list, item)
			}

			args["args"] = list
		case "env":
			env, _ := args["env"].(map[string]any)
			env = maps.Clone(env)

			for i := range r.Count {
				env["V"+strconv.Itoa(i+1)] = item
			}

			args["env"] = env
		default:
			t.Fatalf("repeat field %q", r.Field)
		}
	}

	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	return raw
}

// goos are the platforms a vector applies to.
func (v *terminalVector) goos(t *testing.T) []string {
	t.Helper()

	switch v.Platform {
	case "all":
		return []string{"linux", "darwin", "windows"}
	case "posix":
		return []string{"linux", "darwin"}
	case "windows":
		return []string{"windows"}
	default:
		t.Fatalf("platform %q", v.Platform)

		return nil
	}
}

// decodeTerminal decodes a runInTerminal request's arguments as the
// session's DAP client does (go-dap).
func decodeTerminal(raw json.RawMessage) (godap.RunInTerminalRequestArguments, error) {
	msg, err := godap.DecodeProtocolMessage([]byte(`{"seq":1,"type":"request","command":"runInTerminal","arguments":` + string(raw) + `}`))
	if err != nil {
		return godap.RunInTerminalRequestArguments{}, fmt.Errorf("decode: %w", err)
	}

	r, ok := msg.(*godap.RunInTerminalRequest)
	if !ok {
		return godap.RunInTerminalRequestArguments{}, fmt.Errorf("decoded %T", msg)
	}

	return r.Arguments, nil
}

// TestTerminalVectors: every vector of the shared file, on every platform
// it names: valid ones become the event body unchanged (an empty title
// "eyedbg"), others break the rule it names first.
func TestTerminalVectors(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("testdata/terminal_requests.json")
	if err != nil {
		t.Fatal(err)
	}

	var file struct {
		Vectors []terminalVector `json:"vectors"`
	}

	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	for i := range file.Vectors {
		v := &file.Vectors[i]

		for _, goos := range v.goos(t) {
			t.Run(goos+"/"+v.Name, func(t *testing.T) {
				t.Parallel()

				checkVector(t, v, goos)
			})
		}
	}
}

func checkVector(t *testing.T, v *terminalVector, goos string) {
	t.Helper()

	args, err := decodeTerminal(v.arguments(t))
	if err != nil {
		if v.Valid || v.Rule != 0 {
			t.Fatalf("undecodable (%v), want rule %d", err, v.Rule)
		}

		return
	}

	if v.Rule == 0 && !v.Valid {
		t.Fatal("decoded, want it undecodable")
	}

	body, rule := checkTerminal(args, goos)

	if !v.Valid {
		if want := "rule " + strconv.Itoa(v.Rule) + ":"; !strings.HasPrefix(rule, want) {
			t.Fatalf("rule = %q, want %q...", rule, want)
		}

		return
	}

	if rule != "" {
		t.Fatalf("refused: %s", rule)
	}

	checkBody(t, args, body)
}

// checkBody checks body holds args' values (an empty title "eyedbg").
func checkBody(t *testing.T, args godap.RunInTerminalRequestArguments, body TerminalEventBody) {
	t.Helper()

	title := args.Title
	if title == "" {
		title = defaultTermTitle
	}

	if body.Title != title || body.Cwd != args.Cwd || !slices.Equal(body.Args, args.Args) || len(body.Env) != len(args.Env) {
		t.Fatalf("body = %+v, want the request's values", body)
	}

	for k, want := range args.Env {
		if got := body.Env[k]; (got == nil) != (want == nil) || (got != nil && *got != want) {
			t.Errorf("env %s = %v, want %v", k, got, want)
		}
	}
}

// TestTerminalDecodedLikeTheEnforcer: go-dap decodes keys
// case-insensitively and keeps a duplicate's last value; the facade checks
// the decoded value and sends exactly that, so what the editor gets is what
// was checked.
func TestTerminalDecodedLikeTheEnforcer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		rule string // "" valid
		args []string
	}{
		{name: "case-variant key", raw: `{"CWD":"/","ARGS":["/bin/prog","x"]}`, args: []string{"/bin/prog", "x"}},
		{name: "duplicate args, last wins", raw: `{"cwd":"/","args":["/bin/prog","x\n"],"args":["/bin/prog","y"]}`, args: []string{"/bin/prog", "y"}},
		{name: "duplicate args, bad last", raw: `{"cwd":"/","args":["/bin/prog","y"],"args":["/bin/prog","x\n"]}`, rule: "rule 4:"},
		{name: "duplicate kind", raw: `{"kind":"integrated","KIND":"external","cwd":"/","args":["/bin/prog"]}`, rule: "rule 1:"},
		{name: "case-variant shell flag", raw: `{"ArgsCanBeInterpretedByShell":true,"cwd":"/","args":["/bin/prog"]}`, rule: "rule 2:"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args, err := decodeTerminal(json.RawMessage(tt.raw))
			if err != nil {
				t.Fatal(err)
			}

			body, rule := checkTerminal(args, "linux")
			if !strings.HasPrefix(rule, tt.rule) || (tt.rule == "") != (rule == "") {
				t.Fatalf("rule = %q, want %q", rule, tt.rule)
			}

			if rule == "" && !slices.Equal(body.Args, tt.args) {
				t.Fatalf("args = %q, want %q", body.Args, tt.args)
			}
		})
	}
}

// TestTerminalAnswerOf: the editor's answers.
func TestTerminalAnswerOf(t *testing.T) {
	t.Parallel()

	pid := func(n int64) *int64 { return &n }
	text := func(s string) *string { return &s }

	tests := []struct {
		name  string
		a     TerminalArguments
		want  terminalAnswer
		valid bool
	}{
		{name: "pid", a: TerminalArguments{ProcessID: pid(42)}, want: terminalAnswer{pid: 42}, valid: true},
		{name: "largest pid", a: TerminalArguments{ProcessID: pid(2147483647)}, want: terminalAnswer{pid: 2147483647}, valid: true},
		{name: "error", a: TerminalArguments{Error: text("no shell\nhere x")}, want: terminalAnswer{err: "no shell here x"}, valid: true},
		{name: "empty error", a: TerminalArguments{Error: text("\n")}, want: terminalAnswer{err: "no reason given"}, valid: true},
		{name: "long error", a: TerminalArguments{Error: text(strings.Repeat("é", 1001))}, want: terminalAnswer{err: strings.Repeat("é", 1000)}, valid: true},
		{name: "pid 0", a: TerminalArguments{ProcessID: pid(0)}, want: terminalAnswer{err: "its answer was invalid"}},
		{name: "negative pid", a: TerminalArguments{ProcessID: pid(-1)}, want: terminalAnswer{err: "its answer was invalid"}},
		{name: "pid too large", a: TerminalArguments{ProcessID: pid(2147483648)}, want: terminalAnswer{err: "its answer was invalid"}},
		{name: "neither", want: terminalAnswer{err: "its answer was invalid"}},
		{name: "both", a: TerminalArguments{ProcessID: pid(42), Error: text("x")}, want: terminalAnswer{err: "its answer was invalid"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got, valid := terminalAnswerOf(tt.a); got != tt.want || valid != tt.valid {
				t.Fatalf("answer = %+v, %v; want %+v, %v", got, valid, tt.want, tt.valid)
			}
		})
	}
}

// terminalDriver is the fake driver prepared for a terminal: with
// PrepareOptions.Terminal the fake asks for one while launching.
type terminalDriver struct{ fakeDriver }

func (d terminalDriver) PrepareWith(ctx context.Context, spec session.LaunchSpec, opts session.PrepareOptions) (session.Launch, error) {
	l, err := d.Prepare(ctx, spec)
	if err == nil && opts.Terminal {
		l.Arguments["terminal"] = true
	}

	return l, err
}

// terminalEvent waits for the eyedbg/runInTerminal event.
func (tc *testClient) terminalEvent() *TerminalEvent {
	tc.t.Helper()

	ev, ok := tc.waitEvent(CommandRunInTerminal, nil).(*TerminalEvent)
	if !ok {
		tc.t.Fatal("eyedbg/runInTerminal isn't a TerminalEvent")
	}

	return ev
}

// outputs are the output events' texts so far.
func (tc *testClient) outputs() []string {
	var out []string

	for _, ev := range tc.events {
		if o, ok := ev.(*godap.OutputEvent); ok {
			out = append(out, o.Body.Output)
		}
	}

	return out
}

// TestLaunchInTerminal: a launch with console integratedTerminal gets the
// adapter's request as eyedbg/runInTerminal (the checked values, id 1),
// before the session is bound; a wrong id and a second answer are refused
// without effect; the process id reaches the adapter; the launch
// completes. Nothing of the request reaches the log.
func TestLaunchInTerminal(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, terminalDriver{})
	logs := &logRecorder{}

	tc := joinWith(t, Config{Launcher: &Launcher{Manager: e.m, Defaults: api.FacadeLaunch{ClientDir: e.dir}}, Client: humanC, Logger: slog.New(logs)})
	tc.ok("initialize", `{"adapterID":"eyedbg","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path"}`)

	launch := tc.send("launch", e.launchArgs(`"console":"integratedTerminal","stopOnEntry":true`))

	ev := tc.terminalEvent()
	want := daptest.DefaultTerminal(e.prog)

	if b := ev.Body; b.ID != 1 || b.Title != want.Title || b.Cwd != want.Cwd || !slices.Equal(b.Args, want.Args) || b.Env == nil || len(b.Env) != 0 {
		t.Fatalf("event = %+v, want %+v with id 1", b, want)
	}

	if tc.eventIndex(EventSession) >= 0 {
		t.Errorf("events %s: eyedbg/runInTerminal after the session was bound", tc.eventNames())
	}

	tc.fails(CommandRunInTerminal, `{"id":2,"processId":4242}`, api.CodeInvalidRequest)
	tc.ok(CommandRunInTerminal, `{"id":1,"processId":4242}`)
	tc.fails(CommandRunInTerminal, `{"id":1,"processId":4243}`, api.CodeInvalidRequest)

	tc.bound()
	tc.ok("configurationDone", "")

	if r, _ := tc.response(launch); !r.GetResponse().Success {
		t.Fatalf("launch = %s", errorText(r))
	}

	tc.waitEvent("stopped", nil)

	if got := tc.outputs(); !slices.Contains(got, "fake: runInTerminal processId=4242\n") {
		t.Errorf("output = %q, want the adapter to report the process id", got)
	}

	if got := logs.containing(e.prog, e.dir, want.Title, want.Args[0]); len(got) != 0 {
		t.Errorf("logged %q", got)
	}
}

// TestLaunchInTerminalFails: a request the table refuses fails the launch
// without reaching the editor; the editor's error, an invalid answer, and
// its leaving fail it too.
func TestLaunchInTerminalFails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts daptest.Options
		// answer is the editor's answer to the event ("": none expected).
		answer, answerCode string
		want               string
	}{
		{
			name: "refused by the table",
			opts: daptest.Options{Terminal: &godap.RunInTerminalRequestArguments{Cwd: "/", Args: []string{"/bin/prog", "a\nb"}}},
			want: "the debug adapter asked to run a command eyedbg won't run in a terminal: rule 4",
		},
		{
			name: "the editor's error", answer: `{"id":1,"error":"no\nterminal"}`,
			want: "the editor didn't start the program's terminal: no terminal",
		},
		{
			name: "an invalid answer", answer: `{"id":1,"processId":0}`, answerCode: string(api.CodeInvalidRequest),
			want: "the editor didn't start the program's terminal: its answer was invalid",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := newLaunchEnv(t, terminalDriver{fakeDriver{opts: tt.opts}})
			tc := e.open(t, humanC)

			launch := tc.send("launch", e.launchArgs(`"console":"integratedTerminal"`))

			if tt.answer != "" {
				tc.terminalEvent()

				if tt.answerCode != "" {
					tc.fails(CommandRunInTerminal, tt.answer, api.Code(tt.answerCode))
				} else {
					tc.ok(CommandRunInTerminal, tt.answer)
				}
			}

			r, _ := tc.response(launch)

			er, ok := r.(*godap.ErrorResponse)
			if !ok || er.Message != string(api.CodeAdapterFailed) || er.Body.Error == nil || !strings.Contains(er.Body.Error.Format, tt.want) {
				t.Fatalf("launch = %s, want ADAPTER_ERROR %q", errorText(r), tt.want)
			}

			if tt.answer == "" && tc.eventIndex(CommandRunInTerminal) >= 0 {
				t.Errorf("events %s: the refused request reached the editor", tc.eventNames())
			}

			if list := e.m.List(); len(list) != 0 {
				t.Errorf("sessions = %+v", list)
			}
		})
	}
}

// TestLaunchInTerminalEditorLeaves: the editor disconnecting while the
// terminal is pending ends the launch; nothing stays.
func TestLaunchInTerminalEditorLeaves(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, terminalDriver{})
	tc := e.open(t, humanC)

	tc.send("launch", e.launchArgs(`"console":"integratedTerminal"`))
	tc.terminalEvent()
	tc.ok("disconnect", "")
	tc.waitClosed()

	if list := e.m.List(); len(list) != 0 {
		t.Errorf("sessions = %+v", list)
	}
}

// TestTerminalAnswerRefused: eyedbg/runInTerminal is refused on a joined
// connection, before a launch, and on a launch without a terminal; a
// joined connection never gets the event.
func TestTerminalAnswerRefused(t *testing.T) {
	t.Parallel()

	e := newLaunchEnv(t, terminalDriver{})
	tc := e.open(t, humanC)

	tc.fails(CommandRunInTerminal, `{"id":1,"processId":1}`, api.CodeInvalidRequest)

	id := e.launched(tc)
	tc.fails(CommandRunInTerminal, `{"id":1,"processId":1}`, api.CodeInvalidRequest)

	s, err := e.m.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	joined := joinReady(t, s, agentC, "")
	joined.fails(CommandRunInTerminal, `{"id":1,"processId":1}`, api.CodeInvalidRequest)

	if joined.eventIndex(CommandRunInTerminal) >= 0 || tc.eventIndex(CommandRunInTerminal) >= 0 {
		t.Errorf("events %s / %s: eyedbg/runInTerminal without a terminal launch", joined.eventNames(), tc.eventNames())
	}
}
