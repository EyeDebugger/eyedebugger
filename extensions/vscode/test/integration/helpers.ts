// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What the integration tests share: the eyedbg CLI as the agent (and for
// checks), a recorder of every DAP message VS Code exchanges with eyedbg's
// facade, event-driven waits with deadlines, and the extension's API.

import { execFile, spawn } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import * as vscode from 'vscode';
import type { EyedbgApi, TreeSnapshot } from '../../src/vscode/api';
import type { Ctx } from './harness';

function env(name: string): string {
  const v = process.env[name];
  if (v === undefined || v === '') {
    throw new Error(`${name} is not set: run the suite through test/integration/runner.ts`);
  }
  return v;
}

export const eyedbgPath = env('EYEDBG_TEST_EYEDBG');
export const ws = env('EYEDBG_TEST_WS');
export const wsLink = env('EYEDBG_TEST_WS_LINK');
export const app = path.join(ws, 'app.py');
export const appLink = path.join(wsLink, 'app.py');
export const self = 'human:test';

/** samePath compares paths as the platform does (Windows: case-insensitively, as VS Code's c: and Go's C:). */
export function samePath(a: unknown, b: string): boolean {
  if (typeof a !== 'string') {
    return false;
  }
  return process.platform === 'win32' ? a.toLowerCase() === b.toLowerCase() : a === b;
}

/** markers are app.py's lines by their "# marker: NAME" comment. */
export const marker: Record<string, number> = (() => {
  const out: Record<string, number> = {};
  fs.readFileSync(app, 'utf8')
    .split(/\r?\n/)
    .forEach((line, i) => {
      const m = /# marker: ([a-z-]+)/.exec(line);
      if (m?.[1] !== undefined) {
        out[m[1]] = i + 1;
      }
    });
  return out;
})();

export function line(name: string): number {
  const l = marker[name];
  if (l === undefined) {
    throw new Error(`app.py has no marker ${name}`);
  }
  return l;
}

// biome-ignore lint/suspicious/noExplicitAny: CLI JSON is checked field by field in the tests.
export type Json = any;

export interface CliResult {
  code: number;
  stdout: string;
  stderr: string;
  json: Json;
}

/** cli runs eyedbg (as the agent unless --as is given) in the workspace; it rejects on a non-zero exit unless allowFail. */
export function cli(args: string[], opts: { allowFail?: boolean } = {}): Promise<CliResult> {
  const argv = args.some((a) => a.startsWith('--as=')) ? args : [...args, '--as=agent'];
  return new Promise((resolve, reject) => {
    execFile(
      eyedbgPath,
      argv,
      { cwd: ws, encoding: 'utf8', timeout: 120_000, maxBuffer: 16 << 20, windowsHide: true },
      (err, stdout, stderr) => {
        const code = err === null ? 0 : typeof err.code === 'number' ? err.code : -1;
        let json: Json;
        try {
          json = JSON.parse(stdout);
        } catch {
          json = undefined;
        }
        const r = { code, stdout, stderr, json };
        if (code !== 0 && opts.allowFail !== true) {
          reject(new Error(`eyedbg ${argv.join(' ')}: exit ${code}\n${stdout}${stderr}`));
          return;
        }
        resolve(r);
      },
    );
  });
}

/** latestSeq is the session's newest event sequence number. */
export async function latestSeq(session: string): Promise<number> {
  return (await cli(['events', `--session=${session}`, '--limit=1', '--json'])).json.latest as number;
}

/** waitEvents waits (eyedbg events --wait) for events of kinds after since; returns them. */
export async function waitEvents(session: string, kinds: string, since: number, pred: (e: Json) => boolean) {
  let after = since;
  for (;;) {
    const r = await cli([
      'events',
      `--session=${session}`,
      `--kind=${kinds}`,
      `--since=${after}`,
      '--wait',
      '--timeout=60s',
      '--json',
    ]);
    const evs: Json[] = r.json.events ?? [];
    if (evs.length === 0) {
      throw new Error(`no ${kinds} event after ${since} in ${session} within 60 s`);
    }
    const hit = evs.find(pred);
    if (hit !== undefined) {
      return hit;
    }
    after = evs[evs.length - 1].seq;
  }
}

