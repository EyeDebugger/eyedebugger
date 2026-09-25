// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The .NET views' side of the eyedbg CLI (docs/adr/0015, P2-M9): the
// 'eyedbg dotnet …' command lines and the small state machines the views
// keep (their --json readers are in dotnetParse). Every flag value is bound
// as --flag=value and every path follows "--": no value can become a flag.

import { type CounterValue, isPid, type Sample } from './dotnetParse';
import { type DapMessage, EyedbgError, isRecord } from './protocol';
import { hasNul, isSessionId, type Result } from './validate';

// --- Targets ---

export interface SessionTarget {
  kind: 'session';
  id: string;
}

export interface PidTarget {
  kind: 'pid';
  pid: number;
  /** The process name 'eyedbg dotnet ps' reported ('' if unknown); raw: render it as plain text. */
  name: string;
}

/** A resolved target: a session (the CLI checks it's running and alive) or a process. */
export type Resolved = SessionTarget | PidTarget;

/** The views' target: auto follows the active eyedbg debug session. */
export type Target = { kind: 'auto' } | Resolved;

/**
 * targetArgs are the CLI's target flags. A session target also names the
 * client: the daemon records whoever asks about a session as active, and
 * the default client is the agent.
 */
export function targetArgs(t: Resolved, client: string): string[] {
  return t.kind === 'session' ? [`--session=${t.id}`, `--as=${client}`] : [`--pid=${t.pid}`];
}

/** checkTargetArg reads a command's target argument: {auto: true}, {session} or {pid[, name]}. */
export function checkTargetArg(x: unknown): Result<Target> {
  const shape = 'a target is {auto: true}, {session: "s-…"} or {pid: N}';
  if (!isRecord(x)) {
    return { ok: false, error: shape };
  }
  const given = ['auto', 'session', 'pid'].filter((k) => x[k] !== undefined);
  if (given.length !== 1) {
    return { ok: false, error: shape };
  }
  if (x.auto !== undefined) {
    return x.auto === true ? { ok: true, value: { kind: 'auto' } } : { ok: false, error: shape };
  }
  if (x.session !== undefined) {
    return isSessionId(x.session)
      ? { ok: true, value: { kind: 'session', id: x.session } }
      : { ok: false, error: '"session" must be an eyedbg session id like s-7f3k' };
  }
  if (!isPid(x.pid)) {
    return { ok: false, error: '"pid" must be a whole number from 1 to 2147483647' };
  }
  const name = typeof x.name === 'string' ? Array.from(x.name).slice(0, 200).join('') : '';
  return { ok: true, value: { kind: 'pid', pid: x.pid, name } };
}

export function sameTarget(a: Target, b: Target): boolean {
  switch (a.kind) {
    case 'auto':
      return b.kind === 'auto';
    case 'session':
      return b.kind === 'session' && a.id === b.id;
    default:
      return b.kind === 'pid' && a.pid === b.pid;
  }
}

// --- Command lines (D5: every run has the CLI's own --timeout, and the
// extension waits 15 s longer, so the CLI's error with its hint arrives first) ---

/** A command line and how long the extension lets it run. */
export interface Run {
  argv: string[];
  timeoutMs: number;
}

const slackMs = 15_000;

function run(argv: string[], cliSeconds: number): Run {
  return { argv: [...argv, `--timeout=${cliSeconds}s`], timeoutMs: cliSeconds * 1000 + slackMs };
}

export const cliSeconds = { ps: 30, dump: 120, analyze: 300, traceSlack: 60, traceFile: 300 } as const;

/** The counters a watch samples every second (the CLI's default and minimum interval). */
export const watchIntervalSeconds = 1;

export type Profile = 'cpu' | 'gc';

export function isProfile(x: unknown): x is Profile {
  return x === 'cpu' || x === 'gc';
}

/** checkPath refuses a path no argv can carry: empty, or with a NUL. */
function checkPath(p: string): void {
  if (p === '' || hasNul(p)) {
    throw new EyedbgError('INVALID_REQUEST', 'not a file path');
  }
}

