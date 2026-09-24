// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Which eyedbg executable the extension runs (docs/adr/0015): the
// machine-scoped eyedbg.path setting, else a PATH search the extension does
// itself — absolute entries only, never the current directory (which
// Windows' process spawning would try first) — and on Windows only
// eyedbg.exe (a .cmd or .bat would run through cmd.exe).

import path from 'node:path';

export interface FileInfo {
  isFile: boolean;
  /** POSIX permission bits (ignored on Windows). */
  mode: number;
  size: number;
  mtimeMs: number;
}

/** The environment binary resolution reads, injected for tests. */
export interface BinaryEnv {
  platform: string;
  env: Readonly<Record<string, string | undefined>>;
  home: string;
  stat(p: string): FileInfo | undefined;
}

export type Resolved = { ok: true; path: string; info: FileInfo } | { ok: false; error: string };

export const notFound =
  "eyedbg wasn't found: install it (https://github.com/EyeDebugger/eyedebugger) and put it on PATH, " +
  'or set eyedbg.path in your user settings to its absolute path';

function pathApi(platform: string): path.PlatformPath {
  return platform === 'win32' ? path.win32 : path.posix;
}

/**
 * isAbsolute: on Windows a drive (C:\\) or UNC (\\\\server) path only; a
 * rooted path without a drive (\\bin) depends on the current drive.
 */
export function isAbsolute(p: string, platform: string): boolean {
  if (platform === 'win32') {
    return /^[A-Za-z]:[\\/]/.test(p) || /^[\\/]{2}[^\\/]/.test(p);
  }
  return path.posix.isAbsolute(p);
}

function usable(p: string, platform: string, info: FileInfo | undefined): info is FileInfo {
  if (info === undefined || !info.isFile) {
    return false;
  }
  if (platform === 'win32') {
    return p.toLowerCase().endsWith('.exe');
  }
  return (info.mode & 0o111) !== 0;
}

/** resolveSetting checks an eyedbg.path value. */
function resolveSetting(setting: string, e: BinaryEnv): Resolved {
  const p = pathApi(e.platform);
  let file = setting;
  if (file.startsWith('~/') || (e.platform === 'win32' && file.startsWith('~\\'))) {
    file = p.join(e.home, file.slice(2));
  }
  if (file.includes('\0') || !isAbsolute(file, e.platform)) {
    return {
      ok: false,
      error: `eyedbg.path must be an absolute path (or start with ~/), not ${JSON.stringify(setting)}`,
    };
  }
  file = p.normalize(file);
  if (e.platform === 'win32' && !file.toLowerCase().endsWith('.exe')) {
    return {
      ok: false,
      error: `eyedbg.path must name an .exe file on Windows (a .cmd or .bat would run through cmd.exe), not ${JSON.stringify(setting)}`,
    };
  }
  const info = e.stat(file);
  if (!usable(file, e.platform, info)) {
    return {
      ok: false,
      error: `eyedbg.path ${JSON.stringify(setting)} is not an executable file; fix it in your user settings`,
    };
  }
  return { ok: true, path: file, info };
}

/** pathEntries returns PATH's absolute entries (Windows: the Path key in any case, quotes removed). */
export function pathEntries(e: BinaryEnv): string[] {
  const p = pathApi(e.platform);
  let value: string | undefined;
  if (e.platform === 'win32') {
    const key = Object.keys(e.env).find((k) => k.toUpperCase() === 'PATH');
    value = key === undefined ? undefined : e.env[key];
  } else {
    value = e.env.PATH;
  }
  const out: string[] = [];
  for (let entry of (value ?? '').split(p.delimiter)) {
    if (e.platform === 'win32' && entry.length >= 2 && entry.startsWith('"') && entry.endsWith('"')) {
      entry = entry.slice(1, -1);
    }
    if (entry !== '' && !entry.includes('\0') && isAbsolute(entry, e.platform)) {
      out.push(entry);
    }
  }
  return out;
}

/** resolveBinary finds the eyedbg to run; the result is always an absolute path. */
export function resolveBinary(setting: string, e: BinaryEnv): Resolved {
  if (setting.trim() !== '') {
    return resolveSetting(setting.trim(), e);
  }
  const p = pathApi(e.platform);
  const name = e.platform === 'win32' ? 'eyedbg.exe' : 'eyedbg';
  for (const dir of pathEntries(e)) {
    const file = p.join(dir, name);
    const info = e.stat(file);
    if (usable(file, e.platform, info)) {
      return { ok: true, path: file, info };
    }
  }
  return { ok: false, error: notFound };
}
