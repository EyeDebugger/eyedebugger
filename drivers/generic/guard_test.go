// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package generic

import "testing"

// TestGuardPython runs the bundled python manifest's eval guard.
func TestGuardPython(t *testing.T) {
	t.Parallel()

	check := sideEffects(bundled().Language("python").EvalGuard)

	tests := []struct {
		expr   string
		reason string // "" = allowed
	}{
		{"total", ""},
		{"a.b[0]", ""},
		{"x == 3", ""},
		{"x <= 3", ""},
		{"x != y", ""},
		{"x ==-1", ""},
		{"not (x)", ""},
		{"a if (b) else c", ""},
		{"[i for (i) in xs]", ""},
		{"x in (1, 2)", ""},
		{"len(items)", ""},
		{"len (items)", ""},
		{"isinstance(x, int)", ""},
		{"str(x)", ""},
		{`r"a(b)"`, ""},
		{`"a(b)"`, ""},
		{`'it''s'`, ""},
		{`"a\"(b)"`, ""},
		{"b'x'", ""},
		{"-x", ""},
		{"x ** 2", ""},
		{"x[1:2]", ""},
		{`{"a": 1}`, ""},
		{"lambda: 1", ""},
		{"__exception__", ""},
		{"1e5 + 0x1F", ""},
		{"'''a'b''' + len(x)", ""},
		{`"""x"y"""`, ""},
		{`"" + x`, ""},
		{`"unterminated(`, "use an unterminated string (the rest can't be checked)"},
		{"'''a'b''' + f()", "call a function (f)"},
		{`"""x"y""" + items.append(1)`, "call a function (append)"},
		{`"""open`, "use an unterminated string (the rest can't be checked)"},
		{`r"x`, "use an unterminated string (the rest can't be checked)"},
		{"obj.len(x)", "call a function (len)"},
		{"items.append(1)", "call a function (append)"},
		{"f()", "call a function (f)"},
		{"f ()", "call a function (f)"},
		{"x.y(1)", "call a function (y)"},
		{"a[0](1)", "call a function"},
		{"(f)(1)", "call a function"},
		{"print(x)", "call a function (print)"},
		{"await f()", "call a function (f)"},
		{"größe()", "call a function (größe)"},
		{"(y := 5)", "assign (:=)"},
		{"x = 1", "assign (=)"},
		{"x=-1", "assign (=)"},
		{`f"{x}"`, "use an interpolated string (it can run code)"},
		{`rf'{x}'`, "use an interpolated string (it can run code)"},
		{`F"x"`, "use an interpolated string (it can run code)"},
		{`t"{x}"`, "use an interpolated string (it can run code)"},
		{`len(f"{x}")`, "use an interpolated string (it can run code)"},
	}

	for _, tt := range tests {
		reason, found := check(tt.expr)
		if found != (tt.reason != "") || reason != tt.reason {
			t.Errorf("check(%s) = %q, %v; want %q", tt.expr, reason, found, tt.reason)
		}
	}
}

func TestGuardNil(t *testing.T) {
	t.Parallel()

	if sideEffects(nil) != nil {
		t.Fatal("no evalGuard must mean no check")
	}
}
