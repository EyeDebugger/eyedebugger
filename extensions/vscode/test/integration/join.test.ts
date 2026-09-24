// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from './harness';
import {
  cli,
  endAll,
  extensionApi,
  isEvent,
  type Json,
  join,
  latestSeq,
  leave,
  line,
  rec,
  self,
  stackTop,
  startAgent,
  until,
  waitEvents,
} from './helpers';

// I1: join the agent's session, step as the human, see the agent's step,
// leave (the session keeps running; the human's lease is released).
test('I1 join', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const conn = await join(rec(), id);
  const session = conn.session;
  const stopped = await rec().find(conn, 'stopped', isEvent('stopped'));
  const thread: number = stopped.m.body.threadId;
  assert.equal(await stackTop(session, thread), line('loop-body'));

  let mark = conn.msgs.length;
  await session.customRequest('next', { threadId: thread });
  await rec().find(conn, 'the stop after next', isEvent('stopped'), mark);
  assert.equal(await stackTop(session, thread), line('append'));

  const execs: Json[] = (await cli(['events', `--session=${id}`, '--kind=exec', '--json'])).json.events;
  assert.ok(
    execs.some((e) => e.client === self && e.action === 'next'),
    JSON.stringify(execs),
  );
  const info = (await cli(['sessions', '--json'])).json.sessions.find((s: Json) => s.id === id);
  assert.equal(info.clients.find((c: Json) => c.id === self)?.connected, 1, JSON.stringify(info.clients));
  await until(
    'the lease held by human:test',
    () => api.sessions().find((s) => s.session === id)?.lease?.holder === self,
    [api.onDidChange],
  );

  // The agent steps (and takes the lease: free); the human sees it.
  mark = conn.msgs.length;
  await cli(['next', `--session=${id}`, '--json']);
  await until(
    'activity "agent: next"',
    () =>
      api
        .sessions()
        .find((s) => s.session === id)
        ?.activity.some((l) => l.includes(`${id} agent: next`)),
    [api.onDidChange],
  );
  await rec().find(conn, "the stop after the agent's next", isEvent('stopped'), mark);

  // The human steps again (taking the lease back), then leaves.
  mark = conn.msgs.length;
  await session.customRequest('next', { threadId: thread });
  await rec().find(conn, 'the stop after the second next', isEvent('stopped'), mark);
  const seq = await latestSeq(id);
  await leave(session);
  await waitEvents(id, 'client', seq, (e) => e.client === self && e.action === 'disconnected');
  await waitEvents(id, 'lease', seq, (e) => e.client === self && e.action === 'release' && e.reason === 'disconnected');
  const after = (await cli(['sessions', '--json'])).json.sessions.find((s: Json) => s.id === id);
  assert.equal(after.state, 'stopped', 'leaving never ends the session');
});
