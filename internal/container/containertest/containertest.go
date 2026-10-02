// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package containertest is a fake docker CLI for tests: the test binary
// re-executes itself as docker, answering from a scenario. A package whose
// tests use [Engine] calls [MaybeRun] first in TestMain.
package containertest

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/container"
)

// EnvScenario carries the scenario (JSON) to the fake docker process; set,
// the test binary runs as the fake (MaybeRun). Only the fake's own
// environment has it.
const EnvScenario = "EYEDBG_TEST_FAKE_DOCKER"

// Rule is one answer of the fake.
type Rule struct {
	// Match is the start of the argv after the engine flags (--host=,
	// --context=); "*" matches any one element. The first rule that matches
	// answers.
	Match  []string `json:"match"`
	Stdout string   `json:"stdout,omitempty"`
	Stderr string   `json:"stderr,omitempty"`
	Exit   int      `json:"exit,omitempty"`
	// SaveStdin is a file the fake writes its standard input to.
	SaveStdin string `json:"saveStdin,omitempty"`
	// Flood is how many bytes of "x" the fake writes to stdout after Stdout.
	Flood int `json:"flood,omitempty"`
	// Hang makes the fake wait until it is killed, after reading nothing.
	Hang bool `json:"hang,omitempty"`
}

// Scenario is how the fake behaves.
type Scenario struct {
	// Calls is a file the fake appends each call's full argv to, one JSON
	// array per line.
	Calls string `json:"calls,omitempty"`
	Rules []Rule `json:"rules"`
}

// Engine returns an engine that runs the fake docker with scenario sc.
func Engine(tb testing.TB, sc Scenario) container.Engine {
	tb.Helper()

	self, err := os.Executable()
	if err != nil {
		tb.Fatal(err)
	}

	raw, err := json.Marshal(sc)
	if err != nil {
		tb.Fatal(err)
	}

	return container.Engine{
		Docker: self,
		Command: func(ctx context.Context, argv []string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, self, argv...)
			cmd.Env = append(os.Environ(), EnvScenario+"="+string(raw))

			return cmd
		},
	}
}

// ReadCalls reads the argv lists a scenario's Calls file holds.
func ReadCalls(tb testing.TB, path string) [][]string {
	tb.Helper()

	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		tb.Fatal(err)
	}
	defer f.Close()

	var calls [][]string

	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)

	for sc.Scan() {
		var argv []string
		if err := json.Unmarshal(sc.Bytes(), &argv); err != nil {
			tb.Fatal(err)
		}

		calls = append(calls, argv)
	}

	if err := sc.Err(); err != nil {
		tb.Fatal(err)
	}

	return calls
}

// MaybeRun runs the fake docker and exits when the process is one (its
// environment has [EnvScenario]); otherwise it returns at once.
func MaybeRun() {
	raw := os.Getenv(EnvScenario)
	if raw == "" {
		return
	}

	os.Exit(run(raw, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(raw string, argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var sc Scenario
	if err := json.Unmarshal([]byte(raw), &sc); err != nil {
		_, _ = io.WriteString(stderr, "fake docker: bad scenario: "+err.Error()+"\n")

		return 125
	}

	if sc.Calls != "" {
		if err := appendCall(sc.Calls, argv); err != nil {
			_, _ = io.WriteString(stderr, "fake docker: "+err.Error()+"\n")

			return 125
		}
	}

	rest := argv
	for len(rest) > 0 && (strings.HasPrefix(rest[0], "--host=") || strings.HasPrefix(rest[0], "--context=")) {
		rest = rest[1:]
	}

	for _, r := range sc.Rules {
		if !matches(r.Match, rest) {
			continue
		}

		if r.SaveStdin != "" {
			data, _ := io.ReadAll(stdin)
			if err := os.WriteFile(r.SaveStdin, data, 0o600); err != nil {
				_, _ = io.WriteString(stderr, "fake docker: "+err.Error()+"\n")

				return 125
			}
		}

		if r.Hang {
			time.Sleep(time.Hour)
		}

		_, _ = io.WriteString(stdout, r.Stdout)
		_, _ = io.WriteString(stdout, strings.Repeat("x", r.Flood))
		_, _ = io.WriteString(stderr, r.Stderr)

		return r.Exit
	}

	_, _ = io.WriteString(stderr, "fake docker: no rule for "+strings.Join(rest, " ")+"\n")

	return 125
}

func matches(pattern, argv []string) bool {
	if len(argv) < len(pattern) {
		return false
	}

	for i, p := range pattern {
		if p != "*" && p != argv[i] {
			return false
		}
	}

	return true
}

func appendCall(path string, argv []string) error {
	line, err := json.Marshal(argv)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}

	_, werr := f.Write(append(line, '\n'))

	if cerr := f.Close(); werr == nil {
		werr = cerr
	}

	return werr
}
