// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What the extension knows about each eyedbg debug session in this window,
// fed by the debug adapter tracker (so in the adapter's order), and the
// read-only snapshots the exported API hands out.

import * as vscode from 'vscode';
import { LeaseHeldNotices, RequestNotices } from '../core/lease';
import { type Mirror, MirrorModel } from '../core/mirrors';
import { type Breakpoint, type ClientInfo, EyedbgError, type LeaseInfo } from '../core/protocol';
import { errorNotice, notificationSafe } from '../core/render';
import { launchToken } from './config';

const maxActivity = 200;
const maxNotices = 50;

export interface Annotation {
  uri: string;
  line: number;
  id: number;
  text: string;
}

export type NoticeKind = 'leaseHeld' | 'leaseRequest' | 'afterTakeOver' | 'error' | 'warning' | 'info';

export interface Notice {
  kind: NoticeKind;
  /** The VS Code debug session id, if the notice is about one. */
  session: string;
  text: string;
}

/** One VS Code debug session of type eyedbg. */
export class Tracked {
  readonly mirrors = new MirrorModel((p) => vscode.Uri.file(p).toString());
  readonly leaseHeld = new LeaseHeldNotices();
  readonly requests = new RequestNotices();
  lease: LeaseInfo | undefined;
  clients: ClientInfo[] = [];
  list: Breakpoint[] = [];
  listMore = 0;
  activity: string[] = [];
  askedAfterTakeOver = false;
  /** The facade predates the eyedbg/* messages (docs/adr/0014): no lease UI or annotations. */
  unsupported = false;
  /** The last failed eyedbg/* response per command: VS Code's customRequest error loses its code. */
  readonly failures = new Map<string, { code: string; text: string; holder: string }>();

  constructor(
    readonly session: vscode.DebugSession,
    readonly eyedbgId: string,
    readonly client: string,
  ) {}

  breakpoint(id: number): Breakpoint | undefined {
    return this.list.find((b) => b.id === id);
  }

  addActivity(line: string): void {
    this.activity.push(line);
    if (this.activity.length > maxActivity) {
      this.activity.splice(0, this.activity.length - maxActivity);
    }
  }

  /** adapterStarted resets what belongs to one adapter connection (a Restart starts a new one). */
  adapterStarted(): void {
    this.mirrors.clear();
    this.failures.clear();
  }
}

export interface SessionSnapshot {
  vscodeSession: string;
  session: string;
  client: string;
  launched: boolean;
  unsupported: boolean;
  lease: LeaseInfo | undefined;
  clients: ClientInfo[];
  mirrors: Mirror[];
  breakpoints: Breakpoint[];
  activity: string[];
}

export class State implements vscode.Disposable {
  private readonly tracked = new Map<string, Tracked>();
  private readonly changed = new vscode.EventEmitter<void>();
  readonly onDidChange = this.changed.event;
  /** Launched sessions by launch token: which eyedbg session each launch started (never a config flag). */
  readonly launched = new Map<string, string>();
  /** VS Code sessions whose last disconnect asked for a restart. */
  readonly restarting = new Set<string>();
  notices: Notice[] = [];
  annotations: Annotation[] = [];
  /** Sessions this window stopped when their launch ended; error is the code if it failed. */
  stops: { session: string; error: string }[] = [];

  constructor(readonly log: vscode.LogOutputChannel) {}

  get(id: string): Tracked | undefined {
    return this.tracked.get(id);
  }

  all(): Tracked[] {
    return [...this.tracked.values()];
  }

  /** begin returns the session's entry, keeping it across a Restart (same VS Code session). */
  begin(session: vscode.DebugSession, eyedbgId: string, client: string): Tracked {
    let t = this.tracked.get(session.id);
    if (t === undefined || t.eyedbgId !== eyedbgId || t.client !== client) {
      t = new Tracked(session, eyedbgId, client);
      this.tracked.set(session.id, t);
    }
    t.adapterStarted();
    this.fire();
    return t;
  }

  end(id: string): void {
    if (this.tracked.delete(id)) {
      this.fire();
    }
  }

  isLaunched(t: Tracked): boolean {
    const token: unknown = t.session.configuration[launchToken];
    return typeof token === 'string' && this.launched.get(token) === t.eyedbgId;
  }

  /**
   * show shows a notification (never awaited by the caller's flow: it
   * resolves when the user picks an item). text holds session strings, so
   * it goes through notificationSafe; items are the extension's own.
   */
  show(kind: NoticeKind, text: string, session = '', ...items: string[]): Thenable<string | undefined> {
    const safe = notificationSafe(text, 1000);
    this.notices.push({ kind, session, text: safe });
    if (this.notices.length > maxNotices) {
      this.notices.splice(0, this.notices.length - maxNotices);
    }
    this.fire();
    switch (kind) {
      case 'error':
        return vscode.window.showErrorMessage(safe, ...items);
      case 'leaseHeld':
      case 'warning':
        return vscode.window.showWarningMessage(safe, ...items);
      default:
        return vscode.window.showInformationMessage(safe, ...items);
    }
  }

  /** showError reports an error once, with its code, and offers the log. */
  showError(e: unknown, session = ''): void {
    const err = e instanceof EyedbgError ? e : new EyedbgError('INTERNAL', e instanceof Error ? e.message : String(e));
    if (err.code === 'CANCELED') {
      return;
    }
    this.log.error(`${err.code}: ${err.message}${err.hint !== '' ? ` — ${err.hint}` : ''}`);
    void this.show('error', errorNotice(err.code, err.message), session, 'Show Log').then((pick) => {
      if (pick === 'Show Log') {
        this.log.show(true);
      }
    });
  }

  /**
   * custom sends an eyedbg/* request on t's session. A refusal becomes an
   * EyedbgError with the facade's code (VS Code's own error keeps only the
   * text; the tracker saw the response first). A facade that predates the
   * eyedbg/* messages marks the session unsupported and resolves undefined.
   */
  async custom(
    t: Tracked,
    command: string,
    args: Record<string, unknown>,
  ): Promise<Record<string, unknown> | undefined> {
    if (t.unsupported) {
      return undefined;
    }
    t.failures.delete(command);
    try {
      const body: unknown = await t.session.customRequest(command, args);
      return typeof body === 'object' && body !== null ? (body as Record<string, unknown>) : {};
    } catch (e) {
      const f = t.failures.get(command);
      if (f === undefined) {
        throw new EyedbgError('INTERNAL', e instanceof Error ? e.message : String(e));
      }
      if (f.code === 'INVALID_REQUEST' && f.text.includes('does not support')) {
        t.unsupported = true;
        this.log.warn(
          `session ${t.eyedbgId}: the eyedbg daemon predates ${command}; lease and breakpoint features are off`,
        );
        this.fire();
        return undefined;
      }
      throw new EyedbgError(f.code || 'INTERNAL', f.text);
    }
  }

  fire(): void {
    this.changed.fire();
  }

  snapshots(): SessionSnapshot[] {
    return this.all().map((t) => ({
      vscodeSession: t.session.id,
      session: t.eyedbgId,
      client: t.client,
      launched: this.isLaunched(t),
      unsupported: t.unsupported,
      lease: t.lease === undefined ? undefined : structuredClone(t.lease),
      clients: structuredClone(t.clients),
      mirrors: t.mirrors.all().map((m) => ({ ...m })),
      breakpoints: structuredClone(t.list),
      activity: [...t.activity],
    }));
  }

  dispose(): void {
    this.changed.dispose();
  }
}
