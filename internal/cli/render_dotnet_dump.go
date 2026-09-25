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

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
)

// Text layout of the dump analyses.
const (
	// chainHead is how many links the helper keeps before a cut chain's gap.
	chainHead = 8
	// maxThreadGroups is how many thread groups the text shows.
	maxThreadGroups = 50
)

// writeDump prints 'eyedbg dotnet dump': what was dumped, the path, and
// how to analyze it.
func writeDump(w io.Writer, out dumpOutput, asJSON bool) error {
	if asJSON {
		return writeJSON(w, out)
	}

	var b strings.Builder

	fmt.Fprintf(&b, "dump of %s: %s, %s in %s\n%s\n", out.Target.describe(), out.Type, formatBytes(out.Bytes),
		formatSeconds(out.ElapsedMs), printable(out.Path))

	if out.Private {
		b.WriteString("private to you, removed after 7 days (at most 10 kept)\n")
	}

	fmt.Fprintf(&b, "analyze: eyedbg dotnet heap %[1]s; eyedbg dotnet threads %[1]s\n", shellArg(out.Path))

	return writeText(w, b.String())
}

// dumpSource is the heading's "of pid N (name), session S · dump PATH" or
// "of dump PATH", with the runtime.
func dumpSource(target *dotnetTarget, ref dumpRef, rt dotnet.DumpRuntime) string {
	gc := printable(rt.GC) + " GC"
	if rt.Heaps > 1 {
		gc += fmt.Sprintf(", %d heaps", rt.Heaps)
	}

	runtime := fmt.Sprintf("(.NET %s, %s)", printable(rt.Version), gc)

	if target == nil {
		return "of dump " + printable(ref.Path) + " " + runtime
	}

	return "of " + target.describe() + " · dump " + printable(ref.Path) + " " + runtime
}

// generationLabels are the text's names of the helper's generations.
var generationLabels = map[string]string{ //nolint:gochecknoglobals // A read-only table.
	"loh": "LOH", "poh": "POH",
}

// writeHeap prints 'eyedbg dotnet heap': totals and generations, the top
// types, and the root paths --gcroot asked for.
func writeHeap(w io.Writer, out heapOutput, params dotnet.HeapParams, asJSON bool) error {
	if asJSON {
		return writeJSON(w, out)
	}

	var b strings.Builder

	b.WriteString("heap " + dumpSource(out.Target, out.Dump, out.Runtime) + "\n")

	gens := make([]string, 0, len(out.Generations))
	for _, g := range out.Generations {
		label := generationLabels[g.Name]
		if label == "" {
			label = printable(g.Name)
		}

		gens = append(gens, label+" "+formatBytes(g.Bytes))
	}

	fmt.Fprintf(&b, "%s objects, %s: %s\n", formatCount(out.Objects), formatBytes(out.Bytes), strings.Join(gens, ", "))

	writeTypes(&b, out.HeapResult, params.TypeFilter)

	if out.Gcroot != nil {
		writeGcroot(&b, *out.Gcroot)
	}

	if out.Dump.Taken {
		b.WriteString("same dump: eyedbg dotnet threads " + shellArg(out.Dump.Path) + "\n")
	}

	return writeText(w, b.String())
}

// writeTypes writes the type table: bytes, count, type.
func writeTypes(b *strings.Builder, res dotnet.HeapResult, filter string) {
	matching := ""
	if filter != "" {
		matching = " matching " + strconv.Quote(printable(filter))
	}

	if res.TypeCount == 0 {
		b.WriteString("no types" + matching + "\n")

		return
	}

	fmt.Fprintf(b, "top %d of %s type%s%s by size:\n", len(res.Types), formatCount(int64(res.TypeCount)), plural(res.TypeCount), matching)

	bytesW, countW := len("BYTES"), len("COUNT")
	for _, t := range res.Types {
		bytesW, countW = max(bytesW, len(formatBytes(t.Bytes))), max(countW, len(formatCount(t.Count)))
	}

	fmt.Fprintf(b, "  %*s  %*s  TYPE\n", bytesW, "BYTES", countW, "COUNT")

	for _, t := range res.Types {
		fmt.Fprintf(b, "  %*s  %*s  %s\n", bytesW, formatBytes(t.Bytes), countW, formatCount(t.Count), printable(t.Name))
	}

	if res.TypesOmitted > 0 {
		fmt.Fprintf(b, "…%s more type%s (--top N, --type PATTERN)\n", formatCount(int64(res.TypesOmitted)), plural(res.TypesOmitted))
	}
}

