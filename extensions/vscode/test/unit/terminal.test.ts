// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { test } from 'node:test';
import { parseDap } from '../../src/core/protocol';
import { checkTerminal, localAbsolute, TerminalGate, terminalId, terminalName } from '../../src/core/terminal';

// The bundle runs from out/unit/; the vectors are the facade's (Go) own.
const vectorsFile = path.resolve(
  __dirname,
  '..',
  '..',
  '..',
  '..',
  'internal',
  'facade',
  'testdata',
  'terminal_requests.json',
);

interface Vector {
  name: string;
  platform: 'posix' | 'windows' | 'all';
  valid: boolean;
  rule?: number;
  arguments: Record<string, unknown>;
  repeat?: { field: 'title' | 'args' | 'env'; count: number; item: string; itemRepeat?: number };
}

/** body is a vector's arguments with its repeat applied and an id, as JSON.parse reads the facade's event. */
function body(v: Vector): Record<string, unknown> {
  const args = structuredClone(v.arguments);
  const r = v.repeat;
  if (r !== undefined) {
    const item = r.item.repeat(Math.max(r.itemRepeat ?? 1, 1));
    switch (r.field) {
      case 'title':
        args.title = item.repeat(r.count);
        break;
      case 'args':
        args.args = [...(args.args as unknown[]), ...Array.from({ length: r.count }, () => item)];
        break;
      case 'env': {
        const env = { ...(args.env as Record<string, unknown>) };
        for (let i = 1; i <= r.count; i++) {
          env[`V${i}`] = item;
        }
        args.env = env;
        break;
      }
      default:
        assert.fail(`repeat field ${String(r.field)}`);
    }
  }
  return JSON.parse(JSON.stringify({ ...args, id: 1 }));
}

const platforms = { all: ['linux', 'darwin', 'win32'], posix: ['linux', 'darwin'], windows: ['win32'] } as const;

const vectors = (JSON.parse(fs.readFileSync(vectorsFile, 'utf8')) as { vectors: Vector[] }).vectors;

test('terminal requests: every shared vector (internal/facade/testdata)', () => {
  assert.equal(vectors.length, 71);
  let checked = 0;
  for (const v of vectors) {
    for (const platform of platforms[v.platform]) {
      const what = `${v.name} (${platform})`;
      const b = body(v);
      const r = checkTerminal(b, platform);
      checked++;
      if (v.valid) {
        assert.ok(r.ok, `${what}: ${r.ok ? '' : r.error}`);
        assert.equal(r.value.id, 1, what);
        assert.equal(r.value.cwd, b.cwd, what);
        assert.equal(r.value.title, b.title ?? '', what);
        assert.deepEqual(r.value.args, b.args, what);
        assert.deepEqual({ ...r.value.env }, b.env ?? {}, what);
      } else {
        assert.equal(r.ok, false, what);
        if (!r.ok) {
          assert.equal(r.rule, v.rule, `${what}: ${r.error}`);
          assert.match(r.error, new RegExp(`^rule ${v.rule}: `), what);
        }
      }
    }
  }
  assert.ok(checked > vectors.length, 'the all-platform rows run on each platform');
});

test('terminal requests: the facade event shape', () => {
  const ok = {
    id: 7,
    title: 'Python Debug Console',
    cwd: '/w',
    args: ['/usr/bin/python3', 'a b', '$HOME;echo x'],
    env: { A: 'x', B: null },
  };
  const r = checkTerminal(JSON.parse(JSON.stringify(ok)), 'linux');
  assert.ok(r.ok);
  assert.deepEqual({ ...r.value, env: { ...r.value.env } }, ok);
  assert.equal(terminalId(ok), 7);

  const cases: [string, unknown][] = [
    ['not an object', []],
    ['null', null],
    ['no id', { ...ok, id: undefined }],
    ['id 0', { ...ok, id: 0 }],
    ['negative id', { ...ok, id: -1 }],
    ['fractional id', { ...ok, id: 1.5 }],
    ['string id', { ...ok, id: '1' }],
    ['huge id', { ...ok, id: 2 ** 60 }],
    ['number title', { ...ok, title: 1 }],
    ['null cwd', { ...ok, cwd: null }],
    ['null env', { ...ok, env: null }],
    ['array env', { ...ok, env: ['A=x'] }],
    ['number kind', { ...ok, kind: 1 }],
    ['string shell flag', { ...ok, argsCanBeInterpretedByShell: 'true' }],
  ];
  for (const [name, b] of cases) {
    const c = checkTerminal(b, 'linux');
    assert.equal(c.ok, false, name);
    assert.equal(c.ok ? -1 : c.rule, 0, name);
  }
  for (const id of [undefined, 0, -1, 1.5, '1', 2 ** 60, null]) {
    assert.equal(terminalId({ id }), undefined, String(id));
  }
  assert.equal(terminalId('x'), undefined);

  // An unpaired surrogate (JSON.parse allows one; the facade never sends one).
  const lone = checkTerminal(JSON.parse('{"id":1,"cwd":"/","args":["/bin/p","a\\ud800b"]}'), 'linux');
  assert.equal(lone.ok ? -1 : lone.rule, 4);
  const paired = checkTerminal(JSON.parse('{"id":1,"cwd":"/","args":["/bin/p","a\\ud83d\\ude00b"]}'), 'linux');
  assert.ok(paired.ok);

  // A "__proto__" variable stays a variable and sets no prototype.
  const proto = checkTerminal(JSON.parse('{"id":1,"cwd":"/","args":["/bin/p"],"env":{"__proto__":"x"}}'), 'linux');
  assert.ok(proto.ok);
  assert.equal(Object.getPrototypeOf(proto.value.env), null);
  assert.equal(Object.getOwnPropertyDescriptor(proto.value.env, '__proto__')?.value, 'x');
});

