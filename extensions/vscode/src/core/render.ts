// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Every text the extension shows that holds a session's strings — other
// clients' conditions, log messages, lease-request messages, the adapter's
// messages, program paths — is built here, as plain text: control
// characters become spaces, lengths are capped, and a notification can't
// carry a link (VS Code runs command: links in notifications on click).

import type { ActivityEntry } from './activity';
import type { Mirror } from './mirrors';
import {
  type Breakpoint,
  type ClientInfo,
  clientKind,
  isClientId,
  type LeaseInfo,
  type SessionEvent,
  type SessionInfo,
} from './protocol';

/** plainText makes s one line of plain text: C0/C1 control characters (newlines too) become spaces, at most max characters. */
export function plainText(s: string, max = 1000): string {
  // biome-ignore lint/suspicious/noControlCharactersInRegex: matching control characters is the point.
  const flat = s.replace(/[\u0000-\u001f\u007f-\u009f\u2028\u2029]/g, ' ');
  const chars = Array.from(flat);
  return chars.length > max ? `${chars.slice(0, Math.max(0, max - 1)).join('')}…` : flat;
}

/**
 * notificationSafe is plainText that no VS Code notification turns into a
 * link: a link needs "](" (notifications.ts LINK_REGEX), which becomes "] (".
 */
export function notificationSafe(s: string, max = 1000): string {
  return plainText(s, max).replaceAll('](', '] (');
}

/** noIcons stops "$(name)" rendering as an icon in labels that support icons (QuickPick, status bar). */
export function noIcons(s: string): string {
  return s.replaceAll('$(', '$\u200a(');
}

/** clientLabel is a client id for display: as is when valid, else made plain. */
export function clientLabel(id: string): string {
  return isClientId(id) ? id : noIcons(plainText(id, 80));
}

// --- Activity (the CLI's 'eyedbg events' phrasing) ---

const maxEventLine = 200;

function cutLine(s: string): string {
  const chars = Array.from(s);
  return chars.length > maxEventLine ? `${chars.slice(0, maxEventLine).join('')}…` : s;
}

/** A Windows path: a drive (C:\ or C:/) or UNC (\\server) path. */
const windowsPath = /^(?:[A-Za-z]:[\\/]|\\\\)/;

/** Folds a Windows path for comparison: case-insensitive, either separator. */
function foldWindows(p: string): string {
  return p.replaceAll('/', '\\').toLowerCase();
}

/**
 * location is file:line, relative to base when under it. Under a Windows
 * base, the comparison ignores case and the separator: VS Code's
 * workspaceFolder.uri.fsPath has a lower-case drive letter (c:\…) while an
 * adapter reports the path as given (C:\…) or with forward slashes.
 */
export function location(file: string, line: number, base: string): string {
  let f = file;
  if (base !== '') {
    const b = base.replace(/[\\/]+$/, '');
    const sep = f.charAt(b.length);
    const inside = windowsPath.test(base)
      ? (sep === '\\' || sep === '/') && foldWindows(f.slice(0, b.length)) === foldWindows(b)
      : f.startsWith(`${b}/`) || f.startsWith(`${b}\\`);
    if (inside) {
      f = f.slice(b.length + 1);
    }
  }
  return line > 0 ? `${f}:${line}` : f;
}

function breakpointSpot(bp: Breakpoint, base: string): string {
  let s = bp.function !== '' ? `func:${bp.function}` : location(bp.file, bp.line, base);
  if (bp.condition !== '') {
    s += ` if ${bp.condition}`;
  }
  if (bp.hitCondition !== '') {
    s += ` hit ${bp.hitCondition}`;
  }
  if (bp.logMessage !== '') {
    s += ` log "${bp.logMessage}"`;
  }
  if (bp.temporary) {
    s += ' (run-until)';
  }
  return s;
}

function execVerb(action: string): string {
  switch (action) {
    case 'stepIn':
      return 'step-in';
    case 'stepOut':
      return 'step-out';
    case 'runUntil':
      return 'run-until';
    default:
      return action;
  }
}

function describeLease(e: SessionEvent): string {
  const from = e.previous !== '' ? ` from ${e.previous}` : '';
  const holder = e.lease?.holder ?? '';
  switch (e.action) {
    case 'take':
      return `lease: ${e.client} took it${from}`;
    case 'auto':
      return `lease: ${e.client} took it${from} (auto)`;
    case 'force':
      return `lease: ${e.client} took it${from} (forced)`;
    case 'grant':
      return `lease: ${e.client} gave it to ${holder}`;
    case 'release':
      return e.reason !== '' ? `lease: ${e.client} released it (${e.reason})` : `lease: ${e.client} released it`;
    case 'request':
      return `lease: ${e.client} asks ${holder} for it${e.text !== '' ? `: ${JSON.stringify(cutLine(e.text))}` : ''}`;
    case 'policy':
      return `lease: ${e.client} set the policy to ${e.lease?.policy ?? ''}`;
    default:
      return `lease: ${e.client} ${e.action}`;
  }
}

