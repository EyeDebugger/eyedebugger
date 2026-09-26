// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import {
  adapterUnsupported,
  checkVersion,
  dapArgs,
  dapExitMessage,
  dapLaunchArgs,
  isLive,
  launchCwd,
  launchFeature,
  launchUnsupported,
  parseResult,
  parseSessions,
  redact,
  sessionsArgs,
  versionArgs,
} from '../../src/core/cli';
import { EyedbgError } from '../../src/core/protocol';
import { validateLaunch } from '../../src/core/validate';

function spec(cfg: Record<string, unknown>) {
  const r = validateLaunch(cfg);
  assert.ok(r.ok, r.ok ? '' : r.error);
  return r.value;
}

test('simple command lines', () => {
  assert.deepEqual(versionArgs(), ['version', '--json']);
  assert.deepEqual(sessionsArgs(), ['sessions', '--json']);
  assert.deepEqual(dapArgs('s-7f3k', 'human:ana'), ['dap', '--session=s-7f3k', '--as=human:ana']);
});

test('launch: the descriptor argv holds no configuration value', () => {
  assert.deepEqual(dapLaunchArgs('human:ana'), ['dap', '--launch', '--as=human:ana']);
});

test('launch: needs the dap.launch feature', () => {
  assert.equal(launchFeature, 'dap.launch');
  assert.equal(launchUnsupported('1.2.0', ['dap', 'dap.launch']), '');
  assert.equal(
    launchUnsupported('1.0.0', ['dap', 'presence']),
    "eyedbg 1.0.0 can't start a session from a launch configuration: it lacks dap.launch",
  );
});

test('launch: "adapter" needs the adapter.select feature', () => {
  const withAdapter = spec({ lang: 'dotnet', adapter: 'sharpdbg' });
  assert.equal(adapterUnsupported(withAdapter, '1.0.0', ['dap', 'adapter.select']), '');
  assert.equal(adapterUnsupported(spec({ lang: 'dotnet' }), '1.0.0', ['dap']), '');
  assert.equal(
    adapterUnsupported(withAdapter, '1.0.0', ['dap']),
    'eyedbg 1.0.0 can\'t choose a debug adapter ("adapter": "sharpdbg"): it lacks adapter.select',
  );
});

