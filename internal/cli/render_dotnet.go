// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
)

// writeDotnetPs prints 'eyedbg dotnet ps': a PID NAME SESSION table
// (nothing when empty), or {"schema", "processes"}.
func writeDotnetPs(w io.Writer, rows []psRow, asJSON bool) error {
	if asJSON {
		if rows == nil {
			rows = []psRow{}
		}

		return writeJSON(w, struct {
			Schema    int     `json:"schema"`
			Processes []psRow `json:"processes"`
		}{jsonSchemaVersion, rows})
	}

	if len(rows) == 0 {
		return nil
	}

	var table strings.Builder

	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	_, _ = io.WriteString(tw, "PID\tNAME\tSESSION\n")

	for _, r := range rows {
		session := r.Session
		if session == "" {
			session = "-"
		}

		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\n", r.PID, displayName(r.Name), session)
	}

	_ = tw.Flush() // writes to a strings.Builder cannot fail

	var b strings.Builder
	for line := range strings.Lines(table.String()) {
		b.WriteString(strings.TrimRight(line, " \n") + "\n")
	}

	return writeText(w, b.String())
}

// displayName is a process name fit for a terminal ("?" when unknown).
func displayName(name string) string {
	if name == "" {
		return "?"
	}

	return printable(name)
}

// printable is s fit for a terminal: control characters (C0, DEL, C1) and
// invalid UTF-8 become '?'. Process names and counter names and units come
// from other processes and may carry escape sequences.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}

		return r
	}, strings.ToValidUTF8(s, "?"))
}

// counterWatchLine is a --watch --json line.
type counterWatchLine struct {
	Schema    int                `json:"schema"`
	ElapsedMs int64              `json:"elapsedMs"`
	Counters  []counterWatchItem `json:"counters"`
}

type counterWatchItem struct {
	Name  string  `json:"name"`
	Unit  string  `json:"unit"`
	Kind  string  `json:"kind"`
	Value float64 `json:"value"`
}

// writeCounterSample prints one --watch sample: "+1.1s name=value…" or a
// JSON document.
func writeCounterSample(w io.Writer, s dotnet.CounterSample, asJSON bool) error {
	values := orderCounters(s.Counters)

	if asJSON {
		items := make([]counterWatchItem, 0, len(values))
		for _, v := range values {
			items = append(items, counterWatchItem{Name: v.Name, Unit: v.Unit, Kind: v.Kind, Value: v.Value})
		}

		return writeJSON(w, counterWatchLine{Schema: jsonSchemaVersion, ElapsedMs: s.ElapsedMs, Counters: items})
	}

	var b strings.Builder

	b.WriteString("+" + formatSeconds(s.ElapsedMs))

	for _, v := range values {
		b.WriteString("  " + printable(v.Name) + "=" + formatNumber(v.Value) + printable(v.Unit))
	}

	b.WriteString("\n")

	return writeText(w, b.String())
}

// writeCountersSummary prints the counters summary: a header, one line
// per counter, notes on missed intervals or the process exiting.
func writeCountersSummary(w io.Writer, s countersSummary, asJSON bool) error {
	if asJSON {
		return writeJSON(w, s)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s: %s, %d sample%s over %s (every %ds)\n", s.Target.describe(), s.Provider,
		s.Samples, plural(s.Samples), formatSeconds(s.DurationMs), s.IntervalSec)

	names := make([]string, len(s.Counters))
	width := 0

	for i, c := range s.Counters {
		names[i] = printable(c.Name)
		width = max(width, len(names[i]))
	}

	for i, c := range s.Counters {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, names[i], counterValueText(c))
	}

	if s.MissedIntervals > 0 {
		fmt.Fprintf(&b, "note: %d interval%s brought no sample (the process was paused or busy)\n", s.MissedIntervals, plural(s.MissedIntervals))
	}

	if s.EndReason == dotnet.EndExited {
		b.WriteString("note: the process exited during the collection\n")
	}

	return writeText(w, b.String())
}

// counterValueText is "<last><unit>" plus "(min–max)" when it moved for
// a gauge, "total <n><unit> (<rate><unit>/s)" for a sum.
func counterValueText(c counterStat) string {
	unit := printable(c.Unit)
	if unit != "" && unit != "%" {
		unit = " " + unit
	}

	if c.Kind == dotnet.KindSum && c.Total != nil && c.PerSecond != nil {
		return "total " + formatNumber(*c.Total) + unit + " (" + formatNumber(*c.PerSecond) + unit + "/s)"
	}

	if c.Last == nil {
		return "-"
	}

	s := formatNumber(*c.Last) + unit
	if c.Min != nil && c.Max != nil && formatNumber(*c.Min) != formatNumber(*c.Max) {
		s += " (" + formatNumber(*c.Min) + "–" + formatNumber(*c.Max) + ")"
	}

	return s
}

// formatNumber prints v compactly: integers as such, else no decimals
// from 100 up, 2 decimals from 1 up, 3 significant digits below 1.
func formatNumber(v float64) string {
	a := math.Abs(v)

	switch {
	case v == math.Trunc(v) && a < 1e15, a >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case a >= 1:
		return trimZeros(strconv.FormatFloat(v, 'f', 2, 64))
	default:
		return strconv.FormatFloat(v, 'g', 3, 64)
	}
}

func trimZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}

	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// formatSeconds prints milliseconds as seconds with at most one decimal.
func formatSeconds(ms int64) string {
	return trimZeros(strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64)) + "s"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}

	return "s"
}