function describeBreakpoint(e: SessionEvent, base: string): string {
  const bp = e.breakpoint;
  if (bp === undefined) {
    return `bp ${e.action}`;
  }
  let head = `bp ${bp.id} ${e.action}`;
  switch (e.action) {
    case 'removed':
      return `${head} by ${e.client}${bp.owner !== e.client ? ` (owner ${bp.owner})` : ''}`;
    case 'added':
      return `${head} by ${e.client}: ${breakpointSpot(bp, base)}`;
    default: {
      if (e.client !== '') {
        head += ` by ${e.client}`;
      }
      const s = `${head}: ${breakpointSpot(bp, base)}`;
      if (bp.verified) {
        return `${s} verified`;
      }
      return bp.message !== '' ? `${s} pending: ${bp.message}` : `${s} pending`;
    }
  }
}

/** describeActivity is an eyedbg/activity event's text, as 'eyedbg events' prints it. */
export function describeActivity(e: SessionEvent, base: string): string {
  const thread = e.threadId !== 0 ? ` (thread ${e.threadId})` : '';
  switch (e.kind) {
    case 'exec':
      switch (e.action) {
        case 'eval':
          return `${e.client}: eval ${cutLine(e.text)} (side effects allowed)`;
        case 'set':
          return `${e.client}: set ${cutLine(e.text)}`;
        default:
          return `${e.client}: ${execVerb(e.action)}${thread}`;
      }
    case 'lease':
      return describeLease(e);
    case 'breakpoint':
      return describeBreakpoint(e, base);
    case 'exceptions':
      return e.reason === 'force'
        ? `${e.client}: exceptions ${e.action} (for every client)`
        : `${e.client}: exceptions ${e.action}`;
    case 'client':
      switch (e.action) {
        case 'connected':
          return `client ${e.client} connected (editor)`;
        case 'disconnected':
          return `client ${e.client} disconnected (editor)`;
        default:
          return `client ${e.client} joined`;
      }
    default:
      return `${e.kind}${e.client !== '' ? ` by ${e.client}` : ''}`;
  }
}

function pad(n: number): string {
  return String(n).padStart(2, '0');
}

/** clock is an ISO time as local HH:MM:SS (--:--:-- if it isn't one). */
export function clock(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) {
    return '--:--:--';
  }
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

/** activityLine is one line of the activity channel. */
export function activityLine(e: SessionEvent, session: string, base: string): string {
  return plainText(`${clock(e.time)} ${session} ${describeActivity(e, base)}`, 1000);
}

// --- The Activity view ---

const maxLabel = 200;

/** Stop reasons an exec's label doesn't repeat: they are what the action does. */
const plainStops = new Set(['', 'step', 'pause']);

function execLabel(e: SessionEvent): string {
  const who = clientLabel(e.client);
  switch (e.action) {
    case 'next':
      return `${who} stepped over`;
    case 'stepIn':
      return `${who} stepped into`;
    case 'stepOut':
      return `${who} stepped out`;
    case 'continue':
      return `${who} continued`;
    case 'runUntil':
      return `${who} ran to a line`;
    case 'pause':
      return `${who} paused`;
    case 'eval':
      return `${who} evaluated ${e.text}`;
    case 'set':
      return `${who} set ${e.text}`;
    default:
      return `${who} ${e.action}`;
  }
}

/** activityLabel is an Activity entry's label: what the client did and, for a step, where it stopped (relative to base). */
export function activityLabel(entry: ActivityEntry, base: string): string {
  const e = entry.event;
  let label: string;
  if (e.kind === 'exec') {
    label = execLabel(e);
    if (entry.location !== undefined) {
      label += ` → ${location(entry.location.path, entry.location.line, base)}`;
    }
    if (!plainStops.has(entry.stopReason)) {
      label += ` (${entry.stopReason})`;
    }
  } else {
    label = describeActivity(e, base);
  }
  return plainText(noIcons(label), maxLabel);
}

