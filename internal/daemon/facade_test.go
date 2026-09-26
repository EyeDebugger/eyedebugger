// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// dapFrame frames content as one DAP message.
func dapFrame(content string) string {
	return "Content-Length: " + strconv.Itoa(len(content)) + "\r\n\r\n" + content
}

const initializeRequest = `{"seq":1,"type":"request","command":"initialize","arguments":{"adapterID":"x"}}`

// rawConn is an unauthenticated connection to the daemon.
func rawConn(t *testing.T, p Paths) (conn net.Conn, c *api.Conn) {
	t.Helper()

	var d net.Dialer

	conn, err := d.DialContext(t.Context(), "unix", p.Socket)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	return conn, api.NewConn(conn, conn)
}

// line encodes a request as one JSON-RPC line.
func line(t *testing.T, id int64, method string, params any) string {
	t.Helper()

	req, err := api.NewRequest(id, method, params)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw) + "\n"
}

func hello(t *testing.T, p Paths) api.HelloParams {
	t.Helper()

	token, err := readShared(p.Token)
	if err != nil {
		t.Fatal(err)
	}

	return api.HelloParams{Token: string(token), ProtocolVersion: api.ProtocolVersion}
}

// readResponse reads one JSON-RPC response and decodes its result into v.
func readResponse(t *testing.T, c *api.Conn, v any) error {
	t.Helper()

	var resp api.Response
	if err := c.Read(&resp); err != nil {
		t.Fatalf("read response: %v", err)
	}

	return resp.Decode(v)
}

// TestFacadeOpenPipelined: hello, facade.open and a DAP request sent in one
// write get the open result, then the DAP response: nothing the client
// pipelined past the switch is lost.
func TestFacadeOpenPipelined(t *testing.T) {
	t.Parallel()

	ts := startServer(t)
	snap := startFake(t, ts.paths, "", "")
	conn, c := rawConn(t, ts.paths)

	all := line(t, 1, api.MethodHello, hello(t, ts.paths)) +
		line(t, 2, api.MethodFacadeOpen, api.FacadeOpenParams{SessionRef: api.SessionRef{SessionID: snap.Session.ID, Client: humanID}}) +
		dapFrame(initializeRequest)
	if _, err := conn.Write([]byte(all)); err != nil {
		t.Fatal(err)
	}

	if err := readResponse(t, c, nil); err != nil {
		t.Fatalf("hello: %v", err)
	}

	var open api.FacadeOpenResult
	if err := readResponse(t, c, &open); err != nil || open.SessionID != snap.Session.ID || open.FacadeVersion != api.FacadeVersion {
		t.Fatalf("facade.open = %+v, %v", open, err)
	}

	raw, err := dap.ReadMessage(c.Reader(), 1<<20)
	if err != nil {
		t.Fatalf("read the DAP response: %v", err)
	}

	msg, err := godap.DecodeProtocolMessage(raw)
	if resp, ok := msg.(*godap.InitializeResponse); err != nil || !ok || !resp.Success || resp.RequestSeq != 1 {
		t.Fatalf("DAP response = %s (%v)", raw, err)
	}

	// JSON-RPC is gone for good: a JSON-RPC line closes the connection.
	if _, err := conn.Write([]byte(line(t, 3, api.MethodDaemonStatus, nil))); err != nil {
		t.Fatal(err)
	}

	if rest, err := io.ReadAll(c.Reader()); err != nil || len(rest) != 0 {
		t.Errorf("after a JSON-RPC line: read %q, %v; want the connection closed", rest, err)
	}

	cl := dialUntil(t, ts.paths, ts.done)
	assertLive(t, cl, snap.Session.ID)
}

// assertLive checks that the daemon answers and the session is live.
func assertLive(t *testing.T, cl *Client, id string) {
	t.Helper()
	defer cl.Close()

	var snap api.Snapshot
	if err := cl.Call(t.Context(), api.MethodSessionStatus, api.StatusParams{SessionRef: api.SessionRef{SessionID: id}}, &snap); err != nil ||
		snap.Session.State == api.StateExited {
		t.Errorf("session after the facade connection = %+v, %v", snap.Session, err)
	}
}

