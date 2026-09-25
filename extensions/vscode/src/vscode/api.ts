// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The extension's exported API: read-only snapshots, for tests and other
// extensions. Unstable (apiVersion 0); nothing in it changes a session —
// commands do that, and any extension can already send eyedbg/* requests.

import * as vscode from 'vscode';
import type { DotnetStats } from './dotnet/views';
import type { Annotation, AutoJoinStats, Notice, Reveal, SessionSnapshot, State } from './state';

/** A tree view item as VS Code shows it, with its children. */
export interface TreeSnapshot {
  /** The item's id ('' if it has none). */
  id: string;
  label: string;
  description: string;
  tooltip: string;
  contextValue: string;
  icon: string;
  command: { command: string; arguments: unknown[] } | undefined;
  children: TreeSnapshot[];
}

export interface EyedbgApi {
  readonly apiVersion: 0;
  /** sessions are the eyedbg debug sessions of this window. */
  sessions(): SessionSnapshot[];
  /** annotations are the other clients' breakpoints drawn in visible editors. */
  annotations(): Annotation[];
  /** notices are the last 50 notifications the extension showed. */
  notices(): Notice[];
  /** stops are the sessions this window launched and stopped when their debug session ended. */
  stops(): { session: string; error: string }[];
  /** views are the Activity, Clients and .NET views' items, as shown now. */
  views(): {
    activity: TreeSnapshot[];
    clients: TreeSnapshot[];
    counters: TreeSnapshot[];
    memory: TreeSnapshot[];
    threads: TreeSnapshot[];
    trace: TreeSnapshot[];
  };
  /** dotnet is the .NET views' state: target, watch, operations, command lines. */
  dotnet(): DotnetStats;
  /** autoJoin is the auto-join prompt's state. */
  autoJoin(): AutoJoinStats;
  /** reveals are the last 50 stops followed (recorded before they are shown). */
  reveals(): Reveal[];
  readonly onDidChange: vscode.Event<void>;
}

function text(x: string | vscode.MarkdownString | vscode.TreeItemLabel | undefined): string {
  if (x === undefined) {
    return '';
  }
  if (typeof x === 'string') {
    return x;
  }
  return x instanceof vscode.MarkdownString ? x.value : x.label;
}

/** A tree data provider that answers synchronously (the extension's own). */
export interface SyncTree<T> {
  getChildren(node?: T): T[];
  getTreeItem(node: T): vscode.TreeItem;
}

/** walk snapshots a tree: the items VS Code would get now. */
function walk<T>(p: SyncTree<T>, parent?: T): TreeSnapshot[] {
  const out: TreeSnapshot[] = [];
  for (const n of p.getChildren(parent)) {
    const item = p.getTreeItem(n);
    out.push({
      id: item.id ?? '',
      label: text(item.label),
      description: typeof item.description === 'string' ? item.description : '',
      tooltip: text(item.tooltip),
      contextValue: item.contextValue ?? '',
      icon: item.iconPath instanceof vscode.ThemeIcon ? item.iconPath.id : '',
      command:
        item.command === undefined
          ? undefined
          : { command: item.command.command, arguments: structuredClone(item.command.arguments ?? []) },
      children: walk(p, n),
    });
  }
  return out;
}

export function api<A, C, R>(
  state: State,
  trees: {
    activity: SyncTree<A>;
    clients: SyncTree<C>;
    counters: SyncTree<R>;
    memory: SyncTree<R>;
    threads: SyncTree<R>;
    trace: SyncTree<R>;
  },
  dotnet: () => DotnetStats,
): EyedbgApi {
  return {
    apiVersion: 0,
    sessions: () => state.snapshots(),
    annotations: () => state.annotations.map((a) => ({ ...a })),
    notices: () => state.notices.map((n) => ({ ...n })),
    stops: () => state.stops.map((s) => ({ ...s })),
    views: () => ({
      activity: walk(trees.activity),
      clients: walk(trees.clients),
      counters: walk(trees.counters),
      memory: walk(trees.memory),
      threads: walk(trees.threads),
      trace: walk(trees.trace),
    }),
    dotnet,
    autoJoin: () => structuredClone(state.autoJoin),
    reveals: () => state.reveals.map((r) => ({ ...r })),
    onDidChange: state.onDidChange,
  };
}
