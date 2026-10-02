// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package container runs the docker CLI for eyedbg: it inspects a
// container through a pinned --format template (never its environment,
// command or entrypoint arguments), streams an adapter into it as a tar and
// builds the argv that runs the adapter there (docs/adr/0020).
//
// Nothing here is .NET specific. Every invocation is an argv, never a shell;
// each name that reaches one is checked against a grammar first, so a value
// can't turn into a flag. Docker's output is bounded, and its stderr is cut
// and stripped of control characters before it reaches an error.
package container
