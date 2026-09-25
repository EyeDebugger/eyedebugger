// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { ActivityLog, maxEntries } from '../../src/core/activity';
import { type DapMessage, parseDap, parseEvent, type SessionEvent } from '../../src/core/protocol';
import { activityLabel, activityTooltip } from '../../src/core/render';

function msg(x: Record<string, unknown>): DapMessage {
  const m = parseDap(x);
  assert.ok(m, JSON.stringify(x));
  return m;
}

function ev(x: Record<string, unknown>): SessionEvent {
  const e = parseEvent({ seq: 1, time: '2026-09-24T10:11:12Z', ...x });
  assert.ok(e);
  return e;
}

const exec = (action: string, text = '') => ev({ kind: 'exec', client: 'agent', action, text });
const stopped = (threadId: number, reason = 'step', hint = true) =>
  msg({ seq: 0, type: 'event', event: 'stopped', body: { threadId, reason, preserveFocusHint: hint } });
const stackReq = (seq: number, args: Record<string, unknown>) =>
  msg({ seq, type: 'request', command: 'stackTrace', arguments: args });
const stackResp = (requestSeq: number, frames: unknown[], success = true) =>
  msg({
    seq: 0,
    type: 'response',
    command: 'stackTrace',
    request_seq: requestSeq,
    success,
    body: { stackFrames: frames },
  });
const frame = (path: string, line: number) => ({ id: 1, name: 'f', line, column: 1, source: { path } });
const request = (command: string) => msg({ seq: 9, type: 'request', command, arguments: { threadId: 1 } });
const event = (name: string) => msg({ seq: 0, type: 'event', event: name, body: {} });

/** stepTo runs another client's action to a stop at path:line on thread 1; returns the reveal. */
function stepTo(log: ActivityLog, action: string, path: string, line: number, hint = true, reason = 'step') {
  log.fromAdapter(event('continued'));
  const entry = log.add(exec(action), `agent: ${action}`);
  assert.deepEqual(log.fromAdapter(stopped(1, reason, hint)), { changed: true, reveal: undefined });
  log.fromEditor(stackReq(40, { threadId: 1, startFrame: 0, levels: 1 }));
  return { entry, update: log.fromAdapter(stackResp(40, [frame(path, line)])) };
}

test('another client step lands on its stop, and a hinted stop is revealed', () => {
  const log = new ActivityLog();
  const { entry, update } = stepTo(log, 'next', '/w/app.py', 13);
  assert.deepEqual(update, { changed: true, reveal: { path: '/w/app.py', line: 13 } });
  assert.deepEqual(entry.location, { path: '/w/app.py', line: 13 });
  assert.equal(entry.stopReason, 'step');
  assert.equal(activityLabel(entry, '/w'), 'agent stepped over → app.py:13');
  assert.equal(activityTooltip(entry), 'agent: next\nStopped: step at /w/app.py:13');
  // A second stackTrace response (e.g. the rest of the stack) changes nothing.
  assert.deepEqual(log.fromAdapter(stackResp(40, [frame('/w/other.py', 2)])), { changed: false, reveal: undefined });
  assert.deepEqual(entry.location, { path: '/w/app.py', line: 13 });
});

test('win32: a step is shown relative to the workspace folder whatever the drive letter case and separators', () => {
  // VS Code's workspaceFolder.uri.fsPath has a lower-case drive letter; the
  // adapter reports the path as the program was given (upper case), or with
  // forward slashes.
  const base = 'c:\\Users\\me\\ws';
  for (const [path, want] of [
    ['C:\\Users\\me\\ws\\app.py', 'app.py'],
    ['c:\\users\\ME\\WS\\app.py', 'app.py'],
    ['C:/Users/me/ws/app.py', 'app.py'],
    ['C:\\Users\\me\\ws\\sub\\app.py', 'sub\\app.py'],
    ['C:\\Users\\me\\ws2\\app.py', 'C:\\Users\\me\\ws2\\app.py'],
    ['D:\\Users\\me\\ws\\app.py', 'D:\\Users\\me\\ws\\app.py'],
  ] as const) {
    const log = new ActivityLog();
    const { entry } = stepTo(log, 'next', path, 23);
    assert.equal(activityLabel(entry, base), `agent stepped over → ${want}:23`, path);
    assert.equal(
      activityLabel(entry, `${base}\\`),
      `agent stepped over → ${want}:23`,
      `${path} (base with a separator)`,
    );
  }
  const bp = { id: 2, file: 'C:\\Users\\me\\ws\\app.py', line: 15, requestedLine: 15, owner: 'agent', verified: true };
  const e = new ActivityLog().add(ev({ kind: 'breakpoint', client: 'agent', action: 'added', breakpoint: bp }), '');
  assert.equal(activityLabel(e, base), 'bp 2 added by agent: app.py:15');
  // POSIX paths stay case-sensitive.
  const { entry } = stepTo(new ActivityLog(), 'next', '/W/app.py', 3);
  assert.equal(activityLabel(entry, '/w'), 'agent stepped over → /W/app.py:3');
});

