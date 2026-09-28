// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The debug adapter tracker: the extension reads everything about a session
// from the DAP traffic, in the adapter's order — the mirrors (VS Code hides
// its copies of them from extensions), the eyedbg/* events and refusals.
// The factory returns synchronously (VS Code drops trackers that take over
// a second). A launch connection learns its eyedbg session's id from the
// facade's eyedbg/session event, and runs its program in a terminal on the
// facade's eyedbg/runInTerminal event (docs/adr/0019).

import type * as vscode from 'vscode';
import { dapExitMessage } from '../core/cli';
import { nextRunState } from '../core/dotnet';
import { isLeaseHeld } from '../core/lease';
import {
  type DapMessage,
  errorCode,
  errorHolder,
  errorText,
  isRecord,
  num,
  parseBreakpoints,
  parseClients,
  parseDap,
  parseEvent,
  parseLease,
  type SourceLine,
} from '../core/protocol';
import { checkTerminal, TerminalGate, terminalId } from '../core/terminal';
import { isSessionId } from '../core/validate';
import { client } from './config';
import type { State, Tracked } from './state';
import { runInTerminal, type TerminalAnswer } from './terminal';

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
    const launch = session.configuration.request === 'launch';
    const id: unknown = launch ? '' : session.configuration.session;
    if (!launch && !isSessionId(id)) {
      return undefined;
    }
    let who: string;
    try {
      who = client();
    } catch {
      return undefined;
    }
    const t = this.state.begin(session, id as string, who);
    // Per connection: a Restart's old adapter may report its exit after the
    // new one started, and its exit is no start-up failure if it talked.
    let talked = false;
    // Per connection: this connection asked for a restart, so VS Code's
    // own disconnect after its terminated event doesn't cancel it.
    const conn = { restart: false };
    // Per connection: whether an eyedbg/runInTerminal event may run.
    const gate = new TerminalGate(launch && session.configuration.console === 'integratedTerminal');
    return {
      onWillReceiveMessage: (raw: unknown) => {
        const m = parseDap(raw);
        if (m !== undefined) {
          gate.fromEditor(m);
          this.fromEditor(t, m, conn);
        }
      },
      onDidSendMessage: (raw: unknown) => {
        talked = true;
        const m = parseDap(raw);
        if (m !== undefined) {
          gate.fromAdapter(m);
          if (m.type === 'event' && m.event === 'eyedbg/runInTerminal') {
            this.terminal(session, gate, isRecord(raw) ? raw.body : undefined);
          }
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

  /**
   * fromEditor notes a restart: an attach's Restart sends disconnect
   * {restart: true}; a launch's sends terminate {restart: true}, and VS Code
   * then disconnects without restart after the terminated event.
   */
  private fromEditor(t: Tracked, m: DapMessage, conn: { restart: boolean }): void {
    t.mirrors.fromEditor(m);
    t.log.fromEditor(m);
    if (m.type === 'request' && (m.command === 'disconnect' || m.command === 'terminate')) {
      if (m.arguments?.restart === true) {
        conn.restart = true;
        this.state.restarting.add(t.session.id);
      } else if (!conn.restart) {
        this.state.restarting.delete(t.session.id);
      }
    }
  }

  /**
   * terminal runs an eyedbg/runInTerminal event's program if the gate
   * admits it and its body passes the validation table, and answers the
   * facade; a refusal is answered with an error when the event's id is a
   * positive integer (else not at all). Nothing of the body is logged.
   */
  private terminal(session: vscode.DebugSession, gate: TerminalGate, body: unknown): void {
    const why = gate.admit();
    const checked = why === '' ? checkTerminal(body, process.platform) : undefined;
    if (checked?.ok === true) {
      const id = checked.value.id;
      void runInTerminal(checked.value).then((answer) => this.answerTerminal(session, id, answer));
      return;
    }
    const reason = checked?.ok === false ? checked.error : why;
    this.state.log.warn(`eyedbg/runInTerminal refused: ${reason}`);
    const id = terminalId(body);
    if (id !== undefined) {
      void this.answerTerminal(session, id, { error: `the extension refused it: ${reason}` });
    }
  }

  private async answerTerminal(session: vscode.DebugSession, id: number, answer: TerminalAnswer): Promise<void> {
    if ('error' in answer) {
      this.state.log.warn(`eyedbg/runInTerminal: ${answer.error}`);
    }
    try {
      await session.customRequest('eyedbg/runInTerminal', { id, ...answer });
    } catch {
      this.state.log.warn('eyedbg/runInTerminal: the facade refused the answer');
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
    const running = nextRunState(t.running, m);
    if (running !== t.running) {
      t.running = running;
      this.state.fire();
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
      case 'eyedbg/session': {
        const id: unknown = body.sessionId;
        if (!isSessionId(id)) {
          this.state.log.warn('eyedbg/session: not a session id; ignored');
        } else if (t.learned(id)) {
          this.state.log.info(`launched session ${id}`);
          this.state.fire();
        } else if (id !== t.eyedbgId) {
          this.state.log.warn(`eyedbg/session ${id} on the connection of ${t.eyedbgId}; ignored`);
        }
        break;
      }
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
