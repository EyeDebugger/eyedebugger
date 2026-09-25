// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/artifacts"
	"github.com/eyedebugger/eyedebugger/internal/helper"
	"github.com/eyedebugger/eyedebugger/internal/version"
)

// Bounds and defaults of 'eyedbg dotnet dump|heap|threads' (docs/adr/0016,
// P2-M7).
const (
	defaultDumpTimeout     = 2 * time.Minute
	defaultAnalysisTimeout = 5 * time.Minute
	defaultTop             = 20
	maxTop                 = 1000
	defaultPaths           = 3
	maxPaths               = 20
	defaultFrames          = 10
	maxFrames              = 64
	// helperTimeoutSlack is how much sooner than eyedbg's own deadline the
	// helper is told to give up, so that it reports the expiry itself.
	helperTimeoutSlack = 2 * time.Second
	minHelperTimeout   = time.Second
	// maxDumpTimeout is the helper's own bound on a dump or an analysis.
	maxDumpTimeout = time.Hour
)

const dumpPrivacyHelp = `

A dump holds the program's memory — passwords, tokens, anything it had: eyedbg keeps it in a
directory only you can read (<home>/dumps, <home> being $EYEDBG_HOME or ~/.eyedbg; files 0600),
never prints its values, and removes its dumps after 7 days, keeping at most the newest 10 (files
eyedbg didn't name are left alone). Don't share a dump. The process is suspended while its runtime
writes the dump: about a second for a heap dump of a small program, minutes for a large or full
one. A program stopped at a breakpoint can be dumped too (unlike 'eyedbg dotnet counters').`

const dumpExitHelp = `

Exit codes: 0 success; 1 INVALID_REQUEST (bad flags, no such process or file, another user's
process, --out exists); 2 NO_SESSION, NOT_RUNNING (the session's program hasn't started),
SESSION_EXITED, NOT_DOTNET, DIAGNOSTICS_DISABLED, DIAGNOSTICS_TIMEOUT (the dump or the analysis
didn't finish in time), DUMP_UNSUPPORTED (not a dump eyedbg can read here: another OS or
architecture, truncated, no .NET runtime, or for heap no heap in it); 3 HELPER_NOT_FOUND,
HELPER_MISMATCH (a helper older than eyedbg), DUMP_RUNTIME_MISSING (the dump's .NET runtime isn't
installed here); 4 HELPER_FAILED, DUMP_FAILED (the runtime couldn't write the dump).`

const dumpSharedHelp = `

Runs the .NET side helper (helpers/dotnet next to eyedbg, or $EYEDBG_DOTNET_HELPER) with the
dotnet host, one process per step; only your own processes, and the same caveat about shared
machines as 'eyedbg help dotnet'. Not a debug-session action: no control lease, nothing in the
session's events. A dump taken elsewhere is analyzed with the .NET runtime installed here (the
same version must be); eyedbg's own dumps may use the runtime they were taken with. Nothing is
downloaded.`

// dumpTarget says how a command reaches a dump: a file (DUMP), or a
// process to dump first (--pid N, -s ID, or the only session).
type dumpTarget struct {
	file    string // absolute; "" for a process
	pid     int
	session bool // -s given
}

// check checks DUMP, --pid and -s: at most one.
func (d dumpTarget) check(cmdName string) error {
	switch {
	case d.pid < 0:
		return badDump(cmdName, "--pid must be positive")
	case d.pid > 0 && d.session:
		return badDump(cmdName, "--pid and -s/--session both name a target; pass one")
	case d.file != "" && (d.pid > 0 || d.session):
		return badDump(cmdName, "a DUMP file and --pid or -s/--session both name a target; pass one")
	default:
		return nil
	}
}

func badDump(cmdName, format string, a ...any) error {
	return api.NewError(api.CodeInvalidRequest, fmt.Sprintf(format, a...), "see 'eyedbg help dotnet "+cmdName+"'")
}

