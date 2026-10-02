// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// Text renderers of the compose commands. Like the others, --json output is
// the API value plus "schema"; every renderer takes the time "now" it needs
// as a parameter, so goldens are stable.

// kafkaNoteAfter is how long a stop lasts before the output mentions Kafka
// consumers: max.poll.interval.ms is 5 minutes by default, and a consumer
// stopped for most of it is about to leave its group.
const kafkaNoteAfter = 4 * time.Minute

// stoppedFor is how long a container session has been stopped at now,
// rounded to seconds; false for any other session.
func stoppedFor(s *api.SessionInfo, now time.Time) (time.Duration, bool) {
	if s.Container == nil || s.State != api.StateStopped || s.StoppedAt == nil {
		return 0, false
	}

	return max(now.Sub(*s.StoppedAt), 0).Round(time.Second), true
}

// longStopNotes are the notes a container session stopped for long enough
// earns: its healthcheck has failed by now, and a Kafka consumer is about to
// leave its group. Neither is shown for a stop that is short.
func longStopNotes(s *api.SessionInfo, now time.Time) []string {
	d, ok := stoppedFor(s, now)
	if !ok {
		return nil
	}

	var notes []string

	if after := time.Duration(s.Container.UnhealthyAfter); after > 0 && d >= after {
		notes = append(notes, fmt.Sprintf("stopped %s, past the %s docker's healthcheck allows: container %s is unhealthy by now, "+
			"and its service is down until it continues", d, after, s.Container.Name))
	}

	if d >= kafkaNoteAfter {
		notes = append(notes, fmt.Sprintf("stopped %s: a Kafka consumer in this service leaves its group after max.poll.interval.ms "+
			"(5 minutes by default) and rejoins when it continues", d))
	}

	return notes
}

// memberName is how a group member is named in the output: its compose
// service, else its session id.
func memberName(s *api.SessionInfo) string {
	if s.Container != nil && s.Container.Service != "" {
		return s.Container.Service
	}

	return s.ID
}

// writeTable renders rows as aligned columns, two spaces apart, with every
// line's trailing spaces trimmed and each line indented by indent spaces.
func writeTable(b *strings.Builder, indent int, rows [][]string) {
	var table strings.Builder

	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		_, _ = io.WriteString(tw, strings.Join(r, "\t")+"\n")
	}

	_ = tw.Flush() // writes to a strings.Builder cannot fail

	for line := range strings.Lines(table.String()) {
		b.WriteString(strings.Repeat(" ", indent) + strings.TrimRight(line, " \n") + "\n")
	}
}

// jsonErr is the error a JSON document of a command shows for a member.
func jsonErr(err error) *api.Error {
	if err == nil {
		return nil
	}

	return toErrorDoc(err)
}

// bpOutcome is one member's answer to adding one breakpoint.
type bpOutcome struct {
	Location   string          `json:"location"`
	Breakpoint *api.Breakpoint `json:"breakpoint,omitempty"`
	// Skipped says why the breakpoint isn't for this member (its file is
	// outside the member's path map).
	Skipped string     `json:"skipped,omitempty"`
	Error   *api.Error `json:"error,omitempty"`
}

// text is what a member did with the breakpoint, on one line.
func (o bpOutcome) text(base string) string {
	switch {
	case o.Skipped != "":
		return "skipped: " + o.Skipped
	case o.Error != nil:
		return "failed: " + errorLine(o.Error)
	case o.Breakpoint == nil:
		return "no answer"
	}

	bp := o.Breakpoint
	s := "bp " + strconv.Itoa(bp.ID) + " " + breakpointSpot(bp, base)

	if bp.Line != bp.RequestedLine && bp.RequestedLine > 0 {
		s += " (requested line " + strconv.Itoa(bp.RequestedLine) + ")"
	}

	switch {
	case bp.Verified:
		s += " verified"
	case bp.Message != "":
		s += " pending: " + bp.Message
	default:
		s += " pending"
	}

	return s
}

