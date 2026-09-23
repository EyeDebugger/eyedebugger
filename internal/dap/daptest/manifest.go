// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package daptest

// UserManifest returns a user manifest describing this package's fake DAP
// adapter as the language "fakelang", served by entry (normally
// os.Executable(), a test binary whose TestMain calls [MaybeRun]). attached,
// when not "", is the program an attach template attaches to; "" omits the
// attach template, so attaching is refused (UNSUPPORTED_BY_ADAPTER).
//
// The result marshals to JSON as a user adapter manifest
// (docs/adapter-manifests.md); write it to a file under an adapters/
// directory a [Registry] trusts.
func UserManifest(entry, attached string) map[string]any {
	m := map[string]any{
		"schema": 1, "name": "fakedbg", "description": "The fake test adapter", "version": "1.0",
		"adapter": map[string]any{
			"id": "fake", "entry": entry, "args": []any{noTestsArg},
			"environment": map[string]any{EnvFakeAdapter: "1"},
		},
		"language": map[string]any{"name": "fakelang", "extensions": []any{".fake"}},
		"options": map[string]any{
			argLines: map[string]any{"type": "int", "default": "10", "help": "the program's length"},
			"laps":   map[string]any{"type": "int", "help": "how often it runs"},
		},
		"launch": map[string]any{
			"require": []any{argProgram},
			"arguments": map[string]any{
				argProgram: "${program}", argLines: "${opt.lines}", "stopAtEntry": "${stopOnEntry}", "laps": "${opt.laps}",
			},
		},
		"attachUnsupported": "start it with 'eyedbg start fakelang'",
		"exceptions":        map[string]any{filterAll: []any{filterAll}, "uncaught": []any{filterUserUnhandled}},
		"evalGuard":         map[string]any{"nonCallWords": []any{"not"}, "safeCalls": []any{"len"}, "assignOps": []any{"="}},
	}

	if attached != "" {
		m["attach"] = map[string]any{"arguments": map[string]any{
			argProgram: attached, argLines: 10, "hang": true, "processId": "${pid}",
		}}
	}

	return m
}