/** activityTooltip is an Activity entry's plain tooltip: its channel line and, for a step, the stop. */
export function activityTooltip(entry: ActivityEntry): string {
  const lines = [plainText(entry.line, 1000)];
  if (entry.event.kind === 'exec' && entry.location !== undefined) {
    lines.push(
      plainText(
        `Stopped: ${entry.stopReason || 'unknown reason'} at ${location(entry.location.path, entry.location.line, '')}`,
        1000,
      ),
    );
  }
  return lines.join('\n');
}

// --- The Clients view ---

/** clientsSessionDescription is a session row's description: who has control, the policy, pending requests. */
export function clientsSessionDescription(lease: LeaseInfo | undefined, self: string): string {
  if (lease === undefined) {
    return '';
  }
  const holder = lease.holder === '' ? 'nobody' : lease.holder === self ? 'you' : clientLabel(lease.holder);
  const n = lease.requests.length;
  const requests = n === 0 ? '' : ` · ${n} request${n === 1 ? '' : 's'}`;
  return plainText(noIcons(`control: ${holder} · ${lease.policy}${requests}`), maxLabel);
}

/** clientRowText is a client row's label, description and plain tooltip. */
export function clientRowText(
  c: ClientInfo,
  lease: LeaseInfo | undefined,
  self: string,
): { label: string; description: string; tooltip: string } {
  const label = noIcons(clientLabel(c.id) + (c.id === self ? ' (you)' : ''));
  const parts: string[] = [];
  if (lease !== undefined && lease.holder !== '' && lease.holder === c.id) {
    parts.push('has control');
  }
  if (c.connected > 0) {
    parts.push(c.connected > 1 ? `connected ×${c.connected}` : 'connected');
  }
  if (c.lastSeen !== '') {
    parts.push(`seen ${clock(c.lastSeen)}`);
  }
  const tips = [`${clientLabel(c.id)}${c.id === self ? ' (you)' : ''}`];
  if (parts.length > 0) {
    tips.push(parts.join(', '));
  }
  if (c.firstSeen !== '') {
    tips.push(`First seen ${clock(c.firstSeen)}`);
  }
  const request = lease?.requests.find((r) => r.client === c.id);
  if (request !== undefined) {
    tips.push(`Asks for control${request.message !== '' ? `: "${plainText(request.message, 200)}"` : ''}`);
  }
  return {
    label,
    description: plainText(noIcons(parts.join(' · ')), maxLabel),
    tooltip: tips.map((l) => plainText(l, 400)).join('\n'),
  };
}

// --- The auto-join prompt ---

/** autoJoinPrompt is the notification offering to join info, which agent is debugging. */
export function autoJoinPrompt(info: SessionInfo, agent: string): string {
  return notificationSafe(
    `${clientLabel(agent)} is debugging ${plainText(basename(info.program), 200)} in this workspace (${info.id}, ${info.lang}, ${info.state}).`,
    400,
  );
}

// --- Status bar ---

export function holderText(holder: string, self: string): string {
  if (holder === '') {
    return '$(circle-slash) nobody';
  }
  if (holder === self) {
    return '$(account) you';
  }
  return clientKind(holder) === 'agent' ? `$(hubot) ${clientLabel(holder)}` : `$(person) ${clientLabel(holder)}`;
}

function requestCount(lease: LeaseInfo): string {
  const n = lease.requests.length;
  return n === 0 ? '' : ` · ${n} request${n === 1 ? '' : 's'}`;
}

/** statusText is the lease status bar item's text. */
export function statusText(lease: LeaseInfo, self: string): string {
  const policy = lease.policy !== 'free' ? ` · ${lease.policy}` : '';
  return `${holderText(lease.holder, self)}${policy}${requestCount(lease)}`;
}

/** statusLine is one line on who has control of session, e.g. for the lease menu's title. */
export function statusLine(lease: LeaseInfo, self: string, session: string): string {
  const who =
    lease.holder === ''
      ? 'nobody has control'
      : lease.holder === self
        ? 'you have control'
        : `${clientLabel(lease.holder)} has control`;
  return plainText(`EyeDebugger ${session}: ${who} (lease policy ${lease.policy}).`, 400);
}

/** statusTooltip is the lease status bar item's (plain) tooltip. */
export function statusTooltip(lease: LeaseInfo, self: string, session: string): string {
  const lines = [statusLine(lease, self, session)];
  for (const r of lease.requests) {
    lines.push(`${r.client} asks for control${r.message !== '' ? `: "${plainText(r.message, 200)}"` : ''}`);
  }
  lines.push('Click for control actions.');
  return lines.map((l) => plainText(l, 400)).join('\n');
}

