// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What the four .NET views share (docs/adr/0015, P2-M9): the target (auto,
// a session or a process) and its picker, eyedbg's features, one operation
// per view at a time with its Cancel, the runs log for the API, and a tree
// provider over the pure rows of core/dotnetTrees.

import * as vscode from 'vscode';
import { redact } from '../../core/cli';
import { checkTargetArg, psArgs, type Resolved, type Run, sameTarget, type Target } from '../../core/dotnet';
import { parsePs } from '../../core/dotnetParse';
import { errorText, type Feature, missingFeature, pickItems, targetDescription } from '../../core/dotnetRender';
import type { Ref, Row } from '../../core/dotnetTrees';
import { EyedbgError, isRecord } from '../../core/protocol';
import { debugType } from '../config';
import type { Eyedbg } from '../exec';
import type { State } from '../state';

export type ViewName = 'counters' | 'memory' | 'threads' | 'trace';

export const viewNames: readonly ViewName[] = ['counters', 'memory', 'threads', 'trace'];

export const viewId = (v: ViewName) => `eyedbg.dotnet.${v}`;

const maxRuns = 20;

/** The API's view of an operation view (Memory, Threads, CPU Trace). */
export interface OpStats {
  busy: boolean;
  /** Results this window got (kept or not). */
  runs: number;
  /** The last failure's code ('' after a success; CANCELED is no failure). */
  lastError: string;
}

export class Shared implements vscode.Disposable {
  target: Target = { kind: 'auto' };
  /** The views are shown (a .NET workspace, or Show .NET Views). */
  show = false;
  /** The target picker is open. */
  picking = false;
  /** The last 20 command lines, redacted, for the API. */
  readonly runs: { view: ViewName | 'target'; argv: string[] }[] = [];
  private readonly targetChanged = new vscode.EventEmitter<void>();
  private readonly contexts = new Map<string, unknown>();
  /** onDidChangeTarget fires when the chosen target changes (not when auto follows another session). */
  readonly onDidChangeTarget = this.targetChanged.event;

  constructor(
    readonly state: State,
    readonly eyedbg: Eyedbg,
  ) {}

  dispose(): void {
    this.targetChanged.dispose();
  }

  /** setContext sets a context key when its value changes. */
  setContext(key: string, value: unknown): void {
    if (this.contexts.has(key) && this.contexts.get(key) === value) {
      return;
    }
    this.contexts.set(key, value);
    void vscode.commands.executeCommand('setContext', key, value);
  }

  /** record logs a command line a view runs. */
  record(view: ViewName | 'target', argv: string[]): void {
    this.runs.push({ view, argv: redact(argv) });
    if (this.runs.length > maxRuns) {
      this.runs.splice(0, this.runs.length - maxRuns);
    }
  }

  /** auto is what the auto target follows now: the active eyedbg debug session, else the only one. */
  auto(): Resolved | undefined {
    const active = vscode.debug.activeDebugSession;
    if (active?.type === debugType) {
      const t = this.state.get(active.id);
      if (t !== undefined) {
        return { kind: 'session', id: t.eyedbgId };
      }
    }
    const ids = [...new Set(this.state.all().map((t) => t.eyedbgId))];
    return ids.length === 1 && ids[0] !== undefined ? { kind: 'session', id: ids[0] } : undefined;
  }

  description(): string {
    return targetDescription(this.target, this.target.kind === 'auto' ? this.auto() : undefined);
  }

  /** joined reports whether this window has joined session id. */
  joined(id: string): boolean {
    return this.state.all().some((t) => t.eyedbgId === id);
  }

  /** stopped reports whether a joined session's program is stopped (D6); false if not known. */
  stopped(id: string): boolean {
    const ts = this.state.all().filter((t) => t.eyedbgId === id);
    return ts.length > 0 && ts.every((t) => t.running === false);
  }

  /** runningNow reports whether a joined session's program is known to run. */
  runningNow(id: string): boolean {
    return this.state.all().some((t) => t.eyedbgId === id && t.running === true);
  }

  setTarget(t: Target): void {
    if (sameTarget(t, this.target)) {
      return;
    }
    this.target = t;
    this.state.log.info(`.NET views: target ${this.description()}`);
    this.targetChanged.fire();
    this.state.fire();
  }

  /**
   * resolve is the target to run against: the chosen one, else what auto
   * follows, else (interactive) the one picked; undefined: none.
   */
  async resolve(interactive: boolean): Promise<Resolved | undefined> {
    if (this.target.kind !== 'auto') {
      return this.target;
    }
    const auto = this.auto();
    if (auto !== undefined || !interactive) {
      return auto;
    }
    await this.choose();
    return this.target.kind === 'auto' ? this.auto() : this.target;
  }

  /** chooseCommand is eyedbg.dotnet.chooseTarget: {auto}, {session} or {pid} (validated), else the picker. */
  async chooseCommand(arg?: unknown): Promise<boolean> {
    if (arg === undefined) {
      return this.choose();
    }
    const r = checkTargetArg(arg);
    if (!r.ok) {
      void this.state.show('error', `EyeDebugger: ${r.error}`);
      return false;
    }
    this.setTarget(r.value);
    return true;
  }

