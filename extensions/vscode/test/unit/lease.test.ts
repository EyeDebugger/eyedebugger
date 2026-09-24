// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  type AfterTakeOver,
  afterTakeOver,
  isLeaseHeld,
  LeaseHeldNotices,
  leaseHeldHolder,
  mayTake,
  menuActions,
  RequestNotices,
} from '../../src/core/lease';
import { type LeaseInfo, type LeasePolicy, parseDap } from '../../src/core/protocol';

function lease(holder: string, policy: LeasePolicy = 'free', requests: LeaseInfo['requests'] = []): LeaseInfo {
  return { holder, policy, requests };
}

test('LEASE_HELD detection', () => {
  const byVariables = parseDap({
    seq: 5,
    type: 'response',
    command: 'next',
    request_seq: 4,
    success: false,
    message: 'LEASE_HELD',
    body: {
      error: {
        id: 7006,
        format: 'the lease of session s-a is held by agent (policy handoff) — ask agent',
        variables: { code: 'LEASE_HELD', holder: 'agent' },
        showUser: true,
      },
    },
  });
  assert.ok(byVariables);
  assert.equal(isLeaseHeld(byVariables), true);
  assert.equal(leaseHeldHolder(byVariables, lease('human:x')), 'agent');

  const byMessage = parseDap({
    seq: 5,
    type: 'response',
    command: 'next',
    request_seq: 4,
    success: false,
    message: 'LEASE_HELD',
  });
  assert.ok(byMessage);
  assert.equal(isLeaseHeld(byMessage), true);
  assert.equal(leaseHeldHolder(byMessage, lease('agent:claude')), 'agent:claude');

  for (const other of [
    { seq: 5, type: 'response', command: 'next', request_seq: 4, success: true },
    { seq: 5, type: 'response', command: 'next', request_seq: 4, success: false, message: 'NOT_STOPPED' },
    {
      seq: 5,
      type: 'response',
      command: 'next',
      request_seq: 4,
      success: false,
      message: 'LEASE_HELD',
      body: { error: { variables: { code: 'NOT_STOPPED' } } },
    },
    { seq: 5, type: 'event', event: 'LEASE_HELD' },
  ]) {
    const m = parseDap(other);
    assert.ok(m);
    assert.equal(isLeaseHeld(m), false, JSON.stringify(other));
  }
});

test('the LEASE_HELD notice shows once per holder until the lease changes', () => {
  const n = new LeaseHeldNotices();
  n.leaseChanged(lease('agent', 'handoff'));
  assert.equal(n.shouldShow('agent'), true);
  assert.equal(n.shouldShow('agent'), false);
  n.leaseChanged(lease('agent', 'handoff', [{ client: 'human:x', message: '', at: 't' }]));
  assert.equal(n.shouldShow('agent'), false, 'a request is no change of lease');
  n.leaseChanged(lease('agent', 'human-priority'));
  assert.equal(n.shouldShow('agent'), true, 'a policy change re-arms');
  n.leaseChanged(lease('human:x', 'human-priority'));
  n.leaseChanged(lease('agent', 'human-priority'));
  assert.equal(n.shouldShow('agent'), true, 'a holder change re-arms');
  assert.equal(n.shouldShow('agent:b'), true, 'another holder shows');
});

test('menu actions follow the lease rules', () => {
  const self = 'human:x';
  const cases: [LeaseInfo, string[]][] = [
    [lease(self, 'handoff'), ['release', 'grant', 'policy']],
    [lease('', 'handoff'), ['take', 'grant', 'policy']],
    [lease('agent', 'free'), ['take', 'request']],
    [lease('agent', 'handoff'), ['request', 'forceTake']],
    [lease('agent', 'human-priority'), ['take', 'request']],
    [lease('agent:claude', 'human-priority'), ['take', 'request']],
    [lease('human:y', 'human-priority'), ['request', 'forceTake']],
    [lease('human:y', 'free'), ['take', 'request']],
  ];
  for (const [l, want] of cases) {
    assert.deepEqual(menuActions(l, self), want, JSON.stringify(l));
  }
  assert.equal(mayTake(lease('human:y', 'human-priority'), 'agent'), false);
  assert.equal(mayTake(lease('agent', 'human-priority'), 'agent:b'), false);
});

test('after take-over', () => {
  const self = 'human:x';
  const cases: [AfterTakeOver, LeaseInfo | undefined, LeaseInfo, boolean, string][] = [
    ['ask', lease('agent', 'free'), lease(self, 'free'), false, 'ask'],
    ['ask', lease('agent', 'free'), lease(self, 'free'), true, 'none'],
    ['ask', lease('agent:claude', 'free'), lease(self, 'free'), false, 'ask'],
    ['humanPriority', lease('agent', 'free'), lease(self, 'free'), true, 'switch'],
    ['keep', lease('agent', 'free'), lease(self, 'free'), false, 'none'],
    ['ask', lease('agent:claude', 'handoff'), lease(self, 'handoff'), false, 'none'],
    ['humanPriority', lease('agent', 'handoff'), lease(self, 'handoff'), false, 'none'],
    ['ask', lease('agent', 'free'), lease(self, 'handoff'), false, 'none'],
    ['ask', lease('agent', 'human-priority'), lease(self, 'human-priority'), false, 'none'],
    ['ask', lease('human:y', 'free'), lease(self, 'free'), false, 'none'],
    ['ask', lease('', 'free'), lease(self, 'free'), false, 'none'],
    ['ask', lease(self, 'free'), lease(self, 'free'), false, 'none'],
    ['ask', lease('agent', 'free'), lease('agent:b', 'free'), false, 'none'],
    ['ask', undefined, lease(self, 'free'), false, 'none'],
  ];
  for (const [setting, before, after, asked, want] of cases) {
    assert.equal(afterTakeOver(setting, before, after, self, asked), want, JSON.stringify([setting, before, after]));
  }
});

test('request notices: other clients while self holds, each once', () => {
  const self = 'human:x';
  const n = new RequestNotices();
  const r1 = { client: 'agent', message: 'let me', at: '2026-01-01T00:00:00Z' };
  const r2 = { client: 'agent:b', message: '', at: '2026-01-01T00:00:01Z' };
  assert.deepEqual(n.next(lease('agent', 'handoff', [{ client: self, message: 'm', at: 't' }]), self), []);
  assert.deepEqual(n.next(lease(self, 'handoff', [r1]), self), [{ client: 'agent', message: 'let me' }]);
  assert.deepEqual(n.next(lease(self, 'handoff', [r1, r2]), self), [{ client: 'agent:b', message: '' }]);
  assert.deepEqual(n.next(lease(self, 'handoff', [r1, r2]), self), []);
  const again = { ...r1, at: '2026-01-01T00:00:05Z' };
  assert.deepEqual(n.next(lease(self, 'handoff', [again]), self), [{ client: 'agent', message: 'let me' }]);
});
