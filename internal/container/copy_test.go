// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container_test

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eyedebugger/eyedebugger/internal/api"
	"github.com/eyedebugger/eyedebugger/internal/container"
	"github.com/eyedebugger/eyedebugger/internal/container/containertest"
)

// adapterDir makes an installed-adapter-like directory: modes the tar must
// not copy (0700 directories, 0600 files, a 0700 entry).
func adapterDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	files := map[string]string{"netcoredbg": "#!entry", "libdbgshim.so": "so", "lib/sub/x.dll": "dll"}

	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func testAdapter(dir string) container.Adapter {
	return container.Adapter{Name: "netcoredbg", Version: "3.2.0-1092", Dir: dir, Entry: "netcoredbg", ProbeArgs: []string{"--version"}}
}

type tarEntry struct {
	mode     int64
	uid, gid int
	dir      bool
	body     string
}

func readTar(t *testing.T, data []byte) map[string]tarEntry {
	t.Helper()

	out := map[string]tarEntry{}
	tr := tar.NewReader(bytes.NewReader(data))

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}

		if err != nil {
			t.Fatal(err)
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}

		out[h.Name] = tarEntry{mode: h.Mode, uid: h.Uid, gid: h.Gid, dir: h.Typeflag == tar.TypeDir, body: string(body)}
	}
}

func TestBuildTar(t *testing.T) {
	t.Parallel()

	a := testAdapter(adapterDir(t))

	data, err := a.BuildTar()
	if err != nil {
		t.Fatal(err)
	}

	got := readTar(t, data)

	want := map[string]tarEntry{
		".eyedbg-netcoredbg-3.2.0-1092/":              {mode: 0o755, dir: true},
		".eyedbg-netcoredbg-3.2.0-1092/lib/":          {mode: 0o755, dir: true},
		".eyedbg-netcoredbg-3.2.0-1092/lib/sub/":      {mode: 0o755, dir: true},
		".eyedbg-netcoredbg-3.2.0-1092/lib/sub/x.dll": {mode: 0o644, body: "dll"},
		".eyedbg-netcoredbg-3.2.0-1092/libdbgshim.so": {mode: 0o644, body: "so"},
		".eyedbg-netcoredbg-3.2.0-1092/netcoredbg":    {mode: 0o755, body: "#!entry"},
	}

	if len(got) != len(want) {
		t.Errorf("tar has %d entries, want %d: %v", len(got), len(want), got)
	}

	for name, w := range want {
		if g, ok := got[name]; !ok || g != w {
			t.Errorf("entry %q = %+v (present %v), want %+v", name, g, ok, w)
		}
	}

	if a.ContainerPath() != "/.eyedbg-netcoredbg-3.2.0-1092/netcoredbg" {
		t.Errorf("ContainerPath = %q", a.ContainerPath())
	}

	again, err := a.BuildTar()
	if err != nil || !bytes.Equal(data, again) {
		t.Errorf("a second BuildTar differs (err %v): the tar must be reproducible", err)
	}
}

func TestBuildTarMissingEntry(t *testing.T) {
	t.Parallel()

	a := testAdapter(adapterDir(t))
	a.Entry = "nothing"

	if _, err := a.BuildTar(); err == nil || !strings.Contains(err.Error(), "no nothing") {
		t.Errorf("err = %v", err)
	}
}

func TestBuildTarSymlink(t *testing.T) {
	t.Parallel()

	dir := adapterDir(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}

	if _, err := testAdapter(dir).BuildTar(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("err = %v, want a refusal of the symlink", err)
	}
}

func TestBuildTarTooLarge(t *testing.T) {
	t.Parallel()

	dir := adapterDir(t)

	for _, name := range []string{"big1", "big2"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}

		err = f.Truncate(container.MaxTarBytes/2 + 1)
		_ = f.Close()

		if err != nil {
			t.Skipf("can't make a sparse file: %v", err)
		}
	}

	if _, err := testAdapter(dir).BuildTar(); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("err = %v, want the size cap", err)
	}
}

