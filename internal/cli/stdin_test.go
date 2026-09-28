// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"io"
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
