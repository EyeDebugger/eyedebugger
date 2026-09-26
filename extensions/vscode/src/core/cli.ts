// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The eyedbg command lines the extension runs, and reading their --json
// results. Every flag value is bound as --flag=value, and a launch
// configuration's program arguments follow "--": no value can become a flag.

import path from 'node:path';
import { isAbsolute } from './binary';
import { EyedbgError, isRecord, parseSessionInfo, type SessionInfo, str } from './protocol';
import type { LaunchSpec, Result } from './validate';

/** The features 'eyedbg version --json' must list for this extension. */
export const requiredFeatures = ['dap', 'presence', 'lease.request', 'dap.collab'] as const;

/** The feature a launch configuration's "adapter" needs: 'eyedbg start --adapter'. */
export const adapterFeature = 'adapter.select';

/** The feature a launch configuration needs: 'eyedbg dap --launch' (docs/adr/0019). */
export const launchFeature = 'dap.launch';

/** launchUnsupported is the error text for a launch on an eyedbg without launchFeature, else ''. */
export function launchUnsupported(version: string, features: readonly string[]): string {
  if (features.includes(launchFeature)) {
    return '';
  }
  return `eyedbg ${version} can't start a session from a launch configuration: it lacks ${launchFeature}`;
}

/** adapterUnsupported is the error text for a launch with "adapter" on an eyedbg without adapterFeature, else ''. */
export function adapterUnsupported(spec: LaunchSpec, version: string, features: readonly string[]): string {
  if (spec.adapter === undefined || features.includes(adapterFeature)) {
    return '';
  }
  return `eyedbg ${version} can't choose a debug adapter ("adapter": "${spec.adapter}"): it lacks ${adapterFeature}`;
}

export function versionArgs(): string[] {
  return ['version', '--json'];
}

export function sessionsArgs(): string[] {
  return ['sessions', '--json'];
}

/**
 * launchCwd resolves a launch configuration's cwd against the workspace
 * folder (a relative cwd without a folder is refused) and says where to run
 * 'eyedbg dap --launch' (eyedbg resolves the launch's relative paths against
 * its own directory): the resolved cwd, else the folder, else an absolute
 * program's directory, else nowhere in particular (undefined). The returned
 * spec carries the absolute cwd, so the launch's cwd and the process agree.
 */
export function launchCwd(
  spec: LaunchSpec,
  folder: string | undefined,
  platform: string,
): Result<{ spec: LaunchSpec; cwd: string | undefined }> {
  const p = platform === 'win32' ? path.win32 : path.posix;
  if (spec.cwd !== undefined) {
    let cwd = spec.cwd;
    // A drive-relative cwd (C:foo) resolves against that drive's current
    // directory, which is the extension host's, not the workspace's.
    if (platform === 'win32' && /^[A-Za-z]:(?![\\/])/.test(cwd)) {
      return {
        ok: false,
        error: `"cwd" ${JSON.stringify(cwd)} is relative to a drive's current directory; use an absolute path or one relative to the workspace folder`,
      };
    }
    if (!isAbsolute(cwd, platform)) {
      if (folder === undefined) {
        return {
          ok: false,
          error: `"cwd" ${JSON.stringify(cwd)} is relative and there is no workspace folder to resolve it against`,
        };
      }
      cwd = p.resolve(folder, cwd);
    }
    return { ok: true, value: { spec: { ...spec, cwd }, cwd } };
  }
  if (folder !== undefined) {
    return { ok: true, value: { spec, cwd: folder } };
  }
  if (spec.program !== undefined && isAbsolute(spec.program, platform)) {
    return { ok: true, value: { spec, cwd: p.dirname(spec.program) } };
  }
  return { ok: true, value: { spec, cwd: undefined } };
}

export function dapArgs(session: string, client: string): string[] {
  return ['dap', `--session=${session}`, `--as=${client}`];
}

/** dapLaunchArgs runs a facade connection whose DAP launch starts the session (docs/adr/0019). */
export function dapLaunchArgs(client: string): string[] {
  return ['dap', '--launch', `--as=${client}`];
}

/** redact hides the values of --env=K=V in argv, for the log. */
export function redact(argv: readonly string[]): string[] {
  const out: string[] = [];
  let programArgs = false;
  for (const a of argv) {
    if (programArgs) {
      out.push(a);
    } else if (a === '--') {
      programArgs = true;
      out.push(a);
    } else if (a.startsWith('--env=')) {
      const eq = a.indexOf('=', '--env='.length);
      out.push(eq < 0 ? a : `${a.slice(0, eq)}=***`);
    } else {
      out.push(a);
    }
  }
  return out;
}

