// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What other clients did in a session (eyedbg/activity events), and where
// each of their steps landed. The facade sends another client's step as
// 'continued', then its eyedbg/activity event, then the stop (in that
// order, docs/adr/0014); VS Code asks for the stopped thread's top frame
// after every stop (debugModel.ts refreshTopOfCallstack), so the stop's
// location is read from VS Code's own stackTrace response: no request of
// the extension's. A stop with preserveFocusHint (another client caused
// it) is a candidate for following the agent.

import { type DapMessage, parseStackTop, parseStop, type SessionEvent, type SourceLine } from './protocol';

export const activityKinds = ['exec', 'breakpoint', 'lease', 'client', 'exceptions'] as const;

export type ActivityKind = (typeof activityKinds)[number];

export function isActivityKind(x: unknown): x is ActivityKind {
  return activityKinds.some((k) => k === x);
}

export const maxEntries = 200;

export interface ActivityEntry {
  /** The entry's number in its log (increasing from 1). */
  seq: number;
  event: SessionEvent;
  /** The activity channel's line for it. */
  line: string;
  /** Where it stopped (an exec) or the breakpoint's line. */
  location: SourceLine | undefined;
  /** The reason of the stop an exec caused; '' until it stopped. */
  stopReason: string;
}

/** What a DAP message changed: the entries, and a stop to follow (another client's, located). */
export interface Update {
  changed: boolean;
  reveal: SourceLine | undefined;
}

/** Actions of another client's that make the program run or stop. */
const resumes = new Set(['continue', 'next', 'stepIn', 'stepOut', 'runUntil', 'pause']);

/** The editor's own execution requests: the stop after one is the editor's. */
const editorExecs = new Set([
  'continue',
  'next',
  'stepIn',
  'stepOut',
  'pause',
  'stepBack',
  'reverseContinue',
  'goto',
  'restartFrame',
]);

interface PendingStop {
  thread: number;
  reason: string;
  hint: boolean;
  entry: ActivityEntry | undefined;
  /** The seq of the editor's stackTrace request for the top frame; 0 until sent. */
  request: number;
}

const none: Update = { changed: false, reveal: undefined };

export class ActivityLog {
  private list: ActivityEntry[] = [];
  private next = 1;
  /** Another client's execution request whose stop hasn't come yet. */
  private pending: ActivityEntry | undefined;
  /** The last stop, until its top frame is known. */
  private stop: PendingStop | undefined;

  /** add records an eyedbg/activity event (another client's action); line is its channel line. */
  add(event: SessionEvent, line: string): ActivityEntry {
    const bp = event.breakpoint;
    const entry: ActivityEntry = {
      seq: this.next++,
      event,
      line,
      location:
        event.kind === 'breakpoint' && bp !== undefined && bp.function === '' && bp.file !== '' && bp.line > 0
          ? { path: bp.file, line: bp.line }
          : undefined,
      stopReason: '',
    };
    this.list.push(entry);
    if (this.list.length > maxEntries) {
      this.list.splice(0, this.list.length - maxEntries);
    }
    if (event.kind === 'exec' && resumes.has(event.action)) {
      this.pending = entry;
    }
    return entry;
  }

  /** fromEditor reads a message VS Code sends the adapter. */
  fromEditor(m: DapMessage): void {
    if (m.type !== 'request') {
      return;
    }
    if (editorExecs.has(m.command)) {
      this.pending = undefined;
      return;
    }
    if (m.command !== 'stackTrace' || this.stop === undefined) {
      return;
    }
    const a = m.arguments ?? {};
    const start = a.startFrame;
    if (a.threadId === this.stop.thread && (start === undefined || start === 0)) {
      this.stop.request = m.seq;
    }
  }

  /** fromAdapter reads a message the adapter sends VS Code. */
  fromAdapter(m: DapMessage): Update {
    if (m.type === 'event') {
      switch (m.event) {
        case 'stopped': {
          const s = parseStop(m.body);
          const entry = this.pending;
          this.pending = undefined;
          this.stop = { thread: s.threadId, reason: s.reason, hint: s.hint, entry, request: 0 };
          if (entry !== undefined) {
            entry.stopReason = s.reason;
            return { changed: true, reveal: undefined };
          }
          return none;
        }
        case 'continued':
          this.stop = undefined;
          return none;
        case 'terminated':
          this.reset();
          return none;
        default:
          return none;
      }
    }
    const stop = this.stop;
    if (
      m.type !== 'response' ||
      m.command !== 'stackTrace' ||
      stop === undefined ||
      stop.request === 0 ||
      m.requestSeq !== stop.request
    ) {
      return none;
    }
    if (!m.success) {
      return none;
    }
    this.stop = undefined;
    const top = parseStackTop(m.body);
    if (top === undefined) {
      return none;
    }
    if (stop.entry !== undefined) {
      stop.entry.location = top;
    }
    return { changed: stop.entry !== undefined, reveal: stop.hint ? { ...top } : undefined };
  }

  /** reset forgets the stop being located and the pending request (a new adapter connection, the end). */
  reset(): void {
    this.pending = undefined;
    this.stop = undefined;
  }

  /** clear removes every entry (locating the current stop goes on: following doesn't depend on the list). */
  clear(): void {
    this.list = [];
  }

  get(seq: number): ActivityEntry | undefined {
    return this.list.find((e) => e.seq === seq);
  }

  /** entries are the entries not of a hidden kind, newest first. */
  entries(hidden: readonly string[] = []): ActivityEntry[] {
    return this.list.filter((e) => !hidden.includes(e.event.kind)).reverse();
  }

  get size(): number {
    return this.list.length;
  }
}
