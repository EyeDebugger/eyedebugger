// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package dotnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// touch creates the file (and its directories).
func touch(t *testing.T, path string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("<Project/>"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func realDir(t *testing.T) string {
	t.Helper()

	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func projectsOf(t *testing.T, err error) string {
	t.Helper()

	if api.CodeOf(err) != api.CodeInvalidRequest {
		t.Fatalf("err = %v (%s), want INVALID_REQUEST", err, api.CodeOf(err))
	}

	return err.Error()
}

func TestFindContainerProject(t *testing.T) {
	t.Parallel()

	root := realDir(t)

	for _, p := range []string{
		"Web/Web.csproj", "Worker/Worker.fsproj", "Old/Old.vbproj", "Deep/a/b/c/Deep.csproj",
		"One/Dup.csproj", "Two/Dup.fsproj",
		// Never entered.
		"bin/Hidden/Skipped.csproj", "obj/Hidden/Skipped.csproj", "node_modules/pkg/Skipped.csproj",
		".git/Skipped.csproj", ".hidden/x/Skipped.csproj", "Web/bin/Debug/Skipped.csproj",
		// Wrong name or case.
		"Other/other.csproj", "Other/Web.csproj.bak", "Other/Webx.csproj", "Other/xWeb.csproj", "Other/Web.cs",
	} {
		touch(t, filepath.Join(root, p))
	}

	tests := []struct {
		dll     string
		want    string
		wantErr string
	}{
		{dll: "Web.dll", want: "Web/Web.csproj"},
		{dll: "Worker.dll", want: "Worker/Worker.fsproj"},
		{dll: "Old.dll", want: "Old/Old.vbproj"},
		{dll: "Deep.dll", want: "Deep/a/b/c/Deep.csproj"},
		{dll: "Dup.dll", wantErr: "2 projects build Dup.dll"},
		{dll: "Skipped.dll", wantErr: "no project file for Skipped.dll"},
		{dll: "Missing.dll", wantErr: "no project file for Missing.dll"},
		{dll: "other.dll", want: "Other/other.csproj"},
		{dll: "WEB.dll", wantErr: "no project file"},
		{dll: "../Web.dll", wantErr: "invalid assembly name"},
		{dll: "Web", wantErr: "invalid assembly name"},
		{dll: "", wantErr: "invalid assembly name"},
	}

	for _, tt := range tests {
		t.Run(tt.dll, func(t *testing.T) {
			t.Parallel()

			got, err := FindContainerProject(root, tt.dll, 50_000)
			if tt.wantErr != "" {
				if msg := projectsOf(t, err); !strings.Contains(msg, tt.wantErr) {
					t.Errorf("err = %v, want %q", err, tt.wantErr)
				}

				return
			}

			if err != nil || got != filepath.Join(root, filepath.FromSlash(tt.want)) {
				t.Errorf("FindContainerProject(%q) = %q, %v, want %s", tt.dll, got, err, tt.want)
			}
		})
	}
}

func TestFindContainerProjectNamesTheFlag(t *testing.T) {
	t.Parallel()

	root := realDir(t)
	touch(t, filepath.Join(root, "A", "Dup.csproj"))
	touch(t, filepath.Join(root, "B", "Dup.csproj"))

	_, err := FindContainerProject(root, "Dup.dll", 100)

	var ae *api.Error
	if !errors.As(err, &ae) || !strings.Contains(ae.Hint, "--dotnet-project") || !strings.Contains(ae.Message, filepath.Join(root, "A", "Dup.csproj")) || !strings.Contains(ae.Message, filepath.Join(root, "B", "Dup.csproj")) {
		t.Errorf("err = %#v", err)
	}

	_, err = FindContainerProject(root, "None.dll", 100)
	if !errors.As(err, &ae) || !strings.Contains(ae.Hint, "--dotnet-project") {
		t.Errorf("err = %#v", err)
	}
}

func TestFindContainerProjectListsTenCandidates(t *testing.T) {
	t.Parallel()

	root := realDir(t)
	for i := range 13 {
		touch(t, filepath.Join(root, fmt.Sprintf("d%02d", i), "Dup.csproj"))
	}

	_, err := FindContainerProject(root, "Dup.dll", 1000)
	msg := projectsOf(t, err)

	if !strings.Contains(msg, "13 projects") || !strings.Contains(msg, "d09") || strings.Contains(msg, "d10") || !strings.Contains(msg, "and 3 more") {
		t.Errorf("message = %s", msg)
	}
}

func TestFindContainerProjectLimit(t *testing.T) {
	t.Parallel()

	root := realDir(t)
	for i := range 20 {
		touch(t, filepath.Join(root, fmt.Sprintf("d%02d", i), "f.txt"))
	}

	touch(t, filepath.Join(root, "zzz", "Last.csproj")) // found only past the limit

	_, err := FindContainerProject(root, "Last.dll", 10)
	if msg := projectsOf(t, err); !strings.Contains(msg, "without finishing") {
		t.Errorf("err = %v", err)
	}

	if got, err := FindContainerProject(root, "Last.dll", 1000); err != nil || filepath.Base(got) != "Last.csproj" {
		t.Errorf("within the limit: %q, %v", got, err)
	}
}

func TestFindContainerProjectFollowsNoSymlink(t *testing.T) {
	t.Parallel()

	root := realDir(t)
	outside := realDir(t)

	touch(t, filepath.Join(root, "Real", "Real.csproj"))
	touch(t, filepath.Join(outside, "Escaped", "Escaped.csproj"))
	touch(t, filepath.Join(outside, "Target.csproj"))

	for link, target := range map[string]string{
		"loop":          root,                                    // a loop back to the root
		"outdir":        outside,                                 // a directory elsewhere
		"Linked.csproj": filepath.Join(outside, "Target.csproj"), // a symlink named like a project
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks: %v", err)
		}
	}

	for _, dll := range []string{"Escaped.dll", "Linked.dll", "Target.dll"} {
		if got, err := FindContainerProject(root, dll, 50_000); err == nil {
			t.Errorf("FindContainerProject(%q) = %q through a symlink", dll, got)
		}
	}

	// And the loop doesn't run away: the real project is still found.
	if got, err := FindContainerProject(root, "Real.dll", 50_000); err != nil || got != filepath.Join(root, "Real", "Real.csproj") {
		t.Errorf("Real: %q, %v", got, err)
	}
}

func TestFindContainerProjectResolvesTheRoot(t *testing.T) {
	t.Parallel()

	root := realDir(t)
	touch(t, filepath.Join(root, "Web", "Web.csproj"))

	link := filepath.Join(realDir(t), "rootlink")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks: %v", err)
	}

	// The root itself may be a symlink: the answer is below its real path.
	if got, err := FindContainerProject(link, "Web.dll", 100); err != nil || got != filepath.Join(root, "Web", "Web.csproj") {
		t.Errorf("got %q, %v", got, err)
	}

	if _, err := FindContainerProject(filepath.Join(root, "nowhere"), "Web.dll", 100); api.CodeOf(err) != api.CodeInvalidRequest {
		t.Errorf("a missing root: %v", err)
	}
}

func TestFindContainerProjectSkipsUnreadableDirectories(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs Unix modes and a user that is not root")
	}

	root := realDir(t)
	touch(t, filepath.Join(root, "Web", "Web.csproj"))
	touch(t, filepath.Join(root, "locked", "x.txt"))

	if err := os.Chmod(filepath.Join(root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o700) }) //nolint:gosec // Restore access so the directory can be removed.

	if got, err := FindContainerProject(root, "Web.dll", 100); err != nil || filepath.Base(got) != "Web.csproj" {
		t.Errorf("got %q, %v", got, err)
	}
}

