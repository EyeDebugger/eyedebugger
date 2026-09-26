// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

var testInfo = version.Info{Version: "v0.0.0-test", Commit: "abc1234"}

// shortTempDir returns a temp dir short enough for a Unix socket path;
// t.TempDir paths on macOS can exceed the 104-byte limit.
func shortTempDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "edb") //nolint:usetesting // t.TempDir paths are too long for a Unix socket on macOS.
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "rt")
}

type testServer struct {
	paths Paths
	done  chan error
	stop  context.CancelFunc
}

// startServer runs Serve in the background and waits until it accepts
// connections. Cleanup stops it and waits for Serve to return.
func startServer(t *testing.T) *testServer {
	t.Helper()

	return startServerIn(t, PathsIn(shortTempDir(t)), time.Hour)
}

// startServerIn is startServer with the fake driver, in paths.
func startServerIn(t *testing.T, paths Paths, idle time.Duration) *testServer {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	ts := &testServer{paths: paths, done: make(chan error, 1), stop: cancel}

	go func() {
		ts.done <- Serve(ctx, Config{
			Paths:       ts.paths,
			IdleTimeout: idle,
			Info:        testInfo,
			Logger:      slog.New(slog.DiscardHandler),
			Drivers:     []session.Driver{fakeDriver{}},
		})
	}()

	t.Cleanup(func() {
		cancel()

		if err := ts.wait(t); err != nil {
			t.Errorf("Serve = %v", err)
		}
	})

	cl := dialUntil(t, ts.paths, ts.done)
	_ = cl.Close()

	return ts
}

// wait returns Serve's result, failing the test if it doesn't return soon.
func (ts *testServer) wait(t *testing.T) error {
	t.Helper()

	select {
	case err := <-ts.done:
		ts.done <- err // keep it readable for later calls

		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")

		return nil
	}
}

func dialUntil(t *testing.T, p Paths, done <-chan error) *Client {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		cl, err := Dial(ctx, p, testInfo)
		if err == nil {
			return cl
		}

		select {
		case err := <-done:
			t.Fatalf("Serve returned before accepting connections: %v", err)
		case <-ctx.Done():
			t.Fatalf("daemon never accepted connections: %v", err)
		case <-ticker.C:
		}
	}
}

func TestHelloStatusStop(t *testing.T) {
	t.Parallel()

	ts := startServer(t)

	cl, err := Dial(t.Context(), ts.paths, testInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	if cl.Hello.ProtocolVersion != api.ProtocolVersion || cl.Hello.Version != testInfo.Version ||
		cl.Hello.Commit != testInfo.Commit || cl.Hello.PID != os.Getpid() {
		t.Errorf("hello = %+v", cl.Hello)
	}

	assertStatus(t, cl, ts.paths, time.Hour)

	var res api.StopResult
	if err := cl.Call(t.Context(), api.MethodDaemonStop, api.StopParams{}, &res); err != nil {
		t.Fatal(err)
	}

	if err := ts.wait(t); err != nil {
		t.Fatalf("Serve = %v, want nil", err)
	}

	for _, f := range []string{ts.paths.Socket, ts.paths.Token} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists after stop (err = %v)", f, err)
		}
	}
}

func assertStatus(t *testing.T, cl *Client, p Paths, idle time.Duration) {
	t.Helper()

	var st api.StatusResult
	if err := cl.Call(t.Context(), api.MethodDaemonStatus, nil, &st); err != nil {
		t.Fatal(err)
	}

	if st.Sessions != 0 || st.Socket != p.Socket || time.Duration(st.IdleTimeout) != idle ||
		st.IdleExitAt == nil || !st.IdleExitAt.Equal(st.StartedAt.Add(idle)) {
		t.Errorf("status = %+v", st)
	}
}

func TestUnknownMethod(t *testing.T) {
	t.Parallel()

	ts := startServer(t)

	cl, err := Dial(t.Context(), ts.paths, testInfo)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	err = cl.Call(t.Context(), "no.such.method", nil, nil)
	if got := api.CodeOf(err); got != api.CodeUnknownMethod {
		t.Errorf("code = %q (err = %v), want %q", got, err, api.CodeUnknownMethod)
	}
}

