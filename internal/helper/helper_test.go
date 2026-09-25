// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/helper/helpertest"
)

// TestMain lets the test binary double as the fake helper.
func TestMain(m *testing.M) {
	helpertest.MaybeRun()
	os.Exit(m.Run())
}

// closeSlack is how much longer than its own bound Close may take on a
// loaded CI machine.
const closeSlack = 10 * time.Second

func fakeSpec(t *testing.T, mode string) Spec {
	t.Helper()

	path, env, err := helpertest.Command(mode)
	if err != nil {
		t.Fatal(err)
	}

	return Spec{
		Name: "the fake helper", Path: path, Env: env, Protocol: 1,
		RuntimeMissing: func(code int) *api.Error {
			if code == helpertest.FrameworkMissingCode() || int64(code) == 0x80008096 {
				return api.NewError(api.CodeHelperNotFound, "no runtime for the fake helper", "install one")
			}

			return nil
		},
	}
}

// start starts a fake helper and closes it when the test ends, checking
// that its process is gone then.
func start(t *testing.T, mode string) *Helper {
	t.Helper()

	h, err := Start(t.Context(), fakeSpec(t, mode))
	if err != nil {
		t.Fatalf("Start(%s): %v", mode, err)
	}

	t.Cleanup(func() {
		_ = h.Close()
		requireGone(t, h.PID())
	})

	return h
}

// requireGone checks that the helper's process has ended (processGone).
func requireGone(t *testing.T, pid int) {
	t.Helper()

	if err := processGone(pid); err != nil {
		t.Errorf("helper pid %d still there after Close: %v", pid, err)
	}
}

type countersResult struct {
	Samples   int    `json:"samples"`
	EndReason string `json:"endReason"`
}

type sample struct {
	ElapsedMs int64 `json:"elapsedMs"`
	Counters  []struct {
		Name  string  `json:"name"`
		Value float64 `json:"value"`
	} `json:"counters"`
}

func TestStartFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		mode     string
		want     api.Code
		contains []string
	}{
		{helpertest.ModeProtocol2, api.CodeHelperMismatch, []string{"protocol 2", "speaks 1"}},
		{helpertest.ModeCrash, api.CodeHelperFailed, []string{"exited (exit status 3) before answering", "its stderr:\n  Unhandled exception", "kaboom"}},
		{helpertest.ModeFrameworkMissing, api.CodeHelperNotFound, []string{"no runtime for the fake helper", "You must install"}},
		{helpertest.ModeSilent, api.CodeHelperFailed, []string{"didn't start within 300ms"}},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			t.Parallel()

			spec := fakeSpec(t, tt.mode)
			spec.HelloTimeout = 300 * time.Millisecond

			h, err := Start(t.Context(), spec)
			if err == nil {
				_ = h.Close()
				t.Fatal("Start succeeded")
			}

			e, ok := errors.AsType[*api.Error](err)
			if !ok || e.Code != tt.want {
				t.Fatalf("err = %v (%T), want code %s", err, err, tt.want)
			}

			for _, s := range tt.contains {
				if !strings.Contains(e.Message, s) {
					t.Errorf("message %q lacks %q", e.Message, s)
				}
			}

			if strings.ContainsRune(e.Message, '\x1b') {
				t.Errorf("message %q holds a control character", e.Message)
			}
		})
	}
}

func TestMissingExecutable(t *testing.T) {
	t.Parallel()

	spec := fakeSpec(t, helpertest.ModeOK)
	spec.Path = filepath.Join(t.TempDir(), "nope")

	_, err := Start(t.Context(), spec)
	if api.CodeOf(err) != api.CodeHelperNotFound {
		t.Fatalf("err = %v, want HELPER_NOT_FOUND", err)
	}
}

func TestCall(t *testing.T) {
	t.Parallel()

	h := start(t, helpertest.ModeOK)

	if got := h.Hello(); got.Protocol != 1 || got.Version != "fake" || got.Runtime == "" {
		t.Errorf("Hello() = %+v", got)
	}

	var res struct {
		Processes []struct {
			PID int `json:"pid"`
		} `json:"processes"`
	}

	if err := h.Call(t.Context(), "processes", struct{}{}, &res); err != nil {
		t.Fatal(err)
	}

	if len(res.Processes) != 2 || res.Processes[0].PID != h.PID() || res.Processes[1].PID != os.Getpid() {
		t.Errorf("processes = %+v, want the helper's pid %d and ours %d", res.Processes, h.PID(), os.Getpid())
	}
}

func TestStreamDeliversNotificationsInOrder(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{helpertest.ModeOK, helpertest.ModeStderrFlood} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			h := start(t, mode)

			var got []int64

			var res countersResult

			err := h.Stream(t.Context(), "counters", map[string]any{"pid": 1, "intervalSec": 1, "durationMs": 3000},
				func(method string, params json.RawMessage) error {
					var s sample
					if method != "counters.sample" || json.Unmarshal(params, &s) != nil {
						t.Errorf("notification %s %s", method, params)
					}

					got = append(got, s.ElapsedMs)

					return nil
				}, &res)
			if err != nil {
				t.Fatal(err)
			}

			if len(got) != 3 || got[0] != 1000 || got[1] != 2000 || got[2] != 3000 || res.Samples != 3 || res.EndReason != "duration" {
				t.Errorf("samples at %v, result %+v", got, res)
			}

			h.stderr.mu.Lock()
			n := len(h.stderr.buf)
			h.stderr.mu.Unlock()

			if n > tailBytes {
				t.Errorf("stderr tail holds %d bytes, want at most %d", n, tailBytes)
			}
		})
	}
}

