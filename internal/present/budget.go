// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package present

import "github.com/eyedebugger/eyedebugger/internal/api"

// CharsPerToken is the estimate used to turn a token budget into text size.
const CharsPerToken = 4

// Fitter cuts variable trees to a token budget shared by all its Fit calls,
// so several scopes together stay within one budget.
type Fitter struct {
	left int // characters left; negative means no budget
}

// NewFitter returns a fitter for tokens; 0 or less means no budget.
func NewFitter(tokens int) *Fitter {
	if tokens <= 0 {
		return &Fitter{left: -1}
	}

	return &Fitter{left: tokens * CharsPerToken}
}

// level is one list of variables waiting to be fitted.
type level struct {
	src    []api.Var
	dst    *[]api.Var
	more   *int
	extra  int // members the adapter side already left out
	indent int
}

// Fit returns vars cut to what is left of the budget. It works breadth-first:
// every variable of a level is kept before any member of it is expanded, so
// a budget cut hides detail rather than whole variables. A variable whose
// members were cut keeps HasChildren and has no Children (shown as {…}).
// omitted counts top-level variables left out; truncated reports any cut.
func (f *Fitter) Fit(vars []api.Var) (kept []api.Var, omitted int, truncated bool) {
	if f.left < 0 {
		return vars, 0, false
	}

	queue := []level{{src: vars, dst: &kept, more: &omitted}}

	for len(queue) > 0 && !truncated {
		lv := queue[0]
		queue = queue[1:]

		// Preallocated so the pointers taken below stay valid.
		*lv.dst = make([]api.Var, 0, len(lv.src))

		for i, v := range lv.src {
			c := cost(v, lv.indent)
			if c > f.left {
				truncated = true

				if i == 0 && lv.indent > 0 {
					*lv.dst = nil // nothing fitted: leave the parent collapsed
				} else {
					*lv.more += len(lv.src) - i + lv.extra
				}

				break
			}

			f.left -= c

			nv := v
			nv.Children, nv.More = nil, 0
			*lv.dst = append(*lv.dst, nv)

			if len(v.Children) > 0 {
				last := &(*lv.dst)[len(*lv.dst)-1]
				queue = append(queue, level{src: v.Children, dst: &last.Children, more: &last.More, extra: v.More, indent: lv.indent + 1})
			}
		}

		if !truncated {
			*lv.more += lv.extra
		}
	}

	return kept, omitted, truncated
}

// cost estimates the characters one variable takes as a line of text.
func cost(v api.Var, indent int) int {
	return 2*indent + len(v.Name) + len(v.Type) + len(v.Value) + len(v.Previous) + 8
}
