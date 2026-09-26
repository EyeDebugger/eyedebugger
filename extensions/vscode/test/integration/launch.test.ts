// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import type { Ctx } from './harness';
import { test } from './harness';
import {
  breadthSrc,
  type Conn,
  cli,
  endAll,
  extensionApi,
  isEvent,
  type Json,
  leave,
  rec,
  self,
  skipDotnet,
  stopSession,
  until,
} from './helpers';

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

/** cleanupNew stops, after the test, every session that isn't in known (a launch whose id the test never learned too). */
function cleanupNew(ctx: Ctx, known: Set<string>): void {
  ctx.cleanup(async () => {
    for (const s of await sessions()) {
      if (!known.has(s.id)) {
        await stopSession(s.id);
      }
    }
  });
}

/** launched waits for the first launch connection after the from-th that learned its eyedbg session id. */
function launched(from: number): Promise<Conn> {
  return until(
    'a launch connection with its eyedbg session',
    () =>
      rec()
        .conns.slice(from)
        .find((c) => c.session.configuration.request === 'launch' && c.eyedbgSession !== ''),
    [rec().onDidRecord],
  );
}

// I8: F5 with a launch configuration sends a DAP launch through 'eyedbg dap
// --launch' (docs/adr/0019): the session starts as the human, who holds its
// lease; Stop ends it and eyedbg forgets it; Restart starts a new session
// (the program really restarts) and the old one is gone.
test('I8 launch', async (ctx) => {
  const api = await extensionApi();
  ctx.cleanup(endAll);
  const folder = vscode.workspace.workspaceFolders?.[0];
  const known = new Set((await sessions()).map((s) => s.id as string));
  cleanupNew(ctx, known);

  let from = rec().conns.length;
  assert.equal(await vscode.debug.startDebugging(folder, launchConfig), true);
  const conn = await launched(from);
  const id = conn.eyedbgSession;
  await rec().find(conn, 'the stop on entry', isEvent('stopped'));
  // The id arrives before initialized, so before VS Code configures anything.
  const at = (name: string) => conn.msgs.findIndex(isEvent(name));
  assert.ok(at('eyedbg/session') >= 0 && at('eyedbg/session') < at('initialized'), 'eyedbg/session before initialized');
  const snap = await until(
    'the extension knowing the launched session',
    () => api.sessions().find((s) => s.session === id),
    [api.onDidChange],
  );
  assert.equal(snap.launched, true);
  assert.equal(snap.vscodeSession, conn.session.id);
  const info = (await sessions()).find((s) => s.id === id);
  assert.ok(info, `${id} is listed`);
  assert.equal(info.lease?.holder, self);
  assert.equal(info.clients.find((c: Json) => c.id === self)?.connected, 1);
  const started = (await cli(['events', `--session=${id}`, '--kind=started', '--json'])).json.events;
  assert.equal(started[0]?.client, self, JSON.stringify(started));
  assert.equal((await sessions()).filter((s) => !known.has(s.id)).length, 1, 'one new session');

  // Stop: terminate ends the session and eyedbg forgets it before answering.
  await leave(conn.session);
  assert.ok(!(await sessions()).some((s) => s.id === id), `${id} is gone`);
  assert.ok(!api.sessions().some((s) => s.vscodeSession === conn.session.id), 'the entry ended');

  // Launch again, then Restart: a new session, the old one gone.
  from = rec().conns.length;
  assert.equal(await vscode.debug.startDebugging(folder, launchConfig), true);
  const c1 = await launched(from);
  const id2 = c1.eyedbgSession;
  await rec().find(c1, 'the stop on entry', isEvent('stopped'));
  from = rec().conns.length;
  await vscode.commands.executeCommand('workbench.action.debug.restart');
  const c2 = await launched(from);
  const id3 = c2.eyedbgSession;
  assert.equal(c2.session.id, c1.session.id, 'the same debug session');
  assert.ok(id3 !== id2 && id3 !== id, `a new eyedbg session (${id3})`);
  await rec().find(c2, 'the stop on entry, restarted', isEvent('stopped'));
  assert.ok(
    c1.msgs.some((r) => r.dir === 'out' && r.m.command === 'terminate' && r.m.arguments?.restart === true),
    'the restart terminated the first session',
  );
  const list = await sessions();
  assert.ok(!list.some((s) => s.id === id2), `${id2} is gone`);
  const info3 = list.find((s) => s.id === id3);
  assert.ok(info3 && info3.state !== 'exited', `${id3} runs`);
  assert.equal(info3.clients.find((c: Json) => c.id === self)?.connected, 1);
  assert.equal(list.filter((s) => !known.has(s.id)).length, 1, 'one new session');
  await until('the entry of the restarted session', () => {
    const mine = api.sessions().filter((s) => s.vscodeSession === c1.session.id);
    return mine.length === 1 && mine[0]?.session === id3;
  }, [api.onDidChange]);

  await leave(c2.session);
  assert.ok(!(await sessions()).some((s) => s.id === id3), `${id3} is gone`);
  assert.ok(!api.sessions().some((s) => s.vscodeSession === c1.session.id), 'the entry ended after the restart');
});

// I8b: F5 on a .NET project: its build streams to the Debug Console before
// the session's initialized event, then the program stops on entry.
test('I8b launch .NET project', async (ctx) => {
  if (skipDotnet(ctx, 'I8b')) {
    return;
  }
  ctx.cleanup(endAll);
  const folder = vscode.workspace.workspaceFolders?.[0];
  const known = new Set((await sessions()).map((s) => s.id as string));
  cleanupNew(ctx, known);

  const from = rec().conns.length;
  const ok = await vscode.debug.startDebugging(folder, {
    type: 'eyedbg',
    request: 'launch',
    name: 'Launch breadth',
    lang: 'dotnet',
    project: breadthSrc,
    stopOnEntry: true,
  });
  assert.equal(ok, true);
  const conn = await launched(from);
  await rec().find(conn, 'the stop on entry', isEvent('stopped'), 0, 150_000);
  const init = conn.msgs.findIndex(isEvent('initialized'));
  const built = conn.msgs.findIndex(
    (r) => isEvent('output')(r) && typeof r.m.body?.output === 'string' && r.m.body.output.includes('breadth'),
  );
  assert.ok(built >= 0 && built < init, `build output naming the project before initialized (${built}, ${init})`);
  await leave(conn.session);
  assert.ok(!(await sessions()).some((s) => s.id === conn.eyedbgSession), `${conn.eyedbgSession} is gone`);
}, 240_000);