/** until resolves with check()'s first truthy value, re-checked on each event; rejects after ms. */
export function until<T>(
  what: string,
  check: () => T | undefined | false,
  events: vscode.Event<unknown>[],
  ms = 60_000,
) {
  return new Promise<T>((resolve, reject) => {
    const subs: vscode.Disposable[] = [];
    let done = false;
    const finish = (fn: () => void) => {
      if (!done) {
        done = true;
        clearTimeout(timer);
        for (const s of subs) {
          s.dispose();
        }
        fn();
      }
    };
    const timer = setTimeout(
      () => finish(() => reject(new Error(`timed out after ${ms / 1000} s waiting for ${what}`))),
      ms,
    );
    const attempt = () => {
      try {
        const v = check();
        if (v !== undefined && v !== false) {
          finish(() => resolve(v));
        }
      } catch (e) {
        finish(() => reject(e));
      }
    };
    for (const ev of events) {
      subs.push(ev(attempt));
    }
    attempt();
  });
}

export async function extensionApi(): Promise<EyedbgApi> {
  const ext = vscode.extensions.getExtension<EyedbgApi>('eyedebugger.eyedbg');
  if (ext === undefined) {
    throw new Error('the extension eyedebugger.eyedbg is not loaded');
  }
  return ext.activate();
}

// --- DAP recorder ---

export interface Rec {
  /** out: VS Code → eyedbg; in: eyedbg → VS Code. */
  dir: 'out' | 'in';
  m: Json;
}

/** Conn is one adapter connection of a debug session (a Restart makes a new one). */
export interface Conn {
  session: vscode.DebugSession;
  /** The eyedbg session: an attach's configured one; a launch's from its eyedbg/session event ('' until then). */
  eyedbgSession: string;
  msgs: Rec[];
  exited: boolean;
}

export class Recorder implements vscode.Disposable {
  readonly conns: Conn[] = [];
  private readonly emitter = new vscode.EventEmitter<void>();
  readonly onDidRecord = this.emitter.event;
  /** Debug sessions whose last start/terminate event was a terminate (a Restart starts the same id again). */
  private readonly ended = new Set<string>();
  private readonly subs: vscode.Disposable[] = [];

  constructor() {
    this.subs.push(
      vscode.debug.onDidTerminateDebugSession((s) => {
        this.ended.add(s.id);
        this.emitter.fire();
      }),
      vscode.debug.onDidStartDebugSession((s) => {
        this.ended.delete(s.id);
        this.emitter.fire();
      }),
    );
    this.subs.push(this.trackers());
  }

  /** isEnded reports whether VS Code's debug session id has ended (and not restarted). */
  isEnded(id: string): boolean {
    return this.ended.has(id);
  }

  /** live are the debug sessions the recorder saw that haven't ended, one per id. */
  live(eyedbgSession?: string): vscode.DebugSession[] {
    const out = new Map<string, vscode.DebugSession>();
    for (const c of this.conns) {
      if (!this.ended.has(c.session.id) && (eyedbgSession === undefined || c.eyedbgSession === eyedbgSession)) {
        out.set(c.session.id, c.session);
      }
    }
    return [...out.values()];
  }

  private trackers(): vscode.Disposable {
    return vscode.debug.registerDebugAdapterTrackerFactory('eyedbg', {
      createDebugAdapterTracker: (session) => {
        const launch = session.configuration.request === 'launch';
        const conn: Conn = {
          session,
          eyedbgSession: launch ? '' : String(session.configuration.session),
          msgs: [],
          exited: false,
        };
        this.conns.push(conn);
        this.emitter.fire();
        return {
          onWillReceiveMessage: (m: unknown) => {
            conn.msgs.push({ dir: 'out', m: structuredClone(m) });
            this.emitter.fire();
          },
          onDidSendMessage: (m: unknown) => {
            const msg: Json = structuredClone(m);
            if (launch && conn.eyedbgSession === '' && msg?.type === 'event' && msg.event === 'eyedbg/session') {
              conn.eyedbgSession = String(msg.body?.sessionId);
            }
            conn.msgs.push({ dir: 'in', m: msg });
            this.emitter.fire();
          },
          onExit: () => {
            conn.exited = true;
            this.emitter.fire();
          },
        };
      },
    });
  }

