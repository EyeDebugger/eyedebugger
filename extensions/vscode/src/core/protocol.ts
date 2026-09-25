// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Types of what eyedbg sends — its CLI's --json output and the facade's
// eyedbg/* DAP messages (docs/adr/0014) — and parsers that never trust a
// shape: unknown input becomes a typed value or undefined.

export type LeasePolicy = 'free' | 'handoff' | 'human-priority';

export const leasePolicies: readonly LeasePolicy[] = ['free', 'handoff', 'human-priority'];

export interface LeaseRequest {
  client: string;
  message: string;
  at: string;
}

export interface LeaseInfo {
  policy: LeasePolicy;
  /** The client holding the lease; '' for nobody. */
  holder: string;
  requests: LeaseRequest[];
}

export interface ClientInfo {
  id: string;
  kind: string;
  connected: number;
  /** ISO times; '' when not given. */
  firstSeen: string;
  lastSeen: string;
}

export interface Breakpoint {
  id: number;
  file: string;
  line: number;
  requestedLine: number;
  verified: boolean;
  owner: string;
  message: string;
  condition: string;
  hitCondition: string;
  logMessage: string;
  function: string;
  note: string;
  temporary: boolean;
  editor: boolean;
}

export interface SessionInfo {
  id: string;
  lang: string;
  program: string;
  state: string;
  lease: LeaseInfo | undefined;
  clients: ClientInfo[];
}

/** An entry of a session's event log (api.Event). */
export interface SessionEvent {
  seq: number;
  time: string;
  kind: string;
  client: string;
  action: string;
  previous: string;
  reason: string;
  text: string;
  threadId: number;
  lease: LeaseInfo | undefined;
  breakpoint: Breakpoint | undefined;
}

/** An eyedbg error: the CLI's {"error": {...}} or a facade error response. */
export class EyedbgError extends Error {
  readonly code: string;
  readonly hint: string;

  constructor(code: string, message: string, hint = '') {
    super(message);
    this.name = 'EyedbgError';
    this.code = code;
    this.hint = hint;
  }
}

/** The subset of a DAP message the extension reads. */
export interface DapMessage {
  seq: number;
  type: 'request' | 'response' | 'event';
  command: string;
  event: string;
  requestSeq: number;
  success: boolean;
  message: string;
  body: Record<string, unknown> | undefined;
  arguments: Record<string, unknown> | undefined;
}

export function isRecord(x: unknown): x is Record<string, unknown> {
  return typeof x === 'object' && x !== null && !Array.isArray(x);
}

export function str(x: unknown): string {
  return typeof x === 'string' ? x : '';
}

export function num(x: unknown): number {
  return typeof x === 'number' && Number.isFinite(x) ? x : 0;
}

function int(x: unknown): number {
  return Number.isSafeInteger(x) ? (x as number) : 0;
}

function bool(x: unknown): boolean {
  return x === true;
}

function arr(x: unknown): unknown[] {
  return Array.isArray(x) ? x : [];
}

const clientIdPattern = /^(agent|human)(:[A-Za-z0-9._@+-]{1,64})?$/;

/** isClientId reports whether s is a client id (KIND[:NAME]). */
export function isClientId(s: string): boolean {
  return clientIdPattern.test(s);
}

/** clientKind is 'agent' or 'human' for a client id, else ''. */
export function clientKind(id: string): string {
  if (!isClientId(id)) {
    return '';
  }
  return id.startsWith('agent') ? 'agent' : 'human';
}

export function parseLeasePolicy(x: unknown): LeasePolicy | undefined {
  return leasePolicies.find((p) => p === x);
}

export function parseLease(x: unknown): LeaseInfo | undefined {
  if (!isRecord(x)) {
    return undefined;
  }
  const policy = parseLeasePolicy(x.policy);
  if (policy === undefined) {
    return undefined;
  }
  const requests: LeaseRequest[] = [];
  for (const r of arr(x.requests)) {
    if (isRecord(r) && str(r.client) !== '') {
      requests.push({ client: str(r.client), message: str(r.message), at: str(r.at) });
    }
  }
  return { policy, holder: str(x.holder), requests };
}

export function parseClients(x: unknown): ClientInfo[] {
  const out: ClientInfo[] = [];
  for (const c of arr(x)) {
    if (isRecord(c) && str(c.id) !== '') {
      out.push({
        id: str(c.id),
        kind: str(c.kind),
        connected: int(c.connected),
        firstSeen: str(c.firstSeen),
        lastSeen: str(c.lastSeen),
      });
    }
  }
  return out;
}

