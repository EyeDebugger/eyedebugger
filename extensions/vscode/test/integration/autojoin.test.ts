// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import * as vscode from 'vscode';
import { test } from './harness';
import {
  app,
  cli,
  endAll,
  extensionApi,
  isEvent,
  leave,
  line,
  rec,
  setting,
  startAgent,
  stopSession,
  until,
  ws,
} from './helpers';

// I13: with eyedbg.autoJoin ask and nothing joined, an agent's session of a
// program in the workspace is offered once; joining it from the prompt's
// command works, and it isn't offered again after leaving; a program
// outside the workspace is never offered; never stops the polling.
test('I13 auto-join', async (ctx) => {
  const api = await extensionApi();
  await endAll();
  ctx.cleanup(endAll);
  const prompts = (id: string) => api.notices().filter((n) => n.kind === 'autoJoin' && n.text.includes(id));
  /** polls waits until the poller looked n more times. */
  const polls = async (n: number) => {
    const from = api.autoJoin().polls;
    await until(`${n} more looks for sessions`, () => api.autoJoin().polls >= from + n, [api.onDidChange], 60_000);
  };

  await setting(ctx, 'eyedbg.autoJoin', 'ask');
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const notice = await until(`the auto-join prompt for ${id}`, () => prompts(id)[0], [api.onDidChange], 60_000);
  // The state is the poll's snapshot: the session is listed before it stops.
  assert.match(
    notice.text,
    new RegExp(`^agent is debugging app\\.py in this workspace \\(${id}, python, (starting|running|stopped)\\)\\.$`),
  );
  assert.equal(api.autoJoin().lastError, '');
  assert.ok(api.autoJoin().prompted.includes(id));

  // The prompt's Join runs this command.
  await vscode.commands.executeCommand('eyedbg.joinSession', { session: id });
  const conn = await rec().conn(id);
  await rec().find(conn, `the first stop of ${id}`, isEvent('stopped'));
  await until('no looking while a session is joined', () => api.autoJoin().state === 'idle', [api.onDidChange]);
  await leave(conn.session);
  await polls(2);
  assert.equal(prompts(id).length, 1, 'offered again after leaving');

  // A program outside the workspace is never offered.
  const outside = path.join(path.dirname(ws), 'outside', 'app.py');
  fs.mkdirSync(path.dirname(outside), { recursive: true });
  fs.copyFileSync(app, outside);
  ctx.cleanup(() => fs.rmSync(path.dirname(outside), { recursive: true, force: true }));
  const r = await cli(['start', 'python', `--program=${outside}`, '--json', '--', 'wait']);
  const far: string = r.json.session.id;
  ctx.cleanup(() => stopSession(far));
  await polls(2);
  assert.deepEqual(prompts(far), []);
  assert.deepEqual(
    api.notices().filter((n) => n.kind === 'autoJoin').length,
    1,
    JSON.stringify(api.notices().filter((n) => n.kind === 'autoJoin')),
  );

  // never: the poller goes idle and stays so (checked across a change of state).
  await setting(ctx, 'eyedbg.autoJoin', 'never');
  await until('the poller idle', () => api.autoJoin().state === 'idle', [api.onDidChange]);
  const n = api.autoJoin().polls;
  await vscode.commands.executeCommand('eyedbg.activity.clear');
  assert.equal(api.autoJoin().state, 'idle');
  assert.equal(api.autoJoin().polls, n);
});
