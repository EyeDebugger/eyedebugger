// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { type BinaryEnv, type FileInfo, notFound, pathEntries, resolveBinary } from '../../src/core/binary';

const exe: FileInfo = { isFile: true, mode: 0o755, size: 10, mtimeMs: 1 };
const plain: FileInfo = { isFile: true, mode: 0o644, size: 10, mtimeMs: 1 };
const dir: FileInfo = { isFile: false, mode: 0o755, size: 0, mtimeMs: 1 };

function unix(files: Record<string, FileInfo>, path = '/usr/bin:/opt/eyedbg/bin'): BinaryEnv & { looked: string[] } {
  const looked: string[] = [];
  return {
    platform: 'linux',
    env: { PATH: path },
    home: '/home/u',
    looked,
    stat: (p) => {
      looked.push(p);
      return files[p];
    },
  };
}

function windows(files: Record<string, FileInfo>, env: Record<string, string>): BinaryEnv & { looked: string[] } {
  const looked: string[] = [];
  return {
    platform: 'win32',
    env,
    home: 'C:\\Users\\u',
    looked,
    stat: (p) => {
      looked.push(p);
      return files[p];
    },
  };
}

test('the setting wins over PATH', () => {
  const e = unix({ '/usr/bin/eyedbg': exe, '/x/eyedbg': exe });
  assert.deepEqual(resolveBinary('/x/eyedbg', e), { ok: true, path: '/x/eyedbg', info: exe });
});

test('~/ expands to the home directory', () => {
  const e = unix({ '/home/u/bin/eyedbg': exe });
  const r = resolveBinary('~/bin/eyedbg', e);
  assert.ok(r.ok);
  assert.equal(r.path, '/home/u/bin/eyedbg');
});

test('bad settings are refused', () => {
  const cases: [string, Record<string, FileInfo>, RegExp][] = [
    ['bin/eyedbg', {}, /must be an absolute path/],
    ['./eyedbg', {}, /must be an absolute path/],
    ['eyedbg', { eyedbg: exe }, /must be an absolute path/],
    ['~user/eyedbg', {}, /must be an absolute path/],
    ['/missing/eyedbg', {}, /is not an executable file/],
    ['/dir', { '/dir': dir }, /is not an executable file/],
    ['/plain/eyedbg', { '/plain/eyedbg': plain }, /is not an executable file/],
  ];
  for (const [setting, files, want] of cases) {
    const r = resolveBinary(setting, unix(files));
    assert.equal(r.ok, false, setting);
    if (!r.ok) {
      assert.match(r.error, want, setting);
    }
  }
});

test('Windows settings must name an .exe', () => {
  const files = { 'C:\\t\\eyedbg.cmd': exe, 'C:\\t\\eyedbg.bat': exe, 'C:\\t\\eyedbg': exe, 'C:\\t\\eyedbg.EXE': exe };
  for (const s of ['C:\\t\\eyedbg.cmd', 'C:\\t\\eyedbg.bat', 'C:\\t\\eyedbg']) {
    const r = resolveBinary(s, windows(files, {}));
    assert.equal(r.ok, false, s);
    if (!r.ok) {
      assert.match(r.error, /must name an \.exe file/);
    }
  }
  const ok = resolveBinary('C:\\t\\eyedbg.EXE', windows(files, {}));
  assert.ok(ok.ok);
  for (const s of ['\\t\\eyedbg.exe', 't\\eyedbg.exe', 'C:eyedbg.exe']) {
    const r = resolveBinary(s, windows(files, {}));
    assert.equal(r.ok, false, s);
    if (!r.ok) {
      assert.match(r.error, /must be an absolute path/);
    }
  }
  const home = resolveBinary('~\\bin\\eyedbg.exe', windows({ 'C:\\Users\\u\\bin\\eyedbg.exe': exe }, {}));
  assert.ok(home.ok);
});

test('the PATH scan skips empty and relative entries and never looks in the cwd', () => {
  const e = unix({ eyedbg: exe, './eyedbg': exe, 'bin/eyedbg': exe, '/b/eyedbg': exe }, ':.:bin::./x:/a:/b');
  const r = resolveBinary('', e);
  assert.ok(r.ok);
  assert.equal(r.path, '/b/eyedbg');
  assert.deepEqual(e.looked, ['/a/eyedbg', '/b/eyedbg']);
});

test('Unix needs the execute bit', () => {
  const e = unix({ '/usr/bin/eyedbg': plain, '/opt/eyedbg/bin/eyedbg': exe });
  const r = resolveBinary('', e);
  assert.ok(r.ok);
  assert.equal(r.path, '/opt/eyedbg/bin/eyedbg');
});

test('Windows: Path key in any case, eyedbg.exe only, quotes removed', () => {
  const files = { 'C:\\a\\eyedbg.cmd': exe, 'C:\\a\\eyedbg': exe, 'C:\\b c\\eyedbg.exe': plain };
  const e = windows(files, { Path: '.;C:\\a;;"C:\\b c";rel\\dir;\\rooted' });
  const r = resolveBinary('', e);
  assert.ok(r.ok, r.ok ? '' : r.error);
  assert.equal(r.path, 'C:\\b c\\eyedbg.exe');
  assert.deepEqual(e.looked, ['C:\\a\\eyedbg.exe', 'C:\\b c\\eyedbg.exe']);
  assert.deepEqual(pathEntries(windows({}, { PATH: 'C:\\x' })), ['C:\\x']);
  assert.deepEqual(pathEntries(windows({}, { pAtH: '\\\\srv\\share\\bin' })), ['\\\\srv\\share\\bin']);
});

test('nothing found is the actionable message', () => {
  assert.deepEqual(resolveBinary('', unix({})), { ok: false, error: notFound });
  assert.deepEqual(resolveBinary('   ', unix({}, '')), { ok: false, error: notFound });
  assert.match(notFound, /eyedbg\.path/);
});
