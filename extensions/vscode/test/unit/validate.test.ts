// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { checkSessionId, isSessionId, validateLaunch } from '../../src/core/validate';

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
