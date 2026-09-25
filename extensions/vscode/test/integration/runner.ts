// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Runs the integration suite in real VS Code (1.100.0 and 1.139.0 unless
// EYEDBG_TEST_VSCODE_VERSIONS says otherwise) against real eyedbg binaries
// and debugpy. Every VS Code run gets a fresh temporary root: its own
// profile (never a real VS Code profile), eyedbg runtime and config
// directories and a workspace. EYEDBG_TEST_COUNT=N (or --count N) repeats
// the whole run N times; the first failure stops it.
//
// Environment: EYEDBG_TEST_BIN_DIR (eyedbg and eyedbgd; else they are built
// from the repository with go), EYEDBG_DATA_DIR / EYEDBG_PYTHON (debugpy, as
// for the Go e2e tests). On Linux without DISPLAY it re-runs itself under
// xvfb-run -a.

import { execFileSync, spawnSync } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { runTests } from '@vscode/test-electron';

const defaultVersions = ['1.100.0', '1.139.0'];

const extRoot = path.resolve(__dirname, '..', '..');
const repoRoot = path.resolve(extRoot, '..', '..');
const exe = process.platform === 'win32' ? '.exe' : '';

function log(msg: string): void {
  process.stdout.write(`[runner] ${msg}\n`);
}

function count(): number {
  const args = process.argv.slice(2).filter((a) => a !== '--');
  const i = args.indexOf('--count');
  const raw = i >= 0 ? args[i + 1] : process.env.EYEDBG_TEST_COUNT;
  const n = Number(raw ?? '1');
  if (!Number.isInteger(n) || n < 1) {
    throw new Error(`the run count must be a positive integer, not ${JSON.stringify(raw)}`);
  }
  return n;
}

function onPath(name: string): string | undefined {
  for (const dir of (process.env.PATH ?? '').split(path.delimiter)) {
    if (path.isAbsolute(dir) && fs.existsSync(path.join(dir, name))) {
      return path.join(dir, name);
    }
  }
  return undefined;
}

/** underXvfb re-runs this runner under xvfb-run on a Linux without a display; the exit code, or undefined to go on. */
function underXvfb(): number | undefined {
  if (process.platform !== 'linux' || process.env.DISPLAY || process.env.EYEDBG_TEST_XVFB === '1') {
    return undefined;
  }
  const xvfb = onPath('xvfb-run');
  if (xvfb === undefined) {
    throw new Error('no DISPLAY and no xvfb-run: install xvfb (e.g. sudo apt-get install -y xvfb)');
  }
  log('no DISPLAY: re-running under xvfb-run -a');
  const r = spawnSync(xvfb, ['-a', process.execPath, __filename, ...process.argv.slice(2)], {
    stdio: 'inherit',
    env: { ...process.env, EYEDBG_TEST_XVFB: '1' },
  });
  return r.status ?? 1;
}

/** realProfiles are the VS Code profiles of the user: the runner never points VS Code at them. */
function realProfiles(): string[] {
  const home = os.homedir();
  const out = [path.join(home, '.vscode'), path.join(home, '.config', 'Code')];
  if (process.platform === 'darwin') {
    out.push(path.join(home, 'Library', 'Application Support', 'Code'));
  }
  if (process.env.APPDATA) {
    out.push(path.join(process.env.APPDATA, 'Code'));
  }
  return out.map((p) => (fs.existsSync(p) ? fs.realpathSync(p) : p));
}

function inside(child: string, parent: string): boolean {
  const rel = path.relative(parent, child);
  return rel === '' || (!rel.startsWith('..') && !path.isAbsolute(rel));
}

function refuseRealProfile(dirs: string[]): void {
  for (const d of dirs) {
    const real = fs.realpathSync(d);
    for (const p of realProfiles()) {
      if (inside(real, p)) {
        throw new Error(`refusing to run VS Code with ${real}: it is inside the real VS Code profile ${p}`);
      }
    }
  }
}

function binaries(scratch: string): string {
  const given = process.env.EYEDBG_TEST_BIN_DIR;
  if (given !== undefined && given !== '') {
    const dir = path.resolve(given);
    for (const b of ['eyedbg', 'eyedbgd']) {
      if (!fs.existsSync(path.join(dir, b + exe))) {
        throw new Error(`EYEDBG_TEST_BIN_DIR ${dir} has no ${b}${exe}`);
      }
    }
    return dir;
  }
  const dir = path.join(scratch, 'bin');
  fs.mkdirSync(dir);
  log(`building eyedbg and eyedbgd into ${dir}`);
  execFileSync('go', ['build', '-trimpath', '-o', dir + path.sep, './cmd/eyedbg', './cmd/eyedbgd'], {
    cwd: repoRoot,
    stdio: 'inherit',
  });
  return dir;
}

/** eyedbgEnv is the environment of one run's eyedbg: its own runtime and config directories. */
function eyedbgEnv(root: string): Record<string, string | undefined> {
  return {
    EYEDBG_RUNTIME_DIR: path.join(root, 'rt'),
    EYEDBG_CONFIG_DIR: path.join(root, 'cfg'),
    EYEDBG_CLIENT: undefined,
    EYEDBG_SESSION: undefined,
    EYEDBG_NO_AUTOSTART: undefined,
    EYEDBG_DAEMON_PATH: undefined,
  };
}

/**
 * prepare makes one run's directories. VS Code runs in portable mode
 * (VSCODE_PORTABLE=<root>/portable): its per-user files — argv.json, which
 * it otherwise reads and creates in ~/.vscode, the user data (portable mode
 * takes precedence over --user-data-dir) and the extensions — all live in
 * the temporary root. The explicit --user-data-dir and --extensions-dir
 * name the same directories.
 */
