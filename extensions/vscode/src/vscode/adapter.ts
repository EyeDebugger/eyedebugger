// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The debug adapter of an eyedbg debug session, as the configured human
// client: 'eyedbg dap' for the resolved session (attach), or 'eyedbg dap
// --launch', whose DAP launch starts the session (docs/adr/0019). No
// configuration value reaches the binary or its flags; a launch's only
// influence is the directory it runs in.

import * as vscode from 'vscode';
import { dapArgs, dapLaunchArgs } from '../core/cli';
import { checkSessionId } from '../core/validate';
import { client, launchSpec } from './config';
import type { Eyedbg } from './exec';

export class AdapterFactory implements vscode.DebugAdapterDescriptorFactory {
  constructor(private readonly eyedbg: Eyedbg) {}

  async createDebugAdapterDescriptor(session: vscode.DebugSession): Promise<vscode.DebugAdapterDescriptor> {
    if (session.configuration.request === 'launch') {
      const { cwd } = launchSpec(session.configuration, session.workspaceFolder?.uri.fsPath);
      return new vscode.DebugAdapterExecutable(
        await this.eyedbg.binary(),
        dapLaunchArgs(client()),
        cwd !== undefined ? { cwd } : {},
      );
    }
    const id: unknown = session.configuration.session;
    const bad = checkSessionId(id);
    if (bad !== '') {
      throw new Error(`EyeDebugger: ${bad}`);
    }
    return new vscode.DebugAdapterExecutable(await this.eyedbg.binary(), dapArgs(id as string, client()));
  }
}
