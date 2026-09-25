// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The Memory view: take a heap dump of the target ('eyedbg dotnet dump',
// into eyedbg's private directory) and read its top types ('eyedbg dotnet
// heap -- DUMP'); expanding a type finds why its objects are alive (--gcroot),
// once per dump and type. The extension never reads, copies, moves or
// deletes a dump: it shows and copies its path.

import * as vscode from 'vscode';
import { dumpArgs, gcrootArgs, heapArgs } from '../../core/dotnet';
import { parseDump, parseHeap } from '../../core/dotnetParse';
import { errorText, progressTitle } from '../../core/dotnetRender';
import { type HeapEntry, heapRows } from '../../core/dotnetTrees';
import { EyedbgError } from '../../core/protocol';
import { client } from '../config';
import { asError, OpView, refOf, type Shared, viewId } from './shared';
import type { ThreadsUi } from './threads';

const kept = 3;

export class MemoryUi extends OpView {
  private entries: HeapEntry[] = [];
  private key = 0;

  constructor(
    shared: Shared,
    private readonly threads: ThreadsUi,
  ) {
    super(shared, 'memory', () => heapRows(this.entries));
    this.subs.push(
      this.tree.view.onDidExpandElement((e) => {
        if (e.element.ref?.kind === 'type') {
          void this.gcroot(e.element, false);
        }
      }),
      vscode.commands.registerCommand('eyedbg.dotnet.memory.takeDump', () => this.takeDump()),
      vscode.commands.registerCommand('eyedbg.dotnet.memory.openDump', () => this.openDump()),
      vscode.commands.registerCommand('eyedbg.dotnet.memory.gcroot', (a?: unknown) => this.gcroot(a, true)),
      vscode.commands.registerCommand('eyedbg.dotnet.memory.copyPath', (a?: unknown) => this.copyPath(a)),
      vscode.commands.registerCommand('eyedbg.dotnet.memory.showThreads', (a?: unknown) => {
        const p = this.path(a);
        return p === undefined ? undefined : this.threads.ofDump(p);
      }),
      vscode.commands.registerCommand('eyedbg.dotnet.memory.clear', () => {
        this.entries = [];
        this.tree.refresh();
        this.shared.state.fire();
      }),
    );
  }

  private add(dump: HeapEntry['dump'], heap: Record<string, unknown>): void {
    this.entries.push({ key: ++this.key, at: new Date(), dump, heap: parseHeap(heap), gcroots: new Map() });
    if (this.entries.length > kept) {
      this.entries.splice(0, this.entries.length - kept);
    }
    this.succeeded();
  }

  /** takeDump dumps the target's heap (it is suspended while its runtime writes) and reads its top types. */
  async takeDump(): Promise<void> {
    if (!(await this.ready('dotnet.dump', true))) {
      return;
    }
    const t = await this.shared.resolve(true);
    if (t === undefined) {
      this.noTarget('dump');
      return;
    }
    const signal = this.begin();
    if (signal === undefined) {
      return;
    }
    try {
      const d = dumpArgs(t, client());
      await vscode.window.withProgress(
        { location: vscode.ProgressLocation.Notification, title: progressTitle('Heap dump', t), cancellable: true },
        async (progress, token) => {
          token.onCancellationRequested(() => this.op.cancel());
          progress.report({ message: "writing it — the program is suspended while it's written" });
          const dump = parseDump(await this.run(d, signal));
          progress.report({ message: 'reading the heap' });
          this.add(dump, await this.run(heapArgs(dump.path), signal));
        },
      );
    } catch (e) {
      this.fail(e, true);
    } finally {
      this.op.end();
    }
  }

  /** openDump reads any dump file the user picks. */
  private async openDump(): Promise<void> {
    if (!(await this.ready('dotnet.dump', true))) {
      return;
    }
    const picked = await vscode.window.showOpenDialog({
      title: 'Open a .NET dump',
      canSelectMany: false,
      canSelectFolders: false,
      filters: { Dumps: ['dmp'], 'All files': ['*'] },
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
      const r = heapArgs(uri.fsPath);
      await vscode.window.withProgress({ location: { viewId: viewId('memory') } }, async () => {
        this.add(undefined, await this.run(r, signal));
      });
    } catch (e) {
      this.fail(e, true);
    } finally {
      this.op.end();
    }
  }

  private entry(dump: number): HeapEntry | undefined {
    return this.entries.find((e) => e.key === dump);
  }

  /** path is the dump path of a dump row (or {kind: 'dump', dump}), from this window's own state. */
  private path(a: unknown): string | undefined {
    const ref = refOf(a, 'dump');
    const e = ref === undefined ? undefined : this.entry(ref.dump);
    return e === undefined ? undefined : (e.dump?.path ?? e.heap.dump.path);
  }

  private async copyPath(a: unknown): Promise<void> {
    const p = this.path(a);
    if (p !== undefined) {
      await vscode.env.clipboard.writeText(p);
    }
  }

  /** gcroot finds why a type's objects are alive: a type row (or {kind: 'type', dump, type}). */
  private async gcroot(a: unknown, user: boolean): Promise<void> {
    const ref = refOf(a, 'type');
    const e = ref === undefined ? undefined : this.entry(ref.dump);
    const type = ref === undefined ? undefined : e?.heap.types[ref.type]?.name;
    if (e === undefined || type === undefined) {
      return;
    }
    const now = e.gcroots.get(type);
    if (now?.state === 'running' || now?.state === 'done') {
      return;
    }
    const signal = this.op.begin();
    if (signal === undefined) {
      e.gcroots.set(type, { state: 'busy' });
      this.tree.refresh();
      this.shared.state.fire();
      return;
    }
    e.gcroots.set(type, { state: 'running' });
    this.tree.refresh();
    try {
      const r = gcrootArgs(e.dump?.path ?? e.heap.dump.path, type);
      await vscode.window.withProgress({ location: { viewId: viewId('memory') } }, async () => {
        const result = parseHeap(await this.run(r, signal)).gcroot;
        if (result === undefined) {
          throw new EyedbgError('INTERNAL', 'eyedbg dotnet heap --gcroot printed no gcroot');
        }
        e.gcroots.set(type, { state: 'done', result });
      });
      this.succeeded();
    } catch (err) {
      const x = asError(err);
      if (x.code === 'CANCELED') {
        e.gcroots.delete(type);
      } else {
        e.gcroots.set(type, { state: 'failed', error: errorText(x) });
      }
      this.tree.refresh();
      this.fail(x, user);
    } finally {
      this.op.end();
    }
  }
}
