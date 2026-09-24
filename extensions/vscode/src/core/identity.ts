// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Who the extension acts as in a session: human:NAME.

const namePattern = /^[A-Za-z0-9._@+-]{1,64}$/;

/** defaultName turns an OS user name into a client name: other characters become "-", cut to 64. */
export function defaultName(user: string): string {
  const name = Array.from(user)
    .map((c) => (/^[A-Za-z0-9._@+-]$/.test(c) ? c : '-'))
    .join('')
    .slice(0, 64);
  return name === '' ? 'vscode' : name;
}

export type ClientResult = { ok: true; client: string } | { ok: false; error: string };

/**
 * clientId is the client the extension acts as: human:<eyedbg.clientName>,
 * else human:<the OS user, made valid>. users are the OS user name
 * candidates in order (os.userInfo, USER, USERNAME); the first non-empty
 * one is used.
 */
export function clientId(configured: string, users: readonly (string | undefined)[]): ClientResult {
  if (configured !== '') {
    if (!namePattern.test(configured)) {
      return {
        ok: false,
        error:
          'eyedbg.clientName must be 1 to 64 of letters, digits and . _ @ + - (you join as human:NAME); ' +
          'fix it in your user settings',
      };
    }
    return { ok: true, client: `human:${configured}` };
  }
  const user = users.find((u) => u !== undefined && u !== '') ?? '';
  return { ok: true, client: `human:${defaultName(user)}` };
}
