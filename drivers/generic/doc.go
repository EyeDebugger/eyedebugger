// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package generic is the driver for languages that need no Go code: an
// adapter manifest (internal/adapters, docs/adapter-manifests.md) says how
// to find the adapter, which options the language takes, how to render
// its launch and attach arguments, which exception filters stand for the
// exception modes, and what an expression may not contain for eval to run
// it without --allow-side-effects (guard.go). Python is served this way
// (docs/DESIGN.md §7).
//
// Nothing here knows any language: every fact comes from the manifest.
package generic
