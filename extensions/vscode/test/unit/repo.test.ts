// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Invariants of the extension's own files: headers, licence, the pure core,
// and the manifest's security-relevant settings.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { test } from 'node:test';
import { activityKinds } from '../../src/core/activity';
import { validateLaunch } from '../../src/core/validate';

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

interface Setting {
  scope?: string;
  type?: string;
  default?: unknown;
  enum?: string[];
  items?: { enum?: string[] };
}

interface MenuItem {
  command: string;
  when?: string;
}

interface Manifest {
  engines: Record<string, string>;
  dependencies?: unknown;
  devDependencies: Record<string, string>;
  packageManager: string;
  extensionKind: string[];
  activationEvents: string[];
  capabilities: { untrustedWorkspaces: { supported: boolean }; virtualWorkspaces: boolean };
  contributes: {
    configuration: { properties: Record<string, Setting> };
    viewsContainers: Record<string, { id: string; title: string; icon: string }[]>;
    views: Record<string, { id: string; name: string; when?: string }[]>;
    viewsWelcome: { view: string; contents: string }[];
    commands: { command: string; title: string; category?: string }[];
    menus: Record<string, MenuItem[]>;
    debuggers: { configurationSnippets: { label: string; body: Record<string, unknown> }[] }[];
    walkthroughs: {
      steps: { id: string; description: string; media: { markdown?: string }; completionEvents?: string[] }[];
    }[];
  };
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

test('only src/vscode/exec.ts runs processes', () => {
  const runs =
    /from\s+['"](node:)?child_process['"]|require\(\s*['"](node:)?child_process['"]\s*\)|import\(\s*['"](node:)?child_process['"]/;
  const files = sources('src').filter((f) => runs.test(fs.readFileSync(path.join(root, f), 'utf8')));
  assert.deepEqual(files, [path.join('src', 'vscode', 'exec.ts')]);
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

test('manifest: views, settings, activation', () => {
  const c = manifest.contributes;
  assert.deepEqual(
    c.views.debug?.map((v) => v.id),
    ['eyedbg.activity', 'eyedbg.clients'],
  );
  assert.deepEqual(
    c.viewsWelcome.map((w) => w.view),
    [
      'eyedbg.activity',
      'eyedbg.clients',
      'eyedbg.dotnet.counters',
      'eyedbg.dotnet.memory',
      'eyedbg.dotnet.threads',
      'eyedbg.dotnet.trace',
    ],
  );
  // The .NET views: their own container, shown in .NET workspaces (or on request).
  assert.deepEqual(c.viewsContainers.activitybar, [
    { id: 'eyedbg-dotnet', title: 'EyeDebugger .NET', icon: '$(pulse)' },
  ]);
  assert.deepEqual(
    c.views['eyedbg-dotnet']?.map((v) => [v.id, v.when]),
    [
      ['eyedbg.dotnet.counters', 'eyedbg.dotnet.show'],
      ['eyedbg.dotnet.memory', 'eyedbg.dotnet.show'],
      ['eyedbg.dotnet.threads', 'eyedbg.dotnet.show'],
      ['eyedbg.dotnet.trace', 'eyedbg.dotnet.show'],
    ],
  );
  assert.deepEqual(Object.keys(c.views), ['debug', 'eyedbg-dotnet']);
  const props = c.configuration.properties;
  assert.deepEqual(props['eyedbg.autoJoin']?.enum, ['ask', 'never']);
  assert.equal(props['eyedbg.autoJoin']?.default, 'ask');
  assert.equal(props['eyedbg.autoJoin']?.scope, 'window');
  assert.equal(props['eyedbg.followAgent']?.type, 'boolean');
  assert.equal(props['eyedbg.followAgent']?.default, true);
  assert.equal(props['eyedbg.followAgent']?.scope, 'window');
  assert.equal(props['eyedbg.activity.hide']?.type, 'array');
  assert.deepEqual(props['eyedbg.activity.hide']?.default, []);
  assert.deepEqual(props['eyedbg.activity.hide']?.items?.enum, [...activityKinds]);
  assert.equal(props['eyedbg.activity.hide']?.scope, 'window');
  assert.ok(manifest.activationEvents.includes('onStartupFinished'));
});

// Commands a link or menu may name besides the extension's own.
const builtins = ['workbench.action.terminal.new', 'debug.addConfiguration', 'eyedbg.activity.focus'];

test('manifest: every command a menu or link names exists', () => {
  const c = manifest.contributes;
  const own = new Set(c.commands.map((x) => x.command));
  for (const x of c.commands) {
    assert.ok(x.title !== '' && x.category === 'EyeDebugger', x.command);
  }
  const named: string[] = [];
  for (const items of Object.values(c.menus)) {
    named.push(...items.map((i) => i.command));
  }
  const links = [
    ...c.viewsWelcome.map((w) => w.contents),
    ...c.walkthroughs.flatMap((w) => w.steps.map((s) => s.description)),
  ];
  for (const text of links) {
    named.push(...[...text.matchAll(/\]\(command:([A-Za-z0-9._-]+)\)/g)].map((m) => m[1] ?? ''));
  }
  assert.ok(named.length > 10);
  for (const cmd of named) {
    assert.ok(own.has(cmd) || builtins.includes(cmd), `${cmd} is neither contributed nor a known built-in`);
  }
  // Commands that need a tree node never show in the palette.
  const hiddenInPalette = new Set(
    (c.menus.commandPalette ?? []).filter((i) => i.when === 'false').map((i) => i.command),
  );
  const nodeOnly = [
    'eyedbg.activity.open',
    'eyedbg.dotnet.cancel',
    'eyedbg.dotnet.memory.gcroot',
    'eyedbg.dotnet.memory.copyPath',
    'eyedbg.dotnet.memory.showThreads',
    'eyedbg.dotnet.threads.openFrame',
  ];
  for (const cmd of own) {
    if (nodeOnly.includes(cmd) || cmd.startsWith('eyedbg.clients.')) {
      assert.ok(cmd === 'eyedbg.clients.refresh' || hiddenInPalette.has(cmd), `${cmd} shows in the palette`);
    }
  }
  // The other .NET commands show in the palette only where the views are.
  const palette = new Map((c.menus.commandPalette ?? []).map((i) => [i.command, i.when]));
  for (const cmd of own) {
    if (cmd.startsWith('eyedbg.dotnet.') && cmd !== 'eyedbg.dotnet.showViews' && !nodeOnly.includes(cmd)) {
      assert.equal(palette.get(cmd), 'eyedbg.dotnet.show', cmd);
    }
  }
  assert.equal(palette.has('eyedbg.dotnet.showViews'), false, 'Show .NET Views is always in the palette');
});

test('manifest: walkthrough media ship in the VSIX', () => {
  const ignore = fs.readFileSync(path.join(root, '.vscodeignore'), 'utf8').split(/\r?\n/);
  const steps = manifest.contributes.walkthroughs.flatMap((w) => w.steps);
  assert.equal(steps.length, 5);
  for (const s of steps) {
    const md = s.media.markdown;
    assert.ok(md !== undefined, s.id);
    assert.ok(fs.existsSync(path.join(root, md)), md);
    assert.ok(ignore.includes('!walkthrough/*.md') && /^walkthrough\/[a-z]+\.md$/.test(md), md);
  }
});

/** sample makes a snippet body a launch configuration: ^"…" unquoted, snippet syntax and variables replaced. */
function sample(x: unknown): unknown {
  if (typeof x === 'string') {
    const raw = x.startsWith('^"') && x.endsWith('"') ? x.slice(2, -1) : x;
    return raw
      .replace(/\\\$\{[^}]*\}/g, '/ws')
      .replace(/\$\{\d+:([^}]*)\}/g, '$1')
      .replace(/\$\{[^}]*\}/g, 'x');
  }
  if (Array.isArray(x)) {
    return x.map(sample);
  }
  if (typeof x === 'object' && x !== null) {
    return Object.fromEntries(Object.entries(x).map(([k, v]) => [k, sample(v)]));
  }
  return x;
}

test('manifest: every launch snippet is a valid launch configuration', () => {
  const snippets = manifest.contributes.debuggers[0]?.configurationSnippets ?? [];
  assert.equal(snippets.length, 5);
  let launches = 0;
  for (const s of snippets) {
    const body = sample(s.body) as Record<string, unknown>;
    assert.equal(body.type, 'eyedbg', s.label);
    if (body.request === 'launch') {
      launches++;
      const v = validateLaunch(body);
      assert.ok(v.ok, `${s.label}: ${v.ok ? '' : v.error}`);
    } else {
      assert.equal(body.request, 'attach', s.label);
    }
    assert.doesNotMatch(JSON.stringify(body), /\$\{|\^"/, s.label);
  }
  assert.equal(launches, 4);
  // biome-ignore lint/suspicious/noTemplateCurlyInString: a snippet's text, not a template.
  assert.equal(sample('^"\\${workspaceFolder}/${1:app.py}"'), '/ws/app.py');
});
