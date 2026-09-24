// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Lease decisions (docs/adr/0014 and the session's lease rules): which
// actions the human can take now, and when to tell them something.

import { clientKind, type DapMessage, errorCode, errorHolder, type LeaseInfo } from './protocol';

export const leaseHeldCode = 'LEASE_HELD';

/** isLeaseHeld reports whether m is a response refused with LEASE_HELD. */
export function isLeaseHeld(m: DapMessage): boolean {
  return m.type === 'response' && !m.success && errorCode(m) === leaseHeldCode;
}

/** leaseHeldHolder is the holder a LEASE_HELD response names, else the known lease's. */
export function leaseHeldHolder(m: DapMessage, lease: LeaseInfo | undefined): string {
  return errorHolder(m) || lease?.holder || '';
}

/**
 * LeaseHeldNotices says when to show the "X has control" notice: once per
 * holder until the lease changes (holder or policy).
 */
export class LeaseHeldNotices {
  private shown: string | undefined;
  private last: string | undefined;

  /** leaseChanged records the lease now; a change of holder or policy re-arms the notice. */
  leaseChanged(lease: LeaseInfo): void {
    const key = `${lease.holder}\u0000${lease.policy}`;
    if (this.last !== undefined && this.last !== key) {
      this.shown = undefined;
    }
    this.last = key;
  }

  /** shouldShow reports whether to show the notice for holder, and records it. */
  shouldShow(holder: string): boolean {
    if (this.shown === holder) {
      return false;
    }
    this.shown = holder;
    return true;
  }
}

export type LeaseAction = 'take' | 'forceTake' | 'request' | 'release' | 'grant' | 'policy';

/** mayTake reports whether self may take the lease without force (the session's rules). */
export function mayTake(lease: LeaseInfo, self: string): boolean {
  if (lease.holder === '' || lease.holder === self) {
    return true;
  }
  switch (lease.policy) {
    case 'free':
      return true;
    case 'handoff':
      return false;
    case 'human-priority':
      return clientKind(self) === 'human' && clientKind(lease.holder) !== 'human';
  }
}

/** menuActions are the lease actions self can take now, in menu order. */
export function menuActions(lease: LeaseInfo, self: string): LeaseAction[] {
  if (lease.holder === self) {
    return ['release', 'grant', 'policy'];
  }
  if (lease.holder === '') {
    return ['take', 'grant', 'policy'];
  }
  if (mayTake(lease, self)) {
    return ['take', 'request'];
  }
  return ['request', 'forceTake'];
}

export type AfterTakeOver = 'ask' | 'humanPriority' | 'keep';

/**
 * afterTakeOver decides what to do when the lease moved from before to
 * after: 'ask' or 'switch' (to human-priority) when self got it from an
 * agent under the free policy, per the setting; asked tells whether this
 * session already asked. Only free lets the agent take control back by
 * running a command; under handoff and human-priority it already can't
 * without asking or forcing, so there is nothing to offer.
 */
export function afterTakeOver(
  setting: AfterTakeOver,
  before: LeaseInfo | undefined,
  after: LeaseInfo,
  self: string,
  asked: boolean,
): 'ask' | 'switch' | 'none' {
  if (before === undefined || after.holder !== self || before.holder === self) {
    return 'none';
  }
  if (clientKind(before.holder) !== 'agent' || after.policy !== 'free') {
    return 'none';
  }
  switch (setting) {
    case 'keep':
      return 'none';
    case 'humanPriority':
      return 'switch';
    case 'ask':
      return asked ? 'none' : 'ask';
  }
}

/**
 * RequestNotices says which pending lease requests to show self: other
 * clients' requests while self holds the lease, each once (by client and time).
 */
export class RequestNotices {
  private readonly shown = new Set<string>();

  next(lease: LeaseInfo, self: string): { client: string; message: string }[] {
    if (lease.holder !== self) {
      return [];
    }
    const out: { client: string; message: string }[] = [];
    for (const r of lease.requests) {
      const key = `${r.client}\u0000${r.at}`;
      if (r.client === self || this.shown.has(key)) {
        continue;
      }
      this.shown.add(key);
      out.push({ client: r.client, message: r.message });
    }
    return out;
  }
}
