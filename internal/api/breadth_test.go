// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api_test

import (
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestParseExceptionMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    api.ExceptionMode
		wantErr bool
	}{
		{in: "all", want: api.ExceptionsAll},
		{in: "uncaught", want: api.ExceptionsUncaught},
		{in: "none", want: api.ExceptionsNone},
		{in: "", wantErr: true},
		{in: "All", wantErr: true},
		{in: "user-unhandled", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := api.ParseExceptionMode(tt.in)
			if tt.wantErr {
				if api.CodeOf(err) != api.CodeInvalidRequest {
					t.Fatalf("ParseExceptionMode(%q) err = %v, want INVALID_REQUEST", tt.in, err)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Fatalf("ParseExceptionMode(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}
