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
