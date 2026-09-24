// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package api defines the native JSON-RPC 2.0 API between eyedbgd and its
// clients (the CLI now, the VS Code extension in phase 2): wire types, method
// names, stable error codes, the protocol version and the newline-delimited
// JSON framing (docs/DESIGN.md §2, §5, §6).
//
// Every connection starts with a [MethodHello] request carrying the per-user
// token; the daemon rejects anything else. The hello exchange is frozen across
// protocol versions so that any client can talk to any daemon well enough to
// detect a mismatch and restart it.
//
// [MethodFacadeOpen] turns an authenticated connection into a Debug Adapter
// Protocol connection to one session, for editors (docs/adr/0012).
package api
