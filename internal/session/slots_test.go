// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"reflect"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestSlotsFor(t *testing.T) {
	t.Parallel()

	bp := func(id, line int, owner, cond string) *breakpoint {
		return &breakpoint{Breakpoint: api.Breakpoint{ID: id, RequestedLine: line, Owner: owner, Condition: cond}}
	}

	type want struct {
		line int
		cond string
		note bool
		ids  []int
	}

	tests := []struct {
		name string
		list []*breakpoint
		want []want
	}{
		{"none", nil, nil},
		{
			"one per line keeps order",
			[]*breakpoint{bp(1, 9, "agent", ""), bp(2, 3, "agent", "i > 1")},
			[]want{{9, "", false, []int{1}}, {3, "i > 1", false, []int{2}}},
		},
		{
			"same line owners share",
			[]*breakpoint{bp(1, 5, "agent", ""), bp(2, 5, "human:x", "")},
			[]want{{5, "", false, []int{1, 2}}},
		},
		{
			"unconditional wins",
			[]*breakpoint{bp(1, 5, "agent", "false"), bp(2, 5, "human:x", "")},
			[]want{{5, "", false, []int{1, 2}}},
		},
		{
			"unconditional first wins",
			[]*breakpoint{bp(1, 5, "agent", ""), bp(2, 5, "human:x", "false")},
			[]want{{5, "", false, []int{1, 2}}},
		},
		{
			"equal conditions kept",
			[]*breakpoint{bp(1, 5, "agent", "i == 3"), bp(2, 5, "human:x", "i == 3")},
			[]want{{5, "i == 3", false, []int{1, 2}}},
		},
		{
			"conflict is noted",
			[]*breakpoint{bp(1, 5, "agent", "i == 3"), bp(2, 7, "agent", ""), bp(3, 5, "human:x", "i == 4")},
			[]want{{5, "", true, []int{1, 3}}, {7, "", false, []int{2}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []want

			for _, s := range slotsFor(tt.list) {
				w := want{line: s.line, cond: s.condition, note: s.note}
				for _, b := range s.bps {
					w.ids = append(w.ids, b.ID)
				}

				got = append(got, w)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("slotsFor = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestSlotsForFunctions: function breakpoints group per name, apart from
// line breakpoints.
func TestSlotsForFunctions(t *testing.T) {
	t.Parallel()

	list := []*breakpoint{
		{Breakpoint: api.Breakpoint{ID: 1, Function: "Price", Owner: "agent"}},
		{Breakpoint: api.Breakpoint{ID: 2, RequestedLine: 5, Owner: "agent"}},
		{Breakpoint: api.Breakpoint{ID: 3, Function: "Price", Owner: "human:x", Condition: "i > 1"}},
		{Breakpoint: api.Breakpoint{ID: 4, Function: "Total", Owner: "agent"}},
	}

	type want struct {
		key  slotKey
		cond string
		ids  []int
	}

	var got []want

	for _, s := range slotsFor(list) {
		w := want{key: s.slotKey, cond: s.condition}
		for _, b := range s.bps {
			w.ids = append(w.ids, b.ID)
		}

		got = append(got, w)
	}

	expected := []want{
		{key: slotKey{function: "Price"}, ids: []int{1, 3}},
		{key: slotKey{line: 5}, ids: []int{2}},
		{key: slotKey{function: "Total"}, ids: []int{4}},
	}

	if !reflect.DeepEqual(got, expected) {
		t.Errorf("slotsFor = %+v, want %+v", got, expected)
	}
}
