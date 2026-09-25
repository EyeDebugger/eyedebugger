// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// I14–I18: the .NET views against real programs — breadth under netcoredbg
// as the agent's session, or run bare and picked by pid — with the real
// .NET helper. Dumps and traces land in the run's own EYEDBG_HOME.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import * as vscode from 'vscode';
import type { EyedbgApi, TreeSnapshot } from '../../src/vscode/api';
import { test } from './harness';
import {
  breadthLine,
  breadthSrc,
  cli,
  editorEvents,
  endAll,
  extensionApi,
  eyedbgHome,
  files,
  item,
  join,
  joinRunning,
  rec,
  shownAt,
  skipDotnet,
  startAgent,
  startBareBreadth,
  startDotnetAgent,
  underDir,
  until,
  watchDir,
} from './helpers';

const deadline = 180_000;

/** counters waits for the Counters view's state to satisfy pred. */
function counters(api: EyedbgApi, what: string, pred: (c: ReturnType<EyedbgApi['dotnet']>['counters']) => boolean) {
  return until(
    what,
    () => {
      const c = api.dotnet().counters;
      return pred(c) ? c : undefined;
    },
    [api.onDidChange],
    90_000,
  );
}

/** row finds a root's child row (depth first) whose label is label. */
function row(items: TreeSnapshot[], label: string): TreeSnapshot | undefined {
  return item(items, (i) => i.label === label);
}

/** clock returns a logger of the time since it was made (where a slow test spends it). */
function clock(ctx: { log(msg: string): void }): (what: string) => void {
  const t0 = Date.now();
  return (what) => ctx.log(`${((Date.now() - t0) / 1000).toFixed(1)} s: ${what}`);
}

/** firstLine is a tooltip's first line: a dump's or trace's path. */
function firstLine(s: string): string {
  return s.split('\n', 1)[0] ?? '';
}

async function target(arg: Record<string, unknown>): Promise<void> {
  assert.equal(await vscode.commands.executeCommand('eyedbg.dotnet.chooseTarget', arg), true);
}

// I14: Counters on a joined session (auto target): live samples; Pause ends
// the watch process, Resume starts a new one; a stop at a breakpoint makes
// Start wait, and the watch starts by itself when the program runs again;
// the program's exit ends the watch.
test(
  'I14 dotnet counters',
  async (ctx) => {
    if (skipDotnet(ctx, 'I14')) {
      return;
    }
    const api = await extensionApi();
    ctx.cleanup(endAll);
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.counters.clear'));
    const at = clock(ctx);
    const { id, marker } = await startDotnetAgent(ctx, 'wait');
    await joinRunning(rec(), id);
    at('joined');
    await target({ auto: true });
    assert.equal(api.dotnet().description, `auto: ${id}`);

    await vscode.commands.executeCommand('eyedbg.dotnet.counters.start');
    await counters(api, 'two samples', (c) => c.state === 'running' && c.samples >= 2);
    at('two samples');
    assert.equal(api.dotnet().counters.target, id);
    const rows = api.views().counters;
    assert.match(row(rows, 'cpu-usage')?.description ?? '', /^[\d.,]+ %/);
    assert.match(row(rows, 'working-set')?.description ?? '', /^[\d.,]+ MB/);
    assert.match(row(rows, 'alloc-rate')?.description ?? '', /^\+[\d.,]+ B\/s · total /);
    const watch = api
      .dotnet()
      .runs.filter((r) => r.view === 'counters')
      .at(-1);
    assert.deepEqual(watch?.argv, [
      'dotnet',
      'counters',
      `--session=${id}`,
      '--as=human:test',
      '--watch',
      '--json',
      '--interval=1s',
    ]);

    await vscode.commands.executeCommand('eyedbg.dotnet.counters.pause');
    await counters(api, 'paused, no watch process', (c) => c.state === 'paused' && !c.watching);
    await vscode.commands.executeCommand('eyedbg.dotnet.counters.start');
    await counters(api, 'a sample after Resume', (c) => c.state === 'running' && c.samples >= 1 && c.watching);
    await vscode.commands.executeCommand('eyedbg.dotnet.counters.pause');
    await counters(api, 'paused again', (c) => c.state === 'paused' && !c.watching);
    at('paused, resumed, paused');

    // The agent stops the program at a breakpoint ('eyedbg pause' is refused
    // by netcoredbg here: 0x80070057).
    const bp = await cli([
      'bp',
      'add',
      `${path.join(breadthSrc, 'Program.cs')}:${breadthLine('Program.cs', 'wait-tick')}`,
      `--session=${id}`,
      '--json',
    ]);
    await cli(['wait', `--session=${id}`, '--timeout=60s', '--json']);
    await until('VS Code sees the stop', () => api.sessions().find((s) => s.session === id)?.running === false, [
      api.onDidChange,
    ]);
    await vscode.commands.executeCommand('eyedbg.dotnet.counters.start');
    at('stopped at the breakpoint');
    const waiting = await counters(api, 'waiting for the program to run', (c) => c.state === 'waiting');
    at('waiting');
    assert.match(waiting.message, /stopped at a breakpoint/);
    assert.equal(waiting.watching, false);

    await cli(['bp', 'rm', String(bp.json.breakpoints[0].id), `--session=${id}`, '--json']);
    // 'continue' waits (up to 30 s) for the next stop or the exit: it returns after the marker.
    const resumed = cli(['continue', `--session=${id}`, '--json']);
    await counters(api, 'the watch started by itself', (c) => c.state === 'running' && c.samples >= 1);
    at('the watch started by itself');

    fs.writeFileSync(marker, '');
    const ended = await counters(api, 'the watch ended with the program', (c) => c.state === 'ended' && !c.watching);
    assert.match(ended.message, /process exited/);
    at('ended');
    await resumed;
  },
  deadline,
);