// writeGcroot writes "why alive" and one line per root path.
func writeGcroot(b *strings.Builder, g dotnet.GcrootResult) {
	what := printable(g.Target.Type)
	if g.Target.Address != "" {
		what += " " + printable(g.Target.Address)
	}

	if g.Target.Instances != nil {
		what += fmt.Sprintf(" (%s object%s)", formatCount(*g.Target.Instances), plural(int(*g.Target.Instances)))
	}

	switch {
	case len(g.Paths) == 0 && g.Complete:
		b.WriteString("why alive: " + what + ": no path from a GC root (garbage the next collection frees)\n")

		return
	case len(g.Paths) == 0:
		b.WriteString("why alive: " + what + ": no path found before the search stopped (raise --timeout)\n")

		return
	}

	more := ""
	if !g.Complete {
		more = ", more may exist (--paths N)"
	}

	fmt.Fprintf(b, "why alive: %s: %d path%s%s:\n", what, len(g.Paths), plural(len(g.Paths)), more)

	for _, p := range g.Paths {
		parts := []string{rootText(p.Root)}

		for i, l := range p.Chain {
			if p.ChainOmitted > 0 && i == min(chainHead, len(p.Chain)-1) {
				parts = append(parts, fmt.Sprintf("…%d more…", p.ChainOmitted))
			}

			parts = append(parts, printable(l.Type)+" "+printable(l.Address))
		}

		b.WriteString("  " + strings.Join(parts, " → ") + "\n")
	}
}

// rootText is a root's label: "static Type.field", "stack thread N
// Method(…)", or its kind.
func rootText(r dotnet.RootLabel) string {
	s := printable(r.Kind)

	if r.Name != "" {
		s += " " + printable(r.Name)
	}

	if r.Thread != nil {
		s += fmt.Sprintf(" thread %d", *r.Thread)
	}

	if r.Frame != "" {
		s += " " + printable(r.Frame)
	}

	return s
}

// threadGroup is threads with the same stack and flags.
type threadGroup struct {
	ids     []int
	sample  dotnet.DumpThread
	owner   bool // one holds a lock
	waiting bool // in Monitor.Enter/Wait
}

// writeThreads prints 'eyedbg dotnet threads': the locks, then threads
// grouped by identical stacks.
func writeThreads(w io.Writer, out threadsOutput, asJSON bool) error {
	if asJSON {
		return writeJSON(w, out)
	}

	var b strings.Builder

	b.WriteString("threads " + dumpSource(out.Target, out.Dump, out.Runtime) + "\n")

	fmt.Fprintf(&b, "%d thread%s", len(out.Threads), plural(len(out.Threads)))

	if out.ThreadsOmitted > 0 {
		fmt.Fprintf(&b, ", %d more left out of the result (fewer --frames)", out.ThreadsOmitted)
	}

	b.WriteString("\n")

	owners := writeLocks(&b, out.ThreadsResult)
	groups := groupThreads(out.Threads, owners)

	for i, g := range groups {
		if i == maxThreadGroups {
			fmt.Fprintf(&b, "…%d more groups (--json lists every thread)\n", len(groups)-maxThreadGroups)

			break
		}

		writeThreadGroup(&b, g)
	}

	if out.Dump.Taken {
		b.WriteString("same dump: eyedbg dotnet heap " + shellArg(out.Dump.Path) + "\n")
	}

	return writeText(w, b.String())
}

// writeLocks writes the locks section and returns the owners' ids.
func writeLocks(b *strings.Builder, res dotnet.ThreadsResult) map[int]bool {
	owners := map[int]bool{}

	switch {
	case !res.LocksAvailable:
		b.WriteString("locks: not in a mini or triage dump (take a heap dump)\n")

		return owners
	case len(res.Locks) == 0:
		b.WriteString("locks: none held or waited on\n")

		return owners
	}

	b.WriteString("locks:\n")

	for _, l := range res.Locks {
		held := "not held"
		if l.Owner != nil {
			held = fmt.Sprintf("held by thread %d", *l.Owner)
			owners[*l.Owner] = true
		}

		fmt.Fprintf(b, "  %s %s %s, %d waiting\n", printable(l.Type), printable(l.Object), held, l.Waiting)
	}

	return owners
}

// groupThreads groups threads with identical stacks and flags: lock owners
// first, then threads in Monitor.Enter/Wait, then bigger groups, then by
// the lowest managed id.
func groupThreads(threads []dotnet.DumpThread, owners map[int]bool) []*threadGroup {
	var groups []*threadGroup

	byKey := map[string]*threadGroup{}

	for _, t := range threads {
		key := threadKey(t, owners[t.ManagedID])

		g, ok := byKey[key]
		if !ok {
			g = &threadGroup{sample: t, owner: owners[t.ManagedID], waiting: inMonitor(t)}
			byKey[key] = g
			groups = append(groups, g)
		}

		g.ids = append(g.ids, t.ManagedID)
	}

	rank := func(g *threadGroup) int {
		switch {
		case g.owner:
			return 0
		case g.waiting:
			return 1
		default:
			return 2
		}
	}

	for _, g := range groups {
		slices.Sort(g.ids)
	}

	slices.SortStableFunc(groups, func(a, b *threadGroup) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}

		if len(a.ids) != len(b.ids) {
			return len(b.ids) - len(a.ids)
		}

		return a.ids[0] - b.ids[0]
	})

	return groups
}