  /** choose shows the picker: auto, then 'eyedbg dotnet ps'. */
  private async choose(): Promise<boolean> {
    const qp = vscode.window.createQuickPick<vscode.QuickPickItem & { target: Target }>();
    qp.title = 'EyeDebugger: .NET target';
    qp.placeholder = 'The process the .NET views inspect (they never start one)';
    qp.busy = true;
    qp.matchOnDescription = true;
    qp.items = pickItems([], new Set());
    const cancel = new AbortController();
    const picked = new Promise<Target | undefined>((resolve) => {
      qp.onDidAccept(() => resolve(qp.selectedItems[0]?.target));
      qp.onDidHide(() => resolve(undefined));
    });
    qp.show();
    this.picking = true;
    this.state.fire();
    void this.listProcesses(qp, cancel.signal).finally(() => {
      qp.busy = false;
    });
    const t = await picked;
    cancel.abort();
    qp.dispose();
    this.picking = false;
    this.state.fire();
    if (t === undefined) {
      return false;
    }
    this.setTarget(t);
    return true;
  }

  /** listProcesses fills the picker from 'eyedbg dotnet ps' (it needs dotnet.helper, D13). */
  private async listProcesses(
    qp: vscode.QuickPick<vscode.QuickPickItem & { target: Target }>,
    signal: AbortSignal,
  ): Promise<void> {
    try {
      const missing = await this.missing('dotnet.helper');
      if (missing !== '') {
        qp.placeholder = missing;
        return;
      }
      const run = psArgs();
      this.record('target', run.argv);
      const v = await this.eyedbg.run(run.argv, { timeoutMs: run.timeoutMs, signal, interrupt: true });
      qp.items = pickItems(parsePs(v), new Set(this.state.all().map((t) => t.eyedbgId)));
    } catch (e) {
      if (!(e instanceof EyedbgError && e.code === 'CANCELED')) {
        qp.placeholder = errorText(asError(e));
      }
    }
  }

  /** missing is the message of a view whose eyedbg lacks feature ('' when it has it). */
  async missing(feature: Feature): Promise<string> {
    const b = await this.eyedbg.check();
    return b.features.includes(feature) ? '' : missingFeature(b.version, feature);
  }
}

export function asError(e: unknown): EyedbgError {
  return e instanceof EyedbgError ? e : new EyedbgError('INTERNAL', e instanceof Error ? e.message : String(e));
}

/**
 * Op is a view's one operation at a time: its abort controller, the busy
 * context key (the title bar's Cancel), and the API's stats.
 */
export class Op {
  private controller: AbortController | undefined;
  runs = 0;
  lastError = '';

  constructor(
    private readonly view: ViewName,
    private readonly shared: Shared,
  ) {}

  get busy(): boolean {
    return this.controller !== undefined;
  }

  /** begin starts the operation: its signal, or undefined when one is running. */
  begin(): AbortSignal | undefined {
    if (this.controller !== undefined) {
      return undefined;
    }
    this.controller = new AbortController();
    this.shared.setContext(`eyedbg.dotnet.${this.view}.busy`, true);
    this.shared.state.fire();
    return this.controller.signal;
  }

  end(): void {
    this.controller = undefined;
    this.shared.setContext(`eyedbg.dotnet.${this.view}.busy`, false);
    this.shared.state.fire();
  }

  cancel(): void {
    this.controller?.abort();
  }

  stats(): OpStats {
    return { busy: this.busy, runs: this.runs, lastError: this.lastError };
  }
}

/**
 * RowTree is a view over rows built whole after each change of its data
 * (on the next read): getChildren and getTreeItem only read them (the API
 * walks every node), and a change redraws once the current burst is over
 * (50 ms). A build is linear in the rows: at most 3 results, each capped
 * by the CLI's own limits (types 50, threads × 20 frames).
 */
export class RowTree implements vscode.TreeDataProvider<Row>, vscode.Disposable {
  private readonly changed = new vscode.EventEmitter<Row | undefined>();
  readonly onDidChangeTreeData = this.changed.event;
  readonly view: vscode.TreeView<Row>;
  private roots: Row[] = [];
  private dirty = true;
  private timer: NodeJS.Timeout | undefined;

  constructor(
    id: string,
    private readonly build: () => Row[],
    private readonly decorate: (row: Row, item: vscode.TreeItem) => void = () => {},
  ) {
    this.view = vscode.window.createTreeView(id, { treeDataProvider: this });
  }

  dispose(): void {
    clearTimeout(this.timer);
    this.timer = undefined;
    this.view.dispose();
    this.changed.dispose();
  }

