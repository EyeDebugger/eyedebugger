// Copyright The EyeDebugger Authors
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eyedebugger/eyedebugger/internal/api"
)

// MaxTarBytes bounds the files an adapter's tar may hold.
const MaxTarBytes = 64 << 20

// File modes of the tar: they are fixed, not the host's (a Windows host has
// none, and an installed directory is 0750 on Unix, which a non-root
// container user couldn't traverse).
const (
	dirMode   = 0o755
	entryMode = 0o755
	fileMode  = 0o644
)

// Adapter is an installed adapter directory to put in a container.
type Adapter struct {
	// Name and Version name the directory in the container:
	// /.eyedbg-<Name>-<Version>/.
	Name, Version string
	// Dir is the installed directory on the host.
	Dir string
	// Entry is the executable's path inside Dir (slash-separated).
	Entry string
	// ProbeArgs are the arguments that make the executable print its
	// version and exit 0.
	ProbeArgs []string
}

// check checks what names the adapter's directory in a container.
func (a Adapter) check() error {
	if !grammar(a.Name, isAlnum, isNameChar) || len(a.Name) > maxVersionLen || !validVersion(a.Version) {
		return api.NewError(api.CodeInvalidRequest, "the adapter's name or version is not fit to name a directory: "+
			show(a.Name+" "+a.Version, 80), "")
	}

	if a.Entry == "" || strings.HasPrefix(a.Entry, "/") || strings.Contains(a.Entry, "..") {
		return api.NewError(api.CodeInvalidRequest, "the adapter's entry is not a relative path inside its directory", "")
	}

	return nil
}

// TopDir is the adapter's directory name inside the container's root,
// without slashes: ".eyedbg-netcoredbg-3.2.0-1092".
func (a Adapter) TopDir() string { return ".eyedbg-" + a.Name + "-" + a.Version }

// ContainerPath is the adapter executable's path inside the container.
func (a Adapter) ContainerPath() string { return "/" + a.TopDir() + "/" + a.Entry }

// BuildTar is the tar that puts a's directory at the root of a container:
// the top directory and every directory 0755, the entry 0755, other files
// 0644, all owned by 0:0 with a fixed time. Only regular files and
// directories go in; anything else is an error, as is more than
// [MaxTarBytes] of files or a missing entry.
func (a Adapter) BuildTar() ([]byte, error) {
	if err := a.check(); err != nil {
		return nil, err
	}

	w := &tarPacker{a: a, entry: filepath.FromSlash(a.Entry)}
	w.tw = tar.NewWriter(&w.buf)

	if err := filepath.WalkDir(a.Dir, w.add); err != nil {
		return nil, fmt.Errorf("pack %s: %w", a.Dir, err)
	}

	if !w.sawEntry {
		return nil, fmt.Errorf("pack %s: it has no %s", a.Dir, a.Entry)
	}

	if err := w.tw.Close(); err != nil {
		return nil, fmt.Errorf("pack %s: %w", a.Dir, err)
	}

	return w.buf.Bytes(), nil
}

// tarPacker writes an adapter directory's entries as BuildTar says.
type tarPacker struct {
	a        Adapter
	entry    string // the executable's host-style relative path
	buf      bytes.Buffer
	tw       *tar.Writer
	total    int64
	sawEntry bool
}

// add is the filepath.WalkDir callback.
func (w *tarPacker) add(p string, d fs.DirEntry, err error) error {
	if err != nil {
		return err
	}

	rel, err := filepath.Rel(w.a.Dir, p)
	if err != nil {
		return err
	}

	name := w.a.TopDir()
	if rel != "." {
		name += "/" + filepath.ToSlash(rel)
	}

	// A fixed time keeps the tar reproducible.
	hdr := &tar.Header{Name: name, ModTime: time.Unix(0, 0)}

	switch {
	case d.IsDir():
		hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, name+"/", dirMode

		return w.tw.WriteHeader(hdr)
	case !d.Type().IsRegular():
		return fmt.Errorf("%s is not a regular file or directory", rel)
	}

	info, err := d.Info()
	if err != nil {
		return err
	}

	if w.total += info.Size(); w.total > MaxTarBytes {
		return fmt.Errorf("more than %d bytes of files", MaxTarBytes)
	}

	hdr.Typeflag, hdr.Mode, hdr.Size = tar.TypeReg, fileMode, info.Size()
	if rel == w.entry {
		hdr.Mode, w.sawEntry = entryMode, true
	}

	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}

	return copyFile(w.tw, p, info.Size())
}

// copyFile writes the size bytes of the file at p to w; a file that changed
// size since it was listed is an error.
func copyFile(w io.Writer, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()

	n, err := io.Copy(w, io.LimitReader(f, size+1))
	if err != nil {
		return err
	}

	if n != size {
		return errors.New(p + " changed while it was packed")
	}

	return nil
}

// InstallAdapter makes a runnable in the container with the full id id: it
// streams a's tar to the container's root (docker cp - ID:/), then runs the
// executable with a's ProbeArgs, which must exit 0. Both failures are
// ATTACH_FAILED; docker's own words are in the message.
func (e Engine) InstallAdapter(ctx context.Context, id string, a Adapter) error {
	if !isFullID(id) {
		return api.NewError(api.CodeInternal, "install into a container with a non-full id", "")
	}

	data, err := a.BuildTar()
	if err != nil {
		return err
	}

	const copyHint = "the container's root filesystem must be writable (read-only: true is unsupported)"

	if _, fail := e.run(ctx, CopyTimeout, bytes.NewReader(data), e.Args("cp", "-", id+":/")); fail != nil {
		return api.NewError(api.CodeAttachFailed, "can't copy "+a.Name+" into the container: "+failureText(fail), copyHint)
	}

	if _, fail := e.run(ctx, QueryTimeout, nil, e.Args(append([]string{"exec", id, a.ContainerPath()}, a.ProbeArgs...)...)); fail != nil {
		return api.NewError(api.CodeAttachFailed, a.Name+" doesn't run in the container: "+failureText(fail),
			"only glibc-based linux/amd64 and linux/arm64 images work, not Alpine or other musl images")
	}

	return nil
}

// failureText is a failure as an error message says it: what went wrong and
// docker's own last words.
func failureText(f *failure) string {
	msg := ""
	if f.err != nil {
		msg = f.err.Error()
	}

	if f.stderr != "" {
		if msg != "" {
			msg += ": "
		}

		msg += shortStderr(f.stderr)
	}

	return msg
}
