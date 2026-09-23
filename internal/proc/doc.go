// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Package proc looks up other processes and runs process groups: who owns a
// process eyedbg is asked to attach to (it attaches only to the user's own),
// and starting and killing a command with all its children (a 'dotnet test'
// run and its test host).
//
// Linux reads /proc, other Unix systems run ps, Windows asks the process
// token. Groups are Unix process groups, and on Windows a new process group
// killed with taskkill /T.
package proc
