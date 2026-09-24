// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dap

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	godap "github.com/google/go-dap"
)

// frameAll frames each content as one DAP message.
func frameAll(contents ...string) string {
	var buf bytes.Buffer
	for _, c := range contents {
		_ = writeMessage(&buf, []byte(c))
	}

	return buf.String()
}

func newTestServer(in string, w *bytes.Buffer) *Server {
	return NewServer(bufio.NewReader(strings.NewReader(in)), w, godap.NewCodec(), 1<<20)
}

func TestServerReadRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   []string
		// wantCommand is the request read; empty wants an error.
		wantCommand string
		// wantDecode wants a *DecodeError for this seq and command.
		wantDecode *DecodeError
		wantFatal  error
	}{
		{name: "request", in: []string{`{"seq":1,"type":"request","command":"threads"}`}, wantCommand: "threads"},
		{
			name: "responses and events are skipped",
			in: []string{
				`{"seq":1,"type":"response","request_seq":1,"command":"runInTerminal","success":true}`,
				`{"seq":2,"type":"event","event":"stopped"}`,
				`{"seq":3,"type":"other"}`,
				`{"seq":4,"type":"request","command":"threads"}`,
			},
			wantCommand: "threads",
		},
		{
			name:       "unknown command",
			in:         []string{`{"seq":7,"type":"request","command":"eyedbg/nothing"}`},
			wantDecode: &DecodeError{Seq: 7, Command: "eyedbg/nothing"},
		},
		{
			name:       "arguments of the wrong type",
			in:         []string{`{"seq":8,"type":"request","command":"continue","arguments":{"threadId":"x"}}`},
			wantDecode: &DecodeError{Seq: 8, Command: "continue"},
		},
		{name: "not JSON", in: []string{`{"seq":1,`}, wantFatal: ErrFraming},
		{name: "seq not a number", in: []string{`{"seq":"1","type":"request","command":"threads"}`}, wantFatal: ErrFraming},
		{name: "JSON-RPC line", in: nil, wantFatal: ErrFraming},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			in := frameAll(tt.in...)
			if tt.in == nil {
				in = `{"jsonrpc":"2.0","id":3,"method":"daemon.status"}` + "\r\n\r\n"
			}

			req, err := newTestServer(in, &bytes.Buffer{}).ReadRequest()
			checkReadRequest(t, req, err, tt.wantCommand, tt.wantDecode, tt.wantFatal)
		})
	}
}

// checkReadRequest checks ReadRequest's result: the request wantCommand,
// else a decode error like wantDecode, else the fatal error wantFatal.
func checkReadRequest(t *testing.T, req godap.RequestMessage, err error, wantCommand string, wantDecode *DecodeError, wantFatal error) {
	t.Helper()

	switch {
	case wantCommand != "":
		if err != nil || req.GetRequest().Command != wantCommand {
			t.Errorf("ReadRequest = %+v, %v; want %s", req, err, wantCommand)
		}
	case wantDecode != nil:
		de, ok := errors.AsType[*DecodeError](err)
		if !ok || de.Seq != wantDecode.Seq || de.Command != wantDecode.Command || de.Err == nil {
			t.Errorf("ReadRequest = %+v, %v; want a decode error for %+v", req, err, wantDecode)
		}
	default:
		if _, ok := errors.AsType[*DecodeError](err); ok || !errors.Is(err, wantFatal) {
			t.Errorf("ReadRequest = %+v, %v; want %v", req, err, wantFatal)
		}
	}
}

func TestServerResponses(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	s := newTestServer("", &buf)
	req := &godap.ThreadsRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: 5}, Command: "threads"}}

	if err := s.Respond(req, &godap.ThreadsResponse{}); err != nil {
		t.Fatal(err)
	}

	if err := s.Fail(req, "LEASE_HELD", &godap.ErrorMessage{Id: 7006, Format: "held"}); err != nil {
		t.Fatal(err)
	}

	if err := s.Send(&godap.StoppedEvent{Event: godap.Event{Event: "stopped"}}); err != nil {
		t.Fatal(err)
	}

	r := bufio.NewReader(&buf)
	want := []string{
		`{"seq":1,"type":"response","request_seq":5,"success":true,"command":"threads","body":{"threads":null}}`,
		`{"seq":2,"type":"response","request_seq":5,"success":false,"command":"threads","message":"LEASE_HELD","body":{"error":{"id":7006,"format":"held","showUser":false}}}`,
		`{"seq":3,"type":"event","event":"stopped","body":{"reason":""}}`,
	}

	for _, w := range want {
		got, err := ReadMessage(r, 1<<20)
		if err != nil || string(got) != w {
			t.Errorf("wrote %s, %v; want %s", got, err, w)
		}
	}
}

// TestServerConcurrentWrites checks that concurrent writers produce whole
// frames with strictly increasing seqs.
func TestServerConcurrentWrites(t *testing.T) {
	t.Parallel()

	const writers = 50

	var buf bytes.Buffer

	s := newTestServer("", &buf)

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() {
			if i%2 == 0 {
				req := &godap.ThreadsRequest{Request: godap.Request{ProtocolMessage: godap.ProtocolMessage{Seq: i}, Command: "threads"}}
				_ = s.Respond(req, &godap.ThreadsResponse{})
			} else {
				_ = s.Send(&godap.OutputEvent{Event: godap.Event{Event: "output"}, Body: godap.OutputEventBody{Output: strings.Repeat("o", 5000)}})
			}
		})
	}

	wg.Wait()

	r := bufio.NewReader(&buf)
	for want := 1; want <= writers; want++ {
		raw, err := ReadMessage(r, 1<<20)
		if err != nil {
			t.Fatalf("frame %d: %v", want, err)
		}

		var m godap.ProtocolMessage
		if err := json.Unmarshal(raw, &m); err != nil || m.Seq != want {
			t.Fatalf("frame %d has seq %d (%v)", want, m.Seq, err)
		}
	}

	if _, err := ReadMessage(r, 1<<20); err == nil {
		t.Error("more frames than writes")
	}
}

// TestServerWriteDeadline checks that a peer that never reads fails the
// write at the deadline, and every later write at once.
func TestServerWriteDeadline(t *testing.T) {
	t.Parallel()

	ours, peer := net.Pipe()
	t.Cleanup(func() { _ = ours.Close(); _ = peer.Close() })

	s := NewServer(bufio.NewReader(ours), ours, godap.NewCodec(), 1<<20)
	s.SetWriteTimeout(50 * time.Millisecond)

	ev := &godap.OutputEvent{Event: godap.Event{Event: "output"}, Body: godap.OutputEventBody{Output: "x"}}

	var ne net.Error
	if err := s.Send(ev); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Send to a peer that never reads = %v, want a timeout", err)
	}

	if err := s.Send(ev); !errors.Is(err, errBroken) {
		t.Errorf("Send after a failed write = %v, want %v", err, errBroken)
	}
}