// errorLine is an error on one line: message, code and hint.
func errorLine(e *api.Error) string {
	s := strings.Join(strings.Fields(e.Message), " ") + " [" + string(e.Code) + "]"
	if e.Hint != "" {
		s += " (" + strings.Join(strings.Fields(e.Hint), " ") + ")"
	}

	return s
}

// attachedMember is one service of 'compose attach' in the output.
type attachedMember struct {
	Service   string           `json:"service,omitempty"`
	Container string           `json:"container"`
	Session   *api.SessionInfo `json:"session,omitempty"`
	Skipped   string           `json:"skipped,omitempty"`
	Error     *api.Error       `json:"error,omitempty"`
	// Breakpoints are the --bp breakpoints, in order, as this member took
	// them.
	Breakpoints []bpOutcome `json:"breakpoints,omitempty"`
}

// attachedCount is how many members have a session.
func attachedCount(members []attachedMember) int {
	n := 0

	for i := range members {
		if members[i].Session != nil {
			n++
		}
	}

	return n
}

// pauseNote says what a stop at a breakpoint does to a service of a
// stack; shown once, by 'compose attach'.
const pauseNote = "note: a breakpoint hit stops the whole service: its HTTP callers time out, docker's healthcheck turns the container " +
	"unhealthy after about retries x interval + timeout (shown per service above), and a Kafka consumer leaves its group after " +
	"max.poll.interval.ms (5 minutes by default). Keep stops short; 'eyedbg compose wait' shows how long each has been stopped."

// writeComposeAttach renders the result of 'compose attach'.
func writeComposeAttach(w io.Writer, group string, members []attachedMember, asJSON bool, base string) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema  int              `json:"schema"`
			Group   string           `json:"group"`
			Members []attachedMember `json:"members"`
		}{jsonSchemaVersion, group, members})
	}

	var b strings.Builder

	n := attachedCount(members)
	fmt.Fprintf(&b, "group %s: attached to %d of %d service(s)\n", group, n, len(members))

	rows := make([][]string, len(members))

	for i := range members {
		rows[i] = attachRow(&members[i])
	}

	writeTable(&b, 2, rows)

	if n == 0 {
		return writeText(w, b.String())
	}

	writeAttachMaps(&b, members)
	writeAttachBreakpoints(&b, members, base)

	b.WriteString(pauseNote + "\n")
	b.WriteString("next: eyedbg compose wait -g " + group + "  |  eyedbg compose bp add FILE:LINE -g " + group + "  |  eyedbg compose stop -g " + group + "\n")

	return writeText(w, b.String())
}

// attachRow is a member's line of the attach table.
func attachRow(m *attachedMember) []string {
	name := firstNonEmpty(m.Service, m.Container)

	switch {
	case m.Session != nil:
		detail := "container " + m.Container
		if c := m.Session.Container; c != nil && c.UnhealthyAfter > 0 {
			detail += "; its healthcheck fails after " + time.Duration(c.UnhealthyAfter).String() + " of a stop"
		}

		return []string{name, m.Session.ID, "attached", detail}
	case m.Skipped != "":
		return []string{name, "-", "skipped", m.Skipped}
	case m.Error != nil:
		return []string{name, "-", "failed", errorLine(m.Error)}
	default:
		return []string{name, "-", "no answer", ""}
	}
}

// writeAttachMaps says which path maps the attached sessions use: one line
// when they share one, else one per service.
func writeAttachMaps(b *strings.Builder, members []attachedMember) {
	var (
		seen    []string
		perName = map[string]string{}
	)

	for i := range members {
		m := &members[i]
		if m.Session == nil || m.Session.Container == nil {
			continue
		}

		text := mapText(m.Session.Container.Map)
		perName[firstNonEmpty(m.Service, m.Container)] = text

		if !slices.Contains(seen, text) {
			seen = append(seen, text)
		}
	}

	switch {
	case len(seen) == 1:
		b.WriteString("path map: " + seen[0] + "\n")
	case len(seen) > 1:
		b.WriteString("path maps:\n")

		for i := range members {
			if name := firstNonEmpty(members[i].Service, members[i].Container); perName[name] != "" {
				b.WriteString("  " + name + ": " + perName[name] + "\n")
			}
		}
	}
}

