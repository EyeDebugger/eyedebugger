// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The .NET views' command lines and readers. The fixtures are the CLI's own
// JSON goldens (internal/cli/testdata): a change of the CLI's JSON breaks
// these tests too.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { test } from 'node:test';
import { errorOf } from '../../src/core/cli';
import {
  CounterBoard,
  checkTargetArg,
  checkTypeName,
  dumpArgs,
  gcrootArgs,
  heapArgs,
  isLocalAbsolute,
  LineSplitter,
  nextRunState,
  parseDuration,
  psArgs,
  type Resolved,
  targetArgs,
  threadsArgs,
  traceArgs,
  traceFileArgs,
  watchArgs,
} from '../../src/core/dotnet';
import {
  parseDump,
  parseHeap,
  parsePs,
  parseThreads,
  parseTrace,
  parseWatchLine,
  type Sample,
} from '../../src/core/dotnetParse';
import { type DapMessage, EyedbgError } from '../../src/core/protocol';

// The bundle runs from out/unit/.
const repo = path.resolve(__dirname, '..', '..', '..', '..');

/** golden reads internal/cli/testdata/dotnet_NAME_json.golden: its JSON lines. */
export function golden(name: string): Record<string, unknown>[] {
  const text = fs.readFileSync(path.join(repo, 'internal', 'cli', 'testdata', `dotnet_${name}_json.golden`), 'utf8');
  return text
    .split(/\r?\n/)
    .filter((l) => l.trim() !== '')
    .map((l) => JSON.parse(l) as Record<string, unknown>);
}

export function first(name: string): Record<string, unknown> {
  const v = golden(name)[0];
  assert.ok(v !== undefined, name);
  return v;
}

const session: Resolved = { kind: 'session', id: 's-k3f9' };
const pid: Resolved = { kind: 'pid', pid: 4321, name: 'dotnet' };

test('target args: a session names the client, a pid does not', () => {
  assert.deepEqual(targetArgs(session, 'human:me'), ['--session=s-k3f9', '--as=human:me']);
  assert.deepEqual(targetArgs(pid, 'human:me'), ['--pid=4321']);
});

test('checkTargetArg', () => {
  const cases: [unknown, string | undefined][] = [
    [{ auto: true }, 'auto'],
    [{ session: 's-k3f9' }, 'session'],
    [{ pid: 4321 }, 'pid'],
    [{ pid: 4321, name: 'api' }, 'pid'],
    [{ pid: 2 ** 31 - 1 }, 'pid'],
    [{ pid: 0 }, undefined],
    [{ pid: -1 }, undefined],
    [{ pid: 2 ** 31 }, undefined],
    [{ pid: 1.5 }, undefined],
    [{ pid: '4321' }, undefined],
    [{ session: 's-K3F9' }, undefined],
    [{ session: '--pid=1' }, undefined],
    [{ session: 's-k3f9', pid: 1 }, undefined],
    [{ auto: true, pid: 1 }, undefined],
    [{ auto: 'yes' }, undefined],
    [{}, undefined],
    [null, undefined],
    ['s-k3f9', undefined],
  ];
  for (const [arg, kind] of cases) {
    const r = checkTargetArg(arg);
    assert.equal(r.ok ? r.value.kind : undefined, kind, JSON.stringify(arg));
  }
});

