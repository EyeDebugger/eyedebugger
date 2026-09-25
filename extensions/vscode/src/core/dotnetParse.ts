// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Readers of the 'eyedbg dotnet …' --json output (docs/adr/0015, P2-M9),
// tolerant: unknown fields are ignored, wrong types become 0 / '' or a
// dropped item, lists are capped. Program-derived strings stay raw here;
// dotnetRender makes them plain text.

import { EyedbgError, isRecord } from './protocol';

const maxPid = 2 ** 31 - 1;

/** isPid: a whole number from 1 to 2^31-1. */
export function isPid(x: unknown): x is number {
  return Number.isSafeInteger(x) && (x as number) >= 1 && (x as number) <= maxPid;
}

// --- Reading --json output (never trusting a shape: wrong types become
// 0 / '' / dropped items; lists are capped) ---

const maxItems = 1000;

function s(x: unknown): string {
  return typeof x === 'string' ? x : '';
}

function n(x: unknown): number {
  return typeof x === 'number' && Number.isFinite(x) ? x : 0;
}

function int(x: unknown): number {
  return Number.isSafeInteger(x) ? (x as number) : 0;
}

function optInt(x: unknown): number | undefined {
  return Number.isSafeInteger(x) ? (x as number) : undefined;
}

function list<T>(x: unknown, f: (v: Record<string, unknown>) => T | undefined): T[] {
  const out: T[] = [];
  for (const v of Array.isArray(x) ? x : []) {
    if (out.length === maxItems) {
      break;
    }
    const item = isRecord(v) ? f(v) : undefined;
    if (item !== undefined) {
      out.push(item);
    }
  }
  return out;
}

function rec(x: unknown): Record<string, unknown> {
  return isRecord(x) ? x : {};
}

export interface PsEntry {
  pid: number;
  name: string;
  session: string;
}

export function parsePs(v: Record<string, unknown>): PsEntry[] {
  return list(v.processes, (p) => (isPid(p.pid) ? { pid: p.pid, name: s(p.name), session: s(p.session) } : undefined));
}

export interface ProcessTarget {
  pid: number;
  name: string;
  session: string;
}

function parseTarget(x: unknown): ProcessTarget | undefined {
  return isRecord(x) ? { pid: int(x.pid), name: s(x.name), session: s(x.session) } : undefined;
}

export interface CounterValue {
  name: string;
  unit: string;
  kind: 'gauge' | 'sum';
  value: number;
}

export interface Sample {
  elapsedMs: number;
  counters: CounterValue[];
}

/**
 * parseWatchLine reads a line of 'counters --watch --json': a sample, or
 * undefined for a line that is no sample (blank, not JSON, an unknown
 * shape; the error line, which Eyedbg.stream keeps for itself).
 */
export function parseWatchLine(line: string): Sample | undefined {
  if (line.trim() === '') {
    return undefined;
  }
  let v: unknown;
  try {
    v = JSON.parse(line);
  } catch {
    return undefined;
  }
  if (!isRecord(v)) {
    return undefined;
  }
  if (!Array.isArray(v.counters)) {
    return undefined;
  }
  const seen = new Set<string>();
  const counters = list(v.counters, (c): CounterValue | undefined => {
    const name = s(c.name);
    const kind = c.kind === 'gauge' || c.kind === 'sum' ? c.kind : undefined;
    if (name === '' || seen.has(name) || kind === undefined) {
      return undefined;
    }
    seen.add(name);
    return { name, unit: s(c.unit), kind, value: n(c.value) };
  });
  return { elapsedMs: n(v.elapsedMs), counters };
}

export interface DumpFile {
  path: string;
  private: boolean;
  taken: boolean;
}

function parseDumpFile(x: unknown): DumpFile {
  const d = rec(x);
  return { path: s(d.path), private: d.private === true, taken: d.taken === true };
}

export interface DumpResult {
  target: ProcessTarget | undefined;
  type: string;
  path: string;
  bytes: number;
  private: boolean;
  elapsedMs: number;
}

