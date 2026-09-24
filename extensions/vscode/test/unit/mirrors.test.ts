// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { MirrorModel } from '../../src/core/mirrors';
import { type DapMessage, parseDap } from '../../src/core/protocol';

function msg(x: Record<string, unknown>): DapMessage {
  const m = parseDap(x);
  assert.ok(m, JSON.stringify(x));
  return m;
}

function bpEvent(reason: string, breakpoint: Record<string, unknown>): DapMessage {
  return msg({ seq: 0, type: 'event', event: 'breakpoint', body: { reason, breakpoint } });
}

function mirror(id: number, line: number, path = '/w/app.py', extra: Record<string, unknown> = {}) {
  return { id, verified: true, line, column: 1, source: { path }, message: `agent's breakpoint ${id}`, ...extra };
}

function setBps(seq: number, path: string, entries: Record<string, unknown>[]): DapMessage {
  return msg({
    seq,
    type: 'request',
    command: 'setBreakpoints',
    arguments: { source: { path }, breakpoints: entries },
  });
}

function answer(requestSeq: number, breakpoints: Record<string, unknown>[], success = true): DapMessage {
  return msg({
    seq: 0,
    type: 'response',
    command: 'setBreakpoints',
    request_seq: requestSeq,
    success,
    body: { breakpoints },
  });
}

function ids(m: MirrorModel): [number, string, number][] {
  return m.all().map((x) => [x.id, x.path, x.line]);
}

test('new and changed at column 1 set a mirror; other columns are ignored', () => {
  const m = new MirrorModel();
  assert.equal(m.fromAdapter(bpEvent('new', mirror(3, 12))), true);
  assert.equal(m.fromAdapter(bpEvent('new', { ...mirror(4, 13), column: undefined })), false);
  assert.equal(m.fromAdapter(bpEvent('new', { ...mirror(5, 14), column: 2 })), false);
  assert.equal(m.fromAdapter(bpEvent('new', { ...mirror(6, 15), source: undefined })), false);
  assert.equal(m.fromAdapter(bpEvent('new', { ...mirror(-1, 15) })), false);
  assert.deepEqual(ids(m), [[3, '/w/app.py', 12]]);
  assert.equal(m.get(3)?.message, "agent's breakpoint 3");

  // changed: VS Code applies it to breakpoints it knows only.
  assert.equal(m.fromAdapter(bpEvent('changed', mirror(9, 20))), false);
  assert.equal(m.fromAdapter(bpEvent('changed', mirror(3, 14))), true);
  assert.deepEqual(ids(m), [[3, '/w/app.py', 14]]);
  // An unverified one stays where VS Code had it.
  m.fromAdapter(bpEvent('changed', mirror(3, 30, '/w/app.py', { verified: false })));
  assert.deepEqual(ids(m), [[3, '/w/app.py', 14]]);
  assert.equal(m.get(3)?.verified, false);
  // A changed without the mark column (the client's own) is ignored.
  assert.equal(m.fromAdapter(bpEvent('changed', { ...mirror(3, 40), column: undefined })), false);
});

test('removed deletes', () => {
  const m = new MirrorModel();
  m.fromAdapter(bpEvent('new', mirror(3, 12)));
  m.fromAdapter(bpEvent('new', mirror(4, 13)));
  assert.equal(m.fromAdapter(bpEvent('removed', { id: 3 })), true);
  assert.equal(m.fromAdapter(bpEvent('removed', { id: 3 })), false);
  assert.equal(m.fromAdapter(bpEvent('removed', { id: -2 })), false);
  assert.deepEqual(ids(m), [[4, '/w/app.py', 13]]);
});

test('a setBreakpoints response re-derives the mirrors of its path', () => {
  const m = new MirrorModel();
  m.fromAdapter(bpEvent('new', mirror(3, 12)));
  m.fromAdapter(bpEvent('new', mirror(4, 13)));
  m.fromAdapter(bpEvent('new', mirror(5, 7, '/w/other.py')));
  m.fromEditor(setBps(10, '/w/app.py', [{ line: 5 }, { line: 12, column: 1 }, { line: 20, column: 1 }]));
  const changed = m.fromAdapter(
    answer(10, [
      { id: 1, verified: true, line: 5 },
      mirror(3, 12),
      { id: -1, verified: false, line: 20, message: 'eyedbg: a leftover copy…' },
    ]),
  );
  assert.equal(changed, true);
  // Echo kept, negative id ignored, 4 (not answered) dropped, other path untouched.
  assert.deepEqual(ids(m), [
    [3, '/w/app.py', 12],
    [5, '/w/other.py', 7],
  ]);
});

