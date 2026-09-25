// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The .NET views' trees (Memory, Threads, CPU Trace), built whole from the
// CLI's results as plain rows on each change: labels and descriptions are
// plain text without icons, tooltips plain strings; commands carry ids of
// the extension's own state, never a path.

import type {
  DumpResult,
  DumpThread,
  Frame,
  Gcroot,
  HeapResult,
  Lock,
  MethodSamples,
  RootLabel,
  RootPath,
  ThreadsResult,
  TraceResult,
} from './dotnetParse';
import { clockOf, formatBytes, formatCount, formatSeconds, percent, plural, processText, txt } from './dotnetRender';
import { basename, plainText } from './render';

// --- Tree rows (the views' items, built whole on each change) ---

/** What a row's commands act on: ids the extension looks up in its own state, never a path. */
export type Ref =
  | { kind: 'dump'; dump: number }
  | { kind: 'type'; dump: number; type: number }
  | { kind: 'threads'; run: number }
  | { kind: 'frame'; run: number; group: number; frame: number }
  | { kind: 'trace'; run: number };

export interface Row {
  id: string;
  label: string;
  description: string;
  tooltip: string;
  icon: string;
  context: string;
  /** undefined: a leaf; else expanded or collapsed. */
  expanded: boolean | undefined;
  children: Row[];
  ref: Ref | undefined;
  /** A frame with a source file and line (Threads: it gets the open command). */
  source: boolean;
}

function row(id: string, label: string, more: Partial<Row> = {}): Row {
  return {
    id,
    label,
    description: '',
    tooltip: '',
    icon: '',
    context: '',
    expanded: undefined,
    children: [],
    ref: undefined,
    source: false,
    ...more,
  };
}

// --- Memory ---

export type GcrootState =
  | { state: 'running' }
  | { state: 'busy' }
  | { state: 'failed'; error: string }
  | { state: 'done'; result: Gcroot };

export interface HeapEntry {
  key: number;
  at: Date;
  /** The dump this window took (undefined: a dump file opened). */
  dump: DumpResult | undefined;
  heap: HeapResult;
  /** GC root searches by type name. */
  gcroots: Map<string, GcrootState>;
}

/** rootText is a root's label: "static Holder.Keep", "stack thread 1 App.Run()", or its kind (the CLI's rootText). */
export function rootText(r: RootLabel): string {
  let s = r.kind;
  if (r.name !== '') {
    s += ` ${r.name}`;
  }
  if (r.thread !== undefined) {
    s += ` thread ${r.thread}`;
  }
  if (r.frame !== '') {
    s += ` ${r.frame}`;
  }
  return txt(s);
}

/** The links the helper keeps before a cut chain's gap (the CLI's chainHead). */
const chainHead = 8;

function pathRow(id: string, p: RootPath): Row {
  const links = p.chain.map((l, i) => row(`${id}/${i}`, txt(`${l.type} ${l.address}`), { icon: 'arrow-small-right' }));
  if (p.chainOmitted > 0) {
    links.splice(
      Math.min(chainHead, Math.max(0, p.chain.length - 1)),
      0,
      row(`${id}/gap`, `… ${plural(p.chainOmitted, 'more link')}`),
    );
  }
  const total = p.chain.length + p.chainOmitted;
  return row(id, rootText(p.root), {
    description: plural(total, 'link'),
    tooltip: plainText(`${rootText(p.root)} → … → ${p.chain.at(-1)?.type ?? ''}`, 500),
    icon: 'pin',
    expanded: true,
    children: links,
  });
}

function gcrootRows(id: string, g: GcrootState | undefined): Row[] {
  switch (g?.state) {
    case undefined:
      return [row(`${id}/hint`, 'Expand to find GC roots', { icon: 'search' })];
    case 'running':
      return [row(`${id}/hint`, 'Finding GC roots…', { icon: 'loading~spin' })];
    case 'busy':
      return [row(`${id}/hint`, 'Another operation is running: Find GC Roots when it ends', { icon: 'watch' })];
    case 'failed':
      return [row(`${id}/hint`, g.error, { icon: 'error', tooltip: g.error })];
    case 'done': {
      const r = g.result;
      if (r.paths.length === 0) {
        return [
          row(
            `${id}/none`,
            r.complete
              ? 'No root path found: garbage the next collection frees'
              : 'No root path found before the search stopped',
            { icon: 'info' },
          ),
        ];
      }
      const out = r.paths.map((p, i) => pathRow(`${id}/p${i}`, p));
      if (!r.complete) {
        out.push(row(`${id}/more`, 'More paths may exist', { icon: 'info' }));
      }
      return out;
    }
  }
}