export function parseDump(v: Record<string, unknown>): DumpResult {
  const path = s(v.path);
  if (path === '') {
    throw new EyedbgError('INTERNAL', 'eyedbg dotnet dump printed no path');
  }
  return {
    target: parseTarget(v.target),
    type: s(v.type),
    path,
    bytes: n(v.bytes),
    private: v.private === true,
    elapsedMs: n(v.elapsedMs),
  };
}

export interface Runtime {
  version: string;
  gc: string;
}

function parseRuntime(x: unknown): Runtime {
  const r = rec(x);
  return { version: s(r.version), gc: s(r.gc) };
}

export interface Generation {
  name: string;
  objects: number;
  bytes: number;
}

export interface TypeTotal {
  name: string;
  count: number;
  bytes: number;
}

export interface RootLabel {
  kind: string;
  name: string;
  thread: number | undefined;
  frame: string;
}

export interface Link {
  address: string;
  type: string;
}

export interface RootPath {
  root: RootLabel;
  chain: Link[];
  chainOmitted: number;
}

export interface Gcroot {
  type: string;
  address: string;
  instances: number | undefined;
  paths: RootPath[];
  pathsFound: number;
  complete: boolean;
}

export interface HeapResult {
  target: ProcessTarget | undefined;
  dump: DumpFile;
  runtime: Runtime;
  objects: number;
  bytes: number;
  generations: Generation[];
  typeCount: number;
  types: TypeTotal[];
  typesOmitted: number;
  gcroot: Gcroot | undefined;
}

function parseGcroot(x: unknown): Gcroot | undefined {
  if (!isRecord(x)) {
    return undefined;
  }
  const t = rec(x.target);
  return {
    type: s(t.type),
    address: s(t.address),
    instances: optInt(t.instances),
    paths: list(x.paths, (p) => {
      const r = rec(p.root);
      return {
        root: { kind: s(r.kind), name: s(r.name), thread: optInt(r.thread), frame: s(r.frame) },
        chain: list(p.chain, (l) => ({ address: s(l.address), type: s(l.type) })),
        chainOmitted: int(p.chainOmitted),
      };
    }),
    pathsFound: int(x.pathsFound),
    complete: x.complete === true,
  };
}

export function parseHeap(v: Record<string, unknown>): HeapResult {
  return {
    target: parseTarget(v.target),
    dump: parseDumpFile(v.dump),
    runtime: parseRuntime(v.runtime),
    objects: n(v.objects),
    bytes: n(v.bytes),
    generations: list(v.generations, (g) => ({ name: s(g.name), objects: n(g.objects), bytes: n(g.bytes) })),
    typeCount: int(v.typeCount),
    types: list(v.types, (t) =>
      s(t.name) === '' ? undefined : { name: s(t.name), count: n(t.count), bytes: n(t.bytes) },
    ),
    typesOmitted: int(v.typesOmitted),
    gcroot: parseGcroot(v.gcroot),
  };
}

export interface Frame {
  kind: 'managed' | 'runtime';
  method: string;
  module: string;
  ilOffset: number | undefined;
  /** The source path the PDB recorded (the build machine's; any string). */
  file: string;
  line: number;
}

export interface DumpThread {
  managedId: number;
  osId: number;
  alive: boolean;
  background: boolean;
  threadpool: boolean;
  finalizer: boolean;
  gc: boolean;
  exception: string;
  frames: Frame[];
  framesOmitted: number;
}

export interface Lock {
  object: string;
  type: string;
  owner: number | undefined;
  waiting: number;
}

export interface ThreadsResult {
  target: ProcessTarget | undefined;
  dump: DumpFile;
  runtime: Runtime;
  threads: DumpThread[];
  threadsOmitted: number;
  locks: Lock[];
  locksAvailable: boolean;
}

