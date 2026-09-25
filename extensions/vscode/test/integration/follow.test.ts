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
  editorEvents,
  endAll,
  extensionApi,
  isEvent,
  item,
  join,
  line,
  rec,
  samePath,
  setting,
  shownAt,
  startAgent,
  synced,
  until,
  ws,
} from './helpers';

/** show opens file in the active column and waits until it is the active editor. */
async function show(file: string): Promise<void> {
  await vscode.window.showTextDocument(vscode.Uri.file(file), { preview: false });
  await until(
    `${path.basename(file)} active`,
    () => samePath(vscode.window.activeTextEditor?.document.uri.fsPath, file),
    editorEvents,
  );
}

// I11: with eyedbg.followAgent on, the agent's stop is shown in an editor
// (without VS Code focusing it: the stop is hinted); off, nothing moves.
test('I11 follow', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const other = path.join(ws, 'other.py');
  fs.writeFileSync(other, '# another file\n'.repeat(5));
  ctx.cleanup(() => fs.rmSync(other, { force: true }));
  await setting(ctx, 'eyedbg.followAgent', true);

  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const conn = await join(rec(), id);
  // VS Code shows the replayed stop itself (it isn't hinted): let it finish
  // fetching the stack before showing another file.
  await rec().find(conn, 'the stack of the replayed stop', (r) => r.dir === 'in' && r.m.command === 'stackTrace');
  await synced(conn);
  await show(other);

  let mark = conn.msgs.length;
  const before = api.reveals().length;
  await cli(['next', `--session=${id}`, '--json']);
  const stop = await rec().find(conn, "the stop after the agent's next", isEvent('stopped'), mark);
  assert.equal(stop.m.body.preserveFocusHint, true);
  await until(
    `a reveal of app.py:${line('append')}`,
    () =>
      api
        .reveals()
        .slice(before)
        .some((r) => r.session === id && samePath(r.path, app) && r.line === line('append')),
    [api.onDidChange],
  );
  await until(`app.py shown at line ${line('append')}`, () => shownAt(app, line('append')), editorEvents);

  // Off: the stop is located (the Activity entry) but nothing moves.
  await show(other);
  await setting(ctx, 'eyedbg.followAgent', false);
  const revealed = api.reveals().length;
  mark = conn.msgs.length;
  await cli(['next', `--session=${id}`, '--json']);
  await rec().find(conn, "the stop after the agent's second next", isEvent('stopped'), mark);
  const entry = await until('the activity entry of the second step, located', () => {
    const steps = (item(api.views().activity, (i) => i.label === id)?.children ?? []).filter((i) =>
      i.label.startsWith('agent stepped over → app.py:'),
    );
    return steps.length === 2 ? steps[0] : undefined;
  }, [api.onDidChange]);
  const to = Number(/:(\d+)/.exec(entry.label)?.[1]);
  await synced(conn);
  assert.equal(api.reveals().length, revealed, JSON.stringify(api.reveals().slice(revealed)));
  assert.ok(samePath(vscode.window.activeTextEditor?.document.uri.fsPath, other), 'the active editor moved');
  assert.equal(shownAt(app, to), false, `an editor moved to app.py:${to}`);
});
