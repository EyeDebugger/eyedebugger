// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package daemon implements eyedbgd's lifecycle and the client side of it
// (docs/DESIGN.md §6): the per-user runtime directory, the lifetime lock that
// guarantees one daemon per user, the Unix-socket listener with token
// authentication, idle exit, and the client's auto-start and version
// handshake.
//
// Sessions arrive in milestone 2; until then a daemon always has zero.
package daemon