test('argv: flags bound with =, paths after --, --json and the CLI timeout always', () => {
  const cases: [string, { argv: string[]; timeoutMs: number } | string[], string[], number | undefined][] = [
    ['ps', psArgs(), ['dotnet', 'ps', '--json', '--timeout=30s'], 45_000],
    [
      'watch',
      watchArgs(session, 'human:me'),
      ['dotnet', 'counters', '--session=s-k3f9', '--as=human:me', '--watch', '--json', '--interval=1s'],
      undefined,
    ],
    [
      'dump',
      dumpArgs(pid, 'human:me'),
      ['dotnet', 'dump', '--pid=4321', '--type=heap', '--json', '--timeout=120s'],
      135_000,
    ],
    [
      'heap',
      heapArgs('/d/x.dmp'),
      ['dotnet', 'heap', '--json', '--top=50', '--timeout=300s', '--', '/d/x.dmp'],
      315_000,
    ],
    [
      'heap of a dash path',
      heapArgs('-weird.dmp'),
      ['dotnet', 'heap', '--json', '--top=50', '--timeout=300s', '--', '-weird.dmp'],
      315_000,
    ],
    [
      'gcroot',
      gcrootArgs('/d/x.dmp', 'System.Collections.Generic.List<Retained>'),
      [
        'dotnet',
        'heap',
        '--json',
        '--top=1',
        '--gcroot=System.Collections.Generic.List<Retained>',
        '--paths=3',
        '--timeout=300s',
        '--',
        '/d/x.dmp',
      ],
      315_000,
    ],
    [
      'gcroot of a hostile type name',
      gcrootArgs('/d/x.dmp', '--x $(zap) a b'),
      [
        'dotnet',
        'heap',
        '--json',
        '--top=1',
        '--gcroot=--x $(zap) a b',
        '--paths=3',
        '--timeout=300s',
        '--',
        '/d/x.dmp',
      ],
      315_000,
    ],
    [
      'threads of a target',
      threadsArgs({ target: session, client: 'human:me' }),
      ['dotnet', 'threads', '--session=s-k3f9', '--as=human:me', '--json', '--frames=20', '--timeout=300s'],
      315_000,
    ],
    [
      'threads of a dump',
      threadsArgs({ dump: '/d/x.dmp' }),
      ['dotnet', 'threads', '--json', '--frames=20', '--timeout=300s', '--', '/d/x.dmp'],
      315_000,
    ],
    [
      'trace',
      traceArgs(pid, 'human:me', 'cpu', 10),
      ['dotnet', 'trace', '--pid=4321', '--json', '--profile=cpu', '--duration=10s', '--top=20', '--timeout=70s'],
      85_000,
    ],
    [
      'gc trace of a session',
      traceArgs(session, 'human:me', 'gc', 300),
      [
        'dotnet',
        'trace',
        '--session=s-k3f9',
        '--as=human:me',
        '--json',
        '--profile=gc',
        '--duration=300s',
        '--top=20',
        '--timeout=360s',
      ],
      375_000,
    ],
    [
      'trace file',
      traceFileArgs('/t/a b.nettrace'),
      ['dotnet', 'trace', '--json', '--top=20', '--timeout=300s', '--', '/t/a b.nettrace'],
      315_000,
    ],
  ];
  for (const [name, got, argv, timeoutMs] of cases) {
    if (Array.isArray(got)) {
      assert.deepEqual(got, argv, name);
    } else {
      assert.deepEqual(got.argv, argv, name);
      assert.equal(got.timeoutMs, timeoutMs, name);
    }
  }
});

test('argv: refused values', () => {
  for (const [name, fn] of [
    ['gcroot NUL', () => gcrootArgs('/d/x.dmp', 'A\0B')],
    ['gcroot empty', () => gcrootArgs('/d/x.dmp', '')],
    ['gcroot address-like', () => gcrootArgs('/d/x.dmp', '0x1')],
    ['gcroot address-like upper', () => gcrootArgs('/d/x.dmp', '0X13de')],
    ['heap NUL path', () => heapArgs('/d/x\0.dmp')],
    ['heap empty path', () => heapArgs('')],
    ['threads NUL path', () => threadsArgs({ dump: '\0' })],
    ['trace file NUL', () => traceFileArgs('/t/\0')],
  ] as const) {
    assert.throws(fn, (e: unknown) => e instanceof EyedbgError && e.code === 'INVALID_REQUEST', name);
  }
  assert.equal(checkTypeName('Retained'), '');
  assert.equal(checkTypeName('x0x1'), '');
  assert.match(checkTypeName('0x1'), /address/);
});

