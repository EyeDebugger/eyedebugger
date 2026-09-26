// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  agentOf,
  type CandidateEnv,
  candidates,
  failureDelayMs,
  plan,
  pollDelayMs,
  under,
} from '../../src/core/autojoin';
import type { ClientInfo, SessionInfo } from '../../src/core/protocol';
import { autoJoinPrompt } from '../../src/core/render';

function client(id: string, connected = 0): ClientInfo {
  return { id, kind: id.split(':')[0] ?? '', connected, firstSeen: '', lastSeen: '' };
}

function session(program: string, x: Partial<SessionInfo> = {}): SessionInfo {
  return {
    id: 's-a1',
    lang: 'python',
    program,
    state: 'stopped',
    lease: undefined,
    clients: [client('agent')],
    ...x,
  };
}

function env(x: Partial<CandidateEnv> = {}): CandidateEnv {
  return { folders: ['/ws'], self: 'human:me', skip: new Set(), platform: 'linux', realpath: (p) => p, ...x };
}

const ids = (s: SessionInfo[]) => s.map((x) => x.id);

test('under: posix containment', () => {
  assert.equal(under('/ws/app.py', '/ws', 'linux'), true);
  assert.equal(under('/ws/sub/app.py', '/ws/', 'darwin'), true);
  assert.equal(under('/ws2/app.py', '/ws', 'linux'), false);
  assert.equal(under('/ws/../x.py', '/ws', 'linux'), false);
  assert.equal(under('/ws', '/ws', 'linux'), false);
  assert.equal(under('/WS/app.py', '/ws', 'linux'), false, 'case matters on posix');
});

test('under: win32 containment', () => {
  assert.equal(under('C:\\Ws\\app.py', 'c:\\ws', 'win32'), true);
  assert.equal(under('c:/ws/sub/app.py', 'C:\\WS', 'win32'), true);
  assert.equal(under('D:\\ws\\app.py', 'c:\\ws', 'win32'), false);
  assert.equal(under('c:\\ws2\\app.py', 'c:\\ws', 'win32'), false);
  assert.equal(under('\\\\srv\\share\\ws\\app.py', '\\\\srv\\share\\ws', 'win32'), true);
  assert.equal(under('\\\\srv\\other\\ws\\app.py', '\\\\srv\\share\\ws', 'win32'), false);
  assert.equal(under('c:\\ws', 'c:\\ws', 'win32'), false);
});

test('candidates: only absolute programs under a folder', () => {
  const list = [
    session('/ws/app.py', { id: 's-in' }),
    session('/ws2/app.py', { id: 's-out' }),
    session('app.py', { id: 's-rel' }),
    session('module=pytest', { id: 's-mod' }),
    session('dotnet', { id: 's-attach' }),
    session('/ws', { id: 's-root' }),
  ];
  assert.deepEqual(ids(candidates(list, env())), ['s-in']);
  assert.deepEqual(ids(candidates(list, env({ folders: ['/other', '/ws2'] }))), ['s-out']);
  assert.deepEqual(ids(candidates(list, env({ folders: [] }))), []);
  assert.deepEqual(ids(candidates(list, env({ folders: ['relative'] }))), []);
});

test('candidates: win32 paths', () => {
  const list = [session('C:\\Ws\\app.py', { id: 's-in' }), session('/ws/app.py', { id: 's-posix' })];
  assert.deepEqual(ids(candidates(list, env({ platform: 'win32', folders: ['c:\\ws'] }))), ['s-in']);
});

test('candidates: links resolved on both sides', () => {
  const real: Record<string, string> = { '/link': '/private/ws', '/ws/app.py': '/private/ws/app.py' };
  const realpath = (p: string) => real[p] ?? p;
  assert.deepEqual(ids(candidates([session('/ws/app.py')], env({ folders: ['/link'], realpath }))), ['s-a1']);
  assert.deepEqual(ids(candidates([session('/ws/app.py')], env({ folders: ['/link'] }))), []);
});

test('candidates: live, used by an agent, not by me, not skipped', () => {
  assert.deepEqual(ids(candidates([session('/ws/a.py', { state: 'exited' })], env())), []);
  assert.deepEqual(ids(candidates([session('/ws/a.py', { state: 'lost' })], env())), []);
  assert.deepEqual(ids(candidates([session('/ws/a.py', { state: 'running' })], env())), ['s-a1']);
  assert.deepEqual(ids(candidates([session('/ws/a.py', { clients: [client('human:x')] })], env())), []);
  assert.deepEqual(ids(candidates([session('/ws/a.py', { clients: [] })], env())), []);
  assert.deepEqual(
    ids(candidates([session('/ws/a.py', { clients: [client('agent:b'), client('human:me')] })], env())),
    [],
  );
  assert.deepEqual(ids(candidates([session('/ws/a.py', { clients: [client('agent:b')] })], env())), ['s-a1']);
  assert.deepEqual(ids(candidates([session('/ws/a.py')], env({ skip: new Set(['s-a1']) }))), []);
});

test('candidates: a session this window launched is never offered, an agent in it or not', () => {
  // F5 launches as this window's client, which the session lists from its
  // start (the starter), before the window learns the id: no skip needed.
  const launched = (clients: ClientInfo[]) =>
    session('/ws/a.py', { state: 'starting', clients: [client('human:me', 1), ...clients] });
  assert.deepEqual(ids(candidates([launched([])], env())), []);
  assert.deepEqual(ids(candidates([launched([client('agent:b', 1)])], env())), []);
  assert.deepEqual(ids(candidates([launched([client('agent:b')])], env({ skip: new Set() }))), []);
});

test('agentOf', () => {
  assert.equal(agentOf(session('/ws/a.py', { clients: [client('human:x'), client('agent:b')] })), 'agent:b');
  assert.equal(agentOf(session('/ws/a.py', { clients: [] })), '');
});

test('plan: every gate, and the delays', () => {
  const ok = { setting: 'ask', focused: true, folders: 1, joined: false, failing: false };
  assert.deepEqual(plan(ok), { poll: true, delayMs: pollDelayMs });
  assert.deepEqual(plan({ ...ok, failing: true }), { poll: true, delayMs: failureDelayMs });
  assert.equal(pollDelayMs, 5_000);
  assert.equal(failureDelayMs, 60_000);
  for (const off of [
    { setting: 'never' },
    { setting: 'bogus' },
    { focused: false },
    { folders: 0 },
    { joined: true },
  ]) {
    assert.deepEqual(plan({ ...ok, ...off }), { poll: false }, JSON.stringify(off));
  }
});

test('the prompt is plain text', () => {
  assert.equal(
    autoJoinPrompt(session('/ws/app.py'), 'agent'),
    'agent is debugging app.py in this workspace (s-a1, python, stopped).',
  );
  // Hostile strings: render.test.ts.
  assert.ok(Array.from(autoJoinPrompt(session(`/ws/${'a'.repeat(2000)}.py`), 'agent')).length <= 400);
});
