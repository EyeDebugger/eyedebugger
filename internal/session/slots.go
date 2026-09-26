// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

// sharedFunctionNote is set on function breakpoints whose function holds
// other clients' breakpoints with a different condition. Line breakpoints
// need none: the stop filter checks each one's condition (emulate.go).
const sharedFunctionNote = "shares its function with another client's breakpoint that has a different condition: " +
	"it stops there unconditionally"

// slotKey is where a breakpoint is requested: a function, or a line (of the
// file whose list it is in).
type slotKey struct {
	function string
	line     int
}

// key returns b's slot key.
func (b *breakpoint) key() slotKey { return slotKey{function: b.Function, line: b.RequestedLine} }

// slot is one line of a file (or one function) as sent to the adapter:
// every breakpoint requested there, merged into one DAP breakpoint.
// Adapters keep one breakpoint per line or function (netcoredbg does), so a
// request never names one twice.
type slot struct {
	slotKey

	condition string
	// note means the breakpoints' conditions conflict, so the slot stops
	// unconditionally (at a line, the stop filter then checks each one).
	note bool
	bps  []*breakpoint
}

// slotsFor groups a file's (or the function list's) breakpoints by slot
// key, in order of first appearance. A slot's condition is empty if any of
// its breakpoints has none, the shared condition if all are equal, and
// otherwise empty with note set.
func slotsFor(list []*breakpoint) []slot {
	var slots []slot

	index := make(map[slotKey]int, len(list))

	for _, b := range list {
		i, ok := index[b.key()]
		if !ok {
			i = len(slots)
			index[b.key()] = i
			slots = append(slots, slot{slotKey: b.key(), condition: b.Condition})
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
