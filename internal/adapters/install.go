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
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// maxDownload caps a download whose manifest gives no size.
const maxDownload = 1 << 30

// Install downloads m's pinned build for this platform, checks its size
// and SHA-256 and extracts it into InstallDir(m). It returns the installed
// entry's path, and is a no-op when that already exists. Nothing is left
// behind on failure.
func Install(ctx context.Context, client *http.Client, m *Manifest) (string, error) {
	dir, err := InstallDir(m)
	if err != nil {
		return "", err
	}

	return install(ctx, client, m, runtime.GOOS+"/"+runtime.GOARCH, dir)
}

// NoRelease is the error for a platform m has no download for.
func NoRelease(m *Manifest, platform string) error {
	msg := fmt.Sprintf("%s %s has no prebuilt release for %s", m.Name, m.Version, platform)

	switch {
	case m.Homepage != "" && m.Adapter.Env != "":
		msg += fmt.Sprintf("; build it from %s and set %s", m.Homepage, m.Adapter.Env)
	case m.Homepage != "":
		msg += "; see " + m.Homepage
	}

	return errors.New(msg)
}

func install(ctx context.Context, client *http.Client, m *Manifest, platform, dir string) (string, error) {
	goos, goarch, _ := strings.Cut(platform, "/")

	d, ok := m.DownloadFor(goos, goarch)
	if !ok {
		return "", NoRelease(m, platform)
	}

	if m.Adapter.Runtime == RuntimeNative && filepath.IsAbs(m.Adapter.Entry) {
		return "", fmt.Errorf("%s names an absolute adapter.entry; there is nothing to install", m.Name)
	}

	entry := installedEntry(m, dir)
	if _, err := os.Stat(entry); err == nil {
		return entry, nil
	}

	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", fmt.Errorf("create adapter directory: %w", err)
	}

	archive, err := download(ctx, client, d, parent)
	if err != nil {
		return "", err
	}
	defer os.Remove(archive)

	if err := unpack(m, d, archive, parent, dir); err != nil {
		return "", err
	}

	if _, err := os.Stat(entry); err != nil {
		// The directory was just renamed into place from the staging one.
		_ = os.RemoveAll(dir)

		return "", fmt.Errorf("installed archive has no %s: %w", filepath.Base(entry), err)
	}

	return entry, nil
}

// unpack extracts archive into a staging directory next to dir, then
// renames d.Root (or the whole staging directory) to dir.
func unpack(m *Manifest, d Download, archive, parent, dir string) error {
	staging, err := os.MkdirTemp(parent, "."+m.Name+"-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(staging)

	if d.Archive == archiveZip {
		err = extractZip(archive, staging)
	} else {
		err = extractTarGz(archive, staging)
	}

	if err != nil {
		return fmt.Errorf("extract %s: %w", m.Name, err)
	}

	src := staging
	if d.Root != "" {
		src = filepath.Join(staging, filepath.FromSlash(d.Root))
	}

	if err := os.Rename(src, dir); err != nil {
		return fmt.Errorf("install %s: %w", m.Name, err)
	}

	return nil
}

// download fetches d into a temp file in dir and checks its size and
// digest.
func download(ctx context.Context, client *http.Client, d Download, dir string) (string, error) {
	if u, err := url.Parse(d.URL); err != nil || u.Scheme != "https" {
		return "", fmt.Errorf("download %s: only https URLs are fetched", d.URL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", d.URL, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", d.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", d.URL, resp.Status)
	}

	f, err := os.CreateTemp(dir, ".download-*")
	if err != nil {
		return "", fmt.Errorf("create download file: %w", err)
	}

	err = copyVerified(f, resp.Body, d)

	if cerr := f.Close(); err == nil {
		err = cerr
	}

	if err != nil {
		_ = os.Remove(f.Name())

		return "", fmt.Errorf("download %s: %w", d.URL, err)
	}

	return f.Name(), nil
}

// copyVerified copies at most d's size (else maxDownload) bytes of r to w
// and checks the SHA-256 of what it read.
func copyVerified(w io.Writer, r io.Reader, d Download) error {
	limit := int64(maxDownload)
	if d.Size > 0 {
		limit = d.Size
	}

	h := sha256.New()

	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}

	if n > limit {
		return fmt.Errorf("larger than the expected %d bytes", limit)
	}

	if got := hex.EncodeToString(h.Sum(nil)); got != d.SHA256 {
		return fmt.Errorf("sha256 %s does not match the pinned %s", got, d.SHA256)
	}

	return nil
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