function generationName(name: string): string {
  return name === 'loh' || name === 'poh' ? name.toUpperCase() : txt(name, 40);
}

/** heapRows are the Memory view's roots, newest first. */
export function heapRows(entries: readonly HeapEntry[]): Row[] {
  return [...entries].reverse().map((e) => {
    const id = `dump${e.key}`;
    const h = e.heap;
    const path = e.dump?.path ?? h.dump.path;
    const label = e.dump !== undefined ? `Heap dump ${clockOf(e.at)}` : `Dump file ${txt(basename(path), 100)}`;
    const description = [
      ...(e.dump !== undefined ? [formatBytes(e.dump.bytes)] : []),
      plural(h.objects, 'object'),
      `${formatBytes(h.bytes)} managed`,
    ].join(' · ');
    const target = e.dump?.target ?? h.target;
    const tooltip = [
      plainText(path, 1000),
      ...(h.dump.private ? ['private to you, removed after 7 days (at most 10 kept)'] : []),
      ...(target !== undefined && target.pid > 0 ? [`of ${processText(target)}`] : []),
      `.NET ${plainText(h.runtime.version, 40)}, ${plainText(h.runtime.gc, 40)} GC`,
    ].join('\n');
    const generations = row(`${id}/gens`, 'Generations', {
      expanded: false,
      icon: 'layers',
      children: h.generations.map((g, i) =>
        row(`${id}/gen${i}`, generationName(g.name), {
          description: `${plural(g.objects, 'object')} · ${formatBytes(g.bytes)}`,
        }),
      ),
    });
    const types = h.types.map((t, i) => {
      const tid = `${id}/type${i}`;
      return row(tid, txt(t.name), {
        description: `${plural(t.count, 'object')} · ${formatBytes(t.bytes)}`,
        tooltip: plainText(t.name, 1000),
        icon: 'symbol-class',
        context: 'type',
        expanded: false,
        ref: { kind: 'type', dump: e.key, type: i },
        children: gcrootRows(tid, e.gcroots.get(t.name)),
      });
    });
    if (h.typesOmitted > 0) {
      types.push(row(`${id}/types-more`, `… ${plural(h.typesOmitted, 'more type')}`));
    }
    return row(id, label, {
      description,
      tooltip,
      icon: 'database',
      context: 'dump',
      expanded: true,
      ref: { kind: 'dump', dump: e.key },
      children: [
        generations,
        row(`${id}/types`, `Types (${formatCount(h.types.length)} of ${formatCount(h.typeCount)})`, {
          description: 'by size; expand one for its GC roots',
          icon: 'list-ordered',
          expanded: true,
          children: types,
        }),
      ],
    });
  });
}

// --- Threads ---

export interface ThreadGroup {
  ids: number[];
  osIds: number[];
  sample: DumpThread;
  /** One holds a lock. */
  owner: boolean;
  /** In Monitor.Enter/Wait. */
  waiting: boolean;
}

/** printable is the CLI's: control characters become '?' (so frames group as its text does). */
function printable(s: string): string {
  // biome-ignore lint/suspicious/noControlCharactersInRegex: matching control characters is the point.
  return s.replace(/[\u0000-\u001f\u007f-\u009f]/g, '?');
}

function fileBase(p: string): string {
  return p.slice(Math.max(p.lastIndexOf('/'), p.lastIndexOf('\\')) + 1);
}

/** frameText is the CLI's frame line, which decides grouping and "×2". */
export function frameText(f: Frame): string {
  if (f.kind !== 'managed') {
    return `[runtime frame ${printable(f.method)}]`;
  }
  let s = printable(f.method);
  if (f.ilOffset !== undefined) {
    s += ` +0x${f.ilOffset.toString(16)}`;
  }
  if (f.file !== '' && f.line > 0) {
    s += ` (${printable(fileBase(f.file))}:${f.line})`;
  }
  return s;
}

function threadKey(t: DumpThread, owner: boolean): string {
  return [
    owner,
    t.alive,
    t.background,
    t.threadpool,
    t.finalizer,
    t.gc,
    t.exception,
    t.framesOmitted,
    ...t.frames.map(frameText),
  ].join('\u0000');
}

function inMonitor(t: DumpThread): boolean {
  return t.frames.some((f) => f.kind === 'managed' && f.method.startsWith('System.Threading.Monitor.'));
}