function prepare(
  root: string,
  eyedbg: string,
): { ws: string; link: string; portable: string; user: string; ext: string } {
  const ws = path.join(root, 'ws');
  const link = path.join(root, 'ws-link');
  const portable = path.join(root, 'portable');
  const user = path.join(portable, 'user-data');
  const ext = path.join(portable, 'extensions');
  for (const d of [path.join(root, 'rt'), path.join(root, 'cfg'), ws, path.join(user, 'User'), ext]) {
    fs.mkdirSync(d, { recursive: true });
  }
  fs.copyFileSync(path.join(repoRoot, 'testdata', 'apps', 'python', 'basic', 'app.py'), path.join(ws, 'app.py'));
  fs.symlinkSync(ws, link, process.platform === 'win32' ? 'junction' : 'dir');
  const settings = {
    'eyedbg.path': eyedbg,
    'eyedbg.clientName': 'test',
    'eyedbg.lease.afterTakeOver': 'keep',
    // No prompts or reveals between the steps of tests that don't want them
    // (activity/follow/autojoin tests switch them on).
    'eyedbg.autoJoin': 'never',
    'eyedbg.followAgent': false,
    'security.workspace.trust.enabled': false,
    'window.restoreWindows': 'none',
    'update.mode': 'none',
    'telemetry.telemetryLevel': 'off',
    'extensions.autoUpdate': false,
    'extensions.autoCheckUpdates': false,
    'workbench.startupEditor': 'none',
    'debug.focusWindowOnBreak': false,
    'debug.openDebug': 'neverOpen',
    'git.enabled': false,
  };
  fs.writeFileSync(path.join(user, 'User', 'settings.json'), JSON.stringify(settings, null, 2));
  refuseRealProfile([portable, user, ext]);
  return { ws, link, portable, user, ext };
}

/**
 * removeTree removes a temp tree, logging a failure instead of throwing: a
 * throw from a finally would replace the run's own failure (Windows reports
 * EBUSY/EPERM while a debuggee or eyedbgd is still exiting).
 */
function removeTree(dir: string): void {
  try {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 });
  } catch (e) {
    log(`couldn't remove ${dir}: ${e instanceof Error ? e.message : String(e)}`);
  }
}

function stopDaemon(eyedbg: string, root: string): void {
  const started = Date.now();
  const r = spawnSync(eyedbg, ['daemon', 'stop', '--force'], {
    env: { ...process.env, ...eyedbgEnv(root) },
    encoding: 'utf8',
    timeout: 60_000,
  });
  const took = `${((Date.now() - started) / 1000).toFixed(1)} s`;
  if (r.status !== 0) {
    log(`eyedbg daemon stop --force: exit ${r.status} after ${took}: ${(r.stderr ?? '').trim()}`);
  } else if (Date.now() - started > 5_000) {
    log(`eyedbg daemon stop --force took ${took}`);
  }
}

async function runOnce(run: number, version: string, bin: string): Promise<void> {
  const eyedbg = path.join(bin, `eyedbg${exe}`);
  // Short (the daemon's socket path budget) and canonical (macOS's /var is
  // a symlink; Windows' temp may be an 8.3 short name): ws-link is the only
  // alias the tests make.
  const root = fs.realpathSync.native(fs.mkdtempSync(path.join(os.tmpdir(), 'edbx-')));
  const started = Date.now();
  log(`run ${run}, VS Code ${version}: root ${root}`);
  try {
    const { ws, link, portable, user, ext } = prepare(root, eyedbg);
    await runTests({
      version,
      cachePath: path.join(extRoot, '.vscode-test'),
      extensionDevelopmentPath: extRoot,
      extensionTestsPath: path.join(extRoot, 'out', 'integration', 'suite.js'),
      launchArgs: [ws, `--user-data-dir=${user}`, `--extensions-dir=${ext}`, '--disable-extensions'],
      extensionTestsEnv: {
        ...eyedbgEnv(root),
        VSCODE_PORTABLE: portable,
        EYEDBG_TEST_EYEDBG: eyedbg,
        EYEDBG_TEST_WS: ws,
        EYEDBG_TEST_WS_LINK: link,
        // Window focus is unreliable under xvfb and on a busy desktop.
        EYEDBG_TEST_ASSUME_FOCUSED: '1',
      },
    });
  } finally {
    stopDaemon(eyedbg, root);
    removeTree(root);
    log(`run ${run}, VS Code ${version}: ${((Date.now() - started) / 1000).toFixed(1)} s`);
  }
}

async function main(): Promise<number> {
  const xvfb = underXvfb();
  if (xvfb !== undefined) {
    return xvfb;
  }
  const n = count();
  const versions = (process.env.EYEDBG_TEST_VSCODE_VERSIONS ?? defaultVersions.join(','))
    .split(',')
    .map((v) => v.trim())
    .filter((v) => v !== '');
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'edbx-bin-'));
  try {
    const bin = binaries(scratch);
    for (let run = 1; run <= n; run++) {
      const started = Date.now();
      for (const version of versions) {
        await runOnce(run, version, bin);
      }
      log(`run ${run}/${n} passed in ${((Date.now() - started) / 1000).toFixed(1)} s`);
    }
    log(`all ${n} run(s) passed (VS Code ${versions.join(', ')})`);
    return 0;
  } finally {
    removeTree(scratch);
  }
}

main().then(
  (code) => process.exit(code),
  (e: unknown) => {
    process.stderr.write(`[runner] FAILED: ${e instanceof Error ? e.message : String(e)}\n`);
    process.exit(1);
  },
);
