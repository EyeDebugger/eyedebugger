// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package helper runs side helpers (docs/DESIGN.md §7): separate programs,
// one process per eyedbg invocation, that speak JSON-RPC 2.0 over stdio with
// internal/api's framing (one JSON document per line, under 1 MiB). The
// caller starts one, which says hello, makes one call — whose result may be
// preceded by notifications — and closes it. Closing stdin cancels a call:
// the helper stops what it was doing and exits without answering.
//
// The package knows nothing of any helper's methods; drivers/dotnet
// specifies the .NET helper.
package helper