test('an unhinted stop gets its location but is not revealed', () => {
  const log = new ActivityLog();
  const { entry, update } = stepTo(log, 'continue', '/w/app.py', 20, false, 'breakpoint');
  assert.deepEqual(update, { changed: true, reveal: undefined });
  assert.equal(activityLabel(entry, '/w'), 'agent continued → app.py:20 (breakpoint)');
});

test('a hinted stop without an entry is still revealed', () => {
  const log = new ActivityLog();
  log.fromAdapter(stopped(1));
  log.fromEditor(stackReq(5, { threadId: 1 }));
  assert.deepEqual(log.fromAdapter(stackResp(5, [frame('/w/a.py', 3)])), {
    changed: false,
    reveal: { path: '/w/a.py', line: 3 },
  });
});

test('labels for each exec action and stop reason', () => {
  const cases: [string, string, string][] = [
    ['next', 'step', 'agent stepped over → a.py:1'],
    ['stepIn', 'step', 'agent stepped into → a.py:1'],
    ['stepOut', 'step', 'agent stepped out → a.py:1'],
    ['continue', 'exception', 'agent continued → a.py:1 (exception)'],
    ['runUntil', 'breakpoint', 'agent ran to a line → a.py:1 (breakpoint)'],
    ['pause', 'pause', 'agent paused → a.py:1'],
  ];
  for (const [action, reason, want] of cases) {
    const log = new ActivityLog();
    const { entry } = stepTo(log, action, '/w/a.py', 1, true, reason);
    assert.equal(activityLabel(entry, '/w'), want, action);
  }
  const log = new ActivityLog();
  assert.equal(activityLabel(log.add(exec('eval', 'x()'), ''), '/w'), 'agent evaluated x()');
  assert.equal(activityLabel(log.add(exec('set', 'x'), ''), '/w'), 'agent set x');
  assert.equal(activityLabel(log.add(exec('next'), ''), '/w'), 'agent stepped over');
});

test('eval and set are no pending step', () => {
  const log = new ActivityLog();
  const e = log.add(exec('eval', 'x()'), '');
  log.fromAdapter(stopped(1));
  assert.equal(e.stopReason, '');
});

test("the editor's own execution request clears the pending step", () => {
  for (const command of ['continue', 'next', 'stepIn', 'stepOut', 'pause', 'stepBack', 'reverseContinue', 'goto']) {
    const log = new ActivityLog();
    const e = log.add(exec('next'), '');
    log.fromEditor(request(command));
    assert.deepEqual(log.fromAdapter(stopped(1, 'step', false)), { changed: false, reveal: undefined }, command);
    log.fromEditor(stackReq(3, { threadId: 1 }));
    log.fromAdapter(stackResp(3, [frame('/w/a.py', 2)]));
    assert.equal(e.location, undefined, command);
  }
});

test('stackTrace pairing: thread, startFrame, seq, failure, frame without a path', () => {
  const log = new ActivityLog();
  const e = log.add(exec('next'), '');
  log.fromAdapter(stopped(2));
  log.fromEditor(stackReq(10, { threadId: 1 })); // another thread
  log.fromEditor(stackReq(11, { threadId: 2, startFrame: 1, levels: 19 })); // not the top
  assert.deepEqual(log.fromAdapter(stackResp(10, [frame('/w/x.py', 1)])), { changed: false, reveal: undefined });
  assert.deepEqual(log.fromAdapter(stackResp(11, [frame('/w/x.py', 1)])), { changed: false, reveal: undefined });
  log.fromEditor(stackReq(12, { threadId: 2, startFrame: 0 }));
  assert.deepEqual(log.fromAdapter(stackResp(12, [], false)), { changed: false, reveal: undefined });
  assert.equal(e.location, undefined);
  // A failed response doesn't end the wait: the next top-frame request can locate it.
  log.fromEditor(stackReq(13, { threadId: 2 }));
  assert.deepEqual(log.fromAdapter(stackResp(13, [{ id: 1, name: 'f', line: 4 }])), {
    changed: false,
    reveal: undefined,
  });
  assert.equal(e.location, undefined, 'a frame without a source path has no location');
});

