// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The Threads view: 'eyedbg dotnet threads' of the target (it takes a heap
// dump: lock owners need one) or of a dump Memory took; threads grouped by
// stack as the CLI's text groups them. A frame with a source line opens it
// — the path comes from this window's own result, looked up by ids, and is
// opened only when it is a local absolute path of a regular file (D10).

import fs from 'node:fs';
import * as vscode from 'vscode';
import { isLocalAbsolute, threadsArgs } from '../../core/dotnet';
import { parseThreads } from '../../core/dotnetParse';
import { progressTitle } from '../../core/dotnetRender';
import { groupThreads, type ThreadsEntry, threadsRows } from '../../core/dotnetTrees';
import { plainText } from '../../core/render';
import { client } from '../config';
import { OpView, refOf, type Shared, viewId } from './shared';

const kept = 3;

export class ThreadsUi extends OpView {
  private entries: ThreadsEntry[] = [];
  private key = 0;

  constructor(shared: Shared) {
    super(
      shared,
      'threads',
      () => threadsRows(this.entries),
      (row, item) => {
        if (row.source && row.ref?.kind === 'frame') {
          item.command = {
            command: 'eyedbg.dotnet.threads.openFrame',
            title: 'Open Source',
            arguments: [{ kind: 'frame', run: row.ref.run, group: row.ref.group, frame: row.ref.frame }],
          };
        }
      },
    );
    this.subs.push(
      vscode.commands.registerCommand('eyedbg.dotnet.threads.capture', () => this.capture()),
      vscode.commands.registerCommand('eyedbg.dotnet.threads.openFrame', (a?: unknown) => this.openFrame(a)),
      vscode.commands.registerCommand('eyedbg.dotnet.threads.clear', () => {
        this.entries = [];
        this.tree.refresh();
        this.shared.state.fire();
      }),
    );
  }

  private add(taken: boolean, v: Record<string, unknown>): void {
    const result = parseThreads(v);
    this.entries.push({
      key: ++this.key,
      at: new Date(),
      taken,
      result,
      groups: groupThreads(result.threads, result.locks),
    });
    if (this.entries.length > kept) {
      this.entries.splice(0, this.entries.length - kept);
    }
    this.succeeded();
  }

  /** capture takes the target's threads (a heap dump: the program is suspended while it's written). */
  async capture(): Promise<void> {
    if (!(await this.ready('dotnet.dump', true))) {
      return;
    }
    const t = await this.shared.resolve(true);
    if (t === undefined) {
      this.noTarget('capture');
      return;
    }
    const signal = this.begin();
    if (signal === undefined) {
      return;
    }
    try {
      const r = threadsArgs({ target: t, client: client() });
      await vscode.window.withProgress(
        {
          location: vscode.ProgressLocation.Notification,
          title: progressTitle('Threads', t),
          cancellable: true,
        },
        async (progress, token) => {
          token.onCancellationRequested(() => this.op.cancel());
          progress.report({ message: "taking a heap dump — the program is suspended while it's written" });
          this.add(true, await this.run(r, signal));
        },
      );
    } catch (e) {
      this.fail(e, true);
    } finally {
      this.op.end();
    }
  }

  /** ofDump shows the threads of a dump Memory took (by its path in Memory's own state). */
  async ofDump(dump: string): Promise<void> {
    if (!(await this.ready('dotnet.dump', true))) {
      return;
    }
    const signal = this.begin();
    if (signal === undefined) {
      return;
    }
    void vscode.commands.executeCommand(`${viewId('threads')}.focus`);
    try {
      const r = threadsArgs({ dump });
      await vscode.window.withProgress({ location: { viewId: viewId('threads') } }, async () => {
        this.add(false, await this.run(r, signal));
      });
    } catch (e) {
      this.fail(e, true);
    } finally {
      this.op.end();
    }
  }

  /** openFrame opens a frame's source: {kind: 'frame', run, group, frame} ids of a result this window has. */
  private async openFrame(a: unknown): Promise<void> {
    const ref = refOf(a, 'frame');
    const e = ref === undefined ? undefined : this.entries.find((x) => x.key === ref.run);
    const f = ref === undefined ? undefined : e?.groups[ref.group]?.sample.frames[ref.frame];
    if (f === undefined || f.kind !== 'managed' || f.file === '' || f.line <= 0) {
      return;
    }
    await openSource(this.shared, f.file, f.line);
  }
}

/**
 * openSource opens file at line if it is a local absolute path (the
 * helper's rule, checked before any file system call: a UNC path would
 * connect to its server) of a regular file (not a FIFO or a device).
 */
export async function openSource(shared: Shared, file: string, line: number): Promise<void> {
  const notHere = () => void shared.state.show('info', `Source ${plainText(file, 300)}:${line} isn't on this machine.`);
  if (!isLocalAbsolute(file, process.platform === 'win32')) {
    notHere();
    return;
  }
  try {
    if (!fs.statSync(file).isFile()) {
      notHere();
      return;
    }
  } catch {
    notHere();
    return;
  }
  const pos = new vscode.Position(Math.max(0, line - 1), 0);
  try {
    await vscode.window.showTextDocument(vscode.Uri.file(file), {
      preview: true,
      selection: new vscode.Range(pos, pos),
    });
  } catch (e) {
    shared.state.showError(e);
  }
}