export function psArgs(): Run {
  return run(['dotnet', 'ps', '--json'], cliSeconds.ps);
}

/** watchArgs: until interrupted (the CLI caps a watch at 24 h); no timeout. */
export function watchArgs(t: Resolved, client: string): string[] {
  return ['dotnet', 'counters', ...targetArgs(t, client), '--watch', '--json', `--interval=${watchIntervalSeconds}s`];
}

export function dumpArgs(t: Resolved, client: string): Run {
  return run(['dotnet', 'dump', ...targetArgs(t, client), '--type=heap', '--json'], cliSeconds.dump);
}

export const heapTop = 50;

export function heapArgs(dump: string): Run {
  checkPath(dump);
  const r = run(['dotnet', 'heap', '--json', `--top=${heapTop}`], cliSeconds.analyze);
  return { ...r, argv: [...r.argv, '--', dump] };
}

export const gcrootPaths = 3;

/** checkTypeName refuses what --gcroot can't take as a type name: empty, NUL, or 0x… (an address). */
export function checkTypeName(name: string): string {
  if (name === '' || hasNul(name)) {
    return 'not a type name';
  }
  if (/^0x/i.test(name)) {
    return `${JSON.stringify(name)} reads as an object address, not a type name`;
  }
  return '';
}

export function gcrootArgs(dump: string, type: string): Run {
  checkPath(dump);
  const bad = checkTypeName(type);
  if (bad !== '') {
    throw new EyedbgError('INVALID_REQUEST', bad);
  }
  const r = run(
    ['dotnet', 'heap', '--json', '--top=1', `--gcroot=${type}`, `--paths=${gcrootPaths}`],
    cliSeconds.analyze,
  );
  return { ...r, argv: [...r.argv, '--', dump] };
}

export const threadFrames = 20;

/** threadsArgs: of a target (the CLI takes a heap dump: lock owners need one), or of a dump file. */
export function threadsArgs(of: { target: Resolved; client: string } | { dump: string }): Run {
  const argv = ['dotnet', 'threads'];
  if ('dump' in of) {
    checkPath(of.dump);
    const r = run([...argv, '--json', `--frames=${threadFrames}`], cliSeconds.analyze);
    return { ...r, argv: [...r.argv, '--', of.dump] };
  }
  return run([...argv, ...targetArgs(of.target, of.client), '--json', `--frames=${threadFrames}`], cliSeconds.analyze);
}

export const traceTop = 20;

export function traceArgs(t: Resolved, client: string, profile: Profile, seconds: number): Run {
  return run(
    [
      'dotnet',
      'trace',
      ...targetArgs(t, client),
      '--json',
      `--profile=${profile}`,
      `--duration=${seconds}s`,
      `--top=${traceTop}`,
    ],
    seconds + cliSeconds.traceSlack,
  );
}

export function traceFileArgs(file: string): Run {
  checkPath(file);
  const r = run(['dotnet', 'trace', '--json', `--top=${traceTop}`], cliSeconds.traceFile);
  return { ...r, argv: [...r.argv, '--', file] };
}

/** parseDuration reads a trace duration: whole seconds or minutes, 1s to 5m. */
export function parseDuration(s: string): Result<number> {
  const m = /^(\d{1,4})\s*(s|m)$/i.exec(s.trim());
  const n = m === null ? Number.NaN : Number(m[1]) * ((m[2] ?? '').toLowerCase() === 'm' ? 60 : 1);
  if (!Number.isInteger(n) || n < 1 || n > 300) {
    return { ok: false, error: 'a whole number of seconds or minutes from 1s to 5m, like 10s or 2m' };
  }
  return { ok: true, value: n };
}

// --- The watch's stdout, split into lines ---

/** The longest line a watch may print (a sample is a few KB). */
export const maxLine = 1 << 20;

/**
 * LineSplitter splits a stream's text into lines ("\n", "\r\n" too),
 * holding at most max characters of an unfinished line: past that, push
 * throws. Each character is scanned once.
 */
export class LineSplitter {
  private pending = '';

  constructor(private readonly max = maxLine) {}

