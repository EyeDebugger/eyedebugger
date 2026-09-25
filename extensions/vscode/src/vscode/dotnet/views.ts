// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The "EyeDebugger .NET" views (docs/adr/0015, P2-M9): Counters, Memory,
// Threads and CPU Trace, in their own activity bar container, shown in a
// workspace with a .NET project or after "Show .NET Views"; one target for
// all four; each operation cancellable.

import * as vscode from 'vscode';
import type { Target } from '../../core/dotnet';
import type { Row } from '../../core/dotnetTrees';
import type { Eyedbg } from '../exec';
import type { State } from '../state';
import { type CountersStats, CountersUi } from './counters';
import { MemoryUi } from './memory';
import { type OpStats, Shared, type ViewName, viewNames } from './shared';
import { ThreadsUi } from './threads';
import { TraceUi } from './trace';

/** The API's view of the .NET views. */
export interface DotnetStats {
  show: boolean;
  /** The target picker is open. */
  picking: boolean;
  target: Target;
  /** The views' description: the target, and what auto follows now. */
  description: string;
  counters: CountersStats;
  memory: OpStats;
  threads: OpStats;
  trace: OpStats;
  /** The last 20 command lines, redacted. */
  runs: { view: ViewName | 'target'; argv: string[] }[];
}

const projects = '**/*.{csproj,fsproj,vbproj,sln,slnx}';
const skipped = '**/{node_modules,bin,obj,.git}/**';

export class DotnetUi implements vscode.Disposable {
  private readonly shared: Shared;
  readonly counters: CountersUi;
  readonly memory: MemoryUi;
  readonly threads: ThreadsUi;
  readonly trace: TraceUi;
  private readonly subs: vscode.Disposable[] = [];

  constructor(state: State, eyedbg: Eyedbg) {
    this.shared = new Shared(state, eyedbg);
    this.counters = new CountersUi(this.shared);
    this.threads = new ThreadsUi(this.shared);
    this.memory = new MemoryUi(this.shared, this.threads);
    this.trace = new TraceUi(this.shared);
    this.subs.push(
      this.counters,
      this.memory,
      this.threads,
      this.trace,
      this.shared,
      vscode.debug.onDidChangeActiveDebugSession(() => state.fire()),
      vscode.commands.registerCommand('eyedbg.dotnet.showViews', () => this.showViews()),
      vscode.commands.registerCommand('eyedbg.dotnet.chooseTarget', (a?: unknown) => this.shared.chooseCommand(a)),
      vscode.commands.registerCommand('eyedbg.dotnet.cancel', (a?: unknown) => this.cancel(a)),
    );
    this.shared.setContext('eyedbg.dotnet.show', false);
    void this.detect();
  }

  dispose(): void {
    for (const s of this.subs) {
      s.dispose();
    }
  }

  /** detect shows the views when the workspace has a .NET project (one match is enough). */
  private async detect(): Promise<void> {
    try {
      const found = await vscode.workspace.findFiles(projects, skipped, 1);
      if (found.length > 0) {
        this.setShow(true);
      }
    } catch (e) {
      this.shared.state.log.warn(`looking for a .NET project: ${e instanceof Error ? e.message : String(e)}`);
    }
  }

  private setShow(on: boolean): void {
    this.shared.show = on;
    this.shared.setContext('eyedbg.dotnet.show', on);
    this.shared.state.fire();
  }

  private async showViews(): Promise<void> {
    this.setShow(true);
    await vscode.commands.executeCommand('workbench.view.extension.eyedbg-dotnet');
  }

  /** cancel is eyedbg.dotnet.cancel {view}: a view's operation (Counters: its watch, as Pause). */
  private cancel(a: unknown): Thenable<unknown> | undefined {
    const view = typeof a === 'object' && a !== null ? (a as { view?: unknown }).view : undefined;
    if (!viewNames.includes(view as ViewName)) {
      return undefined;
    }
    if (view === 'counters') {
      return vscode.commands.executeCommand('eyedbg.dotnet.counters.pause');
    }
    this[view as Exclude<ViewName, 'counters'>].op.cancel();
    return undefined;
  }

  stats(): DotnetStats {
    return {
      show: this.shared.show,
      picking: this.shared.picking,
      target: structuredClone(this.shared.target),
      description: this.shared.description(),
      counters: this.counters.stats(),
      memory: this.memory.op.stats(),
      threads: this.threads.op.stats(),
      trace: this.trace.op.stats(),
      runs: this.shared.runs.map((r) => ({ view: r.view, argv: [...r.argv] })),
    };
  }

  /** trees are the four views' providers (the API walks them). */
  trees(): Record<ViewName, { getChildren(r?: Row): Row[]; getTreeItem(r: Row): vscode.TreeItem }> {
    return {
      counters: this.counters.tree,
      memory: this.memory.tree,
      threads: this.threads.tree,
      trace: this.trace.tree,
    };
  }
}