/**
 * groupThreads groups threads with identical stacks and flags, as the
 * CLI's text does: lock owners first, then threads in Monitor.Enter/Wait,
 * then bigger groups, then by the lowest managed id.
 */
export function groupThreads(threads: readonly DumpThread[], locks: readonly Lock[]): ThreadGroup[] {
  const owners = new Set(locks.flatMap((l) => (l.owner !== undefined ? [l.owner] : [])));
  const byKey = new Map<string, ThreadGroup>();
  const groups: ThreadGroup[] = [];
  for (const t of threads) {
    const owner = owners.has(t.managedId);
    const key = threadKey(t, owner);
    let g = byKey.get(key);
    if (g === undefined) {
      g = { ids: [], osIds: [], sample: t, owner, waiting: inMonitor(t) };
      byKey.set(key, g);
      groups.push(g);
    }
    g.ids.push(t.managedId);
    g.osIds.push(t.osId);
  }
  for (const g of groups) {
    g.ids.sort((a, b) => a - b);
  }
  const rank = (g: ThreadGroup) => (g.owner ? 0 : g.waiting ? 1 : 2);
  // Array.prototype.sort is stable, as the CLI's SortStableFunc.
  return groups.sort((a, b) => rank(a) - rank(b) || b.ids.length - a.ids.length || (a.ids[0] ?? 0) - (b.ids[0] ?? 0));
}

/** A frame line: the first frame of a run of identical ones, and how many. */
export interface FrameLine {
  index: number;
  count: number;
  frame: Frame;
}

/** frameLines collapses repeated frames into one line ("×2"), as the CLI does. */
export function frameLines(frames: readonly Frame[]): FrameLine[] {
  const out: FrameLine[] = [];
  for (let i = 0; i < frames.length; ) {
    const text = frameText(frames[i] as Frame);
    let n = 1;
    while (i + n < frames.length && frameText(frames[i + n] as Frame) === text) {
      n++;
    }
    out.push({ index: i, count: n, frame: frames[i] as Frame });
    i += n;
  }
  return out;
}

/** groupFlags are a group's flags, as the CLI's heading lists them (and waiting). */
export function groupFlags(g: ThreadGroup): string[] {
  const t = g.sample;
  const flags: string[] = [];
  if (g.owner) {
    flags.push('holds a lock');
  }
  if (g.waiting) {
    flags.push('waiting for a lock');
  }
  if (!t.alive) {
    flags.push('dead');
  }
  for (const [on, name] of [
    [t.background, 'background'],
    [t.threadpool, 'threadpool'],
    [t.finalizer, 'finalizer'],
    [t.gc, 'gc'],
  ] as const) {
    if (on) {
      flags.push(name);
    }
  }
  if (t.exception !== '') {
    flags.push(`exception ${txt(t.exception, 200)}`);
  }
  return flags;
}

export interface ThreadsEntry {
  key: number;
  at: Date;
  /** Taken for this capture (else: a dump file's, or a Memory dump's). */
  taken: boolean;
  result: ThreadsResult;
  groups: ThreadGroup[];
}

function frameRow(id: string, line: FrameLine, ref: Ref): Row {
  const f = line.frame;
  const label = txt(f.method) + (line.count > 1 ? ` ×${line.count}` : '');
  if (f.kind !== 'managed') {
    return row(id, label, { description: 'runtime frame', icon: 'gear' });
  }
  const source = f.file !== '' && f.line > 0;
  const where = source
    ? `${txt(fileBase(f.file), 100)}:${f.line}`
    : f.ilOffset !== undefined
      ? `+0x${f.ilOffset.toString(16)}`
      : '';
  const tooltip = [
    plainText(f.method, 1000),
    ...(f.module !== '' ? [`module ${plainText(f.module, 200)}`] : []),
    ...(source ? [`${plainText(f.file, 1000)}:${f.line}`] : []),
    ...(f.ilOffset !== undefined ? [`IL offset 0x${f.ilOffset.toString(16)}`] : []),
  ].join('\n');
  return row(id, label, {
    description: [txt(f.module, 100), where].filter((x) => x !== '').join(' · '),
    tooltip,
    icon: source ? 'go-to-file' : 'symbol-method',
    context: source ? 'frame source' : 'frame',
    ref,
    source,
  });
}

function lockRow(id: string, l: Lock): Row {
  return row(id, txt(`${l.type} ${l.object}`), {
    description: `${l.owner !== undefined ? `held by thread ${l.owner}` : 'not held'} · ${formatCount(l.waiting)} waiting`,
    icon: 'lock',
  });
}

