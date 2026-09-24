// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Which of the editor's breakpoints are copies of other clients' (mirrors,
// docs/adr/0014), derived from the DAP traffic between VS Code and eyedbg's
// facade. VS Code never shows these copies to extensions
// (vscode.debug.breakpoints), so the extension follows the same messages
// VS Code applies: breakpoint events at column 1 announce or change a
// mirror, removed events drop it, and each setBreakpoints response says,
// for the request's path, exactly which entries are mirrors (answered at
// column 1 with a positive id; a negative id is a retracted copy). A
// mirror's line is where VS Code shows it: the adapter's line once
// verified, else the line VS Code had (debugModel.ts Breakpoint.lineNumber).

import { type DapMessage, isRecord, num, str } from './protocol';

/** markColumn is the column the facade gives mirrors. */
export const markColumn = 1;

export interface Mirror {
  id: number;
  /** The source path VS Code keys the copy under. */
  path: string;
  line: number;
  verified: boolean;
  /** The facade's message (whose it is and what it does). */
  message: string;
}

interface PendingRequest {
  path: string;
  /** The line of each entry the request sent (0: none). */
  lines: number[];
  /** Lines the request sent at the mark column. */
  markedLines: Set<number>;
}

function asMirror(b: unknown, path?: string): Mirror | undefined {
  if (!isRecord(b)) {
    return undefined;
  }
  const id = num(b.id);
  const line = num(b.line);
  const source = isRecord(b.source) ? str(b.source.path) : '';
  const p = path ?? source;
  if (b.column !== markColumn || !Number.isSafeInteger(id) || id <= 0 || line <= 0 || p === '') {
    return undefined;
  }
  return { id, path: p, line, verified: b.verified === true, message: str(b.message) };
}

export class MirrorModel {
  private readonly mirrors = new Map<number, Mirror>();
  private readonly pending = new Map<number, PendingRequest>();

  /**
   * key says which paths VS Code takes for one editor file: it keys
   * breakpoints by URI (Uri.file(path).toString(), which folds a Windows
   * drive letter's case); the default is the path itself.
   */
  constructor(private readonly key: (path: string) => string = (p) => p) {}

  /** all returns the mirrors, by id. */
  all(): Mirror[] {
    return [...this.mirrors.values()].sort((a, b) => a.id - b.id);
  }

  get(id: number): Mirror | undefined {
    return this.mirrors.get(id);
  }

  clear(): void {
    this.mirrors.clear();
    this.pending.clear();
  }

  /** fromEditor reads a message VS Code sends the adapter. */
  fromEditor(m: DapMessage): void {
    if (m.type !== 'request' || m.command !== 'setBreakpoints') {
      return;
    }
    const source = m.arguments?.source;
    const path = isRecord(source) ? str(source.path) : '';
    if (path === '') {
      return;
    }
    const lines: number[] = [];
    const markedLines = new Set<number>();
    const entries = m.arguments?.breakpoints;
    if (Array.isArray(entries)) {
      for (const e of entries) {
        const line = isRecord(e) ? num(e.line) : 0;
        lines.push(line);
        if (isRecord(e) && e.column === markColumn && line > 0) {
          markedLines.add(line);
        }
      }
    }
    this.pending.set(m.seq, { path, lines, markedLines });
  }

  /** fromAdapter reads a message the adapter sends VS Code; true if the mirrors changed. */
  fromAdapter(m: DapMessage): boolean {
    if (m.type === 'event') {
      return this.event(m);
    }
    if (m.type !== 'response') {
      return false;
    }
    if (m.command === 'disconnect') {
      return this.reset();
    }
    if (m.command !== 'setBreakpoints') {
      return false;
    }
    const req = this.pending.get(m.requestSeq);
    if (req === undefined) {
      return false;
    }
    this.pending.delete(m.requestSeq);
    return m.success ? this.answered(req, m) : this.refused(req);
  }

  private reset(): boolean {
    const changed = this.mirrors.size > 0;
    this.clear();
    return changed;
  }

  private event(m: DapMessage): boolean {
    if (m.event === 'terminated') {
      return this.reset();
    }
    if (m.event !== 'breakpoint' || m.body === undefined) {
      return false;
    }
    const reason = m.body.reason;
    const bp = m.body.breakpoint;
    if (reason === 'removed') {
      const id = isRecord(bp) ? num(bp.id) : 0;
      return this.mirrors.delete(id);
    }
    if (reason !== 'new' && reason !== 'changed') {
      return false;
    }
    const mirror = asMirror(bp);
    if (mirror === undefined) {
      return false;
    }
    if (reason === 'changed') {
      // VS Code applies a change to a breakpoint it knows only, and shows
      // an unverified one at the line it had.
      const old = this.mirrors.get(mirror.id);
      if (old === undefined) {
        return false;
      }
      if (!mirror.verified) {
        mirror.line = old.line;
      }
    }
    this.mirrors.set(mirror.id, mirror);
    return true;
  }

  /** answered: the mirrors under the request's path become the entries answered at the mark column. */
  private answered(req: PendingRequest, m: DapMessage): boolean {
    const before = JSON.stringify(this.all());
    this.dropPath(req.path, () => true);
    const answers = m.body?.breakpoints;
    if (Array.isArray(answers)) {
      answers.forEach((a, i) => {
        const mirror = asMirror(a, req.path);
        if (mirror === undefined) {
          return;
        }
        // VS Code pairs answers with entries by index and shows an
        // unverified breakpoint at the line it sent.
        const sent = req.lines[i] ?? 0;
        if (!mirror.verified && sent > 0) {
          mirror.line = sent;
        }
        this.mirrors.set(mirror.id, mirror);
      });
    }
    return JSON.stringify(this.all()) !== before;
  }

  /**
   * refused: a failed setBreakpoints still settled the facade's view on
   * the request (the editor holds exactly what it sent), so mirrors under
   * the path whose line the request didn't send at the mark column are gone.
   */
  private refused(req: PendingRequest): boolean {
    return this.dropPath(req.path, (mirror) => !req.markedLines.has(mirror.line));
  }

  private dropPath(path: string, drop: (m: Mirror) => boolean): boolean {
    const key = this.key(path);
    let changed = false;
    for (const [id, mirror] of this.mirrors) {
      if (this.key(mirror.path) === key && drop(mirror)) {
        this.mirrors.delete(id);
        changed = true;
      }
    }
    return changed;
  }
}
