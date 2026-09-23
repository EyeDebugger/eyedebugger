// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package dap is EyeDebugger's Debug Adapter Protocol client: it frames and
// correlates requests and responses over an adapter's stdio, delivers events,
// and answers reverse requests (docs/DESIGN.md §2, §3). Message types come
// from github.com/google/go-dap. It is the only package that speaks DAP.
package dap
