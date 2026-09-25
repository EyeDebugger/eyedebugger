// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"testing"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/api"
)

// TestStartAdapterFromEnv checks --adapter wins over EYEDBG_DOTNET_ADAPTER,
// which only dotnet sessions read.
func TestStartAdapterFromEnv(t *testing.T) {
	t.Setenv(envDotnetAdapter, "sharpdbg")

	tests := []struct {
		name, lang, flag, want string
	}{
		{"env", dotnet.Language, "", "sharpdbg"},
		{"flag wins", dotnet.Language, "netcoredbg", "netcoredbg"},
		{"other language", "python", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sf := &sessionFlags{adapter: tt.flag, leasePolicy: "handoff", dump: &dumpFlag{}}
			p := &api.StartParams{Lang: tt.lang}

			if err := sf.params(&globals{}, p); err != nil {
				t.Fatal(err)
			}

			if p.Adapter != tt.want {
				t.Errorf("Adapter = %q, want %q", p.Adapter, tt.want)
			}
		})
	}
}