// fileArg makes a FILE argument absolute and checks it is a regular file
// (following symlinks); noun ("dump", "trace") and hint word the errors.
func fileArg(arg, noun, hint string) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", api.NewError(api.CodeInvalidRequest, fmt.Sprintf("%s %s: %v", noun, arg, err), "")
	}

	info, err := os.Stat(abs)

	switch {
	case err != nil:
		return "", api.NewError(api.CodeInvalidRequest, "no "+noun+" file at "+abs, hint)
	case !info.Mode().IsRegular():
		return "", api.NewError(api.CodeInvalidRequest, abs+" isn't a file", "")
	}

	return abs, nil
}

// checkOut validates --out early: absolute (from the working directory),
// nothing there (not even a dangling symlink), and its parent an existing
// directory. eyedbg never overwrites and never creates directories.
func checkOut(out string) (string, error) {
	abs, err := filepath.Abs(out)
	if err != nil {
		return "", badDump("dump", "--out %s: %v", out, err)
	}

	if info, err := os.Stat(filepath.Dir(abs)); err != nil || !info.IsDir() {
		return "", api.NewError(api.CodeInvalidRequest, "--out: "+filepath.Dir(abs)+" isn't a directory",
			"eyedbg doesn't create directories; make it first")
	}

	if _, err := os.Lstat(abs); !errors.Is(err, fs.ErrNotExist) {
		return "", api.NewError(api.CodeInvalidRequest, abs+" exists; eyedbg never overwrites",
			"pick a new name for --out")
	}

	return abs, nil
}

// checkDumpTimeout checks --timeout: positive, at most maxDumpTimeout.
func checkDumpTimeout(cmdName string, timeout time.Duration) error {
	if timeout <= 0 || timeout > maxDumpTimeout {
		return badDump(cmdName, "--timeout must be more than 0 and at most %s, not %s", maxDumpTimeout, timeout)
	}

	return nil
}

// helperTimeoutMs is the helper's own time budget: what's left of the
// command's, minus helperTimeoutSlack, at least a second.
func helperTimeoutMs(deadline, now time.Time) int64 {
	return max(deadline.Sub(now)-helperTimeoutSlack, minHelperTimeout).Milliseconds()
}

// mismatch turns an older helper's UNKNOWN_METHOD into HELPER_MISMATCH.
func mismatch(method string, err error) error {
	if api.CodeOf(err) != api.CodeUnknownMethod {
		return err
	}

	return api.NewError(api.CodeHelperMismatch, fmt.Sprintf("the .NET helper doesn't know %q (older than eyedbg?)", method),
		"reinstall eyedbg with its helpers/ directory, rebuild it from source ('task build'), or fix $"+dotnet.EnvHelper)
}

// dumpOptions are 'eyedbg dotnet dump' flags.
type dumpOptions struct {
	pid     int
	typ     string
	out     string
	timeout time.Duration

	sessionSet bool
}

// dumpPlan is what a dump command will do.
type dumpPlan struct {
	pid     int
	typ     string
	out     string // absolute, or ""
	timeout time.Duration
}

// plan validates the flags; --out is checked on disk (checkOut).
func (o dumpOptions) plan() (dumpPlan, error) {
	if err := (dumpTarget{pid: o.pid, session: o.sessionSet}).check("dump"); err != nil {
		return dumpPlan{}, err
	}

	switch o.typ {
	case dotnet.DumpHeap, dotnet.DumpMini, dotnet.DumpTriage, dotnet.DumpFull:
	default:
		return dumpPlan{}, badDump("dump", "--type must be heap, mini, triage or full, not %q", o.typ)
	}

	if err := checkDumpTimeout("dump", o.timeout); err != nil {
		return dumpPlan{}, err
	}

	p := dumpPlan{pid: o.pid, typ: o.typ, timeout: o.timeout}

	if o.out != "" {
		out, err := checkOut(o.out)
		if err != nil {
			return dumpPlan{}, err
		}

		p.out = out
	}

	return p, nil
}

