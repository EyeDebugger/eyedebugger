// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

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
	writeSharing(&b, snap.Session)

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

	if snap.Exception != nil {
		writeException(&b, snap.Exception)
	}

	if snap.Reached != nil && !*snap.Reached && snap.Target != nil {
		fmt.Fprintf(&b, "  run-until: did not reach %s (the program stopped or exited first)\n",
			location(snap.Target.File, snap.Target.Line, base))
	}

	writeSnapshotOutput(&b, snap)
	writeSnapshotDump(&b, snap, base)

	return writeText(w, b.String())
}

// exceptionStackLines is how many stack trace lines a snapshot shows.
const exceptionStackLines = 5

// writeException writes the exception the program stopped at: type and
// message, the first stack lines, and inner exceptions.
func writeException(b *strings.Builder, e *api.ExceptionInfo) {
	b.WriteString("  exception: " + exceptionLine(e) + "\n")

	if st := strings.TrimRight(e.StackTrace, "\r\n"); st != "" {
		lines := strings.Split(st, "\n")

		for _, l := range lines[:min(len(lines), exceptionStackLines)] {
			b.WriteString("    " + strings.TrimSpace(l) + "\n")
		}

		if more := len(lines) - exceptionStackLines; more > 0 {
			fmt.Fprintf(b, "    (… %d more lines: eyedbg eval '$exception.StackTrace')\n", more)
		}
	}

	for i := range e.Inner {
		b.WriteString("    inner: " + exceptionLine(&e.Inner[i]) + "\n")
	}
}

// exceptionLine is "Type: message" on one line.
func exceptionLine(e *api.ExceptionInfo) string {
	s := firstNonEmpty(e.Type, e.ID)
	if msg := strings.Join(strings.Fields(firstNonEmpty(e.Message, e.Description)), " "); msg != "" {
		s += ": " + msg
	}

	return s
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

// writeSharing writes who holds the lease and who uses the session, when
// that matters: several clients, a policy other than free, a connected
// editor or a pending lease request.
func writeSharing(b *strings.Builder, s api.SessionInfo) {
	if s.Lease == nil {
		return
	}

	anyConnected := slices.ContainsFunc(s.Clients, func(c api.ClientInfo) bool { return c.Connected > 0 })
	if len(s.Clients) <= 1 && s.Lease.Policy == api.LeaseFree && !anyConnected && len(s.Lease.Requests) == 0 {
		return
	}

	holder := s.Lease.Holder
	if holder == "" {
		holder = "nobody"
	}

	fmt.Fprintf(b, "  lease: %s (%s)", holder, s.Lease.Policy)

	if len(s.Lease.Requests) > 0 {
		requests := make([]string, len(s.Lease.Requests))
		for i, r := range s.Lease.Requests {
			requests[i] = leaseRequestText(r)
		}

		b.WriteString("; requested by " + strings.Join(requests, ", "))
	}

	ids := make([]string, len(s.Clients))
	for i := range s.Clients {
		ids[i] = s.Clients[i].ID
		if s.Clients[i].Connected > 0 {
			ids[i] += " (connected)"
		}
	}

	fmt.Fprintf(b, "; clients: %s\n", strings.Join(ids, ", "))
}

// snapshotHeader is the first line: session, state and why.
func snapshotHeader(snap api.Snapshot) string {
	s := snap.Session

	var b strings.Builder

	lang := s.Lang
	if s.Adapter != "" {
		lang += ", " + s.Adapter
	}

	fmt.Fprintf(&b, "session %s (%s) %s", s.ID, lang, s.State)

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
	case api.StateLost:
		b.WriteString(" (eyedbgd exited while it was live)")
	case api.StateStarting:
	}

	return b.String()
}

// lostNote explains lost sessions under the sessions table.
const lostNote = "(lost: eyedbgd exited while it was live; 'eyedbg sessions --json' has its recording; 'eyedbg stop -s ID' forgets it)\n"

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

	if len(list) == 0 {
		return nil
	}

	var (
		table strings.Builder
		lost  bool
	)

	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "ID\tLANG\tADAPTER\tSTATE\tLEASE\tCONNECTED\tPROGRAM\n")

	for i := range list {
		s := &list[i]
		lost = lost || s.State == api.StateLost

		state := string(s.State)
		if s.State == api.StateStopped && s.Stop != nil {
			state += " (" + s.Stop.Reason + ")"
		}

		if s.State == api.StateExited && s.ExitCode != nil {
			state += fmt.Sprintf(" (code %d)", *s.ExitCode)
		}

		holder := "-"
		if s.Lease != nil && s.Lease.Holder != "" {
			holder = s.Lease.Holder
		}

		adapter := s.Adapter
		if adapter == "" {
			adapter = "-"
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Lang, adapter, state, holder, connectedClients(s.Clients), s.Program)
	}

	_ = tw.Flush() // writes to a strings.Builder cannot fail

	var b strings.Builder
	for line := range strings.Lines(table.String()) {
		b.WriteString(strings.TrimRight(line, " \n") + "\n")
	}

	if lost {
		b.WriteString(lostNote)
	}

	return writeText(w, b.String())
}

