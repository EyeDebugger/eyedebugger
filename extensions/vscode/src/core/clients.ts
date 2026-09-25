// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// The Clients view's model: a session's clients in display order, when each
// was last seen, and which lease actions each row offers (the session's
// lease rules, as the lease menu applies them).

import { menuActions } from './lease';
import { type ClientInfo, isClientId, type LeaseInfo } from './protocol';

export type ClientFlag = 'canRequest' | 'canTake' | 'canRelease' | 'canGrant';

function time(iso: string): number {
  const t = Date.parse(iso);
  return Number.isNaN(t) ? 0 : t;
}

/** later is the later of two ISO times ('' counts as never). */
export function later(a: string, b: string): string {
  if (b === '' || time(b) === 0) {
    return a;
  }
  if (a === '' || time(a) === 0) {
    return b;
  }
  return time(b) > time(a) ? b : a;
}

/**
 * merged are the clients with lastSeen raised to the time of their latest
 * activity event: the session moves lastSeen on every request without an
 * eyedbg/clients event, so the list alone goes stale.
 */
export function merged(clients: readonly ClientInfo[], seen: ReadonlyMap<string, string>): ClientInfo[] {
  return clients.map((c) => ({ ...c, lastSeen: later(c.lastSeen, seen.get(c.id) ?? '') }));
}

/** ordered is the display order: the lease holder, then connected clients, then by last seen (newest first), then by id. */
export function ordered(clients: readonly ClientInfo[], holder: string): ClientInfo[] {
  const rank = (c: ClientInfo) => (c.id === holder && holder !== '' ? 0 : c.connected > 0 ? 1 : 2);
  return [...clients].sort(
    (a, b) => rank(a) - rank(b) || time(b.lastSeen) - time(a.lastSeen) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0),
  );
}

/** clientActions are the lease actions a client's row offers self. */
export function clientActions(lease: LeaseInfo | undefined, client: string, self: string): ClientFlag[] {
  if (lease === undefined || !isClientId(client)) {
    return [];
  }
  if (client === self) {
    return lease.holder === self ? ['canRelease'] : [];
  }
  if (client === lease.holder) {
    return ['canRequest', 'canTake'];
  }
  return lease.holder === self || lease.holder === '' ? ['canGrant'] : [];
}

/** sessionActions are the lease actions a session's row offers self. */
export function sessionActions(lease: LeaseInfo | undefined, self: string): 'canPolicy'[] {
  return lease !== undefined && menuActions(lease, self).includes('policy') ? ['canPolicy'] : [];
}