  /** refresh marks the rows stale (rebuilt on the next read) and redraws the view soon. */
  refresh(): void {
    this.dirty = true;
    if (this.timer !== undefined) {
      return;
    }
    this.timer = setTimeout(() => {
      this.timer = undefined;
      this.changed.fire(undefined);
    }, 50);
  }

  getChildren(row?: Row): Row[] {
    if (row !== undefined) {
      return row.children;
    }
    if (this.dirty) {
      this.roots = this.build();
      this.dirty = false;
    }
    return this.roots;
  }

  getTreeItem(row: Row): vscode.TreeItem {
    const item = new vscode.TreeItem(
      row.label,
      row.expanded === undefined
        ? vscode.TreeItemCollapsibleState.None
        : row.expanded
          ? vscode.TreeItemCollapsibleState.Expanded
          : vscode.TreeItemCollapsibleState.Collapsed,
    );
    item.id = row.id;
    item.description = row.description;
    if (row.tooltip !== '') {
      item.tooltip = row.tooltip;
    }
    if (row.icon !== '') {
      item.iconPath = new vscode.ThemeIcon(row.icon);
    }
    if (row.context !== '') {
      item.contextValue = row.context;
    }
    this.decorate(row, item);
    return item;
  }
}

/**
 * OpView is a view whose actions are single runs (Memory, Threads, CPU
 * Trace): one at a time (Op), its failures as the view's message and, for
 * what the user started, one notification. A cancel is no failure.
 */
export abstract class OpView implements vscode.Disposable {
  readonly op: Op;
  readonly tree: RowTree;
  protected readonly subs: vscode.Disposable[] = [];
  private error = '';

  constructor(
    protected readonly shared: Shared,
    readonly view: ViewName,
    build: () => Row[],
    decorate?: (row: Row, item: vscode.TreeItem) => void,
  ) {
    this.op = new Op(view, shared);
    this.tree = new RowTree(viewId(view), build, decorate);
    this.subs.push(
      this.tree,
      vscode.commands.registerCommand(`eyedbg.dotnet.${view}.cancel`, () => this.op.cancel()),
      shared.state.onDidChange(() => this.changed()),
    );
    this.changed();
  }

  dispose(): void {
    this.op.cancel();
    for (const s of this.subs) {
      s.dispose();
    }
  }

  /** changed updates the view's message and description. */
  private changed(): void {
    this.tree.view.message = this.error;
    this.tree.view.description = this.shared.description();
  }

  /** run runs a command of this view's operation (interruptible). */
  protected async run(r: Run, signal: AbortSignal): Promise<Record<string, unknown>> {
    this.shared.record(this.view, r.argv);
    return this.shared.eyedbg.run(r.argv, { timeoutMs: r.timeoutMs, signal, interrupt: true });
  }

  /** ready checks eyedbg has feature, else says so in the view; false if not. */
  protected async ready(feature: Feature, user: boolean): Promise<boolean> {
    try {
      const missing = await this.shared.missing(feature);
      if (missing === '') {
        return true;
      }
      this.fail(new EyedbgError('VERSION_MISMATCH', missing), user, missing);
    } catch (e) {
      this.fail(e, user);
    }
    return false;
  }

  protected succeeded(): void {
    this.op.runs++;
    this.op.lastError = '';
    this.error = '';
    this.tree.refresh();
    this.shared.state.fire();
  }

  /** fail shows a failure (text: the message, else the error's); a cancel clears the message only. */
  protected fail(e: unknown, user: boolean, text?: string): void {
    const err = asError(e);
    if (err.code === 'CANCELED') {
      this.error = '';
      this.shared.state.log.info(`${this.view}: canceled`);
    } else {
      this.op.lastError = err.code;
      this.error = text ?? errorText(err);
      if (user) {
        this.shared.state.showError(err);
      } else {
        this.shared.state.log.warn(`${this.view}: ${this.error}`);
      }
    }
    this.shared.state.fire();
  }

  /** begin starts this view's operation, or says it is busy (a welcome button stays clickable). */
  protected begin(): AbortSignal | undefined {
    const signal = this.op.begin();
    if (signal === undefined) {
      void this.shared.state.show('info', 'This .NET view is busy: cancel its operation or wait for it.');
    }
    return signal;
  }

  /** noTarget says there is nothing to act on. */
  protected noTarget(what: string): void {
    this.error = `Nothing to ${what}: join an eyedbg session of a .NET program, or Choose .NET Target…`;
    this.shared.state.fire();
  }
}

/** refOf reads a command's node argument: a row (or its ref) of kind, with the numeric fields keys. */
export function refOf<K extends Ref['kind']>(x: unknown, kind: K): Extract<Ref, { kind: K }> | undefined {
  const r = isRecord(x) && isRecord(x.ref) ? x.ref : x;
  if (!isRecord(r) || r.kind !== kind) {
    return undefined;
  }
  for (const [k, v] of Object.entries(r)) {
    if (k !== 'kind' && !(Number.isSafeInteger(v) && (v as number) >= 0)) {
      return undefined;
    }
  }
  return r as Extract<Ref, { kind: K }>;
}
