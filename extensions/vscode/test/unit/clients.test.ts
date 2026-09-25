// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clientActions, later, merged, ordered, sessionActions } from '../../src/core/clients';
import type { ClientInfo, LeaseInfo, LeasePolicy } from '../../src/core/protocol';
import { clientRowText, clientsSessionDescription } from '../../src/core/render';

function client(id: string, connected: number, lastSeen: string, firstSeen = ''): ClientInfo {
  return { id, kind: id.split(':')[0] ?? '', connected, firstSeen, lastSeen };
}

function lease(holder: string, policy: LeasePolicy = 'free', requests: LeaseInfo['requests'] = []): LeaseInfo {
  return { policy, holder, requests };
}

const me = 'human:me';

test('later and merged: lastSeen raised by activity', () => {
  assert.equal(later('2026-09-24T10:00:00Z', '2026-09-24T10:00:05Z'), '2026-09-24T10:00:05Z');
  assert.equal(later('2026-09-24T10:00:05Z', '2026-09-24T10:00:00Z'), '2026-09-24T10:00:05Z');
  assert.equal(later('', '2026-09-24T10:00:00Z'), '2026-09-24T10:00:00Z');
  assert.equal(later('2026-09-24T10:00:00Z', 'bogus'), '2026-09-24T10:00:00Z');
  const clients = [client('agent', 0, '2026-09-24T10:00:00Z'), client(me, 1, '2026-09-24T10:00:01Z')];
  const m = merged(clients, new Map([['agent', '2026-09-24T10:01:00Z']]));
  assert.equal(m[0]?.lastSeen, '2026-09-24T10:01:00Z');
  assert.equal(m[1]?.lastSeen, '2026-09-24T10:00:01Z');
  assert.equal(clients[0]?.lastSeen, '2026-09-24T10:00:00Z', 'the input is not changed');
});

test('ordered: holder, connected, last seen, id', () => {
  const clients = [
    client('agent:b', 0, '2026-09-24T10:00:02Z'),
    client('human:x', 0, '2026-09-24T10:00:09Z'),
    client(me, 1, '2026-09-24T10:00:00Z'),
    client('agent', 0, '2026-09-24T10:00:01Z'),
    client('agent:a', 0, '2026-09-24T10:00:02Z'),
  ];
  assert.deepEqual(
    ordered(clients, 'agent').map((c) => c.id),
    ['agent', me, 'human:x', 'agent:a', 'agent:b'],
  );
  assert.deepEqual(
    ordered(clients, '').map((c) => c.id),
    [me, 'human:x', 'agent:a', 'agent:b', 'agent'],
  );
});

test('clientActions: policy × holder × row', () => {
  for (const policy of ['free', 'handoff', 'human-priority'] as const) {
    // The agent holds.
    assert.deepEqual(clientActions(lease('agent', policy), 'agent', me), ['canRequest', 'canTake'], policy);
    assert.deepEqual(clientActions(lease('agent', policy), me, me), [], policy);
    assert.deepEqual(clientActions(lease('agent', policy), 'human:x', me), [], policy);
    // I hold.
    assert.deepEqual(clientActions(lease(me, policy), me, me), ['canRelease'], policy);
    assert.deepEqual(clientActions(lease(me, policy), 'agent', me), ['canGrant'], policy);
    // Nobody holds.
    assert.deepEqual(clientActions(lease('', policy), 'agent', me), ['canGrant'], policy);
    assert.deepEqual(clientActions(lease('', policy), me, me), [], policy);
    assert.deepEqual(sessionActions(lease(me, policy), me), ['canPolicy'], policy);
    assert.deepEqual(sessionActions(lease('', policy), me), ['canPolicy'], policy);
    assert.deepEqual(sessionActions(lease('agent', policy), me), [], policy);
  }
  assert.deepEqual(clientActions(undefined, 'agent', me), []);
  assert.deepEqual(clientActions(lease(''), 'not a client', me), []);
  assert.deepEqual(sessionActions(undefined, me), []);
});

test('row texts', () => {
  const l = lease('agent', 'handoff', [{ client: me, message: 'my turn', at: '' }]);
  assert.equal(clientsSessionDescription(l, me), 'control: agent · handoff · 1 request');
  assert.equal(clientsSessionDescription(lease(me), me), 'control: you · free');
  assert.equal(clientsSessionDescription(lease(''), me), 'control: nobody · free');
  assert.equal(clientsSessionDescription(undefined, me), '');

  const agent = clientRowText(client('agent', 0, '2026-09-24T10:00:00Z', '2026-09-24T09:00:00Z'), l, me);
  assert.equal(agent.label, 'agent');
  assert.match(agent.description, /^has control · seen \d\d:\d\d:\d\d$/);
  assert.match(agent.tooltip, /^agent\nhas control, seen \d\d:\d\d:\d\d\nFirst seen \d\d:\d\d:\d\d$/);

  const mine = clientRowText(client(me, 2, ''), l, me);
  assert.equal(mine.label, 'human:me (you)');
  assert.equal(mine.description, 'connected ×2');
  assert.equal(mine.tooltip, 'human:me (you)\nconnected ×2\nAsks for control: "my turn"');
  assert.equal(clientRowText(client('human:x', 1, ''), l, me).description, 'connected');
});

test('row texts of hostile strings are plain', () => {
  const l = lease('agent', 'free', [{ client: 'bad\n$(zap)', message: '[a](command:x)\n$(zap)', at: '' }]);
  const r = clientRowText(client('bad\n$(zap)', 0, ''), l, me);
  assert.ok(!r.label.includes('$(') && !r.label.includes('\n'), r.label);
  assert.ok(!r.description.includes('$('));
  for (const line of r.tooltip.split('\n')) {
    assert.ok(line.length <= 400);
  }
  assert.ok(!clientsSessionDescription(lease('$(zap)x'), me).includes('$('));
});
