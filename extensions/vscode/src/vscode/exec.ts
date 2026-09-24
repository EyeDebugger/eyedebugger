// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The only place the extension runs a process (docs/adr/0015): the eyedbg
// binary resolved to an absolute path, through execFile without a shell,
// with every argument built by core/cli.

import { type ExecFileException, execFile } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import * as vscode from 'vscode';
import { type BinaryEnv, type FileInfo, resolveBinary } from '../core/binary';
import { checkVersion, parseResult, redact, type VersionInfo, versionArgs } from '../core/cli';
import { EyedbgError } from '../core/protocol';

const maxBuffer = 4 << 20;

export const timeouts = {
  short: 30_000,
  start: 600_000,
} as const;

/** isDirectory reports whether p is an existing directory. */
export function isDirectory(p: string): boolean {
  try {
    return fs.statSync(p).isDirectory();
  } catch {
    return false;
  }
}

function statFile(p: string): FileInfo | undefined {
  try {
    const s = fs.statSync(p);
    return { isFile: s.isFile(), mode: s.mode, size: s.size, mtimeMs: s.mtimeMs };
  } catch {
    return undefined;
  }
}

function binaryEnv(): BinaryEnv {
  return { platform: process.platform, env: process.env, home: os.homedir(), stat: statFile };
}

export interface RunOptions {
  timeoutMs: number;
  cwd?: string;
  signal?: AbortSignal;
}

export class Eyedbg {
  private readonly versions = new Map<string, VersionInfo>();

  constructor(private readonly log: vscode.LogOutputChannel) {}

  /** binary returns the eyedbg to run (absolute), checked once per (path, size, mtime). */
  async binary(): Promise<string> {
    const setting = vscode.workspace.getConfiguration('eyedbg').get<string>('path', '');
    const r = resolveBinary(typeof setting === 'string' ? setting : '', binaryEnv());
    if (!r.ok) {
      throw new EyedbgError('EYEDBG_NOT_FOUND', r.error);
    }
    const key = `${r.path}\u0000${r.info.size}\u0000${r.info.mtimeMs}`;
    if (!this.versions.has(key)) {
      const v = checkVersion(await this.exec(r.path, versionArgs(), { timeoutMs: timeouts.short }));
      this.versions.set(key, v);
      this.log.info(`using ${r.path} (eyedbg ${v.version})`);
    }
    return r.path;
  }

  /** run runs 'eyedbg ARGS' (a --json command) and returns its JSON result. */
  async run(argv: string[], opts: RunOptions): Promise<Record<string, unknown>> {
    return this.exec(await this.binary(), argv, opts);
  }

  private exec(file: string, argv: string[], opts: RunOptions): Promise<Record<string, unknown>> {
    this.log.info(`run: eyedbg ${redact(argv).join(' ')}`);
    return new Promise((resolve, reject) => {
      execFile(
        file,
        argv,
        {
          shell: false,
          windowsHide: true,
          timeout: opts.timeoutMs,
          maxBuffer,
          encoding: 'utf8',
          ...(opts.cwd !== undefined ? { cwd: opts.cwd } : {}),
          ...(opts.signal !== undefined ? { signal: opts.signal } : {}),
        },
        (err, stdout, stderr) => {
          if (err !== null && typeof err.code !== 'number') {
            reject(this.spawnError(err, argv, opts));
            return;
          }
          try {
            resolve(parseResult(err === null ? 0 : (err.code as number), stdout, stderr));
          } catch (e) {
            if (e instanceof EyedbgError) {
              this.log.warn(`eyedbg ${argv[0] ?? ''}: ${e.code}: ${e.message}${e.hint !== '' ? ` — ${e.hint}` : ''}`);
            }
            reject(e);
          }
        },
      );
    });
  }

  private spawnError(err: ExecFileException, argv: string[], opts: RunOptions): EyedbgError {
    const cmd = argv[0] ?? '';
    let e: EyedbgError;
    if (opts.signal?.aborted === true || err.name === 'AbortError') {
      e = new EyedbgError('CANCELED', `eyedbg ${cmd} was canceled`);
    } else if (err.killed === true) {
      e = new EyedbgError(
        'TIMEOUT',
        `eyedbg ${cmd} took longer than ${Math.round(opts.timeoutMs / 1000)} s and was stopped`,
      );
    } else if (err.code === 'ENOENT' && opts.cwd !== undefined && !isDirectory(opts.cwd)) {
      // Node reports a missing working directory as the spawn's ENOENT.
      e = new EyedbgError('INVALID_REQUEST', `couldn't run eyedbg ${cmd} in ${opts.cwd}: no such directory`);
    } else if (err.code === 'ENOENT' || err.code === 'EACCES') {
      e = new EyedbgError('EYEDBG_NOT_FOUND', `couldn't run eyedbg (${err.code}); check eyedbg.path or PATH`);
    } else if (err.code === 'ERR_CHILD_PROCESS_STDIO_MAXBUFFER') {
      e = new EyedbgError('INTERNAL', `eyedbg ${cmd} printed more than 4 MiB`);
    } else {
      e = new EyedbgError('INTERNAL', `couldn't run eyedbg ${cmd}: ${err.message}`);
    }
    this.log.warn(`eyedbg ${cmd}: ${e.code}: ${e.message}`);
    return e;
  }
}
