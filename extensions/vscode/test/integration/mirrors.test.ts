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
  endAll,
  extensionApi,
  isBpEvent,
  isSetBps,
  type Json,
  join,
  latestSeq,
  line,
  rec,
  self,
  startAgent,
  synced,
  trace,
  until,
  waitEvents,
} from './helpers';

function humanLines(list: Json[]): number[] {
  return list
    .filter((b) => b.owner === self)
    .map((b) => b.line as number)
    .sort((a, b) => a - b);
}

// I2: VS Code shows the agent's breakpoint as a real one (a column-1 copy),
// the extension annotates it, and VS Code's re-send of the copy is echoed:
// it never becomes the human's. I3: disabling and enabling all breakpoints
// hides the mirror and brings it back, the agent's breakpoint intact. Then
// Copy as My Breakpoint makes the human's own (no column: V2) and retracts
// the copy, and Remove Breakpoint for Everyone removes the agent's.
test('I2 mirrors, I3 disable/enable all, copy and remove', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const a = await bpAdd(id, `app.py:${line('price')}`, '--if', 'item > 2');
  const aLine: number = a.line;
  await addBreakpoint(app, line('fail'));
  await vscode.window.showTextDocument(vscode.Uri.file(app));

  const conn = await join(rec(), id);
  const announced = await rec().find(conn, `breakpoint new ${a.id}`, isBpEvent('new', a.id));
  assert.equal(announced.m.body.breakpoint.column, 1);
  assert.equal(announced.m.body.breakpoint.line, aLine);
  await until(
    'the annotation of the agent breakpoint',
    () =>
      api
        .annotations()
        .find(
          (x) =>
            x.id === a.id &&
            x.line === aLine &&
            x.uri === vscode.Uri.file(app).toString() &&
            x.text.includes('agent') &&
            x.text.includes('item > 2'),
        ),
    [api.onDidChange],
  );
  assert.ok(
    api
      .sessions()
      .find((s) => s.session === id)
      ?.mirrors.some((m) => m.id === a.id && m.line === aLine),
  );
  await synced(conn);
  // VS Code's copy is not visible to extensions (it was added without a
  // model event): the API shows only the human's own breakpoint.
  const visible = vscode.debug.breakpoints.filter(
    (b) => b instanceof vscode.SourceBreakpoint && b.location.uri.toString() === vscode.Uri.file(app).toString(),
  ) as vscode.SourceBreakpoint[];
  assert.deepEqual(
    visible.map((b) => b.location.range.start.line + 1),
    [line('fail')],
    'the copies stay invisible to vscode.debug.breakpoints',
  );

  // Adding H2 makes VS Code re-send app.py: the adopted copy at column 1.
  let mark = conn.msgs.length;
  await addBreakpoint(app, line('caught'));
  const req = await rec().find(
    conn,
    'setBreakpoints for app.py with H2',
    (r) => isSetBps(app)(r) && r.m.arguments.breakpoints.some((b: Json) => b.line === line('caught')),
    mark,
  );
  const sent: Json[] = req.m.arguments.breakpoints;
  const i = sent.findIndex((b) => b.line === aLine);
  assert.ok(i >= 0, `VS Code re-sends the copy: ${JSON.stringify(sent)}\n${trace(conn)}`);
  assert.equal(sent[i].column, 1, `the copy keeps column 1: ${JSON.stringify(sent[i])}`);
  const resp = await rec().response(conn, req.i);
  assert.equal(resp.m.body.breakpoints[i].id, a.id, 'the copy is echoed with the mirror');
  assert.equal(resp.m.body.breakpoints[i].column, 1);

  let list = await bps(id);
  assert.equal(list.find((b) => b.id === a.id)?.condition, 'item > 2');
  assert.deepEqual(
    humanLines(list),
    [line('fail'), line('caught')].sort((x, y) => x - y),
  );

  // I3: disable all.
  mark = conn.msgs.length;
  await vscode.commands.executeCommand('workbench.debug.viewlet.action.disableAllBreakpoints');
  const off = await rec().find(conn, 'setBreakpoints for app.py after Disable All', isSetBps(app), mark);
  ctx.log(`Disable All sent for app.py: ${JSON.stringify(off.m.arguments.breakpoints)}`);
  assert.deepEqual(off.m.arguments.breakpoints, []);
  await rec().response(conn, off.i);
  await synced(conn);
  await until('the annotation gone', () => !api.annotations().some((x) => x.id === a.id), [api.onDidChange]);
  list = await bps(id);
  assert.equal(list.find((b) => b.id === a.id)?.condition, 'item > 2');
  assert.deepEqual(humanLines(list), []);

  // Enable all: the copy comes back, echoed.
  mark = conn.msgs.length;
  await vscode.commands.executeCommand('workbench.debug.viewlet.action.enableAllBreakpoints');
  const on = await rec().find(conn, 'setBreakpoints for app.py after Enable All', isSetBps(app), mark);
  const again: Json[] = on.m.arguments.breakpoints;
  const j = again.findIndex((b) => b.line === aLine);
  assert.ok(j >= 0 && again[j].column === 1, `the copy is re-sent at column 1: ${JSON.stringify(again)}`);
  const onResp = await rec().response(conn, on.i);
  assert.equal(onResp.m.body.breakpoints[j].id, a.id);
  list = await bps(id);
  assert.equal(list.find((b) => b.id === a.id)?.condition, 'item > 2');
  assert.deepEqual(
    humanLines(list),
    [line('fail'), line('caught')].sort((x, y) => x - y),
  );
  assert.ok(!humanLines(list).includes(aLine));
  await until('the annotation back', () => api.annotations().find((x) => x.id === a.id && x.line === aLine), [
    api.onDidChange,
  ]);

  // Copy as My Breakpoint: an API breakpoint at character 0 goes out
  // without a column (V2), so the facade takes it as the human's own, with
  // the agent's condition, and retracts the column-1 copy beside it.
  await synced(conn);
  mark = conn.msgs.length;
  await vscode.commands.executeCommand('eyedbg.breakpoints.copyAsMine', { id: a.id });
  const cp = await rec().find(
    conn,
    'setBreakpoints for app.py with the copied breakpoint',
    (r) =>
      isSetBps(app)(r) && r.m.arguments.breakpoints.some((b: Json) => b.line === aLine && b.condition === 'item > 2'),
    mark,
  );
  const cpSent: Json[] = cp.m.arguments.breakpoints;
  ctx.log(`Copy as My Breakpoint sent for app.py: ${JSON.stringify(cpSent)}`);
  const k = cpSent.findIndex((b) => b.line === aLine && b.condition === 'item > 2');
  assert.equal(
    'column' in cpSent[k],
    false,
    `V2: an API breakpoint at character 0 carries no column: ${JSON.stringify(cpSent)}`,
  );
  const cpResp = await rec().response(conn, cp.i);
  const mine: Json = cpResp.m.body.breakpoints[k];
  assert.ok(mine.id > 0 && mine.id !== a.id, `the copy is the human's own: ${JSON.stringify(cpResp.m.body)}`);
  await until('the annotation of the copied breakpoint gone', () => !api.annotations().some((x) => x.id === a.id), [
    api.onDidChange,
  ]);
  list = await bps(id);
  const own = list.find((b) => b.id === mine.id);
  assert.equal(own?.owner, self, JSON.stringify(list));
  assert.equal(own?.line, aLine);
  assert.equal(own?.condition, 'item > 2');
  assert.equal(list.find((b) => b.id === a.id)?.owner, 'agent', "the agent's breakpoint stays");

  // Remove Breakpoint for Everyone (confirmed: no modal) removes the
  // agent's breakpoint from the session, and the log says who did.
  const b = await bpAdd(id, `app.py:${line('append')}`);
  await until(
    "the agent's new breakpoint in the extension's list",
    () =>
      api
        .sessions()
        .find((s) => s.session === id)
        ?.breakpoints.some((x) => x.id === b.id),
    [api.onDidChange],
  );
  const seq = await latestSeq(id);
  mark = conn.msgs.length;
  await vscode.commands.executeCommand('eyedbg.breakpoints.removeForEveryone', { id: b.id, confirmed: true });
  await rec().find(conn, `breakpoint removed ${b.id}`, isBpEvent('removed', b.id), mark);
  list = await bps(id);
  assert.ok(!list.some((x) => x.id === b.id), JSON.stringify(list));
  assert.equal(list.find((x) => x.id === a.id)?.owner, 'agent');
  const removed = await waitEvents(id, 'breakpoint', seq, (e) => e.action === 'removed' && e.breakpoint?.id === b.id);
  assert.equal(removed.client, self);
});