// I15: Memory on a joined session stopped at a breakpoint (a dump is allowed
// then): the dump in the private directory, Retained ×5000, its GC root
// path (static Holder.Keep), and Threads of the same dump (the lock).
test(
  'I15 dotnet memory',
  async (ctx) => {
    if (skipDotnet(ctx, 'I15')) {
      return;
    }
    const api = await extensionApi();
    ctx.cleanup(endAll);
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.memory.clear'));
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.threads.clear'));
    const { id } = await startDotnetAgent(ctx, 'hold', [
      `${path.join(breadthSrc, 'Hold.cs')}:${breadthLine('Hold.cs', 'hold-tick')}`,
    ]);
    await cli(['wait', `--session=${id}`, '--timeout=60s', '--json']);
    await join(rec(), id);
    await target({ auto: true });

    await vscode.commands.executeCommand('eyedbg.dotnet.memory.takeDump');
    const stats = api.dotnet().memory;
    assert.equal(stats.lastError, '', JSON.stringify(api.notices().slice(-2)));
    const root = api.views().memory[0];
    assert.ok(root !== undefined, 'a dump');
    assert.match(root.label, /^Heap dump \d\d:\d\d:\d\d$/);
    const dump = firstLine(root.tooltip);
    ctx.log(`dump ${dump}`);
    assert.ok(underDir(dump, path.join(eyedbgHome, 'dumps')), `${dump} is not under ${eyedbgHome}/dumps`);
    assert.match(root.tooltip, /private to you/);
    const retained = row(root.children, 'Retained');
    assert.match(retained?.description ?? '', /^5,000 objects · /);
    const [, dumpKey, typeIndex] = /^dump(\d+)\/type(\d+)$/.exec(retained?.id ?? '') ?? [];
    assert.ok(dumpKey !== undefined && typeIndex !== undefined, retained?.id);

    await vscode.commands.executeCommand('eyedbg.dotnet.memory.gcroot', {
      kind: 'type',
      dump: Number(dumpKey),
      type: Number(typeIndex),
    });
    const path0 = await until(
      'a GC root path',
      () => row(row(api.views().memory, 'Retained')?.children ?? [], 'static Holder.Keep'),
      [api.onDidChange],
    );
    assert.match(path0.children.at(-1)?.label ?? '', /^Retained 0x[0-9a-f]+$/);

    await vscode.commands.executeCommand('eyedbg.dotnet.memory.showThreads', { kind: 'dump', dump: Number(dumpKey) });
    const locks = await until('the Locks of the dump', () => row(api.views().threads, 'Locks'), [api.onDidChange]);
    assert.ok(
      locks.children.some((l) => /^held by thread \d+ · 1 waiting$/.test(l.description)),
      JSON.stringify(locks.children),
    );
    assert.match(api.views().threads[0]?.label ?? '', /^Threads of .*\.dmp$/);
  },
  deadline,
);

