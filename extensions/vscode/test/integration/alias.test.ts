// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import fs from 'node:fs';
import * as vscode from 'vscode';
import { test } from './harness';
import {
  addBreakpoint,
  app,
  appLink,
  bpAdd,
  bps,
  endAll,
  extensionApi,
  isBpEvent,
  isSetBps,
  join,
  line,
  rec,
  samePath,
  self,
  startAgent,
  synced,
  until,
} from './helpers';

// I6: one file under two paths (ws and the ws-link symlink or junction).
// VS Code keys breakpoints by path; eyedbg by file: the human's breakpoint
// under the link survives a re-send of both paths, the agent's is intact,
// and new mirrors appear under the path the human uses.
test('I6 alias path', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const canonical = fs.realpathSync.native(app);
  const la = line('price');
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const a = await bpAdd(id, `app.py:${la}`);
  const conn = await join(rec(), id);
  const announced = await rec().find(conn, `breakpoint new ${a.id}`, isBpEvent('new', a.id));
  assert.ok(samePath(announced.m.body.breakpoint.source.path, canonical), JSON.stringify(announced.m.body.breakpoint));
  await synced(conn);

  let mark = conn.msgs.length;
  await addBreakpoint(appLink, line('fail'));
  const viaLink = await rec().find(conn, 'setBreakpoints under the link', isSetBps(appLink), mark);
  await rec().response(conn, viaLink.i);
  await synced(conn);
  const h1 = (await bps(id)).find((b) => b.owner === self);
  assert.ok(h1, 'H1 is set');
  if (!samePath(h1.file, canonical)) {
    // Only a Windows junction can land here: eyedbg keys it as another file.
    ctx.log(`SKIP rest of I6: ${appLink} is keyed as ${h1.file}, not ${canonical} (${process.platform})`);
    assert.equal(process.platform, 'win32', 'a symlink must resolve to the same file');
    return;
  }

  mark = conn.msgs.length;
  await vscode.commands.executeCommand('workbench.debug.viewlet.action.disableAllBreakpoints');
  await vscode.commands.executeCommand('workbench.debug.viewlet.action.enableAllBreakpoints');
  const reApp = await rec().find(
    conn,
    'app.py re-sent after Enable All',
    (r) => isSetBps(app)(r) && r.m.arguments.breakpoints.length > 0,
    mark,
  );
  const reLink = await rec().find(
    conn,
    'the link re-sent after Enable All',
    (r) => isSetBps(appLink)(r) && r.m.arguments.breakpoints.length > 0,
    mark,
  );
  await rec().response(conn, reApp.i);
  await rec().response(conn, reLink.i);
  const list = await bps(id);
  assert.deepEqual(
    list.filter((b) => b.owner === self).map((b) => b.line),
    [line('fail')],
    'H1 survives',
  );
  assert.ok(
    list.some((b) => b.id === a.id && b.owner === 'agent' && b.line === la),
    'A is intact',
  );

  await vscode.window.showTextDocument(vscode.Uri.file(appLink));
  mark = conn.msgs.length;
  const b = await bpAdd(id, `app.py:${line('append')}`);
  const nb = await rec().find(conn, `breakpoint new ${b.id}`, isBpEvent('new', b.id), mark);
  assert.ok(samePath(nb.m.body.breakpoint.source.path, appLink), JSON.stringify(nb.m.body.breakpoint));
  await until(
    'the annotation in the editor of the link',
    () => api.annotations().find((x) => x.id === b.id && x.uri === vscode.Uri.file(appLink).toString()),
    [api.onDidChange],
  );
});
