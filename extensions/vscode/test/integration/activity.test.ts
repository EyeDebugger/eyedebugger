// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import { test } from './harness';
import {
  app,
  bpAdd,
  cli,
  editorEvents,
  endAll,
  extensionApi,
  isEvent,
  item,
  join,
  line,
  rec,
  setting,
  shownAt,
  startAgent,
  until,
} from './helpers';

// I10: another client's stops carry preserveFocusHint (the human's own and
// the join's replayed stop don't); the agent's step lands in the Activity
// view on the line it stopped at, which opens on click; a breakpoint entry
// has its line; hidden kinds and Clear.
test('I10 activity', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const conn = await join(rec(), id);
  const session = conn.session;
  const replay = await rec().find(conn, 'the replayed stop', isEvent('stopped'));
  assert.notEqual(replay.m.body.preserveFocusHint, true, JSON.stringify(replay.m.body));
  const thread: number = replay.m.body.threadId;
  const entries = () => item(api.views().activity, (i) => i.label === id)?.children ?? [];

  // The agent steps: a hinted stop, located from VS Code's own stackTrace.
  let mark = conn.msgs.length;
  await cli(['next', `--session=${id}`, '--json']);
  const agentStop = await rec().find(conn, "the stop after the agent's next", isEvent('stopped'), mark);
  assert.equal(agentStop.m.body.preserveFocusHint, true, JSON.stringify(agentStop.m.body));
  const label = `agent stepped over → app.py:${line('append')}`;
  const step = await until(`the activity entry "${label}"`, () => entries().find((i) => i.label === label), [
    api.onDidChange,
  ]);
  assert.equal(step.icon, 'debug-step-over');
  assert.match(step.tooltip, /Stopped: step at /);
  const open = step.command;
  assert.equal(open?.command, 'eyedbg.activity.open');

  // Clicking it opens app.py on that line (VS Code left the hinted stop alone).
  await vscode.commands.executeCommand('eyedbg.activity.open', ...(open?.arguments ?? []));
  await until(`app.py shown at line ${line('append')}`, () => shownAt(app, line('append')), editorEvents);

  // A breakpoint entry has its line.
  const bp = await bpAdd(id, `app.py:${line('price')}`, '--if', 'item > 2');
  const bpLabel = `bp ${bp.id} added by agent: app.py:${line('price')} if item > 2`;
  const bpEntry = await until(`the activity entry "${bpLabel}"`, () => entries().find((i) => i.label === bpLabel), [
    api.onDidChange,
  ]);
  assert.equal(bpEntry.command?.command, 'eyedbg.activity.open');
  assert.equal(bpEntry.icon, 'debug-breakpoint');

  // Hidden kinds are filtered out, not dropped.
  await setting(ctx, 'eyedbg.activity.hide', ['breakpoint']);
  assert.equal(
    entries().some((i) => i.label === bpLabel),
    false,
  );
  assert.ok(entries().some((i) => i.label === label));
  await setting(ctx, 'eyedbg.activity.hide', []);
  assert.ok(entries().some((i) => i.label === bpLabel));

  await vscode.commands.executeCommand('eyedbg.activity.clear');
  assert.deepEqual(entries(), []);
  // The walkthrough's link target exists.
  await vscode.commands.executeCommand('eyedbg.activity.focus');

  // The human's own step: no hint.
  mark = conn.msgs.length;
  await session.customRequest('next', { threadId: thread });
  const own = await rec().find(conn, "the stop after the human's next", isEvent('stopped'), mark);
  assert.notEqual(own.m.body.preserveFocusHint, true, JSON.stringify(own.m.body));
});
