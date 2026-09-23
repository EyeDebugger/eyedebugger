// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package daemon implements eyedbgd's lifecycle and the client side of it
// (docs/DESIGN.md §6): the per-user runtime directory, the lifetime lock that
// guarantees one daemon per user, the Unix-socket listener with token
// authentication, idle exit, and the client's auto-start and version
// handshake.
//
// It serves the session methods of internal/api on top of a session.Manager,
// identifying each request's client, and keeps session metadata and
// recordings in the runtime directory's sessions/ ([FileStore]).
package daemon
