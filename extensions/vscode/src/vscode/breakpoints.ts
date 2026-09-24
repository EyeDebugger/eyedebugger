// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

// Other clients' breakpoints in the editor. VS Code draws each mirror as a
// red dot; this adds, at the end of its line, whose it is and what it does
// (never a gutter icon: that would stop gutter clicks on the line setting
// breakpoints), a plain-text hover, and two commands: Copy as My Breakpoint
// and Remove Breakpoint for Everyone.

import * as vscode from 'vscode';
import type { Mirror } from '../core/mirrors';
import type { Breakpoint } from '../core/protocol';
import {
  annotationText,
  clientLabel,
  hoverCommands,
  hoverParts,
  location,
  noIcons,
  notificationSafe,
  plainText,
} from '../core/render';
import { target } from './lease';
import type { Annotation, State, Tracked } from './state';

interface CommandArgs {
  uri?: unknown;
  lineNumber?: unknown;
  id?: unknown;
  session?: unknown;
  confirmed?: unknown;
}

interface Target {
  t: Tracked;
  id: number;
  bp: Breakpoint | undefined;
  mirror: Mirror | undefined;
}

function optional(s: string | undefined): string | undefined {
  return s === undefined || s === '' ? undefined : s;
}

export class BreakpointsUi implements vscode.Disposable {
  private readonly deco: vscode.TextEditorDecorationType;
  private readonly subs: vscode.Disposable[] = [];

  constructor(private readonly state: State) {
    this.deco = vscode.window.createTextEditorDecorationType({
      after: {
        color: new vscode.ThemeColor('eyedbg.sharedBreakpoint.foreground'),
        margin: '0 0 0 3em',
        fontStyle: 'italic',
      },
      overviewRulerColor: new vscode.ThemeColor('eyedbg.sharedBreakpoint.overviewRuler'),
      overviewRulerLane: vscode.OverviewRulerLane.Right,
    });
    this.subs.push(
      this.deco,
      vscode.window.onDidChangeVisibleTextEditors(() => this.refresh()),
      vscode.commands.registerCommand('eyedbg.breakpoints.copyAsMine', (a?: unknown) =>
        this.run(a, (x) => this.copy(x)),
      ),
      vscode.commands.registerCommand('eyedbg.breakpoints.removeForEveryone', (a?: unknown) =>
        this.run(a, (x) => this.remove(x, (a as CommandArgs | undefined)?.confirmed === true)),
      ),
    );
  }

  dispose(): void {
    for (const s of this.subs) {
      s.dispose();
    }
  }

  /** refresh redraws the annotations of every visible editor. */
  refresh(): void {
    const byUri = new Map<string, { t: Tracked; m: Mirror }[]>();
    for (const t of this.state.all()) {
      if (t.unsupported) {
        continue;
      }
      for (const m of t.mirrors.all()) {
        const key = vscode.Uri.file(m.path).toString();
        const list = byUri.get(key) ?? [];
        list.push({ t, m });
        byUri.set(key, list);
      }
    }
    const drawn: Annotation[] = [];
    for (const editor of vscode.window.visibleTextEditors) {
      const doc = editor.document;
      const uri = doc.uri.toString();
      const lines = new Set<number>();
      const options: vscode.DecorationOptions[] = [];
      for (const { t, m } of (byUri.get(uri) ?? []).sort((a, b) => a.m.id - b.m.id)) {
        if (m.line < 1 || m.line > doc.lineCount || lines.has(m.line)) {
          continue;
        }
        lines.add(m.line);
        const bp = t.breakpoint(m.id);
        const text = annotationText(m, bp, t.client);
        const hover = new vscode.MarkdownString(undefined, false);
        hover.isTrusted = false;
        hover.supportHtml = false;
        for (const part of hoverParts(m, bp, t.client)) {
          hover.appendText(part);
          hover.appendMarkdown('\n\n');
        }
        // The extension's own sentence, no session text: markdown (appendText
        // turns spaces into non-breaking ones, so a long line can't wrap).
        hover.appendMarkdown(hoverCommands);
        options.push({
          range: doc.lineAt(m.line - 1).range,
          hoverMessage: hover,
          renderOptions: { after: { contentText: text } },
        });
        drawn.push({ uri, line: m.line, id: m.id, text });
      }
      editor.setDecorations(this.deco, options);
    }
    drawn.sort((a, b) => a.uri.localeCompare(b.uri) || a.line - b.line);
    if (JSON.stringify(drawn) !== JSON.stringify(this.state.annotations)) {
      this.state.annotations = drawn;
    }
    // The mirrors or the list changed (the API's snapshots show both).
    this.state.fire();
  }

  private async run(a: unknown, fn: (x: Target) => Promise<void>): Promise<void> {
    try {
      const x = await this.resolve((a ?? {}) as CommandArgs);
      if (x !== undefined) {
        await fn(x);
      }
    } catch (e) {
      this.state.showError(e);
    }
  }

