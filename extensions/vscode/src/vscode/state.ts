// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What the extension knows about each eyedbg debug session in this window,
// fed by the debug adapter tracker (so in the adapter's order), and the
// read-only snapshots the exported API hands out.

import * as vscode from 'vscode';
import { ActivityLog } from '../core/activity';
import { later, merged } from '../core/clients';
import { LeaseHeldNotices, RequestNotices } from '../core/lease';
import { type Mirror, MirrorModel } from '../core/mirrors';
import { type Breakpoint, type ClientInfo, EyedbgError, type LeaseInfo } from '../core/protocol';
import { errorNotice, notificationSafe } from '../core/render';

const maxActivity = 200;
const maxNotices = 50;
const maxReveals = 50;
const maxEnded = 3;

export interface Annotation {
  uri: string;
  line: number;
  id: number;
  text: string;
}

export type NoticeKind = 'leaseHeld' | 'leaseRequest' | 'afterTakeOver' | 'autoJoin' | 'error' | 'warning' | 'info';

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
  /** Other clients' actions, and where their steps stopped (the Activity view). */
  readonly log = new ActivityLog();
  /** The time of each client's latest activity event: lastSeen moves without an eyedbg/clients event. */
  readonly seen = new Map<string, string>();
  askedAfterTakeOver = false;
  /** The facade predates the eyedbg/* messages (docs/adr/0014): no lease UI or annotations. */
  unsupported = false;
  /** The last failed eyedbg/* response per command: VS Code's customRequest error loses its code. */
  readonly failures = new Map<string, { code: string; text: string; holder: string }>();
  /** Whether the program runs (true), is stopped or ended (false), or unknown (until its first stop or run). */
  running: boolean | undefined;

  constructor(
    readonly session: vscode.DebugSession,
    private id: string,
    readonly client: string,
  ) {}

  /**
   * eyedbgId is the eyedbg session's id; '' while a launch connection
   * hasn't learned it yet (its eyedbg/session event): State hands out no
   * such entry, and no eyedbg/* request is sent for it.
   */
  get eyedbgId(): string {
    return this.id;
  }

  /** learned sets the id a launch connection learned; only once (false if it was known already). */
  learned(id: string): boolean {
    if (this.id !== '') {
      return false;
    }
    this.id = id;
    return true;
  }

  breakpoint(id: number): Breakpoint | undefined {
    return this.list.find((b) => b.id === id);
  }

  addActivity(line: string): void {
    this.activity.push(line);
    if (this.activity.length > maxActivity) {
      this.activity.splice(0, this.activity.length - maxActivity);
    }
  }

  /** sawClient records a client's activity at time (ISO). */
  sawClient(client: string, time: string): void {
    if (client !== '') {
      this.seen.set(client, later(this.seen.get(client) ?? '', time));
    }
  }

  /** base is the session's workspace folder path ('' if none): paths under it are shown relative. */
  get base(): string {
    return this.session.workspaceFolder?.uri.fsPath ?? '';
  }

  /** adapterStarted resets what belongs to one adapter connection (a Restart starts a new one). */
  adapterStarted(): void {
    this.mirrors.clear();
    this.failures.clear();
    this.log.reset();
    this.running = undefined;
  }
}

/** The activity of a debug session that ended, kept until Clear (the last 3). */
export interface EndedLog {
  key: string;
  session: string;
  base: string;
  log: ActivityLog;
}

/** A stop the extension revealed (followed). */
export interface Reveal {
  session: string;
  path: string;
  line: number;
}

export interface AutoJoinStats {
  /** polling: a look for sessions is scheduled or running. */
  state: 'idle' | 'polling';
  polls: number;
  lastError: string;
  /** The sessions the prompt offered, in order. */
  prompted: string[];
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
  running: boolean | undefined;
}

export class State implements vscode.Disposable {
  private readonly tracked = new Map<string, Tracked>();
  private readonly changed = new vscode.EventEmitter<void>();
  readonly onDidChange = this.changed.event;
  /** VS Code sessions whose adapter connection asked for a restart (disconnect or terminate). */
  readonly restarting = new Set<string>();
  notices: Notice[] = [];
  annotations: Annotation[] = [];
  /** The activity of ended debug sessions, oldest first. */
  ended: EndedLog[] = [];
  private endedCount = 0;
  reveals: Reveal[] = [];
  autoJoin: AutoJoinStats = { state: 'idle', polls: 0, lastError: '', prompted: [] };

  constructor(readonly log: vscode.LogOutputChannel) {}

  /** get is VS Code session id's entry, if its eyedbg session id is known. */
  get(id: string): Tracked | undefined {
    const t = this.tracked.get(id);
    return t !== undefined && t.eyedbgId !== '' ? t : undefined;
  }

  /** all are the entries whose eyedbg session id is known. */
  all(): Tracked[] {
    return [...this.tracked.values()].filter((t) => t.eyedbgId !== '');
  }

  /** active reports whether the window has an eyedbg debug session, its id known or not (a launch starting). */
  active(): boolean {
    return this.tracked.size > 0;
  }

  /**
   * begin returns the entry of a new adapter connection of session:
   * eyedbgId, or '' for a launch connection until it learns it. An attach's
   * Restart keeps the entry (same eyedbg session); a launch's Restart
   * starts a new session, so the old entry ends.
   */
  begin(session: vscode.DebugSession, eyedbgId: string, client: string): Tracked {
    let t = this.tracked.get(session.id);
    if (t === undefined || t.eyedbgId !== eyedbgId || t.client !== client) {
      if (t !== undefined) {
        this.archive(t);
      }
      t = new Tracked(session, eyedbgId, client);
      this.tracked.set(session.id, t);
    }
    t.adapterStarted();
    this.fire();
    return t;
  }

  end(id: string): void {
    const t = this.tracked.get(id);
    if (t === undefined) {
      return;
    }
    this.tracked.delete(id);
    this.archive(t);
    this.fire();
  }

  /** archive keeps an ended entry's activity (the last 3). */
  private archive(t: Tracked): void {
    if (t.log.size > 0 && t.eyedbgId !== '') {
      this.ended.push({ key: `ended-${++this.endedCount}`, session: t.eyedbgId, base: t.base, log: t.log });
      if (this.ended.length > maxEnded) {
        this.ended.splice(0, this.ended.length - maxEnded);
      }
    }
  }

  /** revealed records a followed stop (before it is shown). */
  revealed(r: Reveal): void {
    this.reveals.push(r);
    if (this.reveals.length > maxReveals) {
      this.reveals.splice(0, this.reveals.length - maxReveals);
    }
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
   * eyedbg/* messages marks the session unsupported and resolves undefined,
   * as does an entry whose eyedbg session id isn't known yet.
   */
  async custom(
    t: Tracked,
    command: string,
    args: Record<string, unknown>,
  ): Promise<Record<string, unknown> | undefined> {
    if (t.unsupported || t.eyedbgId === '') {
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
      launched: t.session.configuration.request === 'launch',
      unsupported: t.unsupported,
      lease: t.lease === undefined ? undefined : structuredClone(t.lease),
      // lastSeen raised by the clients' activity, as the Clients view shows it.
      clients: merged(t.clients, t.seen),
      mirrors: t.mirrors.all().map((m) => ({ ...m })),
      breakpoints: structuredClone(t.list),
      activity: [...t.activity],
      running: t.running,
    }));
  }

  dispose(): void {
    this.changed.dispose();
  }
}