func newDotnetDumpCommand(info version.Info, g *globals) *cobra.Command {
	var o dumpOptions

	cmd := &cobra.Command{
		Use:   "dump",
		Short: "Take a dump of a .NET process (its memory, for heap and threads analysis)",
		Long: `Take a dump of a running .NET process: its runtime writes the process's memory to a file in
eyedbg's private dumps directory, and eyedbg prints the file's path, for 'eyedbg dotnet heap PATH'
(what fills the heap, why objects are alive) and 'eyedbg dotnet threads PATH' (what each thread is
doing, who holds a lock). Those commands can also dump and analyze in one go (--pid, -s); take a
dump yourself to analyze the same moment several ways, or to keep it.

--type heap (default) has the managed heap: what both analyses need (lock owners too); mini has
threads and stacks only (much smaller; threads works, heap doesn't); triage is a smaller mini;
full has all memory (as large as the process: slow, big). --out PATH puts the dump there instead
(a new file: eyedbg never overwrites, and doesn't create directories; not removed after 7 days).

Target: --pid N (see 'eyedbg dotnet ps'), else the session (-s ID, $EYEDBG_SESSION, else the only
session): its program running or stopped at a breakpoint. Blocks while the dump is written;
--timeout (default 2m, at most 1h) bounds the command. Output: the target, type, size and time, then
the dump's path; --json: {"schema": 1, "target": {pid, name, session}, "type", "path", "bytes",
"private" (in eyedbg's directory), "elapsedMs"}.` + dumpPrivacyHelp + dumpSharedHelp + dumpExitHelp,
		Example: `  eyedbg dotnet dump                          # the only session's program, a heap dump
  eyedbg dotnet dump --pid 4321 --type mini
  eyedbg dotnet dump -s s-k3f9 --out ./app.dmp`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o.sessionSet = g.session != ""

			p, err := o.plan()
			if err != nil {
				return err
			}

			return runDump(cmd, info, g, p)
		},
	}

	f := cmd.Flags()
	f.IntVar(&o.pid, "pid", 0, "the process to dump (see 'eyedbg dotnet ps'); default: the session's program")
	f.StringVar(&o.typ, "type", dotnet.DumpHeap, "dump type: heap, mini, triage or full")
	f.StringVar(&o.out, "out", "", "write the dump to this new file instead of eyedbg's private directory")
	f.DurationVar(&o.timeout, "timeout", defaultDumpTimeout, "bound on the whole command, at most 1h, e.g. 10m for a full dump")

	return cmd
}

// dumpOutput is the --json output of 'eyedbg dotnet dump'.
type dumpOutput struct {
	Schema    int          `json:"schema"`
	Target    dotnetTarget `json:"target"`
	Type      string       `json:"type"`
	Path      string       `json:"path"`
	Bytes     int64        `json:"bytes"`
	Private   bool         `json:"private"`
	ElapsedMs int64        `json:"elapsedMs"`
}

func runDump(cmd *cobra.Command, info version.Info, g *globals, p dumpPlan) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), p.timeout)
	defer cancel()

	target, err := resolveTarget(cmd, info, g, p.pid, true)
	if err != nil {
		return err
	}

	dir, err := privateDumps()
	if err != nil {
		return err
	}

	path, res, err := takeDump(ctx, dir, target, p.typ, p.timeout)
	if err != nil {
		return err
	}

	out := dumpOutput{
		Schema: jsonSchemaVersion, Target: target, Type: p.typ, Path: path, Bytes: res.Bytes, Private: true, ElapsedMs: res.ElapsedMs,
	}

	if p.out != "" {
		if err := place("dump", path, p.out); err != nil {
			return err
		}

		out.Path, out.Private = p.out, false
	}

	return writeDump(cmd.OutOrStdout(), out, g.json)
}

// privateDumps prepares eyedbg's dumps directory.
func privateDumps() (string, error) {
	dir, err := artifacts.Dir(artifacts.Dumps)
	if err != nil {
		return "", api.NewError(api.CodeInvalidRequest, "eyedbg's dumps directory: "+err.Error(),
			"fix it or set $"+adapters.EnvHome+" to a directory of yours")
	}

	return dir, nil
}

// prune removes eyedbg's old dumps from dir, keeping the newest
// artifacts.Keep; protect (a dump just taken, or about to be analyzed)
// counts among them and is never removed.
func prune(dir, protect string) {
	artifacts.Prune(dir, artifacts.Dumps, time.Now(), artifacts.MaxAge, artifacts.Keep, protect)
}