// publishFixture is a compose directory with a project, and the paths a
// publish of it needs.
type publishFixture struct {
	root, project, out, artifacts string
	log                           *os.File
	record                        string
}

func newPublishFixture(t *testing.T) publishFixture {
	t.Helper()

	f := publishFixture{root: realDir(t)}
	f.project = filepath.Join(f.root, "Web", "Web.csproj")
	touch(t, f.project)

	home := realDir(t)
	f.out = filepath.Join(home, "stage-0123abcd", "web")
	f.artifacts = filepath.Join(home, "artifacts")
	f.record = filepath.Join(home, "record.json")

	log, err := os.OpenFile(filepath.Join(home, "build.log"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = log.Close() })

	f.log = log

	return f
}

// spec is a publish through the fake dotnet (the test binary): sc is the
// fake's scenario; its Record is set to the fixture's.
func (f publishFixture) spec(t *testing.T, sc fakeDotnetScenario) PublishSpec {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	sc.Record = f.record

	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}

	return PublishSpec{
		Host: exe, Project: f.project, Root: f.root, Out: f.out, Artifacts: f.artifacts, DLL: "Web.dll", Log: f.log,
		env: []string{envFakeDotnet + "=" + string(raw)},
	}
}

func (f publishFixture) recorded(t *testing.T) (fakeDotnetRecord, bool) {
	t.Helper()

	raw, err := os.ReadFile(f.record)
	if err != nil {
		return fakeDotnetRecord{}, false
	}

	var rec fakeDotnetRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}

	return rec, true
}

