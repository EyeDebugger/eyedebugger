// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The eyedbg debug type's configurations (docs/adr/0015, 0019): attach
// joins a running session (picked when not named); launch stays a launch,
// validated here, which 'eyedbg dap --launch' turns into a new session;
// dynamic configurations list the running sessions.

import os from 'node:os';
import * as vscode from 'vscode';
import { adapterUnsupported, isLive, launchCwd, launchUnsupported, parseSessions, sessionsArgs } from '../core/cli';
import { clientId } from '../core/identity';
import { EyedbgError, type SessionInfo } from '../core/protocol';
import { joinConfigName, sessionPickItem } from '../core/render';
import { checkSessionId, type LaunchSpec, launchConfiguration, validateLaunch } from '../core/validate';
import { type Eyedbg, isDirectory, timeouts } from './exec';
import type { State } from './state';

export const debugType = 'eyedbg';

function osUser(): string | undefined {
  try {
    return os.userInfo().username;
  } catch {
    return undefined;
  }
}

/** client is who the extension acts as: human:NAME from the machine-scoped eyedbg.clientName, else the OS user. */
export function client(): string {
  const configured = vscode.workspace.getConfiguration('eyedbg').get<string>('clientName', '');
  const r = clientId(typeof configured === 'string' ? configured.trim() : '', [
    osUser(),
    process.env.USER,
    process.env.USERNAME,
  ]);
  if (!r.ok) {
    throw new EyedbgError('INVALID_REQUEST', r.error);
  }
  return r.client;
}

export async function listSessions(eyedbg: Eyedbg): Promise<SessionInfo[]> {
  return parseSessions(await eyedbg.run(sessionsArgs(), { timeoutMs: timeouts.short }));
}

export class ConfigurationProvider implements vscode.DebugConfigurationProvider {
  constructor(
    private readonly eyedbg: Eyedbg,
    private readonly state: State,
  ) {}

  /** F5 without a launch.json: join a running session. */
  resolveDebugConfiguration(
    _folder: vscode.WorkspaceFolder | undefined,
    config: vscode.DebugConfiguration,
  ): vscode.DebugConfiguration {
    if (config.type === undefined && config.request === undefined && config.name === undefined) {
      return { type: debugType, request: 'attach', name: 'Join eyedbg session' };
    }
    return config;
  }

  async resolveDebugConfigurationWithSubstitutedVariables(
    folder: vscode.WorkspaceFolder | undefined,
    config: vscode.DebugConfiguration,
    token?: vscode.CancellationToken,
  ): Promise<vscode.DebugConfiguration | undefined> {
    // A debugServer would bypass the descriptor factory (and so the
    // eyedbg binary check): never honoured for this type.
    delete config.debugServer;
    try {
      if (config.request === 'launch') {
        return await this.launch(folder, config);
      }
      if (config.request === 'attach') {
        return await this.attach(config, token);
      }
      throw new EyedbgError(
        'INVALID_REQUEST',
        `"request" must be attach or launch, not ${JSON.stringify(config.request)}`,
      );
    } catch (e) {
      this.state.showError(e);
      return undefined;
    }
  }

  private async attach(
    config: vscode.DebugConfiguration,
    token: vscode.CancellationToken | undefined,
  ): Promise<vscode.DebugConfiguration | undefined> {
    if (config.session !== undefined && config.session !== '') {
      const bad = checkSessionId(config.session);
      if (bad !== '') {
        throw new EyedbgError('INVALID_REQUEST', bad);
      }
      const s = (await listSessions(this.eyedbg)).find((x) => x.id === config.session);
      if (s === undefined || !isLive(s)) {
        throw new EyedbgError('NO_SESSION', `eyedbg session ${config.session} isn't running`, "see 'eyedbg sessions'");
      }
      return config;
    }
    const live = (await listSessions(this.eyedbg)).filter(isLive);
    if (live.length === 0) {
      throw new EyedbgError(
        'NO_SESSION',
        "No running eyedbg session to join: start one with 'eyedbg start' or an EyeDebugger launch configuration",
      );
    }
    let chosen: SessionInfo | undefined = live[0];
    if (live.length > 1) {
      const items = live.map((s) => ({ ...sessionPickItem(s), id: s.id }));
      const pick = await vscode.window.showQuickPick(
        items,
        { title: 'Join an eyedbg session', matchOnDetail: true },
        token,
      );
      chosen = live.find((s) => s.id === pick?.id);
    }
    if (chosen === undefined) {
      return undefined;
    }
    return { ...config, session: chosen.id };
  }

  /**
   * launch validates a launch configuration and hands it on as the launch
   * request's arguments, exactly as validated; the descriptor factory runs
   * 'eyedbg dap --launch' for it. Nothing is started here.
   */
  private async launch(
    folder: vscode.WorkspaceFolder | undefined,
    config: vscode.DebugConfiguration,
  ): Promise<vscode.DebugConfiguration> {
    const { spec } = launchSpec(config, folder?.uri.fsPath);
    if (spec.cwd !== undefined && !isDirectory(spec.cwd)) {
      throw new EyedbgError('INVALID_REQUEST', `"cwd" ${JSON.stringify(spec.cwd)} is not a directory`);
    }
    const { version, features } = await this.eyedbg.check();
    const unsupported = launchUnsupported(version, features) || adapterUnsupported(spec, version, features);
    if (unsupported !== '') {
      throw new EyedbgError('VERSION_MISMATCH', unsupported, 'update eyedbg');
    }
    return { ...launchConfiguration(config, spec), request: 'launch' } as vscode.DebugConfiguration;
  }
}

/**
 * launchSpec validates a launch configuration and resolves its cwd against
 * folder: the spec (its cwd absolute) and where 'eyedbg dap --launch' runs.
 */
export function launchSpec(
  config: Record<string, unknown>,
  folder: string | undefined,
): { spec: LaunchSpec; cwd: string | undefined } {
  const v = validateLaunch(config);
  if (!v.ok) {
    throw new EyedbgError('INVALID_REQUEST', v.error);
  }
  const resolved = launchCwd(v.value, folder, process.platform);
  if (!resolved.ok) {
    throw new EyedbgError('INVALID_REQUEST', resolved.error);
  }
  return resolved.value;
}

/** DynamicProvider offers one configuration per running session (Run and Debug's list). */
export class DynamicProvider implements vscode.DebugConfigurationProvider {
  constructor(
    private readonly eyedbg: Eyedbg,
    private readonly state: State,
  ) {}

  async provideDebugConfigurations(): Promise<vscode.DebugConfiguration[]> {
    try {
      const live = (await listSessions(this.eyedbg)).filter(isLive);
      return live.map((s) => ({ type: debugType, request: 'attach', name: joinConfigName(s), session: s.id }));
    } catch (e) {
      this.state.log.warn(`listing sessions: ${e instanceof Error ? e.message : String(e)}`);
      return [];
    }
  }
}
