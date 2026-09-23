// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
)

// Environment of a fake test runner (see [RunnerCommand]).
const (
	EnvFakeRunner     = "EYEDBG_TEST_FAKE_RUNNER"
	EnvFakeRunnerCode = "EYEDBG_TEST_FAKE_RUNNER_CODE"
)

// Fake test runner modes.
const (
	// RunnerExit prints "Process Id: <its pid>, Name: testhost", waits
	// until a fake adapter attached to it has exited, prints "Passed!" and
	// exits with its code.
	RunnerExit = "exit"
	// RunnerTwice is RunnerExit with a second, different process id line.
	RunnerTwice = "twice"
	// RunnerHang prints the process id line and waits to be killed.
	RunnerHang = "hang"
	// RunnerNoPID prints a compiler error and exits with its code.
	RunnerNoPID = "nopid"
)

// MaybeRunRunner runs the fake test runner and exits, if this process was
// started by [RunnerCommand]. Otherwise it returns at once. Call it first in
// TestMain.
func MaybeRunRunner() {
	mode := os.Getenv(EnvFakeRunner)
	if mode == "" {
		return
	}

	code, _ := strconv.Atoi(os.Getenv(EnvFakeRunnerCode))

	if err := runRunner(mode); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "fake runner:", err)
		code = 2
	}

	os.Exit(code)
}

// RunnerCommand returns how to start a fake test runner in mode that exits
// with code.
func RunnerCommand(mode string, code int) (path string, args, env []string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, nil, fmt.Errorf("locate the test binary: %w", err)
	}

	return exe, []string{"-test.run=^$"}, []string{EnvFakeRunner + "=" + mode, EnvFakeRunnerCode + "=" + strconv.Itoa(code)}, nil
}

func runRunner(mode string) error {
	pidLine := "Process Id: " + strconv.Itoa(os.Getpid()) + ", Name: testhost\n"

	switch mode {
	case RunnerNoPID:
		_, err := os.Stdout.WriteString("/src/tests/A.cs(3,1): error CS1002: ; expected\n")

		return err
	case RunnerHang:
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)

		if _, err := os.Stdout.WriteString("Host debugging is enabled.\n" + pidLine); err != nil {
			return err
		}

		<-ch

		return nil
	case RunnerExit, RunnerTwice:
		if mode == RunnerTwice {
			pidLine += "Process Id: " + strconv.Itoa(os.Getpid()+1) + ", Name: testhost\n"
		}

		return waitForAttach(pidLine)
	default:
		return fmt.Errorf("unknown mode %q", mode)
	}
}

// waitForAttach publishes a gate for the fake adapter (a port in a file
// named after this process), prints pidLine, and returns once an adapter
// that connected to the gate has gone.
func waitForAttach(pidLine string) error {
	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	gate := gatePath(os.Getpid())

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return errors.New("listener has no TCP address")
	}

	if err := os.WriteFile(gate, []byte(strconv.Itoa(addr.Port)), 0o600); err != nil {
		return fmt.Errorf("write gate: %w", err)
	}
	defer os.Remove(gate)

	if _, err := os.Stdout.WriteString(pidLine); err != nil {
		return err
	}

	conn, err := ln.Accept()
	if err != nil {
		return fmt.Errorf("accept: %w", err)
	}
	defer conn.Close()

	_, _ = io.Copy(io.Discard, conn)

	_, err = os.Stdout.WriteString("Passed!  - Failed: 0, Passed: 1\n")

	return err
}

// gatePath is where a fake runner with pid publishes its gate's port.
func gatePath(pid int) string {
	return filepath.Join(os.TempDir(), "eyedbg-fake-runner-"+strconv.Itoa(pid))
}

// dialGate connects to the gate of the fake runner with pid, if it has one;
// the runner waits until the connection closes.
func dialGate(pid int) net.Conn {
	port, err := os.ReadFile(gatePath(pid))
	if err != nil { // no gate (fs.ErrNotExist): nothing waits
		return nil
	}

	var d net.Dialer

	conn, err := d.DialContext(context.Background(), "tcp", "127.0.0.1:"+strings.TrimSpace(string(port)))
	if err != nil {
		return nil
	}

	return conn
}
