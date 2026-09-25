// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package artifacts

import (
	"strings"
	"testing"
)

func TestCheckOwner(t *testing.T) {
	t.Parallel()

	if err := checkOwner("/d", 501, 501); err != nil {
		t.Fatal(err)
	}

	err := checkOwner("/d", 0, 501)
	if err == nil || !strings.Contains(err.Error(), "owned by uid 0, not by you (uid 501)") {
		t.Fatalf("checkOwner = %v", err)
	}
}
