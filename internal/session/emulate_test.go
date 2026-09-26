// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"reflect"
	"slices"
	"strconv"
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
		wantFor  []int // indexes of the records the stop is for
		wantHits int
		wantLogs int
	}{
		{name: "plain stops", records: []*planRecord{plain()}, held: []bool{true}, wantStop: true, wantFor: []int{0}},
		{name: "plain whose condition failed", records: []*planRecord{plain()}, held: []bool{false}},
		{name: "hit N before", records: []*planRecord{hitAt("==", 3, 1)}, held: []bool{true}, wantHits: 1},
		{name: "hit N at", records: []*planRecord{hitAt("==", 3, 2)}, held: []bool{true}, wantStop: true, wantFor: []int{0}, wantHits: 1},
		{name: "hit N after", records: []*planRecord{hitAt("==", 3, 3)}, held: []bool{true}, wantHits: 1},
		{name: ">=N", records: []*planRecord{hitAt(">=", 2, 5)}, held: []bool{true}, wantStop: true, wantFor: []int{0}, wantHits: 1},
		{name: "%N misses", records: []*planRecord{hitAt("%", 2, 2)}, held: []bool{true}, wantHits: 1},
		{name: "%N", records: []*planRecord{hitAt("%", 2, 3)}, held: []bool{true}, wantStop: true, wantFor: []int{0}, wantHits: 1},
		{name: "condition gates hits", records: []*planRecord{hitAt("==", 1, 0)}, held: []bool{false}},
		{name: "logpoint never stops", records: []*planRecord{logpoint(nil, 0)}, held: []bool{true}, wantHits: 1, wantLogs: 1},
		{name: "logpoint with a hit count", records: []*planRecord{logpoint(&hitCondition{"==", 2}, 0)}, held: []bool{true}, wantHits: 1},
		{
			name: "logpoint and another's plain", records: []*planRecord{logpoint(nil, 4), plain()}, held: []bool{true, true},
			wantStop: true, wantFor: []int{1}, wantHits: 1, wantLogs: 1,
		},
		{
			name: "logpoint and another's false condition", records: []*planRecord{logpoint(nil, 0), plain()}, held: []bool{true, false},
			wantHits: 1, wantLogs: 1,
		},
		{
			name: "every owner whose condition held", records: []*planRecord{plain(), plain(), plain()}, held: []bool{true, false, true},
			wantStop: true, wantFor: []int{0, 2},
		},
		{
			name: "plain and a hit count at its hit", records: []*planRecord{plain(), hitAt("==", 2, 1)}, held: []bool{true, true},
			wantStop: true, wantFor: []int{0, 1}, wantHits: 1,
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

			var gotFor []int

			for _, r := range d.stopFor {
				gotFor = append(gotFor, slices.Index(tt.records, r))
			}

			if !slices.Equal(gotFor, tt.wantFor) {
				t.Errorf("stop for records %v, want %v", gotFor, tt.wantFor)
			}
		})
	}
}

func TestTruthy(t *testing.T) {
	t.Parallel()

	holds := []string{"true", "True", " TRUE ", "1", "-1", "0x1", "2.5", "NaN", "'a'", "[0]", "{Order}", "", "yes", "18446744073709551616"}
	fails := []string{
		"false", "False", " FALSE\n", "0", "-0", "0x0", "0x0000000000000000", "0.0", "0e0", "None", "null", "nil",
		"''", `""`, "[]", "{}", "()", "set()",
	}

	for _, v := range holds {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false, want true", v)
		}
	}

	for _, v := range fails {
		if truthy(v) {
			t.Errorf("truthy(%q) = true, want false", v)
		}
	}
}

// planBP is a line breakpoint of file f for planning tests: requested at
// req, placed at line.
func planBP(id int, f string, req, line int, cond string) *breakpoint {
	return &breakpoint{Breakpoint: api.Breakpoint{ID: id, Owner: "c" + strconv.Itoa(id), File: f, RequestedLine: req, Line: line, Condition: cond}}
}

