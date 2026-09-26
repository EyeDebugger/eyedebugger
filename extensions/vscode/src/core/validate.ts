// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Validation of everything a debug configuration hands to eyedbg
// (docs/adr/0015, 0019): values are checked here, then bound as
// --flag=value or sent as the launch request's arguments exactly as
// validated, so no configuration can add a flag or name another binary.

import { isRecord, type LeasePolicy, leasePolicies } from './protocol';

export const sessionIdPattern = /^s-[a-z0-9]{1,32}$/;
const langPattern = /^[a-z][a-z0-9_+-]{0,31}$/;
const optKeyPattern = /^[A-Za-z][A-Za-z0-9_.-]{0,63}$/;
/** An adapter manifest's name (internal/adapters: namePattern). */
const adapterPattern = /^[a-z][a-z0-9-]{0,31}$/;

export type ExceptionMode = 'all' | 'uncaught' | 'none';
export const exceptionModes: readonly ExceptionMode[] = ['all', 'uncaught', 'none'];

/** A launch configuration's fields, validated. */
export interface LaunchSpec {
  lang: string;
  program?: string;
  project?: string;
  args: string[];
  cwd?: string;
  env: [string, string][];
  opts: [string, string][];
  stopOnEntry: boolean;
  noBuild: boolean;
  leasePolicy?: LeasePolicy;
  exceptions?: ExceptionMode;
  /** The debug adapter, for a language with a choice (dotnet: netcoredbg or sharpdbg). */
  adapter?: string;
}

export type Result<T> = { ok: true; value: T } | { ok: false; error: string };

function fail<T>(error: string): Result<T> {
  return { ok: false, error };
}

/** hasNul reports whether s holds a NUL character, which no argv can carry. */
export function hasNul(s: string): boolean {
  return s.includes('\0');
}

export function isSessionId(x: unknown): x is string {
  return typeof x === 'string' && sessionIdPattern.test(x);
}

/** checkSessionId returns an error text for an invalid session id, else ''. */
export function checkSessionId(x: unknown): string {
  if (isSessionId(x)) {
    return '';
  }
  return `"session" must be an eyedbg session id like s-7f3k (see 'eyedbg sessions'), not ${describe(x)}`;
}

function describe(x: unknown): string {
  if (typeof x === 'string') {
    return JSON.stringify(x.length > 40 ? `${x.slice(0, 40)}…` : x);
  }
  return x === undefined ? 'nothing' : typeof x;
}

function optionalString(cfg: Record<string, unknown>, key: string): Result<string | undefined> {
  const v = cfg[key];
  if (v === undefined || v === null || v === '') {
    return { ok: true, value: undefined };
  }
  if (typeof v !== 'string') {
    return fail(`"${key}" must be a string`);
  }
  if (hasNul(v)) {
    return fail(`"${key}" must not contain a NUL character`);
  }
  return { ok: true, value: v };
}

function optionalBool(cfg: Record<string, unknown>, key: string): Result<boolean> {
  const v = cfg[key];
  if (v === undefined || v === null) {
    return { ok: true, value: false };
  }
  if (typeof v !== 'boolean') {
    return fail(`"${key}" must be true or false`);
  }
  return { ok: true, value: v };
}

function stringMap(cfg: Record<string, unknown>, key: string, keyOk: (k: string) => boolean, what: string) {
  const v = cfg[key];
  const out: [string, string][] = [];
  if (v === undefined || v === null) {
    return { ok: true, value: out } as Result<[string, string][]>;
  }
  if (!isRecord(v)) {
    return fail<[string, string][]>(`"${key}" must be an object of strings`);
  }
  for (const [k, val] of Object.entries(v)) {
    if (!keyOk(k)) {
      return fail<[string, string][]>(`"${key}" has an invalid name ${describe(k)}: ${what}`);
    }
    if (typeof val !== 'string' || hasNul(val)) {
      return fail<[string, string][]>(`"${key}.${k}" must be a string without NUL characters`);
    }
    out.push([k, val]);
  }
  return { ok: true, value: out } as Result<[string, string][]>;
}