  /** conn waits for the n-th (from 0) adapter connection of eyedbg session id. */
  conn(id: string, n = 0): Promise<Conn> {
    return until(`adapter connection ${n} of ${id}`, () => this.conns.filter((c) => c.eyedbgSession === id)[n], [
      this.onDidRecord,
    ]);
  }

  /** find waits for a message of conn matching pred at index from or later; returns its index and the message. */
  find(conn: Conn, what: string, pred: (r: Rec) => boolean, from = 0, ms = 60_000): Promise<{ i: number; m: Json }> {
    return until(
      what,
      () => {
        for (let i = from; i < conn.msgs.length; i++) {
          const r = conn.msgs[i];
          if (r !== undefined && pred(r)) {
            return { i, m: r.m };
          }
        }
        return undefined;
      },
      [this.onDidRecord],
      ms,
    );
  }

  /** response waits for the response to the request at index i. */
  response(conn: Conn, i: number): Promise<{ i: number; m: Json }> {
    const seq = conn.msgs[i]?.m.seq;
    return this.find(
      conn,
      `the response to request ${seq}`,
      (r) => r.dir === 'in' && r.m.type === 'response' && r.m.request_seq === seq,
      i,
    );
  }

  dispose(): void {
    for (const s of this.subs) {
      s.dispose();
    }
    this.emitter.dispose();
  }
}

/** trace is conn's breakpoint traffic, one line per message, for failure messages. */
export function trace(conn: Conn): string {
  const out: string[] = [];
  conn.msgs.forEach((r, i) => {
    const m = r.m;
    if (m.type === 'event' && m.event === 'breakpoint') {
      out.push(`${i} ${r.dir} breakpoint ${m.body.reason} ${JSON.stringify(m.body.breakpoint)}`);
    } else if (m.command === 'setBreakpoints') {
      out.push(
        `${i} ${r.dir} ${m.type} ${m.seq}/${m.request_seq ?? ''} ${JSON.stringify(m.type === 'request' ? m.arguments : m.body)}`,
      );
    } else if (m.type === 'request' || m.type === 'event') {
      out.push(`${i} ${r.dir} ${m.type} ${m.command ?? m.event}`);
    }
  });
  return out.join('\n');
}

let recorder: Recorder | undefined;

/** rec is the suite's recorder (created before the first test). */
export function rec(): Recorder {
  recorder ??= new Recorder();
  return recorder;
}

export const isEvent = (name: string) => (r: Rec) => r.dir === 'in' && r.m.type === 'event' && r.m.event === name;

export const isBpEvent = (reason: string, id: number) => (r: Rec) =>
  r.dir === 'in' &&
  r.m.type === 'event' &&
  r.m.event === 'breakpoint' &&
  r.m.body?.reason === reason &&
  r.m.body?.breakpoint?.id === id;

export const isSetBps = (file: string) => (r: Rec) =>
  r.dir === 'out' &&
  r.m.type === 'request' &&
  r.m.command === 'setBreakpoints' &&
  samePath(r.m.arguments?.source?.path, file);

/**
 * synced returns once VS Code's workbench has applied every message the
 * adapter sent so far. The tests see DAP messages in the extension host
 * (trackers) before the workbench processes them: its queue dispatches
 * them one task apart (abstractDebugAdapter.ts processQueue), so acting on
 * VS Code's breakpoints right after seeing a 'breakpoint new' can overtake
 * it. A request's response is queued behind every earlier message, so its
 * round trip is the barrier; eyedbg/clients changes nothing.
 */
export async function synced(conn: Conn): Promise<void> {
  await conn.session.customRequest('eyedbg/clients', {});
}

// --- Sessions ---