// mapText renders a path map, or says there is none.
func mapText(m []api.PathMapping) string {
	if len(m) == 0 {
		return "none (line breakpoints are refused; add --map REMOTE=LOCAL)"
	}

	parts := make([]string, len(m))
	for i, p := range m {
		parts[i] = p.Remote + "=" + p.Local
	}

	return strings.Join(parts, ", ")
}

// writeAttachBreakpoints lists, per --bp, what each attached service did
// with it.
func writeAttachBreakpoints(b *strings.Builder, members []attachedMember, base string) {
	var locations []string

	for i := range members {
		for _, o := range members[i].Breakpoints {
			if !slices.Contains(locations, o.Location) {
				locations = append(locations, o.Location)
			}
		}
	}

	for _, loc := range locations {
		b.WriteString("breakpoint " + loc + ":\n")

		var rows [][]string

		for i := range members {
			for _, o := range members[i].Breakpoints {
				if o.Location == loc {
					rows = append(rows, []string{firstNonEmpty(members[i].Service, members[i].Container), o.text(base)})
				}
			}
		}

		writeTable(b, 2, rows)
	}
}

// groupWaitView is how a 'compose wait' was asked for.
type groupWaitView struct {
	timeout time.Duration
}

// writeGroupWait renders the result of 'compose wait'.
func writeGroupWait(w io.Writer, res api.GroupWaitResult, view groupWaitView, asJSON bool, base string, now time.Time) error {
	if asJSON {
		if res.Stopped == nil {
			res.Stopped = []api.GroupStop{}
		}

		return writeJSON(w, struct {
			Schema              int `json:"schema"`
			api.GroupWaitResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, res})
	}

	var b strings.Builder

	switch {
	case res.Session != nil:
		s := &res.Session.Session
		b.WriteString("group " + res.Group + ": " + s.ID + " (" + memberName(s) + ") " + waitVerb(s) + "\n")

		if err := writeSnapshotAt(&b, *res.Session, false, base, now); err != nil {
			return err
		}
	case res.TimedOut && len(res.Stopped) > 0:
		fmt.Fprintf(&b, "group %s: no other member stopped within %s (wait again, or 'eyedbg pause -s ID' one)\n", res.Group, view.timeout)
	case res.TimedOut:
		fmt.Fprintf(&b, "group %s: nothing stopped within %s; every member is still running (wait again, or 'eyedbg pause -s ID' one)\n", res.Group, view.timeout)
	case res.Ended:
		b.WriteString("group " + res.Group + ": every member has ended ('eyedbg compose stop -g " + res.Group + "' clears them)\n")
	}

	writeAlsoStopped(&b, res, now)

	if res.Session != nil && res.Session.Session.State == api.StateStopped {
		id := res.Session.Session.ID
		b.WriteString("to act on it: eyedbg vars -s " + id + ", eyedbg continue -s " + id + "; 'eyedbg compose wait --new -g " + res.Group + "' waits for the next stop\n")
	}

	return writeText(w, b.String())
}

// waitVerb is what happened to the member 'compose wait' answered with:
// "stopped: breakpoint", "exited".
func waitVerb(s *api.SessionInfo) string {
	verb := string(s.State)
	if s.State == api.StateStopped && s.Stop != nil {
		verb += ": " + s.Stop.Reason
	}

	return verb
}

