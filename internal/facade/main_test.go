// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package facade

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

// TestMain lets the test binary double as the fake DAP adapter.
func TestMain(m *testing.M) {
	if daptest.MaybeRun() {
		return
	}

	m.Run()
}

// testWait bounds every wait in these tests; none should come close.
const testWait = 20 * time.Second

var (
	agentC = api.Client{ID: "agent", Kind: api.KindAgent}
	humanC = api.Client{ID: "human:t", Kind: api.KindHuman, Name: "t"}
	humanB = api.Client{ID: "human:b", Kind: api.KindHuman, Name: "b"}
)

// fakeDriver runs fake programs of 10 lines. Program arguments: "hang"
// keeps it running after its last line, "lines=N" and "laps=N".
type fakeDriver struct {
	opts        daptest.Options
	sideEffects func(string) (string, bool)
}

func (fakeDriver) Name() string { return "fake" }

func (d fakeDriver) Prepare(_ context.Context, spec session.LaunchSpec) (session.Launch, error) {
	path, args, env, err := daptest.CommandWith(d.opts)
	if err != nil {
		return session.Launch{}, err
	}

	pa := daptest.ProgramArgs{Program: spec.Program, Lines: 10, StopAtEntry: spec.StopOnEntry, Hang: slices.Contains(spec.Args, "hang")}

	for _, a := range spec.Args {
		name, value, _ := strings.Cut(a, "=")
		switch name {
		case "lines":
			pa.Lines, _ = strconv.Atoi(value)
		case "laps":
			pa.Laps, _ = strconv.Atoi(value)
		}
	}

	return session.Launch{
		Adapter: path, AdapterArgs: args, AdapterEnv: env, AdapterID: "fake", Program: spec.Program,
		Arguments: pa.Map(), SideEffects: d.sideEffects,
	}, nil
}

func (fakeDriver) PrepareAttach(context.Context, api.AttachSpec) (session.Launch, error) {
	return session.Launch{}, api.NewError(api.CodeInvalidRequest, "no attach in these tests", "")
}

// callsCheck flags expressions with "(" as method calls.
func callsCheck(expr string) (string, bool) {
	if strings.Contains(expr, "(") {
		return "call a method", true
	}

	return "", false
}