// I16: Threads of a process picked by pid; a frame with a source line opens
// it (off Windows: a heap dump there has no lines).
test(
  'I16 dotnet threads',
  async (ctx) => {
    if (skipDotnet(ctx, 'I16')) {
      return;
    }
    const api = await extensionApi();
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.threads.clear'));
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.chooseTarget', { auto: true }));
    ctx.cleanup(() => vscode.commands.executeCommand('workbench.action.closeAllEditors'));
    const { pid } = await startBareBreadth(ctx, 'hold');
    await target({ pid });
    assert.equal(api.dotnet().description, `pid ${pid}`);

    await vscode.commands.executeCommand('eyedbg.dotnet.threads.capture');
    assert.equal(api.dotnet().threads.lastError, '', JSON.stringify(api.notices().slice(-2)));
    const root = api.views().threads[0];
    assert.match(root?.label ?? '', /^Threads at \d\d:\d\d:\d\d$/);
    const frame = item(root?.children ?? [], (i) => i.label.startsWith('Hold.Own('));
    assert.ok(frame !== undefined, 'a Hold.Own frame');
    const group = root?.children.find((g) => g.children.includes(frame));
    assert.match(group?.description ?? '', /holds a lock/);
    if (process.platform === 'win32') {
      assert.equal(frame.command, undefined);
      ctx.log('NOTE I16: Windows heap dumps have no source lines (C7)');
      return;
    }
    const m = /^(.*):(\d+)$/m.exec(frame.tooltip.split('\n').find((l) => /Hold\.cs:\d+$/.test(l)) ?? '');
    assert.ok(m?.[1] !== undefined && m[2] !== undefined, frame.tooltip);
    const file = m[1];
    const line = Number(m[2]);
    assert.equal(fs.realpathSync(file), fs.realpathSync(path.join(breadthSrc, 'Hold.cs')));
    assert.equal(frame.command?.command, 'eyedbg.dotnet.threads.openFrame');
    await vscode.commands.executeCommand(frame.command.command, ...frame.command.arguments);
    await until(`Hold.cs shown at line ${line}`, () => shownAt(file, line), editorEvents);
  },
  deadline,
);

// I17: CPU and GC traces of a process picked by pid.
test(
  'I17 dotnet trace',
  async (ctx) => {
    if (skipDotnet(ctx, 'I17')) {
      return;
    }
    const api = await extensionApi();
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.trace.clear'));
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.chooseTarget', { auto: true }));
    const { pid } = await startBareBreadth(ctx, 'burn');
    await target({ pid });

    await vscode.commands.executeCommand('eyedbg.dotnet.trace.start', { profile: 'cpu', duration: '3s' });
    assert.equal(api.dotnet().trace.lastError, '', JSON.stringify(api.notices().slice(-2)));
    const cpu = api.views().trace[0];
    assert.match(cpu?.label ?? '', /^CPU trace \d\d:\d\d:\d\d · \d+\.\d s$/);
    const hottest = row(cpu?.children ?? [], 'Hottest methods (exclusive)');
    assert.ok(
      hottest?.children.slice(0, 5).some((m) => m.label === 'Burn.Spin(int32)'),
      JSON.stringify(hottest?.children.map((m) => m.label)),
    );
    const trace = firstLine(cpu?.tooltip ?? '');
    assert.ok(underDir(trace, path.join(eyedbgHome, 'traces')), `${trace} is not under ${eyedbgHome}/traces`);

    await vscode.commands.executeCommand('eyedbg.dotnet.trace.start', { profile: 'gc', duration: '3s' });
    const gc = api.views().trace[0];
    assert.match(gc?.label ?? '', /^GC trace /);
    const gen0 = row(row(gc?.children ?? [], 'Garbage collections')?.children ?? [], 'gen0');
    assert.ok(Number((gen0?.description ?? '0').replaceAll(',', '')) >= 1, JSON.stringify(gc));
  },
  deadline,
);