// writeAlsoStopped lists the other members that are stopped, with how long
// each has been.
func writeAlsoStopped(b *strings.Builder, res api.GroupWaitResult, now time.Time) {
	if len(res.Stopped) == 0 {
		return
	}

	if res.Session == nil {
		b.WriteString("already stopped (before this wait):\n")
	} else {
		b.WriteString("also stopped:\n")
	}

	rows := make([][]string, len(res.Stopped))

	for i, st := range res.Stopped {
		name := st.SessionID
		if st.Service != "" {
			name = st.Service + " (" + st.SessionID + ")"
		}

		rows[i] = []string{name, st.Reason, "stopped " + max(now.Sub(st.StoppedAt), 0).Round(time.Second).String()}
	}

	writeTable(b, 2, rows)
}

// groupEventsView is how a 'compose events' was asked for.
type groupEventsView struct {
	group   string
	newest  bool
	wait    bool
	timeout time.Duration
}

// writeGroupEvents renders the result of 'compose events'.
func writeGroupEvents(w io.Writer, res api.GroupEventsResult, view groupEventsView, asJSON bool, base string) error {
	if asJSON {
		if res.Events == nil {
			res.Events = []api.GroupEvent{}
		}

		return writeJSON(w, struct {
			Schema                int `json:"schema"`
			api.GroupEventsResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, res})
	}

	var b strings.Builder

	if res.Dropped > 0 {
		fmt.Fprintf(&b, "(%d older events were dropped: the logs keep only recent events)\n", res.Dropped)
	}

	labels := eventLabels(res.Events)

	for i := range res.Events {
		e := &res.Events[i]
		fmt.Fprintf(&b, "%s  %d  %s\n", labels[i], e.Event.Seq, describeEvent(&e.Event, base))
	}

	switch {
	case len(res.Events) > 0:
	case view.wait && res.TimedOut:
		fmt.Fprintf(&b, "(no new events within %s)\n", view.timeout)
	default:
		b.WriteString("(no events)\n")
	}

	b.WriteString("next: eyedbg compose events -g " + view.group + " --since " + res.Cursor)

	if res.More > 0 {
		fmt.Fprintf(&b, "  (%d more)", res.More)
	}

	b.WriteString("\n")

	if view.newest && res.More > 0 {
		b.WriteString("(--newest shows only the latest events: reading on from this cursor starts at the older ones left out, and may show these again)\n")
	}

	return writeText(w, b.String())
}

// eventLabels names each event's member: its service, or its session id
// when it has no service or another session of the result has the same one.
func eventLabels(events []api.GroupEvent) []string {
	sessions := map[string]map[string]bool{}

	for i := range events {
		e := &events[i]
		if sessions[e.Service] == nil {
			sessions[e.Service] = map[string]bool{}
		}

		sessions[e.Service][e.SessionID] = true
	}

	labels := make([]string, len(events))

	for i := range events {
		e := &events[i]

		switch {
		case e.Service == "":
			labels[i] = e.SessionID
		case len(sessions[e.Service]) > 1:
			labels[i] = e.Service + "/" + e.SessionID
		default:
			labels[i] = e.Service
		}
	}

	return labels
}

// bpMember is one member's answer to 'compose bp add'.
type bpMember struct {
	bpOutcome

	Service   string `json:"service,omitempty"`
	SessionID string `json:"sessionId"`
}

// writeComposeBpAdd renders the result of 'compose bp add'.
func writeComposeBpAdd(w io.Writer, group string, members []bpMember, asJSON bool, base string) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema  int        `json:"schema"`
			Group   string     `json:"group"`
			Members []bpMember `json:"members"`
		}{jsonSchemaVersion, group, members})
	}

	rows := make([][]string, len(members))
	for i, m := range members {
		rows[i] = []string{firstNonEmpty(m.Service, m.SessionID), m.SessionID, m.text(base)}
	}

	var b strings.Builder

	writeTable(&b, 0, rows)

	return writeText(w, b.String())
}

