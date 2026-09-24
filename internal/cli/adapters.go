// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/eyedebugger/eyedebugger/drivers/dotnet"
	"github.com/eyedebugger/eyedebugger/internal/adapters"
	"github.com/eyedebugger/eyedebugger/internal/api"
)

const (
	installTimeout = 5 * time.Minute
	probeTimeout   = 10 * time.Second

	toolDotnet = dotnet.Language
)

// builtinLanguages are the languages compiled Go drivers serve; every
// other language comes from an adapter manifest.
func builtinLanguages() []string { return []string{dotnet.Language} }

// loadRegistry reads the bundled and the user's adapter manifests.
func loadRegistry() *adapters.Registry { return adapters.Default(builtinLanguages()...) }

// bundledVersion is the version of the bundled manifest name, for help.
func bundledVersion(name string) string {
	m := adapters.Load(adapters.LoadConfig{Bundled: adapters.Bundled(), Builtin: builtinLanguages()}).Adapter(name)
	if m == nil {
		return "?"
	}

	return m.Version
}

func newAdaptersCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adapters",
		Short: "List, install and check debug adapters",
		Long: `List, install and check the debug adapters eyedbg drives (docs/DESIGN.md §7, §8). Each is
described by an adapter manifest (docs/adapter-manifests.md): dotnet uses netcoredbg (Samsung,
MIT), python uses debugpy (Microsoft, MIT). Downloads are pinned to one release and verified by
SHA-256 before use. Microsoft's vsdbg is never used: its license restricts it to Microsoft's IDEs.

Adapters are installed per user under ~/.eyedbg/tools (override with EYEDBG_DATA_DIR); set
EYEDBG_NETCOREDBG to use your own netcoredbg build instead. Your own manifests go in
~/.eyedbg/adapters (EYEDBG_CONFIG_DIR overrides the ~/.eyedbg part): they add languages or
replace a bundled adapter, and are trusted like your shell configuration. EYEDBG_HOME overrides
~/.eyedbg itself, for both.

Without a subcommand, prints this help and exits 0; an unknown subcommand exits 1.`,
		Example: `  eyedbg adapters ls
  eyedbg adapters install netcoredbg
  eyedbg adapters install python
  eyedbg adapters doctor`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(newAdaptersListCommand(g), newAdaptersInstallCommand(g), newAdaptersDoctorCommand(g))

	return cmd
}

func newAdaptersInstallCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "install <adapter|language>",
		Short: "Download and install an adapter",
		Long: `Download the pinned release of an adapter (named, or by the language it debugs) for this
platform, check its size and SHA-256 against its manifest, and extract it. Bundled adapters:
netcoredbg ` + bundledVersion("netcoredbg") + ` for dotnet (prebuilt for linux x64/arm64, macOS arm64 and Windows x64)
and debugpy ` + bundledVersion("debugpy") + ` for python (pure Python, any platform; runs on your own Python
3.10+, which needs no debugpy of its own then). 'eyedbg adapters ls' lists them all.

Needs network access (github.com, files.pythonhosted.org); blocks until done (typically seconds,
at most 5m). Idempotent: an installed adapter is left as is. Prints the installed path ("path" in
--json). Exits 1 for an unknown adapter, an adapter with no download, or a download, checksum or
extraction failure; nothing half-installed is left behind.`,
		Example: `  eyedbg adapters install netcoredbg
  eyedbg adapters install python        # the same as: eyedbg adapters install debugpy`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := resolveAdapter(loadRegistry(), args[0])
			if err != nil {
				return err
			}

			if m.Install == nil {
				return api.NewError(api.CodeInvalidRequest, m.Name+"'s manifest has no download",
					"install it yourself (see 'eyedbg adapters ls --json' for its homepage)")
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), installTimeout)
			defer cancel()

			path, err := adapters.Install(ctx, http.DefaultClient, m)
			if err != nil {
				return err
			}

			if g.json {
				return writeJSON(cmd.OutOrStdout(), struct {
					Schema  int    `json:"schema"`
					Adapter string `json:"adapter"`
					Version string `json:"version"`
					Path    string `json:"path"`
				}{jsonSchemaVersion, m.Name, m.Version, path})
			}

			return writeText(cmd.OutOrStdout(), fmt.Sprintf("%s %s installed at %s\n", m.Name, m.Version, path))
		},
	}
}

