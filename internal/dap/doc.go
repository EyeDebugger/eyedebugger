// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package dap frames the Debug Adapter Protocol in both directions
// (docs/DESIGN.md §2, §3, §9). Its [Client] speaks DAP to an adapter over
// the adapter's stdio: it correlates requests and responses, delivers
// events, and answers reverse requests. Its [Server] is the server side of a
// connection from an editor (the DAP facade): it reads requests and writes
// responses and events. Both read messages through [ReadMessage], which
// bounds every header and body. Message types come from
// github.com/google/go-dap. It is the only package that frames DAP.
package dap
