// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package present

import (
	"reflect"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func tree() []api.Var {
	return []api.Var{
		{Name: "a", Type: "int", Value: "1"},
		{Name: "list", Type: "List<int>", Value: "Count = 3", HasChildren: true, More: 5, Children: []api.Var{
			{Name: "[0]", Type: "int", Value: "7"},
			{Name: "[1]", Type: "int", Value: "8"},
			{Name: "[2]", Type: "int", Value: "9"},
		}},
		{Name: "b", Type: "string", Value: `"hello"`},
	}
}

func names(vars []api.Var) []string {
	out := []string{}
	for _, v := range vars {
		out = append(out, v.Name)
	}

	return out
}

func TestFit(t *testing.T) {
	t.Parallel()

	topCost := cost(tree()[0], 0) + cost(tree()[1], 0) + cost(tree()[2], 0)
	childCost := cost(tree()[1].Children[0], 1)

	tests := []struct {
		name          string
		chars         int // budget in characters; -1 for none
		wantTop       []string
		wantOmitted   int
		wantChildren  []string
		wantMore      int
		wantTruncated bool
	}{
		{"no budget", -1, []string{"a", "list", "b"}, 0, []string{"[0]", "[1]", "[2]"}, 5, false},
		{"everything fits", topCost + 3*childCost, []string{"a", "list", "b"}, 0, []string{"[0]", "[1]", "[2]"}, 5, false},
		{"members cut", topCost + childCost, []string{"a", "list", "b"}, 0, []string{"[0]"}, 7, true},
		{"members collapsed", topCost, []string{"a", "list", "b"}, 0, []string{}, 0, true},
		{"top level cut", cost(tree()[0], 0), []string{"a"}, 2, []string{}, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := &Fitter{left: tt.chars}
			kept, omitted, truncated := f.Fit(tree())

			if got := names(kept); !reflect.DeepEqual(got, tt.wantTop) {
				t.Errorf("top = %v, want %v", got, tt.wantTop)
			}

			if omitted != tt.wantOmitted || truncated != tt.wantTruncated {
				t.Errorf("omitted, truncated = %d, %v; want %d, %v", omitted, truncated, tt.wantOmitted, tt.wantTruncated)
			}

			if len(kept) < 2 {
				return
			}

			if got := names(kept[1].Children); !reflect.DeepEqual(got, tt.wantChildren) || kept[1].More != tt.wantMore {
				t.Errorf("list children = %v more %d, want %v more %d", got, kept[1].More, tt.wantChildren, tt.wantMore)
			}

			if !kept[1].HasChildren {
				t.Error("a cut variable must keep HasChildren")
			}
		})
	}
}

func TestFitDoesNotModifyInput(t *testing.T) {
	t.Parallel()

	in := tree()
	NewFitter(1).Fit(in)

	if !reflect.DeepEqual(in, tree()) {
		t.Error("Fit modified its input")
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()

	prev := []api.Var{
		{Name: "i", Type: "int", Value: "1"},
		{Name: "total", Type: "int", Value: "1"},
		{Name: "gone", Type: "int", Value: "0"},
	}
	cur := []api.Var{
		{Name: "i", Type: "int", Value: "2"},
		{Name: "total", Type: "int", Value: "1"},
		{Name: "fresh", Type: "Order", Value: "{Order}", HasChildren: true, Children: []api.Var{{Name: "Id"}}},
	}

	want := []api.Var{
		{Name: "i", Type: "int", Value: "2", Change: ChangeChanged, Previous: "1"},
		{Name: "fresh", Type: "Order", Value: "{Order}", HasChildren: true, Change: ChangeNew},
	}

	if got := Diff(prev, cur); !reflect.DeepEqual(got, want) {
		t.Errorf("Diff = %+v\nwant %+v", got, want)
	}

	if got := Diff(cur, cur); len(got) != 0 {
		t.Errorf("Diff of equal lists = %+v, want none", got)
	}
}
