// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

// sharedLineNote is set on breakpoints whose line holds other clients'
// breakpoints with a different condition.
const sharedLineNote = "shares its line with another client's breakpoint that has a different condition: it stops there unconditionally"

// slot is one line of a file as sent to the adapter: every breakpoint
// requested at that line, merged into one DAP source breakpoint. Adapters
// keep one breakpoint per line (netcoredbg does), so a file's request never
// names a line twice.
type slot struct {
	line      int
	condition string
	// note means the breakpoints' conditions conflict, so the slot stops
	// unconditionally.
	note bool
	bps  []*breakpoint
}

// slotsFor groups a file's breakpoints by requested line, in order of first
// appearance. A slot's condition is empty if any of its breakpoints has
// none, the shared condition if all are equal, and otherwise empty with
// note set.
func slotsFor(list []*breakpoint) []slot {
	var slots []slot

	index := make(map[int]int, len(list))

	for _, b := range list {
		i, ok := index[b.RequestedLine]
		if !ok {
			i = len(slots)
			index[b.RequestedLine] = i
			slots = append(slots, slot{line: b.RequestedLine, condition: b.Condition})
		}

		slots[i].bps = append(slots[i].bps, b)
	}

	for i := range slots {
		s := &slots[i]

		unconditional, differ := false, false

		for _, b := range s.bps {
			unconditional = unconditional || b.Condition == ""
			differ = differ || b.Condition != s.condition
		}

		switch {
		case unconditional:
			s.condition = ""
		case differ:
			s.condition, s.note = "", true
		}
	}

	return slots
}
