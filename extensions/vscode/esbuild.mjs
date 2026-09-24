// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Bundles the extension and its tests: node esbuild.mjs extension|unit|integration.

import { build } from 'esbuild';

const common = { bundle: true, format: 'cjs', platform: 'node', logLevel: 'warning', minify: false, sourcemap: false };

const modes = {
  // The extension: Node built-ins and vscode only (no runtime dependencies).
  extension: [
    {
      ...common,
      entryPoints: ['src/extension.ts'],
      outfile: 'dist/extension.js',
      target: 'node20',
      external: ['vscode'],
    },
  ],
  // The unit tests: vscode is not external, so a unit-tested module that
  // imports it fails the build.
  unit: [{ ...common, entryPoints: ['test/unit/index.ts'], outfile: 'out/unit/all.test.js', target: 'node22' }],
  // The integration runner (Node) and the suite (in VS Code's extension host).
  integration: [
    {
      ...common,
      entryPoints: ['test/integration/runner.ts'],
      outfile: 'out/integration/runner.js',
      target: 'node22',
      packages: 'external',
    },
    {
      ...common,
      entryPoints: ['test/integration/suite.ts'],
      outfile: 'out/integration/suite.js',
      target: 'node20',
      external: ['vscode'],
    },
  ],
};

const mode = process.argv[2] ?? '';
const configs = modes[mode];
if (!configs) {
  process.stderr.write(`usage: node esbuild.mjs ${Object.keys(modes).join('|')}\n`);
  process.exit(2);
}

await Promise.all(configs.map((c) => build(c)));