test('parseDuration', () => {
  const cases: [string, number | undefined][] = [
    ['10s', 10],
    [' 3s ', 3],
    ['1s', 1],
    ['2m', 120],
    ['5m', 300],
    ['5M', 300],
    ['300s', 300],
    ['301s', undefined],
    ['6m', undefined],
    ['0s', undefined],
    ['10', undefined],
    ['1.5s', undefined],
    ['-1s', undefined],
    ['10 s', 10],
    ['s', undefined],
    ['', undefined],
  ];
  for (const [s, want] of cases) {
    const r = parseDuration(s);
    assert.equal(r.ok ? r.value : undefined, want, JSON.stringify(s));
  }
});

test('parsePs: the golden, hostile names kept raw', () => {
  assert.deepEqual(parsePs(first('ps')), [
    { pid: 812, name: 'dotnet', session: 's-k3f9' },
    { pid: 4321, name: 'api\u001b[2J', session: '' },
    { pid: 70000, name: '', session: '' },
  ]);
  assert.deepEqual(parsePs({ processes: [{ pid: 0 }, { pid: 'x' }, 7, { pid: 5, name: 3 }] }), [
    { pid: 5, name: '', session: '' },
  ]);
  assert.deepEqual(parsePs({}), []);
});

test('parseWatchLine: samples, errors and noise', () => {
  const lines = fs
    .readFileSync(path.join(repo, 'internal', 'cli', 'testdata', 'dotnet_counters_watch_json.golden'), 'utf8')
    .split('\n');
  const samples = lines.map(parseWatchLine).filter((s): s is Sample => s !== undefined);
  assert.equal(samples.length, 2);
  assert.equal(samples[0]?.elapsedMs, 1100);
  assert.deepEqual(samples[0]?.counters[0], { name: 'cpu-usage', unit: '%', kind: 'gauge', value: 0.5 });
  assert.equal(samples[1]?.counters.length, 6);
  assert.equal(parseWatchLine(''), undefined);
  assert.equal(parseWatchLine('not json'), undefined);
  assert.equal(parseWatchLine('[1]'), undefined);
  assert.deepEqual(
    parseWatchLine(
      '{"schema":1,"elapsedMs":1,"counters":[{"name":"a","kind":"rate","value":1},{"name":"b","kind":"sum","value":"x"},{"name":"b","kind":"gauge","value":2}]}',
    )?.counters,
    [{ name: 'b', unit: '', kind: 'sum', value: 0 }],
  );
  // The error line is no sample (Eyedbg.stream reads it with errorOf).
  const errorLine =
    '{"schema":1,"error":{"code":"NOT_RUNNING","message":"session s-k3f9 is stopped","hint":"continue it"}}';
  assert.equal(parseWatchLine(errorLine), undefined);
  const e = errorOf(JSON.parse(errorLine));
  assert.ok(e instanceof EyedbgError && e.code === 'NOT_RUNNING' && e.hint === 'continue it');
  assert.equal(errorOf({ schema: 1, counters: [] }), undefined);
  assert.equal(errorOf({ error: 'x' }), undefined);
  assert.equal(errorOf({ error: {} })?.code, 'INTERNAL');
});

