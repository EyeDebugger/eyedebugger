// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What other clients do in the sessions this window joined: the "EyeDebugger
// Activity" channel, one line per eyedbg/activity event as 'eyedbg events'
// says it, and the Activity view in Run and Debug, where a step lands on
// the line it stopped at. Plain text; never the program's output or values
// (the facade sends neither).

import * as vscode from 'vscode';
import {
  type ActivityEntry,
  type ActivityKind,
  type ActivityLog,
  activityKinds,
  isActivityKind,
} from '../core/activity';
import type { SessionEvent } from '../core/protocol';
import { activityLabel, activityLine, activityTooltip, clock, noIcons, plainText } from '../core/render';
import { openAt } from './follow';
import type { State, Tracked } from './state';

/** A node of the Activity view: a session (by VS Code session id or ended key), or one of its entries. */
export interface ActivityNode {
  session: string;
  seq?: number;
}

const kindLabels: Record<ActivityKind, string> = {
  exec: 'Steps, runs and evaluations',
  breakpoint: 'Breakpoints',
  lease: 'Control (the lease)',
  client: 'Clients joining and leaving',
  exceptions: 'Exception settings',
};

const refreshDelayMs = 50;

function icon(e: SessionEvent): string {
  switch (e.kind) {
    case 'exec':
      switch (e.action) {
        case 'next':
          return 'debug-step-over';
        case 'stepIn':
          return 'debug-step-into';
        case 'stepOut':
          return 'debug-step-out';
        case 'continue':
        case 'runUntil':
          return 'debug-continue';
        case 'pause':
          return 'debug-pause';
        case 'eval':
          return 'debug-console';
        case 'set':
          return 'edit';
        default:
          return 'circle-outline';
      }
    case 'breakpoint':
      return 'debug-breakpoint';
    case 'lease':
      return 'key';
    case 'client':
      return 'person';
    case 'exceptions':
      return 'zap';
    default:
      return 'circle-outline';
  }
}

/** hidden is the eyedbg.activity.hide setting's valid kinds. */
function hidden(): ActivityKind[] {
  const v = vscode.workspace.getConfiguration('eyedbg').get<unknown>('activity.hide', []);
  return Array.isArray(v) ? v.filter(isActivityKind) : [];
}

interface Source {
  label: string;
  ended: boolean;
  base: string;
  log: ActivityLog;
}

export class ActivityUi implements vscode.Disposable, vscode.TreeDataProvider<ActivityNode> {
  private readonly channel: vscode.OutputChannel;
  private readonly changed = new vscode.EventEmitter<ActivityNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;
  private readonly subs: vscode.Disposable[] = [];
  private timer: NodeJS.Timeout | undefined;

