// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Fails unless the VSIX would hold exactly the files the extension ships
// ('vsce ls' lists what 'vsce package' takes, after .vscodeignore).

import { execFileSync } from 'node:child_process';
import { createRequire } from 'node:module';
import path from 'node:path';

const expected = ['LICENSE', 'README.md', 'dist/extension.js', 'icon.png', 'package.json'];

const require = createRequire(import.meta.url);
const vsce = path.join(path.dirname(require.resolve('@vscode/vsce/package.json')), 'vsce');
const out = execFileSync(process.execPath, [vsce, 'ls', '--no-dependencies'], { encoding: 'utf8', shell: false });
const got = out
  .split(/\r?\n/)
  .map((l) => l.trim().replaceAll('\\', '/'))
  .filter((l) => l !== '')
  .sort();

if (JSON.stringify(got) !== JSON.stringify(expected)) {
  process.stderr.write(`the VSIX would hold:\n  ${got.join('\n  ')}\nwant exactly:\n  ${expected.join('\n  ')}\n`);
  process.exit(1);
}

process.stdout.write(`VSIX files OK: ${got.join(', ')}\n`);