test('parseDump / parseHeap / parseThreads / parseTrace: the goldens', () => {
  const d = parseDump(first('dump'));
  assert.equal(d.path, '/home/u/.eyedbg/dumps/4321-20260925T150300Z-heap-1f3a9c0e.dmp');
  assert.equal(d.bytes, 358305992);
  assert.equal(d.private, true);
  assert.deepEqual(d.target, { pid: 4321, name: 'dotnet', session: 's-k3f9' });
  assert.throws(() => parseDump({}), EyedbgError);

  const h = parseHeap(first('heap'));
  assert.equal(h.objects, 5332);
  assert.equal(h.typeCount, 93);
  assert.equal(h.typesOmitted, 89);
  assert.deepEqual(h.types[0], { name: 'Retained', count: 5000, bytes: 240000 });
  assert.equal(h.generations.length, 6);
  assert.equal(h.gcroot?.type, 'Retained');
  assert.equal(h.gcroot?.instances, 5000);
  assert.equal(h.gcroot?.complete, false);
  assert.deepEqual(h.gcroot?.paths[0]?.root, { kind: 'static', name: 'Holder.Keep', thread: undefined, frame: '' });
  assert.equal(h.gcroot?.paths[0]?.chain[2]?.address, '0x13de6eab8');
  assert.equal(h.gcroot?.paths[1]?.chainOmitted, 3);
  assert.equal(h.gcroot?.paths[1]?.root.thread, 1);

  const t = parseThreads(first('threads'));
  assert.equal(t.threads.length, 7);
  assert.equal(t.dump.taken, false);
  assert.equal(t.locksAvailable, true);
  assert.deepEqual(t.locks[0], { object: '0x13de6eb78', type: 'System.Object', owner: 4, waiting: 1 });
  assert.equal(t.locks[1]?.owner, undefined);
  const own = t.threads[0]?.frames[3];
  assert.deepEqual(own, {
    kind: 'managed',
    method: 'Hold.Own(System.Object)',
    module: 'breadth.dll',
    ilOffset: 27,
    file: '/src/breadth/Hold.cs',
    line: 47,
  });
  assert.equal(t.threads[0]?.frames[0]?.kind, 'runtime');
  assert.equal(t.threads[2]?.exception, 'System.InvalidOperationException');

  const cpu = parseTrace(first('trace_cpu'));
  assert.equal(cpu.cpu?.samples, 9812);
  assert.equal(cpu.cpu?.exclusive[0]?.method, 'Burn.Spin(int32)');
  assert.equal(cpu.cpu?.waiting[1]?.module, '');
  assert.equal(cpu.gc?.gen0, 35);
  assert.equal(cpu.allocations, undefined);
  const [gc, empty] = golden('trace_gc').map(parseTrace);
  assert.equal(gc?.cpu, undefined);
  assert.equal(gc?.allocations?.types.length, 3);
  assert.equal(gc?.trace.private, false);
  assert.equal(empty?.cpu, undefined);
  assert.equal(empty?.gc, undefined);
  assert.equal(empty?.allocations, undefined);
  assert.equal(empty?.durationMs, 3001);
});

test('readers cap lists at 1000 items', () => {
  const types = Array.from({ length: 1500 }, (_, i) => ({ name: `T${i}`, count: 1, bytes: 1 }));
  assert.equal(parseHeap({ types }).types.length, 1000);
});

test('CounterBoard: deltas, totals, reset', () => {
  const lines = golden('counters_watch');
  const board = new CounterBoard();
  const sample = (v: Record<string, unknown>) => {
    const s = parseWatchLine(JSON.stringify(v));
    assert.ok(s !== undefined);
    return s;
  };
  board.add(sample(lines[0] as Record<string, unknown>));
  const at = (name: string) => board.rows().find((r) => r.name === name);
  assert.equal(at('cpu-usage')?.delta, undefined, 'no delta on the first sample');
  assert.equal(at('alloc-rate')?.total, 1024);
  board.add(sample(lines[1] as Record<string, unknown>));
  assert.equal(board.samples, 2);
  assert.ok(Math.abs((at('cpu-usage')?.delta ?? 0) - 11.845) < 1e-9);
  assert.equal(at('working-set')?.delta, 0);
  assert.equal(at('alloc-rate')?.value, 2048);
  assert.equal(at('alloc-rate')?.total, 3072);
  assert.equal(at('exception-count')?.total, 2);
  assert.equal(at('alloc-rate')?.delta, undefined, 'a sum has no delta');
  assert.deepEqual(
    board.rows().map((r) => r.name),
    ['cpu-usage', 'working-set', 'alloc-rate', 'exception-count', 'aa-custom', 'zz-custom'],
    "the CLI's order",
  );
  board.reset();
  assert.equal(board.samples, 0);
  assert.equal(board.rows().length, 0);
  board.add(sample(lines[1] as Record<string, unknown>));
  assert.equal(at('cpu-usage')?.delta, undefined, 'deltas restart');
  assert.equal(at('alloc-rate')?.total, 2048, 'totals restart');
});