/** threadsRows are the Threads view's roots, newest first. */
export function threadsRows(entries: readonly ThreadsEntry[]): Row[] {
  return [...entries].reverse().map((e) => {
    const id = `threads${e.key}`;
    const r = e.result;
    const children: Row[] = [];
    if (!r.locksAvailable) {
      children.push(row(`${id}/locks`, 'Locks: not recorded (a mini dump has none)', { icon: 'lock' }));
    } else if (r.locks.length > 0) {
      children.push(
        row(`${id}/locks`, 'Locks', {
          description: plural(r.locks.length, 'lock'),
          icon: 'lock',
          expanded: true,
          children: r.locks.map((l, i) => lockRow(`${id}/lock${i}`, l)),
        }),
      );
    }
    e.groups.forEach((g, gi) => {
      const gid = `${id}/g${gi}`;
      const frames = frameLines(g.sample.frames).map((line) =>
        frameRow(`${gid}/f${line.index}`, line, { kind: 'frame', run: e.key, group: gi, frame: line.index }),
      );
      if (g.sample.frames.length === 0) {
        frames.push(row(`${gid}/none`, '(no frames)'));
      }
      if (g.sample.framesOmitted > 0) {
        frames.push(row(`${gid}/more`, `… ${plural(g.sample.framesOmitted, 'more frame')}`));
      }
      const flags = groupFlags(g);
      children.push(
        row(gid, g.ids.length === 1 ? `thread ${g.ids[0]}` : `threads ${g.ids.join(', ')}`, {
          description: flags.join(' · '),
          tooltip: [
            `managed ids ${g.ids.join(', ')}`,
            `OS thread ids ${g.osIds.join(', ')}`,
            ...(flags.length > 0 ? [flags.join(', ')] : []),
          ].join('\n'),
          icon: g.owner ? 'lock' : g.waiting ? 'watch' : 'pulse',
          expanded: g.owner || g.waiting,
          children: frames,
        }),
      );
    });
    const label = e.taken ? `Threads at ${clockOf(e.at)}` : `Threads of ${txt(basename(r.dump.path), 100)}`;
    return row(id, label, {
      description: [
        plural(r.threads.length, 'thread'),
        ...(r.locksAvailable ? [plural(r.locks.length, 'lock')] : []),
        ...(r.threadsOmitted > 0 ? [`${formatCount(r.threadsOmitted)} more not listed`] : []),
      ].join(' · '),
      tooltip: [
        plainText(r.dump.path, 1000),
        ...(r.target !== undefined && r.target.pid > 0 ? [`of ${processText(r.target)}`] : []),
        `.NET ${plainText(r.runtime.version, 40)}, ${plainText(r.runtime.gc, 40)} GC`,
      ].join('\n'),
      icon: 'list-tree',
      context: 'threads',
      expanded: true,
      ref: { kind: 'threads', run: e.key },
      children,
    });
  });
}

// --- CPU Trace ---

export interface TraceEntry {
  key: number;
  at: Date;
  /** Recorded by this window (else: a trace file opened). */
  taken: boolean;
  result: TraceResult;
}

function methodRows(id: string, rows: readonly MethodSamples[], omitted: number, total: number): Row[] {
  const out = rows.map((m, i) =>
    row(`${id}/${i}`, txt(m.method), {
      description: [
        ...(m.module !== '' ? [txt(m.module, 100)] : []),
        plural(m.samples, 'sample'),
        ...(total > 0 ? [percent(m.samples, total)] : []),
      ].join(' · '),
      tooltip: plainText(m.method, 1000),
      icon: 'symbol-method',
    }),
  );
  if (omitted > 0) {
    out.push(row(`${id}/more`, `… ${formatCount(omitted)} more`));
  }
  return out;
}

const endReasons: Record<string, string> = {
  duration: 'ran its full duration',
  exited: 'the process exited: shorter than asked',
  size: 'reached its size limit (512 MiB) and ended early',
};

/**
 * traceRows are the CPU Trace view's roots, newest first; a trace's
 * sections only when it has them. Percentages are the CLI's: hottest and
 * inclusive of the samples in managed code, waiting of the other samples.
 */