/** bpAdd adds a breakpoint as the agent and returns it. */
export async function bpAdd(id: string, where: string, ...flags: string[]): Promise<Json> {
  const r = await cli(['bp', 'add', where, ...flags, `--session=${id}`, '--json']);
  const bp = (r.json.breakpoints as Json[])[0];
  if (bp === undefined) {
    throw new Error(`bp add ${where}: no breakpoint in ${r.stdout}`);
  }
  return bp;
}

/** startAgent starts app.py as the agent (breakpoints set before it runs) and returns the session id. */
export async function startAgent(ctx: Ctx, extra: string[] = [], mode = 'loop'): Promise<string> {
  const r = await cli(['start', 'python', `--program=${app}`, ...extra, '--timeout=60s', '--json', '--', mode]);
  const id: string = r.json.session.id;
  ctx.log(`agent started ${id} (${r.json.session.state})`);
  ctx.cleanup(() => stopSession(id));
  return id;
}

/** stopSession ends a session whoever holds its lease (the test's cleanup). */
export async function stopSession(id: string): Promise<void> {
  // Leave first: stopping a session VS Code is attached to ends VS Code's
  // debug session on its own, racing a later leave.
  for (const s of rec().live(id)) {
    await leave(s);
  }
  const list = (await cli(['sessions', '--json'])).json.sessions as Json[];
  const s = list.find((x) => x.id === id);
  if (s === undefined || s.state === 'lost') {
    return;
  }
  if (s.state !== 'exited') {
    await cli(['lease', 'take', '--force', `--session=${id}`], { allowFail: true });
  }
  await cli(['stop', `--session=${id}`, '--json'], { allowFail: true });
}

export async function addBreakpoint(file: string, l: number, opts: Partial<vscode.SourceBreakpoint> = {}) {
  const bp = new vscode.SourceBreakpoint(
    new vscode.Location(vscode.Uri.file(file), new vscode.Position(l - 1, 0)),
    true,
    opts.condition,
    opts.hitCondition,
    opts.logMessage,
  );
  vscode.debug.addBreakpoints([bp]);
  return bp;
}

/** join attaches VS Code to session id and waits for its first stop; returns the connection. */
export async function join(rec: Recorder, id: string, n = 0): Promise<Conn> {
  const folder = vscode.workspace.workspaceFolders?.[0];
  const ok = await vscode.debug.startDebugging(folder, {
    type: 'eyedbg',
    request: 'attach',
    name: `join ${id}`,
    session: id,
  });
  if (!ok) {
    const notices = (await extensionApi()).notices().slice(-3);
    throw new Error(`startDebugging for ${id} returned false; last notices: ${JSON.stringify(notices)}`);
  }
  const conn = await rec.conn(id, n);
  await rec.find(conn, `the first stop of ${id}`, isEvent('stopped'));
  return conn;
}

/** leave stops VS Code's debug session and waits until it ended. */
export async function leave(session: vscode.DebugSession): Promise<void> {
  if (rec().isEnded(session.id)) {
    return;
  }
  // Subscribed before stopping, and re-checked on every event: an end
  // that happens first (the session stopped meanwhile) is never missed.
  const ended = until(`the end of debug session ${session.name}`, () => rec().isEnded(session.id), [rec().onDidRecord]);
  await vscode.debug.stopDebugging(session);
  await ended;
}

/** endAll ends every debug session and removes every breakpoint (VS Code's copies of mirrors included). */
export async function endAll(): Promise<void> {
  for (const s of rec().live()) {
    await leave(s);
  }
  await vscode.commands.executeCommand('workbench.debug.viewlet.action.removeAllBreakpoints');
  await vscode.commands.executeCommand('workbench.debug.viewlet.action.enableAllBreakpoints');
}

export async function bps(id: string): Promise<Json[]> {
  return (await cli(['bp', 'ls', `--session=${id}`, '--json'])).json.breakpoints as Json[];
}

export async function stackTop(session: vscode.DebugSession, threadId: number): Promise<number> {
  const st = await session.customRequest('stackTrace', { threadId, startFrame: 0, levels: 1 });
  return st.stackFrames[0].line as number;
}

// --- Settings and views ---

