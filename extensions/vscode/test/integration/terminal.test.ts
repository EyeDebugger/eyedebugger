// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import * as vscode from 'vscode';
import { test } from './harness';
import {
  type Conn,
  cli,
  endAll,
  isEvent,
  type Json,
  leave,
  line,
  type Rec,
  rec,
  startAgent,
  stopSession,
  until,
} from './helpers';

/** Every terminal VS Code opened since opened() was called. */
function opened(): vscode.Disposable & { terminals: vscode.Terminal[] } {
  const terminals: vscode.Terminal[] = [];
  const sub = vscode.window.onDidOpenTerminal((t) => terminals.push(t));
  return { terminals, dispose: () => sub.dispose() };
}

/** printed waits until the program's output (its output events, joined: print() may send a line in parts) holds text. */
function printed(conn: Conn, text: string): Promise<true> {
  const out = () =>
    conn.msgs
      .filter((r) => isEvent('output')(r) && r.m.body?.category !== 'console' && r.m.body?.category !== 'telemetry')
      .map((r) => String(r.m.body?.output ?? ''))
      .join('');
  return until(`the program printing ${JSON.stringify(text)}`, () => out().includes(text) || undefined, [
    rec().onDidRecord,
  ]);
}

async function sessions(): Promise<Json[]> {
  return (await cli(['sessions', '--json'])).json.sessions;
}

// I14: a launch with "console": "integratedTerminal" (docs/adr/0019): the
// facade's eyedbg/runInTerminal makes the extension run the program as a
// terminal's own process — no shell, so the arguments arrive literally —
// the program reads what is typed there, and the session is gone after it
// exits.
test('I14 launch in the terminal', async (ctx) => {
  ctx.cleanup(endAll);
  const folder = vscode.workspace.workspaceFolders?.[0];
  const known = new Set((await sessions()).map((s) => s.id as string));
  ctx.cleanup(async () => {
    for (const s of await sessions()) {
      if (!known.has(s.id)) {
        await stopSession(s.id);
      }
    }
  });
  const terms = opened();
  ctx.cleanup(() => {
    terms.dispose();
    for (const t of terms.terminals) {
      t.dispose();
    }
  });

  const from = rec().conns.length;
  const ok = await vscode.debug.startDebugging(folder, {
    type: 'eyedbg',
    request: 'launch',
    name: 'Launch app in the terminal',
    lang: 'python',
    // biome-ignore lint/suspicious/noTemplateCurlyInString: a VS Code variable, substituted by VS Code.
    program: '${workspaceFolder}/app.py',
    args: ['input', 'a b', '$HOME;echo x'],
    console: 'integratedTerminal',
    stopOnEntry: true,
  });
  assert.equal(ok, true);
  const conn = await until(
    'the launch connection',
    () =>
      rec()
        .conns.slice(from)
        .find((c) => c.session.configuration.request === 'launch'),
    [rec().onDidRecord],
  );
  const stop = await rec().find(conn, 'the stop on entry', isEvent('stopped'));
  assert.equal(stop.m.body.reason, 'entry');

  // One event, answered with the terminal's process id, before the launch's response.
  const events = conn.msgs.filter(isEvent('eyedbg/runInTerminal'));
  assert.equal(events.length, 1, 'one eyedbg/runInTerminal event');
  const answers = conn.msgs.filter(
    (r) => r.dir === 'out' && r.m.type === 'request' && r.m.command === 'eyedbg/runInTerminal',
  );
  assert.equal(answers.length, 1, 'one answer');
  const answer = answers[0]?.m.arguments;
  assert.equal(answer?.id, events[0]?.m.body.id);
  assert.equal(answer?.error, undefined, JSON.stringify(answer));
  const answered = conn.msgs.indexOf(answers[0] as Rec);
  const launchResp = conn.msgs.findIndex((r) => r.dir === 'in' && r.m.type === 'response' && r.m.command === 'launch');
  assert.ok(launchResp < 0 || answered < launchResp, 'answered while the launch was pending');

  assert.equal(terms.terminals.length, 1, 'one terminal');
  const term = terms.terminals[0] as vscode.Terminal;
  assert.equal(term.name, 'Python Debug Console');
  assert.equal(await term.processId, answer?.processId, "the answer is the terminal's process");

  // The program's stdin is the terminal: it says so, then reads a typed line.
  await conn.session.customRequest('continue', { threadId: stop.m.body.threadId });
  await printed(conn, 'tty True\n');
  term.sendText('hello');
  await printed(conn, 'got hello\n');
  await printed(conn, `args ['a b', '$HOME;echo x']`);
  await rec().find(conn, 'the exit', isEvent('exited'));
  await rec().find(conn, 'the end', isEvent('terminated'));

  // VS Code disconnects after terminated; eyedbg forgets the session before answering.
  await rec().find(
    conn,
    "the disconnect's response",
    (r) => r.dir === 'in' && r.m.type === 'response' && r.m.command === 'disconnect',
  );
  assert.ok(!(await sessions()).some((s) => s.id === conn.eyedbgSession), `${conn.eyedbgSession} is gone`);
}, 180_000);

// I14b: joining a session never makes a terminal, whatever the attach
// configuration holds.
test('I14b attach never gets a terminal', async (ctx) => {
  ctx.cleanup(endAll);
  const terms = opened();
  ctx.cleanup(() => terms.dispose());
  const id = await startAgent(ctx, [`--bp=app.py:${line('loop-body')}`]);
  const from = rec().conns.length;
  const ok = await vscode.debug.startDebugging(vscode.workspace.workspaceFolders?.[0], {
    type: 'eyedbg',
    request: 'attach',
    name: `join ${id}`,
    session: id,
    console: 'integratedTerminal',
  });
  assert.equal(ok, true);
  const conn = await until(
    'the attach connection',
    () =>
      rec()
        .conns.slice(from)
        .find((c) => c.eyedbgSession === id),
    [rec().onDidRecord],
  );
  const stop = await rec().find(conn, 'the first stop', isEvent('stopped'));
  // Run to the end: nothing asks for a terminal at any point.
  await cli(['bp', 'rm', 'all', `--session=${id}`]);
  await conn.session.customRequest('continue', { threadId: stop.m.body.threadId });
  await rec().find(conn, 'the exit', isEvent('exited'));
  await leave(conn.session);
  assert.equal(conn.msgs.filter(isEvent('eyedbg/runInTerminal')).length, 0);
  assert.equal(
    conn.msgs.filter((r) => r.dir === 'out' && r.m.command === 'eyedbg/runInTerminal').length,
    0,
    'no answer sent',
  );
  assert.equal(terms.terminals.length, 0, 'no terminal opened');
});
