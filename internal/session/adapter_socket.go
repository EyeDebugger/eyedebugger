// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap"
)

// The connect transport ([Launch.SocketArgs]).
const (
	// defaultConnectTimeout bounds how long an adapter may take to dial in.
	defaultConnectTimeout = 30 * time.Second
	// maxSocketPath is the longest socket path every platform takes: the
	// BSDs' and macOS's sun_path holds 104 bytes with its terminating NUL
	// (Linux's and Windows' 108).
	maxSocketPath = 103
	// socketName is the socket's name in its private directory.
	socketName = "dap.sock"
)

// startSocketAdapter is startAdapter on the connect transport. The socket
// lives in a fresh directory only this user can enter (the same trust as
// the daemon's own socket, docs/DESIGN.md §14), exists only until the
// adapter's one connection is accepted, and is gone, with its directory,
// on every return. An adapter that fails to connect is killed.
func (s *Session) startSocketAdapter(ctx context.Context, launch Launch, stderr io.Writer, timeout time.Duration) error {
	dir, err := os.MkdirTemp("", "eyedbg-dap-")
	if err != nil {
		return api.NewError(api.CodeAdapterFailed, "create the debug adapter's socket directory: "+err.Error(), "")
	}

	// Deferred first, so it runs after the listener's Close below.
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			s.logger.WarnContext(ctx, "remove the debug adapter's socket directory", slog.String("dir", dir), slog.Any("error", err))
		}
	}()

	path := filepath.Join(dir, socketName)
	if len(path) > maxSocketPath {
		return api.NewError(api.CodeAdapterFailed,
			"the debug adapter's socket path "+path+" is "+strconv.Itoa(len(path))+" bytes, over the limit of "+strconv.Itoa(maxSocketPath),
			"set TMPDIR (TEMP on Windows) to a shorter directory")
	}

	ln, err := listenSocket(ctx, path)
	if err != nil {
		return api.NewError(api.CodeAdapterFailed, "the debug adapter's socket: "+err.Error(), "")
	}
	defer func() { _ = ln.Close() }()

	cmd := exec.CommandContext(ctx, launch.Adapter, launch.SocketArgs(path)...) //nolint:gosec // The adapter path comes from the driver (a trusted manifest's executable).
	cmd.Env = append(cmd.Environ(), launch.AdapterEnv...)
	// Stdin stays nil (the null device): DAP runs on the socket.
	cmd.Stdout, cmd.Stderr = stderr, stderr

	if err := cmd.Start(); err != nil {
		return api.NewError(api.CodeAdapterFailed, "start "+launch.Adapter+": "+err.Error(), "check 'eyedbg adapters doctor'")
	}

	// This goroutine alone waits for the process; exited tells everyone
	// else it is gone.
	exited := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(exited)
	}()

	conn, err := acceptAdapter(ctx, ln, exited, timeout)
	if err != nil {
		_ = cmd.Process.Kill()

		// Return with the adapter gone, unless something it started holds
		// its output open.
		t := time.NewTimer(shutdownTimeout)
		defer t.Stop()

		select {
		case <-exited:
		case <-t.C:
		}

		return err
	}

	// Under mu: a test run's session is shared before its adapter starts.
	s.mu.Lock()
	s.cmd, s.conn, s.exited = cmd, conn, exited
	s.client = dap.NewClient(conn, conn, dap.Handlers{Event: s.onEvent})
	s.mu.Unlock()

	go s.watchAdapter()

	return nil
}

// listenSocket listens on a Unix socket at path.
func listenSocket(ctx context.Context, path string) (net.Listener, error) {
	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	// The directory already keeps others out; this is defense in depth.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()

		return nil, fmt.Errorf("restrict socket permissions: %w", err)
	}

	return ln, nil
}

// acceptAdapter waits for the adapter's connection: the first one wins. It
// fails when the adapter exits first, after timeout, or when ctx ends; ln is
// closed then and a connection that raced in is closed too, so nothing is
// left running or open.
func acceptAdapter(ctx context.Context, ln net.Listener, exited <-chan struct{}, timeout time.Duration) (net.Conn, error) {
	type accepted struct {
		conn net.Conn
		err  error
	}

	ch := make(chan accepted, 1)

	go func() {
		conn, err := ln.Accept()
		ch <- accepted{conn, err}
	}()

	t := time.NewTimer(timeout)
	defer t.Stop()

	var err error

	select {
	case a := <-ch:
		if a.err != nil {
			return nil, api.NewError(api.CodeAdapterFailed, "accept the debug adapter's connection: "+a.err.Error(), "")
		}

		return a.conn, nil
	case <-exited:
		err = api.NewError(api.CodeAdapterFailed, "the debug adapter exited before connecting", "see 'eyedbg daemon logs'")
	case <-t.C:
		err = api.NewError(api.CodeAdapterFailed, "the debug adapter did not connect within "+timeout.String(), "see 'eyedbg daemon logs'")
	case <-ctx.Done():
		err = fmt.Errorf("wait for the debug adapter to connect: %w", ctx.Err())
	}

	// Closing the listener ends the Accept; collect it.
	_ = ln.Close()

	if a := <-ch; a.conn != nil {
		_ = a.conn.Close()
	}

	return nil, err
}