// startSession starts a fake session for the agent, on a program file of
// 10 lines; with stop on entry it waits for that stop.
func startSession(t *testing.T, drv fakeDriver, p api.StartParams) *session.Session {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	m := session.NewManager(ctx, session.Config{Drivers: []session.Driver{drv}, Logger: slog.New(slog.DiscardHandler), Stderr: io.Discard})

	t.Cleanup(func() {
		m.StopAll(context.Background())
		cancel()
	})

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	p.Lang, p.Program = "fake", filepath.Join(dir, "prog.txt")
	if err := os.WriteFile(p.Program, []byte(strings.Repeat("line\n", 10)), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := m.Start(t.Context(), agentC, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if p.StopOnEntry {
		if snap := s.Wait(t.Context(), 0, testWait, api.DumpSpec{}); snap.Session.State != api.StateStopped {
			t.Fatalf("session = %+v, want stopped at entry", snap.Session)
		}
	}

	return s
}

// testClient is a DAP client of a facade connection that keeps everything
// the facade sends in order, to check what came before what.
type testClient struct {
	t    *testing.T
	conn net.Conn
	msgs chan godap.Message // closed when the connection ends
	done chan struct{}      // closed when Serve returned

	wmu sync.Mutex
	seq int

	// events are the events received so far, in order; cursor is where
	// the next waitEvent starts looking. responses are the responses
	// received and not yet taken, by request seq.
	events    []godap.EventMessage
	cursor    int
	responses map[int]godap.ResponseMessage
}

// join opens a facade connection for c to s (not yet initialized).
func join(t *testing.T, s *session.Session, c api.Client) *testClient {
	t.Helper()

	return joinWith(t, Config{Session: s, Client: c})
}

// joinWith opens a facade connection with cfg (logging nowhere unless it
// has a logger).
func joinWith(t *testing.T, cfg Config) *testClient {
	t.Helper()

	ours, theirs := net.Pipe()
	tc := &testClient{
		t: t, conn: theirs, msgs: make(chan godap.Message, 10000), done: make(chan struct{}),
		responses: make(map[int]godap.ResponseMessage),
	}

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		if cfg.Logger == nil {
			cfg.Logger = slog.New(slog.DiscardHandler)
		}

		Serve(ctx, cfg, bufio.NewReader(ours), ours)
		_ = ours.Close()
		close(tc.done)
	}()

	codec := godap.NewCodec()
	if err := RegisterMessages(codec); err != nil {
		t.Fatal(err)
	}

	go func() {
		defer close(tc.msgs)

		r := bufio.NewReader(theirs)

		for {
			raw, err := dap.ReadMessage(r, 16<<20)
			if err != nil {
				return
			}

			msg, err := codec.DecodeMessage(raw)
			if err != nil {
				t.Errorf("undecodable message from the facade: %s", raw)

				return
			}

			tc.msgs <- msg
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = theirs.Close()
		tc.waitClosed()
	})

	return tc
}

// joinReady joins and completes the handshake with initialize arguments
// args (JSON, "" for VS Code-like defaults), attach and configurationDone.
func joinReady(t *testing.T, s *session.Session, c api.Client, args string) *testClient {
	t.Helper()

	tc := join(t, s, c)
	tc.handshake(args)

	return tc
}

func (tc *testClient) handshake(args string) {
	tc.t.Helper()

	if args == "" {
		args = `{"adapterID":"eyedbg","linesStartAt1":true,"columnsStartAt1":true,"pathFormat":"path","supportsInvalidatedEvent":true}`
	}

	tc.ok("initialize", args)
	tc.ok("attach", `{}`)
	tc.waitEvent("initialized", nil)
	tc.ok("configurationDone", "")
}

// send writes a request with JSON arguments ("" for none) and returns its
// seq.
func (tc *testClient) send(command, args string) int {
	tc.t.Helper()

	seq, msg := tc.request(command, args)
	tc.rawBytes(msg)

	return seq
}

// request returns a request with JSON arguments ("" for none), framed, and
// its seq.
func (tc *testClient) request(command, args string) (seq int, msg string) {
	tc.wmu.Lock()
	tc.seq++
	seq = tc.seq
	tc.wmu.Unlock()

	raw := `{"seq":` + strconv.Itoa(seq) + `,"type":"request","command":"` + command + `"`
	if args != "" {
		raw += `,"arguments":` + args
	}

	return seq, frame(raw + "}")
}

// frame returns content framed as one DAP message.
func frame(content string) string {
	return "Content-Length: " + strconv.Itoa(len(content)) + "\r\n\r\n" + content
}

// rawBytes writes bytes as they are.
func (tc *testClient) rawBytes(b string) {
	tc.t.Helper()

	tc.wmu.Lock()
	defer tc.wmu.Unlock()

	_ = tc.conn.SetWriteDeadline(time.Now().Add(testWait))
	if _, err := tc.conn.Write([]byte(b)); err != nil {
		tc.t.Fatalf("write to the facade: %v", err)
	}
}

// next returns the next message from the facade, failing the test if none
// comes or the connection ended.
func (tc *testClient) next() godap.Message {
	tc.t.Helper()

	select {
	case msg, ok := <-tc.msgs:
		if !ok {
			tc.t.Fatalf("the facade closed the connection; events so far: %s", tc.eventNames())
		}

		return msg
	case <-time.After(testWait):
		tc.t.Fatalf("nothing from the facade; events so far: %s", tc.eventNames())

		return nil
	}
}

// receive reads one message into events or responses.
func (tc *testClient) receive() {
	tc.t.Helper()

	switch m := tc.next().(type) {
	case godap.EventMessage:
		tc.events = append(tc.events, m)
	case godap.ResponseMessage:
		tc.responses[m.GetResponse().RequestSeq] = m
	default:
		tc.t.Fatalf("unexpected message %T from the facade", m)
	}
}

// response waits for the response to request seq and returns it with the
// number of events received before it.
func (tc *testClient) response(seq int) (resp godap.ResponseMessage, before int) {
	tc.t.Helper()

	for {
		if r, ok := tc.responses[seq]; ok {
			delete(tc.responses, seq)

			return r, len(tc.events)
		}

		tc.receive()
	}
}

// do sends a request and waits for its response.
func (tc *testClient) do(command, args string) godap.ResponseMessage {
	tc.t.Helper()

	r, _ := tc.response(tc.send(command, args))

	return r
}

// ok is do, failing the test on an error response.
func (tc *testClient) ok(command, args string) godap.ResponseMessage {
	tc.t.Helper()

	r := tc.do(command, args)
	if !r.GetResponse().Success {
		tc.t.Fatalf("%s failed: %s", command, errorText(r))
	}

	return r
}

// fails is do, failing the test unless the response is an error with
// code; it returns the error's body.
func (tc *testClient) fails(command, args string, code api.Code) *godap.ErrorMessage {
	tc.t.Helper()

	r := tc.do(command, args)

	e, ok := r.(*godap.ErrorResponse)
	if !ok || r.GetResponse().Success || e.Message != string(code) || e.Body.Error == nil || e.Body.Error.Variables["code"] != string(code) {
		tc.t.Fatalf("%s = %s, want error %s", command, errorText(r), code)
	}

	return e.Body.Error
}

// jsonString is s as a JSON string (Windows paths hold backslashes).
func jsonString(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(raw)
}

func errorText(r godap.ResponseMessage) string {
	raw, err := json.Marshal(r)
	if err != nil {
		return err.Error()
	}

	return string(raw)
}

// waitEvent returns the first event named name (matching pred, if set)
// after the last one waitEvent returned.
func (tc *testClient) waitEvent(name string, pred func(godap.EventMessage) bool) godap.EventMessage {
	tc.t.Helper()

	for {
		for i := tc.cursor; i < len(tc.events); i++ {
			if ev := tc.events[i]; ev.GetEvent().Event == name && (pred == nil || pred(ev)) {
				tc.cursor = i + 1

				return ev
			}
		}

		tc.receive()
	}
}

// eventNames lists the events received so far.
func (tc *testClient) eventNames() string {
	names := make([]string, len(tc.events))
	for i, ev := range tc.events {
		names[i] = ev.GetEvent().Event
	}

	return strings.Join(names, ",")
}

// waitClosed waits until the facade closed the connection and Serve
// returned; messages still in flight are kept.
func (tc *testClient) waitClosed() {
	tc.t.Helper()

	deadline := time.After(testWait)

	for {
		select {
		case msg, ok := <-tc.msgs:
			if !ok {
				select {
				case <-tc.done:
				case <-deadline:
					tc.t.Fatal("Serve did not return")
				}

				return
			}

			switch m := msg.(type) {
			case godap.EventMessage:
				tc.events = append(tc.events, m)
			case godap.ResponseMessage:
				tc.responses[m.GetResponse().RequestSeq] = m
			}
		case <-deadline:
			tc.t.Fatal("the facade did not close the connection")
		}
	}
}
