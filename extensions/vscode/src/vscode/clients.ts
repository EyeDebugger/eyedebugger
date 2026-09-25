// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The Clients view in Run and Debug: who is in each eyedbg session this
// window joined, who has control, when each was last seen, and the lease
// actions on a client's row (the eyedbg.lease.* commands do the work).

import * as vscode from 'vscode';
import { clientActions, merged, ordered, sessionActions } from '../core/clients';
import { clientKind } from '../core/protocol';
import { clientRowText, clientsSessionDescription, noIcons, plainText } from '../core/render';
import type { State } from './state';

/** A node of the Clients view: a session (by VS Code session id), or one of its clients. */
export interface ClientNode {
  session: string;
  client?: string;
}

/** node reads a command's argument: a node, which a caller may give a request's message. */
function node(x: unknown): (ClientNode & { message?: string }) | undefined {
  if (typeof x !== 'object' || x === null) {
    return undefined;
  }
  const n = x as { session?: unknown; client?: unknown; message?: unknown };
  if (typeof n.session !== 'string') {
    return undefined;
  }
  return {
    session: n.session,
    ...(typeof n.client === 'string' ? { client: n.client } : {}),
    ...(typeof n.message === 'string' ? { message: n.message } : {}),
  };
}

export class ClientsUi implements vscode.Disposable, vscode.TreeDataProvider<ClientNode> {
  private readonly changed = new vscode.EventEmitter<ClientNode | undefined>();
  readonly onDidChangeTreeData = this.changed.event;
  private readonly view: vscode.TreeView<ClientNode>;
  private readonly subs: vscode.Disposable[] = [];
  private timer: NodeJS.Timeout | undefined;

  constructor(private readonly state: State) {
    this.view = vscode.window.createTreeView('eyedbg.clients', { treeDataProvider: this });
    const lease = (
      command: string,
      extra: (n: ClientNode & { message?: string }) => Record<string, unknown> = () => ({}),
    ) =>
      vscode.commands.registerCommand(`eyedbg.clients.${command}`, (a?: unknown) => {
        const n = node(a);
        return n === undefined
          ? undefined
          : vscode.commands.executeCommand(`eyedbg.lease.${command}`, { session: n.session, ...extra(n) });
      });
    this.subs.push(
      this.changed,
      this.view,
      this.view.onDidChangeVisibility((e) => {
        if (e.visible) {
          void this.ask();
        }
      }),
      state.onDidChange(() => this.refresh()),
      vscode.commands.registerCommand('eyedbg.clients.refresh', () => this.ask()),
      lease('request', (n) => (n.message !== undefined ? { message: n.message } : {})),
      lease('take'),
      lease('release'),
      lease('grant', (n) => (n.client !== undefined ? { to: n.client } : {})),
      lease('policy'),
    );
  }

  dispose(): void {
    clearTimeout(this.timer);
    this.timer = undefined;
    for (const s of this.subs) {
      s.dispose();
    }
  }

  /** ask re-reads every session's clients (eyedbg/clients; the tracker records the answer). */
  private async ask(): Promise<void> {
    await Promise.all(
      this.state.all().map((t) =>
        this.state.custom(t, 'eyedbg/clients', {}).then(undefined, (e: unknown) => {
          this.state.log.warn(`eyedbg/clients for ${t.eyedbgId}: ${e instanceof Error ? e.message : String(e)}`);
        }),
      ),
    );
  }

  /** refresh redraws the view once the current burst of changes is over. */
  private refresh(): void {
    if (this.timer !== undefined) {
      return;
    }
    this.timer = setTimeout(() => {
      this.timer = undefined;
      this.changed.fire(undefined);
    }, 50);
  }

  getChildren(n?: ClientNode): ClientNode[] {
    if (n === undefined) {
      return this.state
        .all()
        .filter((t) => !t.unsupported)
        .map((t) => ({ session: t.session.id }));
    }
    const t = this.state.get(n.session);
    if (n.client !== undefined || t === undefined) {
      return [];
    }
    return ordered(merged(t.clients, t.seen), t.lease?.holder ?? '').map((c) => ({ session: n.session, client: c.id }));
  }

  getTreeItem(n: ClientNode): vscode.TreeItem {
    const t = this.state.get(n.session);
    if (t === undefined) {
      return new vscode.TreeItem('', vscode.TreeItemCollapsibleState.None);
    }
    if (n.client === undefined) {
      const item = new vscode.TreeItem(noIcons(plainText(t.eyedbgId, 80)), vscode.TreeItemCollapsibleState.Expanded);
      item.id = n.session;
      item.description = clientsSessionDescription(t.lease, t.client);
      item.contextValue = ['session', ...sessionActions(t.lease, t.client)].join(' ');
      return item;
    }
    const c = merged(t.clients, t.seen).find((x) => x.id === n.client);
    if (c === undefined) {
      return new vscode.TreeItem('', vscode.TreeItemCollapsibleState.None);
    }
    const text = clientRowText(c, t.lease, t.client);
    const item = new vscode.TreeItem(text.label, vscode.TreeItemCollapsibleState.None);
    item.id = `${n.session}/${c.id}`;
    item.description = text.description;
    item.tooltip = text.tooltip;
    item.iconPath = new vscode.ThemeIcon(
      c.id === t.client ? 'account' : clientKind(c.id) === 'agent' ? 'hubot' : 'person',
    );
    item.contextValue = ['client', ...clientActions(t.lease, c.id, t.client)].join(' ');
    return item;
  }
}
