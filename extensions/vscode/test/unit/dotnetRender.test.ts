// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// What the .NET views show, from the CLI's JSON goldens: plain text for
// every program-derived string, the CLI's grouping and numbers.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { test } from 'node:test';
import { CounterBoard } from '../../src/core/dotnet';
import {
  type Gcroot,
  parseDump,
  parseHeap,
  parsePs,
  parseThreads,
  parseTrace,
  parseWatchLine,
} from '../../src/core/dotnetParse';
import {
  clockOf,
  counterText,
  errorText,
  formatBytes,
  formatCount,
  formatValue,
  missingFeature,
  percent,
  pickItems,
  progressTitle,
  targetDescription,
  watchMessage,
} from '../../src/core/dotnetRender';
import {
  frameLines,
  groupThreads,
  type HeapEntry,
  heapRows,
  type Row,
  rootText,
  threadsRows,
  traceRows,
} from '../../src/core/dotnetTrees';
import { first, golden } from './dotnet.test';

const repo = path.resolve(__dirname, '..', '..', '..', '..');
const at = new Date(2026, 8, 25, 14, 3, 12);

/** walk lists every row, depth first. */
function walk(rows: Row[]): Row[] {
  return rows.flatMap((r) => [r, ...walk(r.children)]);
}

function find(rows: Row[], pred: (r: Row) => boolean): Row {
  const r = walk(rows).find(pred);
  assert.ok(r !== undefined, 'no such row');
  return r;
}

