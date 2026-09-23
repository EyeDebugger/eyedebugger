// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package adapters

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// EnvNetcoredbg points at a netcoredbg executable, overriding discovery.
const EnvNetcoredbg = "EYEDBG_NETCOREDBG"

// EnvDataDir overrides where adapters are installed.
const EnvDataDir = "EYEDBG_DATA_DIR"

// NetcoredbgVersion is the pinned netcoredbg release.
const NetcoredbgVersion = "3.2.0-1092"

// ErrNotInstalled means an adapter was not found anywhere.
var ErrNotInstalled = errors.New("adapter not installed")

// release is one downloadable build of an adapter.
type release struct {
	url    string
	sha256 string
}

// netcoredbgReleases are the builds Samsung publishes for NetcoredbgVersion,
// with the SHA-256 digests GitHub reports for them. Other platforms must
// build netcoredbg themselves and set EYEDBG_NETCOREDBG.
func netcoredbgRelease(goos, goarch string) (release, bool) {
	const base = "https://github.com/Samsung/netcoredbg/releases/download/" + NetcoredbgVersion + "/"

	switch goos + "/" + goarch {
	case "linux/amd64":
		return release{base + "netcoredbg-linux-amd64.tar.gz", "080eb3b2d2152465f599d3b33d1ee6e747794e11cc0a3773ec689f5e5f2c5afa"}, true
	case "linux/arm64":
		return release{base + "netcoredbg-linux-arm64.tar.gz", "065ff49badec8a695dbea2de6ab6a330c774a191e426a217ab8cc05250627ccb"}, true
	case "darwin/arm64":
		return release{base + "netcoredbg-osx-arm64.zip", "f4fa33b3ff874910cc184b4bb3b9c56d0abdf5c6521cee0b144d7c6e4a6e59ea"}, true
	case "windows/amd64":
		return release{base + "netcoredbg-win64.zip", "3c410a45fa502415203a94fcb88654af65bf8e3dac158a5527a722e7a6b9274a"}, true
	default:
		return release{}, false
	}
}

// DataDir returns where adapters are installed: $EYEDBG_DATA_DIR, else
// <user cache dir>/eyedbg/adapters.
func DataDir() (string, error) {
	if dir := os.Getenv(EnvDataDir); dir != "" {
		return dir, nil
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate adapter directory (set %s): %w", EnvDataDir, err)
	}

	return filepath.Join(cache, "eyedbg", "adapters"), nil
}

// Location says where an adapter was found and how.
type Location struct {
	Path string `json:"path"`
	// Source is "env" (EYEDBG_NETCOREDBG), "installed" (eyedbg adapters
	// install) or "path" (found on PATH).
	Source string `json:"source"`
}

// FindNetcoredbg locates netcoredbg: $EYEDBG_NETCOREDBG, else the pinned
// installed copy, else PATH. It returns ErrNotInstalled if none exists.
func FindNetcoredbg() (Location, error) {
	if p := os.Getenv(EnvNetcoredbg); p != "" {
		if _, err := os.Stat(p); err != nil { //nolint:gosec // The user's own setting names the file to use.
			return Location{}, fmt.Errorf("%s=%s: %w", EnvNetcoredbg, p, err)
		}

		return Location{Path: p, Source: "env"}, nil
	}

	if dir, err := netcoredbgDir(); err == nil {
		p := filepath.Join(dir, exeName("netcoredbg"))
		if _, err := os.Stat(p); err == nil {
			return Location{Path: p, Source: "installed"}, nil
		}
	}

	if p, err := exec.LookPath(exeName("netcoredbg")); err == nil {
		return Location{Path: p, Source: "path"}, nil
	}

	return Location{}, ErrNotInstalled
}

// netcoredbgDir is where the pinned release is (or will be) extracted.
func netcoredbgDir() (string, error) {
	data, err := DataDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(data, "netcoredbg", NetcoredbgVersion), nil
}

// InstallNetcoredbg downloads the pinned netcoredbg release for this
// platform, verifies its SHA-256 and extracts it. It is a no-op when already
// installed.
func InstallNetcoredbg(ctx context.Context, client *http.Client) (string, error) {
	rel, ok := netcoredbgRelease(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return "", fmt.Errorf("netcoredbg %s has no prebuilt release for %s/%s; build it from https://github.com/Samsung/netcoredbg and set %s",
			NetcoredbgVersion, runtime.GOOS, runtime.GOARCH, EnvNetcoredbg)
	}

	dir, err := netcoredbgDir()
	if err != nil {
		return "", err
	}

	exe := filepath.Join(dir, exeName("netcoredbg"))
	if _, err := os.Stat(exe); err == nil {
		return exe, nil
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", fmt.Errorf("create adapter directory: %w", err)
	}

	archive, err := download(ctx, client, rel, filepath.Dir(dir))
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)

	staging, err := os.MkdirTemp(filepath.Dir(dir), ".netcoredbg-*")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	if strings.HasSuffix(rel.url, ".zip") {
		err = extractZip(archive, staging)
	} else {
		err = extractTarGz(archive, staging)
	}

	if err != nil {
		return "", fmt.Errorf("extract netcoredbg: %w", err)
	}

	// The archives hold a single top-level netcoredbg/ directory.
	if err := os.Rename(filepath.Join(staging, "netcoredbg"), dir); err != nil {
		return "", fmt.Errorf("install netcoredbg: %w", err)
	}

	if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("installed archive has no %s: %w", filepath.Base(exe), err)
	}

	return exe, nil
}

// download fetches rel into a temp file in dir and checks its digest.
func download(ctx context.Context, client *http.Client, rel release, dir string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.url, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", rel.url, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", rel.url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", rel.url, resp.Status)
	}

	f, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("create download file: %w", err)
	}

	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), resp.Body)

	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = os.Remove(f.Name())

		return "", fmt.Errorf("download %s: %w", rel.url, err)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != rel.sha256 {
		_ = os.Remove(f.Name())

		return "", fmt.Errorf("download %s: sha256 %s does not match the pinned %s", rel.url, got, rel.sha256)
	}

	return f.Name(), nil
}

// safeJoin joins name under dir, refusing paths that escape it.
func safeJoin(dir, name string) (string, error) {
	p := filepath.Join(dir, filepath.FromSlash(name))
	if p != dir && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the target directory", name)
	}

	return p, nil
}

func extractTarGz(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		p, err := safeJoin(dir, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeFile(p, tr, hdr.FileInfo().Mode().Perm()); err != nil {
				return err
			}
		default:
			// Links and special files are not expected in the release; skip them.
		}
	}
}

func extractZip(archive, dir string) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, zf := range zr.File {
		p, err := safeJoin(dir, zf.Name)
		if err != nil {
			return err
		}

		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(p, 0o750); err != nil {
				return err
			}

			continue
		}

		rc, err := zf.Open()
		if err != nil {
			return err
		}

		mode := zf.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}

		err = writeFile(p, rc, mode|0o100) // zip archives from Windows lose the x bit; netcoredbg needs it on macOS
		_ = rc.Close()

		if err != nil {
			return err
		}
	}

	return nil
}

func writeFile(p string, r io.Reader, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	// The archive is verified by digest before extraction, so its size is
	// trusted.
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}

	return name
}