// connectedClients lists the clients with an editor connection open,
// comma-separated, or "-".
func connectedClients(clients []api.ClientInfo) string {
	var ids []string

	for i := range clients {
		if clients[i].Connected > 0 {
			ids = append(ids, clients[i].ID)
		}
	}

	if len(ids) == 0 {
		return "-"
	}

	return strings.Join(ids, ",")
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

	var b strings.Builder

	b.WriteString(res.Value)

	if res.Type != "" {
		b.WriteString("  (" + res.Type + ")")
	}

	b.WriteString("\n")
	writeVarList(&b, res.Children, res.More, 1)
	writeBudgetHint(&b, res.Truncated, 1)

	return writeText(w, b.String())
}

// writeOutput writes output chunks; a hint about chunks left out goes to
// stderr, so the text on stdout stays exactly what the program printed.
func writeOutput(w, stderr io.Writer, res api.OutputResult, tail, asJSON bool) error {
	if asJSON {
		if res.Lines == nil {
			res.Lines = []api.OutputLine{}
		}

		return writeJSON(w, struct {
			Schema int              `json:"schema"`
			Output []api.OutputLine `json:"output"`
			More   int              `json:"more,omitempty"`
		}{jsonSchemaVersion, res.Lines, res.More})
	}

	var b strings.Builder
	for _, l := range res.Lines {
		b.WriteString(l.Text)
	}

	out := b.String()
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}

	if err := writeText(w, out); err != nil {
		return err
	}

	switch {
	case res.More == 0:
		return nil
	case tail:
		return writeText(stderr, fmt.Sprintf("(%d earlier chunks left out to keep the output under 512 KiB)\n", res.More))
	default:
		last := 0
		if n := len(res.Lines); n > 0 {
			last = res.Lines[n-1].Seq
		}

		return writeText(stderr, fmt.Sprintf("(%d more chunks: eyedbg output --since %d)\n", res.More, last))
	}
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

	for i := range bps {
		bp := &bps[i]
		fmt.Fprintf(&b, "%d  %s  %s", bp.ID, bp.Owner, breakpointSpot(bp, base))

		if bp.Line != bp.RequestedLine {
			fmt.Fprintf(&b, " (requested line %d)", bp.RequestedLine)
		}

		if bp.Temporary {
			b.WriteString(" (run-until, temporary)")
		}

		if bp.Editor {
			b.WriteString(" (editor)")
		}

		if bp.Verified {
			b.WriteString("  verified")
		} else {
			b.WriteString("  pending")

			if bp.Message != "" {
				b.WriteString(": " + bp.Message)
			}
		}

		if bp.Hits > 0 {
			fmt.Fprintf(&b, "  hits %d", bp.Hits)
		}

		if bp.Note != "" {
			b.WriteString("  note: " + bp.Note)
		}

		b.WriteString("\n")
	}

	return writeText(w, b.String())
}

// breakpointSpot is where a breakpoint is and what it does: func:NAME, or
// file:line with the anchor it was found by; then its condition, hit count
// and log message.
func breakpointSpot(bp *api.Breakpoint, base string) string {
	s := "func:" + bp.Function

	if bp.Function == "" {
		s = location(bp.File, bp.Line, base)
		if bp.Anchor != "" {
			s += ` @"` + bp.Anchor + `"`
		}
	}

	if bp.Condition != "" {
		s += " if " + bp.Condition
	}

	if bp.HitCondition != "" {
		s += " hit " + bp.HitCondition
	}

	if bp.LogMessage != "" {
		s += ` log "` + bp.LogMessage + `"`
	}

	return s
}

// writeRemoved reports what 'bp rm' removed and kept.
func writeRemoved(w io.Writer, res api.BreakpointRemoveResult, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema  int `json:"schema"`
			Removed int `json:"removed"`
			Kept    int `json:"kept"`
		}{jsonSchemaVersion, res.Removed, res.Kept})
	}

	msg := fmt.Sprintf("removed %d breakpoint(s)", res.Removed)
	if res.Kept > 0 {
		msg += fmt.Sprintf("; %d of other clients kept (--force removes them)", res.Kept)
	}

	return writeText(w, msg+"\n")
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
