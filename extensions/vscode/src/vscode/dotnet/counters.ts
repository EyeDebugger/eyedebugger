// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The Counters view: a live 'eyedbg dotnet counters --watch --json' of the
// target, its values and changes. Pause ends the watch process (no
// diagnostics session stays in the program); Start/Resume starts a new one.
//
// Every change of the watch — start, pause, a target change, clear — runs
// in one queue, and each watch has a generation: a line or an end from an
// older watch never touches the current one. At most one watch process runs:
// a new one starts after the old one closed.

import * as vscode from 'vscode';
import { CounterBoard, type Resolved, sameTarget, watchArgs } from '../../core/dotnet';
import { parseWatchLine } from '../../core/dotnetParse';
import { counterText, errorText, targetText, type WatchState, watchMessage } from '../../core/dotnetRender';
import type { Row } from '../../core/dotnetTrees';
import { client } from '../config';
import type { Stream } from '../exec';
import { asError, RowTree, type Shared, viewId } from './shared';

export interface CountersStats {
  state: WatchState;
  /** A watch process runs (until its close). */
  watching: boolean;
  samples: number;
  message: string;
  /** The watch's target ('' before the first). */
  target: string;
}

const maxAutoStarts = 3;

export class CountersUi implements vscode.Disposable {
  readonly tree: RowTree;
  private readonly board = new CounterBoard();
  private readonly subs: vscode.Disposable[] = [];
  private state: WatchState = 'idle';
  private watch: Stream | undefined;
  private gen = 0;
  private target: Resolved | undefined;
  private error = '';
  private userStarted = false;
  private queue: Promise<void> = Promise.resolve();
  private restartQueued = false;
  /** The next sample starts a new board (deltas and totals restart with each watch). */
  private fresh = false;
  /** Automatic starts since the target's program last began to run (at most maxAutoStarts). */
  private autoStarts = 0;
  private lastRunning = false;
  private retryTimer: NodeJS.Timeout | undefined;
  private disposed = false;

  constructor(private readonly shared: Shared) {
    this.tree = new RowTree(viewId('counters'), () => this.rows());
    this.subs.push(
      this.tree,
      vscode.commands.registerCommand('eyedbg.dotnet.counters.start', () => this.enqueue(() => this.start(true))),
      vscode.commands.registerCommand('eyedbg.dotnet.counters.pause', () => this.enqueue(() => this.pause())),
      vscode.commands.registerCommand('eyedbg.dotnet.counters.clear', () => this.enqueue(() => this.clear())),
      shared.onDidChangeTarget(() => this.enqueue(() => this.reset())),
      shared.state.onDidChange(() => this.changed()),
    );
    this.changed();
  }

  dispose(): void {
    this.disposed = true;
    this.cancelRetry();
    this.gen++;
    void this.watch?.stop();
    for (const s of this.subs) {
      s.dispose();
    }
  }

  stats(): CountersStats {
    return {
      state: this.state,
      watching: this.watch !== undefined,
      samples: this.board.samples,
      message: this.message(),
      target: this.target === undefined ? '' : targetText(this.target),
    };
  }

  /** enqueue runs fn after every earlier change of the watch (a failure is logged, never breaks the queue). */
  private enqueue(fn: () => Promise<void>): Promise<void> {
    const next = this.queue.then(fn).catch((e: unknown) => {
      this.shared.state.log.error(`Counters: ${asError(e).message}`);
    });
    this.queue = next;
    return next;
  }

  private rows(): Row[] {
    return this.board.rows().map((r) => {
      const text = counterText(r);
      return {
        id: `counter/${r.name}`,
        label: text.label,
        description: text.description,
        tooltip: text.tooltip,
        icon: r.kind === 'sum' ? 'add' : 'pulse',
        context: 'counter',
        expanded: undefined,
        children: [],
        ref: undefined,
        source: false,
      };
    });
  }

  private message(): string {
    const stopped = this.target?.kind === 'session' && this.shared.stopped(this.target.id);
    return watchMessage(this.state, this.target, stopped, this.error);
  }

  /** changed updates what depends on other state: the message, the description, an automatic start (D7). */
  private changed(): void {
    if (this.disposed) {
      return;
    }
    this.tree.view.message = this.message();
    this.tree.view.description = this.shared.description();
    this.shared.setContext(
      'eyedbg.dotnet.counters.running',
      this.state === 'starting' || this.state === 'running' || this.state === 'waiting',
    );
    // An automatic start when a joined session waited for runs again. The
    // daemon may still say stopped for a moment after VS Code heard it run
    // (NOT_RUNNING again): a few tries per run, 1 s and 2 s apart, never a
    // loop; then, or when the session is gone, "waiting" says so no more.
    const t = this.target;
    if (this.state !== 'waiting' || t?.kind !== 'session') {
      return;
    }
    if (!this.shared.joined(t.id)) {
      this.cancelRetry();
      this.set('idle');
      return;
    }
    const running = this.shared.runningNow(t.id);
    if (running && !this.lastRunning) {
      this.autoStarts = 0;
    }
    this.lastRunning = running;
    if (!running || this.restartQueued) {
      return;
    }
    if (this.autoStarts >= maxAutoStarts) {
      this.set('failed', `${t.id} runs, but eyedbg still reported it stopped: press Start`);
      return;
    }
    const delayMs = this.autoStarts * 1000;
    this.autoStarts++;
    this.restartQueued = true;
    this.retryTimer = setTimeout(() => {
      this.retryTimer = undefined;
      void this.enqueue(async () => {
        this.restartQueued = false;
        if (this.state === 'waiting') {
          this.shared.state.log.info(`Counters: ${t.id} runs again: starting the watch`);
          await this.start(false, t);
        }
      });
    }, delayMs);
  }

