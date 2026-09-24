// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The debug adapter of an eyedbg debug session: 'eyedbg dap' for the
// resolved session, as the configured human client.

import * as vscode from 'vscode';
import { dapArgs } from '../core/cli';
import { checkSessionId } from '../core/validate';
import { client } from './config';
import type { Eyedbg } from './exec';

export class AdapterFactory implements vscode.DebugAdapterDescriptorFactory {
  constructor(private readonly eyedbg: Eyedbg) {}

  async createDebugAdapterDescriptor(session: vscode.DebugSession): Promise<vscode.DebugAdapterDescriptor> {
    const id: unknown = session.configuration.session;
    const bad = checkSessionId(id);
    if (bad !== '') {
      throw new Error(`EyeDebugger: ${bad}`);
    }
    return new vscode.DebugAdapterExecutable(await this.eyedbg.binary(), dapArgs(id as string, client()));
  }
}
