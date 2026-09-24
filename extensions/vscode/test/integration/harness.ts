// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// A small test harness for the extension host: tests run one after another,
// each under a deadline, with cleanups that always run; the summary lists
// every result, and run() rejects if any test failed.

export interface Ctx {
  /** cleanup registers fn to run after the test, last registered first. */
  cleanup(fn: () => unknown): void;
  /** log writes a line to the test output. */
  log(msg: string): void;
}

interface Test {
  name: string;
  fn: (ctx: Ctx) => Promise<void>;
  deadlineMs: number;
}

const tests: Test[] = [];

export function test(name: string, fn: (ctx: Ctx) => Promise<void>, deadlineMs = 120_000): void {
  tests.push({ name, fn, deadlineMs });
}

function out(msg: string): void {
  process.stdout.write(`${msg}\n`);
}

function message(e: unknown): string {
  return e instanceof Error ? (e.stack ?? e.message) : String(e);
}

const cleanupDeadlineMs = 60_000;

/** withDeadline rejects if p hasn't settled within ms (the work itself isn't cancelled). */
async function withDeadline<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  const deadline = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what}: deadline of ${ms / 1000} s passed`)), ms);
  });
  try {
    return await Promise.race([p, deadline]);
  } finally {
    clearTimeout(timer);
  }
}

async function runOne(t: Test): Promise<string | undefined> {
  const cleanups: (() => unknown)[] = [];
  const ctx: Ctx = {
    cleanup: (fn) => cleanups.push(fn),
    log: (msg) => out(`    ${msg}`),
  };
  let failure: string | undefined;
  try {
    await withDeadline(t.fn(ctx), t.deadlineMs, 'the test');
  } catch (e) {
    failure = message(e);
  }
  for (const fn of cleanups.reverse()) {
    try {
      await withDeadline(Promise.resolve().then(fn), cleanupDeadlineMs, 'a cleanup');
    } catch (e) {
      out(`    cleanup failed: ${message(e)}`);
      failure ??= `cleanup: ${message(e)}`;
    }
  }
  return failure;
}

/** runAll runs every registered test; only those whose name matches filter, if given. */
export async function runAll(filter?: RegExp): Promise<void> {
  const results: [string, string | undefined, number][] = [];
  for (const t of tests) {
    if (filter !== undefined && !filter.test(t.name)) {
      continue;
    }
    out(`--- ${t.name}`);
    const started = Date.now();
    const failure = await runOne(t);
    const ms = Date.now() - started;
    out(failure === undefined ? `ok   ${t.name} (${ms} ms)` : `FAIL ${t.name} (${ms} ms)\n${failure}`);
    results.push([t.name, failure, ms]);
  }
  const failed = results.filter(([, f]) => f !== undefined);
  out(`\n${results.length - failed.length} passed, ${failed.length} failed`);
  for (const [name] of failed) {
    out(`  FAIL ${name}`);
  }
  if (failed.length > 0 || results.length === 0) {
    throw new Error(results.length === 0 ? 'no tests ran' : `${failed.length} test(s) failed`);
  }
}
