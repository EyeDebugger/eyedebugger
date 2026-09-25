// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package helpertest is a fake side helper for tests: the test binary
// re-executes itself as the helper, in a mode that says how it behaves. A
// package whose tests run it calls MaybeRun first in TestMain.
package helpertest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
)

// Environment of a fake helper.
const (
	// EnvMode selects the fake helper's mode; set, the test binary runs as
	// the fake helper (MaybeRun).
	EnvMode = "EYEDBG_TEST_FAKE_HELPER"
	// EnvSamples is how many counters.sample notifications a counters call
	// sends in ModeOK (default 3).
	EnvSamples = "EYEDBG_TEST_FAKE_HELPER_SAMPLES"
	// EnvEnd is the endReason ModeOK's counters result reports (default
	// "duration").
	EnvEnd = "EYEDBG_TEST_FAKE_HELPER_END"
)

// Modes.
const (
	// ModeOK answers hello with protocol 1; processes with its own pid and
	// its parent's; counters with EnvSamples samples, then its result — or,
	// with durationMs 0, the samples and then nothing until stdin closes.
	ModeOK = "ok"
	// ModeProtocol2 answers hello with protocol 2.
	ModeProtocol2 = "protocol2"
	// ModeCrash writes to stderr and exits with code 3 before hello.
	ModeCrash = "crash"
	// ModeFrameworkMissing exits like the dotnet host missing a framework
	// (FrameworkMissingCode) before hello.
	ModeFrameworkMissing = "framework-missing"
	// ModeSilent never writes; it exits when stdin closes.
	ModeSilent = "silent"
	// ModeStall answers hello, then nothing; it exits when stdin closes.
	ModeStall = "stall"
	// ModeHuge answers hello, then a call with a line over 1 MiB.
	ModeHuge = "huge"
	// ModeGarbage answers hello, then a call with a line that isn't JSON.
	ModeGarbage = "garbage"
	// ModeErrorPrefix + CODE answers hello, then a call with error CODE.
	ModeErrorPrefix = "error:"
	// ModeStderrFlood writes 1 MiB to stderr, then behaves as ModeOK.
	ModeStderrFlood = "stderr-flood"
	// ModeExitMid answers hello, sends one counters.sample, then exits 1.
	ModeExitMid = "exit-mid"
	// ModeIgnoreEOF answers hello, then neither answers nor exits when
	// stdin closes; only a kill ends it.
	ModeIgnoreEOF = "ignore-eof"
)

// FrameworkMissingCode is the dotnet host's exit code when the framework
// is missing (0x80008096; 150, its low byte, on Unix).
func FrameworkMissingCode() int {
	if runtime.GOOS == "windows" {
		return -0x7FFF7F6A // 0x80008096 as an int32
	}

	return 150
}

// StderrText is what ModeCrash writes to stderr.
const StderrText = "Unhandled exception. System.InvalidOperationException: \x1b[31mkaboom\x1b[0m"

// Command returns how to start a fake helper in mode: the test binary,
// and the environment selecting the mode.
func Command(mode string) (path string, env []string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, fmt.Errorf("locate the test binary: %w", err)
	}

	return exe, []string{EnvMode + "=" + mode}, nil
}

// MaybeRun runs the fake helper and exits the process, if this process was
// started as one (EnvMode set). Otherwise it returns at once.
func MaybeRun() {
	mode := os.Getenv(EnvMode)
	if mode == "" {
		return
	}

	os.Exit(run(mode, os.Stdin, os.Stdout, os.Stderr))
}

// JSON-RPC member names and values the fake writes.
const (
	jsonrpc  = "jsonrpc"
	version2 = "2.0"
)

type request struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type fake struct {
	mode string
	in   *bufio.Reader
	out  io.Writer
}

