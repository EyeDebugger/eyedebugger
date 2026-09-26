// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// EyeDebugger for VS Code (docs/adr/0015, 0019): join an eyedbg session —
// or start one — as a human client, through 'eyedbg dap'.

import * as vscode from 'vscode';
import { errorNotice } from './core/render';
import { checkSessionId } from './core/validate';
import { ActivityUi } from './vscode/activity';
import { AdapterFactory } from './vscode/adapter';
import { api, type EyedbgApi } from './vscode/api';
import { AutoJoin } from './vscode/autojoin';
import { BreakpointsUi } from './vscode/breakpoints';
import { ClientsUi } from './vscode/clients';
import { ConfigurationProvider, DynamicProvider, debugType } from './vscode/config';
import { DotnetUi } from './vscode/dotnet/views';
import { Eyedbg } from './vscode/exec';
import { Follow } from './vscode/follow';
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
  const clients = new ClientsUi(state);
  const follow = new Follow(state);
  const dotnet = new DotnetUi(state, eyedbg);

  const tracker = new TrackerFactory(state, {
    lease: (t, l) => lease.lease(t, l),
    leaseHeld: (t, m) => lease.leaseHeld(t, m),
    breakpointsChanged: () => breakpoints.refresh(),
    activity: (t, e) => activity.add(t, e),
    activityChanged: () => state.fire(),
    reveal: (t, at) => follow.reveal(t, at),
  });

  /** join joins session (validated), or picks one; a session this window already has is left as it is. */
  const join = async (session?: unknown): Promise<boolean> => {
    const folder = vscode.workspace.workspaceFolders?.[0];
    if (session === undefined) {
      return vscode.debug.startDebugging(folder, { type: debugType, request: 'attach', name: 'Join eyedbg session' });
    }
    const bad = checkSessionId(session);
    if (bad !== '') {
      void state.show('error', errorNotice('INVALID_REQUEST', bad));
      return false;
    }
    if (state.all().some((t) => t.eyedbgId === session)) {
      return true;
    }
    return vscode.debug.startDebugging(folder, {
      type: debugType,
      request: 'attach',
      name: `Join ${session}`,
      session,
    });
  };

  const checkInstallation = async (): Promise<void> => {
    try {
      const b = await eyedbg.check();
      void state.show('info', `eyedbg ${b.version} at ${b.path}`);
    } catch (e) {
      state.showError(e);
    }
  };

  // A debug session's entry ends with it, unless VS Code is restarting it
  // (a new adapter connection follows). A launched session needs no stop
  // here: its connection's terminate ended it, and the facade forgets a
  // launched session that has exited when its connection ends (ADR 0019).
  const terminated = (session: vscode.DebugSession): void => {
    if (session.type !== debugType) {
      return;
    }
    if (state.restarting.delete(session.id)) {
      return;
    }
    state.end(session.id);
    breakpoints.refresh();
  };

  context.subscriptions.push(
    log,
    state,
    lease,
    breakpoints,
    activity,
    clients,
    follow,
    dotnet,
    new AutoJoin(state, eyedbg, (id) => join(id), process.env.EYEDBG_TEST_ASSUME_FOCUSED === '1'),
    vscode.debug.registerDebugConfigurationProvider(debugType, new ConfigurationProvider(eyedbg, state)),
    vscode.debug.registerDebugConfigurationProvider(
      debugType,
      new DynamicProvider(eyedbg, state),
      vscode.DebugConfigurationProviderTriggerKind.Dynamic,
    ),
    vscode.debug.registerDebugAdapterDescriptorFactory(debugType, new AdapterFactory(eyedbg)),
    vscode.debug.registerDebugAdapterTrackerFactory(debugType, tracker),
    vscode.debug.onDidTerminateDebugSession((s) => terminated(s)),
    vscode.commands.registerCommand('eyedbg.joinSession', (a?: unknown) =>
      join(typeof a === 'object' && a !== null ? (a as { session?: unknown }).session : undefined),
    ),
    vscode.commands.registerCommand('eyedbg.checkInstallation', checkInstallation),
  );

  return api(state, { activity, clients, ...dotnet.trees() }, () => dotnet.stats());
}

export function deactivate(): void {}
