// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// EnvFakeChild is the variable that makes [MaybeRun] serve as a fake child
// ([ServeChild]) dialing its value, a loopback TCP address.
const EnvFakeChild = "EYEDBG_TEST_FAKE_CHILD"

// ChildCommand returns how to start a fake child dialing addr: this test
// binary, with no tests to run and [EnvFakeChild] set.
func ChildCommand(addr string) (*exec.Cmd, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the test binary: %w", err)
	}

	cmd := exec.CommandContext(context.Background(), exe, noTestsArg)
	cmd.Env = append(os.Environ(), EnvFakeChild+"="+addr)

	return cmd, nil
}

// startChild starts a fake child dialing addr, as a plain child of this
// process (in its process group on Unix, its child on Windows), and returns
// once the listener has accepted the child and read its pid. Its stdin and stderr are
// the null device, and nothing waits for it.
func startChild(addr string) error {
	cmd, err := ChildCommand(addr)
	if err != nil {
		return err
	}

	ready, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("child stdout: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", cmd.Path, err)
	}

	defer ready.Close()

	line, err := bufio.NewReader(ready).ReadString('\n')
	if line != childReady {
		return fmt.Errorf("the child did not start: %q, %w", line, err)
	}

	return nil
}

// childReady is what a fake child writes to its stdout once the listener
// has read its pid.
const childReady = "ready\n"

// childAck is the listener's answer to a fake child's pid.
const childAck = "ok\n"

// ServeChild dials addr, writes its pid as a line, waits for the
// listener's answer ([AcceptChild]), says so on stdout ([childReady]), and
// then echoes what it reads until the connection ends: whoever listens at
// addr learns the child is alive from an echo, and that it is gone from
// the connection ending. Until it has answered, nothing can kill the child
// unseen: on Windows a connection reset before it is accepted is dropped.
func ServeChild(addr string) error {
	var d net.Dialer

	conn, err := d.DialContext(context.Background(), "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, strconv.Itoa(os.Getpid())+"\n"); err != nil {
		return fmt.Errorf("write pid: %w", err)
	}

	r := bufio.NewReader(conn)

	if ack, err := r.ReadString('\n'); ack != childAck {
		return fmt.Errorf("the listener answered %q, %w", ack, err)
	}

	if _, err := os.Stdout.WriteString(childReady); err != nil {
		return fmt.Errorf("say ready: %w", err)
	}

	_, _ = io.Copy(conn, r)

	return nil
}

// Child is the listener's end of a fake child's connection.
type Child struct {
	Conn net.Conn
	PID  int

	r *bufio.Reader
}

// AcceptChild accepts a fake child's connection on ln, reads its pid and
// answers it: only then does the child go on.
func AcceptChild(ln net.Listener) (*Child, error) {
	conn, err := ln.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept the child: %w", err)
	}

	r := bufio.NewReader(conn)

	line, err := r.ReadString('\n')
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("read the child's pid: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("child pid %q: %w", line, err)
	}

	if _, err := io.WriteString(conn, childAck); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("answer the child: %w", err)
	}

	return &Child{Conn: conn, PID: pid, r: r}, nil
}

// Alive is whether the child echoes a ping (an error: no longer).
func (c *Child) Alive() error {
	if _, err := io.WriteString(c.Conn, "ping\n"); err != nil {
		return fmt.Errorf("ping the child: %w", err)
	}

	line, err := c.r.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read the child's echo: %w", err)
	}

	if line != "ping\n" {
		return fmt.Errorf("the child echoed %q", line)
	}

	return nil
}

// Gone waits until the child's connection ends — the child exited or was
// killed: its end closed or reset — and fails only when a deadline set on
// Conn passes first.
func (c *Child) Gone() error {
	_, err := io.Copy(io.Discard, c.r)
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return fmt.Errorf("wait for the child to go: %w", err)
	}

	return nil
}
