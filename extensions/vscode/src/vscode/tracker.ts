// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The debug adapter tracker: the extension reads everything about a session
// from the DAP traffic, in the adapter's order — the mirrors (VS Code hides
// its copies of them from extensions), the eyedbg/* events and refusals.
// The factory returns synchronously (VS Code drops trackers that take over
// a second).

import type * as vscode from 'vscode';
import { dapExitMessage } from '../core/cli';
import { isLeaseHeld } from '../core/lease';
import {
  type DapMessage,
  errorCode,
  errorHolder,
  errorText,
  num,
  parseBreakpoints,
  parseClients,
  parseDap,
  parseEvent,
  parseLease,
  type SourceLine,
} from '../core/protocol';
import { isSessionId } from '../core/validate';
import { client } from './config';
import type { State, Tracked } from './state';

/** What the tracker hands on. */
export interface Handlers {
  lease(t: Tracked, lease: ReturnType<typeof parseLease>): void;
  leaseHeld(t: Tracked, m: DapMessage): void;
  breakpointsChanged(): void;
  activity(t: Tracked, e: NonNullable<ReturnType<typeof parseEvent>>): void;
  /** activityChanged: a step's stop was located (the Activity view). */
  activityChanged(): void;
  /** reveal: another client's stop, located (following the agent). */
  reveal(t: Tracked, at: SourceLine): void;
}

export class TrackerFactory implements vscode.DebugAdapterTrackerFactory {
  constructor(
    private readonly state: State,
    private readonly handlers: Handlers,
  ) {}

  createDebugAdapterTracker(session: vscode.DebugSession): vscode.DebugAdapterTracker | undefined {
    const id: unknown = session.configuration.session;
    if (!isSessionId(id)) {
      return undefined;
    }
    let who: string;
    try {
      who = client();
    } catch {
      return undefined;
    }
    const t = this.state.begin(session, id, who);
    // Per connection: a Restart's old adapter may report its exit after the
    // new one started, and its exit is no start-up failure if it talked.
    let talked = false;
    return {
      onWillReceiveMessage: (raw: unknown) => {
        const m = parseDap(raw);
        if (m !== undefined) {
          this.fromEditor(t, m);
        }
      },
      onDidSendMessage: (raw: unknown) => {
        talked = true;
        const m = parseDap(raw);
        if (m !== undefined) {
          this.fromAdapter(t, m);
        }
      },
      onExit: (code: number | undefined) => {
        if (!talked) {
          // VS Code drops the adapter's stderr: say what its exit code means.
          void this.state.show('error', dapExitMessage(code, t.eyedbgId), session.id);
        }
      },
    };
  }

  private fromEditor(t: Tracked, m: DapMessage): void {
    t.mirrors.fromEditor(m);
    t.log.fromEditor(m);
    if (m.type === 'request' && m.command === 'disconnect') {
      if (m.arguments?.restart === true) {
        this.state.restarting.add(t.session.id);
      } else {
        this.state.restarting.delete(t.session.id);
      }
    }
  }

  private fromAdapter(t: Tracked, m: DapMessage): void {
    if (t.mirrors.fromAdapter(m)) {
      this.handlers.breakpointsChanged();
    }
    const u = t.log.fromAdapter(m);
    if (u.changed) {
      this.handlers.activityChanged();
    }
    if (u.reveal !== undefined) {
      this.handlers.reveal(t, u.reveal);
    }
    if (m.type === 'event') {
      this.event(t, m);
    } else if (m.type === 'response') {
      this.response(t, m);
    }
  }

  private event(t: Tracked, m: DapMessage): void {
    const body = m.body ?? {};
    switch (m.event) {
      case 'eyedbg/lease':
        this.handlers.lease(t, parseLease(body.lease));
        break;
      case 'eyedbg/clients':
        t.clients = parseClients(body.clients);
        this.state.fire();
        break;
      case 'eyedbg/breakpoints':
        t.list = parseBreakpoints(body.breakpoints);
        t.listMore = num(body.more);
        this.handlers.breakpointsChanged();
        break;
      case 'eyedbg/activity': {
        const e = parseEvent(body.event);
        if (e !== undefined) {
          this.handlers.activity(t, e);
        }
        break;
      }
      default:
        break;
    }
  }

  private response(t: Tracked, m: DapMessage): void {
    if (!m.success) {
      if (m.command.startsWith('eyedbg/')) {
        t.failures.set(m.command, { code: errorCode(m), text: errorText(m), holder: errorHolder(m) });
      } else if (isLeaseHeld(m)) {
        this.handlers.leaseHeld(t, m);
      }
      return;
    }
    const body = m.body ?? {};
    switch (m.command) {
      case 'eyedbg/lease':
        this.handlers.lease(t, parseLease(body.lease));
        break;
      case 'eyedbg/clients':
        t.clients = parseClients(body.clients);
        if (body.lease !== undefined) {
          this.handlers.lease(t, parseLease(body.lease));
        }
        this.state.fire();
        break;
      case 'eyedbg/breakpoints':
        t.list = parseBreakpoints(body.breakpoints);
        t.listMore = num(body.more);
        this.handlers.breakpointsChanged();
        break;
      default:
        break;
    }
  }
}
