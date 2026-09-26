// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { checkSessionId, isSessionId, launchConfiguration, launchKeys, validateLaunch } from '../../src/core/validate';

test('session ids', () => {
  const cases: [unknown, boolean][] = [
    ['s-7f3k', true],
    ['s-abcdefghijklmnopqrstuvwxyz012345', true],
    ['s-abcdefghijklmnopqrstuvwxyz0123456', false],
    ['s-', false],
    ['s-X', false],
    ['s-7f3k;rm', false],
    ['--as=agent', false],
    ['-s', false],
    ['s-7f3k\0', false],
    ['', false],
    ['ś-7f3k', false],
    [42, false],
    [undefined, false],
  ];
  for (const [id, want] of cases) {
    assert.equal(isSessionId(id), want, String(id));
    assert.equal(checkSessionId(id) === '', want, String(id));
  }
});

test('launch configurations: valid', () => {
  const r = validateLaunch({
    lang: 'python',
    program: '/w/app.py',
    args: ['--as=agent', '-s', 'x y'],
    cwd: '/w',
    env: { A: '1', B: 'x=y' },
    opts: { justMyCode: 'false', 'a.b-c_d': 'v' },
    stopOnEntry: true,
    leasePolicy: 'handoff',
    exceptions: 'uncaught',
  });
  assert.ok(r.ok, r.ok ? '' : r.error);
  assert.deepEqual(r.value, {
    lang: 'python',
    program: '/w/app.py',
    cwd: '/w',
    args: ['--as=agent', '-s', 'x y'],
    env: [
      ['A', '1'],
      ['B', 'x=y'],
    ],
    opts: [
      ['justMyCode', 'false'],
      ['a.b-c_d', 'v'],
    ],
    stopOnEntry: true,
    noBuild: false,
    leasePolicy: 'handoff',
    exceptions: 'uncaught',
  });
  const withAdapter = validateLaunch({ lang: 'dotnet', adapter: 'sharpdbg' });
  assert.ok(withAdapter.ok);
  assert.equal(withAdapter.value.adapter, 'sharpdbg');
  const min = validateLaunch({ lang: 'dotnet', program: '', args: null, adapter: '' });
  assert.ok(min.ok);
  assert.deepEqual(min.value, { lang: 'dotnet', args: [], env: [], opts: [], stopOnEntry: false, noBuild: false });
});

test('launch configurations: refused', () => {
  const long = 'x'.repeat(33);
  const cases: [string, Record<string, unknown>, RegExp][] = [
    ['no lang', {}, /"lang"/],
    ['flag lang', { lang: '--as=agent' }, /"lang"/],
    ['dash lang', { lang: '-s' }, /"lang"/],
    ['upper lang', { lang: 'Python' }, /"lang"/],
    ['long lang', { lang: `p${long}` }, /"lang"/],
    ['unicode lang', { lang: 'pythön' }, /"lang"/],
    ['NUL program', { lang: 'python', program: 'a\0b' }, /"program" must not contain a NUL/],
    ['number program', { lang: 'python', program: 3 }, /"program" must be a string/],
    ['args not array', { lang: 'python', args: '--x' }, /"args"/],
    ['args NUL', { lang: 'python', args: ['a\0'] }, /"args"/],
    ['args number', { lang: 'python', args: [1] }, /"args"/],
    ['env key with =', { lang: 'python', env: { 'A=B': 'x' } }, /"env" has an invalid name/],
    ['env empty key', { lang: 'python', env: { '': 'x' } }, /"env" has an invalid name/],
    ['env NUL value', { lang: 'python', env: { A: 'x\0' } }, /"env.A"/],
    ['env number value', { lang: 'python', env: { A: 1 } }, /"env.A"/],
    ['env array', { lang: 'python', env: ['A=1'] }, /"env" must be an object/],
    ['opt flag key', { lang: 'python', opts: { 'a=b --as': 'x' } }, /"opts" has an invalid name/],
    ['opt dash key', { lang: 'python', opts: { '-x': 'x' } }, /"opts" has an invalid name/],
    ['opt long key', { lang: 'python', opts: { [`a${'b'.repeat(64)}`]: 'x' } }, /"opts" has an invalid name/],
    ['policy', { lang: 'python', leasePolicy: 'mine' }, /"leasePolicy"/],
    ['exceptions', { lang: 'python', exceptions: 'some' }, /"exceptions"/],
    ['stopOnEntry string', { lang: 'python', stopOnEntry: 'yes' }, /"stopOnEntry"/],
    ['noBuild without program', { lang: 'dotnet', noBuild: true }, /"noBuild" needs "program"/],
    ['flag adapter', { lang: 'dotnet', adapter: '--as=agent' }, /"adapter"/],
    ['upper adapter', { lang: 'dotnet', adapter: 'SharpDbg' }, /"adapter"/],
    ['long adapter', { lang: 'dotnet', adapter: `s${long}` }, /"adapter"/],
    ['number adapter', { lang: 'dotnet', adapter: 1 }, /"adapter"/],
  ];
  for (const [name, cfg, want] of cases) {
    const r = validateLaunch(cfg);
    assert.equal(r.ok, false, name);
    if (!r.ok) {
      assert.match(r.error, want, name);
    }
  }
});

