// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"reflect"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestParseHit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in    string
		want  *hitCondition
		holds []int // counts 1..6 where it holds
	}{
		{in: "3", want: &hitCondition{"==", 3}, holds: []int{3}},
		{in: " >= 4 ", want: &hitCondition{">=", 4}, holds: []int{4, 5, 6}},
		{in: "%2", want: &hitCondition{"%", 2}, holds: []int{2, 4, 6}},
		{in: "0"},
		{in: "-1"},
		{in: ">3"},
		{in: "==3"},
		{in: "%0"},
		{in: "x"},
		{in: ""},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := parseHit(tt.in)
			if tt.want == nil {
				if api.CodeOf(err) != api.CodeInvalidRequest {
					t.Fatalf("parseHit(%q) = %+v, %v; want INVALID_REQUEST", tt.in, got, err)
				}

				return
			}

			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseHit(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
			}

			holds := holdsAt(got, 6)

			if !reflect.DeepEqual(holds, tt.holds) {
				t.Errorf("holds at %v, want %v", holds, tt.holds)
			}
		})
	}
}

// holdsAt returns the counts 1..n where h holds.
func holdsAt(h *hitCondition, n int) []int {
	var out []int

	for c := 1; c <= n; c++ {
		if h.holds(c) {
			out = append(out, c)
		}
	}

	return out
}

func TestParseLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      string
		want    []logPart
		wantErr string
	}{
		{in: "i={i} total={ total }", want: []logPart{{text: "i="}, {expr: "i"}, {text: " total="}, {expr: "total"}}},
		{in: "plain", want: []logPart{{text: "plain"}}},
		{in: "{{literal}} {x}", want: []logPart{{text: "{literal} "}, {expr: "x"}}},
		{in: "{a}{b}", want: []logPart{{expr: "a"}, {expr: "b"}}},
		{in: "a } b", wantErr: "has no {"},
		{in: "a {b", wantErr: "not closed"},
		{in: "a {} b", wantErr: "no expression"},
		{in: "  ", wantErr: "empty"},
		{in: strings.Repeat("{x}", 11), wantErr: "more than 10"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()

			got, err := parseLog(tt.in)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseLog(%q) = %+v, %v; want error %q", tt.in, got, err, tt.wantErr)
				}

				return
			}

			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseLog(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()

	logs := []logPart{{text: "hi"}}
	plain := func() *planRecord { return &planRecord{} }
	hitAt := func(op string, n, hits int) *planRecord { return &planRecord{hit: &hitCondition{op, n}, hits: hits} }
	logpoint := func(hit *hitCondition, hits int) *planRecord { return &planRecord{log: logs, hit: hit, hits: hits} }

	tests := []struct {
		name     string
		records  []*planRecord
		held     []bool
		wantStop bool
		wantHits int
		wantLogs int
	}{
		{name: "plain stops", records: []*planRecord{plain()}, held: []bool{true}, wantStop: true},
		{name: "plain whose condition failed", records: []*planRecord{plain()}, held: []bool{false}},
		{name: "hit N before", records: []*planRecord{hitAt("==", 3, 1)}, held: []bool{true}, wantHits: 1},
		{name: "hit N at", records: []*planRecord{hitAt("==", 3, 2)}, held: []bool{true}, wantStop: true, wantHits: 1},
		{name: "hit N after", records: []*planRecord{hitAt("==", 3, 3)}, held: []bool{true}, wantHits: 1},
		{name: ">=N", records: []*planRecord{hitAt(">=", 2, 5)}, held: []bool{true}, wantStop: true, wantHits: 1},
		{name: "%N misses", records: []*planRecord{hitAt("%", 2, 2)}, held: []bool{true}, wantHits: 1},
		{name: "%N", records: []*planRecord{hitAt("%", 2, 3)}, held: []bool{true}, wantStop: true, wantHits: 1},
		{name: "condition gates hits", records: []*planRecord{hitAt("==", 1, 0)}, held: []bool{false}},
		{name: "logpoint never stops", records: []*planRecord{logpoint(nil, 0)}, held: []bool{true}, wantHits: 1, wantLogs: 1},
		{name: "logpoint with a hit count", records: []*planRecord{logpoint(&hitCondition{"==", 2}, 0)}, held: []bool{true}, wantHits: 1},
		{
			name: "logpoint and another's plain", records: []*planRecord{logpoint(nil, 4), plain()}, held: []bool{true, true},
			wantStop: true, wantHits: 1, wantLogs: 1,
		},
		{
			name: "logpoint and another's false condition", records: []*planRecord{logpoint(nil, 0), plain()}, held: []bool{true, false},
			wantHits: 1, wantLogs: 1,
		},
		{name: "nothing", records: nil, held: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := decide(tt.records, tt.held)
			if d.stop != tt.wantStop || len(d.hit) != tt.wantHits || len(d.logs) != tt.wantLogs {
				t.Fatalf("decide = stop %v, %d hits, %d logs; want %v, %d, %d", d.stop, len(d.hit), len(d.logs), tt.wantStop, tt.wantHits, tt.wantLogs)
			}
		})
	}
}
