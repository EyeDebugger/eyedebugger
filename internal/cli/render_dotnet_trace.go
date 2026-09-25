// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
)

// writeTrace prints 'eyedbg dotnet trace': what was traced, the file, and
// the summary — hottest methods, waits, GCs, allocations.
func writeTrace(w io.Writer, out traceOutput, asJSON bool) error {
	if asJSON {
		return writeJSON(w, out)
	}

	var b strings.Builder

	writeTraceHeading(&b, out)

	if out.Target != nil {
		fmt.Fprintf(&b, "%s (%s)\n", printable(out.Trace.Path), formatBytes(out.Trace.Bytes))

		retention := ""
		if out.Trace.Private {
			retention = "private to you, removed after 7 days (at most 10 kept); "
		}

		b.WriteString(retention + "open it in PerfView or with 'dotnet-trace convert'\n")
	}

	if line := missingLine(out); line != "" {
		b.WriteString(line + "\n")
	}

	if c := out.CPU; c != nil {
		writeMethods(&b, "hottest methods (exclusive: at the top of the stack, in managed code):", c.Exclusive, c.ExclusiveOmitted, c.Managed)
		writeMethods(&b, "inclusive (anywhere in the stack, in managed code):", c.Inclusive, c.InclusiveOmitted, c.Managed)
		writeMethods(&b, "waiting or in native code, at:", c.Waiting, c.WaitingOmitted, c.Other)
	}

	if out.GC != nil {
		b.WriteString(gcLine(*out.GC, out.DurationMs) + "\n")
	}

	if out.Allocations != nil {
		writeAllocations(&b, *out.Allocations)
	}

	writeTraceNotes(&b, out)

	return writeText(w, b.String())
}

// missingLine says what a readable trace doesn't have, when that is part
// of the answer (a quiet process traced for gc has no collections): one
// line, or "" when there is nothing to say.
func missingLine(out traceOutput) string {
	in := "in " + formatSeconds(out.DurationMs)

	switch {
	case out.Profile == dotnet.ProfileGC && out.GC == nil && out.Allocations == nil:
		return "no garbage collections and no allocation ticks " + in + " (less than about 100 KB allocated)"
	case out.Profile == dotnet.ProfileGC && out.GC == nil:
		return "no garbage collections " + in
	case out.Profile == dotnet.ProfileGC && out.Allocations == nil:
		return "no allocation ticks " + in + " (less than about 100 KB allocated)"
	case out.CPU != nil || out.GC != nil || out.Allocations != nil:
		if out.Profile == dotnet.ProfileCPU && out.CPU == nil {
			return "no CPU samples " + in
		}

		return ""
	case out.Target == nil:
		return "no CPU samples or GC events in this trace"
	default:
		return "no CPU samples or GC events " + in
	}
}

// writeTraceHeading writes "cpu trace of pid N (name), session S: 10s, …"
// or "trace PATH (SIZE): 10s, …".
func writeTraceHeading(b *strings.Builder, out traceOutput) {
	if out.Target != nil {
		fmt.Fprintf(b, "%s trace of %s: %s", printable(out.Profile), out.Target.describe(), formatSeconds(out.DurationMs))
	} else {
		fmt.Fprintf(b, "trace %s (%s): %s", printable(out.Trace.Path), formatBytes(out.Trace.Bytes), formatSeconds(out.DurationMs))
	}

	if c := out.CPU; c != nil {
		fmt.Fprintf(b, ", %s sample%s on %d thread%s (%s in managed code, %s waiting or in native code)",
			formatCount(c.Samples), plural64(c.Samples), c.Threads, plural(c.Threads), formatCount(c.Managed), formatCount(c.Other))
	}

	b.WriteString("\n")
}

// writeMethods writes a method table: samples, their share of total, the
// method and its module; nothing when the list is empty.
func writeMethods(b *strings.Builder, title string, rows []dotnet.MethodSamples, omitted int, total int64) {
	if len(rows) == 0 {
		return
	}

	b.WriteString(title + "\n")

	samplesW := len("SAMPLES")
	for _, r := range rows {
		samplesW = max(samplesW, len(formatCount(r.Samples)))
	}

	fmt.Fprintf(b, "  %*s  %6s  METHOD\n", samplesW, "SAMPLES", "%")

	for _, r := range rows {
		module := ""
		if r.Module != "" {
			module = "  [" + printable(r.Module) + "]"
		}

		fmt.Fprintf(b, "  %*s  %6s  %s%s\n", samplesW, formatCount(r.Samples), percent(r.Samples, total), printable(r.Method), module)
	}

	if omitted > 0 {
		fmt.Fprintf(b, "…%s more (--top N)\n", formatCount(int64(omitted)))
	}
}