test('continued and terminated drop the stop being located', () => {
  for (const name of ['continued', 'terminated']) {
    const log = new ActivityLog();
    const e = log.add(exec('next'), '');
    log.fromAdapter(stopped(1));
    log.fromEditor(stackReq(3, { threadId: 1 }));
    log.fromAdapter(event(name));
    assert.deepEqual(log.fromAdapter(stackResp(3, [frame('/w/a.py', 2)])), { changed: false, reveal: undefined }, name);
    assert.equal(e.location, undefined, name);
  }
  const log = new ActivityLog();
  const e = log.add(exec('next'), '');
  log.fromAdapter(event('terminated'));
  log.fromAdapter(stopped(1));
  assert.equal(e.stopReason, '', 'terminated forgets the pending step');
});

test('a new stop replaces the one being located', () => {
  const log = new ActivityLog();
  const first = log.add(exec('next'), '');
  log.fromAdapter(stopped(1));
  log.fromEditor(stackReq(3, { threadId: 1 }));
  log.fromAdapter(stopped(1, 'breakpoint', false));
  assert.deepEqual(log.fromAdapter(stackResp(3, [frame('/w/a.py', 2)])), { changed: false, reveal: undefined });
  assert.equal(first.location, undefined);
});

test('breakpoint entries have the breakpoint location; function breakpoints none', () => {
  const log = new ActivityLog();
  const bp = { id: 3, file: '/w/app.py', line: 12, requestedLine: 12, owner: 'agent', verified: true };
  const e = log.add(ev({ kind: 'breakpoint', client: 'agent', action: 'added', breakpoint: bp }), '');
  assert.deepEqual(e.location, { path: '/w/app.py', line: 12 });
  assert.equal(activityLabel(e, '/w'), 'bp 3 added by agent: app.py:12');
  const f = log.add(
    ev({ kind: 'breakpoint', client: 'agent', action: 'added', breakpoint: { ...bp, function: 'main' } }),
    '',
  );
  assert.equal(f.location, undefined);
  const l = log.add(ev({ kind: 'lease', client: 'agent', action: 'take' }), '');
  assert.equal(l.location, undefined);
  assert.equal(activityLabel(l, '/w'), 'lease: agent took it');
});

test('entries: newest first, hidden kinds filtered, cleared, capped', () => {
  const log = new ActivityLog();
  log.add(exec('next'), 'a');
  log.add(ev({ kind: 'lease', client: 'agent', action: 'take' }), 'b');
  log.add(ev({ kind: 'client', client: 'agent', action: 'joined' }), 'c');
  assert.deepEqual(
    log.entries().map((e) => e.line),
    ['c', 'b', 'a'],
  );
  assert.deepEqual(
    log.entries(['lease', 'client']).map((e) => e.line),
    ['a'],
  );
  assert.equal(log.get(2)?.line, 'b');
  log.clear();
  assert.deepEqual(log.entries(), []);
  for (let i = 0; i < maxEntries + 5; i++) {
    log.add(exec('eval', String(i)), String(i));
  }
  assert.equal(log.size, maxEntries);
  assert.equal(log.entries()[0]?.line, String(maxEntries + 4));
  assert.equal(log.get(4), undefined, 'the oldest are dropped');
});

test('hostile text never renders icons or newlines in labels', () => {
  const log = new ActivityLog();
  const e = log.add(exec('eval', `$(zap) x\n[a](command:workbench.action.quit)${'y'.repeat(2000)}`), 'line\nx');
  const label = activityLabel(e, '/w');
  assert.ok(!label.includes('$('), label);
  assert.ok(!label.includes('\n'), label);
  assert.ok(Array.from(label).length <= 200, String(label.length));
  assert.equal(activityTooltip(e), 'line x');
  const bad = log.add(ev({ kind: 'exec', client: '$(zap)\nagent', action: 'next' }), '');
  assert.ok(!activityLabel(bad, '').includes('$('));
});