test('an echo is keyed by the request path, and an unverified one stays at the sent line', () => {
  const m = new MirrorModel();
  m.fromEditor(
    setBps(1, '/link/app.py', [
      { line: 12, column: 1 },
      { line: 30, column: 1 },
    ]),
  );
  m.fromAdapter(answer(1, [mirror(7, 12, '/real/app.py'), mirror(8, 31, '/link/app.py', { verified: false })]));
  assert.deepEqual(ids(m), [
    [7, '/link/app.py', 12],
    [8, '/link/app.py', 30],
  ]);
});

test('unknown request_seq and other responses are ignored', () => {
  const m = new MirrorModel();
  m.fromAdapter(bpEvent('new', mirror(3, 12)));
  assert.equal(m.fromAdapter(answer(99, [])), false);
  assert.equal(m.fromAdapter(msg({ seq: 0, type: 'response', command: 'next', request_seq: 1, success: true })), false);
  m.fromEditor(msg({ seq: 2, type: 'request', command: 'setBreakpoints', arguments: { source: {} } }));
  assert.equal(m.fromAdapter(answer(2, [])), false);
  assert.deepEqual(ids(m), [[3, '/w/app.py', 12]]);
  // A response is paired once.
  m.fromEditor(setBps(4, '/w/app.py', [{ line: 12, column: 1 }]));
  m.fromAdapter(answer(4, [mirror(3, 12)]));
  assert.equal(m.fromAdapter(answer(4, [])), false);
  assert.deepEqual(ids(m), [[3, '/w/app.py', 12]]);
});

test('a refused setBreakpoints keeps only the mirrors the request sent at column 1', () => {
  const m = new MirrorModel();
  m.fromAdapter(bpEvent('new', mirror(3, 12)));
  m.fromAdapter(bpEvent('new', mirror(4, 13)));
  m.fromAdapter(bpEvent('new', mirror(5, 7, '/w/other.py')));
  m.fromEditor(setBps(3, '/w/app.py', [{ line: 12, column: 1 }, { line: 13 }]));
  assert.equal(
    m.fromAdapter(msg({ seq: 0, type: 'response', command: 'setBreakpoints', request_seq: 3, success: false })),
    true,
  );
  assert.deepEqual(ids(m), [
    [3, '/w/app.py', 12],
    [5, '/w/other.py', 7],
  ]);
});

test('terminated and the disconnect response clear', () => {
  for (const end of [
    msg({ seq: 0, type: 'event', event: 'terminated' }),
    msg({ seq: 0, type: 'response', command: 'disconnect', request_seq: 1, success: true }),
  ]) {
    const m = new MirrorModel();
    m.fromAdapter(bpEvent('new', mirror(3, 12)));
    m.fromEditor(setBps(5, '/w/app.py', []));
    assert.equal(m.fromAdapter(end), true);
    assert.deepEqual(m.all(), []);
    assert.equal(m.fromAdapter(answer(5, [mirror(3, 12)])), false, 'pending requests are forgotten');
  }
});

test('two paths of one file are kept apart', () => {
  const m = new MirrorModel();
  m.fromAdapter(bpEvent('new', mirror(3, 12, '/real/app.py')));
  m.fromAdapter(bpEvent('new', mirror(4, 20, '/link/app.py')));
  m.fromEditor(setBps(1, '/link/app.py', []));
  m.fromAdapter(answer(1, []));
  assert.deepEqual(ids(m), [[3, '/real/app.py', 12]]);
});

test('the mirror follows its breakpoint (the retraction before a disconnect response)', () => {
  const m = new MirrorModel();
  m.fromAdapter(bpEvent('new', mirror(3, 12)));
  m.fromEditor(msg({ seq: 9, type: 'request', command: 'disconnect', arguments: { restart: true } }));
  m.fromAdapter(bpEvent('removed', { id: 3 }));
  assert.deepEqual(m.all(), []);
});

test('paths VS Code takes for one file share their mirrors (a Windows drive letter)', () => {
  const m = new MirrorModel((p) => p.replace(/^[A-Z]:/, (d) => d.toLowerCase()));
  m.fromAdapter(bpEvent('new', mirror(3, 12, 'C:\\w\\app.py')));
  m.fromEditor(setBps(1, 'c:\\w\\app.py', []));
  assert.equal(m.fromAdapter(answer(1, [])), true);
  assert.deepEqual(m.all(), []);
});