func TestCallFailures(t *testing.T) {
	t.Parallel()

	ignore := func(string, json.RawMessage) error { return nil }

	tests := []struct {
		mode     string
		method   string
		onNotify NotifyFunc
		want     api.Code
		contains string
	}{
		{helpertest.ModeHuge, "processes", nil, api.CodeHelperFailed, "broke the protocol: read message: message too long"},
		{helpertest.ModeGarbage, "processes", nil, api.CodeHelperFailed, "broke the protocol: decode message"},
		{helpertest.ModeErrorPrefix + "NOT_DOTNET", "counters", ignore, api.CodeNotDotnet, "fake  [2Jerror"},
		{helpertest.ModeExitMid, "counters", ignore, api.CodeHelperFailed, "exited (exit status 1) before answering"},
		// No notification handler (Call): a notification breaks the protocol.
		{helpertest.ModeOK, "counters", nil, api.CodeHelperFailed, `unexpected message "counters.sample"`},
	}

	for _, tt := range tests {
		t.Run(tt.mode, func(t *testing.T) {
			t.Parallel()

			h := start(t, tt.mode)

			err := h.Stream(t.Context(), tt.method, map[string]any{"durationMs": 1000}, tt.onNotify, nil)

			e, ok := errors.AsType[*api.Error](err)
			if !ok || e.Code != tt.want || !strings.Contains(e.Message, tt.contains) {
				t.Fatalf("err = %v, want %s containing %q", err, tt.want, tt.contains)
			}
		})
	}
}

func TestNotifyErrorEndsTheCall(t *testing.T) {
	t.Parallel()

	h := start(t, helpertest.ModeOK)
	errStop := errors.New("stop here")

	err := h.Stream(t.Context(), "counters", map[string]any{"durationMs": 1000},
		func(string, json.RawMessage) error { return errStop }, nil)
	if !errors.Is(err, errStop) {
		t.Fatalf("err = %v, want %v", err, errStop)
	}
}

func TestCancelMidStreamClosesQuietly(t *testing.T) {
	t.Parallel()

	h := start(t, helpertest.ModeOK)
	ctx, cancel := context.WithCancel(t.Context())

	defer cancel()

	n := 0

	// durationMs 0: the fake streams, then waits for stdin to close.
	err := h.Stream(ctx, "counters", map[string]any{"durationMs": 0}, func(string, json.RawMessage) error {
		n++
		if n == 3 {
			cancel()
		}

		return nil
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	began := time.Now()

	if err := h.Close(); err != nil {
		t.Errorf("Close: %v (the fake exits when stdin closes)", err)
	}

	if d := time.Since(began); d >= WaitDelay {
		t.Errorf("Close took %v; the helper should have exited on stdin's end", d)
	}

	requireGone(t, h.PID())

	if err := h.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestCallTimeoutThenClose(t *testing.T) {
	t.Parallel()

	h := start(t, helpertest.ModeStall)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)

	defer cancel()

	if err := h.Call(ctx, "processes", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}

	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestCloseKillsAHelperIgnoringEOF(t *testing.T) {
	t.Parallel()

	h := start(t, helpertest.ModeIgnoreEOF)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)

	defer cancel()

	if err := h.Call(ctx, "processes", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}

	began := time.Now()

	if err := h.Close(); err == nil || !strings.Contains(err.Error(), "killed") {
		t.Errorf("Close = %v, want a kill", err)
	}

	if d := time.Since(began); d < WaitDelay || d > WaitDelay+closeSlack {
		t.Errorf("Close took %v, want about %v", d, WaitDelay)
	}

	requireGone(t, h.PID())
}

func TestStartContextEndsTheHelper(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	h, err := Start(ctx, fakeSpec(t, helpertest.ModeStall))
	if err != nil {
		t.Fatal(err)
	}

	cancel()

	// os/exec's Cancel closes stdin: the stalled fake exits by itself.
	if err := h.Call(context.Background(), "processes", nil, nil); !errors.Is(err, context.Canceled) && api.CodeOf(err) != api.CodeHelperFailed {
		t.Errorf("Call after the helper's ctx ended: %v", err)
	}

	if err := h.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	requireGone(t, h.PID())
}

func TestCloseInterruptsACall(t *testing.T) {
	t.Parallel()

	h := start(t, helpertest.ModeStall)
	done := make(chan error, 1)

	go func() { done <- h.Call(t.Context(), "processes", nil, nil) }()

	_ = h.Close()

	if err := <-done; !errors.Is(err, errClosed) {
		t.Errorf("Call = %v, want errClosed", err)
	}
}

func TestTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		writes []string
		want   string
	}{
		{"empty", nil, ""},
		{"lines", []string{"a\n", "b\r\n", "\n\n", "  c  "}, "a\nb\nc"},
		{"control", []string{"x\x1b[31my\x07z\tw\u0085v"}, "x [31my z w v"},
		{"invalid utf-8", []string{"a\xffb"}, "a�b"},
		{"last 20 lines", []string{strings.Repeat("l\n", 30) + "end"}, strings.Repeat("l\n", 19) + "end"},
		{"last 4 KiB", []string{"first\n" + strings.Repeat("y", tailBytes-4), "\nlast"}, strings.Repeat("y", tailBytes-5) + "\nlast"},
		{"one huge write", []string{"lost\n" + strings.Repeat("z", 2*tailBytes)}, strings.Repeat("z", tailBytes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tl := newTail()

			for _, w := range tt.writes {
				if n, err := tl.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}

			if len(tl.buf) > tailBytes || cap(tl.buf) != tailBytes {
				t.Errorf("buffer len %d cap %d, want at most %d, fixed", len(tl.buf), cap(tl.buf), tailBytes)
			}

			if got := tl.Sanitized(); got != tt.want {
				t.Errorf("Sanitized() = %q, want %q", got, tt.want)
			}
		})
	}
}
