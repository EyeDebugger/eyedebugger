// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import type { TreeSnapshot } from '../../src/vscode/api';
import { test } from './harness';
import {
  cli,
  endAll,
  extensionApi,
  item,
  join,
  latestSeq,
  line,
  rec,
  self,
  startAgent,
  until,
  waitEvents,
} from './helpers';

// I12: the Clients view lists who is in the session and who has control;
// its row actions request and give control; the agent's activity moves its
// last-seen time; Refresh re-reads the clients.
test('I12 clients', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const conn = await join(rec(), id);
  const node = (client: string) => ({ session: conn.session.id, client });
  const row = (client: string): TreeSnapshot | undefined =>
    item(api.views().clients, (i) => i.label === id)?.children.find(
      (c) => c.label === client || c.label === `${client} (you)`,
    );
  const flags = (client: string) => (row(client)?.contextValue ?? '').split(' ');

  await until(
    'the agent holding control and human:test connected',
    () => row('agent')?.description.startsWith('has control') && row(self)?.description.startsWith('connected'),
    [api.onDidChange],
  );
  const root = item(api.views().clients, (i) => i.label === id);
  assert.equal(root?.description, 'control: agent · free');
  assert.equal(root?.children[0]?.label, 'agent', 'the holder first');
  assert.equal(row(self)?.label, `${self} (you)`);
  assert.equal(row(self)?.icon, 'account');
  assert.equal(row('agent')?.icon, 'hubot');
  assert.deepEqual(flags('agent'), ['client', 'canRequest', 'canTake']);
  assert.deepEqual(flags(self), ['client']);

  let seq = await latestSeq(id);
  await vscode.commands.executeCommand('eyedbg.clients.request', { ...node('agent'), message: 'my turn' });
  const asked = await waitEvents(id, 'lease', seq, (e) => e.action === 'request' && e.client === self);
  assert.equal(asked.text, 'my turn');

  await cli(['lease', 'grant', self, `--session=${id}`, '--json']);
  await until(
    'human:test holding control',
    () => row(self)?.description.startsWith('has control') && flags(self).includes('canRelease'),
    [api.onDidChange],
  );
  assert.deepEqual(flags('agent'), ['client', 'canGrant']);
  assert.ok(item(api.views().clients, (i) => i.label === id)?.contextValue.includes('canPolicy'));

  seq = await latestSeq(id);
  await vscode.commands.executeCommand('eyedbg.clients.grant', node('agent'));
  await waitEvents(id, 'lease', seq, (e) => e.action === 'grant' && e.client === self);
  assert.equal((await cli(['lease', `--session=${id}`, '--json'])).json.lease.holder, 'agent');

  // The agent's step moves its last-seen time without an eyedbg/clients event.
  seq = await latestSeq(id);
  await cli(['next', `--session=${id}`, '--json']);
  const exec = await waitEvents(id, 'exec', seq, (e) => e.client === 'agent' && e.action === 'next');
  await until("the agent's last seen at its step or later", () => {
    const c = api
      .sessions()
      .find((s) => s.session === id)
      ?.clients.find((x) => x.id === 'agent');
    return c !== undefined && Date.parse(c.lastSeen) >= Date.parse(exec.time);
  }, [api.onDidChange]);

  const mark = conn.msgs.length;
  await vscode.commands.executeCommand('eyedbg.clients.refresh');
  await rec().find(
    conn,
    'an eyedbg/clients request',
    (r) => r.dir === 'out' && r.m.type === 'request' && r.m.command === 'eyedbg/clients',
    mark,
  );
});