// bpListMember is one member's breakpoints, for 'compose bp ls'.
type bpListMember struct {
	Service     string           `json:"service,omitempty"`
	SessionID   string           `json:"sessionId"`
	Breakpoints []api.Breakpoint `json:"breakpoints"`
	Error       *api.Error       `json:"error,omitempty"`
}

// writeComposeBpList renders the result of 'compose bp ls'.
func writeComposeBpList(w io.Writer, group string, members []bpListMember, asJSON bool, base string) error {
	if asJSON {
		// A member with no breakpoints is an empty array, not null; the
		// caller's members are not changed.
		members = slices.Clone(members)

		for i := range members {
			if members[i].Breakpoints == nil {
				members[i].Breakpoints = []api.Breakpoint{}
			}
		}

		return writeJSON(w, struct {
			Schema  int            `json:"schema"`
			Group   string         `json:"group"`
			Members []bpListMember `json:"members"`
		}{jsonSchemaVersion, group, members})
	}

	var b strings.Builder

	for _, m := range members {
		b.WriteString(firstNonEmpty(m.Service, m.SessionID) + " (" + m.SessionID + "):\n")

		switch {
		case m.Error != nil:
			b.WriteString("  failed: " + errorLine(m.Error) + "\n")
		case len(m.Breakpoints) == 0:
			b.WriteString("  (none)\n")
		}

		var lines strings.Builder
		if err := writeBreakpoints(&lines, m.Breakpoints, false, base); err != nil {
			return err
		}

		for line := range strings.Lines(lines.String()) {
			b.WriteString("  " + line)
		}
	}

	return writeText(w, b.String())
}

// bpRemoveMember is one member's answer to 'compose bp rm all'.
type bpRemoveMember struct {
	Service   string     `json:"service,omitempty"`
	SessionID string     `json:"sessionId"`
	Removed   int        `json:"removed"`
	Kept      int        `json:"kept"`
	Error     *api.Error `json:"error,omitempty"`
}

// writeComposeBpRemove renders the result of 'compose bp rm all'.
func writeComposeBpRemove(w io.Writer, group string, members []bpRemoveMember, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema  int              `json:"schema"`
			Group   string           `json:"group"`
			Members []bpRemoveMember `json:"members"`
		}{jsonSchemaVersion, group, members})
	}

	rows := make([][]string, len(members))

	for i, m := range members {
		text := fmt.Sprintf("removed %d breakpoint(s)", m.Removed)

		switch {
		case m.Error != nil:
			text = "failed: " + errorLine(m.Error)
		case m.Kept > 0:
			text += fmt.Sprintf("; %d of other clients kept (--force removes them)", m.Kept)
		}

		rows[i] = []string{firstNonEmpty(m.Service, m.SessionID), m.SessionID, text}
	}

	var b strings.Builder

	writeTable(&b, 0, rows)

	return writeText(w, b.String())
}

// stopMember is one member's answer to 'compose stop'.
type stopMember struct {
	Service   string           `json:"service,omitempty"`
	SessionID string           `json:"sessionId"`
	Session   *api.SessionInfo `json:"session,omitempty"`
	Error     *api.Error       `json:"error,omitempty"`
}

// writeComposeStop renders the result of 'compose stop'.
func writeComposeStop(w io.Writer, group string, members []stopMember, asJSON bool) error {
	if asJSON {
		return writeJSON(w, struct {
			Schema  int          `json:"schema"`
			Group   string       `json:"group"`
			Members []stopMember `json:"members"`
		}{jsonSchemaVersion, group, members})
	}

	rows := make([][]string, len(members))

	for i, m := range members {
		text := "failed: no answer"

		switch {
		case m.Error != nil:
			text = "failed: " + errorLine(m.Error)
		case m.Session != nil:
			text = endedPhrase(*m.Session)
		}

		rows[i] = []string{firstNonEmpty(m.Service, m.SessionID), m.SessionID, text}
	}

	var b strings.Builder

	writeTable(&b, 0, rows)

	return writeText(w, b.String())
}
