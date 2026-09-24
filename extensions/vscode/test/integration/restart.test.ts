// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import { test } from './harness';
import {
  addBreakpoint,
  app,
  bpAdd,
  bps,
  cli,
  endAll,
  extensionApi,
  isBpEvent,
  isSetBps,
  type Json,
  join,
  line,
  rec,
  self,
  startAgent,
  synced,
  until,
} from './helpers';

// I4: VS Code's Restart of a joined session: the old connection retracts
// the mirrors before answering disconnect {restart: true}, the new one
// starts without copies and announces them again; the human keeps their
// breakpoints and the lease (the restart grace).
test('I4 restart', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const a = await bpAdd(id, `app.py:${line('price')}`);
  await addBreakpoint(app, line('fail'));
  await addBreakpoint(app, line('caught'));
  const conn = await join(rec(), id);
  await rec().find(conn, `breakpoint new ${a.id}`, isBpEvent('new', a.id));
  await synced(conn);
  const mine = (list: Json[]) =>
    list
      .filter((b) => b.owner === self)
      .map((b) => b.id as number)
      .sort((x, y) => x - y);
  const before = mine(await bps(id));
  assert.equal(before.length, 2);

  await vscode.commands.executeCommand('eyedbg.lease.take');
  await until(
    'the lease held by human:test',
    () => api.sessions().find((s) => s.session === id)?.lease?.holder === self,
    [api.onDidChange],
  );

  const mark = conn.msgs.length;
  await vscode.commands.executeCommand('workbench.action.debug.restart');
  const disc = await rec().find(
    conn,
    'disconnect',
    (r) => r.dir === 'out' && r.m.type === 'request' && r.m.command === 'disconnect',
    mark,
  );
  assert.equal(disc.m.arguments?.restart, true, JSON.stringify(disc.m.arguments));
  const removed = await rec().find(conn, `breakpoint removed ${a.id}`, isBpEvent('removed', a.id), disc.i);
  const answered = await rec().response(conn, disc.i);
  assert.ok(removed.i < answered.i, 'the mirrors are retracted before the disconnect response');

  const conn2 = await rec().conn(id, 1);
  const first = await rec().find(conn2, 'the first setBreakpoints for app.py', isSetBps(app));
  assert.ok(
    !first.m.arguments.breakpoints.some((b: Json) => b.column === 1),
    `no copy is left to re-send: ${JSON.stringify(first.m.arguments.breakpoints)}`,
  );
  const done = await rec().find(
    conn2,
    'configurationDone',
    (r) => r.dir === 'out' && r.m.type === 'request' && r.m.command === 'configurationDone',
  );
  await rec().find(conn2, `breakpoint new ${a.id} again`, isBpEvent('new', a.id), done.i);

  assert.deepEqual(mine(await bps(id)), before, "the human's breakpoints keep their ids");
  const lease = (await cli(['lease', `--session=${id}`, '--json'])).json.lease;
  assert.equal(lease.holder, self, 'the lease survives the restart');
});
