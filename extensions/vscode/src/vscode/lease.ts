// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The control lease in the editor: a status bar item for the active eyedbg
// session, its menu and commands, the "X has control" notice on LEASE_HELD,
// other clients' requests, and the offer to switch to human-priority after
// taking control from an agent.

import * as vscode from 'vscode';
import {
  type AfterTakeOver,
  afterTakeOver,
  type LeaseAction,
  leaseHeldCode,
  leaseHeldHolder,
  menuActions,
} from '../core/lease';
import { type DapMessage, EyedbgError, isClientId, type LeaseInfo, parseLeasePolicy } from '../core/protocol';
import {
  clientLabel,
  leaseHeldNotice,
  noIcons,
  notificationSafe,
  plainText,
  requestNotice,
  statusLine,
  statusText,
  statusTooltip,
} from '../core/render';
import { debugType } from './config';
import type { State, Tracked } from './state';

const actionLabels: Record<LeaseAction, string> = {
  take: 'Take Control',
  forceTake: 'Take Control by Force…',
  request: 'Request Control…',
  release: 'Release Control',
  grant: 'Give Control to…',
  policy: 'Change Lease Policy…',
};

interface CommandArgs {
  session?: unknown;
  force?: unknown;
  confirmed?: unknown;
  message?: unknown;
  to?: unknown;
  policy?: unknown;
}

function args(x: unknown): CommandArgs {
  return typeof x === 'object' && x !== null ? (x as CommandArgs) : {};
}

/** target is the eyedbg session a command acts on: the one named, else the active one, else the only one. */
export function target(state: State, sessionArg: unknown): Tracked | undefined {
  if (typeof sessionArg === 'string') {
    return state.get(sessionArg);
  }
  const active = vscode.debug.activeDebugSession;
  if (active?.type === debugType) {
    const t = state.get(active.id);
    if (t !== undefined) {
      return t;
    }
  }
  const all = state.all();
  return all.length === 1 ? all[0] : undefined;
}

export class LeaseUi implements vscode.Disposable {
  private readonly item: vscode.StatusBarItem;
  private readonly subs: vscode.Disposable[] = [];

  constructor(private readonly state: State) {
    this.item = vscode.window.createStatusBarItem('eyedbg.lease', vscode.StatusBarAlignment.Left, 50);
    this.item.name = 'EyeDebugger Control';
    this.item.command = 'eyedbg.lease.menu';
    this.subs.push(
      this.item,
      state.onDidChange(() => this.render()),
      vscode.debug.onDidChangeActiveDebugSession(() => this.render()),
      vscode.commands.registerCommand('eyedbg.lease.menu', (a?: unknown) => this.guard(a, (t) => this.menu(t))),
      vscode.commands.registerCommand('eyedbg.lease.take', (a?: unknown) =>
        this.guard(a, (t) => this.take(t, args(a).force === true, args(a).confirmed === true)),
      ),
      vscode.commands.registerCommand('eyedbg.lease.request', (a?: unknown) =>
        this.guard(a, (t) => this.request(t, args(a).message)),
      ),
      vscode.commands.registerCommand('eyedbg.lease.release', (a?: unknown) =>
        this.guard(a, (t) => this.act(t, { action: 'release' })),
      ),
      vscode.commands.registerCommand('eyedbg.lease.grant', (a?: unknown) =>
        this.guard(a, (t) => this.grant(t, args(a).to)),
      ),
      vscode.commands.registerCommand('eyedbg.lease.policy', (a?: unknown) =>
        this.guard(a, (t) => this.policy(t, args(a).policy)),
      ),
    );
  }

  dispose(): void {
    for (const s of this.subs) {
      s.dispose();
    }
  }

  /** lease records a session's lease (from an eyedbg/lease event or response) and reacts to the change. */
  lease(t: Tracked, lease: LeaseInfo | undefined): void {
    if (lease === undefined) {
      return;
    }
    const before = t.lease;
    t.lease = lease;
    t.leaseHeld.leaseChanged(lease);
    for (const r of t.requests.next(lease, t.client)) {
      this.showRequest(t, r.client, r.message);
    }
    const setting = vscode.workspace.getConfiguration('eyedbg').get<AfterTakeOver>('lease.afterTakeOver', 'ask');
    const decision = afterTakeOver(
      setting === 'humanPriority' || setting === 'keep' ? setting : 'ask',
      before,
      lease,
      t.client,
      t.askedAfterTakeOver,
    );
    if (decision !== 'none') {
      t.askedAfterTakeOver = true;
    }
    if (decision === 'switch') {
      void this.act(t, { action: 'policy', policy: 'human-priority' }).catch((e: unknown) =>
        this.state.showError(e, t.session.id),
      );
    } else if (decision === 'ask') {
      void this.state
        .show(
          'afterTakeOver',
          `Under the free policy, the agent's next run or step takes control of ${t.eyedbgId} back. Switch it to human-priority, so the agent has to ask?`,
          t.session.id,
          'Switch',
          'Keep',
        )
        .then((pick) => (pick === 'Switch' ? this.act(t, { action: 'policy', policy: 'human-priority' }) : undefined))
        .then(undefined, (e: unknown) => this.state.showError(e, t.session.id));
    }
    this.state.fire();
  }

  /** leaseHeld handles a request VS Code sent that was refused with LEASE_HELD. */
  leaseHeld(t: Tracked, m: DapMessage): void {
    const holder = leaseHeldHolder(m, t.lease);
    if (t.unsupported || !t.leaseHeld.shouldShow(holder)) {
      return;
    }
    void this.state
      .show(
        'leaseHeld',
        leaseHeldNotice(clientLabel(holder), t.eyedbgId, t.lease?.policy ?? ''),
        t.session.id,
        'Request Control',
        'Take Over',
      )
      .then((pick) => {
        if (pick === 'Request Control') {
          return this.request(t, undefined);
        }
        if (pick === 'Take Over') {
          return this.take(t, false, false);
        }
        return undefined;
      })
      .then(undefined, (e: unknown) => this.state.showError(e, t.session.id));
  }