export function parseBreakpoint(x: unknown): Breakpoint | undefined {
  if (!isRecord(x) || int(x.id) === 0) {
    return undefined;
  }
  return {
    id: int(x.id),
    file: str(x.file),
    line: int(x.line),
    requestedLine: int(x.requestedLine),
    verified: bool(x.verified),
    owner: str(x.owner),
    message: str(x.message),
    condition: str(x.condition),
    hitCondition: str(x.hitCondition),
    logMessage: str(x.logMessage),
    function: str(x.function),
    note: str(x.note),
    temporary: bool(x.temporary),
    editor: bool(x.editor),
  };
}

export function parseBreakpoints(x: unknown): Breakpoint[] {
  const out: Breakpoint[] = [];
  for (const b of arr(x)) {
    const bp = parseBreakpoint(b);
    if (bp !== undefined) {
      out.push(bp);
    }
  }
  return out;
}

export function parseSessionInfo(x: unknown): SessionInfo | undefined {
  if (!isRecord(x) || str(x.id) === '') {
    return undefined;
  }
  return {
    id: str(x.id),
    lang: str(x.lang),
    program: str(x.program),
    state: str(x.state),
    lease: parseLease(x.lease),
    clients: parseClients(x.clients),
  };
}

export function parseEvent(x: unknown): SessionEvent | undefined {
  if (!isRecord(x) || str(x.kind) === '') {
    return undefined;
  }
  return {
    seq: int(x.seq),
    time: str(x.time),
    kind: str(x.kind),
    client: str(x.client),
    action: str(x.action),
    previous: str(x.previous),
    reason: str(x.reason),
    text: str(x.text),
    threadId: int(x.threadId),
    lease: parseLease(x.lease),
    breakpoint: parseBreakpoint(x.breakpoint),
  };
}

/** parseDap reads a DAP message; undefined if it isn't one. */
export function parseDap(x: unknown): DapMessage | undefined {
  if (!isRecord(x)) {
    return undefined;
  }
  const type = x.type;
  if (type !== 'request' && type !== 'response' && type !== 'event') {
    return undefined;
  }
  return {
    seq: int(x.seq),
    type,
    command: str(x.command),
    event: str(x.event),
    requestSeq: int(x.request_seq),
    success: bool(x.success),
    message: str(x.message),
    body: isRecord(x.body) ? x.body : undefined,
    arguments: isRecord(x.arguments) ? x.arguments : undefined,
  };
}

/** errorCode is a failed DAP response's eyedbg error code (body.error.variables.code, else its message). */
export function errorCode(m: DapMessage): string {
  const e = m.body?.error;
  if (isRecord(e) && isRecord(e.variables) && typeof e.variables.code === 'string') {
    return e.variables.code;
  }
  return /^[A-Z_]+$/.test(m.message) ? m.message : '';
}

/** errorText is a failed DAP response's text: body.error.format, else its message. */
export function errorText(m: DapMessage): string {
  const e = m.body?.error;
  if (isRecord(e) && typeof e.format === 'string' && e.format !== '') {
    return e.format;
  }
  return m.message;
}

/** errorHolder is a LEASE_HELD response's lease holder, if it names one. */
export function errorHolder(m: DapMessage): string {
  const e = m.body?.error;
  if (isRecord(e) && isRecord(e.variables) && typeof e.variables.holder === 'string') {
    return e.variables.holder;
  }
  return '';
}

/** A DAP stopped event's body, as far as the extension reads it. */
export interface Stop {
  threadId: number;
  reason: string;
  /** preserveFocusHint: another client caused the stop (docs/adr/0014). */
  hint: boolean;
}

/** parseStop reads a stopped event's body. */
export function parseStop(body: Record<string, unknown> | undefined): Stop {
  const b = body ?? {};
  return { threadId: int(b.threadId), reason: str(b.reason), hint: bool(b.preserveFocusHint) };
}

/** A source location: an absolute path and a 1-based line. */
export interface SourceLine {
  path: string;
  line: number;
}

/** parseStackTop reads the top frame of a stackTrace response's body: undefined unless it has a source path and a line. */
export function parseStackTop(body: Record<string, unknown> | undefined): SourceLine | undefined {
  const frame = arr(body?.stackFrames)[0];
  if (!isRecord(frame) || !isRecord(frame.source)) {
    return undefined;
  }
  const path = str(frame.source.path);
  const line = frame.line;
  if (path === '' || !Number.isSafeInteger(line) || (line as number) < 1) {
    return undefined;
  }
  return { path, line: line as number };
}
