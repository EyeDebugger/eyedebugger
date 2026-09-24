// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// EyeDebugger for VS Code (docs/adr/0015): join an eyedbg session — or
// start one — as a human client, through 'eyedbg dap'.

import * as vscode from 'vscode';
import { stopArgs } from './core/cli';
import { errorNotice } from './core/render';
import { isSessionId } from './core/validate';
import { ActivityUi } from './vscode/activity';
import { AdapterFactory } from './vscode/adapter';
import { api, type EyedbgApi } from './vscode/api';
import { BreakpointsUi } from './vscode/breakpoints';
import { ConfigurationProvider, client, DynamicProvider, debugType, launchToken } from './vscode/config';
import { Eyedbg, timeouts } from './vscode/exec';
import { LeaseUi } from './vscode/lease';
import { State } from './vscode/state';
import { TrackerFactory } from './vscode/tracker';

export function activate(context: vscode.ExtensionContext): EyedbgApi {
  const log = vscode.window.createOutputChannel('EyeDebugger', { log: true });
  const state = new State(log);
  const eyedbg = new Eyedbg(log);
  const lease = new LeaseUi(state);
  const breakpoints = new BreakpointsUi(state);
  const activity = new ActivityUi(state);

  const tracker = new TrackerFactory(state, {
    lease: (t, l) => lease.lease(t, l),
    leaseHeld: (t, m) => lease.leaseHeld(t, m),
    breakpointsChanged: () => breakpoints.refresh(),
    activity: (t, e) => activity.add(t, e),
  });

  // A session this window launched is stopped when its debug session ends,
  // unless VS Code is restarting it (then it joins the same session again).
  const terminated = async (session: vscode.DebugSession): Promise<void> => {
    if (session.type !== debugType) {
      return;
    }
    const restarting = state.restarting.delete(session.id);
    if (restarting) {
      return;
    }
    state.end(session.id);
    breakpoints.refresh();
    const token: unknown = session.configuration[launchToken];
    const id: unknown = session.configuration.session;
    if (typeof token !== 'string' || !isSessionId(id) || state.launched.get(token) !== id) {
      return;
    }
    state.launched.delete(token);
    try {
      await eyedbg.run(stopArgs(id, client()), { timeoutMs: timeouts.short });
      state.stops.push({ session: id, error: '' });
      log.info(`stopped session ${id}`);
    } catch (e) {
      const text = e instanceof Error ? e.message : String(e);
      const code = typeof (e as { code?: unknown }).code === 'string' ? (e as { code: string }).code : '';
      state.stops.push({ session: id, error: code || text });
      void state.show('warning', `Couldn't stop eyedbg session ${id}: ${errorNotice(code, text)}`);
    }
    state.fire();
  };

  context.subscriptions.push(
    log,
    state,
    lease,
    breakpoints,
    activity,
    vscode.debug.registerDebugConfigurationProvider(debugType, new ConfigurationProvider(eyedbg, state)),
    vscode.debug.registerDebugConfigurationProvider(
      debugType,
      new DynamicProvider(eyedbg, state),
      vscode.DebugConfigurationProviderTriggerKind.Dynamic,
    ),
    vscode.debug.registerDebugAdapterDescriptorFactory(debugType, new AdapterFactory(eyedbg)),
    vscode.debug.registerDebugAdapterTrackerFactory(debugType, tracker),
    vscode.debug.onDidTerminateDebugSession((s) => void terminated(s)),
    vscode.commands.registerCommand('eyedbg.joinSession', () =>
      vscode.debug.startDebugging(vscode.workspace.workspaceFolders?.[0], {
        type: debugType,
        request: 'attach',
        name: 'Join eyedbg session',
      }),
    ),
  );

  return api(state);
}

export function deactivate(): void {}