/** quote cuts s to at most n characters for an error message. */
export function quote(s: string, n = 200): string {
  const chars = Array.from(s.trim());
  return JSON.stringify(chars.length > n ? `${chars.slice(0, n).join('')}…` : chars.join(''));
}

/**
 * parseResult reads a --json command's stdout: the value, or the
 * EyedbgError it reports ({"error": {code, message, hint}}, exit non-zero).
 * Output that isn't JSON is an INTERNAL error quoting a little of it.
 */
export function parseResult(exitCode: number, stdout: string, stderr: string): Record<string, unknown> {
  let value: unknown;
  try {
    value = JSON.parse(stdout);
  } catch {
    const shown = stdout.trim() !== '' ? stdout : stderr;
    throw new EyedbgError(
      'INTERNAL',
      `eyedbg exited with code ${exitCode} and printed no JSON: ${quote(shown)}`,
      'run the same eyedbg command in a terminal to see why',
    );
  }
  if (!isRecord(value)) {
    throw new EyedbgError('INTERNAL', `eyedbg printed unexpected JSON: ${quote(stdout)}`);
  }
  const err = errorOf(value);
  if (err !== undefined) {
    throw err;
  }
  if (exitCode !== 0) {
    throw new EyedbgError('INTERNAL', `eyedbg exited with code ${exitCode}: ${quote(stderr || stdout)}`);
  }
  return value;
}

/** errorOf is the error a --json result reports ({"error": {code, message, hint}}), if any. */
export function errorOf(value: unknown): EyedbgError | undefined {
  const err = isRecord(value) ? value.error : undefined;
  if (!isRecord(err)) {
    return undefined;
  }
  return new EyedbgError(str(err.code) || 'INTERNAL', str(err.message) || 'eyedbg failed', str(err.hint));
}

export interface VersionInfo {
  version: string;
  features: string[];
}

/** checkVersion validates 'eyedbg version --json': schema 1, name eyedbg, every required feature. */
export function checkVersion(v: Record<string, unknown>): VersionInfo {
  if (v.schema !== 1 || v.name !== 'eyedbg') {
    throw new EyedbgError(
      'VERSION_MISMATCH',
      `this isn't an eyedbg this extension understands (version --json: schema ${quote(String(v.schema), 20)}, name ${quote(str(v.name), 40)})`,
      'check eyedbg.path',
    );
  }
  const features = Array.isArray(v.features) ? v.features.filter((f): f is string => typeof f === 'string') : [];
  const missing = requiredFeatures.filter((f) => !features.includes(f));
  const version = str(v.version) || 'unknown';
  if (missing.length > 0) {
    throw new EyedbgError(
      'VERSION_MISMATCH',
      `eyedbg ${version} is too old for this extension: it lacks ${missing.join(', ')}`,
      'update eyedbg',
    );
  }
  return { version, features };
}

/** parseSessions reads 'eyedbg sessions --json'. */
export function parseSessions(v: Record<string, unknown>): SessionInfo[] {
  const out: SessionInfo[] = [];
  for (const s of Array.isArray(v.sessions) ? v.sessions : []) {
    const info = parseSessionInfo(s);
    if (info !== undefined) {
      out.push(info);
    }
  }
  return out;
}

/** isLive reports whether a session can be joined. */
export function isLive(s: SessionInfo): boolean {
  return s.state !== 'exited' && s.state !== 'lost';
}

/**
 * dapExitMessage explains an 'eyedbg dap' that exited before the debug
 * session started (its stderr is lost); session is '' for a launch
 * connection ('eyedbg dap --launch'), which has no session yet.
 */
export function dapExitMessage(code: number | undefined, session: string): string {
  if (session === '') {
    switch (code) {
      case 0:
        return 'The connection to eyedbg ended before the session started.';
      case 3:
        return "eyedbg couldn't start its daemon, or the running daemon is too old for this extension or refused its token: run 'eyedbg daemon stop' and try again.";
      default:
        return `Lost the connection to the eyedbg daemon (exit code ${code ?? 'unknown'}); 'eyedbg daemon logs' may say why.`;
    }
  }
  switch (code) {
    case 0:
      return `The connection to eyedbg session ${session} ended.`;
    case 2:
      return `eyedbg session ${session} doesn't exist or has exited (see 'eyedbg sessions').`;
    case 3:
      return "The running eyedbg daemon is too old for this extension, or refused its token: run 'eyedbg daemon stop' and try again.";
    default:
      return `Lost the connection to the eyedbg daemon for session ${session} (exit code ${code ?? 'unknown'}); 'eyedbg daemon logs' may say why.`;
  }
}