  push(chunk: string): string[] {
    const out: string[] = [];
    let from = 0;
    for (let i = chunk.indexOf('\n'); i >= 0; i = chunk.indexOf('\n', from)) {
      const line = this.pending + chunk.slice(from, i);
      this.pending = '';
      if (line.length > this.max) {
        throw new EyedbgError('INTERNAL', `eyedbg printed a line longer than ${this.max} characters`);
      }
      out.push(line.endsWith('\r') ? line.slice(0, -1) : line);
      from = i + 1;
    }
    this.pending += chunk.slice(from);
    if (this.pending.length > this.max) {
      this.pending = '';
      throw new EyedbgError('INTERNAL', `eyedbg printed a line longer than ${this.max} characters`);
    }
    return out;
  }

  /** end returns the last, unterminated line, if any. */
  end(): string[] {
    const rest = this.pending.endsWith('\r') ? this.pending.slice(0, -1) : this.pending;
    this.pending = '';
    return rest === '' ? [] : [rest];
  }
}

// --- Counters ---

export interface CounterRow extends CounterValue {
  /** A gauge's change since the previous sample (none on the first one). */
  delta: number | undefined;
  /** A sum's total since the watch started. */
  total: number;
  elapsedMs: number;
}

/** CounterBoard is the latest sample of a watch, with gauges' deltas and sums' totals since it started. */
export class CounterBoard {
  private readonly previous = new Map<string, number>();
  private readonly totals = new Map<string, number>();
  private current: CounterRow[] = [];
  samples = 0;

  add(sample: Sample): void {
    this.current = sample.counters.map((c) => {
      const before = this.previous.get(c.name);
      const total = c.kind === 'sum' ? (this.totals.get(c.name) ?? 0) + c.value : 0;
      if (c.kind === 'sum') {
        this.totals.set(c.name, total);
      } else {
        this.previous.set(c.name, c.value);
      }
      return {
        ...c,
        delta: c.kind === 'gauge' && before !== undefined ? c.value - before : undefined,
        total,
        elapsedMs: sample.elapsedMs,
      };
    });
    this.samples++;
  }

  rows(): readonly CounterRow[] {
    return this.current;
  }

  reset(): void {
    this.previous.clear();
    this.totals.clear();
    this.current = [];
    this.samples = 0;
  }
}

// --- Whether a joined session's program runs (D6) ---

const resumes = new Set(['continue', 'next', 'stepIn', 'stepOut', 'stepBack', 'reverseContinue', 'goto']);

/**
 * nextRunState is a joined session's program state after an adapter
 * message: running (true), stopped or ended (false), or unknown (as
 * before). An adapter sends no 'continued' for the client's own resume:
 * its successful response says it.
 */
export function nextRunState(prev: boolean | undefined, m: DapMessage): boolean | undefined {
  if (m.type === 'event') {
    switch (m.event) {
      case 'stopped':
      case 'terminated':
      case 'exited':
        return false;
      case 'continued':
        return true;
      default:
        return prev;
    }
  }
  if (m.type === 'response' && m.success && resumes.has(m.command)) {
    return true;
  }
  return prev;
}

// --- Opening a frame's source (D10) ---

/**
 * isLocalAbsolute is the helper's rule for a path a dump names
 * (helpers/dotnet … SourceLines.IsLocalAbsolute, ClrMD's
 * IsSafeAbsoluteLocalPath), exactly: never two leading separators (UNC,
 * \\?\, \\.\); on Windows a drive root (X:\ or X:/), elsewhere a leading
 * /. Checked before any file system call: on Windows even a stat of a UNC
 * path connects to its server.
 */
export function isLocalAbsolute(path: string, windows: boolean): boolean {
  const sep = (c: string | undefined) => c === '/' || c === '\\';
  if (path.trim() === '' || hasNul(path) || (path.length >= 2 && sep(path[0]) && sep(path[1]))) {
    return false;
  }
  return windows
    ? path.length >= 3 && /^[A-Za-z]$/.test(path[0] ?? '') && path[1] === ':' && sep(path[2])
    : path[0] === '/';
}