// takeDump has the target's runtime write a dump under a fresh name in dir,
// checks it arrived (a regular file, not empty, 0600), then prunes dir with
// it protected. On failure no dump of this call is left behind.
func takeDump(ctx context.Context, dir string, target dotnetTarget, typ string, timeout time.Duration) (string, dotnet.DumpResult, error) {
	name, err := artifacts.Name(dir, artifacts.Dumps, target.PID, typ, time.Now(), rand.Reader)
	if err != nil {
		return "", dotnet.DumpResult{}, err
	}

	path := filepath.Join(dir, name)
	deadline, _ := ctx.Deadline()
	params := dotnet.DumpParams{PID: target.PID, Type: typ, Path: path, TimeoutMs: helperTimeoutMs(deadline, time.Now())}

	var res dotnet.DumpResult

	err = withHelper(ctx, timeout, func(h *helper.Helper) error {
		return mismatch(dotnet.MethodDump, h.Call(ctx, dotnet.MethodDump, params, &res))
	})
	if err == nil {
		err = checkDump(path)
	}

	if err != nil {
		removeStray(path)

		return "", dotnet.DumpResult{}, err
	}

	// After the dump, so that it counts: at most artifacts.Keep remain.
	prune(dir, path)

	return path, res, nil
}

// checkDump checks the runtime's dump is at path: a regular file (not a
// symlink), not empty; on Unix it is made 0600 again.
func checkDump(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return api.NewError(api.CodeDumpFailed, "the runtime reported a dump but none is at "+path,
			"the process runs in another filesystem namespace (a container?): take the dump inside it")
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o600); err != nil {
			return api.NewError(api.CodeDumpFailed, "restrict "+path+" to you: "+err.Error(), "")
		}
	}

	return nil
}

// removeStray removes what a failed or canceled dump left at path (a
// regular file only; Prune catches one that appears later).
func removeStray(path string) {
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		_ = os.Remove(path)
	}
}

// place moves a private artifact (what: "dump", "trace") to --out without
// replacing anything.
func place(what, path, out string) error {
	err := artifacts.Place(path, out)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, artifacts.ErrExists):
		return api.NewError(api.CodeInvalidRequest, out+" exists; eyedbg never overwrites",
			"the "+what+" is kept at "+path+"; pick a new name for --out")
	default:
		return api.NewError(api.CodeInvalidRequest, "put the "+what+" at "+out+": "+err.Error(), "the "+what+" is kept at "+path)
	}
}

// analysisOptions are the flags 'eyedbg dotnet heap' and 'threads' share.
type analysisOptions struct {
	file        string
	pid         int
	dumpType    string
	timeout     time.Duration
	sessionSet  bool
	dumpTypeSet bool
}

// analysisPlan is how an analysis reaches its dump.
type analysisPlan struct {
	target   dumpTarget
	dumpType string // live targets: the dump to take
	timeout  time.Duration
}

// plan validates what heap and threads share; allowed are the --dump-type
// values the command accepts (the first is the default).
func (o analysisOptions) plan(cmdName string, allowed ...string) (analysisPlan, error) {
	t := dumpTarget{pid: o.pid, session: o.sessionSet}

	if o.file != "" {
		file, err := fileArg(o.file, "dump", "take one with 'eyedbg dotnet dump'")
		if err != nil {
			return analysisPlan{}, err
		}

		t.file = file
	}

	if err := t.check(cmdName); err != nil {
		return analysisPlan{}, err
	}

	if err := checkDumpTimeout(cmdName, o.timeout); err != nil {
		return analysisPlan{}, err
	}

	p := analysisPlan{target: t, dumpType: allowed[0], timeout: o.timeout}

	if o.dumpTypeSet {
		if t.file != "" {
			return analysisPlan{}, badDump(cmdName, "--dump-type is for a process (--pid, -s); a DUMP file is analyzed as it is")
		}

		if !slices.Contains(allowed, o.dumpType) {
			return analysisPlan{}, badDump(cmdName, "--dump-type must be %s or %s, not %q",
				strings.Join(allowed[:len(allowed)-1], ", "), allowed[len(allowed)-1], o.dumpType)
		}

		p.dumpType = o.dumpType
	}

	return p, nil
}