function launched(cfg: Record<string, unknown>): Record<string, unknown> {
  const r = validateLaunch(cfg);
  assert.ok(r.ok, r.ok ? '' : r.error);
  return launchConfiguration(cfg, r.value);
}

test('launch arguments: every eyedbg key as validated, VS Code keys kept', () => {
  const cfg = {
    type: 'eyedbg',
    request: 'launch',
    name: 'x',
    __sessionId: 'abc',
    lang: 'python',
    program: '/w/app.py',
    project: '',
    cwd: null,
    args: ['--as=agent'],
    env: { A: '1' },
    stopOnEntry: true,
    leasePolicy: 'handoff',
    adapter: '',
  };
  assert.deepEqual(launched(cfg), {
    type: 'eyedbg',
    request: 'launch',
    name: 'x',
    __sessionId: 'abc',
    lang: 'python',
    program: '/w/app.py',
    args: ['--as=agent'],
    env: { A: '1' },
    opts: {},
    stopOnEntry: true,
    noBuild: false,
    leasePolicy: 'handoff',
  });
  assert.equal(cfg.project, '', 'the configuration itself is unchanged');
  assert.deepEqual(launched({ lang: 'dotnet' }), {
    lang: 'dotnet',
    args: [],
    env: {},
    opts: {},
    stopOnEntry: false,
    noBuild: false,
  });
});

test('launch arguments: keys differing from an eyedbg key only in case are dropped', () => {
  const out = launched({
    lang: 'python',
    LANG: 'dotnet',
    Program: '/etc/x',
    PROJECT: '/p',
    Cwd: '/',
    ARGS: ['x'],
    Env: { A: '1' },
    oPTS: { a: 'b' },
    StopOnEntry: false,
    nobuild: true,
    LeasePolicy: 'free',
    EXCEPTIONS: 'all',
    Adapter: 'sharpdbg',
    Session: 's-1',
  });
  const lower = new Set(launchKeys.map((k) => k.toLowerCase()));
  for (const k of Object.keys(out)) {
    assert.ok(!lower.has(k.toLowerCase()) || (launchKeys as readonly string[]).includes(k), k);
  }
  assert.equal(out.lang, 'python');
  assert.equal(out.program, undefined);
  assert.equal(out.Session, 's-1', 'not an eyedbg key');
});

test('launch arguments: a "__proto__" key stays a plain key', () => {
  const cfg = JSON.parse('{"lang":"python","__proto__":{"polluted":1},"env":{"__proto__":"x"}}');
  const out = launched(cfg);
  assert.equal(Object.getPrototypeOf(out), Object.prototype);
  assert.deepEqual(Object.getOwnPropertyDescriptor(out, '__proto__')?.value, { polluted: 1 });
  assert.equal(Object.getOwnPropertyDescriptor(out.env, '__proto__')?.value, 'x');
  assert.match(JSON.stringify(out), /"env":\{"__proto__":"x"\}/);
});
