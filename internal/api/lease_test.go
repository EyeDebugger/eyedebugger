// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestParseLeasePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    LeasePolicy
		wantErr bool
	}{
		{in: "", want: LeaseFree},
		{in: "free", want: LeaseFree},
		{in: "handoff", want: LeaseHandoff},
		{in: "human-priority", want: LeaseHumanPriority},
		{in: "Free", wantErr: true},
		{in: "human_priority", wantErr: true},
		{in: "none", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := ParseLeasePolicy(tt.in)
			if tt.wantErr {
				if CodeOf(err) != CodeInvalidRequest {
					t.Errorf("ParseLeasePolicy(%q) = %q, %v; want INVALID_REQUEST", tt.in, got, err)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Errorf("ParseLeasePolicy(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
			}
		})
	}
}
