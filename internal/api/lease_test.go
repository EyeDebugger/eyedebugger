// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

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

// TestPresenceFieldsJSON checks that the fields added for presence and
// lease requests are left out when empty and survive a round trip.
func TestPresenceFieldsJSON(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		value any
		want  string
		fresh func() any
	}{
		{
			name:  "empty lease",
			value: &LeaseInfo{Policy: LeaseFree},
			want:  `{"policy":"free"}`,
			fresh: func() any { return &LeaseInfo{} },
		},
		{
			name: "lease with requests",
			value: &LeaseInfo{Policy: LeaseHandoff, Holder: "agent", Requests: []LeaseRequest{
				{Client: "human:t", Message: "let me step", At: at},
				{Client: "agent:b", At: at},
			}},
			want: `{"policy":"handoff","holder":"agent","requests":[` +
				`{"client":"human:t","message":"let me step","at":"2026-09-24T12:00:00Z"},` +
				`{"client":"agent:b","at":"2026-09-24T12:00:00Z"}]}`,
			fresh: func() any { return &LeaseInfo{} },
		},
		{
			name:  "client not connected",
			value: &ClientInfo{Client: Client{ID: "agent", Kind: KindAgent}, FirstSeen: at, LastSeen: at},
			want:  `{"id":"agent","kind":"agent","firstSeen":"2026-09-24T12:00:00Z","lastSeen":"2026-09-24T12:00:00Z"}`,
			fresh: func() any { return &ClientInfo{} },
		},
		{
			name: "client connected",
			value: &ClientInfo{
				Client: Client{ID: "human:t", Kind: KindHuman, Name: "t"}, FirstSeen: at, LastSeen: at, Connected: 2,
			},
			want: `{"id":"human:t","kind":"human","name":"t","firstSeen":"2026-09-24T12:00:00Z",` +
				`"lastSeen":"2026-09-24T12:00:00Z","connected":2}`,
			fresh: func() any { return &ClientInfo{} },
		},
		{
			name:  "request params",
			value: &LeaseRequestParams{SessionRef: SessionRef{SessionID: "s1"}, Message: "why"},
			want:  `{"sessionId":"s1","message":"why"}`,
			fresh: func() any { return &LeaseRequestParams{} },
		},
		{
			name:  "request params without message",
			value: &LeaseRequestParams{SessionRef: SessionRef{SessionID: "s1"}},
			want:  `{"sessionId":"s1"}`,
			fresh: func() any { return &LeaseRequestParams{} },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			b, err := json.Marshal(tt.value)
			if err != nil || string(b) != tt.want {
				t.Fatalf("Marshal = %s, %v; want %s", b, err, tt.want)
			}

			got := tt.fresh()
			if err := json.Unmarshal(b, got); err != nil || !reflect.DeepEqual(got, tt.value) {
				t.Errorf("round trip = %+v, %v; want %+v", got, err, tt.value)
			}
		})
	}
}
