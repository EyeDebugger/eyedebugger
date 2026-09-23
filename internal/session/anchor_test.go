// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

func TestResolveAnchor(t *testing.T) {
	t.Parallel()

	src := "using System;\n" +
		"var total = 0;\n" +
		"for (var i = 1; i <= 3; i++)\n" +
		"{\n" +
		"    total   +=  Price(i);\n" +
		"    Console.WriteLine(total);\n" +
		"}\n" +
		"Console.WriteLine(total);\n"

	tests := []struct {
		name     string
		content  string
		anchor   string
		want     int
		wantCode api.Code
		wantText string // in the message or hint
	}{
		{name: "exact", content: src, anchor: "var total = 0;", want: 2},
		{name: "substring", content: src, anchor: "i <= 3", want: 3},
		{name: "whitespace collapses", content: src, anchor: "total += Price(i)", want: 5},
		{name: "anchor whitespace collapses", content: src, anchor: "  total\t+=   Price(i);  ", want: 5},
		{name: "case-sensitive", content: src, anchor: "VAR TOTAL", wantCode: api.CodeAnchorNotFound},
		{name: "ambiguous", content: src, anchor: "Console.WriteLine(total);", wantCode: api.CodeAnchorAmbiguous, wantText: "6, 8"},
		{name: "not found suggests", content: src, anchor: "total += Cost(i)", wantCode: api.CodeAnchorNotFound, wantText: "5: total"},
		{name: "not found without suggestions", content: src, anchor: "nothing like it", wantCode: api.CodeAnchorNotFound, wantText: "case-sensitively"},
		{name: "CRLF", content: strings.ReplaceAll(src, "\n", "\r\n"), anchor: "Price(i);", want: 5},
		{name: "BOM", content: "\xef\xbb\xbfvar first = 1;\nvar second = 2;\n", anchor: "var first", want: 1},
		{name: "empty", content: src, anchor: "   ", wantCode: api.CodeInvalidRequest},
		{name: "newline", content: src, anchor: "var total\nfor", wantCode: api.CodeInvalidRequest},
		{name: "too big", content: strings.Repeat("x", maxSourceFileSize+1), anchor: "x", wantCode: api.CodeInvalidRequest, wantText: "too big"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "Program.cs")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := resolveAnchor(path, tt.anchor)
			if tt.wantCode != "" {
				e, ok := err.(*api.Error) //nolint:errorlint // resolveAnchor returns *api.Error unwrapped.
				if !ok || e.Code != tt.wantCode || !strings.Contains(e.Message+" "+e.Hint, tt.wantText) {
					t.Fatalf("resolveAnchor(%q) = %d, %v; want %s with %q", tt.anchor, got, err, tt.wantCode, tt.wantText)
				}

				return
			}

			if err != nil || got != tt.want {
				t.Fatalf("resolveAnchor(%q) = %d, %v; want %d", tt.anchor, got, err, tt.want)
			}
		})
	}
}

func TestClosestLines(t *testing.T) {
	t.Parallel()

	lines := []string{
		"total += Price(i);",
		"total -= Price(j);",
		"unrelated = 1;",
		"Price(i);",
		"total += Cost(i) + Price(i);",
	}

	if got := closestLines(lines, "total += Price(k);", 3); !slices.Equal(got, []int{1, 5, 2}) {
		t.Fatalf("closestLines = %v, want [1 5 2]", got)
	}

	if got := closestLines(lines, "nothing", 3); len(got) != 0 {
		t.Fatalf("closestLines(nothing) = %v, want none", got)
	}
}

func TestLCS(t *testing.T) {
	t.Parallel()

	tests := []struct {
		a, b string
		want int
	}{
		{"a b c", "a b c", 3},
		{"a b c", "c b a", 1},
		{"a x b y c", "a b c", 3},
		{"", "a", 0},
	}

	for _, tt := range tests {
		if got := lcs(strings.Fields(tt.a), strings.Fields(tt.b)); got != tt.want {
			t.Errorf("lcs(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
