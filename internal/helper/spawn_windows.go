// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package helper

import (
	"os/exec"
	"syscall"
)

// createNoWindow is CREATE_NO_WINDOW: the helper gets no console, so none
// flashes up when a GUI (the VS Code extension) runs eyedbg.
const createNoWindow = 0x08000000

// prepare sets platform attributes before proc.StartGroup starts cmd in
// its own process group (CREATE_NEW_PROCESS_GROUP, OR-ed in there).
func prepare(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}
