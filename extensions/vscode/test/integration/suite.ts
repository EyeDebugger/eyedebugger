// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The integration suite, loaded by VS Code's extension host
// (--extensionTestsPath): run() rejects if any test failed.
// EYEDBG_TEST_FILTER (a regular expression) runs only matching tests.

import { runAll } from './harness';
import { rec } from './helpers';
import './join.test';
import './mirrors.test';
import './restart.test';
import './stale.test';
import './alias.test';
import './lease.test';
import './launch.test';
import './pick.test';

export async function run(): Promise<void> {
  rec();
  const filter = process.env.EYEDBG_TEST_FILTER;
  await runAll(filter !== undefined && filter !== '' ? new RegExp(filter) : undefined);
}
