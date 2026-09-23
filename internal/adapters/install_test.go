// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// archiveFile is one file of a test archive.
type archiveFile struct {
	name, content string
}

func zipOf(t *testing.T, files ...archiveFile) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)
	for _, f := range files {
		w, err := zw.Create(f.name)
		if err != nil {
			t.Fatal(err)
		}

		if _, err := w.Write([]byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func tarGzOf(t *testing.T, files ...archiveFile) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}

		if _, err := tw.Write([]byte(f.content)); err != nil {
			t.Fatal(err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// serve serves body over https and counts the requests.
func serve(t *testing.T, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

// nativeManifest is a native adapter whose one download is d.
func nativeManifest(d Download) *Manifest {
	return &Manifest{
		Name: "netcoredbg", Version: "3.2.0-1092", Homepage: "https://github.com/Samsung/netcoredbg",
		Adapter: Adapter{ID: "coreclr", Entry: "netcoredbg", Env: "EYEDBG_NETCOREDBG"},
		Install: &InstallSpec{Downloads: map[string]Download{"*": d}},
	}
}

func TestInstall(t *testing.T) {
	t.Parallel()

	exe := exeName("netcoredbg")

	tests := []struct {
		name    string
		body    []byte
		archive string
		root    string
		python  bool
		want    string // installed entry, relative to the install dir
	}{
		{"zip with a top-level directory", zipOf(t, archiveFile{"netcoredbg/" + exe, "bin"}, archiveFile{"netcoredbg/lib.so", "x"}), archiveZip, "netcoredbg", false, exe},
		{"tar.gz with a top-level directory", tarGzOf(t, archiveFile{"netcoredbg/" + exe, "bin"}), archiveTarGz, "netcoredbg", false, exe},
		{"wheel-like zip, whole archive", zipOf(t, archiveFile{"toy/adapter/__main__.py", "x"}, archiveFile{"toy-1.0.dist-info/METADATA", "m"}), archiveZip, "", true, filepath.Join("toy", "adapter")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, hits := serve(t, tt.body)
			m := nativeManifest(Download{URL: srv.URL + "/a", SHA256: digest(tt.body), Archive: tt.archive, Root: tt.root, Size: int64(len(tt.body))})

			if tt.python {
				m.Adapter = Adapter{ID: "toy", Runtime: RuntimePython, Entry: "toy/adapter"}
			}

			dir := filepath.Join(t.TempDir(), m.Name, m.Version)

			got, err := install(t.Context(), srv.Client(), m, "linux/amd64", dir)
			if err != nil || got != filepath.Join(dir, tt.want) {
				t.Fatalf("install = %q, %v; want %s", got, err, filepath.Join(dir, tt.want))
			}

			again, err := install(t.Context(), srv.Client(), m, "linux/amd64", dir)
			if err != nil || again != got || hits.Load() != 1 {
				t.Fatalf("second install = %q, %v after %d downloads; want a no-op", again, err, hits.Load())
			}

			checkNoLeftovers(t, filepath.Dir(dir), m.Version)
		})
	}
}

// checkNoLeftovers checks that parent holds only keep (or nothing).
func checkNoLeftovers(t *testing.T, parent string, keep ...string) {
	t.Helper()

	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}

	for _, e := range entries {
		if len(keep) == 0 || e.Name() != keep[0] {
			t.Errorf("left behind: %s", filepath.Join(parent, e.Name()))
		}
	}
}

func TestInstallFailuresLeaveNothing(t *testing.T) {
	t.Parallel()

	body := zipOf(t, archiveFile{"netcoredbg/" + exeName("netcoredbg"), "bin"})

	tests := []struct {
		name string
		d    func(url string) Download
		want string
	}{
		{"wrong digest", func(url string) Download {
			return Download{URL: url, SHA256: strings.Repeat("0", 64), Archive: archiveZip, Root: "netcoredbg"}
		}, "does not match the pinned"},
		{"larger than its size", func(url string) Download {
			return Download{URL: url, SHA256: digest(body), Archive: archiveZip, Root: "netcoredbg", Size: int64(len(body) - 1)}
		}, "larger than the expected"},
		{"no entry in the archive", func(url string) Download {
			return Download{URL: url, SHA256: digest(body), Archive: archiveZip, Root: "netcoredbg"}
		}, "installed archive has no"},
		{"root not in the archive", func(url string) Download {
			return Download{URL: url, SHA256: digest(body), Archive: archiveZip, Root: "other"}
		}, "install netcoredbg"},
		{"http", func(string) Download {
			return Download{URL: "http://example.invalid/a", SHA256: digest(body), Archive: archiveZip, Root: "netcoredbg"}
		}, "only https"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv, _ := serve(t, body)
			m := nativeManifest(tt.d(srv.URL + "/a"))

			if tt.name == "no entry in the archive" {
				m.Adapter.Entry = "missing"
			}

			parent := t.TempDir()

			_, err := install(t.Context(), srv.Client(), m, "linux/amd64", filepath.Join(parent, m.Version))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("install = %v, want an error containing %q", err, tt.want)
			}

			checkNoLeftovers(t, parent)
		})
	}
}

// TestNoReleaseMessage pins netcoredbg's message for a platform without a
// prebuilt release to the one eyedbg printed before manifests.
func TestNoReleaseMessage(t *testing.T) {
	t.Parallel()

	m := Load(LoadConfig{Bundled: Bundled(), Builtin: builtinLangs}).Adapter("netcoredbg")

	_, err := install(t.Context(), http.DefaultClient, m, "plan9/amd64", t.TempDir())

	const want = "netcoredbg 3.2.0-1092 has no prebuilt release for plan9/amd64; build it from https://github.com/Samsung/netcoredbg and set EYEDBG_NETCOREDBG"
	if err == nil || err.Error() != want {
		t.Fatalf("install on plan9 = %v\nwant %s", err, want)
	}
}

func TestExtractRejectsEscapes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	archive := filepath.Join(dir, "a.zip")

	if err := os.WriteFile(archive, zipOf(t, archiveFile{"../evil", "x"}), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := extractZip(archive, filepath.Join(dir, "out")); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("extractZip(../evil) = %v, want an escape error", err)
	}
}