// I18: a cancelled trace ends at once and (off Windows) leaves no file; a
// Python session and an exited process are refused with the CLI's codes.
test(
  'I18 dotnet cancel and errors',
  async (ctx) => {
    if (skipDotnet(ctx, 'I18')) {
      return;
    }
    const api = await extensionApi();
    ctx.cleanup(endAll);
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.counters.clear'));
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.chooseTarget', { auto: true }));
    const bare = await startBareBreadth(ctx, 'wait');
    await target({ pid: bare.pid });

    const traces = path.join(eyedbgHome, 'traces');
    fs.mkdirSync(eyedbgHome, { recursive: true });
    const before = files(traces);
    const w = watchDir(eyedbgHome);
    ctx.cleanup(() => w.dispose());
    const started = vscode.commands.executeCommand('eyedbg.dotnet.trace.start', { profile: 'cpu', duration: '60s' });
    await until('the trace file', () => files(traces).length > before.length, [w.event], 60_000);
    assert.equal(api.dotnet().trace.busy, true);
    const t0 = Date.now();
    await vscode.commands.executeCommand('eyedbg.dotnet.cancel', { view: 'trace' });
    await until('the trace canceled', () => !api.dotnet().trace.busy, [api.onDidChange], 15_000);
    await started;
    ctx.log(`trace canceled in ${Date.now() - t0} ms`);
    assert.equal(api.dotnet().trace.lastError, '');
    const left = files(traces).filter((f) => !before.includes(f));
    if (process.platform === 'win32') {
      ctx.log(`NOTE I18: Windows ends eyedbg forcefully: ${left.length} partial trace file(s) left for pruning`);
    } else {
      assert.deepEqual(left, [], 'eyedbg discarded the canceled trace');
    }

    // A process that exited.
    bare.end();
    await bare.exited;
    await vscode.commands.executeCommand('eyedbg.dotnet.counters.start');
    const gone = await counters(api, 'the watch failed', (c) => c.state === 'failed');
    assert.match(gone.message, /^INVALID_REQUEST: no process \d+/);

    // A Python session.
    const py = await startAgent(ctx);
    await target({ session: py });
    await vscode.commands.executeCommand('eyedbg.dotnet.counters.start');
    const notDotnet = await counters(api, 'NOT_DOTNET', (c) => c.state === 'failed' && c.target === py);
    assert.match(notDotnet.message, /^NOT_DOTNET: session s-[a-z0-9]+ debugs python, not \.NET/);
  },
  deadline,
);

// I19: Start Counters with nothing to follow opens the target picker; the
// target chosen there is watched (the target change the pick causes must
// not end the watch it started — review F1).
test(
  'I19 dotnet counters via the picker',
  async (ctx) => {
    if (skipDotnet(ctx, 'I19')) {
      return;
    }
    const api = await extensionApi();
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.counters.clear'));
    ctx.cleanup(() => vscode.commands.executeCommand('eyedbg.dotnet.chooseTarget', { auto: true }));
    ctx.cleanup(() => vscode.commands.executeCommand('workbench.action.closeQuickOpen'));
    const { pid } = await startBareBreadth(ctx, 'wait');
    await target({ auto: true });
    assert.equal(api.sessions().length, 0, 'nothing for auto to follow');

    const started = vscode.commands.executeCommand('eyedbg.dotnet.counters.start');
    await until('the target picker', () => api.dotnet().picking, [api.onDidChange]);
    await target({ pid });
    await vscode.commands.executeCommand('workbench.action.closeQuickOpen');
    await started;
    const c = await counters(api, 'the picked process watched', (x) => x.state === 'running' && x.samples >= 1);
    assert.equal(c.target, `pid ${pid}`);
    assert.equal(api.dotnet().picking, false);
  },
  deadline,
);
