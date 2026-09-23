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

	return writeText(w, b.String())
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
		fmt.Fprintf(&b, "#%d %s", f.Index, f.Name)

		if f.File != "" {
			fmt.Fprintf(&b, " %s", location(f.File, f.Line, base))
		}

		b.WriteString("\n")
	}

	return writeText(w, b.String())
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
		}

		writeVarList(&b, sc.Vars, sc.More, 1)
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

		b.WriteString(" = " + v.Value)

		if v.HasChildren && len(v.Children) == 0 {
			b.WriteString(" {…}")
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