// dumpRef is the "dump" member of heap and threads --json output.
type dumpRef struct {
	Path string `json:"path"`
	// Private: in eyedbg's dumps directory (taken by eyedbg).
	Private bool `json:"private"`
	// Taken: this command took it.
	Taken bool `json:"taken"`
}

// analysisInput is the dump an analysis reads, and whose it is.
type analysisInput struct {
	target *dotnetTarget // nil for a DUMP file
	ref    dumpRef
	// path is what the helper opens: for eyedbg's own dump, the private
	// file artifacts.Own checked, so that the file checked is the one
	// read.
	path    string
	trusted bool
}

// analysisTarget resolves the process a heap or threads command dumps
// first (nil for a DUMP file).
func analysisTarget(cmd *cobra.Command, info version.Info, g *globals, p analysisPlan) (*dotnetTarget, error) {
	if p.target.file != "" {
		return nil, nil //nolint:nilnil // No process: the command reads a file.
	}

	target, err := resolveTarget(cmd, info, g, p.target.pid, true)
	if err != nil {
		return nil, err
	}

	return &target, nil
}

// prepareAnalysis takes the dump of target, or checks the DUMP file (target
// nil); ctx carries the command's deadline. refuse says whether a dump of a
// type (from an eyedbg dump's name) can't serve this analysis.
func prepareAnalysis(ctx context.Context, target *dotnetTarget, p analysisPlan, refuse func(typ string) error) (analysisInput, error) {
	if target != nil {
		dir, err := privateDumps()
		if err != nil {
			return analysisInput{}, err
		}

		path, _, err := takeDump(ctx, dir, *target, p.dumpType, p.timeout)
		if err != nil {
			return analysisInput{}, err
		}

		return analysisInput{target: target, ref: dumpRef{Path: path, Private: true, Taken: true}, path: path, trusted: true}, nil
	}

	resolved, err := filepath.EvalSymlinks(p.target.file)
	if err != nil {
		return analysisInput{}, api.NewError(api.CodeInvalidRequest, fmt.Sprintf("dump %s: %v", p.target.file, err), "")
	}

	dir, err := privateDumps()
	if err != nil {
		return analysisInput{}, err
	}

	prune(dir, resolved)

	return fileInput(dir, p.target.file, resolved, refuse)
}

// fileInput decides how a DUMP file is analyzed. Trust and the path the
// helper opens come from one check (artifacts.Own on the path already
// resolved): an eyedbg dump is sent as the private file that check found,
// so a symlink changed in between can't pair trust with another file.
func fileInput(dir, file, resolved string, refuse func(typ string) error) (analysisInput, error) {
	in := analysisInput{ref: dumpRef{Path: file}, path: file}

	if own, ok := artifacts.Own(dir, artifacts.Dumps, resolved); ok {
		in.ref.Private, in.trusted, in.path = true, true, own

		if err := refuse(artifacts.TypeOf(artifacts.Dumps, filepath.Base(own))); err != nil {
			return analysisInput{}, err
		}
	}

	return in, nil
}

// heapOptions are 'eyedbg dotnet heap' flags.
type heapOptions struct {
	analysisOptions

	top        int
	typeFilter string
	gcroot     string
	paths      int
	pathsSet   bool
}

// heapPlan is what a heap command will do.
type heapPlan struct {
	analysisPlan

	params dotnet.HeapParams // without Path, TrustRecordedRuntime, TimeoutMs
}