export function traceRows(entries: readonly TraceEntry[]): Row[] {
  return [...entries].reverse().map((e) => {
    const id = `trace${e.key}`;
    const r = e.result;
    const children: Row[] = [];
    const cpu = r.cpu;
    if (cpu !== undefined) {
      if (cpu.samples === 0) {
        children.push(row(`${id}/nocpu`, 'No CPU samples', { icon: 'info' }));
      }
      for (const [key, label, list, omitted, total, expanded] of [
        ['excl', 'Hottest methods (exclusive)', cpu.exclusive, cpu.exclusiveOmitted, cpu.managed, true],
        ['incl', 'Inclusive (callers that lead there)', cpu.inclusive, cpu.inclusiveOmitted, cpu.managed, false],
        ['wait', 'Waiting or in native code', cpu.waiting, cpu.waitingOmitted, cpu.other, false],
      ] as const) {
        if (list.length > 0) {
          children.push(
            row(`${id}/${key}`, label, {
              description: plural(total, 'sample'),
              icon: key === 'wait' ? 'watch' : 'flame',
              expanded,
              children: methodRows(`${id}/${key}`, list, omitted, total),
            }),
          );
        }
      }
    }
    const gc = r.gc;
    if (gc !== undefined) {
      const rows = [
        row(`${id}/gc/gen0`, 'gen0', { description: formatCount(gc.gen0) }),
        row(`${id}/gc/gen1`, 'gen1', { description: formatCount(gc.gen1) }),
        row(`${id}/gc/gen2`, 'gen2', { description: formatCount(gc.gen2) }),
        ...(gc.background > 0 ? [row(`${id}/gc/bg`, 'background', { description: formatCount(gc.background) })] : []),
        ...(gc.induced > 0
          ? [row(`${id}/gc/induced`, 'induced by GC.Collect', { description: formatCount(gc.induced) })]
          : []),
        row(`${id}/gc/pauses`, 'pauses', {
          description: `${gc.pauseTotalMs.toFixed(1)} ms total · ${gc.pauseMaxMs.toFixed(1)} ms max`,
        }),
      ];
      children.push(
        row(`${id}/gc`, 'Garbage collections', {
          description: plural(gc.collections, 'collection'),
          icon: 'trash',
          expanded: true,
          children: rows,
        }),
      );
    }
    const al = r.allocations;
    if (al !== undefined) {
      const types = al.types.map((t, i) =>
        row(`${id}/alloc/${i}`, txt(t.name), {
          description: `≈ ${formatBytes(t.bytes)} · ${plural(t.ticks, 'tick')}`,
          tooltip: plainText(t.name, 1000),
          icon: 'symbol-class',
        }),
      );
      if (al.typesOmitted > 0) {
        types.push(row(`${id}/alloc/more`, `… ${plural(al.typesOmitted, 'more type')}`));
      }
      children.push(
        row(`${id}/alloc`, 'Allocations', {
          description: `≈ ${formatBytes(al.bytes)} (sampled about every 100 KB)`,
          icon: 'symbol-array',
          expanded: true,
          children: types,
        }),
      );
    }
    if (cpu === undefined && gc === undefined && al === undefined) {
      children.push(row(`${id}/empty`, 'Nothing recorded: no CPU samples, GC or allocation events', { icon: 'info' }));
    }
    const kind = r.profile === 'gc' ? 'GC' : 'CPU';
    const label = e.taken
      ? `${kind} trace ${clockOf(e.at)} · ${formatSeconds(r.durationMs)}`
      : `Trace file ${txt(basename(r.trace.path), 100)} · ${formatSeconds(r.durationMs)}`;
    const description =
      cpu !== undefined && cpu.samples > 0
        ? `${plural(cpu.samples, 'sample')} · ${plural(cpu.threads, 'thread')}`
        : gc !== undefined
          ? plural(gc.collections, 'collection')
          : 'nothing recorded';
    const tooltip = [
      plainText(r.trace.path, 1000),
      ...(r.trace.private ? ['private to you, removed after 7 days (at most 10 kept)'] : []),
      ...(r.target !== undefined && r.target.pid > 0 ? [`of ${processText(r.target)}`] : []),
      ...(r.endReason !== '' ? [`the trace ${endReasons[r.endReason] ?? plainText(r.endReason, 40)}`] : []),
      ...(r.eventsLost > 0 ? [`${formatCount(r.eventsLost)} events lost (the buffer overflowed): counts are low`] : []),
      ...(cpu !== undefined && cpu.unresolvedFrames > 0
        ? [`${plural(cpu.unresolvedFrames, 'frame')} without a method name, counted as (unresolved)`]
        : []),
    ].join('\n');
    return row(id, label, {
      description,
      tooltip,
      icon: cpu !== undefined && cpu.samples > 0 ? 'flame' : 'trash',
      context: 'trace',
      expanded: true,
      ref: { kind: 'trace', run: e.key },
      children,
    });
  });
}