// resolveAdapter finds the manifest named s, or serving language s.
func resolveAdapter(reg *adapters.Registry, s string) (*adapters.Manifest, error) {
	if m := reg.Resolve(s); m != nil {
		return m, nil
	}

	var names []string

	for _, m := range reg.Adapters() {
		name := m.Name
		if l := m.LanguageName(); l != "" {
			name += " (" + l + ")"
		}

		names = append(names, name)
	}

	return nil, fmt.Errorf("unknown adapter %q (available: %s)", s, strings.Join(names, ", "))
}

func newAdaptersDoctorCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [adapter|language...]",
		Short: "Check that adapters and toolchains are usable",
		Long: `Check what debugging needs on this machine and say what to fix. For dotnet: that netcoredbg
is found (and where from: EYEDBG_NETCOREDBG, installed, or PATH) and runs, and that the dotnet
host is found and reports its version. For python: which interpreter a start from this directory
would use (EYEDBG_PYTHON, else your active $VIRTUAL_ENV, else a .venv or venv here, else python3,
python or 'py -3' on PATH; start's --opt python=PATH overrides them) and which debugpy it runs (the one 'eyedbg adapters
install debugpy' downloaded, else the interpreter's own). Other adapters are checked the same
way, by their manifests.

Without arguments it checks every adapter: one that isn't installed is reported as "missing"
with how to install it, and doesn't fail the check (a .NET-only machine needs no Python). Name
adapters or languages to check only those: then a missing one is a problem.

Runs each tool once (at most 10s each); changes nothing. Output: one line per check, "ok",
"missing" or "problem", with a fix ("checks" in --json; "missing": true marks a missing one that
doesn't fail the check). Exits 1 if a check has a problem: something installed that doesn't work,
a broken user manifest, or a named adapter or language that is missing.`,
		Example: `  eyedbg adapters doctor
  eyedbg adapters doctor python
  eyedbg adapters doctor dotnet --json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			checks, err := systemDoctor().run(cmd.Context(), loadRegistry(), args)
			if err != nil {
				return err
			}

			if err := writeDoctor(cmd, checks, g.json); err != nil {
				return err
			}

			for _, c := range checks {
				if !c.OK && !c.Missing {
					return errDoctorProblems
				}
			}

			return nil
		},
	}
}

var errDoctorProblems = errors.New("some checks have problems (see above)")

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
	// Missing marks something not installed that wasn't asked for by
	// name: reported, but not a problem.
	Missing bool `json:"missing,omitempty"`
}

// doctor runs the checks; its functions are the system's, or fakes.
type doctor struct {
	find          func(m *adapters.Manifest) (adapters.Location, error)
	findHost      func() (string, error)
	probe         func(ctx context.Context, exe string, args ...string) (string, error)
	resolvePython func(ctx context.Context, m *adapters.Manifest, in adapters.PythonInput) (adapters.Runtime, error)
	workDir       string
}

func systemDoctor() doctor {
	return doctor{
		find: adapters.Find, findHost: dotnet.FindHost, probe: probe, resolvePython: adapters.ResolvePython,
		workDir: workDir(),
	}
}

// run checks the adapters named (by adapter or language name), or every
// adapter and every manifest problem when none is.
func (d doctor) run(ctx context.Context, reg *adapters.Registry, names []string) ([]doctorCheck, error) {
	var checks []doctorCheck

	if len(names) == 0 {
		for _, p := range reg.Problems() {
			checks = append(checks, doctorCheck{Name: "manifest", Detail: problemText(p), Fix: "fix or remove the manifest"})
		}

		for _, m := range doctorOrder(reg.Adapters()) {
			checks = append(checks, d.check(ctx, m, false)...)
		}

		return checks, nil
	}

	for _, name := range names {
		m, err := resolveAdapter(reg, name)
		if err != nil {
			return nil, err
		}

		checks = append(checks, d.check(ctx, m, true)...)
	}

	return checks, nil
}

// problemText is a manifest problem as one line.
func problemText(p adapters.Problem) string {
	if p.Path == "" {
		return p.Err.Error()
	}

	return p.Path + ": " + p.Err.Error()
}

// doctorOrder puts the adapter serving dotnet first (netcoredbg's checks
// come first, as before manifests), then the others by name.
func doctorOrder(ms []*adapters.Manifest) []*adapters.Manifest {
	out := slices.Clone(ms)
	slices.SortStableFunc(out, func(a, b *adapters.Manifest) int {
		ad, bd := a.LanguageName() == toolDotnet, b.LanguageName() == toolDotnet

		switch {
		case ad && !bd:
			return -1
		case bd && !ad:
			return 1
		default:
			return strings.Compare(a.Name, b.Name)
		}
	})

	return out
}

// check checks one adapter (and, for dotnet, the dotnet host). named
// makes a missing adapter a problem.
func (d doctor) check(ctx context.Context, m *adapters.Manifest, named bool) []doctorCheck {
	var checks []doctorCheck

	if m.Adapter.Runtime == adapters.RuntimePython {
		checks = append(checks, d.pythonCheck(ctx, m))
	} else {
		checks = append(checks, d.nativeCheck(ctx, m))
	}

	if m.LanguageName() == toolDotnet {
		checks = append(checks, d.hostCheck(ctx))
	}

	if named {
		for i := range checks {
			checks[i].Missing = false
		}
	}

	return checks
}

// nativeCheck finds a native adapter and runs it with its versionArgs.
func (d doctor) nativeCheck(ctx context.Context, m *adapters.Manifest) doctorCheck {
	loc, err := d.find(m)

	switch {
	case errors.Is(err, adapters.ErrNotInstalled):
		return doctorCheck{Name: m.Name, Detail: "not found", Fix: nativeInstallFix(m), Missing: true}
	case err != nil:
		return doctorCheck{Name: m.Name, Detail: err.Error(), Fix: "fix or unset " + m.Adapter.Env}
	case len(m.Adapter.VersionArgs) == 0:
		return doctorCheck{Name: m.Name, OK: true, Detail: fmt.Sprintf("found (%s, %s)", loc.Source, loc.Path)}
	}

	out, err := d.probe(ctx, loc.Path, m.Adapter.VersionArgs...)
	if err != nil {
		fix := m.Adapter.NotRunningHint
		if fix == "" {
			fix = "reinstall it"
		}

		return doctorCheck{Name: m.Name, Detail: loc.Path + " does not run: " + err.Error(), Fix: fix}
	}

	return doctorCheck{Name: m.Name, OK: true, Detail: fmt.Sprintf("%s (%s, %s)", firstLine(out), loc.Source, loc.Path)}
}

// nativeInstallFix says how to get a native adapter: "run 'eyedbg
// adapters install netcoredbg' or set EYEDBG_NETCOREDBG".
func nativeInstallFix(m *adapters.Manifest) string {
	var ways []string

	if m.Install != nil {
		ways = append(ways, "run 'eyedbg adapters install "+m.Name+"'")
	}

	if m.Adapter.Env != "" {
		ways = append(ways, "set "+m.Adapter.Env)
	}

	if len(ways) == 0 {
		return "install " + m.Adapter.Entry
	}

	return strings.Join(ways, " or ")
}

// hostCheck finds the dotnet host and runs it with --version.
func (d doctor) hostCheck(ctx context.Context) doctorCheck {
	host, err := d.findHost()
	if err != nil {
		return doctorCheck{
			Name: toolDotnet, Detail: "the dotnet host was not found", Missing: true,
			Fix: "install the .NET SDK and put dotnet on PATH or set DOTNET_ROOT (the daemon uses the environment of the eyedbg that started it)",
		}
	}

	out, err := d.probe(ctx, host, "--version")
	if err != nil {
		return doctorCheck{Name: toolDotnet, Detail: host + " does not run: " + err.Error(), Fix: "repair the .NET SDK install"}
	}

	return doctorCheck{Name: toolDotnet, OK: true, Detail: fmt.Sprintf("SDK %s (%s)", firstLine(out), host)}
}

// pythonCheck finds the interpreter and package a Python adapter runs on,
// as a start in the current directory would.
func (d doctor) pythonCheck(ctx context.Context, m *adapters.Manifest) doctorCheck {
	rt, err := d.resolvePython(ctx, m, adapters.PythonInput{Cwd: d.workDir, VirtualEnv: os.Getenv("VIRTUAL_ENV")})
	if err != nil {
		c := doctorCheck{Name: m.Name, Detail: err.Error(), Missing: errors.Is(err, adapters.ErrNotInstalled)}
		if apiErr, ok := errors.AsType[*api.Error](err); ok {
			c.Fix = apiErr.Hint
		}

		return c
	}

	return doctorCheck{Name: m.Name, OK: true, Detail: fmt.Sprintf("%s %s (%s, %s) on Python %s (%s, %s)",
		m.Python.Module, rt.ModuleVersion, rt.RootSource, rt.Root, rt.Version, rt.Exe, rt.Source)}
}

func probe(ctx context.Context, exe string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, exe, args...).CombinedOutput()

	return string(out), err
}

func writeDoctor(cmd *cobra.Command, checks []doctorCheck, asJSON bool) error {
	if asJSON {
		return writeJSON(cmd.OutOrStdout(), struct {
			Schema int           `json:"schema"`
			Checks []doctorCheck `json:"checks"`
		}{jsonSchemaVersion, checks})
	}

	var b strings.Builder

	for _, c := range checks {
		switch {
		case c.OK:
			fmt.Fprintf(&b, "ok       %s: %s\n", c.Name, c.Detail)
		case c.Missing:
			fmt.Fprintf(&b, "missing  %s: %s\n         fix: %s\n", c.Name, c.Detail, c.Fix)
		default:
			fmt.Fprintf(&b, "problem  %s: %s\n         fix: %s\n", c.Name, c.Detail, c.Fix)
		}
	}

	return writeText(cmd.OutOrStdout(), b.String())
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(line)
}

func newAdaptersListCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the adapters eyedbg knows",
		Long: `List every adapter manifest eyedbg loaded: the bundled ones and yours (from ~/.eyedbg/adapters;
EYEDBG_CONFIG_DIR or EYEDBG_HOME moves it), with the version
'eyedbg adapters install' fetches, the language each debugs ("built-in" when a Go driver serves
it) and its --opt options for 'eyedbg start'. A manifest of yours replaces the bundled one of the
same name or language. Manifests that could not be loaded are listed as "ignored" with why (also
'eyedbg adapters doctor').