func TestBuildTarNames(t *testing.T) {
	t.Parallel()

	dir := adapterDir(t)

	for _, bad := range []container.Adapter{
		{Name: "net core", Version: "1", Dir: dir, Entry: "netcoredbg"},
		{Name: "netcoredbg", Version: "../1", Dir: dir, Entry: "netcoredbg"},
		{Name: "netcoredbg", Version: "", Dir: dir, Entry: "netcoredbg"},
		{Name: "netcoredbg", Version: "1", Dir: dir, Entry: "../netcoredbg"},
		{Name: "netcoredbg", Version: "1", Dir: dir, Entry: "/netcoredbg"},
	} {
		if _, err := bad.BuildTar(); api.CodeOf(err) != api.CodeInvalidRequest {
			t.Errorf("BuildTar(%+v) err = %v, want INVALID_REQUEST", bad, err)
		}
	}
}

func TestInstallAdapterCopiesThenProbes(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	saved := filepath.Join(tmp, "stdin.tar")
	calls := filepath.Join(tmp, "calls")

	e := containertest.Engine(t, containertest.Scenario{Calls: calls, Rules: []containertest.Rule{
		{Match: []string{"cp", "-", fullID + ":/"}, SaveStdin: saved},
		{Match: []string{"exec", fullID, "/.eyedbg-netcoredbg-3.2.0-1092/netcoredbg", "--version"}, Stdout: "NET Core debugger\n"},
	}})
	e.Context = "desk"

	if err := e.InstallAdapter(t.Context(), fullID, testAdapter(adapterDir(t))); err != nil {
		t.Fatal(err)
	}

	want := [][]string{
		{"--context=desk", "cp", "-", fullID + ":/"},
		{"--context=desk", "exec", fullID, "/.eyedbg-netcoredbg-3.2.0-1092/netcoredbg", "--version"},
	}
	if got := containertest.ReadCalls(t, calls); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("docker calls = %q, want %q", got, want)
	}

	data, err := os.ReadFile(saved)
	if err != nil {
		t.Fatal(err)
	}

	if len(readTar(t, data)) != 6 {
		t.Error("the copied tar has the wrong entries")
	}
}

func TestInstallAdapterReadOnlyRoot(t *testing.T) {
	t.Parallel()

	e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{
		{Match: []string{"cp"}, Exit: 1, Stderr: "Error response from daemon: container rootfs is marked read-only"},
	}})

	err := e.InstallAdapter(t.Context(), fullID, testAdapter(adapterDir(t)))
	if api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "marked read-only") {
		t.Errorf("err = %v, want ATTACH_FAILED naming the read-only root", err)
	}
}

func TestInstallAdapterProbeFails(t *testing.T) {
	t.Parallel()

	e := containertest.Engine(t, containertest.Scenario{Rules: []containertest.Rule{
		{Match: []string{"cp"}},
		{Match: []string{"exec"}, Exit: 126, Stderr: "exec: no such file or directory (musl?)"},
	}})

	err := e.InstallAdapter(t.Context(), fullID, testAdapter(adapterDir(t)))
	if api.CodeOf(err) != api.CodeAttachFailed || !strings.Contains(err.Error(), "doesn't run in the container") {
		t.Fatalf("err = %v", err)
	}

	if apiErr, ok := errors.AsType[*api.Error](err); !ok || !strings.Contains(apiErr.Hint, "musl") {
		t.Errorf("error %v has no glibc/musl hint", err)
	}
}

func TestInstallAdapterOnlyFullIDs(t *testing.T) {
	t.Parallel()

	e := containertest.Engine(t, containertest.Scenario{})

	for _, id := range []string{"web-1", "0123456789ab", "--privileged", fullID + "0"} {
		if err := e.InstallAdapter(t.Context(), id, testAdapter(adapterDir(t))); err == nil {
			t.Errorf("InstallAdapter accepted %q", id)
		}
	}
}