test('launch cwd: resolved against the folder, absolute in the spec and the process', () => {
  const cases: [string, Record<string, unknown>, string | undefined, string | undefined, string | undefined][] = [
    // platform, config, folder, want cwd (process), want spec.cwd (undefined: none)
    ['linux', { cwd: 'src' }, '/w', '/w/src', '/w/src'],
    ['linux', { cwd: '../other/./x' }, '/w/a', '/w/other/x', '/w/other/x'],
    ['linux', { cwd: '/abs' }, '/w', '/abs', '/abs'],
    ['linux', { cwd: '/abs' }, undefined, '/abs', '/abs'],
    ['linux', {}, '/w', '/w', undefined],
    ['linux', { program: '/p/app.py' }, undefined, '/p', undefined],
    ['linux', { program: 'app.py' }, undefined, undefined, undefined],
    ['win32', { cwd: 'src' }, 'C:\\w', 'C:\\w\\src', 'C:\\w\\src'],
    ['win32', { cwd: 'D:/abs' }, 'C:\\w', 'D:/abs', 'D:/abs'],
    ['win32', { cwd: '\\rooted' }, 'C:\\w', 'C:\\rooted', 'C:\\rooted'],
    ['win32', { program: 'C:\\p\\app.py' }, undefined, 'C:\\p', undefined],
    ['win32', { cwd: 'src' }, '\\\\srv\\share\\w', '\\\\srv\\share\\w\\src', '\\\\srv\\share\\w\\src'],
  ];
  for (const [platform, cfg, folder, wantCwd, wantFlag] of cases) {
    const r = launchCwd(spec({ lang: 'python', ...cfg }), folder, platform);
    assert.ok(r.ok, JSON.stringify([platform, cfg, folder]));
    assert.equal(r.value.cwd, wantCwd, JSON.stringify([platform, cfg, folder]));
    assert.equal(r.value.spec.cwd, wantFlag, JSON.stringify([platform, cfg, folder]));
  }
  const r = launchCwd(spec({ lang: 'python', cwd: 'src' }), undefined, 'linux');
  assert.ok(!r.ok);
  assert.match(r.error, /"cwd" "src" is relative and there is no workspace folder/);
  for (const cwd of ['C:foo', 'd:', 'C:.']) {
    const d = launchCwd(spec({ lang: 'python', cwd }), 'D:\\ws', 'win32');
    assert.ok(!d.ok, cwd);
    assert.match(d.error, /relative to a drive's current directory/);
  }
});

test('redaction hides --env values only', () => {
  assert.deepEqual(
    redact(['start', 'python', '--env=TOKEN=s3cret', '--env=EMPTY=', '--program=a', '--', '--env=K=V']),
    ['start', 'python', '--env=TOKEN=***', '--env=EMPTY=***', '--program=a', '--', '--env=K=V'],
  );
});

test('results', () => {
  assert.deepEqual(parseResult(0, '{"schema":1,"sessions":[]}', ''), { schema: 1, sessions: [] });
  const cases: [number, string, string, string, RegExp][] = [
    [
      2,
      '{"schema":1,"error":{"code":"LEASE_HELD","message":"held by agent","hint":"ask agent"}}',
      '',
      'LEASE_HELD',
      /held by agent/,
    ],
    [1, 'panic: boom', '', 'INTERNAL', /printed no JSON: "panic: boom"/],
    [1, '', 'Error: bad', 'INTERNAL', /printed no JSON: "Error: bad"/],
    [1, 'x'.repeat(500), '', 'INTERNAL', /"x{200}…"/],
    [0, '[1,2]', '', 'INTERNAL', /unexpected JSON/],
    [3, '{"schema":1}', 'oops', 'INTERNAL', /exited with code 3: "oops"/],
    [1, '{"error":{}}', '', 'INTERNAL', /eyedbg failed/],
  ];
  for (const [code, stdout, stderr, wantCode, wantMsg] of cases) {
    assert.throws(
      () => parseResult(code, stdout, stderr),
      (e: unknown) => e instanceof EyedbgError && e.code === wantCode && wantMsg.test(e.message),
      stdout,
    );
  }
  try {
    parseResult(2, '{"error":{"code":"NO_SESSION","message":"m","hint":"h"}}', '');
  } catch (e) {
    assert.ok(e instanceof EyedbgError);
    assert.equal(e.hint, 'h');
  }
});

test('version check', () => {
  const good = {
    schema: 1,
    name: 'eyedbg',
    version: 'v0.2.0',
    features: ['dap', 'presence', 'lease.request', 'dap.collab', 'more'],
  };
  assert.deepEqual(checkVersion(good), { version: 'v0.2.0', features: good.features });
  const cases: [Record<string, unknown>, RegExp][] = [
    [{ ...good, schema: 2 }, /isn't an eyedbg/],
    [{ ...good, name: 'other' }, /isn't an eyedbg/],
    [{ ...good, features: ['dap'] }, /lacks presence, lease\.request, dap\.collab/],
    [{ ...good, features: undefined }, /lacks dap, presence/],
    [{ ...good, features: [1, 'dap', 'presence', 'lease.request'] }, /lacks dap\.collab/],
  ];
  for (const [v, want] of cases) {
    assert.throws(
      () => checkVersion(v),
      (e: unknown) => e instanceof EyedbgError && e.code === 'VERSION_MISMATCH' && want.test(e.message),
    );
  }
});

test('sessions and start results', () => {
  // internal/cli/testdata/sessions_json_clients.golden
  const golden =
    '{"schema":1,"sessions":[{"id":"s-k3f9","lang":"dotnet","program":"/work/app/bin/Debug/net10.0/app.dll","state":"stopped","pid":4242,"createdAt":"2026-09-23T12:00:00Z","stop":{"reason":"breakpoint","threadId":4242},"lease":{"policy":"handoff","holder":"human:ijat","since":"2026-09-23T12:00:00Z","requests":[{"client":"agent","message":"I need to step into Total()","at":"2026-09-23T12:02:00Z"}]},"clients":[{"id":"agent","kind":"agent","firstSeen":"2026-09-23T12:00:00Z","lastSeen":"2026-09-23T12:00:00Z"},{"id":"human:ijat","kind":"human","name":"ijat","firstSeen":"2026-09-23T12:00:00Z","lastSeen":"2026-09-23T12:01:00Z","connected":1}],"recording":"/run/user/1000/eyedbg/sessions/s-k3f9.jsonl"}]}';
  const sessions = parseSessions(parseResult(0, golden, ''));
  assert.deepEqual(sessions, [
    {
      id: 's-k3f9',
      lang: 'dotnet',
      program: '/work/app/bin/Debug/net10.0/app.dll',
      state: 'stopped',
      lease: {
        policy: 'handoff',
        holder: 'human:ijat',
        requests: [{ client: 'agent', message: 'I need to step into Total()', at: '2026-09-23T12:02:00Z' }],
      },
      clients: [
        {
          id: 'agent',
          kind: 'agent',
          connected: 0,
          firstSeen: '2026-09-23T12:00:00Z',
          lastSeen: '2026-09-23T12:00:00Z',
        },
        {
          id: 'human:ijat',
          kind: 'human',
          connected: 1,
          firstSeen: '2026-09-23T12:00:00Z',
          lastSeen: '2026-09-23T12:01:00Z',
        },
      ],
    },
  ]);
  assert.deepEqual(parseSessions({ sessions: [{}, 3, { id: 's-a', state: 'exited' }] }).length, 1);
  assert.equal(isLive({ ...sessions[0], state: 'exited' } as never), false);
  assert.equal(isLive({ ...sessions[0], state: 'lost' } as never), false);
  assert.equal(isLive({ ...sessions[0], state: 'running' } as never), true);
});

test('dap exit codes', () => {
  assert.match(dapExitMessage(2, 's-a'), /doesn't exist or has exited/);
  assert.match(dapExitMessage(3, 's-a'), /too old/);
  assert.match(dapExitMessage(1, 's-a'), /Lost the connection/);
  assert.match(dapExitMessage(undefined, 's-a'), /exit code unknown/);
  assert.match(dapExitMessage(0, 's-a'), /ended/);
  // A launch connection has no session id yet.
  assert.match(dapExitMessage(3, ''), /couldn't start its daemon.*'eyedbg daemon stop' and try again/);
  assert.match(dapExitMessage(1, ''), /^Lost the connection to the eyedbg daemon \(exit code 1\)/);
  assert.match(dapExitMessage(undefined, ''), /exit code unknown/);
  assert.match(dapExitMessage(0, ''), /ended before the session started/);
  for (const code of [0, 1, 3, undefined]) {
    assert.doesNotMatch(dapExitMessage(code, ''), /session {2}|session \(/, String(code));
  }
});