/** setting sets an eyedbg setting in the user settings for the test, and restores it after. */
export async function setting(ctx: Ctx, key: string, value: unknown): Promise<void> {
  const cfg = () => vscode.workspace.getConfiguration();
  const before = cfg().inspect(key)?.globalValue;
  const apply = async (v: unknown) => {
    await cfg().update(key, v, vscode.ConfigurationTarget.Global);
    const want = JSON.stringify(v === undefined ? cfg().inspect(key)?.defaultValue : v);
    await until(`${key} = ${want}`, () => JSON.stringify(cfg().get(key)) === want, [
      vscode.workspace.onDidChangeConfiguration,
    ]);
  };
  ctx.cleanup(() => apply(before));
  await apply(value);
}

/** item finds the first item of a view (depth first) matching pred. */
export function item(items: TreeSnapshot[], pred: (i: TreeSnapshot) => boolean): TreeSnapshot | undefined {
  for (const i of items) {
    if (pred(i)) {
      return i;
    }
    const c = item(i.children, pred);
    if (c !== undefined) {
      return c;
    }
  }
  return undefined;
}

/** shownAt reports whether a visible editor shows file with the cursor on line (1-based). */
export function shownAt(file: string, l: number): boolean {
  return vscode.window.visibleTextEditors.some(
    (e) => samePath(e.document.uri.fsPath, file) && e.selection.active.line === l - 1,
  );
}

/** editorEvents are the events after which shownAt may change. */
export const editorEvents: vscode.Event<unknown>[] = [
  vscode.window.onDidChangeVisibleTextEditors,
  vscode.window.onDidChangeActiveTextEditor,
  vscode.window.onDidChangeTextEditorSelection,
];

// --- .NET (breadth, the runner's build of testdata/apps/dotnet/breadth) ---

/** dotnetTests: the runner built breadth and checked dotnet, the helper and netcoredbg (EYEDBG_TEST_DOTNET != 0). */
export const dotnetTests = process.env.EYEDBG_TEST_DOTNET === '1';
export const breadth = process.env.EYEDBG_TEST_BREADTH ?? '';
export const breadthSrc = process.env.EYEDBG_TEST_BREADTH_SRC ?? '';
const dotnetHost = process.env.EYEDBG_TEST_DOTNET_HOST ?? '';
/** The run's EYEDBG_HOME: dumps and traces land in it. */
export const eyedbgHome = process.env.EYEDBG_HOME ?? '';

/** skipDotnet logs a skip and reports true when the .NET tests are off. */
export function skipDotnet(ctx: Ctx, name: string): boolean {
  if (!dotnetTests) {
    ctx.log(`SKIP ${name}: EYEDBG_TEST_DOTNET=0`);
  }
  return !dotnetTests;
}

/** breadthLine is the line of breadth's source file with "// marker: NAME". */
export function breadthLine(file: string, name: string): number {
  const lines = fs.readFileSync(path.join(breadthSrc, file), 'utf8').split(/\r?\n/);
  const i = lines.findIndex((l) => l.includes(`// marker: ${name}`));
  if (i < 0) {
    throw new Error(`${file} has no marker ${name}`);
  }
  return i + 1;
}

let markers = 0;

/** marker is a new path whose creation ends a breadth scenario (in the run's temporary root). */
function newMarker(): string {
  return path.join(path.dirname(ws), `mk-${process.pid}-${++markers}`);
}

/**
 * startDotnetAgent starts breadth SCENARIO as the agent under netcoredbg
 * (breakpoints first) and waits for its 'ready PID'; the cleanup creates
 * its marker (the program ends) and stops the session.
 */
export async function startDotnetAgent(
  ctx: Ctx,
  scenario: string,
  bps: string[] = [],
): Promise<{ id: string; pid: number; marker: string }> {
  const marker = newMarker();
  const r = await cli([
    'start',
    'dotnet',
    `--program=${breadth}`,
    ...bps.map((b) => `--bp=${b}`),
    '--timeout=90s',
    '--json',
    '--',
    scenario,
    marker,
  ]);
  const id: string = r.json.session.id;
  ctx.cleanup(async () => {
    fs.writeFileSync(marker, '');
    await stopSession(id);
  });
  const ready = await waitEvents(id, 'output', 0, (e) => /^ready \d+/.test(e.text ?? ''));
  const pid = Number(/^ready (\d+)/.exec(ready.text)?.[1]);
  ctx.log(`agent started ${id}: breadth ${scenario}, pid ${pid}`);
  return { id, pid, marker };
}