test('LineSplitter: chunks, CRLF, the end, oversize', () => {
  const s = new LineSplitter(10);
  assert.deepEqual(s.push('ab'), []);
  assert.deepEqual(s.push('c\nde'), ['abc']);
  assert.deepEqual(s.push('\r\n\nx\r'), ['de', '']);
  assert.deepEqual(s.push('\ny'), ['x']);
  assert.deepEqual(s.end(), ['y']);
  assert.deepEqual(s.end(), []);
  assert.deepEqual(s.push('0123456789\n'), ['0123456789']);
  assert.throws(() => s.push('01234567890'), EyedbgError);
  const t = new LineSplitter(10);
  t.push('012345');
  assert.throws(() => t.push('67890\n'), EyedbgError, 'a line finished in a later chunk');
  const many = new LineSplitter(1 << 20);
  const text = '{"a":1}\n'.repeat(10_000);
  let n = 0;
  for (let i = 0; i < text.length; i += 777) {
    n += many.push(text.slice(i, i + 777)).length;
  }
  assert.equal(n, 10_000);
});

function dap(x: Partial<DapMessage>): DapMessage {
  return {
    seq: 1,
    type: 'event',
    command: '',
    event: '',
    requestSeq: 0,
    success: true,
    message: '',
    body: undefined,
    arguments: undefined,
    ...x,
  };
}

test('nextRunState', () => {
  const cases: [boolean | undefined, Partial<DapMessage>, boolean | undefined][] = [
    [undefined, { event: 'stopped' }, false],
    [false, { event: 'continued' }, true],
    [true, { event: 'terminated' }, false],
    [true, { event: 'exited' }, false],
    [undefined, { event: 'output' }, undefined],
    [false, { type: 'response', command: 'continue' }, true],
    [false, { type: 'response', command: 'next' }, true],
    [false, { type: 'response', command: 'stepIn' }, true],
    [false, { type: 'response', command: 'stepOut' }, true],
    [false, { type: 'response', command: 'stepBack' }, true],
    [false, { type: 'response', command: 'reverseContinue' }, true],
    [false, { type: 'response', command: 'goto' }, true],
    [false, { type: 'response', command: 'continue', success: false }, false],
    [false, { type: 'response', command: 'stackTrace' }, false],
    [true, { type: 'request', command: 'pause' }, true],
  ];
  for (const [prev, m, want] of cases) {
    assert.equal(nextRunState(prev, dap(m)), want, JSON.stringify(m));
  }
});

test("isLocalAbsolute: the helper's cases (SourceLinesTests.OnlyLocalAbsolutePaths) and more", () => {
  const cases: [string, boolean, boolean][] = [
    // helpers/dotnet/tests/EyeDbg.DotnetHelper.Tests/SourceLinesTests.cs, in order.
    ['C:\\app\\app.dll', true, true],
    ['C:/app/app.dll', true, true],
    ['\\\\attacker\\share\\app.dll', true, false],
    ['//attacker/share/app.dll', true, false],
    ['\\\\?\\C:\\app\\app.dll', true, false],
    ['\\\\.\\pipe\\x.dll', true, false],
    ['\\app\\app.dll', true, false],
    ['C:app.dll', true, false],
    ['app.dll', true, false],
    ['/app/app.dll', false, true],
    ['//attacker/share/app.dll', false, false],
    ['\\\\attacker\\share\\app.dll', false, false],
    ['app.dll', false, false],
    ['', false, false],
    ['/app/a\0.dll', false, false],
    // More.
    ['C:foo', true, false],
    ['\\\\?\\C:\\x', true, false],
    ['//srv/s', true, false],
    ['\\\\.\\pipe\\x', true, false],
    ['/\\srv\\s', true, false],
    ['\\/srv/s', false, false],
    ['/app/app.dll', true, false],
    ['C:\\app', false, false],
    ['z:\\a', true, true],
    ['1:\\a', true, false],
    ['é:\\a', true, false],
    ['C:', true, false],
    ['   ', false, false],
    [' /x', false, false],
    ['C:\\a\0', true, false],
    ['/', false, true],
  ];
  for (const [p, windows, want] of cases) {
    assert.equal(isLocalAbsolute(p, windows), want, `${JSON.stringify(p)} windows=${windows}`);
  }
});