/** assertPlain: no control character and no icon syntax in any shown text. */
function assertPlain(rows: Row[]): void {
  for (const r of walk(rows)) {
    for (const s of [r.label, r.description]) {
      // biome-ignore lint/suspicious/noControlCharactersInRegex: the point.
      assert.doesNotMatch(s, /[\u0000-\u001f\u007f-\u009f]/, JSON.stringify(s));
      assert.doesNotMatch(s, /\$\(/, JSON.stringify(s));
    }
    // biome-ignore lint/suspicious/noControlCharactersInRegex: the point (newlines separate tooltip lines).
    assert.doesNotMatch(r.tooltip, /[\u0000-\u0009\u000b-\u001f\u007f-\u009f]/, JSON.stringify(r.tooltip));
  }
}

test('numbers as the CLI prints them', () => {
  assert.equal(formatCount(5000), '5,000');
  assert.equal(formatBytes(240000), '234.4 KiB');
  assert.equal(formatBytes(358305992), '341.7 MiB');
  assert.equal(formatBytes(1048576), '1.0 MiB');
  assert.equal(formatBytes(1023), '1023 B');
  assert.equal(formatBytes(19012345678), '17.7 GiB');
  assert.equal(percent(4120, 6001), '68.7%');
  assert.equal(percent(3790, 3811), '99.4%');
  assert.equal(percent(1, 0), '');
  assert.equal(formatValue(12.345), '12.3');
  assert.equal(formatValue(0.5), '0.5');
  assert.equal(formatValue(0.0000044), '0');
  assert.equal(formatValue(-0.0000044), '0');
  assert.equal(formatValue(2048), '2,048');
  assert.equal(formatValue(49.25), '49.3');
  assert.equal(formatValue(7.891), '7.89');
  assert.equal(clockOf(at), '14:03:12');
});

test('counters: gauge and sum rows over the golden', () => {
  const board = new CounterBoard();
  for (const v of golden('counters_watch')) {
    const s = parseWatchLine(JSON.stringify(v));
    assert.ok(s !== undefined);
    board.add(s);
  }
  const text = (name: string) => {
    const r = board.rows().find((x) => x.name === name);
    assert.ok(r !== undefined, name);
    return counterText(r);
  };
  assert.equal(text('cpu-usage').description, '12.3 % ▲ 11.8');
  assert.equal(text('working-set').description, '49.3 MB =');
  assert.equal(text('alloc-rate').description, '+2,048 B/s · total 3,072 B');
  assert.equal(text('exception-count').description, '+0/s · total 2');
  assert.match(text('cpu-usage').tooltip, /^cpu-usage\nunit: %\na gauge/);
  assert.match(text('cpu-usage').tooltip, /sampled at \+2\.1 s$/);
  const first = new CounterBoard();
  first.add({ elapsedMs: 1, counters: [{ name: 'g', unit: '', kind: 'gauge', value: 3 }] });
  first.add({ elapsedMs: 2, counters: [{ name: 'g', unit: '', kind: 'gauge', value: 1 }] });
  assert.equal(counterText(first.rows()[0] as never).description, '1 ▼ 2');
  const hostile = counterText({
    name: 'evil\u001b]0;x\u0007 $(zap)',
    unit: 'B\u001b[2J',
    kind: 'sum',
    value: 1,
    total: 1,
    delta: undefined,
    elapsedMs: 0,
  });
  assert.equal(hostile.label, 'evil ]0;x  $ (zap)');
  assert.equal(hostile.description, '+1 B [2J/s · total 1 B [2J');
});

test('targets and the picker', () => {
  assert.equal(targetDescription({ kind: 'auto' }, undefined), 'auto');
  assert.equal(targetDescription({ kind: 'auto' }, { kind: 'session', id: 's-k3f9' }), 'auto: s-k3f9');
  assert.equal(targetDescription({ kind: 'pid', pid: 4321, name: 'api\u001b[2J' }, undefined), 'pid 4321 api [2J');
  const items = pickItems(parsePs(first('ps')), new Set(['s-k3f9']));
  assert.deepEqual(
    items.map((i) => [i.label, i.description]),
    [
      ['Follow the active debug session (auto)', ''],
      ['dotnet', 'pid 812 · s-k3f9 (joined)'],
      ['api [2J', 'pid 4321'],
      ['?', 'pid 70000'],
    ],
  );
  assert.deepEqual(items[1]?.target, { kind: 'session', id: 's-k3f9' });
  assert.deepEqual(items[2]?.target, { kind: 'pid', pid: 4321, name: 'api\u001b[2J' });
  const order = pickItems(
    [
      { pid: 9, name: 'b', session: '' },
      { pid: 5, name: 'a', session: 's-b' },
      { pid: 7, name: 'c', session: 's-a' },
      { pid: 3, name: 'd', session: '' },
      { pid: 8, name: 'e', session: '$(zap)' },
    ],
    new Set(['s-a']),
  );
  assert.deepEqual(
    order.slice(1).map((i) => i.description),
    ['pid 7 · s-a (joined)', 'pid 5 · s-b', 'pid 3', 'pid 8', 'pid 9'],
  );
  assert.equal(order[4]?.target.kind, 'pid', 'a session that is no session id is not taken as one');
});

test('progress titles: a process name never becomes a notification link', () => {
  assert.equal(progressTitle('Heap dump', { kind: 'session', id: 's-k3f9' }), 'Heap dump of s-k3f9');
  const t = progressTitle('Threads', { kind: 'pid', pid: 7, name: '[x](command:workbench.action.quit) $(zap)' });
  assert.equal(t, 'Threads of pid 7 [x] (command:workbench.action.quit) $\u200a(zap)');
  assert.doesNotMatch(t, /\]\(/);
});

test('messages', () => {
  assert.equal(
    errorText({ code: 'NOT_DOTNET', message: 'pid 5 is no .NET process\nmore', hint: 'pick one from ps' }),
    'NOT_DOTNET: pid 5 is no .NET process — pick one from ps',
  );
  assert.equal(errorText({ code: '', message: 'm](command:x)', hint: '' }), 'm](command:x)');
  assert.match(missingFeature('v0.1.0', 'dotnet.dump'), /^This eyedbg \(v0\.1\.0\) has no \.NET dumps/);
  const s = { kind: 'session', id: 's-k3f9' } as const;
  assert.equal(watchMessage('running', s, false), 'Watching s-k3f9 every 1 s — each sample shows about 1 s late');
  assert.equal(watchMessage('running', s, true), 's-k3f9 is stopped at a breakpoint: no samples until it runs');
  assert.match(watchMessage('waiting', s, true), /stopped at a breakpoint: the watch starts when it runs again/);
  assert.match(watchMessage('ended', s, false), /process exited/);
  assert.equal(watchMessage('failed', s, false, 'X: y'), 'X: y');
  assert.equal(watchMessage('idle', undefined, false), '');
});

test('memory: a taken dump with types and a GC root search', () => {
  // The goldens' hostile names hold ESC and a raw C1 CSI (U+009B): both become spaces.
  const heap = parseHeap(first('heap'));
  const gcroot = heap.gcroot as Gcroot;
  heap.gcroot = undefined;
  const e: HeapEntry = { key: 1, at, dump: parseDump(first('dump')), heap, gcroots: new Map() };
  let rows = heapRows([e]);
  assertPlain(rows);
  const root = rows[0] as Row;
  assert.equal(root.label, 'Heap dump 14:03:12');
  assert.equal(root.description, '341.7 MiB · 5,332 objects · 435.8 KiB managed');
  assert.match(root.tooltip, /^\/home\/u\/\.eyedbg\/dumps\/4321-.*\.dmp\nprivate to you/);
  assert.match(root.tooltip, /of pid 4321 \(dotnet\), session s-k3f9/);
  assert.deepEqual(root.ref, { kind: 'dump', dump: 1 });
  assert.equal(root.context, 'dump');
  const gens = find(rows, (r) => r.label === 'Generations');
  assert.equal(gens.expanded, false);
  assert.deepEqual(
    gens.children.map((g) => g.label),
    ['gen0', 'gen1', 'gen2', 'LOH', 'POH', 'frozen'],
  );
  const types = find(rows, (r) => r.label.startsWith('Types'));
  assert.equal(types.label, 'Types (4 of 93)');
  assert.deepEqual(
    types.children.map((t) => [t.label, t.description]),
    [
      ['Retained', '5,000 objects · 234.4 KiB'],
      ['Retained[]', '13 objects · 128.3 KiB'],
      ['App.Evil [2J ', '1 object · 1.0 MiB'],
      ['System.String', '1,144 objects · 48.4 KiB'],
      ['… 89 more types', ''],
    ],
  );
  const retained = types.children[0] as Row;
  assert.deepEqual(retained.ref, { kind: 'type', dump: 1, type: 0 });
  assert.equal(retained.expanded, false);
  assert.deepEqual(
    retained.children.map((c) => c.label),
    ['Expand to find GC roots'],
  );

  e.gcroots.set('Retained', { state: 'running' });
  assert.equal(heapRows([e])[0]?.children[1]?.children[0]?.children[0]?.label, 'Finding GC roots…');
  e.gcroots.set('Retained', { state: 'done', result: gcroot });
  rows = heapRows([e]);
  assertPlain(rows);
  const paths = rows[0]?.children[1]?.children[0]?.children ?? [];
  assert.deepEqual(
    paths.map((p) => [p.label, p.description]),
    [
      ['static Holder.Keep', '3 links'],
      ['stack thread 1 App.Evil ]0;x .Run()', '4 links'],
      ['More paths may exist', ''],
    ],
  );
  assert.deepEqual(
    paths[0]?.children.map((c) => c.label),
    ['System.Collections.Generic.List<Retained> 0x13de13da8', 'Retained[] 0x13de54198', 'Retained 0x13de6eab8'],
  );
  assert.deepEqual(
    paths[1]?.children.map((c) => c.label),
    ['… 3 more links', 'App.Evil [2J  0x13de6eb48'],
    "the gap where the CLI puts it ('→ …3 more… →')",
  );
  e.gcroots.set('Retained', { state: 'done', result: { ...gcroot, paths: [], complete: true } });
  assert.equal(
    heapRows([e])[0]?.children[1]?.children[0]?.children[0]?.label,
    'No root path found: garbage the next collection frees',
  );
  e.gcroots.set('Retained', { state: 'failed', error: 'DIAGNOSTICS_TIMEOUT: x' });
  assert.equal(heapRows([e])[0]?.children[1]?.children[0]?.children[0]?.label, 'DIAGNOSTICS_TIMEOUT: x');

  // A dump file opened: no dump result, newest first.
  const file: HeapEntry = { key: 2, at, dump: undefined, heap: parseHeap(first('heap')), gcroots: new Map() };
  const both = heapRows([e, file]);
  assert.deepEqual(
    both.map((r) => r.label),
    ['Dump file 4321-20260925T150300Z-heap-1f3a9c0e.dmp', 'Heap dump 14:03:12'],
  );
  assert.equal(both[0]?.description, '5,332 objects · 435.8 KiB managed');
});

test('rootText as the CLI', () => {
  assert.equal(rootText({ kind: 'static', name: 'Holder.Keep', thread: undefined, frame: '' }), 'static Holder.Keep');
  assert.equal(rootText({ kind: 'stack', name: '', thread: 1, frame: 'App.Run()' }), 'stack thread 1 App.Run()');
  assert.equal(rootText({ kind: 'strong handle', name: '', thread: undefined, frame: '' }), 'strong handle');
  assert.equal(rootText({ kind: 'static', name: '$(zap)', thread: undefined, frame: '' }), 'static $ (zap)');
});

/** textGroups are the thread groups' headings of each case of the CLI's text golden, in order. */
function textGroups(): string[][] {
  const out: string[][] = [];
  for (const l of fs
    .readFileSync(path.join(repo, 'internal', 'cli', 'testdata', 'dotnet_threads.golden'), 'utf8')
    .split(/\r?\n/)) {
    if (l.startsWith('threads of ')) {
      out.push([]);
    } else if (/^threads? \d/.test(l)) {
      out.at(-1)?.push(l.replace(/ \(.*$|:$/g, ''));
    }
  }
  return out;
}

test("threads: grouped in the CLI text's order", () => {
  const t = parseThreads(first('threads'));
  const groups = groupThreads(t.threads, t.locks);
  const heading = (ids: number[]) => (ids.length === 1 ? `thread ${ids[0]}` : `threads ${ids.join(', ')}`);
  // Without locks (a mini dump): the owner is no owner, and the order is the gcroot golden's.
  const [withLocks, mini] = textGroups();
  assert.equal(withLocks?.length, 5);
  assert.deepEqual(
    groups.map((g) => heading(g.ids)),
    withLocks,
  );
  // Without locks (a mini dump): thread 4 owns nothing, and the order is the second case's.
  assert.deepEqual(
    groupThreads(t.threads, []).map((g) => heading(g.ids)),
    mini,
  );
  assert.deepEqual(groups[2]?.osIds, [4007, 4008, 4009]);
  assert.deepEqual(
    frameLines(groups[0]?.sample.frames ?? []).map((l) => [l.index, l.count]),
    [
      [0, 2],
      [2, 1],
      [3, 1],
    ],
  );
});

test('threads: rows', () => {
  const t = parseThreads(first('threads'));
  const rows = threadsRows([{ key: 3, at, taken: true, result: t, groups: groupThreads(t.threads, t.locks) }]);
  assertPlain(rows);
  const root = rows[0] as Row;
  assert.equal(root.label, 'Threads at 14:03:12');
  assert.equal(root.description, '7 threads · 2 locks');
  const locks = root.children[0] as Row;
  assert.equal(locks.label, 'Locks');
  assert.deepEqual(
    locks.children.map((l) => [l.label, l.description]),
    [
      ['System.Object 0x13de6eb78', 'held by thread 4 · 1 waiting'],
      ['App.Evil [2J  0x13de6ec00', 'not held · 2 waiting'],
    ],
  );
  assert.deepEqual(
    root.children.slice(1).map((g) => [g.label, g.description]),
    [
      ['thread 4', 'holds a lock'],
      ['thread 5', 'waiting for a lock · exception System.InvalidOperationException'],
      ['threads 7, 8, 9', 'background · threadpool'],
      ['thread 1', ''],
      ['thread 2', 'background · finalizer'],
    ],
  );
  const owner = root.children[1] as Row;
  assert.deepEqual(
    owner.children.map((f) => [f.label, f.description, f.source]),
    [
      ['InlinedCallFrame ×2', 'runtime frame', false],
      ['System.Threading.Thread.Sleep(Int32)', 'System.Private.CoreLib.dll · +0x0', false],
      ['Hold.Own(System.Object)', 'breadth.dll · Hold.cs:47', true],
      ['… 2 more frames', '', false],
    ],
  );
  assert.deepEqual(owner.children[2]?.ref, { kind: 'frame', run: 3, group: 0, frame: 3 });
  assert.match(owner.children[2]?.tooltip ?? '', /\/src\/breadth\/Hold\.cs:47/);
  assert.match(owner.tooltip, /OS thread ids 4004/);
  // A mini dump: no locks recorded; a dump file's capture.
  const mini = { ...t, locks: [], locksAvailable: false };
  const r2 = threadsRows([{ key: 4, at, taken: false, result: mini, groups: groupThreads(mini.threads, []) }]);
  assert.equal(r2[0]?.label, 'Threads of app.dmp');
  assert.equal(r2[0]?.description, '7 threads');
  assert.equal(r2[0]?.children[0]?.label, 'Locks: not recorded (a mini dump has none)');
});

test('trace: cpu summary', () => {
  const rows = traceRows([{ key: 5, at, taken: true, result: parseTrace(first('trace_cpu')) }]);
  assertPlain(rows);
  const root = rows[0] as Row;
  assert.equal(root.label, 'CPU trace 14:03:12 · 10.0 s');
  assert.equal(root.description, '9,812 samples · 12 threads');
  assert.match(root.tooltip, /^\/home\/u\/\.eyedbg\/traces\/.*\.nettrace\nprivate to you/);
  assert.match(root.tooltip, /the trace ran its full duration/);
  assert.match(root.tooltip, /21 frames without a method name/);
  assert.deepEqual(
    root.children.map((c) => [c.label, c.description, c.expanded]),
    [
      ['Hottest methods (exclusive)', '6,001 samples', true],
      ['Inclusive (callers that lead there)', '6,001 samples', false],
      ['Waiting or in native code', '3,811 samples', false],
      ['Garbage collections', '37 collections', true],
    ],
  );
  assert.deepEqual(
    root.children[0]?.children.map((m) => [m.label, m.description]),
    [
      ['Burn.Spin(int32)', 'breadth · 4,120 samples · 68.7%'],
      ['System.Threading.Thread.<PollGC>g__PollGCWorker|67_0()', 'System.Private.CoreLib · 1,210 samples · 20.2%'],
      ['App.Evil [2J .Run()', 'breadth · 671 samples · 11.2%'],
      ['… 2 more', ''],
    ],
  );
  assert.deepEqual(
    root.children[2]?.children.map((m) => [m.label, m.description]),
    [
      ['System.Threading.Thread.Sleep(int32)', 'System.Private.CoreLib · 3,790 samples · 99.4%'],
      ['(unresolved)', '21 samples · 0.6%'],
    ],
    "percent of the waiting samples, as the CLI's text",
  );
  assert.deepEqual(
    root.children[3]?.children.map((m) => [m.label, m.description]),
    [
      ['gen0', '35'],
      ['gen1', '2'],
      ['gen2', '0'],
      ['induced by GC.Collect', '1'],
      ['pauses', '12.3 ms total · 1.2 ms max'],
    ],
  );
});

test('trace: gc summaries, the empty one, a file', () => {
  const [gc, empty] = golden('trace_gc').map(parseTrace);
  assert.ok(gc !== undefined && empty !== undefined);
  const rows = traceRows([
    { key: 6, at, taken: false, result: gc },
    { key: 7, at, taken: true, result: empty },
  ]);
  assertPlain(rows);
  const [e, g] = rows;
  assert.equal(e?.label, 'GC trace 14:03:12 · 3.0 s');
  assert.equal(e?.description, 'nothing recorded');
  assert.deepEqual(
    e?.children.map((c) => c.label),
    ['Nothing recorded: no CPU samples, GC or allocation events'],
  );
  assert.equal(g?.label, 'Trace file gc.nettrace · 10.0 s');
  assert.equal(g?.description, '3,048 collections');
  assert.match(g?.tooltip ?? '', /^\/tmp\/my traces\/gc\.nettrace\nof pid 77 \(evil \[2J\)/);
  assert.match(g?.tooltip ?? '', /the process exited: shorter than asked/);
  assert.match(g?.tooltip ?? '', /5 events lost/);
  const alloc = g?.children.find((c) => c.label === 'Allocations');
  assert.equal(alloc?.description, '≈ 17.8 GiB (sampled about every 100 KB)');
  assert.deepEqual(
    alloc?.children.map((c) => [c.label, c.description]),
    [
      ['System.Byte[]', '≈ 17.7 GiB · 185,000 ticks'],
      ['App.Evil ]0;x Type', '≈ 90.6 MiB · 900 ticks'],
      ['System.String', '≈ 4.8 MiB · 100 ticks'],
      ['… 1 more type', ''],
    ],
  );
});