/**
 * startBareBreadth runs breadth SCENARIO without a debugger and waits for
 * its 'ready PID'; the cleanup creates its marker and waits for its exit
 * (killing it after 20 s).
 */
export async function startBareBreadth(
  ctx: Ctx,
  scenario: string,
): Promise<{ pid: number; exited: Promise<void>; end: () => void }> {
  const marker = newMarker();
  const child = spawn(dotnetHost, [breadth, scenario, marker], {
    cwd: breadthSrc,
    stdio: ['ignore', 'pipe', 'inherit'],
    windowsHide: true,
    env: { ...process.env, DOTNET_CLI_TELEMETRY_OPTOUT: '1', DOTNET_NOLOGO: '1' },
  });
  const exited = new Promise<void>((resolve) => child.once('exit', () => resolve()));
  ctx.cleanup(async () => {
    fs.writeFileSync(marker, '');
    let timer: NodeJS.Timeout | undefined;
    await Promise.race([exited, new Promise<void>((resolve) => (timer = setTimeout(resolve, 20_000)))]);
    clearTimeout(timer);
    if (child.exitCode === null && child.signalCode === null) {
      ctx.log(`breadth ${scenario} (pid ${child.pid}) didn't exit 20 s after its marker: killing it`);
      child.kill('SIGKILL');
      await exited;
    }
  });
  const pid = await new Promise<number>((resolve, reject) => {
    let out = '';
    const timer = setTimeout(() => reject(new Error(`breadth ${scenario} printed no 'ready' in 60 s: ${out}`)), 60_000);
    child.stdout?.setEncoding('utf8');
    child.stdout?.on('data', (s: string) => {
      out += s;
      const m = /ready (\d+)/.exec(out);
      if (m?.[1] !== undefined) {
        clearTimeout(timer);
        resolve(Number(m[1]));
      }
    });
    child.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`breadth ${scenario} exited (${code}) before it was ready: ${out}`));
    });
  });
  ctx.log(`bare breadth ${scenario}: pid ${pid}`);
  return { pid, exited, end: () => fs.writeFileSync(marker, '') };
}

/** joinRunning joins a running session (no first stop to wait for): once VS Code's configurationDone is answered. */
export async function joinRunning(rec: Recorder, id: string): Promise<Conn> {
  const folder = vscode.workspace.workspaceFolders?.[0];
  const ok = await vscode.debug.startDebugging(folder, {
    type: 'eyedbg',
    request: 'attach',
    name: `join ${id}`,
    session: id,
  });
  if (!ok) {
    throw new Error(`startDebugging for ${id} returned false`);
  }
  const conn = await rec.conn(id);
  await rec.find(
    conn,
    `configurationDone answered for ${id}`,
    (r) => r.dir === 'in' && r.m.type === 'response' && r.m.command === 'configurationDone',
  );
  return conn;
}

/** files lists a directory's files ([] if it doesn't exist). */
export function files(dir: string): string[] {
  try {
    return fs.readdirSync(dir).sort();
  } catch {
    return [];
  }
}

/** underDir reports whether p is inside dir (real paths; Windows: case-insensitively). */
export function underDir(p: string, dir: string): boolean {
  const real = (x: string) => fs.realpathSync.native(x);
  const rel = path.relative(real(dir), real(p));
  return rel !== '' && !rel.startsWith('..') && !path.isAbsolute(rel);
}

/** watchDir is an event for any change under dir (recursive; the directory must exist). */
export function watchDir(dir: string): vscode.Disposable & { event: vscode.Event<void> } {
  const emitter = new vscode.EventEmitter<void>();
  const w = fs.watch(dir, { recursive: true }, () => emitter.fire());
  return {
    event: emitter.event,
    dispose: () => {
      w.close();
      emitter.dispose();
    },
  };
}
