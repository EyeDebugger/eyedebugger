// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Following the agent (eyedbg.followAgent): another client's stop in the
// active eyedbg session is shown in an editor without taking focus. The
// facade marks such stops with preserveFocusHint, so VS Code itself leaves
// them alone (docs/adr/0014); this does what VS Code does for a stop with
// debug.focusEditorOnBreak off. VS Code keeps the frame it last focused
// (and shows its variables) after a quick step, and no API or command
// focuses a frame quietly: the human clicks the top frame (ADR 0015).

import * as vscode from 'vscode';
import { isAbsolute } from '../core/binary';
import type { SourceLine } from '../core/protocol';
import { debugType } from './config';
import type { State, Tracked } from './state';

/** openAt shows path at line in a text editor (a file: URI only). */
export async function openAt(at: SourceLine, opts: vscode.TextDocumentShowOptions): Promise<void> {
  if (!isAbsolute(at.path, process.platform)) {
    throw new Error(`not an absolute path: ${at.path}`);
  }
  const pos = new vscode.Position(at.line - 1, 0);
  await vscode.window.showTextDocument(vscode.Uri.file(at.path), { ...opts, selection: new vscode.Range(pos, pos) });
}

function samePath(a: string, b: string): boolean {
  return process.platform === 'win32' ? a.toLowerCase() === b.toLowerCase() : a === b;
}

export class Follow implements vscode.Disposable {
  private readonly subs: vscode.Disposable[] = [];

  constructor(private readonly state: State) {
    const set = (on: boolean) =>
      vscode.workspace.getConfiguration('eyedbg').update('followAgent', on, vscode.ConfigurationTarget.Global);
    this.subs.push(
      vscode.commands.registerCommand('eyedbg.followAgent.enable', () => set(true)),
      vscode.commands.registerCommand('eyedbg.followAgent.disable', () => set(false)),
    );
  }

  dispose(): void {
    for (const s of this.subs) {
      s.dispose();
    }
  }

  /** reveal shows another client's stop in t, if following is on and t is the active eyedbg session. */
  reveal(t: Tracked, at: SourceLine): void {
    if (vscode.workspace.getConfiguration('eyedbg').get<boolean>('followAgent', true) !== true) {
      return;
    }
    const active = vscode.debug.activeDebugSession;
    const all = this.state.all();
    const current = active?.type === debugType ? active.id === t.session.id : all.length === 1 && all[0] === t;
    if (!current || !isAbsolute(at.path, process.platform)) {
      return;
    }
    // The column already showing the file, else the active one (VS Code's
    // own choice for a stop it doesn't focus).
    const shown = vscode.window.visibleTextEditors.find(
      (e) => e.document.uri.scheme === 'file' && samePath(e.document.uri.fsPath, at.path),
    );
    const viewColumn = shown?.viewColumn ?? vscode.window.activeTextEditor?.viewColumn;
    this.state.revealed({ session: t.eyedbgId, path: at.path, line: at.line });
    this.state.fire();
    openAt(at, { preserveFocus: true, preview: true, ...(viewColumn !== undefined ? { viewColumn } : {}) }).then(
      undefined,
      (e: unknown) => this.state.log.warn(`following ${t.eyedbgId}: ${e instanceof Error ? e.message : String(e)}`),
    );
  }
}