var okOutputs = []string{"Web.dll", "Web.pdb", "Web.deps.json"}

func TestPublishForContainerArgv(t *testing.T) {
	t.Parallel()

	f := newPublishFixture(t)

	if err := PublishForContainer(t.Context(), f.spec(t, fakeDotnetScenario{Write: okOutputs, Stdout: "building\n", Stderr: "warning\n"})); err != nil {
		t.Fatal(err)
	}

	rec, ran := f.recorded(t)
	if !ran {
		t.Fatal("dotnet did not run")
	}

	want := []string{
		"publish", f.project, "-c", "Debug", "-o", f.out, "--artifacts-path", f.artifacts,
		"-p:UseAppHost=false", "-p:DebugType=portable",
		"-p:PathMap=" + f.root + string(filepath.Separator) + "=/src/",
		"-nologo", "-tl:off",
	}

	if !slices.Equal(rec.Argv, want) {
		t.Errorf("argv = %q\nwant   %q", rec.Argv, want)
	}

	// Built in the compose directory (where global.json is looked for), with
	// the same environment every build here gets.
	if rec.Cwd != f.root || rec.Telemetry != "1" || rec.NoLogo != "1" {
		t.Errorf("cwd %q, telemetry %q, nologo %q", rec.Cwd, rec.Telemetry, rec.NoLogo)
	}

	// Both streams went to the log.
	log, err := os.ReadFile(f.log.Name())
	if err != nil || string(log) != "building\nwarning\n" {
		t.Errorf("log = %q, %v", log, err)
	}
}

// TestPublishForContainerUsesRealpaths: the PathMap names the resolved root
// and the project is the resolved file, so the PDBs and the map agree.
func TestPublishForContainerUsesRealpaths(t *testing.T) {
	t.Parallel()

	f := newPublishFixture(t)

	link := filepath.Join(realDir(t), "rootlink")
	if err := os.Symlink(f.root, link); err != nil {
		t.Skipf("symlinks: %v", err)
	}

	spec := f.spec(t, fakeDotnetScenario{Write: okOutputs})
	spec.Root, spec.Project = link, filepath.Join(link, "Web", "Web.csproj")

	if err := PublishForContainer(t.Context(), spec); err != nil {
		t.Fatal(err)
	}

	rec, _ := f.recorded(t)
	if rec.Argv[1] != f.project || !slices.Contains(rec.Argv, "-p:PathMap="+f.root+string(filepath.Separator)+"=/src/") {
		t.Errorf("argv = %q", rec.Argv)
	}
}

