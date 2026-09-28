// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
)

// TestCancelOnEOF: EOF and a read error both cancel the returned context,
// the same way an interrupt does.
func TestCancelOnEOF(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		closeErr error // nil closes with plain EOF
	}{
		{name: "EOF"},
		{name: "read error", closeErr: errors.New("stdin broke")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r, w := io.Pipe()
			t.Cleanup(func() { _ = w.Close() })

			ctx, cancel := CancelOnEOF(t.Context(), r)
			t.Cleanup(cancel)

			if err := ctx.Err(); err != nil {
				t.Fatalf("ctx already done before EOF: %v", err)
			}

			if tt.closeErr != nil {
				_ = w.CloseWithError(tt.closeErr)
			} else {
				_ = w.Close()
			}

			<-ctx.Done()

			if !errors.Is(ctx.Err(), context.Canceled) {
				t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
			}
		})
	}
}

// TestCancelOnEOFParentCancel: the parent's own cancellation cancels the
// returned context too, with no EOF needed — stdin may never close.
func TestCancelOnEOFParentCancel(t *testing.T) {
	t.Parallel()

	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })

	parent, parentCancel := context.WithCancel(t.Context())

	ctx, cancel := CancelOnEOF(parent, r)
	t.Cleanup(cancel)

	parentCancel()
	<-ctx.Done()

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}

// TestCancelOnEOFDrains: a read that returns data, not EOF, does not
// cancel — the draining goroutine keeps reading instead.
func TestCancelOnEOFDrains(t *testing.T) {
	t.Parallel()

	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })

	ctx, cancel := CancelOnEOF(t.Context(), r)
	t.Cleanup(cancel)

	written := make(chan struct{})

	go func() {
		_, _ = w.Write([]byte("not EOF yet"))
		close(written)
	}()

	<-written

	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx done after a plain read: %v", err)
	}

	_ = w.Close()
	<-ctx.Done()
}

// TestWrapCancelOnEOFClearsEnv: WrapCancelOnEOF always clears the env var it
// read, enabled or not, so an autostarted daemon, its adapters and the
// debuggee never inherit it (F5) — they all build their own environment
// from this process's.
func TestWrapCancelOnEOFClearsEnv(t *testing.T) {
	// Not t.Parallel(): sets an env var (t.Setenv forbids it anyway).
	for _, value := range []string{"1", "0", ""} {
		t.Setenv(EnvCancelOnStdinEOF, value)

		root := NewEyedbgCommand(testInfo)

		_, cancel := WrapCancelOnEOF(t.Context(), root, []string{"status"})
		cancel()

		if v, ok := os.LookupEnv(EnvCancelOnStdinEOF); ok {
			t.Errorf("value %q: env var still set to %q after WrapCancelOnEOF", value, v)
		}
	}
}

// TestWrapCancelOnEOFDisabled: unset (or not "1"), WrapCancelOnEOF returns
// ctx unchanged — no goroutine reads root's stdin at all.
func TestWrapCancelOnEOFDisabled(t *testing.T) {
	// Not t.Parallel(): sets an env var.
	t.Setenv(EnvCancelOnStdinEOF, "0")

	root := NewEyedbgCommand(testInfo)

	parent := t.Context()

	got, cancel := WrapCancelOnEOF(parent, root, []string{"status"})
	defer cancel()

	if got != parent {
		t.Error("WrapCancelOnEOF wrapped ctx while disabled")
	}
}

// TestWrapCancelOnEOFAppliesToOtherCommands: enabled, for any command but
// 'dap', stdin EOF cancels the returned context, same as CancelOnEOF alone.
func TestWrapCancelOnEOFAppliesToOtherCommands(t *testing.T) {
	// Not t.Parallel(): sets an env var.
	t.Setenv(EnvCancelOnStdinEOF, "1")

	root := NewEyedbgCommand(testInfo)

	r, w := io.Pipe()
	t.Cleanup(func() { _ = w.Close() })
	root.SetIn(r)

	ctx, cancel := WrapCancelOnEOF(t.Context(), root, []string{"status"})
	t.Cleanup(cancel)

	if err := ctx.Err(); err != nil {
		t.Fatalf("ctx already done: %v", err)
	}

	_ = w.Close()
	<-ctx.Done()

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
}
