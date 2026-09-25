// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Runs the integration suite in real VS Code (1.100.0 and 1.139.0 unless
// EYEDBG_TEST_VSCODE_VERSIONS says otherwise) against real eyedbg binaries
// and debugpy. Every VS Code run gets a fresh temporary root: its own
// profile (never a real VS Code profile), eyedbg runtime and config
// directories and a workspace. EYEDBG_TEST_COUNT=N (or --count N) repeats
// the whole run N times; the first failure stops it.
//
// Environment: EYEDBG_TEST_BIN_DIR (eyedbg and eyedbgd, and the .NET helper
// in helpers/dotnet; else they are built from the repository with go and
// dotnet), EYEDBG_DATA_DIR / EYEDBG_PYTHON (debugpy, as for the Go e2e
// tests; the adapters' directory defaults to ~/.eyedbg/tools, used as is),
// EYEDBG_TEST_DOTNET=0 (skip the .NET tests; else a missing dotnet, helper
// or netcoredbg fails the run). Each VS Code run has its own EYEDBG_HOME, so
// dumps and traces never reach ~/.eyedbg. The .NET sample (breadth) is
// built once per runner invocation. On Linux without DISPLAY it re-runs
// itself under xvfb-run -a.

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

/** dotnetTests: the .NET tests run unless EYEDBG_TEST_DOTNET=0. */
const dotnetTests = process.env.EYEDBG_TEST_DOTNET !== '0';

const helperDll = path.join('helpers', 'dotnet', 'eyedbg-dotnet-helper.dll');

function binaries(scratch: string, dotnet: string | undefined): string {
  const given = process.env.EYEDBG_TEST_BIN_DIR;
  if (given !== undefined && given !== '') {
    const dir = path.resolve(given);
    for (const b of ['eyedbg', 'eyedbgd']) {
      if (!fs.existsSync(path.join(dir, b + exe))) {
        throw new Error(`EYEDBG_TEST_BIN_DIR ${dir} has no ${b}${exe}`);
      }
    }
    if (dotnetTests && !fs.existsSync(path.join(dir, helperDll))) {
      throw new Error(
        `EYEDBG_TEST_BIN_DIR ${dir} has no ${helperDll} (publish it there, see CONTRIBUTING.md), or set EYEDBG_TEST_DOTNET=0`,
      );
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
  if (dotnet !== undefined) {
    log(`publishing the .NET helper into ${path.join(dir, 'helpers', 'dotnet')}`);
    execFileSync(
      dotnet,
      [
        'publish',
        path.join('src', 'EyeDbg.DotnetHelper', 'EyeDbg.DotnetHelper.csproj'),
        '-c',
        'Release',
        '-p:RestoreLockedMode=true',
        '-nodeReuse:false',
        '-p:UseSharedCompilation=false',
        '-o',
        path.join(dir, 'helpers', 'dotnet'),
      ],
      { cwd: path.join(repoRoot, 'helpers', 'dotnet'), stdio: 'inherit' },
    );
  }
  return dir;
}

/** dataDir is where the adapters are: EYEDBG_DATA_DIR, else ~/.eyedbg/tools (EYEDBG_HOME moves elsewhere). */
function dataDir(): string {
  const given = process.env.EYEDBG_DATA_DIR;
  return given !== undefined && given !== '' ? path.resolve(given) : path.join(os.homedir(), '.eyedbg', 'tools');
}

/** The .NET sample the .NET tests run: breadth, built in the scratch directory. */
interface Breadth {
  dll: string;
  src: string;
}

/**
 * dotnetSetup checks what the .NET tests need — dotnet, netcoredbg — and
 * builds testdata/apps/dotnet/breadth once, in a copy without bin/obj, so no
 * build server stays behind; undefined when EYEDBG_TEST_DOTNET=0.
 */
function dotnetSetup(scratch: string, eyedbg: string, dotnet: string | undefined): Breadth | undefined {
  if (!dotnetTests) {
    log('EYEDBG_TEST_DOTNET=0: skipping the .NET tests');
    return undefined;
  }
  if (dotnet === undefined) {
    throw new Error('no dotnet on PATH: install the .NET 10 SDK, or set EYEDBG_TEST_DOTNET=0');
  }
  const doctor = spawnSync(eyedbg, ['adapters', 'doctor', 'dotnet', '--json'], {
    env: { ...process.env, EYEDBG_DATA_DIR: dataDir() },
    encoding: 'utf8',
    timeout: 60_000,
  });
  const line = (doctor.stdout ?? '').split(/\r?\n/)[0] ?? '';
  let checks: { name?: string; ok?: boolean; detail?: string; fix?: string }[] = [];
  try {
    checks = (JSON.parse(line) as { checks?: typeof checks }).checks ?? [];
  } catch {
    throw new Error(`eyedbg adapters doctor dotnet printed no JSON: ${doctor.stdout}${doctor.stderr}`);
  }
  const bad = checks.filter((c) => c.ok !== true);
  if (bad.length > 0 || checks.length === 0) {
    throw new Error(
      `the .NET tests need: ${bad.map((c) => `${c.name}: ${c.detail} (${c.fix ?? ''})`).join('; ') || 'a netcoredbg'} — or set EYEDBG_TEST_DOTNET=0`,
    );
  }
  const src = path.join(fs.realpathSync.native(scratch), 'breadth');
  fs.cpSync(path.join(repoRoot, 'testdata', 'apps', 'dotnet', 'breadth'), src, {
    recursive: true,
    filter: (f) => !['bin', 'obj'].includes(path.basename(f)),
  });
  log(`building breadth in ${src}`);
  execFileSync(dotnet, ['build', '-c', 'Debug', '-nodeReuse:false', '-p:UseSharedCompilation=false'], {
    cwd: src,
    stdio: 'inherit',
    env: { ...process.env, DOTNET_CLI_TELEMETRY_OPTOUT: '1', DOTNET_NOLOGO: '1' },
  });
  const debug = path.join(src, 'bin', 'Debug');
  const tfm = fs.readdirSync(debug).find((d) => fs.existsSync(path.join(debug, d, 'breadth.dll')));
  if (tfm === undefined) {
    throw new Error(`no breadth.dll under ${debug}`);
  }
  return { dll: path.join(debug, tfm, 'breadth.dll'), src };
}

/** eyedbgEnv is the environment of one run's eyedbg: its own runtime, config and home (dumps, traces) directories. */
function eyedbgEnv(root: string): Record<string, string | undefined> {
  return {
    EYEDBG_RUNTIME_DIR: path.join(root, 'rt'),
    EYEDBG_CONFIG_DIR: path.join(root, 'cfg'),
    EYEDBG_HOME: path.join(root, 'home'),
    EYEDBG_DATA_DIR: dataDir(),
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

async function runOnce(run: number, version: string, bin: string, breadth: Breadth | undefined): Promise<void> {
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
        EYEDBG_TEST_DOTNET: breadth !== undefined ? '1' : '0',
        EYEDBG_TEST_BREADTH: breadth?.dll ?? '',
        EYEDBG_TEST_BREADTH_SRC: breadth?.src ?? '',
        EYEDBG_TEST_DOTNET_HOST: onPath(`dotnet${exe}`) ?? '',
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
    const dotnet = dotnetTests ? onPath(`dotnet${exe}`) : undefined;
    const bin = binaries(scratch, dotnet);
    const breadth = dotnetSetup(scratch, path.join(bin, `eyedbg${exe}`), dotnet);
    for (let run = 1; run <= n; run++) {
      const started = Date.now();
      for (const version of versions) {
        await runOnce(run, version, bin, breadth);
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