  constructor(private readonly state: State) {
    this.channel = vscode.window.createOutputChannel('EyeDebugger Activity');
    this.subs.push(
      this.channel,
      this.changed,
      vscode.window.createTreeView('eyedbg.activity', { treeDataProvider: this }),
      state.onDidChange(() => this.refresh()),
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (e.affectsConfiguration('eyedbg.activity.hide')) {
          this.refresh();
        }
      }),
      vscode.commands.registerCommand('eyedbg.showActivity', () => this.channel.show(true)),
      vscode.commands.registerCommand('eyedbg.activity.open', (a?: unknown) => this.open(a)),
      vscode.commands.registerCommand('eyedbg.activity.filter', () => this.filter()),
      vscode.commands.registerCommand('eyedbg.activity.clear', () => this.clear()),
    );
  }

  dispose(): void {
    clearTimeout(this.timer);
    this.timer = undefined;
    for (const s of this.subs) {
      s.dispose();
    }
  }

  /** add records an eyedbg/activity event of t. */
  add(t: Tracked, e: SessionEvent): void {
    const line = activityLine(e, t.eyedbgId, t.base);
    this.channel.appendLine(line);
    t.addActivity(line);
    t.log.add(e, line);
    t.sawClient(e.client, e.time);
    this.state.fire();
  }

  /** refresh redraws the view once the current burst of changes is over. */
  refresh(): void {
    if (this.timer !== undefined) {
      return;
    }
    this.timer = setTimeout(() => {
      this.timer = undefined;
      this.changed.fire(undefined);
    }, refreshDelayMs);
  }

  private source(key: string): Source | undefined {
    const t = this.state.get(key);
    if (t !== undefined) {
      return { label: t.eyedbgId, ended: false, base: t.base, log: t.log };
    }
    const e = this.state.ended.find((x) => x.key === key);
    return e === undefined ? undefined : { label: e.session, ended: true, base: e.base, log: e.log };
  }

  getChildren(node?: ActivityNode): ActivityNode[] {
    if (node === undefined) {
      return [
        ...this.state.all().map((t) => ({ session: t.session.id })),
        ...[...this.state.ended].reverse().map((e) => ({ session: e.key })),
      ];
    }
    if (node.seq !== undefined) {
      return [];
    }
    return (this.source(node.session)?.log.entries(hidden()) ?? []).map((e) => ({ session: node.session, seq: e.seq }));
  }

  getTreeItem(node: ActivityNode): vscode.TreeItem {
    const src = this.source(node.session);
    if (node.seq === undefined) {
      const item = new vscode.TreeItem(
        noIcons(plainText(src?.label ?? node.session, 80)),
        vscode.TreeItemCollapsibleState.Expanded,
      );
      item.id = node.session;
      item.description = src?.ended === true ? 'ended' : '';
      item.contextValue = 'session';
      return item;
    }
    const entry = src?.log.get(node.seq);
    if (src === undefined || entry === undefined) {
      return new vscode.TreeItem('', vscode.TreeItemCollapsibleState.None);
    }
    return this.entryItem(node, entry, src.base);
  }

  private entryItem(node: ActivityNode, entry: ActivityEntry, base: string): vscode.TreeItem {
    const item = new vscode.TreeItem(activityLabel(entry, base), vscode.TreeItemCollapsibleState.None);
    item.id = `${node.session}/${entry.seq}`;
    item.description = clock(entry.event.time);
    item.tooltip = activityTooltip(entry);
    item.iconPath = new vscode.ThemeIcon(icon(entry.event));
    item.contextValue = entry.location !== undefined ? 'entry located' : 'entry';
    if (entry.location !== undefined) {
      item.command = {
        command: 'eyedbg.activity.open',
        title: 'Open',
        arguments: [{ session: node.session, seq: entry.seq }],
      };
    }
    return item;
  }

  /** open shows an entry's location (a user's click: it may take focus). */
  private async open(a: unknown): Promise<void> {
    const n = typeof a === 'object' && a !== null ? (a as { session?: unknown; seq?: unknown }) : {};
    const at =
      typeof n.session === 'string' && typeof n.seq === 'number'
        ? this.source(n.session)?.log.get(n.seq)?.location
        : undefined;
    if (at === undefined) {
      return;
    }
    try {
      await openAt(at, { preview: true });
    } catch (e) {
      this.state.showError(e);
    }
  }

  private async filter(): Promise<void> {
    const now = hidden();
    const picks = await vscode.window.showQuickPick(
      activityKinds.map((k) => ({ label: kindLabels[k], activity: k, picked: !now.includes(k) })),
      { title: 'Show in EyeDebugger Activity', canPickMany: true },
    );
    if (picks === undefined) {
      return;
    }
    const hide = activityKinds.filter((k) => !picks.some((p) => p.activity === k));
    await vscode.workspace.getConfiguration('eyedbg').update('activity.hide', hide, vscode.ConfigurationTarget.Global);
  }

  private clear(): void {
    for (const t of this.state.all()) {
      t.log.clear();
    }
    this.state.ended = [];
    this.state.fire();
  }
}
