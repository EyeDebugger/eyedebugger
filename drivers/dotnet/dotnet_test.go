// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/session"
)

func TestWithoutResultJSON(t *testing.T) {
	t.Parallel()

	out := "{\n  \"Properties\": {\n    \"TargetPath\": \"/x.dll\"\n  }\n}\n/src/Program.cs(1,9): error CS0029: bad\n"
	if got, want := withoutResultJSON(out), "/src/Program.cs(1,9): error CS0029: bad\n"; got != want {
		t.Errorf("withoutResultJSON = %q, want %q", got, want)
	}

	if got := withoutResultJSON("error: no json\n"); got != "error: no json\n" {
		t.Errorf("non-JSON output changed: %q", got)
	}
}

func TestTail(t *testing.T) {
	t.Parallel()

	if got := tail("a\n\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("tail = %q, want %q", got, "b\nc")
	}
}

func TestPrepareRejectsOptions(t *testing.T) {
	t.Parallel()

	// Options are refused before netcoredbg or the dotnet host is looked
	// for, so this holds on a machine without either.
	_, err := New().Prepare(t.Context(), session.LaunchSpec{Program: "/nonexistent/app.dll", Options: map[string]string{"x": "1"}})
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "--opt") {
		t.Fatalf("Prepare with options: err = %v, want INVALID_REQUEST about --opt", err)
	}
}
