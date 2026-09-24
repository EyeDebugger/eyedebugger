// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import type { Mirror } from '../../src/core/mirrors';
import { type Breakpoint, parseBreakpoint, parseEvent, type SessionEvent } from '../../src/core/protocol';
import {
  activityLine,
  annotationText,
  clock,
  describeActivity,
  errorNotice,
  hoverParts,
  joinConfigName,
  leaseHeldNotice,
  noIcons,
  notificationSafe,
  plainText,
  requestNotice,
  sessionPickItem,
  statusLine,
  statusText,
  statusTooltip,
} from '../../src/core/render';

// VS Code's notification link syntax, copied from
// src/vs/base/common/linkedText.ts at tag 1.100.0 (unchanged at 1.139.0).
const LINK_REGEX = /\[([^\]]+)\]\(((?:https?:\/\/|command:|file:)[^)\s]+)(?: (["'])(.+?)(\3))?\)/gi;

const hostile = [
  'x [a](command:workbench.action.quit) **b** <img src=x>',
  '[click](https://evil.example)',
  '[f](file:///etc/passwd)',
  '[a](command:x "title")',
  '[[a](command:x)](command:y)',
  '[a]\n(command:x)',
  '[a](COMMAND:x)',
  '$(alert) [a](command:x?%5B%22y%22%5D)',
];

test('plainText flattens control characters and caps length', () => {
  assert.equal(plainText('a\nb\r\tc\u0000d\u007fe\u0085f\u2028g'), 'a b  c d e f g');
  assert.equal(plainText('abcdef', 4), 'abc…');
  assert.equal(plainText('😀😀😀', 2), '😀…');
  assert.equal(plainText('ok'), 'ok');
});

test('notificationSafe never leaves a link VS Code would render', () => {
  for (const s of hostile) {
    const safe = notificationSafe(s);
    LINK_REGEX.lastIndex = 0;
    assert.equal(LINK_REGEX.test(safe), false, `${s} -> ${safe}`);
    for (const f of [leaseHeldNotice(s, 's-a', 'handoff'), requestNotice(s, s), errorNotice('X', s)]) {
      LINK_REGEX.lastIndex = 0;
      assert.equal(LINK_REGEX.test(f), false, f);
    }
  }
  // The regex itself does match the raw text (the test is meaningful).
  LINK_REGEX.lastIndex = 0;
  assert.equal(LINK_REGEX.test(hostile[0] ?? ''), true);
});

test('notices', () => {
  assert.equal(leaseHeldNotice('agent', 's-7f3k', 'handoff'), 'agent has control of s-7f3k (handoff).');
  assert.equal(requestNotice('agent', 'let me step'), 'agent asks for control: “let me step”');
  assert.equal(requestNotice('agent', ''), 'agent asks for control.');
  assert.equal(errorNotice('BUILD_FAILED', 'build failed\nerror CS1002'), 'BUILD_FAILED: build failed');
});

test('noIcons', () => {
  assert.equal(noIcons('$(hubot) x').startsWith('$('), false);
});

function ev(x: Record<string, unknown>): SessionEvent {
  const e = parseEvent({ seq: 1, time: '2026-09-24T10:11:12Z', ...x });
  assert.ok(e);
  return e;
}

test('activity in the CLI phrasing', () => {
  const bp = { id: 3, file: '/w/app.py', line: 12, requestedLine: 12, owner: 'agent', verified: true };
  const cases: [Record<string, unknown>, string][] = [
    [{ kind: 'exec', client: 'agent', action: 'next' }, 'agent: next'],
    [{ kind: 'exec', client: 'agent', action: 'stepIn', threadId: 1 }, 'agent: step-in (thread 1)'],
    [{ kind: 'exec', client: 'agent', action: 'stepOut' }, 'agent: step-out'],
    [{ kind: 'exec', client: 'agent', action: 'runUntil' }, 'agent: run-until'],
    [{ kind: 'exec', client: 'agent', action: 'continue' }, 'agent: continue'],
    [{ kind: 'exec', client: 'agent', action: 'pause' }, 'agent: pause'],
    [{ kind: 'exec', client: 'agent', action: 'eval', text: 'x()' }, 'agent: eval x() (side effects allowed)'],
    [{ kind: 'exec', client: 'agent', action: 'set', text: 'x' }, 'agent: set x'],
    [{ kind: 'lease', client: 'agent', action: 'take', previous: 'human:x' }, 'lease: agent took it from human:x'],
    [
      { kind: 'lease', client: 'agent', action: 'auto', previous: 'human:x' },
      'lease: agent took it from human:x (auto)',
    ],
    [{ kind: 'lease', client: 'agent', action: 'force' }, 'lease: agent took it (forced)'],
    [
      { kind: 'lease', client: 'agent', action: 'grant', lease: { policy: 'free', holder: 'human:x' } },
      'lease: agent gave it to human:x',
    ],
    [{ kind: 'lease', client: 'agent', action: 'release' }, 'lease: agent released it'],
    [
      { kind: 'lease', client: 'agent', action: 'release', reason: 'disconnected' },
      'lease: agent released it (disconnected)',
    ],
    [
      {
        kind: 'lease',
        client: 'agent',
        action: 'request',
        text: 'why',
        lease: { policy: 'handoff', holder: 'human:x' },
      },
      'lease: agent asks human:x for it: "why"',
    ],
    [
      { kind: 'lease', client: 'agent', action: 'policy', lease: { policy: 'handoff', holder: 'agent' } },
      'lease: agent set the policy to handoff',
    ],
    [{ kind: 'breakpoint', client: 'agent', action: 'added', breakpoint: bp }, 'bp 3 added by agent: app.py:12'],
    [
      {
        kind: 'breakpoint',
        client: 'agent',
        action: 'added',
        breakpoint: { ...bp, condition: 'i > 2', hitCondition: '>=3', logMessage: 'i={i}' },
      },
      'bp 3 added by agent: app.py:12 if i > 2 hit >=3 log "i={i}"',
    ],
    [{ kind: 'breakpoint', client: 'agent', action: 'removed', breakpoint: bp }, 'bp 3 removed by agent'],
    [
      { kind: 'breakpoint', client: 'agent:b', action: 'removed', breakpoint: bp },
      'bp 3 removed by agent:b (owner agent)',
    ],
    [
      { kind: 'breakpoint', client: 'agent', action: 'changed', breakpoint: bp },
      'bp 3 changed by agent: app.py:12 verified',
    ],
    [
      {
        kind: 'breakpoint',
        client: 'agent',
        action: 'changed',
        breakpoint: { ...bp, verified: false, message: 'no code' },
      },
      'bp 3 changed by agent: app.py:12 pending: no code',
    ],
    [
      { kind: 'breakpoint', client: 'agent', action: 'added', breakpoint: { id: 4, function: 'main', owner: 'agent' } },
      'bp 4 added by agent: func:main',
    ],
    [{ kind: 'exceptions', client: 'agent', action: 'all' }, 'agent: exceptions all'],
    [
      { kind: 'exceptions', client: 'agent', action: 'none', reason: 'force' },
      'agent: exceptions none (for every client)',
    ],
    [{ kind: 'client', client: 'agent' }, 'client agent joined'],
    [{ kind: 'client', client: 'human:y', action: 'connected' }, 'client human:y connected (editor)'],
    [{ kind: 'client', client: 'human:y', action: 'disconnected' }, 'client human:y disconnected (editor)'],
  ];
  for (const [x, want] of cases) {
    assert.equal(describeActivity(ev(x), '/w'), want);
  }
  assert.equal(
    describeActivity(ev({ kind: 'breakpoint', client: 'agent', action: 'added', breakpoint: bp }), 'C:\\w'),
    'bp 3 added by agent: /w/app.py:12',
  );
  const line = activityLine(ev({ kind: 'exec', client: 'agent', action: 'eval', text: 'a\nb' }), 's-7f3k', '/w');
  assert.equal(line, `${clock('2026-09-24T10:11:12Z')} s-7f3k agent: eval a b (side effects allowed)`);
  assert.match(clock('2026-09-24T10:11:12Z'), /^\d\d:\d\d:\d\d$/);
  assert.equal(clock('nope'), '--:--:--');
  const long = activityLine(ev({ kind: 'exec', client: 'agent', action: 'eval', text: 'x'.repeat(5000) }), 's-a', '');
  assert.ok(Array.from(long).length <= 1000);
});

test('status bar', () => {
  const self = 'human:x';
  const cases: [string, string, number, string][] = [
    [self, 'free', 0, '$(account) you'],
    ['agent', 'handoff', 0, '$(hubot) agent · handoff'],
    ['agent:claude', 'human-priority', 1, '$(hubot) agent:claude · human-priority · 1 request'],
    ['human:bob', 'free', 2, '$(person) human:bob · 2 requests'],
    ['', 'free', 0, '$(circle-slash) nobody'],
    ['$(bug) evil', 'free', 0, '$(person) $\u200a(bug) evil'],
  ];
  for (const [holder, policy, n, want] of cases) {
    const requests = Array.from({ length: n }, (_, i) => ({ client: 'agent', message: '', at: String(i) }));
    assert.equal(statusText({ holder, policy: policy as never, requests }, self), want);
  }
  const tip = statusTooltip(
    { holder: 'agent', policy: 'handoff', requests: [{ client: self, message: 'a\nb', at: 't' }] },
    self,
    's-a',
  );
  assert.equal(
    tip,
    'EyeDebugger s-a: agent has control (lease policy handoff).\nhuman:x asks for control: "a b"\nClick for control actions.',
  );
  assert.equal(
    statusLine({ holder: self, policy: 'free', requests: [] }, self, 's-a'),
    'EyeDebugger s-a: you have control (lease policy free).',
  );
  assert.equal(
    statusLine({ holder: '', policy: 'handoff', requests: [] }, self, 's-a'),
    'EyeDebugger s-a: nobody has control (lease policy handoff).',
  );
});

function bp(x: Record<string, unknown>): Breakpoint {
  const b = parseBreakpoint({ id: 3, file: '/w/app.py', line: 12, owner: 'agent', verified: true, ...x });
  assert.ok(b);
  return b;
}

const m: Mirror = {
  id: 3,
  path: '/w/app.py',
  line: 12,
  verified: true,
  message: "agent's breakpoint — removing it here only hides it",
};

test('annotations', () => {
  const self = 'human:x';
  assert.equal(annotationText(m, bp({}), self), '⬥ agent');
  assert.equal(
    annotationText(m, bp({ condition: 'total > 3', hitCondition: '>= 5', logMessage: 'i={i}' }), self),
    '⬥ agent · if total > 3 · hit >= 5 · logs "i={i}"',
  );
  assert.equal(annotationText(m, bp({ owner: self }), self), '⬥ you (CLI)');
  assert.equal(annotationText({ ...m, verified: false }, bp({}), self), '⬥ agent · not verified');
  assert.equal(annotationText(m, undefined, self), '⬥ shared breakpoint');
  const long = annotationText(m, bp({ condition: `a\n${'x'.repeat(300)}` }), self);
  assert.equal(Array.from(long).length, 120);
  assert.ok(!long.includes('\n'));
});

test('hover parts', () => {
  const self = 'human:x';
  assert.deepEqual(
    hoverParts(
      m,
      bp({ condition: 'x [a](command:y)', hitCondition: '3', logMessage: 'v', note: 'n', message: 'adapter' }),
      self,
    ),
    [
      'Breakpoint 3 of agent (EyeDebugger)',
      'Condition: x [a](command:y)',
      'Hit condition: 3',
      "Log message: v (doesn't stop)",
      'n',
      'Verified',
      'adapter',
    ],
  );
  assert.deepEqual(hoverParts({ ...m, verified: false }, bp({ owner: self }), self), [
    'Your breakpoint 3, set outside this editor (EyeDebugger)',
    'Not verified',
  ]);
  assert.deepEqual(hoverParts(m, undefined, self), [
    'Breakpoint 3 of another client (EyeDebugger)',
    m.message,
    'Verified',
  ]);
});

test('sessions', () => {
  const info = {
    id: 's-7f3k',
    lang: 'python',
    program: '/w/app.py',
    state: 'stopped',
    lease: { policy: 'handoff' as const, holder: 'agent', requests: [] },
    clients: [
      { id: 'agent', kind: 'agent', connected: 0 },
      { id: 'human:x', kind: 'human', connected: 1 },
    ],
  };
  assert.deepEqual(sessionPickItem(info), {
    label: 's-7f3k',
    description: 'python · stopped · control: agent',
    detail: '/w/app.py — connected: human:x',
  });
  assert.equal(joinConfigName(info), 'Join s-7f3k — python app.py (agent has control)');
  assert.equal(
    joinConfigName({ ...info, lease: undefined, program: 'C:\\w\\a.py' }),
    'Join s-7f3k — python a.py (nobody has control)',
  );
  assert.ok(!sessionPickItem({ ...info, program: '$(bug)\n/x' }).detail.includes('$('));
});
