// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Fails unless the VSIX would hold exactly the files the extension ships
// ('vsce ls' lists what 'vsce package' takes, after .vscodeignore), and,
// once packaged, unless every image of its README is an absolute URL into
// the repository's extensions/vscode/images (vsce derives image URLs from
// the repository's root, not its directory: package passes the base).

import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const expected = [
  'LICENSE',
  'README.md',
  'dist/extension.js',
  'icon.png',
  'package.json',
  'walkthrough/adapters.md',
  'walkthrough/install.md',
  'walkthrough/join.md',
  'walkthrough/launch.md',
  'walkthrough/watch.md',
];

const imagesUrl = 'https://github.com/EyeDebugger/eyedebugger/raw/HEAD/extensions/vscode/';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
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

// readEntry reads one file of a zip, with the zip reader vsce itself uses.
function readEntry(zipPath, name) {
  const yauzl = createRequire(require.resolve('@vscode/vsce/package.json'))('yauzl');
  return new Promise((resolve, reject) => {
    yauzl.open(zipPath, { lazyEntries: true }, (err, zip) => {
      if (err) {
        reject(err);
        return;
      }
      zip.on('entry', (entry) => {
        if (entry.fileName !== name) {
          zip.readEntry();
          return;
        }
        zip.openReadStream(entry, (e, stream) => {
          if (e) {
            reject(e);
            return;
          }
          const chunks = [];
          stream.on('data', (c) => chunks.push(c));
          stream.on('end', () => {
            zip.close();
            resolve(Buffer.concat(chunks).toString('utf8'));
          });
        });
      });
      zip.on('end', () => resolve(undefined));
      zip.readEntry();
    });
  });
}

// The packaged README (vsce rewrote its links).
const vsix = path.join(root, 'out', 'eyedbg.vsix');
if (!fs.existsSync(vsix)) {
  process.stderr.write(`no ${vsix}: run pnpm run package\n`);
  process.exit(1);
}
const readme = await readEntry(vsix, 'extension/readme.md');
if (typeof readme !== 'string') {
  process.stderr.write(`${vsix} has no extension/readme.md\n`);
  process.exit(1);
}
const images = [...readme.matchAll(/!\[[^\]]*\]\(([^)\s]+)/g)].map((m) => m[1]);
const bad = images.filter((u) => {
  if (!u.startsWith(imagesUrl)) {
    return true;
  }
  const rel = u.slice(imagesUrl.length);
  return !/^images\/[A-Za-z0-9._-]+\.png$/.test(rel) || !fs.existsSync(path.join(root, rel));
});
if (bad.length > 0) {
  process.stderr.write(
    `the packaged README has images that aren't ${imagesUrl}images/<a file in images/>:\n  ${bad.join('\n  ')}\n`,
  );
  process.exit(1);
}
process.stdout.write(`VSIX README images OK: ${images.length}\n`);