// plan validates heap's flags.
func (o heapOptions) plan() (heapPlan, error) {
	ap, err := o.analysisOptions.plan("heap", dotnet.DumpHeap, dotnet.DumpFull)
	if err != nil {
		return heapPlan{}, err
	}

	if o.top < 1 || o.top > maxTop {
		return heapPlan{}, badDump("heap", "--top must be 1 to %d, not %d", maxTop, o.top)
	}

	if o.paths < 1 || o.paths > maxPaths {
		return heapPlan{}, badDump("heap", "--paths must be 1 to %d, not %d", maxPaths, o.paths)
	}

	if o.pathsSet && o.gcroot == "" {
		return heapPlan{}, badDump("heap", "--paths goes with --gcroot")
	}

	params := dotnet.HeapParams{Top: o.top, TypeFilter: o.typeFilter, Paths: o.paths}

	switch {
	case o.gcroot == "":
		params.Paths = 1
	case strings.HasPrefix(o.gcroot, "0x") || strings.HasPrefix(o.gcroot, "0X"):
		if !isHexAddress(o.gcroot) {
			return heapPlan{}, badDump("heap", "--gcroot %q: an address is 0x and 1 to 16 hex digits", o.gcroot)
		}

		params.Gcroot = &dotnet.GcrootParams{Address: strings.ToLower(o.gcroot)}
	default:
		params.Gcroot = &dotnet.GcrootParams{Type: o.gcroot}
	}

	return heapPlan{analysisPlan: ap, params: params}, nil
}

func isHexAddress(s string) bool {
	digits := s[2:]
	if digits == "" || len(digits) > 16 {
		return false
	}

	return strings.Trim(strings.ToLower(digits), "0123456789abcdef") == ""
}

func newDotnetHeapCommand(info version.Info, g *globals) *cobra.Command {
	var o heapOptions

	cmd := &cobra.Command{
		Use:   "heap [DUMP]",
		Short: "Show what fills a .NET process's heap, and why objects are alive",
		Long: `Show what fills the managed heap of a .NET process or dump: the objects and bytes in each
generation (gen0, gen1, gen2, LOH, POH, frozen) and the types holding the most bytes (--top N,
default 20; --type PATTERN keeps types whose full name contains it, case-sensitive). Use it when
memory grows: the top types say what accumulates. --gcroot TYPE (a full type name, or a part that
matches one type) or --gcroot 0xADDRESS says why those objects are alive: up to --paths N (default
3) chains of references from a GC root — a static field ("static Holder.Keep"), a thread's stack
("stack thread 1 Method(…)"), a handle (strong, pinned, …) or the finalizer queue — to them; the
longest chains are cut in the middle. Never shows field values or string contents: types, counts,
sizes, addresses, root kinds and static field names only.

Target: a DUMP file (e.g. from 'eyedbg dotnet dump'), else a process — --pid N, else the session
(-s ID, $EYEDBG_SESSION, else the only session), running or stopped at a breakpoint — which is
dumped first (a heap dump, or --dump-type full) into eyedbg's private directory; the output names
that dump, so 'eyedbg dotnet threads PATH' can read the same moment. A mini or triage dump has no
heap: refused (INVALID_REQUEST at once for eyedbg's own, whose name says so; else DUMP_UNSUPPORTED).

Blocks while dumping and analyzing: seconds for a small program; --timeout (default 5m, at most 1h)
bounds the whole command (a search for root paths cut by it returns the paths found). --json:
{"schema": 1, "target" (for a process), "dump": {path, private, taken}, "runtime": {version, gc,
heaps, platform, arch}, "objects", "bytes", "generations": [{name, objects, bytes}], "typeCount",
"types": [{name, count, bytes}], "typesOmitted", "gcroot": {target: {type, address, instances},
paths: [{root: {kind, name, thread, frame}, chain: [{address, type}], chainOmitted}], pathsFound,
complete}}; addresses are hex strings.` + dumpPrivacyHelp + dumpSharedHelp + dumpExitHelp,
		Example: `  eyedbg dotnet heap                              # dump the only session's program, analyze it
  eyedbg dotnet heap --pid 4321 --top 10
  eyedbg dotnet heap ~/.eyedbg/dumps/4321-20260925T150300Z-heap-1f3a9c0e.dmp --gcroot MyApp.Order
  eyedbg dotnet heap app.dmp --type MyApp. --gcroot 0x7f3a1c02e8`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.file = strings.Join(args, "") // DUMP, if given (at most one)
			o.sessionSet, o.dumpTypeSet, o.pathsSet = g.session != "", cmd.Flags().Changed("dump-type"), cmd.Flags().Changed("paths")

			p, err := o.plan()
			if err != nil {
				return err
			}

			return runHeap(cmd, info, g, p)
		},
	}

	f := cmd.Flags()
	f.IntVar(&o.pid, "pid", 0, "dump and analyze this process (see 'eyedbg dotnet ps'); default: the session's program")
	f.IntVar(&o.top, "top", defaultTop, "how many types to list, biggest first (1-1000)")
	f.StringVar(&o.typeFilter, "type", "", "only types whose full name contains this (case-sensitive)")
	f.StringVar(&o.gcroot, "gcroot", "", "why objects are alive: a type name (full, or a unique part) or an object's 0xADDRESS")
	f.IntVar(&o.paths, "paths", defaultPaths, "with --gcroot: how many root paths to find (1-20)")
	f.StringVar(&o.dumpType, "dump-type", dotnet.DumpHeap, "for a process: the dump to take, heap or full")
	f.DurationVar(&o.timeout, "timeout", defaultAnalysisTimeout, "bound on the whole command (dump and analysis), at most 1h, e.g. 10m")

	return cmd
}