Reads the manifests only; doesn't check what is installed (that is 'eyedbg adapters doctor').
Output: a table ("adapters" and "problems" in --json, with each adapter's download for this
platform, options, file extensions and project markers). Exits 0, even with ignored manifests.`,
		Example: `  eyedbg adapters ls
  eyedbg adapters ls --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg := loadRegistry()
			platform := runtime.GOOS + "/" + runtime.GOARCH

			if g.json {
				return writeJSON(cmd.OutOrStdout(), listJSON(reg, platform))
			}

			return writeText(cmd.OutOrStdout(), listText(reg))
		},
	}
}

// adapterJSON is one adapter in 'adapters ls --json'.
type adapterJSON struct {
	Name        string        `json:"name"`
	Version     string        `json:"version"`
	Description string        `json:"description"`
	Homepage    string        `json:"homepage,omitempty"`
	Language    string        `json:"language,omitempty"`
	Builtin     bool          `json:"builtin"`
	Runtime     string        `json:"runtime"`
	Source      string        `json:"source"`
	Path        string        `json:"path,omitempty"`
	Replaces    string        `json:"replaces,omitempty"`
	Download    *downloadJSON `json:"download,omitempty"`
	Options     []optionJSON  `json:"options"`
	Extensions  []string      `json:"extensions"`
	Markers     []string      `json:"markers"`
}

