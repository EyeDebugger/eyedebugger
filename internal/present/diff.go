// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package present

import "github.com/eyedebugger/eyedebugger/internal/api"

// Change kinds set on [api.Var.Change].
const (
	ChangeNew     = "new"
	ChangeChanged = "changed"
)

// Diff returns the variables of cur that are new or have another value or
// type than in prev, compared by name at the top level only (an object's
// value is often just its type name, so member changes are not seen). The
// result has no children.
func Diff(prev, cur []api.Var) []api.Var {
	before := make(map[string]api.Var, len(prev))
	for _, v := range prev {
		before[v.Name] = v
	}

	out := []api.Var{}

	for _, v := range cur {
		v.Children, v.More = nil, 0

		old, ok := before[v.Name]

		switch {
		case !ok:
			v.Change = ChangeNew
		case old.Value != v.Value || old.Type != v.Type:
			v.Change, v.Previous = ChangeChanged, old.Value
		default:
			continue
		}

		out = append(out, v)
	}

	return out
}
