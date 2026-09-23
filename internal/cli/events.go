// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// eventsFlags are the flags of 'eyedbg events'.
type eventsFlags struct {
	since, limit int
	kinds        []string
	wait         bool
	timeout      time.Duration
}

func newEventsCommand(info version.Info, g *globals) *cobra.Command {
	var f eventsFlags

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show what happened in a session, or wait for it",
		Long: `Show a session's event log: what every client and the program did, one line per event, oldest
first, each with its sequence number: started, client joined, lease changes, execution commands
(who ran what), stops, program output, breakpoints added/removed/changed, threads, exit and end.
Use it to see what another client (a human in an IDE, another agent) did, or to follow the session.

Without --since, shows the newest --limit events (default 100). --since N shows the events after N,
oldest first: pass the last sequence number you have seen (the "next:" line gives it) to read
on. --kind keeps some kinds only (comma-separated: started, client, lease, exec, continued,
stopped, output, breakpoint, thread, exited, ended).

--wait blocks until a matching event exists (after --since, or from now without it), at most
--timeout (default 30s): a timeout is not an error, it prints "(no new events within ...)". The
log keeps the last 10000 events (8 MiB); older ones are dropped and a line says how many. Output
text is cut to one line per event (the whole chunk, cut at 1000 characters, in --json), and the
result to --budget tokens.

Read-only; never affects the program. --json: {"schema", "events": [{"seq", "time", "kind",
...}], "latest", "dropped", "more", "timedOut"}.` + sessionHelp,
		Example: `  eyedbg events                          # the newest 100 events
  eyedbg events --since 42                # what happened after event 42
  eyedbg events --wait --kind lease       # block until the lease changes
  eyedbg events --since 42 --wait --json  # follow the session`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params, err := f.params(g, cmd.Flags().Changed("since"))
			if err != nil {
				return err
			}

			timeout := daemonCallTimeout
			if f.wait {
				timeout = f.timeout + callSlack
			}

			var res api.EventsResult
			if err := call(cmd, info, timeout, api.MethodEvents, params, &res); err != nil {
				return err
			}

			return writeEvents(cmd.OutOrStdout(), res, eventsView{newest: params.Newest, timeout: f.timeout, wait: f.wait}, g.json, workDir())
		},
	}

	fl := cmd.Flags()
	fl.IntVar(&f.since, "since", 0, "show events after this sequence number, oldest first (default: the newest events)")
	fl.IntVar(&f.limit, "limit", 100, "most events to show; 0 for the maximum (1000)")
	fl.StringSliceVar(&f.kinds, "kind", nil, "only these kinds (comma-separated)")
	fl.BoolVar(&f.wait, "wait", false, "wait until a matching event exists, at most --timeout")
	fl.DurationVar(&f.timeout, "timeout", defaultExecTimeout, "longest time --wait waits")

	return cmd
}

// params validates the flags and builds the request.
func (f *eventsFlags) params(g *globals, sinceGiven bool) (api.EventsParams, error) {
	if f.since < 0 || f.limit < 0 {
		return api.EventsParams{}, errors.New("--since and --limit take a number from 0")
	}

	p := api.EventsParams{SessionRef: g.ref(), Since: f.since, Limit: f.limit, Budget: g.budget}
	if p.Limit == 0 {
		p.Limit = api.MaxEventsLimit
	}

	for _, k := range f.kinds {
		if !slices.Contains(api.EventKinds(), api.EventKind(k)) {
			return api.EventsParams{}, fmt.Errorf("invalid --kind %q: want %s", k, api.EventKindNames())
		}

		p.Kinds = append(p.Kinds, api.EventKind(k))
	}

	switch {
	case f.wait:
		p.Wait = api.Duration(f.timeout)
		if !sinceGiven {
			p.Since = -1
		}
	case !sinceGiven:
		p.Newest = true
	}

	return p, nil
}

// eventsView is how the events were asked for, for the text output.
type eventsView struct {
	newest  bool
	wait    bool
	timeout time.Duration
}

func writeEvents(w io.Writer, res api.EventsResult, view eventsView, asJSON bool, base string) error {
	if asJSON {
		if res.Events == nil {
			res.Events = []api.Event{}
		}

		return writeJSON(w, struct {
			Schema           int `json:"schema"`
			api.EventsResult     //nolint:embeddedstructfieldcheck // "schema" leads the JSON object.
		}{jsonSchemaVersion, res})
	}

	var b strings.Builder

	if res.Dropped > 0 {
		fmt.Fprintf(&b, "(%d older events were dropped: the log keeps only recent events)\n", res.Dropped)
	}

	if view.newest && res.More > 0 {
		fmt.Fprintf(&b, "(%d earlier events not shown: 'eyedbg events --since 0' pages from the oldest)\n", res.More)
	}

	for i := range res.Events {
		fmt.Fprintf(&b, "%d  %s\n", res.Events[i].Seq, describeEvent(&res.Events[i], base))
	}

	next := res.Latest

	switch {
	case len(res.Events) > 0:
		next = res.Events[len(res.Events)-1].Seq
	case view.wait && res.TimedOut:
		fmt.Fprintf(&b, "(no new events within %s)\n", view.timeout)
	default:
		b.WriteString("(no events)\n")
	}

	fmt.Fprintf(&b, "next: eyedbg events --since %d", next)

	if !view.newest && res.More > 0 {
		fmt.Fprintf(&b, "  (%d more)", res.More)
	}

	b.WriteString("\n")

	return writeText(w, b.String())
}

