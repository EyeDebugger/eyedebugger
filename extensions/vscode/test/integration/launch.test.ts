// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import { test } from './harness';
import { cli, endAll, extensionApi, isEvent, type Json, leave, rec, self, stopSession, until } from './helpers';

const launchConfig = {
  type: 'eyedbg',
  request: 'launch',
  name: 'Launch app',
  lang: 'python',
  // biome-ignore lint/suspicious/noTemplateCurlyInString: a VS Code variable, substituted by VS Code.
  program: '${workspaceFolder}/app.py',
  args: ['loop'],
  stopOnEntry: true,
};

async function sessions(): Promise<Json[]> {
  return (await cli(['sessions', '--json'])).json.sessions;
}

// I8: F5 with a launch configuration starts the session as the human and
// joins it; ending the debug session stops it; a Restart joins the same
// session again instead.
test('I8 launch', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const folder = vscode.workspace.workspaceFolders?.[0];
  const known = new Set((await sessions()).map((s) => s.id as string));

  assert.equal(await vscode.debug.startDebugging(folder, launchConfig), true);
  const launched = await until(
    'a launched session',
    () => api.sessions().find((s) => s.launched && !known.has(s.session)),
    [api.onDidChange],
  );
  const id = launched.session;
  ctx.cleanup(() => stopSession(id));
  const conn = await rec().conn(id);
  await rec().find(conn, 'the stop on entry', isEvent('stopped'));
  const info = (await sessions()).find((s) => s.id === id);
  assert.ok(info, `${id} is listed`);
  assert.equal(info.lease?.holder, self);
  assert.equal(info.clients.find((c: Json) => c.id === self)?.connected, 1);
  const started = (await cli(['events', `--session=${id}`, '--kind=started', '--json'])).json.events;
  assert.equal(started[0]?.client, self, JSON.stringify(started));
  assert.equal((await sessions()).filter((s) => !known.has(s.id)).length, 1, 'one new session');

  await leave(conn.session);
  const stop = await until('the extension stopping it', () => api.stops().find((s) => s.session === id), [
    api.onDidChange,
  ]);
  assert.equal(stop.error, '');
  assert.ok(!(await sessions()).some((s) => s.id === id), `${id} is gone`);

  // Launch again; Restart re-joins the same session.
  assert.equal(await vscode.debug.startDebugging(folder, launchConfig), true);
  const second = await until(
    'a second launched session',
    () => api.sessions().find((s) => s.launched && s.session !== id && !known.has(s.session)),
    [api.onDidChange],
  );
  const id2 = second.session;
  ctx.cleanup(() => stopSession(id2));
  const c1 = await rec().conn(id2);
  await rec().find(c1, 'the stop on entry', isEvent('stopped'));
  await vscode.commands.executeCommand('workbench.action.debug.restart');
  const c2 = await rec().conn(id2, 1);
  await rec().find(c2, 'the stop, joined again', isEvent('stopped'));
  const info2 = (await sessions()).find((s) => s.id === id2);
  assert.ok(info2 && info2.state !== 'exited', `${id2} still runs`);
  assert.equal(info2.clients.find((c: Json) => c.id === self)?.connected, 1);
  assert.ok(!api.stops().some((s) => s.session === id2), 'a restart does not stop the session');
  assert.equal(
    (await sessions()).filter((s) => !known.has(s.id) && s.state !== 'exited' && s.state !== 'lost').length,
    1,
    'no second session',
  );

  await leave(c2.session);
  await until('the extension stopping the second', () => api.stops().find((s) => s.session === id2), [api.onDidChange]);
});