// gcLine is "GC: N collections (gen0 …), pauses X ms total (P% of the
// time), Y ms max".
func gcLine(g dotnet.GCSummary, durationMs int64) string {
	kinds := ""
	if g.Background > 0 {
		kinds += fmt.Sprintf("; %d background", g.Background)
	}

	if g.Induced > 0 {
		kinds += fmt.Sprintf("; %d induced by GC.Collect", g.Induced)
	}

	share := ""
	if durationMs > 0 {
		share = " (" + strconv.FormatFloat(100*g.PauseTotalMs/float64(durationMs), 'f', 1, 64) + "% of the time)"
	}

	return fmt.Sprintf("GC: %s collection%s (gen0 %s, gen1 %s, gen2 %s%s), pauses %s ms total%s, %s ms max",
		formatCount(int64(g.Collections)), plural(g.Collections), formatCount(int64(g.Gen0)), formatCount(int64(g.Gen1)),
		formatCount(int64(g.Gen2)), kinds, formatMs(g.PauseTotalMs), share, formatMs(g.PauseMaxMs))
}

// writeAllocations writes the allocated total and the top types: bytes
// (estimated from the ticks), ticks, type.
func writeAllocations(b *strings.Builder, a dotnet.AllocationSummary) {
	fmt.Fprintf(b, "allocated ≈ %s (sampled: an event about every 100 KB); top %d of %s allocated type%s:\n",
		formatBytes(a.Bytes), len(a.Types), formatCount(int64(a.TypeCount)), plural(a.TypeCount))

	// "≈" is one column but three bytes: the header is padded by columns.
	const bytesHeader = "BYTES≈"

	bytesW, ticksW := utf8.RuneCountInString(bytesHeader), len("TICKS")
	for _, t := range a.Types {
		bytesW, ticksW = max(bytesW, len(formatBytes(t.Bytes))), max(ticksW, len(formatCount(t.Ticks)))
	}

	fmt.Fprintf(b, "  %s%s  %*s  TYPE\n", strings.Repeat(" ", bytesW-utf8.RuneCountInString(bytesHeader)), bytesHeader, ticksW, "TICKS")

	for _, t := range a.Types {
		fmt.Fprintf(b, "  %*s  %*s  %s\n", bytesW, formatBytes(t.Bytes), ticksW, formatCount(t.Ticks), printable(t.Name))
	}

	if a.TypesOmitted > 0 {
		fmt.Fprintf(b, "…%s more type%s (--top N)\n", formatCount(int64(a.TypesOmitted)), plural(a.TypesOmitted))
	}
}

// writeTraceNotes writes what makes the numbers less than complete: an
// early end, lost events, frames without names.
func writeTraceNotes(b *strings.Builder, out traceOutput) {
	switch out.EndReason {
	case dotnet.EndExited:
		fmt.Fprintf(b, "the process exited after %s: the trace is shorter than --duration\n", formatSeconds(out.DurationMs))
	case dotnet.EndSize:
		fmt.Fprintf(b, "the trace reached its size limit (512 MiB) after %s and ended early\n", formatSeconds(out.DurationMs))
	}

	if out.EventsLost > 0 {
		fmt.Fprintf(b, "%s event%s lost (the trace buffer overflowed): counts are low\n", formatCount(out.EventsLost), plural64Were(out.EventsLost))
	}

	if c := out.CPU; c != nil && c.UnresolvedFrames > 0 {
		fmt.Fprintf(b, "%s frame%s had no method name and count as (unresolved): the process may have ended before its method names were recorded\n",
			formatCount(c.UnresolvedFrames), plural64(c.UnresolvedFrames))
	}
}

// percent is n as a share of total ("68.7%"), or "" without a total.
func percent(n, total int64) string {
	if total <= 0 {
		return ""
	}

	return strconv.FormatFloat(100*float64(n)/float64(total), 'f', 1, 64) + "%"
}

// formatMs prints milliseconds with one decimal ("12.3").
func formatMs(ms float64) string {
	return strconv.FormatFloat(ms, 'f', 1, 64)
}

func plural64(n int64) string {
	if n == 1 {
		return ""
	}

	return "s"
}

// plural64Were is " was" or "s were".
func plural64Were(n int64) string {
	if n == 1 {
		return " was"
	}

	return "s were"
}
