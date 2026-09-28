// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The extension's side of the eyedbg/runInTerminal exchange (docs/adr/0019
// D7–D9): the facade asks the editor whose launch asked for
// "console": "integratedTerminal" to run a program in a terminal, and the
// extension runs args[0] itself as the terminal's process — no shell. The
// event is checked again here against the facade's validation table
// (internal/facade/terminal.go), parsed as JSON.parse left it; a request
// that breaks a rule is refused, never cut or rewritten. The shared test
// vectors are internal/facade/testdata/terminal_requests.json.

import type { DapMessage } from './protocol';
import { isRecord } from './protocol';
import { noIcons, plainText } from './render';

/** The limits of a terminal request (the validation table). */
const maxArgs = 1000;
const maxEnv = 1000;
const maxBytes = 256 << 10;
const maxTitle = 200;

const envName = /^[A-Za-z_][A-Za-z0-9_]{0,127}$/;
/**
 * C0, DEL, C1, the line and paragraph separators — and, with the u flag,
 * an unpaired surrogate (JSON.parse lets one through; the facade never
 * sends one, as Go's JSON encoder emits valid UTF-8 only).
 */
// biome-ignore lint/suspicious/noControlCharactersInRegex: matching control characters is the point.
const forbidden = /[\u0000-\u001f\u007f-\u009f\u2028\u2029]|[\uD800-\uDFFF]/u;

/** A checked terminal request: run args[0] with args[1..] in cwd, env added (null unsets). */
export interface TerminalRequest {
  id: number;
  title: string;
  cwd: string;
  args: string[];
  /** A prototype-less object: a "__proto__" variable stays a variable. */
  env: Record<string, string | null>;
}

export type TerminalCheck = { ok: true; value: TerminalRequest } | { ok: false; rule: number; error: string };

function refuse(rule: number, error: string): TerminalCheck {
  return { ok: false, rule, error: `rule ${rule}: ${error}` };
}

/** terminalId is an event body's id if it is a positive integer, else undefined. */
export function terminalId(body: unknown): number | undefined {
  const id = isRecord(body) ? body.id : undefined;
  return typeof id === 'number' && Number.isSafeInteger(id) && id >= 1 ? id : undefined;
}

/** isSep reports whether c is a path separator on any platform. */
function isSep(c: string | undefined): boolean {
  return c === '/' || c === '\\';
}

/**
 * localAbsolute reports whether p is a local absolute path on platform:
 * never two leading separators (UNC, \\?\, \\.\), on Windows a drive root
 * (C:\ or C:/), elsewhere a leading /.
 */
export function localAbsolute(p: string, platform: string): boolean {
  if (isSep(p[0]) && isSep(p[1])) {
    return false;
  }
  if (platform !== 'win32') {
    return p.startsWith('/');
  }
  return /^[A-Za-z]:[\\/]/.test(p);
}

/**
 * checkTerminal checks an eyedbg/runInTerminal event's body for platform
 * (process.platform) and returns the request, or the first rule it breaks
 * (0: not the shape the facade sends). Rules 1 and 2 (kind, shell) are
 * checked when the body carries them: the facade's event doesn't, as it
 * refused them already.
 */
