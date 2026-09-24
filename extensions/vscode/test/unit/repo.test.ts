// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Invariants of the extension's own files: headers, licence, the pure core,
// and the manifest's security-relevant settings.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { test } from 'node:test';

// The bundle runs from out/unit/.
const root = path.resolve(__dirname, '..', '..');

const header = '// Copyright The EyeDebugger Authors\n// SPDX-License-Identifier: Apache-2.0\n';

function sources(dir: string): string[] {
  const out: string[] = [];
  for (const e of fs.readdirSync(path.join(root, dir), { withFileTypes: true })) {
    const rel = path.join(dir, e.name);
    if (e.isDirectory()) {
      out.push(...sources(rel));
    } else if (/\.(ts|mjs)$/.test(e.name)) {
      out.push(rel);
    }
  }
  return out;
}

interface Manifest {
  engines: Record<string, string>;
  dependencies?: unknown;
  devDependencies: Record<string, string>;
  packageManager: string;
  extensionKind: string[];
  capabilities: { untrustedWorkspaces: { supported: boolean }; virtualWorkspaces: boolean };
  contributes: { configuration: { properties: Record<string, { scope?: string }> } };
}

const manifest = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8')) as Manifest;

test('every source file starts with the SPDX header', () => {
  const files = [...sources('src'), ...sources('test'), ...sources('scripts'), 'esbuild.mjs'];
  assert.ok(files.length > 3, `found only ${files.join(', ')}`);
  for (const f of files) {
    const text = fs.readFileSync(path.join(root, f), 'utf8').replaceAll('\r\n', '\n');
    assert.ok(text.startsWith(header), `${f} lacks the two-line SPDX header`);
  }
});

test('LICENSE is the repository licence', () => {
  const own = fs.readFileSync(path.join(root, 'LICENSE'));
  const repo = fs.readFileSync(path.join(root, '..', '..', 'LICENSE'));
  assert.ok(own.equals(repo), 'extensions/vscode/LICENSE differs from /LICENSE');
});

test('nothing under src/core imports vscode', () => {
  for (const f of sources(path.join('src', 'core'))) {
    const text = fs.readFileSync(path.join(root, f), 'utf8');
    assert.doesNotMatch(text, /from\s+['"]vscode['"]|require\(\s*['"]vscode['"]\s*\)|import\(\s*['"]vscode['"]/, f);
  }
});

test('manifest invariants', () => {
  const props = manifest.contributes.configuration.properties;
  assert.equal(props['eyedbg.path']?.scope, 'machine');
  assert.equal(props['eyedbg.clientName']?.scope, 'machine');
  assert.equal(manifest.capabilities.untrustedWorkspaces.supported, false);
  assert.equal(manifest.capabilities.virtualWorkspaces, false);
  assert.deepEqual(manifest.extensionKind, ['workspace']);
  assert.equal(manifest.engines.vscode, '^1.100.0');
  assert.equal(manifest.devDependencies['@types/vscode'], '1.100.0');
  assert.equal(manifest.dependencies, undefined, 'the extension has no runtime dependencies');
  for (const [name, version] of Object.entries(manifest.devDependencies)) {
    assert.match(version, /^\d+\.\d+\.\d+$/, `${name} must be pinned exactly`);
  }
  assert.match(manifest.packageManager, /^pnpm@11\.\d+\.\d+\+sha512\.[0-9a-f]{128}$/);
});