  /** resolve finds the breakpoint a command is for: a line (gutter menu), an id, or a pick. */
  private async resolve(a: CommandArgs): Promise<Target | undefined> {
    if (a.uri !== undefined && typeof a.lineNumber === 'number') {
      const uri = a.uri instanceof vscode.Uri ? a.uri.toString() : typeof a.uri === 'string' ? a.uri : '';
      for (const t of this.state.all()) {
        const m = t.mirrors.all().find((x) => x.line === a.lineNumber && vscode.Uri.file(x.path).toString() === uri);
        if (m !== undefined) {
          return { t, id: m.id, bp: t.breakpoint(m.id), mirror: m };
        }
      }
      void this.state.show('info', "There is no other client's breakpoint on this line.");
      return undefined;
    }
    const t = target(this.state, a.session);
    if (t === undefined) {
      void this.state.show('error', 'No eyedbg debug session is active.');
      return undefined;
    }
    if (typeof a.id === 'number') {
      const bp = t.breakpoint(a.id);
      const mirror = t.mirrors.get(a.id);
      if (bp === undefined && mirror === undefined) {
        void this.state.show('error', `There is no breakpoint ${a.id} in ${t.eyedbgId}.`);
        return undefined;
      }
      return { t, id: a.id, bp, mirror };
    }
    const base = t.session.workspaceFolder?.uri.fsPath ?? '';
    const others = t.list.filter((b) => b.owner !== t.client || !b.editor);
    if (others.length === 0) {
      void this.state.show('info', `${t.eyedbgId} has no breakpoints of other clients.`);
      return undefined;
    }
    const pick = await vscode.window.showQuickPick(
      others.map((b) => ({
        label: noIcons(plainText(`${b.id} · ${clientLabel(b.owner)}`, 100)),
        description: noIcons(plainText(b.function !== '' ? `func:${b.function}` : location(b.file, b.line, base), 200)),
        detail: noIcons(
          plainText(
            [
              b.condition && `if ${b.condition}`,
              b.hitCondition && `hit ${b.hitCondition}`,
              b.logMessage && `logs "${b.logMessage}"`,
            ]
              .filter((s) => s !== '')
              .join(' · '),
            300,
          ),
        ),
        id: b.id,
      })),
      { title: `Breakpoints of other clients in ${t.eyedbgId}` },
    );
    if (pick === undefined) {
      return undefined;
    }
    return { t, id: pick.id, bp: t.breakpoint(pick.id), mirror: t.mirrors.get(pick.id) };
  }

  /** copy adds the breakpoint as the human's own: at character 0, so it carries no column (not a copy). */
  private async copy(x: Target): Promise<void> {
    const cond = optional(x.bp?.condition);
    const hit = optional(x.bp?.hitCondition);
    const log = optional(x.bp?.logMessage);
    if (x.bp !== undefined && x.bp.function !== '') {
      vscode.debug.addBreakpoints([new vscode.FunctionBreakpoint(x.bp.function, true, cond, hit, log)]);
      return;
    }
    const file = x.mirror?.path ?? x.bp?.file ?? '';
    const line = x.mirror?.line ?? x.bp?.line ?? 0;
    if (file === '' || line < 1) {
      return;
    }
    const at = new vscode.Location(vscode.Uri.file(file), new vscode.Position(line - 1, 0));
    vscode.debug.addBreakpoints([new vscode.SourceBreakpoint(at, true, cond, hit, log)]);
  }

  /** remove removes the breakpoint from the session, for every client, after asking. */
  private async remove(x: Target, confirmed: boolean): Promise<void> {
    const owner = x.bp?.owner ?? '';
    if (!confirmed) {
      const where =
        x.bp === undefined
          ? `breakpoint ${x.id}`
          : x.bp.function !== ''
            ? `func:${x.bp.function}`
            : location(x.bp.file, x.bp.line, x.t.session.workspaceFolder?.uri.fsPath ?? '');
      const cond = x.bp?.condition ? ` if ${x.bp.condition}` : '';
      const ok = await vscode.window.showWarningMessage(
        notificationSafe(
          `Remove ${owner !== '' ? `${clientLabel(owner)}'s` : 'the'} breakpoint ${where}${cond} for everyone?`,
          400,
        ),
        {
          modal: true,
          detail: `It is removed from eyedbg session ${x.t.eyedbgId}; its owner sees that you removed it.`,
        },
        'Remove',
      );
      if (ok !== 'Remove') {
        return;
      }
    }
    const body = await this.state.custom(x.t, 'eyedbg/breakpoints', {
      action: 'remove',
      id: x.id,
      force: owner !== x.t.client,
    });
    if (body === undefined) {
      void this.state.show('warning', `The eyedbg daemon of ${x.t.eyedbgId} is too old for this.`);
    }
  }
}