  private render(): void {
    const active = vscode.debug.activeDebugSession;
    const t = active?.type === debugType ? this.state.get(active.id) : undefined;
    if (t === undefined || t.lease === undefined || t.unsupported) {
      this.item.hide();
      return;
    }
    this.item.text = statusText(t.lease, t.client);
    this.item.tooltip = statusTooltip(t.lease, t.client, t.eyedbgId);
    this.item.show();
  }

  private async guard(a: unknown, fn: (t: Tracked) => Promise<unknown>): Promise<void> {
    const t = target(this.state, args(a).session);
    if (t === undefined) {
      void this.state.show('error', 'No eyedbg debug session is active.');
      return;
    }
    try {
      await fn(t);
    } catch (e) {
      this.state.showError(e, t.session.id);
    }
  }

  private async act(t: Tracked, a: Record<string, unknown>): Promise<void> {
    const body = await this.state.custom(t, 'eyedbg/lease', a);
    if (body === undefined) {
      void this.state.show(
        'warning',
        `The eyedbg daemon of ${t.eyedbgId} is too old for this: run 'eyedbg daemon stop' and join again.`,
      );
    }
  }

  private async menu(t: Tracked): Promise<void> {
    if (t.lease === undefined) {
      return;
    }
    const picks = menuActions(t.lease, t.client).map((a) => ({ label: actionLabels[a], action: a }));
    const pick = await vscode.window.showQuickPick(picks, {
      title: noIcons(statusLine(t.lease, t.client, t.eyedbgId)),
    });
    switch (pick?.action) {
      case 'take':
        return this.take(t, false, false);
      case 'forceTake':
        return this.take(t, true, false);
      case 'request':
        return this.request(t, undefined);
      case 'release':
        return this.act(t, { action: 'release' });
      case 'grant':
        return this.grant(t, undefined);
      case 'policy':
        return this.policy(t, undefined);
      default:
        return undefined;
    }
  }

  private async take(t: Tracked, force: boolean, confirmed: boolean): Promise<void> {
    if (!force) {
      try {
        return await this.act(t, { action: 'take' });
      } catch (e) {
        if (!(e instanceof EyedbgError) || e.code !== leaseHeldCode) {
          throw e;
        }
      }
    }
    if (!confirmed) {
      const holder = notificationSafe(clientLabel(t.lease?.holder ?? ''), 80);
      const ok = await vscode.window.showWarningMessage(
        `Take control of ${t.eyedbgId} from ${holder} by force?`,
        {
          modal: true,
          detail: `The lease policy (${t.lease?.policy ?? 'unknown'}) doesn't let you take it. ${holder} is told.`,
        },
        'Take Over',
      );
      if (ok !== 'Take Over') {
        return;
      }
    }
    await this.act(t, { action: 'take', force: true });
  }

  private async request(t: Tracked, message: unknown): Promise<void> {
    let text = typeof message === 'string' ? message : undefined;
    if (text === undefined) {
      text = await vscode.window.showInputBox({
        title: `Request control of ${t.eyedbgId}`,
        prompt: `Why? (optional; ${clientLabel(t.lease?.holder ?? '')} sees it)`,
        validateInput: (v) => (Array.from(v).length > 200 ? 'At most 200 characters' : undefined),
      });
      if (text === undefined) {
        return;
      }
    }
    await this.act(t, text !== '' ? { action: 'request', message: text } : { action: 'request' });
  }

  private async grant(t: Tracked, to: unknown): Promise<void> {
    let who = typeof to === 'string' ? to : undefined;
    if (who === undefined) {
      const others = t.clients.filter((c) => c.id !== t.client && isClientId(c.id));
      const pick = await vscode.window.showQuickPick(
        others.map((c) => ({ label: c.id, description: c.connected > 0 ? 'connected' : '' })),
        { title: `Give control of ${t.eyedbgId} to…` },
      );
      who = pick?.label;
    }
    if (who === undefined) {
      return;
    }
    if (!isClientId(who)) {
      throw new EyedbgError('INVALID_REQUEST', `not a client id: ${plainText(who, 80)}`);
    }
    await this.act(t, { action: 'grant', to: who });
  }

  private async policy(t: Tracked, policy: unknown): Promise<void> {
    let p = parseLeasePolicy(policy);
    if (policy === undefined) {
      const pick = await vscode.window.showQuickPick(
        [
          { label: 'free', description: 'anyone may take control; running a command takes it' },
          { label: 'handoff', description: 'control moves only when its holder gives it' },
          { label: 'human-priority', description: 'handoff, but a human may take it from an agent' },
        ],
        { title: `Lease policy of ${t.eyedbgId}` },
      );
      p = parseLeasePolicy(pick?.label);
      if (p === undefined) {
        return;
      }
    }
    if (p === undefined) {
      throw new EyedbgError('INVALID_REQUEST', 'the lease policy must be free, handoff or human-priority');
    }
    await this.act(t, { action: 'policy', policy: p });
  }

  private showRequest(t: Tracked, from: string, message: string): void {
    void this.state
      .show('leaseRequest', requestNotice(clientLabel(from), message), t.session.id, 'Give Control', 'Release')
      .then((pick) => {
        if (pick === 'Give Control') {
          return this.act(t, { action: 'grant', to: from });
        }
        if (pick === 'Release') {
          return this.act(t, { action: 'release' });
        }
        return undefined;
      })
      .then(undefined, (e: unknown) => this.state.showError(e, t.session.id));
  }
}
