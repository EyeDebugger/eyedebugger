// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package session

import (
	"slices"
	"testing"

	godap "github.com/google/go-dap"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/dap/daptest"
)

func TestLocalScopes(t *testing.T) {
	t.Parallel()

	locals := godap.Scope{Name: "Locals", PresentationHint: "locals"}
	globals := godap.Scope{Name: "Globals"}
	plain := godap.Scope{Name: "Locals"}
	regs := godap.Scope{Name: "Registers", PresentationHint: "registers"}
	costly := godap.Scope{Name: "Statics", Expensive: true}

	tests := []struct {
		name   string
		scopes []godap.Scope
		want   []string
	}{
		{"marked locals only", []godap.Scope{locals, globals}, []string{"Locals"}},
		{"none marked: every cheap one", []godap.Scope{plain, regs, costly}, []string{"Locals", "Registers"}},
		{"one unmarked scope (netcoredbg)", []godap.Scope{plain}, []string{"Locals"}},
		{"none", nil, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got []string
			for _, sc := range localScopes(tt.scopes) {
				got = append(got, sc.Name)
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("localScopes = %v, want %v", got, tt.want)
			}
		})
	}
}

// varNames returns the names of vars.
func varNames(vars []api.Var) []string {
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		names = append(names, v.Name)
	}

	return names
}

// TestStopCaptureReadsLocalsScope: with an adapter that also returns a
// Globals scope, snapshots and --changed read only the scope marked
// "locals", while vars and --expand still see every scope.
func TestStopCaptureReadsLocalsScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		globals    bool
		wantScopes []string
	}{
		{"globals scope", true, []string{"Locals", "Globals"}},
		{"default", false, []string{"Locals"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m := newTestManagerWith(t, nil, fakeDriver{opts: daptest.Options{GlobalsScope: tt.globals}})
			s := start(t, m, agentC, api.StartParams{LaunchSpec: api.LaunchSpec{StopOnEntry: true}})

			checkCaptures(t, s, []string{"line", "x"})
			scopes := checkScopes(t, s, tt.wantScopes)

			if tt.globals {
				checkGlobals(t, s, scopes[1])
			}
		})
	}
}

// checkCaptures checks the snapshot's locals and changes, and Changes.
func checkCaptures(t *testing.T, s *Session, want []string) {
	t.Helper()

	snap := s.Snapshot(t.Context(), api.DumpSpec{Dump: []string{api.DumpLocals, api.DumpChanged}})
	if snap.Locals == nil || !slices.Equal(varNames(snap.Locals.Vars), want) {
		t.Fatalf("snapshot locals = %+v, want %v", snap.Locals, want)
	}

	if snap.Changes == nil || !slices.Equal(varNames(snap.Changes.Vars), want) {
		t.Fatalf("snapshot changes = %+v, want %v", snap.Changes, want)
	}

	changed, err := s.Changes(t.Context(), 0)
	if err != nil || len(changed) != 1 || !slices.Equal(varNames(changed[0].Vars), want) {
		t.Fatalf("Changes = %+v, %v; want %v", changed, err, want)
	}
}

// checkScopes checks the names of the scopes vars shows and returns them.
func checkScopes(t *testing.T, s *Session, want []string) []api.Scope {
	t.Helper()

	scopes, err := s.Vars(t.Context(), 0, 1, 0)
	if err != nil {
		t.Fatalf("vars: %v", err)
	}

	names := make([]string, 0, len(scopes))
	for _, sc := range scopes {
		names = append(names, sc.Name)
	}

	if !slices.Equal(names, want) {
		t.Fatalf("vars scopes = %v, want %v", names, want)
	}

	return scopes
}

// checkGlobals checks that vars and --expand still see the Globals scope.
func checkGlobals(t *testing.T, s *Session, globals api.Scope) {
	t.Helper()

	if got := varNames(globals.Vars); !slices.Equal(got, []string{"g"}) {
		t.Fatalf("Globals vars = %v, want [g]", got)
	}

	exp, err := s.Expand(t.Context(), 0, "g", 1, 0)
	if err != nil || len(exp) != 1 || len(exp[0].Vars) != 1 || exp[0].Vars[0].Value != "1" {
		t.Fatalf("expand g = %+v, %v; want g = 1", exp, err)
	}
}
