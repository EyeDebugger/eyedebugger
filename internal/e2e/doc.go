// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package e2e drives eyedbg and eyedbgd as real, separately built and
// spawned binaries, the way an agent or a human at a shell would: through
// the CLI, its autostart of the daemon, and its exit codes, not by calling
// any eyedbg package directly. It is the CLI-level counterpart to the
// driver-level end-to-end tests in drivers/dotnet and drivers/generic
// (docs/CONVENTIONS.md § Testing).
package e2e
