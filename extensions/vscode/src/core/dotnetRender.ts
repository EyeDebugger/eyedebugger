// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What the .NET views show (their trees are in dotnetTrees): type, method,
// module, file and process names come from the program and its dumps, so
// every one is plain text without icons, and tooltips are plain strings.
// Never values: the CLI reports none.

import type { CounterRow, Resolved, Target } from './dotnet';
import type { ProcessTarget, PsEntry } from './dotnetParse';
import { noIcons, notificationSafe, plainText } from './render';
import { isSessionId } from './validate';

/** txt is a program-derived string for a label or description. */
export function txt(s: string, max = 300): string {
  return noIcons(plainText(s, max));
}

// --- Numbers (en-US, so the tests are stable) ---

const counts = new Intl.NumberFormat('en-US', { maximumFractionDigits: 0 });

/** formatCount is n with thousands separators ("5,000"), as the CLI prints it. */
export function formatCount(n: number): string {
  return counts.format(n);
}

const byteUnits = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];

/** formatBytes is a size in IEC units with one decimal ("341.7 MiB"; bytes as such below 1 KiB), as the CLI prints it. */
export function formatBytes(n: number): string {
  if (n < 1024) {
    return `${Math.round(n)} B`;
  }
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < byteUnits.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(1)} ${byteUnits[i]}`;
}

const valueFormats = [0, 1, 2, 3].map((d) => new Intl.NumberFormat('en-US', { maximumFractionDigits: d }));

/** formatValue is a counter value: fewer decimals the bigger it is ("12.3", "0.041", "2,048"). */
export function formatValue(v: number): string {
  const a = Math.abs(v);
  const f = valueFormats[a >= 1000 ? 0 : a >= 10 ? 1 : a >= 1 ? 2 : 3] as Intl.NumberFormat;
  const s = f.format(v);
  return s === '-0' ? '0' : s;
}

/** formatSeconds is milliseconds as seconds with one decimal ("10.0 s"). */
export function formatSeconds(ms: number): string {
  return `${(ms / 1000).toFixed(1)} s`;
}

/** percent is n of total with one decimal ("68.7%"), '' without a total, as the CLI prints it. */
export function percent(n: number, total: number): string {
  return total > 0 ? `${((100 * n) / total).toFixed(1)}%` : '';
}

function pad(n: number): string {
  return String(n).padStart(2, '0');
}

/** clockOf is d's local time, HH:MM:SS. */
export function clockOf(d: Date): string {
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

export function plural(n: number, one: string, many = `${one}s`): string {
  return `${formatCount(n)} ${n === 1 ? one : many}`;
}

// --- Targets ---

/** targetText names a resolved target: "s-k3f9" or "pid 4321 dotnet". */
export function targetText(t: Resolved): string {
  return t.kind === 'session' ? t.id : `pid ${t.pid}${t.name !== '' ? ` ${txt(t.name, 60)}` : ''}`;
}

/**
 * progressTitle is a progress notification's title about t ("Heap dump of
 * pid 4321 api"): a process name can't become a link in a notification.
 */
export function progressTitle(what: string, t: Resolved): string {
  return notificationSafe(`${what} of ${targetText(t)}`, 200);
}

/** targetDescription is a view's description: the target, and what auto follows now. */
export function targetDescription(t: Target, auto: Resolved | undefined): string {
  if (t.kind !== 'auto') {
    return targetText(t);
  }
  return auto === undefined ? 'auto' : `auto: ${targetText(auto)}`;
}

export function processText(p: ProcessTarget): string {
  const name = p.name === '' ? '?' : txt(p.name, 60);
  return `pid ${p.pid} (${name})${p.session !== '' ? `, session ${txt(p.session, 40)}` : ''}`;
}

export interface PickItem {
  label: string;
  description: string;
  target: Target;
}

/**
 * pickItems are the target picker's entries: auto first, then the
 * processes 'eyedbg dotnet ps' lists — this window's joined sessions,
 * other sessions, then the rest, each by pid. A process with a session
 * becomes a session target (the CLI then checks it is still that session's).
 */
export function pickItems(ps: readonly PsEntry[], joined: ReadonlySet<string>): PickItem[] {
  const session = (p: PsEntry) => (isSessionId(p.session) ? p.session : '');
  const rank = (p: PsEntry) => (session(p) === '' ? 2 : joined.has(session(p)) ? 0 : 1);
  const sorted = [...ps].sort((a, b) => rank(a) - rank(b) || a.pid - b.pid);
  return [
    { label: 'Follow the active debug session (auto)', description: '', target: { kind: 'auto' } },
    ...sorted.map((p): PickItem => {
      const id = session(p);
      return {
        label: p.name === '' ? '?' : txt(p.name, 80),
        description: id !== '' ? `pid ${p.pid} · ${id}${joined.has(id) ? ' (joined)' : ''}` : `pid ${p.pid}`,
        target: id !== '' ? { kind: 'session', id } : { kind: 'pid', pid: p.pid, name: p.name },
      };
    }),
  ];
}

// --- Messages ---

/** errorText is an error for a view's message: "CODE: message — hint", one plain line. */
export function errorText(e: { code: string; message: string; hint: string }): string {
  const first = e.message.split(/\r?\n/, 1)[0] ?? '';
  return plainText(`${e.code !== '' ? `${e.code}: ` : ''}${first}${e.hint !== '' ? ` — ${e.hint}` : ''}`, 500);
}

export type Feature = 'dotnet.helper' | 'dotnet.dump' | 'dotnet.trace';

/** missingFeature is the message of a view whose eyedbg lacks its feature. */
export function missingFeature(version: string, feature: Feature): string {
  const what = { 'dotnet.helper': 'counters', 'dotnet.dump': 'dumps', 'dotnet.trace': 'traces' }[feature];
  return plainText(`This eyedbg (${version}) has no .NET ${what} (${feature}): update eyedbg`, 300);
}

export type WatchState = 'idle' | 'starting' | 'running' | 'paused' | 'waiting' | 'ended' | 'failed';

/** watchMessage is the Counters view's message; stopped: the joined session is stopped at a breakpoint. */
export function watchMessage(state: WatchState, target: Resolved | undefined, stopped: boolean, error = ''): string {
  const t = target === undefined ? 'the program' : targetText(target);
  switch (state) {
    case 'starting':
      return `Starting a watch of ${t}…`;
    case 'running':
      return stopped
        ? `${t} is stopped at a breakpoint: no samples until it runs`
        : `Watching ${t} every 1 s — each sample shows about 1 s late`;
    case 'paused':
      return 'Paused: Resume starts a new watch';
    case 'waiting':
      return `${t} is stopped at a breakpoint: the watch starts when it runs again`;
    case 'ended':
      return 'The watch ended: the process exited (or 24 h passed)';
    case 'failed':
      return error;
    default:
      return '';
  }
}

// --- Counters ---

function withUnit(v: string, unit: string): string {
  return unit === '' ? v : `${v} ${txt(unit, 20)}`;
}

/** counterText is a counter's row: a gauge's value and change, a sum's growth per second and total. */
export function counterText(r: CounterRow): { label: string; description: string; tooltip: string } {
  let description: string;
  if (r.kind === 'sum') {
    const unit = r.unit === '' ? '' : ` ${txt(r.unit, 20)}`;
    description = `+${formatValue(r.value)}${unit}/s · total ${formatValue(r.total)}${unit}`;
  } else {
    description = withUnit(formatValue(r.value), r.unit);
    if (r.delta !== undefined) {
      description += r.delta === 0 ? ' =' : ` ${r.delta > 0 ? '▲' : '▼'} ${formatValue(Math.abs(r.delta))}`;
    }
  }
  const tooltip = [
    plainText(r.name, 200),
    `unit: ${r.unit === '' ? '(none)' : plainText(r.unit, 40)}`,
    r.kind === 'sum'
      ? 'a sum: its growth in each 1 s interval, and the total since the watch started'
      : 'a gauge: its value when sampled, and the change since the previous sample',
    `sampled at +${formatSeconds(r.elapsedMs)}`,
  ].join('\n');
  return { label: txt(r.name, 100), description, tooltip };
}