// withAmbiguousChar puts c in one of the paths of spec (where), keeping the
// project inside the root.
func withAmbiguousChar(t *testing.T, f publishFixture, spec *PublishSpec, where, c string) {
	t.Helper()

	switch where {
	case "root":
		// The root's name holds the character; the project lies in it.
		bad := filepath.Join(realDir(t), "my"+c+"app")
		touch(t, filepath.Join(bad, "Web", "Web.csproj"))
		spec.Root, spec.Project = bad, filepath.Join(bad, "Web", "Web.csproj")
	case "project":
		touch(t, filepath.Join(f.root, "Web", "W"+c+"eb.csproj"))
		spec.Project = filepath.Join(f.root, "Web", "W"+c+"eb.csproj")
	case "out":
		spec.Out = f.out + c + "x"
	case "artifacts":
		spec.Artifacts = f.artifacts + c + "x"
	}
}

// TestPublishForContainerRefusesAmbiguousPaths: a path that MSBuild would
// split (',' ';'), unescape ('%'), or Roslyn's path map would misread ('='),
// or that holds a quote or a control character, is refused — in every path
// that reaches MSBuild — and dotnet never runs.
func TestPublishForContainerRefusesAmbiguousPaths(t *testing.T) {
	t.Parallel()

	for _, c := range []string{",", ";", "%", "=", `"`, "\n", "\t", "\x07", "%3B"} {
		if runtime.GOOS == "windows" && strings.ContainsAny(c, "\"\n\t\x07") {
			continue
		}

		t.Run(fmt.Sprintf("%q", c), func(t *testing.T) {
			t.Parallel()

			for _, where := range []string{"root", "project", "out", "artifacts"} {
				f := newPublishFixture(t)
				spec := f.spec(t, fakeDotnetScenario{Write: okOutputs})
				withAmbiguousChar(t, f, &spec, where, c)

				err := PublishForContainer(t.Context(), spec)
				if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "MSBuild would split") {
					t.Errorf("%s with %q: err = %v (%s)", where, c, err, api.CodeOf(err))
				}

				if _, ran := f.recorded(t); ran {
					t.Errorf("%s with %q: dotnet ran", where, c)
				}
			}
		})
	}
}

// TestPublishForContainerAcceptsOrdinaryPaths: spaces, '$', '(' and unicode
// are fine (verified with MSBuild: they stay literal).
func TestPublishForContainerAcceptsOrdinaryPaths(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"my app", "a$b", "a$(X)b", "a'b", "a@b", "a&b", "a#b", "ünï", "a+b", "a[b]", "a~b", "a!b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := newPublishFixture(t)
			dir := filepath.Join(realDir(t), name)
			touch(t, filepath.Join(dir, "Web", "Web.csproj"))

			spec := f.spec(t, fakeDotnetScenario{Write: okOutputs})
			spec.Root, spec.Project = dir, filepath.Join(dir, "Web", "Web.csproj")

			if err := PublishForContainer(t.Context(), spec); err != nil {
				t.Fatal(err)
			}

			rec, _ := f.recorded(t)
			if !slices.Contains(rec.Argv, "-p:PathMap="+dir+string(filepath.Separator)+"=/src/") {
				t.Errorf("argv = %q", rec.Argv)
			}
		})
	}
}

func TestPublishForContainerRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mod  func(f publishFixture, s *PublishSpec)
		want string
	}{
		{"project outside the root", func(_ publishFixture, s *PublishSpec) {
			other := realDir(t)
			touch(t, filepath.Join(other, "Web.csproj"))
			s.Project = filepath.Join(other, "Web.csproj")
		}, "not inside the compose directory"},
		{"project in a sibling that starts with the root's name", func(f publishFixture, s *PublishSpec) {
			sibling := f.root + "2"
			touch(t, filepath.Join(sibling, "Web.csproj"))
			t.Cleanup(func() { _ = os.RemoveAll(sibling) })
			s.Project = filepath.Join(sibling, "Web.csproj")
		}, "not inside the compose directory"},
		{"project is the root", func(f publishFixture, s *PublishSpec) { s.Project = f.root }, "not inside"},
		{"project through dotdot", func(f publishFixture, s *PublishSpec) {
			s.Project = filepath.Join(f.root, "Web", "..", "..", filepath.Base(f.root)+"2", "Web.csproj")
		}, "can't be resolved"},
		{"project is not a project file", func(f publishFixture, s *PublishSpec) {
			touch(t, filepath.Join(f.root, "Web", "Program.cs"))
			s.Project = filepath.Join(f.root, "Web", "Program.cs")
		}, "not a project file"},
		{"project missing", func(f publishFixture, s *PublishSpec) { s.Project = filepath.Join(f.root, "Nope.csproj") }, "can't be resolved"},
		{"root is a file system root", func(_ publishFixture, s *PublishSpec) { s.Root = filepath.VolumeName(os.TempDir()) + string(filepath.Separator) }, "file system root"},
		{"root missing", func(f publishFixture, s *PublishSpec) { s.Root = filepath.Join(f.root, "nowhere") }, "can't be resolved"},
		{"relative root", func(_ publishFixture, s *PublishSpec) { s.Root = "rel" }, ""},
		{"relative project", func(_ publishFixture, s *PublishSpec) { s.Project = "Web/Web.csproj" }, ""},
		{"relative out", func(_ publishFixture, s *PublishSpec) { s.Out = "out" }, ""},
		{"relative artifacts", func(_ publishFixture, s *PublishSpec) { s.Artifacts = "artifacts" }, ""},
		{"dll with a slash", func(_ publishFixture, s *PublishSpec) { s.DLL = "a/Web.dll" }, "invalid assembly name"},
		{"dll without .dll", func(_ publishFixture, s *PublishSpec) { s.DLL = "Web" }, "invalid assembly name"},
		{"no log", func(_ publishFixture, s *PublishSpec) { s.Log = nil }, ""},
		{"no host", func(_ publishFixture, s *PublishSpec) { s.Host = "" }, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newPublishFixture(t)
			spec := f.spec(t, fakeDotnetScenario{Write: okOutputs})
			tt.mod(f, &spec)

			err := PublishForContainer(t.Context(), spec)
			if err == nil || (tt.want != "" && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}

			if _, ran := f.recorded(t); ran {
				t.Error("dotnet ran")
			}
		})
	}
}

func TestPublishForContainerRefusesAProjectThatResolvesOutside(t *testing.T) {
	t.Parallel()

	f := newPublishFixture(t)
	outside := realDir(t)
	touch(t, filepath.Join(outside, "Evil.csproj"))

	link := filepath.Join(f.root, "Web", "Evil.csproj")
	if err := os.Symlink(filepath.Join(outside, "Evil.csproj"), link); err != nil {
		t.Skipf("symlinks: %v", err)
	}

	spec := f.spec(t, fakeDotnetScenario{Write: okOutputs})
	spec.Project = link

	err := PublishForContainer(t.Context(), spec)
	if api.CodeOf(err) != api.CodeInvalidRequest || !strings.Contains(err.Error(), "not inside") {
		t.Errorf("err = %v", err)
	}

	if _, ran := f.recorded(t); ran {
		t.Error("dotnet ran")
	}
}

