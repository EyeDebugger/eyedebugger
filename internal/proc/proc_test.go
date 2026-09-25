// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"testing"
)

// envHelper makes the test binary a child that waits for a signal.
const envHelper = "EYEDBG_TEST_PROC_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(envHelper) {
	case "exit":
		return
	case "wait":
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)

		_, _ = os.Stdout.WriteString("ready\n")

		<-ch

		return
	}

	m.Run()
}

func TestLookupSelf(t *testing.T) {
	t.Parallel()

	info, err := Lookup(t.Context(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	if !info.SameUser || info.Name == "" || info.Owner == "" || info.PID != os.Getpid() {
		t.Fatalf("Lookup(self) = %+v, want same user with a name and owner", info)
	}
}

func TestLookupNoProcess(t *testing.T) {
	t.Parallel()

	if _, err := Lookup(t.Context(), 0); !errors.Is(err, ErrNoProcess) {
		t.Fatalf("Lookup(0) err = %v, want ErrNoProcess", err)
	}

	// Some pid in a high range is free; which one is up to the system.
	for pid := 1<<22 - 4; pid > 1<<22-400; pid -= 4 {
		_, err := Lookup(t.Context(), pid)
		if errors.Is(err, ErrNoProcess) {
			return
		}

		if err != nil {
			t.Fatalf("Lookup(%d) err = %v, want ErrNoProcess or success", pid, err)
		}
	}

	t.Fatal("every probed pid exists")
}

func TestKillGroup(t *testing.T) {
	t.Parallel()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), envHelper+"=wait")

	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := StartGroup(cmd); err != nil {
		t.Fatal(err)
	}

	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper said %q, %v", line, err)
	}

	if err := KillGroup(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}

	err = cmd.Wait()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); !ok || exitErr.Success() {
		t.Fatalf("Wait after KillGroup = %v, want killed", err)
	}

	if err := KillGroup(t.Context(), cmd); err != nil {
		t.Fatalf("second KillGroup = %v, want nil for a gone group", err)
	}
}