export function checkTerminal(body: unknown, platform: string): TerminalCheck {
  if (!isRecord(body)) {
    return refuse(0, 'the request is not an object');
  }
  const id = terminalId(body);
  const { title = '', cwd = '', args = [], env = {}, kind, argsCanBeInterpretedByShell: shell } = body;
  if (
    id === undefined ||
    typeof title !== 'string' ||
    typeof cwd !== 'string' ||
    !Array.isArray(args) ||
    !args.every((a) => typeof a === 'string') ||
    !isRecord(env) ||
    (kind !== undefined && typeof kind !== 'string') ||
    (shell !== undefined && typeof shell !== 'boolean')
  ) {
    return refuse(0, 'the request is malformed');
  }
  const argv = args as string[];
  const vars = Object.entries(env);

  // Rules 1 and 2: the integrated terminal, no shell.
  if (kind !== undefined && kind !== '' && kind !== 'integrated') {
    return refuse(1, 'only the integrated terminal');
  }
  if (shell === true) {
    return refuse(2, 'its arguments would go through a shell');
  }
  // Rule 3: 1 to 1000 arguments.
  if (argv.length < 1 || argv.length > maxArgs) {
    return refuse(3, 'it needs 1 to 1000 arguments');
  }
  // Rules 4 and 5: no control characters, at most 256 KiB in all.
  const strs = [title, cwd, ...argv];
  for (const [k, v] of vars) {
    strs.push(k);
    if (typeof v === 'string') {
      strs.push(v);
    }
  }
  let size = 0;
  for (const s of strs) {
    if (forbidden.test(s)) {
      return refuse(4, 'it holds a control character');
    }
    size += Buffer.byteLength(s, 'utf8');
  }
  if (size > maxBytes) {
    return refuse(5, 'it is larger than 256 KiB');
  }
  // Rule 6: local absolute program and working directory.
  const program = argv[0] ?? '';
  if (!localAbsolute(program, platform) || !localAbsolute(cwd, platform)) {
    return refuse(6, 'its program and working directory must be local absolute paths');
  }
  // Rule 7: on Windows an .exe, and no quote-enclosed argument with a
  // space (node-pty would pass it unquoted and the program would split it).
  if (platform === 'win32') {
    if (!/\.exe$/i.test(program)) {
      return refuse(7, 'on Windows its program must be an .exe');
    }
    if (argv.some((a) => a.length > 1 && a.startsWith('"') && a.endsWith('"') && a.includes(' '))) {
      return refuse(7, 'on Windows no argument may start and end with " while holding a space');
    }
  }
  // Rule 8: at most 1000 variables, plain names, string or null values.
  if (vars.length > maxEnv) {
    return refuse(8, 'it sets more than 1000 environment variables');
  }
  const out: Record<string, string | null> = Object.create(null);
  for (const [k, v] of vars) {
    if (!envName.test(k)) {
      return refuse(8, "an environment variable's name isn't a plain name");
    }
    if (typeof v !== 'string' && v !== null) {
      return refuse(8, "an environment variable's value isn't a string or null");
    }
    Object.defineProperty(out, k, { value: v, enumerable: true, writable: true, configurable: true });
  }
  // Rule 9: a title of at most 200 characters.
  if (Array.from(title).length > maxTitle) {
    return refuse(9, 'its title is longer than 200 characters');
  }
  return { ok: true, value: { id, title, cwd, args: [...argv], env: out } };
}

/** terminalName is the terminal's name: the title (else "eyedbg") as plain text, no icons. */
export function terminalName(title: string): string {
  return noIcons(plainText(title === '' ? 'eyedbg' : title, maxTitle));
}

/**
 * TerminalGate is one adapter connection's acceptance of eyedbg/runInTerminal
 * events (defense in depth; the facade routes them only so): only for a
 * launch configuration with "console": "integratedTerminal", only while its
 * launch request is sent and not answered, only the first event.
 */
export class TerminalGate {
  private launchSeq: number | undefined;
  private answered = false;
  private seen = false;

  /** wanted: the debug session's configuration is a launch with console integratedTerminal. */
  constructor(private readonly wanted: boolean) {}

  /** fromEditor notes the launch request (VS Code → the facade). */
  fromEditor(m: DapMessage): void {
    if (m.type === 'request' && m.command === 'launch' && this.launchSeq === undefined) {
      this.launchSeq = m.seq;
    }
  }

  /** fromAdapter notes the launch's response (the facade → VS Code). */
  fromAdapter(m: DapMessage): void {
    if (m.type === 'response' && m.command === 'launch' && m.requestSeq === this.launchSeq) {
      this.answered = true;
    }
  }

  /** admit is called once per eyedbg/runInTerminal event: '' to run it, else why not (no values). */
  admit(): string {
    const first = !this.seen;
    this.seen = true;
    if (!this.wanted) {
      return 'this debug session did not ask for a terminal';
    }
    if (this.launchSeq === undefined || this.answered) {
      return 'no launch of this connection is starting';
    }
    if (!first) {
      return 'a terminal was asked for already';
    }
    return '';
  }
}