func TestFacadeOpenUnauthenticated(t *testing.T) {
	t.Parallel()

	req, err := api.NewRequest(1, api.MethodFacadeOpen, api.FacadeOpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	assertRejected(t, startServer(t), req)
}

// TestFacadeOpenErrors: a refused facade.open leaves the connection
// speaking JSON-RPC.
func TestFacadeOpenErrors(t *testing.T) {
	t.Parallel()

	ts := startServer(t)
	live := startFake(t, ts.paths, "", "")

	tests := []struct {
		name string
		ref  api.SessionRef
		want api.Code
	}{
		{name: "bad client", ref: api.SessionRef{SessionID: live.Session.ID, Client: "robot"}, want: api.CodeInvalidRequest},
		{name: "unknown session", ref: api.SessionRef{SessionID: "s-none"}, want: api.CodeNoSession},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cl := dialUntil(t, ts.paths, ts.done)
			defer cl.Close()

			err := cl.Call(t.Context(), api.MethodFacadeOpen, api.FacadeOpenParams{SessionRef: tt.ref}, nil)
			if got := api.CodeOf(err); got != tt.want {
				t.Errorf("facade.open = %v, want %s", err, tt.want)
			}

			var st api.StatusResult
			if err := cl.Call(t.Context(), api.MethodDaemonStatus, nil, &st); err != nil {
				t.Errorf("daemon.status after a refused facade.open: %v", err)
			}
		})
	}
}

// TestFacadeOpenExited: a session whose program exited is SESSION_EXITED.
func TestFacadeOpenExited(t *testing.T) {
	t.Parallel()

	ts := startServer(t)
	snap := startFake(t, ts.paths, "", "", "lines=2")

	var res api.Snapshot
	mustCall(t, ts.paths, api.MethodExec, api.ExecParams{SessionRef: api.SessionRef{SessionID: snap.Session.ID}, Kind: "continue"}, &res)

	if res.Session.State != api.StateExited {
		t.Fatalf("session = %+v, want exited", res.Session)
	}

	err := callAs(t, ts.paths, api.MethodFacadeOpen, api.FacadeOpenParams{SessionRef: api.SessionRef{SessionID: snap.Session.ID}}, nil)
	if got := api.CodeOf(err); got != api.CodeSessionExited {
		t.Errorf("facade.open on an exited session = %v", err)
	}
}

// TestFacadeClientDetach: the client side of the switch, and the daemon
// closing an open facade connection when it stops.
func TestFacadeClientDetach(t *testing.T) {
	t.Parallel()

	ts := startServer(t)
	snap := startFake(t, ts.paths, "", "")

	cl, err := Dial(t.Context(), ts.paths, testInfo)
	if err != nil {
		t.Fatal(err)
	}

	var open api.FacadeOpenResult
	if err := cl.Call(t.Context(), api.MethodFacadeOpen, api.FacadeOpenParams{SessionRef: api.SessionRef{SessionID: snap.Session.ID}}, &open); err != nil {
		t.Fatal(err)
	}

	r, conn := cl.Detach()
	defer conn.Close()

	if _, err := conn.Write([]byte(dapFrame(initializeRequest))); err != nil {
		t.Fatal(err)
	}

	br := bufio.NewReader(r)

	raw, err := dap.ReadMessage(br, 1<<20)
	if err != nil || !bytes.Contains(raw, []byte(`"command":"initialize"`)) {
		t.Fatalf("DAP response = %s, %v", raw, err)
	}

	ts.stop()

	if err := ts.wait(t); err != nil {
		t.Fatalf("Serve = %v", err)
	}

	if _, err := dap.ReadMessage(br, 1<<20); !errors.Is(err, io.EOF) {
		t.Errorf("read after the daemon stopped = %v, want EOF", err)
	}
}