// threadKey is what makes two threads the same in the text: flags,
// exception type and frames.
func threadKey(t dotnet.DumpThread, owner bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%v|%v|%v|%v|%v|%v|%s|%d", owner, t.Alive, t.Background, t.Threadpool, t.Finalizer, t.GC, t.Exception, t.FramesOmitted)

	for _, f := range t.Frames {
		b.WriteString("\x00" + frameText(f))
	}

	return b.String()
}

// inMonitor says a thread's frames are in Monitor (waiting for a lock).
func inMonitor(t dotnet.DumpThread) bool {
	for _, f := range t.Frames {
		if f.Kind == "managed" && strings.HasPrefix(f.Method, "System.Threading.Monitor.") {
			return true
		}
	}

	return false
}

// writeThreadGroup writes a group's heading and its frames.
func writeThreadGroup(b *strings.Builder, g *threadGroup) {
	t := g.sample

	var flags []string

	if len(g.ids) > 1 {
		flags = append(flags, fmt.Sprintf("%d threads", len(g.ids)))
	}

	for _, f := range []struct {
		on   bool
		name string
	}{
		{g.owner, "holds a lock"},
		{!t.Alive, "dead"},
		{t.Background, "background"},
		{t.Threadpool, "threadpool"},
		{t.Finalizer, "finalizer"},
		{t.GC, "gc"},
	} {
		if f.on {
			flags = append(flags, f.name)
		}
	}

	if t.Exception != "" {
		flags = append(flags, "exception "+printable(t.Exception))
	}

	ids := make([]string, len(g.ids))
	for i, id := range g.ids {
		ids[i] = strconv.Itoa(id)
	}

	heading := "thread " + ids[0]
	if len(ids) > 1 {
		heading = "threads " + strings.Join(ids, ", ")
	}

	if len(flags) > 0 {
		heading += " (" + strings.Join(flags, ", ") + ")"
	}

	b.WriteString(heading + ":\n")

	if len(t.Frames) == 0 {
		b.WriteString("  (no frames)\n")
	}

	writeFrames(b, t.Frames)

	if t.FramesOmitted > 0 {
		fmt.Fprintf(b, "  …%d more frame%s (--frames N)\n", t.FramesOmitted, plural(t.FramesOmitted))
	}
}

// writeFrames writes one line per frame; the runtime often reports a frame
// twice in a row: one line, counted ("×2").
func writeFrames(b *strings.Builder, frames []dotnet.DumpFrame) {
	for i := 0; i < len(frames); {
		text, n := frameText(frames[i]), 1
		for i+n < len(frames) && frameText(frames[i+n]) == text {
			n++
		}

		if n > 1 {
			text += fmt.Sprintf(" ×%d", n)
		}

		b.WriteString("  " + text + "\n")
		i += n
	}
}

// frameText is "Method(args) +0xIL (File.cs:12)" or "[runtime frame NAME]".
func frameText(f dotnet.DumpFrame) string {
	if f.Kind != "managed" {
		return "[runtime frame " + printable(f.Method) + "]"
	}

	s := printable(f.Method)
	if f.ILOffset != nil {
		s += fmt.Sprintf(" +0x%x", *f.ILOffset)
	}

	if f.File != "" && f.Line > 0 {
		s += fmt.Sprintf(" (%s:%d)", printable(fileBase(f.File)), f.Line)
	}

	return s
}

// fileBase is a source path's last element, for either OS's separators
// (the PDB records the build machine's paths).
func fileBase(path string) string {
	return path[strings.LastIndexAny(path, `/\`)+1:]
}

// formatBytes prints a size in IEC units with one decimal ("342.1 MiB";
// bytes as such below 1 KiB).
func formatBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	v, i := float64(n), 0
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}

	for v /= unit; v >= unit && i < len(units)-1; i++ {
		v /= unit
	}

	return strconv.FormatFloat(v, 'f', 1, 64) + " " + units[i]
}

// formatCount prints n with thousands separators ("5,000").
func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)

	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")

	var b strings.Builder

	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}

		b.WriteRune(c)
	}

	if neg {
		return "-" + b.String()
	}

	return b.String()
}

// shellArg is a path as one shell word: as it is when it has no special
// characters, else single-quoted (printable in both cases).
func shellArg(s string) string {
	safe := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.,/:@+=~"
	if filepath.Separator != '/' {
		safe += string(filepath.Separator)
	}

	s = printable(s)
	if s != "" && strings.Trim(s, safe) == "" {
		return s
	}

	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
