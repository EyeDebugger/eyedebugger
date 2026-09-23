// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"maps"
	"testing"
)

func TestAttachArguments(t *testing.T) {
	t.Parallel()

	want := map[string]any{"name": "eyedbg", "type": "coreclr", "request": "attach", "processId": 4242, "justMyCode": true}
	if got := attachArguments(4242); !maps.Equal(got, want) {
		t.Fatalf("attachArguments = %v, want %v", got, want)
	}
}
