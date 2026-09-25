// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The CPU Trace view: 'eyedbg dotnet trace' of the target for a duration
// (CPU samples or GC events; the trace file stays in eyedbg's private
// directory), or the summary of a .nettrace file the user picks.

import * as vscode from 'vscode';
import { isProfile, type Profile, parseDuration, traceArgs, traceFileArgs } from '../../core/dotnet';
import { parseTrace } from '../../core/dotnetParse';
import { progressTitle } from '../../core/dotnetRender';
import { type TraceEntry, traceRows } from '../../core/dotnetTrees';
import { isRecord } from '../../core/protocol';
import { client } from '../config';
import { OpView, type Shared, viewId } from './shared';

const kept = 3;

/** traceArg reads Start Trace's optional argument: {profile: 'cpu'|'gc', duration: '10s'}. */
function traceArg(a: unknown): { profile: Profile; seconds: number } | string | undefined {
  if (a === undefined) {
    return undefined;
  }
  if (!isRecord(a) || !isProfile(a.profile) || typeof a.duration !== 'string') {
    return 'Start Trace takes {profile: "cpu" or "gc", duration: "10s"}';
  }
  const d = parseDuration(a.duration);
  return d.ok ? { profile: a.profile, seconds: d.value } : `duration: ${d.error}`;
}

export class TraceUi extends OpView {
  private entries: TraceEntry[] = [];
  private key = 0;

  constructor(shared: Shared) {
    super(shared, 'trace', () => traceRows(this.entries));
    this.subs.push(
      vscode.commands.registerCommand('eyedbg.dotnet.trace.start', (a?: unknown) => this.start(a)),
      vscode.commands.registerCommand('eyedbg.dotnet.trace.openFile', () => this.openFile()),
      vscode.commands.registerCommand('eyedbg.dotnet.trace.clear', () => {
        this.entries = [];
        this.tree.refresh();
        this.shared.state.fire();
      }),
    );
  }

  private add(taken: boolean, v: Record<string, unknown>): void {
    this.entries.push({ key: ++this.key, at: new Date(), taken, result: parseTrace(v) });
    if (this.entries.length > kept) {
      this.entries.splice(0, this.entries.length - kept);
    }
    this.succeeded();
  }

  /** ask asks for the profile and the duration. */
  private async ask(): Promise<{ profile: Profile; seconds: number } | undefined> {
    const pick = await vscode.window.showQuickPick(
      [
        { label: 'CPU — hottest methods', profile: 'cpu' as const },
        { label: 'GC — collections and allocations', profile: 'gc' as const },
      ],
      { title: 'EyeDebugger: trace what?' },
    );
    if (pick === undefined) {
      return undefined;
    }
    const d = await vscode.window.showInputBox({
      title: 'EyeDebugger: trace for how long?',
      value: '10s',
      prompt: 'From 1s to 5m; the program runs slower while it is traced',
      validateInput: (s) => {
        const r = parseDuration(s);
        return r.ok ? undefined : r.error;
      },
    });
    const r = d === undefined ? undefined : parseDuration(d);
    return r?.ok === true ? { profile: pick.profile, seconds: r.value } : undefined;
  }

  /** start traces the target: {profile, duration} (validated), else asked. */
  private async start(a: unknown): Promise<void> {
    const given = traceArg(a);
    if (typeof given === 'string') {
      void this.shared.state.show('error', `EyeDebugger: ${given}`);
      return;
    }
    if (!(await this.ready('dotnet.trace', true))) {
      return;
    }
    const what = given ?? (await this.ask());
    if (what === undefined) {
      return;
    }
    const t = await this.shared.resolve(true);
    if (t === undefined) {
      this.noTarget('trace');
      return;
    }
    const signal = this.begin();
    if (signal === undefined) {
      return;
    }
    let tick: NodeJS.Timeout | undefined;
    try {
      const r = traceArgs(t, client(), what.profile, what.seconds);
      await vscode.window.withProgress(
        {
          location: vscode.ProgressLocation.Notification,
          title: progressTitle(what.profile === 'gc' ? 'GC trace' : 'CPU trace', t),
          cancellable: true,
        },
        async (progress, token) => {
          token.onCancellationRequested(() => this.op.cancel());
          let left = what.seconds;
          progress.report({ message: `${left} s left` });
          tick = setInterval(() => {
            left--;
            progress.report({ message: left > 0 ? `${left} s left` : 'summarizing' });
            if (left <= 0) {
              clearInterval(tick);
            }
          }, 1000);
          this.add(true, await this.run(r, signal));
        },
      );
    } catch (e) {
      this.fail(e, true);
    } finally {
      clearInterval(tick);
      this.op.end();
    }
  }

  /** openFile summarizes a .nettrace file the user picks. */
  private async openFile(): Promise<void> {
    if (!(await this.ready('dotnet.trace', true))) {
      return;
    }
    const picked = await vscode.window.showOpenDialog({
      title: 'Open a .NET trace',
      canSelectMany: false,
      canSelectFolders: false,
      filters: { Traces: ['nettrace'], 'All files': ['*'] },
    });
    const uri = picked?.[0];
    if (uri === undefined || uri.scheme !== 'file') {
      return;
    }
    const signal = this.begin();
    if (signal === undefined) {
      return;
    }
    try {
      const r = traceFileArgs(uri.fsPath);
      await vscode.window.withProgress({ location: { viewId: viewId('trace') } }, async () => {
        this.add(false, await this.run(r, signal));
      });
    } catch (e) {
      this.fail(e, true);
    } finally {
      this.op.end();
    }
  }
}
