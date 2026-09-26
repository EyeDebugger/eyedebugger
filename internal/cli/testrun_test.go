// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"slices"
	"testing"
)

// TestTestCommandDashArgs drives the real 'eyedbg test' command (cobra's own
// ArgsLenAtDash, not just [testCommandArgs]) against the fake driver, which
// isn't a session.Tester: a parse error (too many arguments before "--")
// must be caught before any daemon call, and dash arguments must reach the
// driver layer (the fake driver's own "can't debug test runs" refusal)
// rather than being rejected as a CLI usage error.
func TestTestCommandDashArgs(t *testing.T) {
	serveInProcess(t, isolate(t))

	expectOutput(t, run(t, exitError, "test", "fake", "a", "b"),
		`unexpected argument "b" (put test app arguments after "--")`)
	expectOutput(t, run(t, exitError, "test", "fake", "Adds", "--", "-x"),
		"[INVALID_REQUEST]", "fake can't debug test runs")
}

// TestTestCommandArgs checks the <lang> [filter] [-- test app args...]
// split, mirroring startFlags.params' ArgsLenAtDash pattern.
func TestTestCommandArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		args       []string
		dash       int
		wantFilter string
		wantArgs   []string
		wantErr    string
	}{
		{
			name:       "filter and dash args",
			args:       []string{"dotnet", "Adds", "-x"},
			dash:       2,
			wantFilter: "Adds",
			wantArgs:   []string{"-x"},
		},
		{
			name:       "no filter, dash args",
			args:       []string{"dotnet", "-x"},
			dash:       1,
			wantFilter: "",
			wantArgs:   []string{"-x"},
		},
		{
			name:    "third argument before dash is an error",
			args:    []string{"dotnet", "a", "b"},
			dash:    -1,
			wantErr: `unexpected argument "b" (put test app arguments after "--")`,
		},
		{
			name:    "third argument before an explicit dash is an error",
			args:    []string{"dotnet", "Adds", "extra", "-x"},
			dash:    3,
			wantErr: `unexpected argument "extra" (put test app arguments after "--")`,
		},
		{
			name:       "lang only",
			args:       []string{"dotnet"},
			dash:       -1,
			wantFilter: "",
			wantArgs:   nil,
		},
		{
			name:       "lang and filter, no dash",
			args:       []string{"dotnet", "Adds"},
			dash:       -1,
			wantFilter: "Adds",
			wantArgs:   nil,
		},
		{
			name:       "lang, empty dash args",
			args:       []string{"dotnet"},
			dash:       1,
			wantFilter: "",
			wantArgs:   []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			filter, args, err := testCommandArgs(tt.args, tt.dash)

			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("testCommandArgs(%v, %d) error = %v, want %q", tt.args, tt.dash, err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("testCommandArgs(%v, %d) = %v", tt.args, tt.dash, err)
			}

			if filter != tt.wantFilter {
				t.Errorf("filter = %q, want %q", filter, tt.wantFilter)
			}

			if !slices.Equal(args, tt.wantArgs) {
				t.Errorf("args = %v, want %v", args, tt.wantArgs)
			}
		})
	}
}
