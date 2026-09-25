// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The auto-join prompt's decisions: which running sessions to offer (an
// agent's, whose program lies under a workspace folder, not yet used from
// here), and when to look for them ('eyedbg sessions', which never starts
// the daemon).

import path from 'node:path';
import { isAbsolute } from './binary';
import { isLive } from './cli';
import { clientKind, type SessionInfo } from './protocol';

export type AutoJoinSetting = 'ask' | 'never';

export const pollDelayMs = 5_000;
export const failureDelayMs = 60_000;

export interface CandidateEnv {
  /** The workspace folders' paths (file: folders only). */
  folders: readonly string[];
  /** This window's client id. */
  self: string;
  /** Sessions not to offer again (ignored, joined or launched here). */
  skip: ReadonlySet<string>;
  platform: string;
  /** realpath resolves a path's links; on error it returns the path as given. */
  realpath: (p: string) => string;
}

/** under reports whether file lies strictly inside folder (both absolute, links resolved). */
export function under(file: string, folder: string, platform: string): boolean {
  const p = platform === 'win32' ? path.win32 : path.posix;
  const [f, d] = platform === 'win32' ? [file.toLowerCase(), folder.toLowerCase()] : [file, folder];
  const rel = p.relative(d, f);
  return rel !== '' && !rel.startsWith('..') && !p.isAbsolute(rel);
}

/** candidates are the sessions to offer, in the order listed. */
export function candidates(sessions: readonly SessionInfo[], env: CandidateEnv): SessionInfo[] {
  const folders = env.folders.filter((f) => isAbsolute(f, env.platform)).map((f) => env.realpath(f));
  return sessions.filter((s) => {
    if (!isLive(s) || env.skip.has(s.id) || !isAbsolute(s.program, env.platform)) {
      return false;
    }
    if (!s.clients.some((c) => clientKind(c.id) === 'agent') || s.clients.some((c) => c.id === env.self)) {
      return false;
    }
    const program = env.realpath(s.program);
    return folders.some((f) => under(program, f, env.platform));
  });
}

/** agentOf is the session's first agent client ('' if none). */
export function agentOf(s: SessionInfo): string {
  return s.clients.find((c) => clientKind(c.id) === 'agent')?.id ?? '';
}

export interface PlanInput {
  setting: string;
  /** The window has focus. */
  focused: boolean;
  /** How many file: workspace folders the window has. */
  folders: number;
  /** The window has an eyedbg debug session. */
  joined: boolean;
  /** The window is starting a session ('eyedbg start'). */
  launching: boolean;
  /** The last poll failed. */
  failing: boolean;
}

export type Plan = { poll: false } | { poll: true; delayMs: number };

/** plan says whether to look for sessions, and how long after the previous look finished. */
export function plan(i: PlanInput): Plan {
  if (i.setting !== 'ask' || !i.focused || i.folders === 0 || i.joined || i.launching) {
    return { poll: false };
  }
  return { poll: true, delayMs: i.failing ? failureDelayMs : pollDelayMs };
}
