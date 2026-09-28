// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Runs a checked eyedbg/runInTerminal request (docs/adr/0019 D7): args[0]
// is the terminal's own process (shellPath), args[1..] its arguments — no
// shell parses them, and nothing is typed into a terminal (no sendText).
// Nothing of the request (program, arguments, directory, environment,
// title) is logged.

import * as vscode from 'vscode';
import { type TerminalRequest, terminalName } from '../core/terminal';

/** How long the terminal's process may take to start. */
const processWaitMs = 20_000;

/** The answer to the facade: the process id of the program, or why there is none. */
export type TerminalAnswer = { processId: number } | { error: string };

const notStarted = "the terminal's process didn't start";

/** runInTerminal starts req's program in a new terminal and resolves with its process id, or an error. */
export async function runInTerminal(req: TerminalRequest): Promise<TerminalAnswer> {
  const [program, ...args] = req.args;
  if (program === undefined) {
    return { error: 'no program to run' };
  }
  let terminal: vscode.Terminal;
  try {
    terminal = vscode.window.createTerminal({
      name: terminalName(req.title),
      shellPath: program,
      shellArgs: args,
      cwd: req.cwd,
      env: req.env,
      iconPath: new vscode.ThemeIcon('debug'),
      isTransient: true,
    });
  } catch {
    return { error: "the terminal couldn't be created" };
  }
  terminal.show(true);

  const subs: vscode.Disposable[] = [];
  let timer: NodeJS.Timeout | undefined;
  try {
    const pid = await Promise.race([
      Promise.resolve(terminal.processId).then(
        (p) => p,
        () => undefined,
      ),
      new Promise<undefined>((resolve) => {
        subs.push(vscode.window.onDidCloseTerminal((t) => t === terminal && resolve(undefined)));
      }),
      new Promise<'timeout'>((resolve) => {
        timer = setTimeout(() => resolve('timeout'), processWaitMs);
      }),
    ]);
    if (pid === 'timeout') {
      // Don't leave a program running that nobody debugs.
      terminal.dispose();
      return { error: notStarted };
    }
    if (pid === undefined || !Number.isSafeInteger(pid) || pid < 1 || pid > 0x7fffffff) {
      return { error: notStarted };
    }
    return { processId: pid };
  } finally {
    clearTimeout(timer);
    for (const s of subs) {
      s.dispose();
    }
  }
}
