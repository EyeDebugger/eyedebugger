// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package helper

import "os/exec"

// prepare sets platform attributes before proc.StartGroup starts cmd in
// its own process group. Nothing to add on Unix: stdio are pipes, so the
// background group never touches the terminal.
func prepare(*exec.Cmd) {}