func TestPublishForContainerFailure(t *testing.T) {
	t.Parallel()

	f := newPublishFixture(t)

	var out strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&out, "line %03d\n", i)
	}

	out.WriteString("error CS1002: ; expected \x1b[31mred\x07\n")

	err := PublishForContainer(t.Context(), f.spec(t, fakeDotnetScenario{Exit: 1, Stdout: out.String(), Stderr: "stderr line\n"}))

	var ae *api.Error
	if !errors.As(err, &ae) || ae.Code != api.CodeBuildFailed {
		t.Fatalf("err = %#v", err)
	}

	msg := ae.Message
	if !strings.Contains(msg, "error CS1002") || !strings.Contains(msg, "line 100") || !strings.Contains(msg, "stderr line") {
		t.Errorf("the end of the log is missing: %s", msg)
	}

	// 40 lines, not the whole log.
	if strings.Contains(msg, "line 050") || strings.Count(msg, "\n") > 41 {
		t.Errorf("more than the last 40 lines: %d newlines", strings.Count(msg, "\n"))
	}

	for _, r := range msg {
		if r != '\n' && (r < ' ' || r == 0x7f) {
			t.Errorf("control character %q in the message", r)
		}
	}

	if !strings.Contains(ae.Hint, f.log.Name()) {
		t.Errorf("hint = %q, want the log path", ae.Hint)
	}
}

func TestPublishForContainerNeedsItsOutputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		write []string
		want  string
	}{
		{"nothing written", nil, "no Web.dll"},
		{"no pdb", []string{"Web.dll"}, "no Web.pdb"},
		{"no dll", []string{"Web.pdb"}, "no Web.dll"},
		{"another assembly", []string{"Other.dll", "Other.pdb"}, "no Web.dll"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := newPublishFixture(t)

			err := PublishForContainer(t.Context(), f.spec(t, fakeDotnetScenario{Write: tt.write}))
			if api.CodeOf(err) != api.CodeBuildFailed || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v (%s), want BUILD_FAILED %q", err, api.CodeOf(err), tt.want)
			}
		})
	}
}

func TestPublishForContainerOutputsAreRegularFiles(t *testing.T) {
	t.Parallel()

	f := newPublishFixture(t)
	spec := f.spec(t, fakeDotnetScenario{Write: []string{"Web.pdb"}})

	// Web.dll is a symlink to a file elsewhere: not an output.
	if err := os.MkdirAll(f.out, 0o750); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(realDir(t), "elsewhere.dll")
	touch(t, target)

	if err := os.Symlink(target, filepath.Join(f.out, "Web.dll")); err != nil {
		t.Skipf("symlinks: %v", err)
	}

	if err := PublishForContainer(t.Context(), spec); api.CodeOf(err) != api.CodeBuildFailed {
		t.Errorf("err = %v", err)
	}
}

func TestPublishForContainerContextEnds(t *testing.T) {
	t.Parallel()

	t.Run("canceled by the caller", func(t *testing.T) {
		t.Parallel()

		f := newPublishFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		err := PublishForContainer(ctx, f.spec(t, fakeDotnetScenario{Write: okOutputs}))
		if !errors.Is(err, context.Canceled) || api.CodeOf(err) != "" {
			t.Errorf("err = %v (%s)", err, api.CodeOf(err))
		}
	})

	t.Run("time limit", func(t *testing.T) {
		t.Parallel()

		f := newPublishFixture(t)
		spec := f.spec(t, fakeDotnetScenario{Write: okOutputs})
		spec.timeout = 1 // a nanosecond: over before dotnet can finish

		err := PublishForContainer(t.Context(), spec)
		if api.CodeOf(err) != api.CodeBuildFailed || !strings.Contains(err.Error(), "did not finish in time") {
			t.Errorf("err = %v (%s)", err, api.CodeOf(err))
		}
	})
}

func TestPublishForContainerHostThatCannotRun(t *testing.T) {
	t.Parallel()

	f := newPublishFixture(t)
	spec := f.spec(t, fakeDotnetScenario{})
	spec.Host = filepath.Join(f.root, "no-such-dotnet")

	var ae *api.Error

	err := PublishForContainer(t.Context(), spec)
	if !errors.As(err, &ae) || ae.Code != api.CodeBuildFailed || !strings.Contains(ae.Hint, "dotnet --info") {
		t.Errorf("err = %v", err)
	}
}