  /** cancelRetry drops a pending automatic start. */
  private cancelRetry(): void {
    clearTimeout(this.retryTimer);
    this.retryTimer = undefined;
    this.restartQueued = false;
  }

  private set(state: WatchState, error = ''): void {
    this.state = state;
    this.error = error;
    this.shared.state.fire();
  }

  /** start starts a watch of target (else the views' target), unless one runs. */
  private async start(user: boolean, target?: Resolved): Promise<void> {
    if (this.watch !== undefined && (this.state === 'starting' || this.state === 'running')) {
      return;
    }
    let t: Resolved | undefined;
    let argv: string[];
    try {
      const missing = await this.shared.missing('dotnet.helper');
      if (missing !== '') {
        this.set('failed', missing);
        return;
      }
      t = target ?? (await this.shared.resolve(user));
      if (this.disposed) {
        return;
      }
      if (t === undefined) {
        this.set('failed', 'Nothing to watch: join an eyedbg session of a .NET program, or Choose .NET Target…');
        return;
      }
      argv = watchArgs(t, client());
    } catch (e) {
      this.set('failed', errorText(asError(e)));
      if (user) {
        this.shared.state.showError(e);
      }
      return;
    }
    await this.stopWatch();
    if (this.disposed) {
      return;
    }
    // The old rows stay until the new watch's first sample (or a failure's message says why none came).
    this.fresh = true;
    this.target = t;
    this.userStarted = user;
    if (user) {
      this.autoStarts = 0;
    }
    const g = ++this.gen;
    this.set('starting');
    this.shared.record('counters', argv);
    let s: Stream;
    try {
      s = await this.shared.eyedbg.stream(argv, (line) => this.line(g, line));
    } catch (e) {
      if (g === this.gen) {
        this.set('failed', errorText(asError(e)));
        if (user) {
          this.shared.state.showError(e);
        }
      }
      return;
    }
    if (g !== this.gen) {
      // Superseded while it started (the extension is going away).
      void s.stop();
      return;
    }
    this.watch = s;
    this.shared.state.fire();
    s.done.then(
      () => this.ended(g, s, undefined),
      (e: unknown) => this.ended(g, s, e),
    );
  }

  private line(g: number, line: string): void {
    if (g !== this.gen) {
      return;
    }
    const sample = parseWatchLine(line);
    if (sample === undefined) {
      return;
    }
    if (this.fresh) {
      this.fresh = false;
      this.board.reset();
    }
    this.board.add(sample);
    this.tree.refresh();
    if (this.state === 'starting') {
      this.set('running');
    } else {
      this.shared.state.fire();
    }
  }

  /** ended handles a watch's end: on its own (the process exited), or a failure. */
  private ended(g: number, s: Stream, err: unknown): void {
    if (this.watch === s) {
      this.watch = undefined;
    }
    if (g !== this.gen || this.disposed) {
      // Stopped by this window: whoever stopped it set the state.
      this.shared.state.fire();
      return;
    }
    if (err === undefined) {
      this.set('ended');
      return;
    }
    const e = asError(err);
    const t = this.target;
    if (e.code === 'NOT_RUNNING' && t?.kind === 'session') {
      if (this.shared.joined(t.id)) {
        // D6 sees it run again: changed() starts the watch then.
        this.set('waiting');
        this.changed();
      } else {
        this.set('failed', `${t.id} is stopped at a breakpoint: continue it, then Resume`);
      }
      return;
    }
    this.set('failed', errorText(e));
    // Notified when the user's Start or Resume failed before this watch's first sample.
    if (this.userStarted && this.fresh) {
      this.shared.state.showError(e);
    }
  }

  /** stopWatch ends the running watch (its lines and end are ignored from now) and waits for its close. */
  private async stopWatch(): Promise<void> {
    this.gen++;
    const s = this.watch;
    if (s !== undefined) {
      await s.stop();
    }
  }

  private async pause(): Promise<void> {
    if (this.state !== 'starting' && this.state !== 'running' && this.state !== 'waiting') {
      return;
    }
    this.cancelRetry();
    this.set('paused');
    await this.stopWatch();
    this.shared.state.fire();
  }

  private async clear(): Promise<void> {
    this.cancelRetry();
    await this.stopWatch();
    this.board.reset();
    this.tree.refresh();
    this.set('idle');
  }

  /**
   * reset: the target changed; a watch of the old one ends. A Start that
   * asked for the target (the picker) queued this behind itself and
   * already watches the new one: that watch stays.
   */
  private async reset(): Promise<void> {
    const now = this.shared.target;
    if (now.kind !== 'auto' && this.target !== undefined && sameTarget(now, this.target)) {
      return;
    }
    this.cancelRetry();
    await this.stopWatch();
    this.board.reset();
    this.tree.refresh();
    this.target = undefined;
    this.set('idle');
  }
}