/** validateLaunch checks a launch configuration (after variable substitution). */
export function validateLaunch(cfg: Record<string, unknown>): Result<LaunchSpec> {
  const lang = cfg.lang;
  if (typeof lang !== 'string' || !langPattern.test(lang)) {
    return fail(
      `"lang" must name an eyedbg language such as python or dotnet (see 'eyedbg adapters ls'), not ${describe(lang)}`,
    );
  }

  const spec: LaunchSpec = { lang, args: [], env: [], opts: [], stopOnEntry: false, noBuild: false };

  for (const key of ['program', 'project', 'cwd'] as const) {
    const r = optionalString(cfg, key);
    if (!r.ok) {
      return r;
    }
    if (r.value !== undefined) {
      spec[key] = r.value;
    }
  }

  const args = cfg.args;
  if (args !== undefined && args !== null) {
    if (!Array.isArray(args) || args.some((a) => typeof a !== 'string' || hasNul(a))) {
      return fail('"args" must be an array of strings without NUL characters');
    }
    spec.args = args as string[];
  }

  const env = stringMap(
    cfg,
    'env',
    (k) => k !== '' && !k.includes('=') && !hasNul(k),
    'names are non-empty without "="',
  );
  if (!env.ok) {
    return env;
  }
  spec.env = env.value;

  const opts = stringMap(cfg, 'opts', (k) => optKeyPattern.test(k), 'a letter, then letters, digits, "_", "." or "-"');
  if (!opts.ok) {
    return opts;
  }
  spec.opts = opts.value;

  for (const key of ['stopOnEntry', 'noBuild'] as const) {
    const r = optionalBool(cfg, key);
    if (!r.ok) {
      return r;
    }
    spec[key] = r.value;
  }

  const policy = cfg.leasePolicy;
  if (policy !== undefined && policy !== null) {
    const p = leasePolicies.find((x) => x === policy);
    if (p === undefined) {
      return fail(`"leasePolicy" must be free, handoff or human-priority, not ${describe(policy)}`);
    }
    spec.leasePolicy = p;
  }

  const exceptions = cfg.exceptions;
  if (exceptions !== undefined && exceptions !== null) {
    const e = exceptionModes.find((x) => x === exceptions);
    if (e === undefined) {
      return fail(`"exceptions" must be all, uncaught or none, not ${describe(exceptions)}`);
    }
    spec.exceptions = e;
  }

  const adapter = cfg.adapter;
  if (adapter !== undefined && adapter !== null && adapter !== '') {
    if (typeof adapter !== 'string' || !adapterPattern.test(adapter)) {
      return fail(
        `"adapter" must name a debug adapter such as netcoredbg or sharpdbg (see 'eyedbg adapters ls'), not ${describe(adapter)}`,
      );
    }
    spec.adapter = adapter;
  }

  if (spec.noBuild && spec.program === undefined) {
    return fail('"noBuild" needs "program"');
  }

  return { ok: true, value: spec };
}

/** The launch arguments eyedbg reads (internal/facade: launchKey); names are case-sensitive. */
export const launchKeys = [
  'lang',
  'program',
  'project',
  'cwd',
  'args',
  'env',
  'opts',
  'stopOnEntry',
  'noBuild',
  'leasePolicy',
  'exceptions',
  'adapter',
] as const;

/**
 * launchConfiguration is cfg as the launch request carries it to eyedbg:
 * every eyedbg key written back exactly as spec validated it (an absent
 * one deleted), and every key that differs from one only in case deleted
 * (eyedbg refuses those: its JSON decoding would match them). Other keys
 * (VS Code's own) are kept. cfg itself isn't changed.
 */
export function launchConfiguration(cfg: Record<string, unknown>, spec: LaunchSpec): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(cfg)) {
    const lower = k.toLowerCase();
    if (!launchKeys.some((known) => known.toLowerCase() === lower)) {
      // Defined, not assigned: a "__proto__" key stays a plain key.
      Object.defineProperty(out, k, { value: v, enumerable: true, writable: true, configurable: true });
    }
  }
  const put = (k: (typeof launchKeys)[number], v: unknown) => {
    if (v !== undefined) {
      out[k] = v;
    }
  };
  put('lang', spec.lang);
  put('program', spec.program);
  put('project', spec.project);
  put('cwd', spec.cwd);
  put('args', [...spec.args]);
  put('env', Object.fromEntries(spec.env));
  put('opts', Object.fromEntries(spec.opts));
  put('stopOnEntry', spec.stopOnEntry);
  put('noBuild', spec.noBuild);
  put('leasePolicy', spec.leasePolicy);
  put('exceptions', spec.exceptions);
  put('adapter', spec.adapter);
  return out;
}