export function parseThreads(v: Record<string, unknown>): ThreadsResult {
  return {
    target: parseTarget(v.target),
    dump: parseDumpFile(v.dump),
    runtime: parseRuntime(v.runtime),
    threads: list(v.threads, (t) => ({
      managedId: int(t.managedId),
      osId: int(t.osId),
      alive: t.alive === true,
      background: t.background === true,
      threadpool: t.threadpool === true,
      finalizer: t.finalizer === true,
      gc: t.gc === true,
      exception: s(t.exception),
      frames: list(t.frames, (f) => ({
        kind: f.kind === 'managed' ? 'managed' : 'runtime',
        method: s(f.method),
        module: s(f.module),
        ilOffset: optInt(f.ilOffset),
        file: s(f.file),
        line: int(f.line),
      })),
      framesOmitted: int(t.framesOmitted),
    })),
    threadsOmitted: int(v.threadsOmitted),
    locks: list(v.locks, (l) => ({
      object: s(l.object),
      type: s(l.type),
      owner: optInt(l.owner),
      waiting: int(l.waiting),
    })),
    locksAvailable: v.locksAvailable === true,
  };
}

export interface MethodSamples {
  method: string;
  module: string;
  samples: number;
}

export interface CpuSummary {
  samples: number;
  managed: number;
  other: number;
  threads: number;
  unresolvedFrames: number;
  exclusive: MethodSamples[];
  exclusiveOmitted: number;
  inclusive: MethodSamples[];
  inclusiveOmitted: number;
  waiting: MethodSamples[];
  waitingOmitted: number;
}

export interface GcSummary {
  collections: number;
  gen0: number;
  gen1: number;
  gen2: number;
  background: number;
  induced: number;
  pauseTotalMs: number;
  pauseMaxMs: number;
}

export interface AllocSummary {
  ticks: number;
  bytes: number;
  typeCount: number;
  types: { name: string; ticks: number; bytes: number }[];
  typesOmitted: number;
}

export interface TraceResult {
  target: ProcessTarget | undefined;
  trace: { path: string; private: boolean; taken: boolean; bytes: number };
  profile: string;
  endReason: string;
  durationMs: number;
  eventsLost: number;
  cpu: CpuSummary | undefined;
  gc: GcSummary | undefined;
  allocations: AllocSummary | undefined;
}

function methods(x: unknown): MethodSamples[] {
  return list(x, (m) => ({ method: s(m.method), module: s(m.module), samples: n(m.samples) }));
}

export function parseTrace(v: Record<string, unknown>): TraceResult {
  const t = rec(v.trace);
  const cpu = isRecord(v.cpu) ? v.cpu : undefined;
  const gc = isRecord(v.gc) ? v.gc : undefined;
  const al = isRecord(v.allocations) ? v.allocations : undefined;
  return {
    target: parseTarget(v.target),
    trace: { path: s(t.path), private: t.private === true, taken: t.taken === true, bytes: n(t.bytes) },
    profile: s(v.profile),
    endReason: s(v.endReason),
    durationMs: n(v.durationMs),
    eventsLost: n(v.eventsLost),
    cpu:
      cpu === undefined
        ? undefined
        : {
            samples: n(cpu.samples),
            managed: n(cpu.managed),
            other: n(cpu.other),
            threads: n(cpu.threads),
            unresolvedFrames: n(cpu.unresolvedFrames),
            exclusive: methods(cpu.exclusive),
            exclusiveOmitted: n(cpu.exclusiveOmitted),
            inclusive: methods(cpu.inclusive),
            inclusiveOmitted: n(cpu.inclusiveOmitted),
            waiting: methods(cpu.waiting),
            waitingOmitted: n(cpu.waitingOmitted),
          },
    gc:
      gc === undefined
        ? undefined
        : {
            collections: n(gc.collections),
            gen0: n(gc.gen0),
            gen1: n(gc.gen1),
            gen2: n(gc.gen2),
            background: n(gc.background),
            induced: n(gc.induced),
            pauseTotalMs: n(gc.pauseTotalMs),
            pauseMaxMs: n(gc.pauseMaxMs),
          },
    allocations:
      al === undefined
        ? undefined
        : {
            ticks: n(al.ticks),
            bytes: n(al.bytes),
            typeCount: n(al.typeCount),
            types: list(al.types, (x) => ({ name: s(x.name), ticks: n(x.ticks), bytes: n(x.bytes) })),
            typesOmitted: n(al.typesOmitted),
          },
  };
}