// heapOutput is the --json output of 'eyedbg dotnet heap'.
type heapOutput struct {
	Schema int           `json:"schema"`
	Target *dotnetTarget `json:"target,omitempty"`
	Dump   dumpRef       `json:"dump"`

	dotnet.HeapResult //nolint:embeddedstructfieldcheck // JSON order: schema, target and dump, then the helper's result.
}

func runHeap(cmd *cobra.Command, info version.Info, g *globals, p heapPlan) error {
	target, err := analysisTarget(cmd, info, g, p.analysisPlan)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), p.timeout)
	defer cancel()

	in, err := prepareAnalysis(ctx, target, p.analysisPlan, func(typ string) error {
		if typ == dotnet.DumpMini || typ == dotnet.DumpTriage {
			return api.NewError(api.CodeInvalidRequest, "a "+typ+" dump has no heap to analyze",
				"take one with 'eyedbg dotnet dump --type heap', or read its threads with 'eyedbg dotnet threads'")
		}

		return nil
	})
	if err != nil {
		return err
	}

	params := p.params
	params.Path, params.TrustRecordedRuntime = in.path, in.trusted
	deadline, _ := ctx.Deadline()
	params.TimeoutMs = helperTimeoutMs(deadline, time.Now())

	var res dotnet.HeapResult

	err = withHelper(ctx, p.timeout, func(h *helper.Helper) error {
		return mismatch(dotnet.MethodHeap, h.Call(ctx, dotnet.MethodHeap, params, &res))
	})
	if err != nil {
		return err
	}

	return writeHeap(cmd.OutOrStdout(), heapOutput{Schema: jsonSchemaVersion, Target: in.target, Dump: in.ref, HeapResult: res}, params, g.json)
}

// threadsOptions are 'eyedbg dotnet threads' flags.
type threadsOptions struct {
	analysisOptions

	frames int
}

// threadsPlan is what a threads command will do.
type threadsPlan struct {
	analysisPlan

	frames int
}

// plan validates threads' flags.
func (o threadsOptions) plan() (threadsPlan, error) {
	ap, err := o.analysisOptions.plan("threads", dotnet.DumpHeap, dotnet.DumpMini, dotnet.DumpFull)
	if err != nil {
		return threadsPlan{}, err
	}

	if o.frames < 1 || o.frames > maxFrames {
		return threadsPlan{}, badDump("threads", "--frames must be 1 to %d, not %d", maxFrames, o.frames)
	}

	return threadsPlan{analysisPlan: ap, frames: o.frames}, nil
}

