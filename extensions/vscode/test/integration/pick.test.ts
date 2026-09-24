// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import { test } from './harness';
import { cli, endAll, extensionApi, isEvent, type Json, line, rec, startAgent, stopSession } from './helpers';

// I9: attach without a session joins the only live one; with none, it
// doesn't start and says why.
test('I9 pick', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  await endAll();
  for (const s of (await cli(['sessions', '--json'])).json.sessions as Json[]) {
    await stopSession(s.id);
  }
  const folder = vscode.workspace.workspaceFolders?.[0];
  const join = { type: 'eyedbg', request: 'attach', name: 'Join eyedbg session' };

  const n0 = api.notices().length;
  assert.equal(await vscode.debug.startDebugging(folder, join), false);
  assert.ok(
    api
      .notices()
      .slice(n0)
      .some((n) => n.kind === 'error' && n.text.includes('No running eyedbg session')),
    JSON.stringify(api.notices().slice(n0)),
  );

  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  assert.equal(await vscode.debug.startDebugging(folder, join), true);
  const conn = await rec().conn(id);
  await rec().find(conn, 'the stop', isEvent('stopped'));
});
