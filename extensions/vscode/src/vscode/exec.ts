// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The only place the extension runs a process (docs/adr/0015): the eyedbg
// binary resolved to an absolute path, through execFile — or spawn, for a
// command that streams lines until stopped — without a shell, with every
// argument built by core/cli or core/dotnet.
//
// Interrupting (the .NET commands, P2-M9): an abort, a timeout or stop()
// closes eyedbg's stdin, kept open as a pipe for exactly this — with
// EYEDBG_CANCEL_ON_STDIN_EOF=1 set, eyedbg treats that like Ctrl-C (it
// ends its helper and discards a partial dump or trace). Off Windows it
// also sends SIGINT, which runs the same path sooner; on Windows a signal
// always ends the process at once (Node has no graceful kill there), so
// closing stdin is the only way to ask first. SIGKILL 10 s later either
// way if eyedbg is still running.

import { type ChildProcess, type ExecFileException, execFile, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import * as vscode from 'vscode';
import { type BinaryEnv, type FileInfo, resolveBinary } from '../core/binary';
import { checkVersion, errorOf, parseResult, quote, redact, type VersionInfo, versionArgs } from '../core/cli';
import { LineSplitter } from '../core/dotnet';
import { EyedbgError } from '../core/protocol';

const maxBuffer = 4 << 20;

/** The CLI's opt-in: stdin EOF (or a read error) cancels it like Ctrl-C. */
const envCancelOnStdinEOF = 'EYEDBG_CANCEL_ON_STDIN_EOF';

/** How long an interrupted eyedbg gets to exit before SIGKILL. */
const killAfterMs = 10_000;

/** How much of a streaming command's stderr is kept, for its failure. */
const stderrTail = 4096;

export const timeouts = {
  short: 30_000,
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
  /** quiet logs the run and its failures at trace level (the auto-join poller: every 5 s). */
  quiet?: boolean;
  /** interrupt: an abort or a timeout interrupts eyedbg (closes stdin, SIGINT off Windows), then kills it 10 s later. */
  interrupt?: boolean;
}

export interface StreamOptions {
  /** An abort stops the command, as stop() does. */
  signal?: AbortSignal;
  quiet?: boolean;
}

/** How a streaming command ended: its exit code (null: a signal), and whether stop() ended it. */
export interface StreamEnd {
  code: number | null;
  stopped: boolean;
}

export interface Stream {
  /**
   * done settles once the command exited and its output closed: resolved
   * when it exited 0 or was stopped; rejected with its JSON error, else
   * INTERNAL quoting its stderr.
   */
  done: Promise<StreamEnd>;
  /** stop interrupts the command (closes stdin; SIGINT off Windows) and resolves when it has ended. */
  stop(): Promise<StreamEnd>;
}

/** interruptSignal is SIGINT: sent off Windows only, alongside closing stdin (see the file header). */
function interruptSignal(): NodeJS.Signals {
  return 'SIGINT';
}

function running(child: ChildProcess): boolean {
  return child.exitCode === null && child.signalCode === null;
}

export class Eyedbg {
  private readonly versions = new Map<string, VersionInfo>();

  constructor(private readonly log: vscode.LogOutputChannel) {}

  /** binary returns the eyedbg to run (absolute), checked once per (path, size, mtime). */
  async binary(quiet = false): Promise<string> {
    return (await this.check(quiet)).path;
  }

  /** check is binary with the version and features it reported. */
  async check(quiet = false): Promise<{ path: string; version: string; features: string[] }> {
    const setting = vscode.workspace.getConfiguration('eyedbg').get<string>('path', '');
    const r = resolveBinary(typeof setting === 'string' ? setting : '', binaryEnv());
    if (!r.ok) {
      throw new EyedbgError('EYEDBG_NOT_FOUND', r.error);
    }
    const key = `${r.path}\u0000${r.info.size}\u0000${r.info.mtimeMs}`;
    let v = this.versions.get(key);
    if (v === undefined) {
      v = checkVersion(await this.exec(r.path, versionArgs(), { timeoutMs: timeouts.short, quiet }));
      this.versions.set(key, v);
      this.log.info(`using ${r.path} (eyedbg ${v.version})`);
      // The walkthrough's first step checks off.
      void vscode.commands.executeCommand('setContext', 'eyedbg.installed', true);
    }
    return { path: r.path, version: v.version, features: [...v.features] };
  }

  /** run runs 'eyedbg ARGS' (a --json command) and returns its JSON result. */
  async run(argv: string[], opts: RunOptions): Promise<Record<string, unknown>> {
    return this.exec(await this.binary(opts.quiet === true), argv, opts);
  }

  /** warn logs at warning level, or at trace for a quiet run. */
  private warn(opts: RunOptions, msg: string): void {
    if (opts.quiet === true) {
      this.log.trace(msg);
    } else {
      this.log.warn(msg);
    }
  }

  private exec(file: string, argv: string[], opts: RunOptions): Promise<Record<string, unknown>> {
    const line = `run: eyedbg ${redact(argv).join(' ')}`;
    if (opts.quiet === true) {
      this.log.trace(line);
    } else {
      this.log.info(line);
    }
    return new Promise((resolve, reject) => {
      const interruptible = opts.interrupt === true;
      // reason, once stopInterruptible ran: which of the two closed stdin
      // (and, off Windows, sent SIGINT). Node's own timeout/killSignal
      // aren't used for an interruptible run — on Windows a kill signal
      // always ends the process at once, before eyedbg could react to
      // stdin closing — so this run times itself out instead.
      let reason: 'abort' | 'timeout' | undefined;
      const child = execFile(
        file,
        argv,
        {
          shell: false,
          windowsHide: true,
          maxBuffer,
          encoding: 'utf8',
          ...(interruptible ? { env: { ...process.env, [envCancelOnStdinEOF]: '1' } } : { timeout: opts.timeoutMs }),
          ...(opts.cwd !== undefined ? { cwd: opts.cwd } : {}),
          ...(opts.signal !== undefined && !interruptible ? { signal: opts.signal } : {}),
        },
        (err, stdout, stderr) => {
          opts.signal?.removeEventListener('abort', abort);
          clearTimeout(timer);
          if (interruptible && reason !== undefined) {
            // eyedbg ran its Ctrl-C path, and may have printed "context canceled".
            const synthetic =
              reason === 'timeout'
                ? Object.assign(new Error('eyedbg timed out'), { killed: true })
                : Object.assign(new Error('canceled'), { name: 'AbortError' });
            reject(this.spawnError(synthetic, argv, opts));
            return;
          }
          if (err !== null && typeof err.code !== 'number') {
            reject(this.spawnError(err, argv, opts));
            return;
          }
          try {
            resolve(parseResult(err === null ? 0 : (err.code as number), stdout, stderr));
          } catch (e) {
            if (e instanceof EyedbgError) {
              this.warn(opts, `eyedbg ${argv[0] ?? ''}: ${e.code}: ${e.message}${e.hint !== '' ? ` — ${e.hint}` : ''}`);
            }
            reject(e);
          }
        },
      );
      const stopInterruptible = (r: 'abort' | 'timeout') => {
        if (reason !== undefined || child.pid === undefined || !running(child)) {
          return;
        }
        reason = r;
        child.stdin?.end();
        if (process.platform !== 'win32') {
          child.kill(interruptSignal());
        }
      };
      const abort = () => stopInterruptible('abort');
      const timer =
        interruptible && opts.timeoutMs > 0
          ? setTimeout(() => stopInterruptible('timeout'), opts.timeoutMs)
          : undefined;
      if (interruptible) {
        opts.signal?.addEventListener('abort', abort, { once: true });
        if (opts.signal?.aborted === true) {
          abort();
        }
        this.killLater(child, opts);
      }
    });
  }

  /**
   * killLater kills child with SIGKILL 10 s after an abort or a timeout
   * interrupted it, if it is still running: a hung eyedbg can't keep the
   * view busy. Every timer and listener goes when the child exits.
   */
  private killLater(child: ChildProcess, opts: { timeoutMs?: number; signal?: AbortSignal }): () => void {
    let kill: NodeJS.Timeout | undefined;
    const arm = () => {
      if (kill === undefined && child.pid !== undefined && running(child)) {
        kill = setTimeout(() => {
          if (running(child)) {
            this.log.warn(
              `eyedbg (pid ${child.pid ?? '?'}) didn't exit ${killAfterMs / 1000} s after it was interrupted: killing it`,
            );
            child.kill('SIGKILL');
          }
        }, killAfterMs);
      }
    };
    const timeout = opts.timeoutMs !== undefined ? setTimeout(arm, opts.timeoutMs) : undefined;
    opts.signal?.addEventListener('abort', arm, { once: true });
    child.once('exit', () => {
      clearTimeout(timeout);
      clearTimeout(kill);
      opts.signal?.removeEventListener('abort', arm);
    });
    return arm;
  }

  /**
   * stream runs 'eyedbg ARGS' (a --json command that prints a line per
   * result until interrupted, like 'dotnet counters --watch') and calls
   * onLine with each line that isn't its error. A line over 1 Mi
   * characters kills it and fails it (INTERNAL).
   */
  async stream(argv: string[], onLine: (line: string) => void, opts: StreamOptions = {}): Promise<Stream> {
    const file = await this.binary(opts.quiet === true);
    const quiet: RunOptions = { timeoutMs: 0, ...(opts.quiet !== undefined ? { quiet: opts.quiet } : {}) };
    const cmd = `eyedbg ${redact(argv).join(' ')}`;
    if (opts.quiet === true) {
      this.log.trace(`run: ${cmd}`);
    } else {
      this.log.info(`run: ${cmd}`);
    }
    const child = spawn(file, argv, {
      shell: false,
      windowsHide: true,
      stdio: ['pipe', 'pipe', 'pipe'],
      env: { ...process.env, [envCancelOnStdinEOF]: '1' },
    });
    const splitter = new LineSplitter();
    let stopped = false;
    let failure: EyedbgError | undefined;
    let reported: EyedbgError | undefined;
    let errText = '';
    const interrupt = this.killLater(child, opts.signal !== undefined ? { signal: opts.signal } : {});
    const stop = () => {
      stopped = true;
      if (running(child) && child.pid !== undefined) {
        child.stdin?.end();
        if (process.platform !== 'win32') {
          child.kill(interruptSignal());
        }
        interrupt();
      }
    };
    const lines = (ls: string[]) => {
      for (const l of ls) {
        const error = errorLine(l);
        if (error !== undefined) {
          reported = error;
          continue;
        }
        try {
          onLine(l);
        } catch (e) {
          this.warn(quiet, `${cmd}: a line's handler failed: ${e instanceof Error ? e.message : String(e)}`);
        }
      }
    };
    child.stdout?.setEncoding('utf8');
    child.stdout?.on('data', (chunk: string) => {
      if (failure !== undefined) {
        return;
      }
      try {
        lines(splitter.push(chunk));
      } catch (e) {
        failure = e instanceof EyedbgError ? e : new EyedbgError('INTERNAL', String(e));
        child.kill('SIGKILL');
      }
    });
    child.stderr?.setEncoding('utf8');
    child.stderr?.on('data', (chunk: string) => {
      errText = (errText + chunk).slice(-stderrTail);
    });
    const done = new Promise<StreamEnd>((resolve, reject) => {
      let settled = false;
      const fail = (e: EyedbgError) => {
        this.warn(quiet, `${cmd}: ${e.code}: ${e.message}${e.hint !== '' ? ` — ${e.hint}` : ''}`);
        reject(e);
      };
      child.on('error', (err: NodeJS.ErrnoException) => {
        if (settled || child.pid !== undefined) {
          // Not a spawn failure (a kill that failed): 'close' still comes.
          this.log.warn(`${cmd}: ${err.message}`);
          return;
        }
        settled = true;
        fail(this.spawnError(err as ExecFileException, argv, quiet));
      });
      child.once('close', (code: number | null) => {
        if (settled) {
          return;
        }
        settled = true;
        if (failure === undefined) {
          lines(splitter.end());
        }
        if (failure !== undefined) {
          fail(failure);
        } else if (stopped || code === 0) {
          resolve({ code, stopped });
        } else if (reported !== undefined) {
          fail(reported);
        } else {
          fail(
            new EyedbgError(
              'INTERNAL',
              `${cmd} exited with ${code === null ? `signal ${child.signalCode ?? '?'}` : `code ${code}`}: ${quote(errText)}`,
              'run the same eyedbg command in a terminal to see why',
            ),
          );
        }
      });
    });
    opts.signal?.addEventListener('abort', stop, { once: true });
    if (opts.signal?.aborted === true) {
      stop();
    }
    void done.then(
      () => opts.signal?.removeEventListener('abort', stop),
      () => opts.signal?.removeEventListener('abort', stop),
    );
    return {
      done,
      stop: () => {
        stop();
        return done.then(
          (end) => end,
          () => ({ code: child.exitCode, stopped: true }),
        );
      },
    };
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
    this.warn(opts, `eyedbg ${cmd}: ${e.code}: ${e.message}`);
    return e;
  }
}

/** errorLine reads a streamed line that is eyedbg's JSON error ({"error": {code, message, hint}}). */
function errorLine(line: string): EyedbgError | undefined {
  if (!line.startsWith('{') || !line.includes('"error"')) {
    return undefined;
  }
  try {
    return errorOf(JSON.parse(line));
  } catch {
    return undefined;
  }
}