// run is the fake helper; it returns the exit code.
func run(mode string, stdin io.Reader, stdout, stderr io.Writer) int {
	switch mode {
	case ModeCrash:
		_, _ = io.WriteString(stderr, StderrText+"\n   at Program.Main()\n")

		return 3
	case ModeFrameworkMissing:
		_, _ = io.WriteString(stderr, "You must install or update .NET to run this application.\n")

		return FrameworkMissingCode()
	case ModeStderrFlood:
		_, _ = stderr.Write([]byte(strings.Repeat("noise\n", (1<<20)/6)))
		mode = ModeOK
	}

	f := &fake{mode: mode, in: bufio.NewReaderSize(stdin, 1<<20), out: stdout}

	if mode == ModeSilent {
		_, _ = io.Copy(io.Discard, f.in)

		return 0
	}

	hello, ok := f.next()
	if !ok || hello.Method != "hello" {
		return 2
	}

	protocol := 1
	if mode == ModeProtocol2 {
		protocol = 2
	}

	f.result(hello.ID, map[string]any{"protocol": protocol, "version": "fake", "runtime": runtime.Version()})

	call, ok := f.next()
	if !ok {
		return 0
	}

	return f.serve(call)
}

func (f *fake) serve(call request) int {
	switch {
	case f.mode == ModeStall:
		_, _ = io.Copy(io.Discard, f.in)

		return 0
	case f.mode == ModeIgnoreEOF:
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		<-ch

		return 0
	case f.mode == ModeHuge:
		f.result(call.ID, strings.Repeat("x", 1<<20))

		return 0
	case f.mode == ModeGarbage:
		_, _ = io.WriteString(f.out, "not json\n")

		return 0
	case strings.HasPrefix(f.mode, ModeErrorPrefix):
		f.fail(call.ID, strings.TrimPrefix(f.mode, ModeErrorPrefix), "fake \x1b[2Jerror", "fake hint")

		return 0
	case f.mode == ModeExitMid:
		f.sample(1)

		return 1
	}

	switch call.Method {
	case "processes":
		f.result(call.ID, map[string]any{"processes": []map[string]int{{"pid": os.Getpid()}, {"pid": os.Getppid()}}})
	case "counters":
		return f.counters(call)
	default:
		f.fail(call.ID, "UNKNOWN_METHOD", "unknown method", "")
	}

	return 0
}

func (f *fake) counters(call request) int {
	var p struct {
		DurationMs int64 `json:"durationMs"`
	}

	_ = json.Unmarshal(call.Params, &p)

	n := 3
	if s := os.Getenv(EnvSamples); s != "" {
		n, _ = strconv.Atoi(s)
	}

	for i := 1; i <= n; i++ {
		f.sample(i)
	}

	if p.DurationMs == 0 { // until stdin closes, then no answer
		_, _ = io.Copy(io.Discard, f.in)

		return 0
	}

	end := os.Getenv(EnvEnd)
	if end == "" {
		end = "duration"
	}

	f.result(call.ID, map[string]any{"samples": n, "endReason": end})

	return 0
}

// sample sends counters.sample number i (1-based): i seconds in, cpu-usage
// 10·i %, working-set 100+i MB, exception-count i.
func (f *fake) sample(i int) {
	counter := func(name, display, unit, kind string, value float64) map[string]any {
		return map[string]any{
			"provider": "System.Runtime", "name": name, "displayName": display, "unit": unit, "kind": kind, "value": value,
		}
	}

	f.write(map[string]any{jsonrpc: version2, "method": "counters.sample", "params": map[string]any{
		"elapsedMs": 1000 * i,
		"counters": []map[string]any{
			counter("cpu-usage", "CPU Usage", "%", "gauge", float64(10*i)),
			counter("working-set", "Working Set", "MB", "gauge", float64(100+i)),
			counter("exception-count", "Exception Count", "", "sum", float64(i)),
		},
	}})
}

// next reads the next request; false at the end of stdin or on bad input.
func (f *fake) next() (request, bool) {
	line, err := f.in.ReadBytes('\n')
	if err != nil && (!errors.Is(err, io.EOF) || len(line) == 0) {
		return request{}, false
	}

	var req request

	return req, json.Unmarshal(line, &req) == nil
}

func (f *fake) result(id int64, result any) {
	f.write(map[string]any{jsonrpc: version2, "id": id, "result": result})
}

// fail sends an error response with the daemon's error shape.
func (f *fake) fail(id int64, code, message, hint string) {
	f.write(map[string]any{jsonrpc: version2, "id": id, "error": map[string]any{
		"code": -32000, "message": message, "data": map[string]any{"code": code, "message": message, "hint": hint},
	}})
}

func (f *fake) write(v map[string]any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // the fake's own messages always encode
	}

	_, _ = f.out.Write(append(b, '\n'))
}