func TestRejectsUnauthenticatedConnections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		req  func() (api.Request, error)
	}{
		{"wrong token", func() (api.Request, error) {
			return api.NewRequest(1, api.MethodHello, api.HelloParams{Token: "wrong", ProtocolVersion: api.ProtocolVersion})
		}},
		{"no hello first", func() (api.Request, error) {
			return api.NewRequest(1, api.MethodDaemonStop, api.StopParams{Force: true})
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req, err := tt.req()
			if err != nil {
				t.Fatal(err)
			}

			assertRejected(t, startServer(t), req)
		})
	}
}

// assertRejected sends req as a connection's first request and checks that
// the daemon answers UNAUTHORIZED, closes the connection and keeps running.
func assertRejected(t *testing.T, ts *testServer, req api.Request) {
	t.Helper()

	var d net.Dialer

	conn, err := d.DialContext(t.Context(), "unix", ts.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	c := api.NewConn(conn, conn)
	if err := c.Write(req); err != nil {
		t.Fatal(err)
	}

	var resp api.Response
	if err := c.Read(&resp); err != nil {
		t.Fatal(err)
	}

	if got := api.CodeOf(resp.Decode(nil)); got != api.CodeUnauthorized {
		t.Errorf("code = %q, want %q", got, api.CodeUnauthorized)
	}

	if err := c.Read(&resp); !errors.Is(err, io.EOF) {
		t.Errorf("connection still open after rejection: read err = %v", err)
	}

	cl, err := Dial(t.Context(), ts.paths, testInfo)
	if err != nil {
		t.Fatalf("daemon stopped after rejected request: %v", err)
	}
	_ = cl.Close()
}

func TestOneDaemonPerUser(t *testing.T) {
	t.Parallel()

	ts := startServer(t)

	err := Serve(t.Context(), Config{Paths: ts.paths, Info: testInfo, Logger: slog.New(slog.DiscardHandler)})
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Serve = %v, want ErrAlreadyRunning", err)
	}

	// The first daemon is unaffected, including its socket and token.
	cl, err := Dial(t.Context(), ts.paths, testInfo)
	if err != nil {
		t.Fatalf("first daemon unreachable after second start: %v", err)
	}
	_ = cl.Close()
}

func TestIdleExit(t *testing.T) {
	t.Parallel()

	// No connection is needed (or reliable: a slow machine may already be
	// idle before a client dials): Serve must return nil on its own.
	err := Serve(t.Context(), Config{
		Paths:       PathsIn(shortTempDir(t)),
		IdleTimeout: 50 * time.Millisecond,
		Info:        testInfo,
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("Serve = %v, want nil after idle timeout", err)
	}
}

func TestExitsWhenSocketRemoved(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the removal it guards against is systemd's cleanup of $XDG_RUNTIME_DIR")
	}

	ts := startServer(t)

	if err := os.Remove(ts.paths.Socket); err != nil {
		t.Fatal(err)
	}

	if err := ts.wait(t); err != nil {
		t.Fatalf("Serve = %v, want nil after its socket was removed", err)
	}
}

