// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The extension's exported API: read-only snapshots, for tests and other
// extensions. Unstable (apiVersion 0); nothing in it changes a session —
// commands do that, and any extension can already send eyedbg/* requests.

import type * as vscode from 'vscode';
import type { Annotation, Notice, SessionSnapshot, State } from './state';

export interface EyedbgApi {
  readonly apiVersion: 0;
  /** sessions are the eyedbg debug sessions of this window. */
  sessions(): SessionSnapshot[];
  /** annotations are the other clients' breakpoints drawn in visible editors. */
  annotations(): Annotation[];
  /** notices are the last 50 notifications the extension showed. */
  notices(): Notice[];
  /** stops are the sessions this window launched and stopped when their debug session ended. */
  stops(): { session: string; error: string }[];
  readonly onDidChange: vscode.Event<void>;
}

export function api(state: State): EyedbgApi {
  return {
    apiVersion: 0,
    sessions: () => state.snapshots(),
    annotations: () => state.annotations.map((a) => ({ ...a })),
    notices: () => state.notices.map((n) => ({ ...n })),
    stops: () => state.stops.map((s) => ({ ...s })),
    onDidChange: state.onDidChange,
  };
}
