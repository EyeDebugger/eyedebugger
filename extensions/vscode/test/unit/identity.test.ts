// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { clientId, defaultName } from '../../src/core/identity';

test('default names', () => {
  const cases: [string, string][] = [
    ['John Smith', 'John-Smith'],
    ['ijat', 'ijat'],
    ['', 'vscode'],
    ['x'.repeat(80), 'x'.repeat(64)],
    ['añb', 'a-b'],
    ['a😀b', 'a-b'],
    ['DOMAIN\\user', 'DOMAIN-user'],
    ['a.b_c@d+e-f', 'a.b_c@d+e-f'],
  ];
  for (const [user, want] of cases) {
    assert.equal(defaultName(user), want, user);
  }
});

test('client ids', () => {
  assert.deepEqual(clientId('ana', ['ijat']), { ok: true, client: 'human:ana' });
  assert.deepEqual(clientId('', [undefined, '', 'John Smith']), { ok: true, client: 'human:John-Smith' });
  assert.deepEqual(clientId('', []), { ok: true, client: 'human:vscode' });
  for (const bad of ['a b', 'x'.repeat(65), 'human:ana', 'añb', '--as=agent', 'a\nb']) {
    const r = clientId(bad, ['ijat']);
    assert.equal(r.ok, false, bad);
    if (!r.ok) {
      assert.match(r.error, /eyedbg\.clientName must be 1 to 64/);
    }
  }
});
