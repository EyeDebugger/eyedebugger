// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// "EyeDebugger Activity": what other clients do in the sessions this window
// joined, one line per eyedbg/activity event, as 'eyedbg events' says it.
// Plain text; never the program's output or values (the facade sends
// neither).

import * as vscode from 'vscode';
import type { SessionEvent } from '../core/protocol';
import { activityLine } from '../core/render';
import type { State, Tracked } from './state';

export class ActivityUi implements vscode.Disposable {
  private readonly channel: vscode.OutputChannel;
  private readonly subs: vscode.Disposable[] = [];

  constructor(private readonly state: State) {
    this.channel = vscode.window.createOutputChannel('EyeDebugger Activity');
    this.subs.push(
      this.channel,
      vscode.commands.registerCommand('eyedbg.showActivity', () => this.channel.show(true)),
    );
  }

  add(t: Tracked, e: SessionEvent): void {
    const line = activityLine(e, t.eyedbgId, t.session.workspaceFolder?.uri.fsPath ?? '');
    this.channel.appendLine(line);
    t.addActivity(line);
    this.state.fire();
  }

  dispose(): void {
    for (const s of this.subs) {
      s.dispose();
    }
  }
}
