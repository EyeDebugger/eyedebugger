// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Text renderers for session commands. Paths are shown relative to base
// when they are under it. --json output is the API value plus "schema".

func writeSnapshot(w io.Writer, snap api.Snapshot, asJSON bool, base string) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema       int `json:"schema"`
			api.Snapshot     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, snap})
	}

	var b strings.Builder

	b.WriteString(snapshotHeader(snap) + "\n")

	if f := snap.Frame; f != nil {
		fmt.Fprintf(&b, "  at %s %s\n", f.Name, location(f.File, f.Line, base))
	}

	width := 0
	for _, l := range snap.Source {
		width = max(width, len(strconv.Itoa(l.Line)))
	}

	for _, l := range snap.Source {
		marker := "  "
		if l.Current {
			marker = "> "
		}

		fmt.Fprintf(&b, "  %s%*d | %s\n", marker, width, l.Line, l.Text)
	}

	if snap.Reached != nil && !*snap.Reached && snap.Target != nil {
		fmt.Fprintf(&b, "  run-until: did not reach %s (the program stopped or exited first)\n",
			location(snap.Target.File, snap.Target.Line, base))
	}

	writeSnapshotOutput(&b, snap)
	writeSnapshotDump(&b, snap, base)

	return writeText(w, b.String())
}

// writeSnapshotDump writes what --dump asked for.
func writeSnapshotDump(b *strings.Builder, snap api.Snapshot, base string) {
	if c := snap.Changes; c != nil {
		switch {
		case c.NewFrame:
			b.WriteString("  locals (first stop in this function):\n")
		case len(c.Vars) == 0 && c.More == 0:
			b.WriteString("  changed since the previous stop: nothing\n")
		default:
			b.WriteString("  changed since the previous stop:\n")
		}

		vars := c.Vars
		if c.NewFrame { // all new: the header says so
			vars = make([]api.Var, len(c.Vars))
			for i, v := range c.Vars {
				v.Change = ""
				vars[i] = v
			}
		}

		writeVarList(b, vars, c.More, 2)
		writeBudgetHint(b, c.Truncated, 2)
	}

	if sc := snap.Locals; sc != nil {
		b.WriteString("  locals:\n")
		writeVarList(b, sc.Vars, sc.More, 2)
		writeBudgetHint(b, sc.Truncated, 2)
	}

	if len(snap.Stack) > 0 {
		b.WriteString("  stack:\n")

		for _, f := range snap.Stack {
			b.WriteString("    " + frameLine(f, base) + "\n")
		}
	}
}

// writeSnapshotOutput writes what the program printed since it resumed.
func writeSnapshotOutput(b *strings.Builder, snap api.Snapshot) {
	if len(snap.Output) == 0 {
		return
	}

	b.WriteString("  output since it resumed:\n")

	if snap.OutputOmitted > 0 {
		fmt.Fprintf(b, "    (%d earlier chunks not shown: 'eyedbg output')\n", snap.OutputOmitted)
	}

	var text strings.Builder
	for _, l := range snap.Output {
		text.WriteString(l.Text)
	}

	for line := range strings.Lines(text.String()) {
		b.WriteString("    " + strings.TrimRight(line, "\r\n") + "\n")
	}
}

func writeBudgetHint(b *strings.Builder, truncated bool, indent int) {
	if truncated {
		fmt.Fprintf(b, "%s(cut to fit --budget: 'eyedbg vars --expand NAME' shows one variable, --budget 0 shows all)\n",
			strings.Repeat("  ", indent))
	}
}

// snapshotHeader is the first line: session, state and why.
func snapshotHeader(snap api.Snapshot) string {
	s := snap.Session

	var b strings.Builder

	fmt.Fprintf(&b, "session %s (%s) %s", s.ID, s.Lang, s.State)

	switch s.State {
	case api.StateStopped:
		if s.Stop != nil {
			fmt.Fprintf(&b, ": %s", s.Stop.Reason)

			if detail := firstNonEmpty(s.Stop.Text, s.Stop.Description); detail != "" && detail != s.Stop.Reason {
				fmt.Fprintf(&b, " (%s)", detail)
			}

			fmt.Fprintf(&b, ", thread %d", s.Stop.ThreadID)
		}
	case api.StateExited:
		if s.ExitCode != nil {
			fmt.Fprintf(&b, " with code %d", *s.ExitCode)
		}

		if s.EndReason != "" {
			fmt.Fprintf(&b, ": %s", s.EndReason)
		}
	case api.StateRunning:
		if snap.TimedOut {
			b.WriteString(" (still running when the wait ended; 'eyedbg wait' or 'eyedbg pause')")
		}
	case api.StateStarting:
	}

	return b.String()
}

func writeSessions(w io.Writer, list []api.SessionInfo, asJSON bool) error {
	if asJSON {
		if list == nil {
			list = []api.SessionInfo{}
		}

		return writeJSON(w, struct {
			Schema   int               `json:"schema"`
			Sessions []api.SessionInfo `json:"sessions"`
		}{jsonSchemaVersion, list})
	}

	var b strings.Builder

	for i := range list {
		s := &list[i]
		state := string(s.State)
		if s.State == api.StateStopped && s.Stop != nil {
			state += " (" + s.Stop.Reason + ")"
		}

		if s.State == api.StateExited && s.ExitCode != nil {
			state += fmt.Sprintf(" (code %d)", *s.ExitCode)
		}

		b.WriteString(strings.TrimRight(fmt.Sprintf("%s  %s  %s  %s", s.ID, s.Lang, state, s.Program), " ") + "\n")
	}

	return writeText(w, b.String())
}

