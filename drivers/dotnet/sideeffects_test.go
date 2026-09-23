// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import "testing"

func TestSideEffects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		expr string
		want string // "" = no finding
	}{
		{"total", ""},
		{"a.b.c", ""},
		{"list[0]", ""},
		{"(int)x", ""},
		{"x == 3", ""},
		{"x >= 3", ""},
		{"x<=3", ""},
		{"x != 3", ""},
		{"typeof(Foo)", ""},
		{"nameof(x)", ""},
		{`"a(b)"`, ""},
		{`"a\"(b)"`, ""},
		{"'('", ""},
		{`@"a(b)"`, ""},
		{"a => a", ""},
		{"c > (d)", ""},
		{"x is (1 or 2)", ""},
		{"$exception.Message", ""},
		{"x.ToString()", "call a method (ToString)"},
		{"Foo (1)", "call a method (Foo)"},
		{"Foo<int>(1)", "call a method (Foo)"},
		{"a<b>(c)", "call a method (a)"},
		{"getF()(1)", "call a method (getF)"},
		{"items[0](1)", "call a method"},
		{"new Foo()", reasonNew},
		{"x = 1", reasonAssign},
		{"x += 1", reasonAssign},
		{"x <<= 1", reasonAssign},
		{"x>>=1", reasonAssign},
		{"x ??= y", reasonAssign},
		{"i++", reasonIncrement},
		{"--i", reasonIncrement},
		{`$"{Foo()}"`, reasonInterpolated},
		{`$@"{x}"`, reasonInterpolated},
		{`@$"{x}"`, reasonInterpolated},
		{"Foo /**/ (1)", "call a method (Foo)"},
		{"Foo // a comment", ""},
		{"x /* ( */ + 1", ""},
		{"Fooé()", "call a method (Fooé)"},
		{"é()", "call a method (é)"},
		{"x.Méthode()", "call a method (Méthode)"},
		{"a.b.Größe()", "call a method (Größe)"},
		{"größe + 1", ""},
		{"(int)(x)", ""},
		{"(System.Int32)(x)", ""},
		{"((int)x)(1)", "call a method"},
		{"(a + b)(1)", "call a method"},
		{"(x)", ""},
		{"()(1)", "call a method"},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			t.Parallel()

			got, found := SideEffects(tt.expr)
			if found != (tt.want != "") || got != tt.want {
				t.Fatalf("SideEffects(%q) = %q, %v; want %q", tt.expr, got, found, tt.want)
			}
		})
	}
}
