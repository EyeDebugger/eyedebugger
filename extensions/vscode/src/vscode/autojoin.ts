// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The auto-join prompt (eyedbg.autoJoin): while the window has focus and no
// eyedbg debug session, look for sessions every 5 s ('eyedbg sessions
// --json': the resolved eyedbg binary, a fixed argv, never starting the
// daemon) and offer to join an agent's session whose program is in this
// workspace. One look at a time (a setTimeout chain, never overlapping);
// failures are logged once each, never shown, and back off to 60 s. At
// most one prompt is open; one is offered per session, never again once
// ignored, joined or launched here.

import fs from 'node:fs';
import * as vscode from 'vscode';
import { agentOf, candidates, type Plan, plan } from '../core/autojoin';
import { parseSessions, sessionsArgs } from '../core/cli';
import type { SessionInfo } from '../core/protocol';
import { autoJoinPrompt } from '../core/render';
import { client } from './config';
import type { Eyedbg } from './exec';
import type { State } from './state';

const pollTimeoutMs = 10_000;

function realpath(p: string): string {
  try {
    return fs.realpathSync.native(p);
  } catch {
    return p;
  }
}

function folders(): string[] {
  return (vscode.workspace.workspaceFolders ?? []).filter((f) => f.uri.scheme === 'file').map((f) => f.uri.fsPath);
}

export class AutoJoin implements vscode.Disposable {
  private readonly subs: vscode.Disposable[] = [];
  private timer: NodeJS.Timeout | undefined;
  private polling = false;
  private failing = false;
  private disposed = false;
  /** Sessions never to offer again in this window. */
  private readonly skip = new Set<string>();
  /** The session whose prompt is open. */
  private open: string | undefined;
  /** Failure messages already logged. */
  private readonly logged = new Set<string>();

  constructor(
    private readonly state: State,
    private readonly eyedbg: Eyedbg,
    private readonly join: (session: string) => Thenable<unknown>,
    /** The window counts as focused (EYEDBG_TEST_ASSUME_FOCUSED: focus is unreliable under xvfb). */
    private readonly assumeFocused: boolean,
  ) {
    this.subs.push(
      vscode.window.onDidChangeWindowState(() => this.replan()),
      vscode.workspace.onDidChangeWorkspaceFolders(() => this.replan()),
      state.onDidChange(() => this.replan()),
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (e.affectsConfiguration('eyedbg.path')) {
          this.failing = false;
          clearTimeout(this.timer); // the back-off wait no longer applies
          this.timer = undefined;
        }
        if (e.affectsConfiguration('eyedbg.autoJoin') || e.affectsConfiguration('eyedbg.path')) {
          this.replan();
        }
      }),
    );
    this.replan();
  }

  dispose(): void {
    this.disposed = true;
    clearTimeout(this.timer);
    this.timer = undefined;
    for (const s of this.subs) {
      s.dispose();
    }
  }

  private plan(): Plan {
    return plan({
      setting: vscode.workspace.getConfiguration('eyedbg').get<string>('autoJoin', 'ask'),
      focused: this.assumeFocused || vscode.window.state.focused,
      folders: folders().length,
      joined: this.state.active(),
      failing: this.failing,
    });
  }

  /** replan schedules the next look, or cancels it; a look in progress schedules its own successor. */
  private replan(): void {
    if (this.disposed) {
      return;
    }
    // A session joined or launched here is never offered.
    for (const t of this.state.all()) {
      this.skip.add(t.eyedbgId);
    }
    const p = this.plan();
    if (!p.poll) {
      clearTimeout(this.timer);
      this.timer = undefined;
    } else if (this.timer === undefined && !this.polling) {
      this.timer = setTimeout(() => {
        this.timer = undefined;
        void this.poll();
      }, p.delayMs);
    }
    this.setState(this.timer !== undefined || this.polling ? 'polling' : 'idle');
  }

  private setState(s: 'idle' | 'polling'): void {
    if (this.state.autoJoin.state !== s) {
      this.state.autoJoin.state = s;
      this.state.fire();
    }
  }

  private async poll(): Promise<void> {
    if (this.disposed || !this.plan().poll) {
      this.replan();
      return;
    }
    this.polling = true;
    try {
      const self = client();
      const sessions = parseSessions(await this.eyedbg.run(sessionsArgs(), { timeoutMs: pollTimeoutMs, quiet: true }));
      this.failing = false;
      // The plan may have changed while the run was in flight (never, a join, a launch, focus lost).
      if (!this.disposed && this.plan().poll) {
        this.offer(sessions, self);
      }
    } catch (e) {
      this.failing = true;
      const text = e instanceof Error ? e.message : String(e);
      const code = typeof (e as { code?: unknown }).code === 'string' ? (e as { code: string }).code : '';
      const msg = code !== '' ? `${code}: ${text}` : text;
      this.state.autoJoin.lastError = msg;
      if (!this.logged.has(msg)) {
        this.logged.add(msg);
        this.state.log.warn(`looking for agent sessions (eyedbg.autoJoin): ${msg}`);
      }
    } finally {
      this.polling = false;
      this.state.autoJoin.polls++;
      this.state.fire();
      this.replan();
    }
  }

  private offer(sessions: SessionInfo[], self: string): void {
    const list = candidates(sessions, {
      folders: folders(),
      self,
      skip: this.skip,
      platform: process.platform,
      realpath,
    });
    // An unanswered prompt blocks others only while its session is still one to offer.
    if (this.open !== undefined && !list.some((s) => s.id === this.open)) {
      this.open = undefined;
    }
    const first = list[0];
    if (this.open !== undefined || first === undefined) {
      return;
    }
    const id = first.id;
    this.open = id;
    this.state.autoJoin.prompted.push(id);
    void this.state.show('autoJoin', autoJoinPrompt(first, agentOf(first)), '', 'Join', 'Ignore').then((pick) => {
      if (this.open === id) {
        this.open = undefined;
      }
      this.skip.add(id);
      if (pick === 'Join') {
        return Promise.resolve(this.join(id)).then(undefined, (e: unknown) => this.state.showError(e));
      }
      return undefined;
    });
  }
}