test('terminal requests: local absolute paths', () => {
  const cases: [string, string, boolean][] = [
    ['/usr/bin/python3', 'linux', true],
    ['/', 'darwin', true],
    ['//x', 'linux', false],
    ['/\\x', 'linux', false],
    ['\\x', 'linux', false],
    ['x', 'linux', false],
    ['', 'linux', false],
    ['C:\\x.exe', 'linux', false],
    ['C:\\x.exe', 'win32', true],
    ['c:/x.exe', 'win32', true],
    ['C:', 'win32', false],
    ['C:x.exe', 'win32', false],
    ['\\\\server\\share\\x.exe', 'win32', false],
    ['\\\\?\\C:\\x.exe', 'win32', false],
    ['//server/x.exe', 'win32', false],
    ['\\x.exe', 'win32', false],
    ['/x.exe', 'win32', false],
    ['1:\\x.exe', 'win32', false],
    ['é:\\x.exe', 'win32', false],
  ];
  for (const [p, platform, want] of cases) {
    assert.equal(localAbsolute(p, platform), want, `${p} (${platform})`);
  }
});

function msg(m: Record<string, unknown>) {
  const d = parseDap(m);
  assert.ok(d);
  return d;
}

const launchReq = msg({ type: 'request', seq: 3, command: 'launch', arguments: {} });
const launchResp = msg({ type: 'response', seq: 9, request_seq: 3, command: 'launch', success: true });

test('terminal acceptance: only its launch, while it starts, once', () => {
  // Launch pending: the first event runs, a second doesn't.
  let g = new TerminalGate(true);
  g.fromEditor(launchReq);
  assert.equal(g.admit(), '');
  assert.match(g.admit(), /already/);

  // Before the launch request, or after its response: refused.
  g = new TerminalGate(true);
  assert.match(g.admit(), /no launch/);
  g = new TerminalGate(true);
  g.fromEditor(launchReq);
  g.fromAdapter(launchResp);
  assert.match(g.admit(), /no launch/);

  // Another request's response doesn't end the window; a second launch request doesn't move it.
  g = new TerminalGate(true);
  g.fromEditor(launchReq);
  g.fromEditor(msg({ type: 'request', seq: 4, command: 'launch' }));
  g.fromAdapter(msg({ type: 'response', seq: 8, request_seq: 4, command: 'launch', success: true }));
  g.fromAdapter(msg({ type: 'response', seq: 9, request_seq: 3, command: 'setBreakpoints', success: true }));
  assert.equal(g.admit(), '');

  // A refused first event still counts: no second chance.
  g = new TerminalGate(true);
  assert.notEqual(g.admit(), '');
  g.fromEditor(launchReq);
  assert.notEqual(g.admit(), '');

  // Not a terminal launch (an attach, or console internalConsole): never.
  g = new TerminalGate(false);
  g.fromEditor(launchReq);
  assert.match(g.admit(), /did not ask/);
  g = new TerminalGate(false);
  g.fromEditor(msg({ type: 'request', seq: 1, command: 'attach' }));
  assert.match(g.admit(), /did not ask/);
});

test('terminal names: plain text, no icons', () => {
  assert.equal(terminalName(''), 'eyedbg');
  assert.equal(terminalName('Python Debug Console'), 'Python Debug Console');
  assert.equal(terminalName('$(zap) x'), '$\u200a(zap) x');
  assert.equal(terminalName('a\nb\u0085c\u2028d'), 'a b c d');
  const long = terminalName('x'.repeat(250));
  assert.equal(Array.from(long).length, 200);
  assert.ok(long.endsWith('…'));
});