// downloadJSON is the build 'adapters install' fetches on this platform.
type downloadJSON struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size,omitempty"`
}

type optionJSON struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default,omitempty"`
	Help    string `json:"help"`
}

type problemJSON struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type listOutput struct {
	Schema   int           `json:"schema"`
	Adapters []adapterJSON `json:"adapters"`
	Problems []problemJSON `json:"problems"`
}

func listJSON(reg *adapters.Registry, platform string) listOutput {
	out := listOutput{Schema: jsonSchemaVersion, Adapters: []adapterJSON{}, Problems: []problemJSON{}}
	goos, goarch, _ := strings.Cut(platform, "/")

	for _, m := range reg.Adapters() {
		a := adapterJSON{
			Name: m.Name, Version: m.Version, Description: m.Description, Homepage: m.Homepage,
			Language: m.LanguageName(), Builtin: m.Builtin(), Runtime: m.Adapter.Runtime, Source: m.Source,
			Path: m.Path, Replaces: m.Replaces, Options: []optionJSON{}, Extensions: []string{}, Markers: []string{},
		}

		if a.Runtime == adapters.RuntimeNative {
			a.Runtime = "native"
		}

		if d, ok := m.DownloadFor(goos, goarch); ok {
			a.Download = &downloadJSON{URL: d.URL, SHA256: d.SHA256, Size: d.Size}
		}

		for _, name := range sortedOptions(m) {
			o := m.Options[name]
			a.Options = append(a.Options, optionJSON{Name: name, Type: o.Kind(), Default: o.Default, Help: o.Help})
		}

		if m.Language != nil {
			a.Extensions = append(a.Extensions, m.Language.Extensions...)
			a.Markers = append(a.Markers, m.Language.Markers...)
		}

		out.Adapters = append(out.Adapters, a)
	}

	for _, p := range reg.Problems() {
		out.Problems = append(out.Problems, problemJSON{Path: p.Path, Error: p.Err.Error()})
	}

	return out
}