func TestPlanLocked(t *testing.T) {
	t.Parallel()

	const f, g = "/src/a.py", "/src/b.py"

	logpoint := planBP(9, f, 10, 10, "b")
	logpoint.log = []logPart{{text: "hi"}}

	tests := []struct {
		name string
		bps  map[string][]*breakpoint
		// want is each record's condition to evaluate, by breakpoint id.
		want     map[int]string
		wantEval []string
	}{
		{name: "none", bps: map[string][]*breakpoint{}, want: map[int]string{}},
		{
			name: "one conditional: the adapter checked it",
			bps:  map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a")}},
			want: map[int]string{1: ""},
		},
		{
			name: "uniform",
			bps:  map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), planBP(2, f, 10, 10, "a")}},
			want: map[int]string{1: "", 2: ""},
		},
		{
			name:     "unconditional and conditional: only the condition",
			bps:      map[string][]*breakpoint{f: {planBP(1, f, 10, 10, ""), planBP(2, f, 10, 10, "c")}},
			want:     map[int]string{1: "", 2: "c"},
			wantEval: []string{"c"},
		},
		{
			name:     "differing: both",
			bps:      map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), planBP(2, f, 10, 10, "b")}},
			want:     map[int]string{1: "a", 2: "b"},
			wantEval: []string{"a", "b"},
		},
		{
			name:     "two requested lines placed on one",
			bps:      map[string][]*breakpoint{f: {planBP(1, f, 8, 10, "a"), planBP(2, f, 10, 10, "b")}},
			want:     map[int]string{1: "a", 2: "b"},
			wantEval: []string{"a", "b"},
		},
		{
			name:     "same text twice evaluates once",
			bps:      map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), planBP(2, f, 10, 10, ""), planBP(3, f, 10, 10, "a")}},
			want:     map[int]string{1: "a", 2: "", 3: "a"},
			wantEval: []string{"a"},
		},
		{
			name:     "a logpoint's condition counts",
			bps:      map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), logpoint}},
			want:     map[int]string{1: "a", 9: "b"},
			wantEval: []string{"a", "b"},
		},
		{
			name: "uniform over the stop's line only",
			bps:  map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), planBP(2, f, 12, 12, "b")}},
			want: map[int]string{1: ""},
		},
		{
			name: "per file",
			bps:  map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a")}, g: {planBP(2, g, 10, 10, "b")}},
			want: map[int]string{1: "", 2: ""},
		},
		{
			name: "function breakpoints are not planned",
			bps:  map[string][]*breakpoint{funcKey: {{Breakpoint: api.Breakpoint{ID: 1, Function: "F", Line: 10, Condition: "a"}}}},
			want: map[int]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &Session{bps: tt.bps}
			records, _ := s.planLocked(api.Frame{Line: 10}, func(*breakpoint) bool { return true })

			got := map[int]string{}
			for _, r := range records {
				got[r.b.ID] = r.condition
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("conditions to evaluate = %v, want %v", got, tt.want)
			}

			if eval := toEvaluate(records); !slices.Equal(eval, tt.wantEval) {
				t.Errorf("evaluated = %q, want %q", eval, tt.wantEval)
			}
		})
	}
}

func TestNeedsFilterLocked(t *testing.T) {
	t.Parallel()

	const f = "/src/a.py"

	hit := planBP(3, f, 12, 12, "")
	hit.hit = &hitCondition{op: "==", n: 2}
	fn := func(id int, cond string) *breakpoint {
		return &breakpoint{Breakpoint: api.Breakpoint{ID: id, Function: "F", Condition: cond}}
	}

	tests := []struct {
		name string
		bps  map[string][]*breakpoint
		want bool
	}{
		{name: "none", bps: map[string][]*breakpoint{}},
		{name: "one conditional", bps: map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a")}}},
		{name: "two equal", bps: map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), planBP(2, f, 10, 10, "a")}}},
		{name: "different lines", bps: map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), planBP(2, f, 12, 12, "b")}}},
		{name: "unconditional and conditional", bps: map[string][]*breakpoint{f: {planBP(1, f, 10, 10, ""), planBP(2, f, 10, 10, "c")}}, want: true},
		{name: "differing", bps: map[string][]*breakpoint{f: {planBP(1, f, 10, 10, "a"), planBP(2, f, 10, 10, "b")}}, want: true},
		{name: "differing once placed", bps: map[string][]*breakpoint{f: {planBP(1, f, 8, 10, "a"), planBP(2, f, 10, 10, "b")}}, want: true},
		{name: "emulated", bps: map[string][]*breakpoint{f: {hit}}, want: true},
		{name: "function-only conflict", bps: map[string][]*breakpoint{funcKey: {fn(1, "a"), fn(2, "b")}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			s := &Session{bps: tt.bps}
			if got := s.needsFilterLocked(); got != tt.want {
				t.Errorf("needsFilterLocked = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAttributionSortsAndCopies(t *testing.T) {
	t.Parallel()

	records := []*planRecord{{b: planBP(7, "/f", 1, 1, "")}, {b: planBP(3, "/f", 1, 1, "")}}
	a, b := attribution(records), attribution(records)

	want := []api.StopBreakpoint{{ID: 3, Owner: "c3"}, {ID: 7, Owner: "c7"}}
	if !slices.Equal(a, want) {
		t.Fatalf("attribution = %+v, want %+v", a, want)
	}

	a[0].ID = 99
	if b[0].ID != 3 {
		t.Error("two attributions share a backing array")
	}
}
