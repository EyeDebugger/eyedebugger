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
  type Conn,
  cli,
  endAll,
  isBpEvent,
  isEvent,
  isSetBps,
  type Json,
  join,
  line,
  rec,
  self,
  startAgent,
  synced,
} from './helpers';

function kill(pid: unknown): void {
  if (typeof pid !== 'number' || pid <= 0) {
    return;
  }
  try {
    process.kill(pid, 'SIGKILL');
  } catch (e) {
    if ((e as NodeJS.ErrnoException).code !== 'ESRCH') {
      throw e;
    }
  }
}

/** crash SIGKILLs the daemon, then the session's debuggee, and waits until VS Code's debug session ended. */
async function crash(conn: Conn, id: string): Promise<void> {
  const debuggee = (await cli(['sessions', '--json'])).json.sessions.find((s: Json) => s.id === id)?.pid;
  const daemon = (await cli(['daemon', 'status', '--json'])).json.pid;
  const ended = new Promise<void>((resolve) => {
    const sub = vscode.debug.onDidTerminateDebugSession((s) => {
      if (s.id === conn.session.id) {
        sub.dispose();
        resolve();
      }
    });
  });
  kill(daemon);
  kill(debuggee);
  await ended;
}

// I5: copies VS Code kept when the connection died (as after an editor
// restart): at the next join a copy on a line the agent has a breakpoint on
// is echoed with it, never the human's; a copy on a line nobody has is
// retracted (negative id, then removed).
test('I5 stale copies', async (ctx) => {
  ctx.cleanup(endAll);
  const l = line('price');
  const s1 = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const a = await bpAdd(s1, `app.py:${l}`);
  await addBreakpoint(app, line('fail'));
  const c1 = await join(rec(), s1);
  await rec().find(c1, `breakpoint new ${a.id}`, isBpEvent('new', a.id));
  await synced(c1);
  await crash(c1, s1);

  // S2: the agent has a breakpoint at l again.
  const s2 = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`, `--bp=app.py:${l}`]);
  const a2 = (await bps(s2)).find((b) => b.line === l && b.owner === 'agent');
  assert.ok(a2, 'S2 has the agent breakpoint at l');
  const c2 = await join(rec(), s2);
  const first = await rec().find(c2, 'the first setBreakpoints for app.py', isSetBps(app));
  const sent: Json[] = first.m.arguments.breakpoints;
  const i = sent.findIndex((b) => b.line === l && b.column === 1);
  assert.ok(i >= 0, `VS Code re-sends the stale copy at column 1: ${JSON.stringify(sent)}`);
  const resp = await rec().response(c2, first.i);
  assert.equal(resp.m.body.breakpoints[i].id, a2.id, 'the stale copy is echoed with the agent breakpoint');
  const joined = await rec().find(c2, 'the join finished (eyedbg/breakpoints)', isEvent('eyedbg/breakpoints'), first.i);
  await synced(c2);
  assert.ok(!c2.msgs.slice(0, joined.i + 1).some(isBpEvent('new', a2.id)), 'an echoed copy is not announced again');
  const list2 = await bps(s2);
  assert.deepEqual(
    list2.filter((b) => b.owner === self).map((b) => b.line),
    [line('fail')],
    'the human owns only H1',
  );
  assert.ok(list2.some((b) => b.id === a2.id && b.owner === 'agent'));
  await crash(c2, s2);

  // S3: nobody has a breakpoint at l.
  const s3 = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const c3 = await join(rec(), s3);
  const first3 = await rec().find(c3, 'the first setBreakpoints for app.py', isSetBps(app));
  const sent3: Json[] = first3.m.arguments.breakpoints;
  const k = sent3.findIndex((b) => b.line === l && b.column === 1);
  assert.ok(k >= 0, `VS Code re-sends the stale copy: ${JSON.stringify(sent3)}`);
  const resp3 = await rec().response(c3, first3.i);
  const retracted = resp3.m.body.breakpoints[k];
  assert.ok(retracted.id < 0, `the stale copy is retracted: ${JSON.stringify(retracted)}`);
  await rec().find(c3, 'the retracted copy removed', isBpEvent('removed', retracted.id), resp3.i);
  const list3 = await bps(s3);
  assert.ok(!list3.some((b) => b.line === l), `nobody has a breakpoint at ${l}: ${JSON.stringify(list3)}`);
});