// describeEvent renders one event as a short sentence.
func describeEvent(e *api.Event, base string) string {
	switch e.Kind {
	case api.EventStarted:
		s := "started by " + e.Client + ": " + location(e.Program, 0, base)
		if e.Lease != nil {
			s += " (lease policy " + string(e.Lease.Policy) + ")"
		}

		return s
	case api.EventClient:
		return "client " + e.Client + " joined"
	case api.EventLease:
		return describeLease(e)
	case api.EventExec:
		return e.Client + ": " + execVerb(e.Action) + threadSuffix(e.ThreadID)
	case api.EventContinued:
		return "continued" + threadSuffix(e.ThreadID)
	case api.EventStopped:
		return describeStop(e.Stop)
	case api.EventOutput:
		return describeOutput(e)
	case api.EventBreakpoint:
		return describeBreakpoint(e, base)
	case api.EventThread:
		return fmt.Sprintf("thread %d %s", e.ThreadID, e.Reason)
	case api.EventExited:
		if e.ExitCode != nil {
			return fmt.Sprintf("exited with code %d", *e.ExitCode)
		}

		return "exited"
	case api.EventEnded:
		return "ended: " + e.Reason
	default:
		return string(e.Kind)
	}
}

func describeLease(e *api.Event) string {
	from := ""
	if e.Previous != "" {
		from = " from " + e.Previous
	}

	holder := ""
	if e.Lease != nil {
		holder = e.Lease.Holder
	}

	switch e.Action {
	case "take":
		return "lease: " + e.Client + " took it" + from
	case "auto":
		return "lease: " + e.Client + " took it" + from + " (auto)"
	case "force":
		return "lease: " + e.Client + " took it" + from + " (forced)"
	case "grant":
		return "lease: " + e.Client + " gave it to " + holder
	case "release":
		return "lease: " + e.Client + " released it"
	case "policy":
		policy := ""
		if e.Lease != nil {
			policy = string(e.Lease.Policy)
		}

		return "lease: " + e.Client + " set the policy to " + policy
	default:
		return "lease: " + e.Client + " " + e.Action
	}
}

// execVerb is the CLI command for an exec action.
func execVerb(action string) string {
	switch action {
	case "stepIn":
		return "step-in"
	case "stepOut":
		return "step-out"
	case "runUntil":
		return "run-until"
	default:
		return action
	}
}

func threadSuffix(id int) string {
	if id == 0 {
		return ""
	}

	return fmt.Sprintf(" (thread %d)", id)
}

// maxEventLine cuts the text shown for an output or stop event.
const maxEventLine = 200

func describeStop(stop *api.StopInfo) string {
	if stop == nil {
		return "stopped"
	}

	s := "stopped: " + stop.Reason

	if detail := firstNonEmpty(stop.Text, stop.Description); detail != "" && detail != stop.Reason {
		s += " (" + cutLine(detail) + ")"
	}

	return s + fmt.Sprintf(", thread %d", stop.ThreadID)
}

// describeOutput shows an output chunk's first line and how many more it
// has.
func describeOutput(e *api.Event) string {
	lines := strings.Split(strings.TrimRight(e.Text, "\r\n"), "\n")

	s := "output (" + e.Category + "): " + cutLine(strings.TrimRight(lines[0], "\r"))
	if more := len(lines) - 1; more > 0 {
		s += fmt.Sprintf(" (+%d line", more)
		if more > 1 {
			s += "s"
		}

		s += ")"
	}

	if e.Truncated {
		s += " (cut)"
	}

	return s
}

// cutLine cuts s to maxEventLine characters.
func cutLine(s string) string {
	if r := []rune(s); len(r) > maxEventLine {
		return string(r[:maxEventLine]) + "…"
	}

	return s
}

func describeBreakpoint(e *api.Event, base string) string {
	bp := e.Breakpoint
	if bp == nil {
		return "bp " + e.Action
	}

	head := "bp " + strconv.Itoa(bp.ID) + " " + e.Action

	switch e.Action {
	case "removed":
		s := head + " by " + e.Client
		if bp.Owner != e.Client {
			s += " (owner " + bp.Owner + ")"
		}

		return s
	case "added":
		return head + " by " + e.Client + ": " + breakpointWhere(bp, base)
	default:
		if e.Client != "" {
			head += " by " + e.Client
		}

		s := head + ": " + breakpointWhere(bp, base)
		if bp.Verified {
			return s + " verified"
		}

		if bp.Message != "" {
			return s + " pending: " + bp.Message
		}

		return s + " pending"
	}
}

// breakpointWhere is "file:line[ if cond][ (run-until)]".
func breakpointWhere(bp *api.Breakpoint, base string) string {
	s := location(bp.File, bp.Line, base)
	if bp.Condition != "" {
		s += " if " + bp.Condition
	}

	if bp.Temporary {
		s += " (run-until)"
	}

	return s
}
