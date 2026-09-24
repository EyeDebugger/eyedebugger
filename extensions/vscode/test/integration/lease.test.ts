// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import { test } from './harness';
import {
  cli,
  endAll,
  extensionApi,
  isEvent,
  join,
  latestSeq,
  line,
  rec,
  self,
  startAgent,
  until,
  waitEvents,
} from './helpers';

// VS Code's notification link syntax (src/vs/base/common/linkedText.ts, 1.100.0).
const LINK_REGEX = /\[([^\]]+)\]\(((?:https?:\/\/|command:|file:)[^)\s]+)(?: (["'])(.+?)(\3))?\)/gi;

async function setAfterTakeOver(value: string): Promise<void> {
  const cfg = () => vscode.workspace.getConfiguration('eyedbg');
  await cfg().update('lease.afterTakeOver', value, vscode.ConfigurationTarget.Global);
  await until(`eyedbg.lease.afterTakeOver = ${value}`, () => cfg().get('lease.afterTakeOver') === value, [
    vscode.workspace.onDidChangeConfiguration,
  ]);
}

// I7: under handoff the human's step is refused (and told who has control),
// asks for control, gets it, loses it to a forced take, takes it back by
// force (handoff kept), and after a take under free the session switches to
// human-priority; an agent's request can't put a link into a notification.
test('I7 lease', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  ctx.cleanup(() => setAfterTakeOver('keep'));
  const id = await startAgent(ctx, ['--lease-policy=handoff', `--bp=app.py:${line('loop-body')}`]);
  const conn = await join(rec(), id);
  const session = conn.session;
  const thread: number = (await rec().find(conn, 'stopped', isEvent('stopped'))).m.body.threadId;
  const snap = () => api.sessions().find((s) => s.session === id);

  await assert.rejects(Promise.resolve(session.customRequest('next', { threadId: thread })));
  await until(
    'the LEASE_HELD notice naming agent',
    () => api.notices().find((n) => n.kind === 'leaseHeld' && n.text.startsWith(`agent has control of ${id}`)),
    [api.onDidChange],
  );

  const seq = await latestSeq(id);
  await vscode.commands.executeCommand('eyedbg.lease.request', { message: 'let me step' });
  const asked = await waitEvents(id, 'lease', seq, (e) => e.action === 'request' && e.client === self);
  assert.equal(asked.text, 'let me step');

  await cli(['lease', 'grant', self, `--session=${id}`, '--json']);
  await until('the lease granted to human:test', () => snap()?.lease?.holder === self, [api.onDidChange]);
  let mark = conn.msgs.length;
  await session.customRequest('next', { threadId: thread });
  await rec().find(conn, 'the stop after next', isEvent('stopped'), mark);

  await cli(['lease', 'take', '--force', `--session=${id}`, '--json']);
  await until('the lease back with the agent', () => snap()?.lease?.holder === 'agent', [api.onDidChange]);
  await assert.rejects(Promise.resolve(session.customRequest('next', { threadId: thread })));

  // afterTakeOver acts only under free: a forced take under handoff keeps
  // handoff; a take under free switches to human-priority.
  await setAfterTakeOver('humanPriority');
  const policySeq = await latestSeq(id);
  await vscode.commands.executeCommand('eyedbg.lease.take', { force: true, confirmed: true });
  await until('human:test holding under handoff', () => snap()?.lease?.holder === self, [api.onDidChange]);
  assert.equal(snap()?.lease?.policy, 'handoff');
  await cli(['lease', 'take', '--force', `--session=${id}`, '--json']);
  await cli(['lease', 'policy', 'free', `--session=${id}`, '--json']);
  await until(
    'the agent holding under free',
    () => snap()?.lease?.holder === 'agent' && snap()?.lease?.policy === 'free',
    [api.onDidChange],
  );
  await vscode.commands.executeCommand('eyedbg.lease.take');
  await until(
    'human:test holding under human-priority',
    () => snap()?.lease?.holder === self && snap()?.lease?.policy === 'human-priority',
    [api.onDidChange],
  );
  const lease = (await cli(['lease', `--session=${id}`, '--json'])).json.lease;
  assert.equal(lease.policy, 'human-priority');
  assert.equal(lease.holder, self);
  const policies = (
    await cli(['events', `--session=${id}`, '--kind=lease', `--since=${policySeq}`, '--json'])
  ).json.events
    .filter((e: { action: string }) => e.action === 'policy')
    .map((e: { client: string; lease: { policy: string } }) => `${e.client} ${e.lease.policy}`);
  assert.deepEqual(policies, ['agent free', `${self} human-priority`]);

  const hostile = 'x [a](command:workbench.action.quit)';
  await cli(['lease', 'request', '--message', hostile, `--session=${id}`, '--json']);
  const n = await until(
    "the agent's request notice",
    () => api.notices().find((x) => x.kind === 'leaseRequest' && x.text.includes('command:workbench.action.quit')),
    [api.onDidChange],
  );
  LINK_REGEX.lastIndex = 0;
  assert.equal(LINK_REGEX.test(n.text), false, n.text);
  mark = conn.msgs.length;
  await session.customRequest('next', { threadId: thread });
  await rec().find(conn, 'the stop after the last next', isEvent('stopped'), mark);
});