func writeStack(w io.Writer, frames []api.Frame, asJSON bool, base string) error {
	if asJSON {
		if frames == nil {
			frames = []api.Frame{}
		}

		return writeJSON(w, struct {
			Schema int         `json:"schema"`
			Frames []api.Frame `json:"frames"`
		}{jsonSchemaVersion, frames})
	}

	var b strings.Builder

	for _, f := range frames {
		b.WriteString(frameLine(f, base) + "\n")
	}

	return writeText(w, b.String())
}

// frameLine is "#N function file:line".
func frameLine(f api.Frame, base string) string {
	s := fmt.Sprintf("#%d %s", f.Index, f.Name)
	if f.File != "" {
		s += " " + location(f.File, f.Line, base)
	}

	return s
}

func writeVars(w io.Writer, scopes []api.Scope, asJSON bool) error {
	if asJSON {
		if scopes == nil {
			scopes = []api.Scope{}
		}

		return writeJSON(w, struct {
			Schema int         `json:"schema"`
			Scopes []api.Scope `json:"scopes"`
		}{jsonSchemaVersion, scopes})
	}

	var b strings.Builder

	for _, sc := range scopes {
		fmt.Fprintf(&b, "%s:\n", sc.Name)

		if sc.Expensive {
			b.WriteString("  (not fetched: the adapter marks this scope expensive)\n")
		} else if len(sc.Vars) == 0 && sc.More == 0 {
			b.WriteString("  (none)\n")
		}

		writeVarList(&b, sc.Vars, sc.More, 1)
		writeBudgetHint(&b, sc.Truncated, 1)
	}

	return writeText(w, b.String())
}

func writeVarList(b *strings.Builder, vars []api.Var, more, indent int) {
	pad := strings.Repeat("  ", indent)

	for _, v := range vars {
		b.WriteString(pad + v.Name)

		if v.Type != "" {
			b.WriteString(": " + v.Type)
		}

		if v.Value != "" || v.Type != "" {
			b.WriteString(" = " + v.Value)
		}

		if v.HasChildren && len(v.Children) == 0 {
			b.WriteString(" {…}")
		}

		switch v.Change {
		case "changed":
			b.WriteString("  (was " + v.Previous + ")")
		case "new":
			b.WriteString("  (new)")
		}

		b.WriteString("\n")

		writeVarList(b, v.Children, v.More, indent+1)
	}

	if more > 0 {
		fmt.Fprintf(b, "%s… %d more\n", pad, more)
	}
}

func writeEval(w io.Writer, res api.EvalResult, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema         int `json:"schema"`
			api.EvalResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, res})
	}

	s := res.Value
	if res.Type != "" {
		s += "  (" + res.Type + ")"
	}

	return writeText(w, s+"\n")
}

func writeOutput(w io.Writer, lines []api.OutputLine, asJSON bool) error {
	if asJSON {
		if lines == nil {
			lines = []api.OutputLine{}
		}

		return writeJSON(w, struct {
			Schema int              `json:"schema"`
			Output []api.OutputLine `json:"output"`
		}{jsonSchemaVersion, lines})
	}

	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l.Text)
	}

	out := b.String()
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	return writeText(w, out)
}

func writeBreakpoints(w io.Writer, bps []api.Breakpoint, asJSON bool, base string) error {
	if asJSON {
		if bps == nil {
			bps = []api.Breakpoint{}
		}

		return writeJSON(w, struct {
			Schema      int              `json:"schema"`
			Breakpoints []api.Breakpoint `json:"breakpoints"`
		}{jsonSchemaVersion, bps})
	}

	var b strings.Builder

	for _, bp := range bps {
		fmt.Fprintf(&b, "%d  %s", bp.ID, location(bp.File, bp.Line, base))

		if bp.Line != bp.RequestedLine {
			fmt.Fprintf(&b, " (requested line %d)", bp.RequestedLine)
		}

		if bp.Condition != "" {
			fmt.Fprintf(&b, " if %s", bp.Condition)
		}

		if bp.Temporary {
			b.WriteString(" (run-until, temporary)")
		}

		if bp.Verified {
			b.WriteString("  verified")
		} else {
			b.WriteString("  pending")

			if bp.Message != "" {
				b.WriteString(": " + bp.Message)
			}
		}

		b.WriteString("\n")
	}

	return writeText(w, b.String())
}

// location renders file:line, relative to base when file is under it.
func location(file string, line int, base string) string {
	if base != "" {
		if rel, err := filepath.Rel(base, file); err == nil && !strings.HasPrefix(rel, "..") {
			file = rel
		}
	}

	if line > 0 {
		return fmt.Sprintf("%s:%d", file, line)
	}

	return file
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}

	return ""
}