// --- Other clients' breakpoints ---

const maxAnnotation = 120;

function ownerPhrase(owner: string, self: string): string {
  return owner === self ? 'you (CLI)' : clientLabel(owner);
}

/**
 * annotationText is the end-of-line text beside a mirror: whose it is and
 * what it does; bp is the session's breakpoint (undefined until listed).
 */
export function annotationText(mirror: Mirror, bp: Breakpoint | undefined, self: string): string {
  const parts: string[] = [];
  if (bp === undefined) {
    parts.push('shared breakpoint');
  } else {
    parts.push(ownerPhrase(bp.owner, self));
    if (bp.condition !== '') {
      parts.push(`if ${bp.condition}`);
    }
    if (bp.hitCondition !== '') {
      parts.push(`hit ${bp.hitCondition}`);
    }
    if (bp.logMessage !== '') {
      parts.push(`logs "${bp.logMessage}"`);
    }
  }
  if (!mirror.verified) {
    parts.push('not verified');
  }
  return plainText(`⬥ ${parts.join(' · ')}`, maxAnnotation);
}

/** hoverCommands is the hover's last paragraph: a constant, safe as markdown (no markup characters). */
export const hoverCommands =
  'Right-click the line number for "Copy as My Breakpoint" or "Remove Breakpoint for Everyone…". ' +
  'Removing the red dot only hides it in this window.';

/** hoverParts are a mirror's hover paragraphs, each plain text (the caller appends them as text). */
export function hoverParts(mirror: Mirror, bp: Breakpoint | undefined, self: string): string[] {
  const parts: string[] = [];
  if (bp === undefined) {
    parts.push(`Breakpoint ${mirror.id} of another client (EyeDebugger)`);
    if (mirror.message !== '') {
      parts.push(mirror.message);
    }
  } else {
    parts.push(
      bp.owner === self
        ? `Your breakpoint ${bp.id}, set outside this editor (EyeDebugger)`
        : `Breakpoint ${bp.id} of ${bp.owner} (EyeDebugger)`,
    );
    if (bp.condition !== '') {
      parts.push(`Condition: ${bp.condition}`);
    }
    if (bp.hitCondition !== '') {
      parts.push(`Hit condition: ${bp.hitCondition}`);
    }
    if (bp.logMessage !== '') {
      parts.push(`Log message: ${bp.logMessage} (doesn't stop)`);
    }
    if (bp.note !== '') {
      parts.push(bp.note);
    }
  }
  parts.push(mirror.verified ? 'Verified' : 'Not verified');
  if (bp !== undefined && bp.message !== '') {
    parts.push(bp.message);
  }
  return parts.map((p) => plainText(p, 500));
}

// --- Notifications ---

export function leaseHeldNotice(holder: string, session: string, policy: string): string {
  return notificationSafe(
    `${holder || 'Another client'} has control of ${session}${policy !== '' ? ` (${policy})` : ''}.`,
  );
}

export function requestNotice(client: string, message: string): string {
  return notificationSafe(`${client} asks for control${message !== '' ? `: “${message}”` : '.'}`, 400);
}

/** errorNotice is an eyedbg error for a notification: its code and first line. */
export function errorNotice(code: string, message: string): string {
  const first = message.split(/\r?\n/, 1)[0] ?? '';
  return notificationSafe(code !== '' ? `${code}: ${first}` : first, 400);
}

// --- Sessions ---

export function basename(p: string): string {
  const parts = p.split(/[\\/]/);
  return parts[parts.length - 1] ?? p;
}

function control(info: SessionInfo): string {
  const holder = info.lease?.holder ?? '';
  return holder === '' ? 'nobody has control' : `${clientLabel(holder)} has control`;
}

/** sessionPickItem is a session's QuickPick entry. */
export function sessionPickItem(info: SessionInfo): { label: string; description: string; detail: string } {
  const connected = info.clients.filter((c) => c.connected > 0).map((c) => clientLabel(c.id));
  const detail = `${info.program}${connected.length > 0 ? ` — connected: ${connected.join(', ')}` : ''}`;
  return {
    label: noIcons(plainText(info.id, 40)),
    description: noIcons(plainText(`${info.lang} · ${info.state} · control: ${info.lease?.holder || 'nobody'}`, 200)),
    detail: noIcons(plainText(detail, 300)),
  };
}

/** joinConfigName names a dynamic configuration that joins info. */
export function joinConfigName(info: SessionInfo): string {
  return plainText(`Join ${info.id} — ${info.lang} ${basename(info.program)} (${control(info)})`, 200);
}
