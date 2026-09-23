// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

func TestParseClient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    Client
		wantErr bool
	}{
		{in: "", want: Client{ID: "agent", Kind: KindAgent}},
		{in: "agent", want: Client{ID: "agent", Kind: KindAgent}},
		{in: "human", want: Client{ID: "human", Kind: KindHuman}},
		{in: "agent:claude-1", want: Client{ID: "agent:claude-1", Kind: KindAgent, Name: "claude-1"}},
		{in: "human:ijat@host", want: Client{ID: "human:ijat@host", Kind: KindHuman, Name: "ijat@host"}},
		{in: "human:a.b_c+d", want: Client{ID: "human:a.b_c+d", Kind: KindHuman, Name: "a.b_c+d"}},
		{in: "agent:" + strings.Repeat("x", 64), want: Client{ID: "agent:" + strings.Repeat("x", 64), Kind: KindAgent, Name: strings.Repeat("x", 64)}},
		{in: "Agent", wantErr: true},
		{in: "robot", wantErr: true},
		{in: "agent:", wantErr: true},
		{in: "agent:a:b", wantErr: true},
		{in: "agent:" + strings.Repeat("x", 65), wantErr: true},
		{in: "agent:a b", wantErr: true},
		{in: ":x", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := ParseClient(tt.in)
			if tt.wantErr {
				if CodeOf(err) != CodeInvalidRequest {
					t.Errorf("ParseClient(%q) = %+v, %v; want INVALID_REQUEST", tt.in, got, err)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Errorf("ParseClient(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
			}

			if got.IsHuman() != (tt.want.Kind == KindHuman) {
				t.Errorf("IsHuman() = %v for %+v", got.IsHuman(), got)
			}
		})
	}
}
