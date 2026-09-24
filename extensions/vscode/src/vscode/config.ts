// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The eyedbg debug type's configurations (docs/adr/0015): attach joins a
// running session (picked when not named); launch runs 'eyedbg start' and
// becomes an attach to the new session; dynamic configurations list the
// running sessions.

import { randomUUID } from 'node:crypto';
import os from 'node:os';
import * as vscode from 'vscode';
import { isLive, launchCwd, parseSessions, parseStarted, sessionsArgs, startArgs } from '../core/cli';
import { clientId } from '../core/identity';
import { EyedbgError, type SessionInfo } from '../core/protocol';
import { joinConfigName, sessionPickItem } from '../core/render';
import { checkSessionId, validateLaunch } from '../core/validate';
import { type Eyedbg, isDirectory, timeouts } from './exec';
import type { State } from './state';

export const debugType = 'eyedbg';

/** launchToken is the resolved configuration's key into State.launched. */
export const launchToken = '__eyedbgLaunch';

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
    delete config[launchToken];
    try {
      if (config.request === 'launch') {
        return await this.launch(folder, config, token);
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

  private async launch(
    folder: vscode.WorkspaceFolder | undefined,
    config: vscode.DebugConfiguration,
    token: vscode.CancellationToken | undefined,
  ): Promise<vscode.DebugConfiguration | undefined> {
    const v = validateLaunch(config);
    if (!v.ok) {
      throw new EyedbgError('INVALID_REQUEST', v.error);
    }
    const resolved = launchCwd(v.value, folder?.uri.fsPath, process.platform);
    if (!resolved.ok) {
      throw new EyedbgError('INVALID_REQUEST', resolved.error);
    }
    const { spec, cwd } = resolved.value;
    if (spec.cwd !== undefined && !isDirectory(spec.cwd)) {
      throw new EyedbgError('INVALID_REQUEST', `"cwd" ${JSON.stringify(spec.cwd)} is not a directory`);
    }
    const argv = startArgs(spec, client());
    const started = await vscode.window.withProgress(
      {
        location: vscode.ProgressLocation.Notification,
        title: `EyeDebugger: starting a ${spec.lang} session`,
        cancellable: true,
      },
      async (_progress, cancel) => {
        const abort = new AbortController();
        const subs = [cancel.onCancellationRequested(() => abort.abort())];
        if (token !== undefined) {
          subs.push(token.onCancellationRequested(() => abort.abort()));
        }
        try {
          return parseStarted(
            await this.eyedbg.run(argv, {
              timeoutMs: timeouts.start,
              signal: abort.signal,
              ...(cwd !== undefined ? { cwd } : {}),
            }),
          );
        } catch (e) {
          if (e instanceof EyedbgError && e.code === 'CANCELED') {
            void this.state.show(
              'warning',
              "Canceled starting the eyedbg session; if it had started already, it still runs (see 'eyedbg sessions').",
            );
          }
          throw e;
        } finally {
          for (const s of subs) {
            s.dispose();
          }
        }
      },
    );
    const key = randomUUID();
    this.state.launched.set(key, started.id);
    this.state.log.info(`started session ${started.id} (${spec.lang})`);
    return { ...config, request: 'attach', session: started.id, [launchToken]: key };
  }
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