func newDotnetThreadsCommand(info version.Info, g *globals) *cobra.Command {
	var o threadsOptions

	cmd := &cobra.Command{
		Use:   "threads [DUMP]",
		Short: "Show what a .NET process's threads are doing, and who holds a lock",
		Long: `Show the managed threads of a .NET process or dump — ids, flags (background, threadpool,
finalizer, the type of a current exception) and the top --frames N (default 10) frames of each,
with source file:line where the program's PDB is found — and its contended locks: each monitor
that is held or waited on, its owner thread and how many threads wait. Use it when a program
hangs, deadlocks or stalls: threads with the same stack are shown once ("threads 7, 8, 9 (3
threads, …)"), lock owners and waiters first. Frames read "Method(args) +0xIL (File.cs:12)" or
"[runtime frame NAME]"; set a breakpoint there next ('eyedbg bp add File.cs:12'). Never shows
thread names or exception messages (they are the program's strings).

Known limits: only locks a sync block records — contended lock(obj) / Monitor — are listed; an
uncontended lock, and .NET 9's System.Threading.Lock, aren't. Which lock a waiting thread waits on
isn't recorded: its top frames show Monitor.Enter or Wait.

Target: a DUMP file, else a process — --pid N, else the session (-s ID, $EYEDBG_SESSION, else the
only session), running or stopped at a breakpoint — which is dumped first into eyedbg's private
directory: a heap dump by default (lock owners need it; 'eyedbg dotnet heap PATH' can read the same
dump), --dump-type mini (smaller, no locks) or full (all memory: large). On Windows only a full dump
records IL offsets and source lines (heap and mini dumps show methods only). Blocks while dumping
and analyzing: seconds for a small program; --timeout (default 5m, at most 1h) bounds the whole
command. --json: {"schema": 1, "target" (for a process), "dump": {path, private, taken}, "runtime",
"threads": [{managedId, osId, alive, background, threadpool, finalizer, gc, exception, frames:
[{kind: "managed", method, module, ilOffset, file, line} or {kind: "runtime", method}],
framesOmitted}], "threadsOmitted", "locks": [{object, type, owner, waiting}], "locksAvailable"};
every thread is listed there.` + dumpPrivacyHelp + dumpSharedHelp + dumpExitHelp,
		Example: `  eyedbg dotnet threads                           # dump the only session's program, show its threads
  eyedbg dotnet threads --pid 4321 --dump-type mini
  eyedbg dotnet threads ~/.eyedbg/dumps/4321-20260925T150300Z-heap-1f3a9c0e.dmp --frames 30`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				o.file = args[0]
			}

			o.sessionSet = g.session != ""
			o.dumpTypeSet = cmd.Flags().Changed("dump-type")

			p, err := o.plan()
			if err != nil {
				return err
			}

			return runThreads(cmd, info, g, p)
		},
	}

	f := cmd.Flags()
	f.IntVar(&o.pid, "pid", 0, "dump and analyze this process (see 'eyedbg dotnet ps'); default: the session's program")
	f.IntVar(&o.frames, "frames", defaultFrames, "frames per thread, from the top (1-64)")
	f.StringVar(&o.dumpType, "dump-type", dotnet.DumpHeap, "for a process: the dump to take, heap (locks too), mini or full")
	f.DurationVar(&o.timeout, "timeout", defaultAnalysisTimeout, "bound on the whole command (dump and analysis), at most 1h, e.g. 10m")

	return cmd
}

// threadsOutput is the --json output of 'eyedbg dotnet threads'.
type threadsOutput struct {
	Schema int           `json:"schema"`
	Target *dotnetTarget `json:"target,omitempty"`
	Dump   dumpRef       `json:"dump"`

	dotnet.ThreadsResult //nolint:embeddedstructfieldcheck // JSON order: schema, target and dump, then the helper's result.
}

func runThreads(cmd *cobra.Command, info version.Info, g *globals, p threadsPlan) error {
	target, err := analysisTarget(cmd, info, g, p.analysisPlan)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), p.timeout)
	defer cancel()

	in, err := prepareAnalysis(ctx, target, p.analysisPlan, func(string) error { return nil })
	if err != nil {
		return err
	}

	deadline, _ := ctx.Deadline()
	params := dotnet.ThreadsParams{
		Path: in.path, TrustRecordedRuntime: in.trusted, Frames: p.frames, TimeoutMs: helperTimeoutMs(deadline, time.Now()),
	}

	var res dotnet.ThreadsResult

	err = withHelper(ctx, p.timeout, func(h *helper.Helper) error {
		return mismatch(dotnet.MethodThreads, h.Call(ctx, dotnet.MethodThreads, params, &res))
	})
	if err != nil {
		return err
	}

	return writeThreads(cmd.OutOrStdout(), threadsOutput{Schema: jsonSchemaVersion, Target: in.target, Dump: in.ref, ThreadsResult: res}, g.json)
}
