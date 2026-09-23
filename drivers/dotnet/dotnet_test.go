// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import "testing"

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