// sortedOptions are m's option names, sorted.
func sortedOptions(m *adapters.Manifest) []string {
	names := make([]string, 0, len(m.Options))
	for name := range m.Options {
		names = append(names, name)
	}

	slices.Sort(names)

	return names
}

// listText renders the adapters as a table; each row may be followed by
// indented lines (options, the file a user manifest came from).
func listText(reg *adapters.Registry) string {
	ms := reg.Adapters()

	var table strings.Builder

	tw := tabwriter.NewWriter(&table, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ADAPTER\tVERSION\tLANGUAGE\tSOURCE\tDESCRIPTION")

	for _, m := range ms {
		lang := m.LanguageName()

		switch {
		case lang == "":
			lang = "-"
		case m.Builtin():
			lang += " (built-in)"
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", m.Name, m.Version, lang, m.Source, m.Description)
	}

	_ = tw.Flush()

	rows := strings.Split(strings.TrimSuffix(table.String(), "\n"), "\n")

	var b strings.Builder

	b.WriteString(rows[0] + "\n")

	for i, m := range ms {
		b.WriteString(rows[i+1] + "\n")
		b.WriteString(listDetails(m))
	}

	for _, p := range reg.Problems() {
		b.WriteString("ignored: " + problemText(p) + "\n")
	}

	return b.String()
}

// listDetails are the indented lines under an adapter's row.
func listDetails(m *adapters.Manifest) string {
	var b strings.Builder

	if m.Path != "" {
		b.WriteString("  file: " + m.Path)

		if m.Replaces != "" {
			b.WriteString(" (replaces " + m.Replaces + ")")
		}

		b.WriteString("\n")
	}

	if len(m.Options) > 0 {
		var opts []string

		for _, name := range sortedOptions(m) {
			o := m.Options[name]
			s := name

			if o.Kind() != adapters.OptionString {
				s += "=" + o.Kind()
			}

			if o.Default != "" {
				s += " (default " + o.Default + ")"
			}

			opts = append(opts, s)
		}

		b.WriteString("  options: " + strings.Join(opts, ", ") + "\n")
	}

	return b.String()
}