func TestStaleFilesAreReplaced(t *testing.T) {
	t.Parallel()

	p := PathsIn(shortTempDir(t))
	if err := p.Prepare(); err != nil {
		t.Fatal(err)
	}

	// Leftovers of a daemon that was killed: a socket path and an old token.
	for _, f := range []string{p.Socket, p.Token} {
		if err := os.WriteFile(f, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- Serve(ctx, Config{Paths: p, Info: testInfo, Logger: slog.New(slog.DiscardHandler)})
	}()

	cl := dialUntil(t, p, done)
	_ = cl.Close()

	cancel()

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A daemon replaces its token while clients read it: every Dial reads the
// token, and after a killed daemon left its token behind a client polling
// for the new one reads that. On Windows a rename can't replace an open
// file, so replaceFile retries; without that, about every other write here
// failed with "Access is denied" and the daemon exited during start-up.
// TestWriteFileAtomicWhileRead models Dial: readers use readShared, the same
// way Dial reads the token, in a tight loop with no pause. On Windows this
// no longer waits for a gap between reads (see replaceFile/openShared); on
// Unix it always passed.
func TestWriteFileAtomicWhileRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})

	const readers = 2
	for range readers {
		go func() {
			defer func() { done <- struct{}{} }()

			for {
				select {
				case <-stop:
					return
				default:
					_, _ = readShared(path)
				}
			}
		}()
	}

	var err error
	for i := 0; i < 50 && err == nil; i++ {
		err = writeFileAtomic(path, []byte(strconv.Itoa(i)), 0o600)
	}

	close(stop)

	for range readers {
		<-done
	}

	if err != nil {
		t.Fatalf("writeFileAtomic while the file is read: %v", err)
	}

	if got, err := os.ReadFile(path); err != nil || string(got) != "49" {
		t.Fatalf("token = %q, %v; want the last write, 49", got, err)
	}
}

// TestWriteFileAtomicWhileOpen holds the file open, sharing delete
// (openShared), across a writeFileAtomic: on Windows this proves the
// POSIX-semantics rename succeeds against such a handle, deterministically
// (no timing). On Unix it passes trivially, since rename never blocks on an
// open file there.
func TestWriteFileAtomicWhileOpen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	held, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	if err := writeFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("writeFileAtomic while the file is open: %v", err)
	}

	if got, err := readShared(path); err != nil || string(got) != "new" {
		t.Fatalf("token = %q, %v; want %q", got, err, "new")
	}

	if got, err := io.ReadAll(held); err != nil || string(got) != "old" {
		t.Fatalf("the held handle read = %q, %v; want the pre-replace content %q", got, err, "old")
	}
}

func TestDialWhenNotRunning(t *testing.T) {
	t.Parallel()

	p := PathsIn(shortTempDir(t))

	_, err := Dial(t.Context(), p, testInfo)
	if got := api.CodeOf(err); got != api.CodeDaemonNotRunning {
		t.Fatalf("code = %q (err = %v), want %q", got, err, api.CodeDaemonNotRunning)
	}
}

func TestConnectNoAutostart(t *testing.T) {
	t.Parallel()

	p := PathsIn(shortTempDir(t))

	_, _, err := Connect(t.Context(), p, testInfo, StartOptions{NoAutostart: true})
	if got := api.CodeOf(err); got != api.CodeDaemonNotRunning {
		t.Fatalf("code = %q (err = %v), want %q", got, err, api.CodeDaemonNotRunning)
	}

	if !strings.Contains(errorHint(err), EnvNoAutostart) {
		t.Errorf("hint %q doesn't mention %s", errorHint(err), EnvNoAutostart)
	}
}

func TestPrepareRejectsLongSocketPath(t *testing.T) {
	t.Parallel()

	p := PathsIn(filepath.Join(shortTempDir(t), strings.Repeat("x", maxSocketPath)))

	if err := p.Prepare(); err == nil || !strings.Contains(err.Error(), EnvRuntimeDir) {
		t.Fatalf("Prepare = %v, want an error mentioning %s", err, EnvRuntimeDir)
	}
}

func TestTailLog(t *testing.T) {
	t.Parallel()

	p := PathsIn(t.TempDir())
	if err := os.WriteFile(p.Log, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		n    int
		want string
	}{
		{0, "a b c"},
		{2, "b c"},
		{5, "a b c"},
	}

	for _, tt := range tests {
		lines, err := TailLog(p, tt.n)
		if err != nil {
			t.Fatal(err)
		}

		if got := strings.Join(lines, " "); got != tt.want {
			t.Errorf("TailLog(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func errorHint(err error) string {
	if e, ok := errors.AsType[*api.Error](err); ok {
		return e.Hint
	}

	return ""
}
